//go:build windows

package browser

import (
	"os"
	"os/exec"
)

// ApplyDetachedProcAttr is a no-op on Windows. CREATE_NEW_PROCESS_GROUP via
// SysProcAttr.CreationFlags can be added later if Chrome lifetime decoupling
// becomes a real issue on Windows.
//
// Exported so dw-browser CLI (headless path) can call it cross-platform.
func ApplyDetachedProcAttr(cmd *exec.Cmd) {}

// KillChromeProcessGroup falls back to a plain single-process kill on
// Windows: ApplyDetachedProcAttr above doesn't group Chrome's children here,
// so there's no group to target yet (see its doc comment).
func KillChromeProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Kill()
	}
}
