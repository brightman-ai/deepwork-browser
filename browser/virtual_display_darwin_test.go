//go:build darwin

package browser

import (
	"os"
	"testing"
)

// sharedVDM is created once for all VirtualDisplay tests and torn down in
// TestMain. Sharing a single manager avoids rapid create/destroy/create
// cycles: WindowServer needs a few seconds between CGVirtualDisplay teardown
// and the next creation.
//
// If Ensure fails (e.g. a stale display from a prior run is still registered
// or this machine has no WindowServer), all tests skip gracefully.
var sharedVDM *VirtualDisplayManager

func TestMain(m *testing.M) {
	sharedVDM = &VirtualDisplayManager{}
	// Ignore errors — individual tests call sharedVDM.Ensure and skip if nil.
	_ = sharedVDM.Ensure

	code := m.Run

	_ = sharedVDM.Close
	os.Exit(code)
}

// requireDisplay calls t.Skip if the shared virtual display is not available.
func requireDisplay(t *testing.T) {
	t.Helper
	if err := sharedVDM.Ensure; err != nil {
		t.Skipf("CGVirtualDisplay not available (possibly a stale display from a prior run occupies the slot): %v", err)
	}
	if sharedVDM.DisplayID == 0 {
		t.Skip("CGVirtualDisplay created but displayID is 0 — skipping")
	}
}

// TestVirtualDisplayEnsure verifies that Ensure creates a non-zero displayID
// and that bounds are populated (non-zero width and height).
func TestVirtualDisplayEnsure(t *testing.T) {
	requireDisplay(t)

	id := sharedVDM.DisplayID
	t.Logf("displayID=%d", id)

	sharedVDM.mu.Lock
	w, h := sharedVDM.w, sharedVDM.h
	sharedVDM.mu.Unlock

	x, y := sharedVDM.WindowPositionAt(0)
	t.Logf("bounds: origin=(%d,%d) size=(%dx%d)", x, y, w, h)

	if w == 0 || h == 0 {
		t.Errorf("virtual display bounds are zero: w=%d h=%d — WindowServer may not have registered the display yet", w, h)
	}
}

// TestVirtualDisplayIdempotent verifies that calling Ensure twice on the
// same manager returns the same displayID (no duplicate display is created).
func TestVirtualDisplayIdempotent(t *testing.T) {
	requireDisplay(t)
	firstID := sharedVDM.DisplayID

	// Second call must be a no-op.
	if err := sharedVDM.Ensure; err != nil {
		t.Fatalf("second Ensure error: %v", err)
	}
	secondID := sharedVDM.DisplayID

	if firstID != secondID {
		t.Errorf("displayID changed after second Ensure: first=%d second=%d", firstID, secondID)
	}
	t.Logf("idempotent check passed: displayID=%d", firstID)
}

// TestVirtualDisplayWindowPosition verifies that WindowPositionAt returns
// coordinates within the virtual display's bounding box.
func TestVirtualDisplayWindowPosition(t *testing.T) {
	requireDisplay(t)

	const offset = 50
	px, py := sharedVDM.WindowPositionAt(offset)
	t.Logf("WindowPositionAt(%d) = (%d, %d)", offset, px, py)

	sharedVDM.mu.Lock
	vx, vy, vw, vh := sharedVDM.x, sharedVDM.y, sharedVDM.w, sharedVDM.h
	sharedVDM.mu.Unlock

	if vw == 0 || vh == 0 {
		t.Skip("bounds not populated — skipping position range check")
	}

	if px < vx || px >= vx+vw {
		t.Errorf("WindowPositionAt x=%d outside virtual display [%d, %d)", px, vx, vx+vw)
	}
	if py < vy || py >= vy+vh {
		t.Errorf("WindowPositionAt y=%d outside virtual display [%d, %d)", py, vy, vy+vh)
	}
}

// TestVirtualDisplayClose verifies that Close resets all internal state.
// This test uses a COPY of the shared display's internal state (after closing
// sharedVDM) rather than creating a second display, so it doesn't hit the
// macOS one-display-per-session limit.
func TestVirtualDisplayClose(t *testing.T) {
	requireDisplay(t)

	// Capture pre-close state.
	preID := sharedVDM.DisplayID
	if preID == 0 {
		t.Fatal("displayID is 0 before Close — precondition failed")
	}

	if err := sharedVDM.Close; err != nil {
		t.Fatalf("Close error: %v", err)
	}

	// All state must be reset after Close.
	if sharedVDM.DisplayID != 0 {
		t.Errorf("displayID=%d after Close, expected 0", sharedVDM.DisplayID)
	}

	sharedVDM.mu.Lock
	displayPtr := sharedVDM.display
	x, y, w, h := sharedVDM.x, sharedVDM.y, sharedVDM.w, sharedVDM.h
	sharedVDM.mu.Unlock

	if displayPtr != nil {
		t.Error("display pointer is non-nil after Close")
	}
	if x != 0 || y != 0 || w != 0 || h != 0 {
		t.Errorf("bounds not zeroed after Close: x=%d y=%d w=%d h=%d", x, y, w, h)
	}

	// Second Close must be idempotent (no panic, no error).
	if err := sharedVDM.Close; err != nil {
		t.Errorf("second Close returned error: %v", err)
	}

	t.Logf("Close state reset verified (pre-close displayID=%d)", preID)
}
