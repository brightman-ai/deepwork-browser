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
