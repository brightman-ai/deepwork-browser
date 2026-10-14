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
