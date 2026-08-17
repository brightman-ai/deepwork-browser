//go:build linux

package browser

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type ownerlessChromeProcess struct {
	pid        int
	pgid       int
	profileDir string
	startedAt  time.Time
}

// findOwnerlessChromeProcesses walks /proc for Chrome browser processes that
// have been reparented to init (PPID==1) and whose --user-data-dir we recognise
// as a dw-browser or chromedp profile.
//
// PPID==1 is the reap signal, and it is a strong one on Linux: a Chrome whose
// launcher is alive still has that launcher as its parent, so PPID==1 means
// either the launcher is gone or it deliberately detached — and the deliberate
// case is covered by the live-session allowlist in RunChromeGC.
//
// Only the *browser* process is reported (a zygote/renderer has --type=), since
// killing its process group takes the whole tree anyway.
func findOwnerlessChromeProcesses() []ownerlessChromeProcess {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	bootTime := time.Now()
	var found []ownerlessChromeProcess
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || len(raw) == 0 {
			continue
		}
		args := ParseProcCmdline(raw)
		if len(args) == 0 || !isChromeBrowserArgv(args) {
			continue
		}
		if pidPPIDFromProc(pid) != 1 {
			continue
		}
		profileDir := ChromeProfileDirFromArgs(args)
		if !chromeGCProfileDirIsOurs(profileDir) {
			continue
		}
		info, err := os.Stat(filepath.Join("/proc", entry.Name()))
		startedAt := bootTime
		if err == nil {
			startedAt = info.ModTime()
		}
		found = append(found, ownerlessChromeProcess{
			pid:        pid,
			pgid:       ChromeProcessGroupID(pid),
			profileDir: filepath.Clean(profileDir),
			startedAt:  startedAt,
		})
	}
	return found
}

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

// isChromeBrowserArgv distinguishes the browser process from its own children.
// Chrome's helpers all carry --type=; the browser process is the only one
// without it, and it is the only one whose process group needs killing.
func isChromeBrowserArgv(args []string) bool {
	base := filepath.Base(args[0])
	if !strings.Contains(base, "chrome") && !strings.Contains(base, "chromium") {
		return false
	}
	if strings.Contains(base, "crashpad") {
		return false
	}
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "--type=") {
			return false
		}
	}
	return true
}

func pidPPIDFromProc(pid int) int {
	body, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	closeParen := strings.LastIndexByte(string(body), ')')
	if closeParen < 0 {
		return 0
	}
	fields := strings.Fields(string(body)[closeParen+1:])
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}
