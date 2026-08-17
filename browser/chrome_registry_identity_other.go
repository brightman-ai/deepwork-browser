//go:build !linux

package browser

// processStartToken has no cheap portable equivalent outside Linux, so it
// returns "" and pid-recycle detection degrades to a plain liveness check. That
// is the honest degradation: on darwin/windows a recycled owner pid can protect
// an abandoned Chrome until `dw-browser gc --older-than` sweeps it, rather than
// silently claiming a guarantee the platform does not give.
func processStartToken(pid int) string { return "" }
