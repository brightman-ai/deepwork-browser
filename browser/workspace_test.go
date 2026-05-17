// Package browser — Workspace cross-platform contract tests.
//
// Contract invariants (DDC-I-11, DDC-I-12, DDC-I-21, refined TH-0419):
//   - NewWorkspace() must not panic on any supported platform
//   - EnsureSpace() must return (id, nil) or (0, err) — never panic
//   - LaunchChromeInSpace() with invalid spec returns error without spawning
//   - Close() must be safe to call multiple times
//
// Real Chrome launch verification is in integration tests (need Chrome binary).
//
// [cross-platform bug class: DDC-I-12]
package browser

import (
	"runtime"
	"testing"
)

// TestWorkspaceNewDoesNotPanic verifies constructor is safe on all platforms.
func TestWorkspaceNewDoesNotPanic(t *testing.T) {
	t.Logf("platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	ws := NewWorkspace()
	if ws == nil {
		t.Fatal("NewWorkspace() returned nil")
	}
}

// TestWorkspaceCloseIdempotent verifies double-Close does not panic.
func TestWorkspaceCloseIdempotent(t *testing.T) {
	ws := NewWorkspace()
	if err := ws.Close(); err != nil {
		t.Logf("first Close: %v (acceptable)", err)
	}
	if err := ws.Close(); err != nil {
		t.Logf("second Close: %v (acceptable)", err)
	}
}

// TestWorkspaceEnsureSpaceDoesNotPanic verifies EnsureSpace is safe.
// On macOS with ≥2 Spaces, returns (id>0, nil).
// On macOS with 1 Space, returns (0, err) with descriptive message.
// On Linux, returns (0, nil) — Xvfb provides isolation.
// On Windows, returns (0, nil) — stub.
func TestWorkspaceEnsureSpaceDoesNotPanic(t *testing.T) {
	ws := NewWorkspace()
	defer ws.Close()

	id, err := ws.EnsureSpace()
	t.Logf("EnsureSpace() -> id=%d, err=%v [OS=%s]", id, err, runtime.GOOS)

	switch runtime.GOOS {
	case "darwin":
		if err != nil {
			// Acceptable: user may have only 1 Space.
			t.Logf("macOS EnsureSpace failed (may need ≥2 Spaces in Mission Control): %v", err)
		} else if id <= 0 {
			t.Errorf("macOS EnsureSpace returned id=%d with nil error — expected id>0", id)
		}
	case "linux":
		if err != nil {
			t.Errorf("Linux EnsureSpace unexpected error: %v", err)
		}
	case "windows":
		if err != nil {
			t.Errorf("Windows EnsureSpace stub unexpected error: %v", err)
		}
	}
}

// TestWorkspaceLaunchChromeInSpaceRejectsInvalidSpec verifies the contract
// guard: empty ChromePath → error, no process spawned.
func TestWorkspaceLaunchChromeInSpaceRejectsInvalidSpec(t *testing.T) {
	ws := NewWorkspace()
	defer ws.Close()

	if runtime.GOOS == "darwin" {
		// EnsureSpace must succeed before LaunchChromeInSpace can run on darwin.
		if _, err := ws.EnsureSpace(); err != nil {
			t.Skipf("darwin EnsureSpace failed (need ≥2 Spaces): %v", err)
		}
	}

	// Empty ChromePath → rejected before any fork.
	_, err := ws.LaunchChromeInSpace(ChromeLaunchSpec{
		ChromePath: "",
		DebugPort:  9999,
	})
	if err == nil {
		t.Fatal("LaunchChromeInSpace with empty ChromePath should error")
	}
	t.Logf("LaunchChromeInSpace(empty path) correctly rejected: %v", err)

	// Invalid debug port → rejected before any fork.
	_, err = ws.LaunchChromeInSpace(ChromeLaunchSpec{
		ChromePath: "/usr/bin/false",
		DebugPort:  0,
	})
	if err == nil {
		t.Fatal("LaunchChromeInSpace with DebugPort=0 should error")
	}
	t.Logf("LaunchChromeInSpace(port=0) correctly rejected: %v", err)
}

// TestWorkspaceLinuxDoesNotTouchXvfb verifies Linux workspace is truly a no-op.
// [Iron Rule: Linux display_manager.go::ensureDisplayLinux() must not be modified]
func TestWorkspaceLinuxDoesNotTouchXvfb(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only test")
	}
	ws := NewWorkspace()
	id, err := ws.EnsureSpace()
	if err != nil {
		t.Fatalf("Linux Workspace.EnsureSpace() returned error: %v", err)
	}
	if id != 0 {
		t.Fatalf("Linux Workspace.EnsureSpace() returned id=%d, want 0 (Xvfb handles isolation)", id)
	}
}
