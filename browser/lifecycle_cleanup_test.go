package browser

import (
	"context"
	"testing"
)

func Test_TC09_LifecycleCleanupInvokesBrowserCoreCancelFuncs(t *testing.T) {
	var browserCanceled, allocCanceled int
	impl := &browserCoreImpl{
		browserCancel: func() { browserCanceled++ },
		allocCancel:   func() { allocCanceled++ },
	}

	if err := impl.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if browserCanceled != 1 {
		t.Fatalf("browserCancel calls = %d, want 1", browserCanceled)
	}
	if allocCanceled != 1 {
		t.Fatalf("allocCancel calls = %d, want 1", allocCanceled)
	}
	if impl.browserCancel != nil {
		t.Fatalf("browserCancel should be cleared")
	}
	if impl.allocCancel != nil {
		t.Fatalf("allocCancel should be cleared")
	}
}

func Test_TC09_LifecycleCleanupInvokesLaunchCleanupCancelFuncs(t *testing.T) {
	var browserCanceled, allocCanceled int
	entry := &chromePoolEntry{
		browserCancel: func() { browserCanceled++ },
		allocCancel:   func() { allocCanceled++ },
	}

	cleanupEntryLaunch(entry)

	if browserCanceled != 1 {
		t.Fatalf("browserCancel calls = %d, want 1", browserCanceled)
	}
	if allocCanceled != 1 {
		t.Fatalf("allocCancel calls = %d, want 1", allocCanceled)
	}
	if entry.browserCancel != nil {
		t.Fatalf("browserCancel should be cleared")
	}
	if entry.allocCancel != nil {
		t.Fatalf("allocCancel should be cleared")
	}
}
