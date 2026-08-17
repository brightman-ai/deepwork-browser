//go:build !windows

package browser

import (
	"os/exec"
	"syscall"
)

// ApplyOwnedChromeProcAttr is the launch-side half of Chrome lifetime ownership
// on Unix. Every Chrome this process forks — pool ExecAllocator, BrowserMuxHost
// child, legacy launcher, ad-hoc chromedp — must go through it.
//
// It sets Setpgid so the forked process becomes the leader of a brand new
// process group, which is the *only* handle that survives the snap launch
// chain. Measured on this project's dev host (2026-08-17):
//
//	exec.Command("/usr/bin/chromium-browser")   ->  /bin/sh wrapper
//	  exec /snap/bin/chromium -> /usr/bin/snap   ->  snap-confine (setuid root)
//	    exec the real chrome                     ->  same PID throughout
//
//	after the parent exits:  chrome PPID == 1, PGID == the pid we forked
//
// So: the PID we get back *is* the real browser process (the wrapper execs, it
// does not fork), but Pdeathsig — which chromedp's own allocateCmdOptions sets,
// and which ApplyParentDeathKill sets — is silently cleared by the kernel when
// snap-confine, a setuid binary, is exec'd. That is documented kernel behavior
// for PR_SET_PDEATHSIG across a credential-changing execve, not a snap bug, and
// it is why "we already set Pdeathsig" was never actually protecting anything
// here. PGID, by contrast, is inherited straight through the whole chain and
// through every zygote/renderer/gpu-process/utility fork underneath, so
// kill(-pgid) reaches the entire tree in one shot — see KillChromeProcessGroup.
//
// Pdeathsig is still set as a second line of defence: it costs nothing and it
// does work for non-snap Chrome (a plain /usr/bin/google-chrome-stable keeps
// it), where it closes the SIGKILL-the-owner window that userspace cleanup
// cannot.
//
// Not for intentionally-detached Chrome (dw-browser open/session, where Chrome
// must outlive the CLI by design): those use ApplyDetachedProcAttr, which is
// the same Setpgid without the parent-death kill.
func ApplyOwnedChromeProcAttr(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	applyPdeathsigToProcAttr(cmd.SysProcAttr)
}

// ChromeProcessGroupID returns the process group pid belongs to, or 0 when it
// cannot be determined (process gone, or not permitted). Used to record an
// owned Chrome's pgid at launch so a later reaper can group-kill it even after
// the process has been reparented to init.
func ChromeProcessGroupID(pid int) int {
	if pid <= 0 {
		return 0
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid <= 0 {
		return 0
	}
	return pgid
}

// KillChromeProcessGroupID SIGKILLs a whole process group by pgid, without
// needing the leader to still be alive. KillChromeProcessGroup(pid) can only
// group-kill while the leader is running (it reads the pgid off the leader);
// once the leader has exited, its surviving children are still in that group
// and this is what reaches them.
//
// It refuses to fire on our own process group, and that refusal is not
// theoretical: a Chrome forked by a chromedp allocator that never set Setpgid
// inherits *the caller's* pgid, so a cleanup path holding that number would
// SIGKILL the test binary, `go test`, and the shell that ran them. Measured the
// hard way — the first version of the leak gate killed its own terminal and
// reported an empty log. Callers must fall back to KillChromeProcessTree for
// those, which walks children instead of trusting the group.
func KillChromeProcessGroupID(pgid int) {
	if pgid <= 1 || pgid == syscall.Getpgrp() {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// KillChromeProcessTree kills pid and everything under it, and is the safe
// choice whenever the pid's process group cannot be trusted to contain only
// Chrome. Prefers the one-shot group kill when pid actually leads its own group
// (everything ApplyOwnedChromeProcAttr / ApplyDetachedProcAttr forks does);
// otherwise walks descendants so a Chrome sharing our group is still fully
// reaped without taking us with it.
func KillChromeProcessTree(pid int) {
	if pid <= 1 {
		return
	}
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid && pgid != syscall.Getpgrp() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	// Kill leaves first so a dying parent cannot fork more children mid-sweep.
	for _, child := range childPIDsOf(pid) {
		KillChromeProcessTree(child)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
