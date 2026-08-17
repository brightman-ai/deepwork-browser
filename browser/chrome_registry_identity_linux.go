//go:build linux

package browser

import (
	"os"
	"strconv"
	"strings"
)

// processStartToken returns a value that changes when a pid is recycled — on
// Linux, field 22 of /proc/<pid>/stat (starttime, in clock ticks since boot).
// Without it, "owner pid is alive" is a lie as soon as the OS hands that number
// to someone else, and an abandoned Chrome would be protected forever by an
// unrelated process. Empty string means "cannot tell", and callers fall back to
// the plain liveness check.
func processStartToken(pid int) string {
	if pid <= 0 {
		return ""
	}
	body, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	// "pid (comm) state ppid ...": comm can contain spaces and parens, so scan
	// from the last ')' instead of splitting the whole line.
	closeParen := strings.LastIndexByte(string(body), ')')
	if closeParen < 0 {
		return ""
	}
	fields := strings.Fields(string(body)[closeParen+1:])
	// fields[0] is state (field 3), so starttime (field 22) is fields[19].
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}
