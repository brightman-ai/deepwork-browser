package browser

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
)

func TestTargetActivationWaitsForRuntimeInputWindow(t *testing.T) {
	tracker := NewTargetTracker(context.Background())
	tracker.SetBrowserInputKey(fmt.Sprintf("test-activation-%d-%d", os.Getpid(), time.Now().UnixNano()), 0)
	tracker.inputLockWait = 0
	held, err := tracker.acquireInputLock()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	if err := tracker.activateBrowserTarget("page", "manual_switch"); err == nil {
		t.Fatal("target activation escaped held runtime input window")
	}
	if err := tracker.activateBrowserTargetNoFront("page", "programmatic_nav_follow"); err == nil {
		t.Fatal("no-front activation escaped held runtime input window")
	}
	held.Release()
	if err := tracker.activateBrowserTarget("page", "manual_switch"); err != nil {
		t.Fatalf("activation did not recover after release: %v", err)
	}
}

func TestTargetEventCollectionDoesNotWaitForInputLock(t *testing.T) {
	tracker := NewTargetTracker(context.Background())
	tracker.SetBrowserInputKey(fmt.Sprintf("test-event-collection-%d-%d", os.Getpid(), time.Now().UnixNano()), 0)
	held, err := tracker.acquireInputLock()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	tracker.eventReady = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		tracker.enqueueBrowserEvent(targetTrackerBrowserEvent{kind: targetTrackerEventCreated, info: &target.Info{TargetID: "page", Type: "page"}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("event collection blocked on runtime input lock")
	}
	if len(tracker.eventQueue) != 1 {
		t.Fatal("event data was lost")
	}
}
