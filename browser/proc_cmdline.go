package browser

import "strings"

// ParseProcCmdline splits /proc/<pid>/cmdline into arguments, and it exists
// because Chrome does not leave that file in the format everyone assumes.
//
// The kernel writes argv NUL-separated, and every parser in the wild splits on
// "\x00". Chrome rewrites its own argv in place into a single space-joined
// blob (it reuses the argv buffer for its process title), so for a live Chrome
// the whole command line arrives as *one* NUL-terminated string — measured on
// this host: argc==1 for browser, zygote and renderer alike. Split it on NUL
// and you get a single token, which is why "does argv contain --type=" and
// "does argv contain --user-data-dir=" both silently answered no, and why a
// PPID-scanning leak gate saw nothing while 60 orphans sat in ps.
//
// Falling back to whitespace splitting recovers flag tokens. It does mangle
// flags whose *value* contains spaces (--user-agent=Mozilla/5.0 (X11; …)) into
// several tokens, which is harmless here: nothing reads those, and the two
// flags that are read — --type= and --user-data-dir= — never carry spaces in
// any path this project produces.
func ParseProcCmdline(raw []byte) []string {
	s := strings.TrimRight(string(raw), "\x00")
	if s == "" {
		return nil
	}
	if strings.Contains(s, "\x00") {
		return strings.Split(s, "\x00")
	}
	return strings.Fields(s)
}
