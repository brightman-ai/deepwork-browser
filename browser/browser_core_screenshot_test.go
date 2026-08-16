package browser

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunScreenshotWithTimeoutCancelsStuckCapture(t *testing.T) {
	captureCanceled := make(chan struct{})
	started := time.Now()
	_, err := runScreenshotWithTimeout(
		context.Background(),
		context.Background(),
		20*time.Millisecond,
		func(ctx context.Context) ([]byte, error) {
			<-ctx.Done()
			close(captureCanceled)
			return nil, ctx.Err()
		},
	)
	if !errors.Is(err, errScreenshotTimeout) {
		t.Fatalf("runScreenshotWithTimeout() error = %v, want errScreenshotTimeout", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("stuck screenshot returned after %s, want <= 500ms", elapsed)
	}
	select {
	case <-captureCanceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed-out screenshot capture did not observe context cancellation")
	}
}

func TestRunScreenshotWithTimeoutReturnsImage(t *testing.T) {
	want := []byte("jpeg")
	got, err := runScreenshotWithTimeout(
		context.Background(),
		context.Background(),
		time.Second,
		func(context.Context) ([]byte, error) { return want, nil },
	)
	if err != nil {
		t.Fatalf("runScreenshotWithTimeout() error: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("runScreenshotWithTimeout() = %q, want %q", got, want)
	}
}

func TestTargetWebSocketURLUsesExistingPageIdentity(t *testing.T) {
	got, err := targetWebSocketURL(
		"ws://127.0.0.1:25137/devtools/browser/browser-owner?ignored=1",
		"ABC123",
	)
	if err != nil {
		t.Fatalf("targetWebSocketURL() error: %v", err)
	}
	const want = "ws://127.0.0.1:25137/devtools/page/ABC123"
	if got != want {
		t.Fatalf("targetWebSocketURL() = %q, want %q", got, want)
	}
}

func TestTargetWebSocketURLRejectsAmbiguousTargetIdentity(t *testing.T) {
	if _, err := targetWebSocketURL("ws://127.0.0.1:25137/devtools/browser/owner", "../other"); err == nil {
		t.Fatal("targetWebSocketURL() accepted an ambiguous target ID")
	}
}
