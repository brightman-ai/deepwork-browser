//go:build windows

package browser

import "os/exec"

// ApplyDetachedProcAttr is a no-op on Windows. CREATE_NEW_PROCESS_GROUP via
// SysProcAttr.CreationFlags can be added later if Chrome lifetime decoupling
// becomes a real issue on Windows.
//
// Exported so dw-browser CLI (headless path) can call it cross-platform.
func ApplyDetachedProcAttr(cmd *exec.Cmd) {}
