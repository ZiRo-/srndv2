//
// frontend_http.go
//
// srnd http frontend implementation
//
package srnd

import (
  "github.com/dchest/captcha"
  "github.com/gorilla/mux"
  "github.com/gorilla/sessions"
  "bytes"
  "fmt"
  "io"
  "log"
  "net"
  "net/http"
  "os"
  "path/filepath"
  "strings"
)


// regenerate a newsgroup page
type groupRegenRequest struct {
  // which newsgroup
  group string
  // page number
  page int
}

type httpFrontend struct {

  modui ModUI
  httpmux *mux.Router
  daemon *NNTPDaemon
  postchan chan NNTPMessage
  recvpostchan chan NNTPMessage
  bindaddr string
  name string

  webroot_dir string
  template_dir string
  static_dir string

  regen_threads int
  
  prefix string
  regenThreadChan chan string
  regenGroupChan chan groupRegenRequest
  ukkoChan chan bool
  
  store *sessions.CookieStore
}

// do we allow this newsgroup?
func (self httpFrontend) AllowNewsgroup(group string) bool {
  // XXX: hardcoded nntp prefix
  // TODO: make configurable nntp prefix
  return strings.HasPrefix(group, "overchan.")
}

// try to delete root post's page
func (self httpFrontend) deleteThreadMarkup(root_post_id string) {
  fname :=  self.getFilenameForThread(root_post_id)
  log.Println("delete file", fname)
  os.Remove(fname)
}

func (self httpFrontend) getFilenameForThread(root_post_id string) string {
  fname := fmt.Sprintf("thread-%s.html", ShortHashMessageID(root_post_id))
  return filepath.Join(self.webroot_dir, fname)
}

func (self httpFrontend) getFilenameForBoardPage(boardname string, pageno int) string {
  fname := fmt.Sprintf("%s-%d.html", boardname, pageno)
  return filepath.Join(self.webroot_dir, fname)
}

func (self httpFrontend) NewPostsChan() chan NNTPMessage {
  return self.postchan
}

func (self httpFrontend) PostsChan() chan NNTPMessage {
  return self.recvpostchan
}

// regen every newsgroup
func (self httpFrontend) regenAll() {
  log.Println("regen all on http frontend")
  // tell to regen ukko first
  self.ukkoChan <- true
  // get all groups
  groups := self.daemon.database.GetAllNewsgroups()
  if groups != nil {
    for _, group := range groups {
      // send every thread for this group down the regen thread channel
      self.daemon.database.GetGroupThreads(group, self.regenThreadChan)
      self.regenerateBoard(group)
    }
  }
}


// regen every page of the board
func (self httpFrontend) regenerateBoard(group string) {
  // regen the entire board too
  pages := self.daemon.database.GetGroupPageCount(group)
  // regen all pages
  var page int64
  for ; page < pages ; page ++ {
    req := groupRegenRequest{group, int(page)}
    self.regenGroupChan <- req
  }
}

// regenerate a board page for newsgroup
func (self httpFrontend) regenerateBoardPage(newsgroup string, pageno int) {
  var err error
  var perpage int
  perpage, err = self.daemon.database.GetThreadsPerPage(newsgroup)
  if err != nil {
    log.Println("board regen fallback to default threads per page because", err)
    // fallback
    perpage = 10
  }
  board_page := self.daemon.database.GetGroupForPage(self.prefix, self.name, newsgroup, pageno, perpage)
  if board_page == nil {
    log.Println("failed to regen board", newsgroup)
    return
  }
  fname := self.getFilenameForBoardPage(newsgroup, pageno)
  wr, err := OpenFileWriter(fname)
  if err == nil {
    err = board_page.RenderTo(wr)
    wr.Close()
    if err != nil {
      log.Println("did not write board page",fname, err)
    }
  } else {
    log.Println("cannot open", fname, err)
  }
  log.Println("regenerated file", fname)
}

// regenerate the ukko page
func (self httpFrontend) regenUkko() {
  // get the last 5 bumped threads
  roots := self.daemon.database.GetLastBumpedThreads("", 5)
  var threads []ThreadModel
  for _, rootpost := range roots {
    // for each root post
    // get the last 5 posts
    post := self.daemon.database.GetPostModel(self.prefix, rootpost)
    if post == nil {
      log.Println("failed to get root post", rootpost)
      return
    }
    posts := []PostModel{post}
    if self.daemon.database.ThreadHasReplies(rootpost) {
      repls := self.daemon.database.GetThreadReplyPostModels(self.prefix, rootpost, 5)
      if repls == nil {
        log.Println("failed to get replies for", rootpost)
        return
      }
      posts = append(posts, repls...)
    }
    threads = append(threads, NewThreadModel(self.prefix, posts))
  }
  wr, err := OpenFileWriter(filepath.Join(self.webroot_dir, "ukko.html"))
  if err == nil {
    io.WriteString(wr, renderUkko(self.prefix, threads))
    wr.Close()
  } else {
    log.Println("error generating ukko", err)
  }
}

