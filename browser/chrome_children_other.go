//go:build !linux && !windows

package browser

// childPIDsOf has no /proc to read outside Linux. Returning nothing degrades
// KillChromeProcessTree to a single-pid kill for the rare case it covers (a
// Chrome sharing our process group); the common case there — a Chrome that
// leads its own group because we forked it — is already handled by the group
// kill and does not need this.
func childPIDsOf(pid int) []int { return nil }
