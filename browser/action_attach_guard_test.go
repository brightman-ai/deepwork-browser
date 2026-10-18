package browser

import (
	"context"
	"errors"
	"testing"
)

// Attached headed actions must validate display containment without invoking
// Page.bringToFront. This test exercises the policy seam used by the
// cross-process attach path; it does not launch Chrome or touch a display.
func TestAttachedHeadedActivationGuardFailsClosed(t *testing.T) {
	called := 0
	guardErr := errors.New("display containment lost")
	e := newActionEngine(newSnapshotEngine())
	e.targetActivation = func(context.Context) (bool, error) {
		called++
		return false, guardErr
	}
	brought, err := e.targetActivation(context.Background())
	if !errors.Is(err, guardErr) || brought {
		t.Fatalf("guard result = brought=%t err=%v", brought, err)
	}
	if called != 1 {
		t.Fatalf("guard called %d times, want 1", called)
	}
}

func TestAttachedHeadedActivationGuardDoesNotReportBringToFront(t *testing.T) {
	e := newActionEngine(newSnapshotEngine())
	e.targetActivation = func(context.Context) (bool, error) { return false, nil }
	brought, err := e.targetActivation(context.Background())
	if err != nil || brought {
		t.Fatalf("attached no-front activation = brought=%t err=%v", brought, err)
	}
}
