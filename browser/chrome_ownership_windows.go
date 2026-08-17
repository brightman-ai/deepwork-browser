//go:build windows

package browser

import (
	"os"
	"os/exec"
)

// ApplyOwnedChromeProcAttr is a no-op on Windows for the same reason
// ApplyDetachedProcAttr is: there is no Setpgid, and the equivalent guarantee
// needs a Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. Stated rather
// than silently implied: on Windows an owned Chrome is only reached by the
// single-PID kill in KillChromeProcessGroup.
func ApplyOwnedChromeProcAttr(cmd *exec.Cmd) {}

// ChromeProcessGroupID returns the pid itself on Windows — there are no process
// groups to record, and the registry's group-kill degrades to a PID kill.
func ChromeProcessGroupID(pid int) int {
	if pid <= 0 {
		return 0
	}
	return pid
}

// KillChromeProcessTree degrades to a single-process kill on Windows, same as
// KillChromeProcessGroup — see its doc for why the Job Object equivalent is not
// wired up yet.
func KillChromeProcessTree(pid int) { KillChromeProcessGroupID(pid) }

// KillChromeProcessGroupID degrades to a single-process kill on Windows.
func KillChromeProcessGroupID(pgid int) {
	if pgid <= 0 {
		return
	}
	if proc, err := os.FindProcess(pgid); err == nil {
		_ = proc.Kill()
	}
}
