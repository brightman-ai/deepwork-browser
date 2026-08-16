package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunActionWithTimeoutBoundsWorkerThatIgnoresCancellation(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := time.Now()
	_, err := runActionWithTimeout(context.Background(), 30*time.Millisecond, "focus @r2", func(context.Context) (*Snapshot, error) {
		<-release
		return nil, nil
	})
	if err == nil {
		t.Fatal("stuck action unexpectedly succeeded")
	}
	var timeoutErr *actionExecutionTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %T %v, want *actionExecutionTimeoutError", err, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded compatibility", err)
	}
	if !errors.Is(err, ErrActFailed) {
		t.Fatalf("error = %v, want ErrActFailed compatibility", err)
	}
	if !strings.Contains(err.Error(), `action "focus @r2" timed out`) {
		t.Fatalf("timeout error is not actionable: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("watchdog returned after %s, want bounded", elapsed)
	}
}
