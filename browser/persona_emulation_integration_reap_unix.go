//go:build personacheck && !windows

package browser

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// reapStaleChromedpRunnerOrphans runs once when this test binary loads,
// before any test executes (see init below). TestPersonaEmulation_Integration
// bypasses the CLI launcher entirely — raw chromedp.NewExecAllocator — so its
// Chrome has no session file / owner marker for `dw-browser profile prune` to
// ever find, and isn't under any dw-browser-cli root either (chromedp uses
// its own /tmp/chromedp-runner* default). If a prior run of this binary was
// killed hard (timeout, CI cancel, Ctrl-C) before its own defer cancelA()
// could run, that Chrome is reparented to init — PPID==1 is the reap signal:
// no live parent means genuinely abandoned, not a concurrently-running
// test's session (which would still have a live PPID).
//
// ApplyParentDeathKill (chrome_handle_unix.go) is meant to prevent this at
// the source via Pdeathsig, but on this project's dev machine snap-confined
// chromium clears Pdeathsig on its AppArmor exec transition (kernel behavior,
// not a bug — verified empirically 2026-07-19) — this is the fallback for
// when that source-level prevention doesn't apply.
func reapStaleChromedpRunnerOrphans() {
	tmp := os.TempDir()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "chromedp-runner") {
			continue
		}
		dir := filepath.Join(tmp, e.Name())
		pids := platformFindChromePIDsByProfileDir(dir)
		if len(pids) == 0 {
			// No process left using this dir at all — a prior run already
			// died and got reaped (or exited cleanly but chromedp only
			// removes it via its own cancel(), which a hard kill skips).
			// Nothing to kill; the directory itself is pure leftover.
			_ = os.RemoveAll(dir)
			continue
		}
		orphaned := false
		for _, pid := range pids {
			if pidPPID(pid) == 1 {
				orphaned = true
				break
			}
		}
		if !orphaned {
			// At least one live, non-orphaned process still owns this dir
			// — a concurrently-running test's session. Leave it alone.
			continue
		}
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		_ = os.RemoveAll(dir)
	}
}

// pidPPID reads the parent PID of pid from /proc, or 0 if it can't be
// determined (process gone, /proc unavailable). Cheaper than shelling out to
// ps for what's normally a handful of directories.
func pidPPID(pid int) int {
	body, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// Format: "pid (comm) state ppid ...". comm may contain spaces/parens,
	// so scan from the last ')' rather than splitting naively.
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

func init() {
	reapStaleChromedpRunnerOrphans()
}
