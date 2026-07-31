//go:build !windows

package browser

import (
	"os"
	"os/exec"
	"syscall"
)

// ApplyDetachedProcAttr sets Setpgid so the spawned Chrome lives in its own
// process group. This decouples Chrome's lifetime from the parent process's
// signal-handling pgrp (important for dw-browser CLI which exits while Chrome
// must remain).
//
// Exported so dw-browser CLI (headless path) can reuse the same cross-platform
// helper without re-implementing platform-specific syscall imports.
func ApplyDetachedProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// ApplyParentDeathKill sets Pdeathsig so the kernel SIGKILLs the spawned
// Chrome the instant this process dies — including via SIGKILL, which no
// userspace cleanup (our own defers, or chromedp's ModifyCmdFunc doc: "the
// default version of the command ... sends SIGKILL to any open browsers when
// the Go program exits") ever gets a chance to run for. Intended for ad-hoc
// chromedp.NewExecAllocator callers that bypass the CLI launcher entirely
// (e.g. persona_emulation_integration_test.go) and have no other lifecycle
// tracking (no session file, no owner marker, no profile-prune reach since
// they don't even live under a dw-browser-cli root) — this is the only
// guarantee against leaving an orphaned Chrome behind when the test binary
// itself is killed (timeout, CI cancel, Ctrl-C). Not used by the CLI's own
// launcher: interactive/service sessions are Setpgid-detached on purpose so
// Chrome outlives the CLI (see ApplyDetachedProcAttr) — Pdeathsig would break
// that by design, not by accident.
func ApplyParentDeathKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

// KillChromeProcessGroup kills a Setpgid-detached Chrome's whole process
// group in one shot, not just the leader PID. Chrome's zygote/gpu-process/
// renderer/utility/crashpad_handler children inherit the leader's pgid at
// fork time (ApplyDetachedProcAttr above) and keep running after a
// single-PID kill — verified empirically: they go on writing into the
// profile dir (crash reports, cache, prefs) for seconds afterward, silently
// resurrecting a directory os.RemoveAll just reported successfully removing.
// Falls back to a plain single-process kill if pid isn't a group leader
// (e.g. a PID handed to us by a different launcher that didn't Setpgid).
func KillChromeProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Kill()
	}
}
