package browser

import (
	"context"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
)

func TestTargetTrackerBrowserEventIngressDoesNotBlockOnTargetCleanup(t *testing.T) {
	tracker := NewTargetTracker(nil)
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	tracker.targets["secondary"] = &trackedTarget{
		ID:  "secondary",
		URL: "http://example.test",
		Cancel: func() {
			close(cleanupStarted)
			<-releaseCleanup
		},
	}
	tracker.order = []target.ID{"secondary"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tracker.startBrowserEventLoop(ctx)

	started := time.Now()
	tracker.enqueueBrowserEvent(targetTrackerBrowserEvent{
		kind: targetTrackerEventDestroyed,
		id:   "secondary",
	})
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("browser event ingress blocked for %s", elapsed)
	}

	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("ordered browser event worker did not start target cleanup")
	}

	tracker.mu.RLock()
	_, stillTracked := tracker.targets["secondary"]
	tracker.mu.RUnlock()
	if stillTracked {
		t.Fatal("target remained in tracker while cleanup was pending")
	}
	close(releaseCleanup)
}