// regnerate a thread given the messageID of the root post
// TODO: don't load from store
func (self httpFrontend) regenerateThread(rootMessageID string) {
  // get the root post
  post := self.daemon.database.GetPostModel(self.prefix, rootMessageID)
  if post == nil {
    log.Println("failed to regen thread, root post is nil", rootMessageID)
    return
  }
  posts := []PostModel{post}
  // get replies
  if self.daemon.database.ThreadHasReplies(rootMessageID) {
    repls :=  self.daemon.database.GetThreadReplyPostModels(self.prefix, rootMessageID, 0)
    if repls == nil {
      log.Println("failed to regen thread, replies was nil for op", rootMessageID)
      return
    }
    posts = append(posts, repls...)
  }
  
  // make thread model
  thread := NewThreadModel(self.prefix, posts)
  // get filename for thread
  fname := self.getFilenameForThread(rootMessageID)
  // open writer for file
  wr, err := OpenFileWriter(fname)
  if err != nil {
    log.Println(err)
    return
  }
  // render the thread
  err = thread.RenderTo(wr)
  wr.Close()
  if err == nil {
    log.Printf("regenerated file %s", fname)
  } else {
    log.Printf("failed to render %s", err)
  }  
}

func (self httpFrontend) poll() {
  chnl := self.PostsChan()
  modChnl := self.modui.MessageChan()
  for {
    select {
    case nntp := <- modChnl:
      // forward signed messages to daemon
      self.postchan <- nntp
    case nntp := <- chnl:
      // get root post and tell frontend to regen that thread
      if len(nntp.Reference()) > 0 {
        self.regenThreadChan <- nntp.Reference()
      } else {
        self.regenThreadChan <- nntp.MessageID()
      }
      // regen the newsgroup we're in
      // TODO: regen only what we need to
      pages := self.daemon.database.GetGroupPageCount(nntp.Newsgroup())
      // regen all pages
      var page int64
      for ; page < pages ; page ++ {
        req := groupRegenRequest{nntp.Newsgroup(), int(page)}
        self.regenGroupChan <- req
      }
      // regen ukko
      self.ukkoChan <- true
    }
  }
}

// select loop for channels
func (self httpFrontend) pollregen() {
  for {
    select {
      // listen for regen thread requests
    case msgid := <- self.regenThreadChan:
      self.regenerateThread(msgid)
      
      // listen for regen board requests
    case req := <- self.regenGroupChan:
      self.regenerateBoardPage(req.group, req.page)
    }
  }
}


func (self httpFrontend) pollukko() {
  for {
    _ : <- self.ukkoChan
    self.regenUkko()
    log.Println("ukko regenerated")
  }
}

