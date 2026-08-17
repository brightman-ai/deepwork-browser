//go:build !linux && !windows

package browser

import "syscall"

// applyPdeathsigToProcAttr is a no-op outside Linux: syscall.SysProcAttr has no
// Pdeathsig field on darwin/BSD. The honest implementation, not a stub — there
// is no equivalent (see ApplyParentDeathKill's doc). Setpgid, which
// ApplyOwnedChromeProcAttr sets unconditionally, is the portable half and is
// the one that actually carries the guarantee on the snap-confined host this
// leak was found on.
func applyPdeathsigToProcAttr(attr *syscall.SysProcAttr) {}
