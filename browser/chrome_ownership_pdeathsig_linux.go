//go:build linux

package browser

import "syscall"

// applyPdeathsigToProcAttr sets Pdeathsig on an already-allocated SysProcAttr.
// Split out for the same reason ApplyParentDeathKill is (commit a9fd558):
// Pdeathsig is a Linux-only field of syscall.SysProcAttr, so touching it from
// a !windows file breaks the darwin/BSD build.
func applyPdeathsigToProcAttr(attr *syscall.SysProcAttr) {
	if attr == nil {
		return
	}
	attr.Pdeathsig = syscall.SIGKILL
}