// handle new post via http request for a board
func (self httpFrontend) handle_postform(wr http.ResponseWriter, r *http.Request, board string) {

  // captcha stuff
  captcha_id := ""
  captcha_solution := ""

  // post fail message
  post_fail := ""

  // post message
  msg := ""
  
  // the nntp message
  var nntp nntpArticle
  nntp.headers = make(ArticleHeaders)



  // encrypt IP Addresses
  // when a post is recv'd from a frontend, the remote address is given its own symetric key that the local srnd uses to encrypt the address with, for privacy
  // when a mod event is fired, it includes the encrypted IP address and the symetric key that frontend used to encrypt it, thus allowing others to determine the IP address
  // each stnf will optinally comply with the mod event, banning the address from being able to post from that frontend
  // this will be done eventually but for now that requires too much infrastrucutre, let's go with regular IP Addresses for now.
  
  // get the "real" ip address from the request

  address , _, err := net.SplitHostPort(r.RemoteAddr)
  // TODO: have in config upstream proxy ip and check for that
  if strings.HasPrefix(address, "127.") {
    // if it's loopback check headers for reverse proxy headers
    // TODO: make sure this isn't a tor user being sneaky
    address = getRealIP(r.Header.Get("X-Real-IP"))
  }
    
  // check for banned
  if len(address) > 0 {
    banned, err :=  self.daemon.database.CheckIPBanned(address)
    if err == nil {
      if banned {
        wr.WriteHeader(403)
        // TODO: ban messages
        io.WriteString(wr,  "nigguh u banned.")
        return
      }
    } else {
      wr.WriteHeader(500)
      io.WriteString(wr, "error checking for ban: ")
      io.WriteString(wr, err.Error())
      return
    }
  }
  if len(address) == 0 {
    address = "Tor"
  }
  if ! strings.HasPrefix(address, "127.") {
    // set the ip address of the poster to be put into article headers
    // if we cannot determine it, i.e. we are on Tor/i2p, this value is not set
    if address == "Tor" {
      nntp.headers.Set("X-Tor-Poster", "1")
    } else {
      address, err = self.daemon.database.GetEncAddress(address)
      nntp.headers.Set("X-Encrypted-IP", address)
      // TODO: add x-tor-poster header for tor exits
    }
  }
  
  // if we don't have an address for the poster try checking for i2p httpd headers
  address = r.Header.Get("X-I2P-DestHash")
  // TODO: make sure this isn't a Tor user being sneaky
  if len(address) > 0 {
    nntp.headers.Set("X-I2P-DestHash", address)
  }
  

  // set newsgroup
  nntp.headers.Set("Newsgroups", board)
  
  // redirect url
  url := self.prefix
  // mime part handler
  var part_buff bytes.Buffer
  mp_reader, err := r.MultipartReader()
  if err != nil {
    errmsg := fmt.Sprintf("httpfrontend post handler parse multipart POST failed: %s", err)
    log.Println(errmsg)
    wr.WriteHeader(500)
    io.WriteString(wr, errmsg)
    return
  }
  for {
    part, err := mp_reader.NextPart()
    if err == nil {
      // get the name of the part
      partname := part.FormName()

      // read part for attachment
      if partname == "attachment" {
        log.Println("attaching file...")
        att := self.daemon.store.ReadAttachmentFromMimePart(part, false)
        nntp = nntp.Attach(att).(nntpArticle)
        continue
      }

      io.Copy(&part_buff, part)
      
      // check for values we want
      if partname == "subject" {
        subject := part_buff.String()
        if len(subject) == 0 {
          subject = "None"
        }
        nntp.headers.Set("Subject", subject)
        if isSage(subject) {
          nntp.headers.Set("X-Sage", "1")
        }
      } else if partname == "name" {
        name := part_buff.String()
        if len(name) == 0 {
          name = "Anonymous"
        }  
        nntp.headers.Set("From", nntpSanitize(fmt.Sprintf("%s <%s@%s>", name, name, self.name)))
        nntp.headers.Set("Message-ID", genMessageID(self.name))
      } else if partname == "message" {
        msg = part_buff.String()
      } else if partname == "reference" {
        ref := part_buff.String()
        if len(ref) == 0 {
          url = fmt.Sprintf("%s.html", board)
        } else if ValidMessageID(ref) {
          if self.daemon.database.HasArticleLocal(ref) {
            nntp.headers.Set("References", ref)
            url = fmt.Sprintf("thread-%s.html", ShortHashMessageID(ref))
          } else {
            // no such article
            url = fmt.Sprintf("%s.html", board)
            post_fail += "we don't have "
            post_fail += ref
            post_fail += "locally, can't reply. "
          }
        } else {
          post_fail += "invalid reference: "
          post_fail += ref
          post_fail += ", not posting. "
        }
          

      } else if partname == "captcha" {
        captcha_id = part_buff.String()
      } else if partname == "captcha_solution" {
        captcha_solution = part_buff.String()
      }      
      // we done
      // reset buffer for reading parts
      part_buff.Reset()
      // close our part
      part.Close()
    } else {
      if err != io.EOF {
        errmsg := fmt.Sprintf("httpfrontend post handler error reading multipart: %s", err)
        log.Println(errmsg)
        wr.WriteHeader(500)
        io.WriteString(wr, errmsg)
        return
      }
      break
    }
  }


  // make error template param
  resp_map := make(map[string]string)
  resp_map["redirect_url"] = url
  
  if len(captcha_solution) == 0 || len(captcha_id) == 0 {
    post_fail += "no captcha provided. "
  }
  
  if ! captcha.VerifyString(captcha_id, captcha_solution) {
    post_fail += "failed captcha. "
  }

  if len(nntp.attachments) == 0 && len(msg) == 0 {
    post_fail += "no message. "
  }
  if len(post_fail) > 0 {
    wr.WriteHeader(200)
    resp_map["reason"] = post_fail
    fname := filepath.Join(defaultTemplateDir(), "post_fail.mustache")
    io.WriteString(wr, templateRender(fname, resp_map))
    return
  }
  // set message
  nntp.message = createPlaintextAttachment(msg)
  // set date
  nntp.headers.Set("Date", timeNowStr())
  // append path from frontend
  nntp.AppendPath(self.name)
  // send message off to daemon
  log.Printf("uploaded %d attachments", len(nntp.Attachments()))
  nntp.Pack()
  self.postchan <- nntp

  // send success reply
  wr.WriteHeader(200)
  // determine the root post so we can redirect to the thread for it
  msg_id := nntp.headers.Get("Reference", nntp.MessageID())
  // render response as success
  url = fmt.Sprintf("%sthread-%s.html", self.prefix, ShortHashMessageID(msg_id))
  fname := filepath.Join(defaultTemplateDir(), "post_success.mustache")
  io.WriteString(wr, templateRender(fname, map[string]string {"message_id" : nntp.MessageID(), "redirect_url" : url}))
}



