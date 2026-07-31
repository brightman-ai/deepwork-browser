//go:build personacheck && windows

package browser

// reapStaleChromedpRunnerOrphans is a no-op on Windows: no /proc, and
// chromedp doesn't default to a Windows temp-dir naming this package
// recognizes reliably. See the unix version's doc comment for what this
// guards against.
func init() {}
