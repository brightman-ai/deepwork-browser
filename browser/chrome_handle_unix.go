//go:build !windows

package browser

import (
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
