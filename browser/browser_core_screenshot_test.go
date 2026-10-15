package browser

import (
	"bytes"
	"context"
	"errors"
	"strings"
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

func TestScreenshotTimeoutRetainsPageStateAndRetryDecision(t *testing.T) {
	for _, tc := range []struct {
		name     string
		probe    pageLoadProbe
		probeErr error
		retry    bool
	}{
		{"blocked_initial_render", pageLoadProbe{ReadyState: "loading", VisibilityState: "visible", PendingStylesheets: 1}, nil, false},
		{"body_waiting_for_styles", pageLoadProbe{ReadyState: "loading", HasBody: true, BodyTextLength: 24, PendingStylesheets: 1}, nil, false},
		{"finished_image_only", pageLoadProbe{ReadyState: "complete", HasBody: true, BodyChildCount: 1}, nil, true},
		{"already_painted_loading", pageLoadProbe{ReadyState: "loading", HasBody: true, PaintCount: 1}, nil, true},
		{"probe_unavailable", pageLoadProbe{}, errors.New("connection closed"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &screenshotCaptureError{cause: errScreenshotTimeout, probe: tc.probe, probeErr: tc.probeErr}
			if !errors.Is(err, errScreenshotTimeout) {
				t.Fatal("timeout identity lost")
			}
			if got := screenshotRetryUseful(err); got != tc.retry {
				t.Fatalf("retry=%t, want %t: %v", got, tc.retry, err)
			}
			if tc.probeErr == nil && !strings.Contains(err.Error(), "document_ready="+tc.probe.ReadyState) {
				t.Fatalf("missing sampled page state: %v", err)
			}
		})
	}
	transportErr := &screenshotCaptureError{cause: errors.New("websocket disconnected"), probe: pageLoadProbe{ReadyState: "loading"}}
	if !screenshotRetryUseful(transportErr) {
		t.Fatal("transport failures must remain eligible for isolated reconnection")
	}
}

func TestCaptureDiagnosticsCannotPreventSuccessfulScreenshot(t *testing.T) {
	want := []byte("real capture bytes")
	got, err := captureWithFailureDiagnostics(func() ([]byte, error) {
		return want, nil
	}, func() (pageLoadProbe, error) {
		t.Fatal("successful screenshot must not wait for or require Runtime diagnostics")
		return pageLoadProbe{}, context.DeadlineExceeded
	})
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("successful capture lost: %q, %v", got, err)
	}
	_, err = captureWithFailureDiagnostics(func() ([]byte, error) {
		return nil, errScreenshotTimeout
	}, func() (pageLoadProbe, error) {
		return pageLoadProbe{}, context.DeadlineExceeded
	})
	if !errors.Is(err, errScreenshotTimeout) || !screenshotRetryUseful(err) || !strings.Contains(err.Error(), "page_state=unavailable") {
		t.Fatalf("failed diagnostic replaced capture cause or disabled recovery: %v", err)
	}
}