// handle posting / postform
func (self httpFrontend) handle_poster(wr http.ResponseWriter, r *http.Request) {
  path := r.URL.Path
  var board string
  // extract board
  parts := strings.Count(path, "/")
  if parts > 1 {
    board = strings.Split(path, "/")[2]
  }
  
  // this is a POST request
  if r.Method == "POST" && self.AllowNewsgroup(board) && newsgroupValidFormat(board) {
    self.handle_postform(wr, r, board)
  } else {
    wr.WriteHeader(403)
    io.WriteString(wr, "Nope")
  }
}

func (self httpFrontend) new_captcha(wr http.ResponseWriter, r *http.Request) {
  io.WriteString(wr, captcha.NewLen(5))
}

func (self httpFrontend) Mainloop() {
  EnsureDir(self.webroot_dir)
  if ! CheckFile(self.template_dir) {
    log.Fatalf("no such template folder %s", self.template_dir)
  }

  threads := self.regen_threads 

  // check for invalid number of threads
  if threads > 0 {
    threads = 1
  }
  
  // set up handler mux
  self.httpmux = mux.NewRouter()
  
  // create mod ui
  self.modui = createHttpModUI(self)

  // modui handlers
  self.httpmux.Path("/mod/").HandlerFunc(self.modui.ServeModPage).Methods("GET")
  self.httpmux.Path("/mod/keygen").HandlerFunc(self.modui.HandleKeyGen).Methods("GET")
  self.httpmux.Path("/mod/login").HandlerFunc(self.modui.HandleLogin).Methods("POST")
  self.httpmux.Path("/mod/del/{article_hash}").HandlerFunc(self.modui.HandleDeletePost).Methods("GET")
  self.httpmux.Path("/mod/ban/{address}").HandlerFunc(self.modui.HandleBanAddress).Methods("GET")
  self.httpmux.Path("/mod/addkey/{pubkey}").HandlerFunc(self.modui.HandleAddPubkey).Methods("GET")
  self.httpmux.Path("/mod/delkey/{pubkey}").HandlerFunc(self.modui.HandleDelPubkey).Methods("GET")
  
  // webroot handler
  self.httpmux.Path("/").Handler(http.FileServer(http.Dir(self.webroot_dir)))
  self.httpmux.Path("/thm/{f}").Handler(http.FileServer(http.Dir(self.webroot_dir)))
  self.httpmux.Path("/img/{f}").Handler(http.FileServer(http.Dir(self.webroot_dir)))
  self.httpmux.Path("/{f}.html").Handler(http.FileServer(http.Dir(self.webroot_dir)))
  self.httpmux.Path("/static/{f}").Handler(http.FileServer(http.Dir(self.static_dir)))
  // post handler
  self.httpmux.Path("/post/{f}").HandlerFunc(self.handle_poster).Methods("POST")
  // captcha handlers
  self.httpmux.Path("/captcha/new").HandlerFunc(self.new_captcha).Methods("GET")
  self.httpmux.Path("/captcha/{f}").Handler(captcha.Server(350, 175)).Methods("GET")

  
  // make regen threads
  for threads > 0 {    
    go self.pollregen()
    threads -- 
  }

  // poll for ukko regen
  go self.pollukko()
  
  // poll channels
  go self.poll()
  
  // trigger regen
  self.regenAll()

  // start webserver here
  log.Printf("frontend %s binding to %s", self.name, self.bindaddr)
  
  err := http.ListenAndServe(self.bindaddr, self.httpmux)
  if err != nil {
    log.Fatalf("failed to bind frontend %s %s", self.name, err)
  }
}

func (self httpFrontend) Regen(msg ArticleEntry) {
  self.regenThreadChan <- msg.MessageID()
  self.regenerateBoard(msg.Newsgroup())
  self.ukkoChan <- true
}


// create a new http based frontend
func NewHTTPFrontend(daemon *NNTPDaemon, config map[string]string) Frontend {
  var front httpFrontend
  front.daemon = daemon
  front.bindaddr = config["bind"]
  front.name = config["name"]
  front.webroot_dir = config["webroot"]
  front.static_dir = config["static_files"]
  front.template_dir = config["templates"]
  front.prefix = config["prefix"]
  front.regen_threads = mapGetInt(config, "regen_threads", 1)
  front.store = sessions.NewCookieStore([]byte(config["api-secret"]))
  front.postchan = make(chan NNTPMessage, 16)
  front.recvpostchan = make(chan NNTPMessage, 16)
  front.regenThreadChan = make(chan string, 16)
  front.regenGroupChan = make(chan groupRegenRequest, 8)
  front.ukkoChan = make(chan bool)
  return front
}
