//go:build windows

package browser

import "testing"

func spawnMuxHostLookalike(t *testing.T, hostID string) int {
	t.Helper()
	t.Skip("unix-only: needs a process whose command line can be inspected with ps")
	return 0
}

func spawnChromeLookalike(t *testing.T, profileDir string) int {
	t.Helper()
	t.Skip("unix-only: needs a process whose command line can be inspected with ps")
	return 0
}
