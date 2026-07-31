//go:build linux

package browser

import (
	"os/exec"
	"syscall"
)

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
