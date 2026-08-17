//go:build !linux

package browser

import "time"

type ownerlessChromeProcess struct {
	pid        int
	pgid       int
	profileDir string
	startedAt  time.Time
}

// findOwnerlessChromeProcesses returns nothing outside Linux. The PPID==1 scan
// it implements there reads /proc; the portable substitute (parsing `ps`) is
// available but the reparenting signal itself is much weaker on darwin, where
// launchd adopts processes for reasons unrelated to abandonment. Rather than
// ship a heuristic that could kill the Human's own Chrome, gc on those
// platforms relies on the registry pass and the stale-profile-dir pass, both of
// which are evidence-based.
func findOwnerlessChromeProcesses() []ownerlessChromeProcess { return nil }
