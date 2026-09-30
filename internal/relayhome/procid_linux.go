package relayhome

import (
	"os"
	"strconv"
	"strings"
)

// ProcessIdentity names process pid by its start time (clock ticks since
// boot, /proc/<pid>/stat field 22), or "" if it is not running or cannot
// be read. A reused pid gets a different one.
func ProcessIdentity(pid int) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')') // the command name may hold spaces and parens
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:]) // from field 3 (state) on
	if len(f) < 20 || f[0] == "Z" || f[0] == "X" {
		return ""
	}
	return strconv.Itoa(pid) + "@" + f[19]
}
