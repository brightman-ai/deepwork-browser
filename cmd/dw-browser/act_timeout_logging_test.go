package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func TestRunCLIActionWithTimeoutBoundsStuckWorker(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := time.Now()
	_, err := runCLIActionWithTimeout(context.Background(), 30*time.Millisecond, "focus @r2", func(context.Context) (*browser.Snapshot, error) {
		<-release // deliberately ignore cancellation, like a wedged CDP call
		return nil, nil
	})
	if err == nil {
		t.Fatal("stuck action unexpectedly succeeded")
	}
	var timeoutErr *cliActTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %T %v, want *cliActTimeoutError", err, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded compatibility", err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("watchdog returned after %s, want bounded", elapsed)
	}
}

func TestRunCLIActionWithTimeoutReturnsSuccessfulResult(t *testing.T) {
	want := &browser.Snapshot{URL: "https://example.test/"}
	got, err := runCLIActionWithTimeout(context.Background(), time.Second, "focus @r1", func(context.Context) (*browser.Snapshot, error) {
		return want, nil
	})
	if err != nil {
		t.Fatalf("runCLIActionWithTimeout: %v", err)
	}
	if got != want {
		t.Fatalf("snapshot = %#v, want %#v", got, want)
	}
}

func TestVerboseRequestedAndAbsoluteWheelViewportInvalidation(t *testing.T) {
	if verboseRequested([]string{"act", "--id", "x"}) {
		t.Fatal("verboseRequested enabled without --verbose")
	}
	if !verboseRequested([]string{"act", "--verbose", "--id", "x"}) {
		t.Fatal("verboseRequested missed --verbose")
	}
	if !actionMovesWitnessViewport("wheelat 120,200 down 2") {
		t.Fatal("absolute wheelat should invalidate viewport refs")
	}
	if actionMovesWitnessViewport("wheelat css=#chart 50% 50% -240") {
		t.Fatal("selector-relative canvas wheelat should not imply page viewport movement")
	}
}
