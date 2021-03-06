package config

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"os"
)

// main configuration
type Config struct {

	// list of feeds to add on runtime
	Feeds []*FeedConfig `json:"feeds"`
	// nntp server configuration
	NNTP *NNTPServerConfig `json:"nntp"`
	// log level
	Log string `json:"log"`

	Store *StoreConfig `json:"storage"`

	// unexported fields ...

	// absolute filepath to configuration
	fpath string
}

// default configuration
var DefaultConfig = Config{
	Store: &StoreConfig{
		Path: "storage",
	},
	NNTP: &NNTPServerConfig{

		Bind: "127.0.0.1:1119",
		Name: "nntp.server.tld",
		Article: &ArticleConfig{
			AllowGroups:          []string{"ctl", "overchan.test"},
			DisallowGroups:       []string{"overchan.cp"},
			ForceWhitelist:       false,
			AllowAnon:            true,
			AllowAttachments:     true,
			AllowAnonAttachments: false,
		},
	},
}

// reload configuration
func (c *Config) Reload() (err error) {
	var b []byte
	b, err = ioutil.ReadFile(c.fpath)
	if err == nil {
		err = json.Unmarshal(b, c)
	}
	return
}

// ensure that a config file exists
// creates one if it does not exist
func Ensure(fname string) (cfg *Config, err error) {
	_, err = os.Stat(fname)
	if os.IsNotExist(err) {
		err = nil
		var d []byte
		d, err = json.Marshal(&DefaultConfig)
		if err == nil {
			b := new(bytes.Buffer)
			err = json.Indent(b, d, "", "  ")
			if err == nil {
				err = ioutil.WriteFile(fname, b.Bytes(), 0600)
			}
		}
	}
	if err == nil {
		cfg, err = Load(fname)
	}
	return
}

// load configuration file
func Load(fname string) (cfg *Config, err error) {
	cfg = new(Config)
	cfg.fpath = fname
	err = cfg.Reload()
	if err != nil {
		cfg = nil
	}
	return
}
