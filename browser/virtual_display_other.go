//go:build !darwin

package browser

import "time"

// VirtualDisplayManager is a no-op stub on non-Darwin platforms.
//
// On Linux, virtual display isolation is handled by Xvfb (:99) managed by
// DisplayManager. On Windows, the current strategy falls back to headless
// mode (CreateDesktop Win32 API is P2 scope — DELTA-BS09-THREE-MODE §8).
//
// All methods are safe to call concurrently.
type VirtualDisplayManager struct{}

// Ensure is a no-op on non-Darwin platforms. Always returns nil.
func (vdm *VirtualDisplayManager) Ensure() error {
	return nil
}

// WindowPosition returns (0, 0) on non-Darwin platforms.
// Linux Chrome instances are positioned via DISPLAY=:99; absolute screen
// coordinates are managed by the window manager.
func (vdm *VirtualDisplayManager) WindowPosition() (x, y int) {
	return 0, 0
}

// WindowPositionAt returns (offset, offset) — helper for tests.
func (vdm *VirtualDisplayManager) WindowPositionAt(offset int) (x, y int) {
	return offset, offset
}

// DisplayID returns 0 on non-Darwin platforms.
func (vdm *VirtualDisplayManager) DisplayID() uint32 {
	return 0
}

func (vdm *VirtualDisplayManager) CountWindowsOutsideDisplay(pid int) int {
	return 0
}

func (vdm *VirtualDisplayManager) VerifyChromeContained(pid int, timeout time.Duration) error {
	return nil
}

// Close is a no-op on non-Darwin platforms. Always returns nil.
func (vdm *VirtualDisplayManager) Close() error {
	return nil
}
