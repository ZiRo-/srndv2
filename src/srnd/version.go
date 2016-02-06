//
// version.go -- contains srnd version strings
//

package srnd

import "fmt"

var major_version = 3
var minor_version = 1
var program_name = "srnd"

func Version() string {
	return fmt.Sprintf("%s version 2.%d.%d", program_name, major_version, minor_version)
}
