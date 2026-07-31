//go:build !linux && !windows

package browser

import "os/exec"

// ApplyParentDeathKill is a no-op outside Linux.
//
// Pdeathsig is a Linux-only field on syscall.SysProcAttr: the kernel's
// "SIGKILL me when my parent dies" contract simply does not exist on Darwin or
// the BSDs, and there is no drop-in equivalent (the closest, a kqueue NOTE_EXIT
// watcher, needs a live supervising goroutine — which is exactly what an
// unclean kill of this process would take down with it).
//
// This file exists because the declaration lived under a `!windows` build tag
// while the field it sets is `linux`-only, so the whole package failed to
// compile on macOS with "unknown field Pdeathsig". Splitting by the tag that
// matches the actual constraint fixes that without weakening Linux, where the
// guarantee is real.
//
// The callers are ad-hoc chromedp test allocators that have no other lifecycle
// tracking. On Linux they keep the kernel-level guarantee; elsewhere they fall
// back to what every other path already relies on — the deferred cleanup and
// chromedp's own SIGKILL-on-exit. The residual exposure is narrow and specific:
// a Chrome orphaned only when the test binary itself is SIGKILLed on a non-Linux
// host. Silently doing nothing is the honest behaviour here; pretending
// otherwise would need a supervisor this function has no way to own.
func ApplyParentDeathKill(cmd *exec.Cmd) {}
