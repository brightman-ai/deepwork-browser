package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDocumentAcquisitionFailureReceipt(t *testing.T) {
	complete := DocumentCleanup{BrowserClosed: true, ProfileRemoved: true, NetworkClosed: true}
	for _, tc := range []struct {
		name    string
		cause   error
		cleanup DocumentCleanup
		reason  string
	}{
		{"timeout", context.DeadlineExceeded, complete, "timeout"},
		{"canceled", context.Canceled, complete, "canceled"},
		{"cleanup", &documentCleanupFailure{errors.New("slot release failed")}, complete, "cleanup_failed"},
		{"network policy", &publicNetworkPolicyFailure{errors.New("byte budget")}, complete, "network_policy"},
		{"timeout and enforcement", errors.Join(context.DeadlineExceeded, &publicNetworkPolicyFailure{errors.New("byte budget")}), complete, "network_policy"},
		{"unsupported and cleanup", errors.Join(&UnsupportedDocumentContentError{ContentType: "application/rss+xml"}, &documentCleanupFailure{errors.New("browser close failed")}), complete, "cleanup_failed"},
		{"unsupported incomplete cleanup", &UnsupportedDocumentContentError{ContentType: "application/rss+xml"}, DocumentCleanup{NetworkClosed: true}, "cleanup_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt := newDocumentAttempt("https://example.com/request")
			attempt.advance("navigation")
			attempt.diagnostic.URL = "https://example.com/observed"
			attempt.beginCleanup(true)
			policy := DocumentPolicy{Network: "page-requests-public-only", NetworkBytes: 123}
			attempt.diagnostic.Policy, attempt.diagnostic.Cleanup = policy, tc.cleanup
			doc, err := settleDocumentReceipt(&Document{Content: "unusable partial"}, tc.cause, policy, tc.cleanup)
			doc, err = attempt.settle(doc, err)
			var failure *DocumentAcquisitionError
			if doc != nil || !errors.As(err, &failure) || !errors.Is(err, tc.cause) {
				t.Fatalf("lost nil document/error contract: %v %v", doc, err)
			}
			if _, direct := err.(*UnsupportedDocumentContentError); direct {
				t.Fatal("unsafe reroutable direct unsupported")
			}
			d := failure.Diagnostic
			if d.Reason != tc.reason || d.Stage != "navigation" || d.RequestedURL != "https://example.com/request" || d.URL != "https://example.com/observed" || d.Usable || d.Cleanup != tc.cleanup || d.Policy != policy {
				t.Fatalf("bad diagnostic: %+v", d)
			}
			wire, marshalErr := json.Marshal(d)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if strings.Contains(string(wire), `"content"`) || strings.Contains(string(wire), "unusable partial") {
				t.Fatalf("DOM leaked into failure: %s", wire)
			}
		})
	}
}

func TestDocumentAcquisitionPureUnsupportedStaysDirect(t *testing.T) {
	complete := DocumentCleanup{BrowserClosed: true, ProfileRemoved: true, NetworkClosed: true}
	policy := DocumentPolicy{Network: "page-requests-public-only"}
	unsupported := &UnsupportedDocumentContentError{URL: "https://example.com/feed", ContentType: "application/rss+xml"}
	attempt := newDocumentAttempt(unsupported.URL)
	doc, err := settleDocumentReceipt(nil, unsupported, policy, complete)
	doc, err = attempt.settle(doc, err)
	if doc != nil || err != unsupported || !unsupported.Reroutable || unsupported.Policy != policy || unsupported.Cleanup != complete {
		t.Fatalf("pure unsupported regressed: %v %+v", doc, err)
	}
}

func TestDocumentAcquisitionTimingUsesEnteredPhases(t *testing.T) {
	attempt := newDocumentAttempt("https://example.com/")
	for _, phase := range []string{"queue", "startup", "navigation", "read"} {
		attempt.advance(phase)
		attempt.phaseStarted = time.Now().Add(-2 * time.Millisecond)
	}
	attempt.beginCleanup(true)
	attempt.phaseStarted = time.Now().Add(-2 * time.Millisecond)
	_, err := attempt.settle(nil, context.DeadlineExceeded)
	d := err.(*DocumentAcquisitionError).Diagnostic
	for _, elapsed := range []float64{d.Timing.QueueMS, d.Timing.StartupMS, d.Timing.NavigationMS, d.Timing.ReadMS, d.Timing.CleanupMS} {
		if elapsed < 2 {
			t.Fatalf("phase timing missing: %+v", d.Timing)
		}
	}
	if d.Stage != "read" || d.Timing.ElapsedMS <= 0 {
		t.Fatalf("lost failed stage/elapsed: %+v", d)
	}
}

func TestDocumentAcquisitionPreStartupFailureIsTyped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	doc, err := FetchDocument(ctx, DocumentRequest{URL: "https://example.com/"})
	var failure *DocumentAcquisitionError
	if doc != nil || !errors.As(err, &failure) || !errors.Is(err, context.Canceled) {
		t.Fatalf("bad cancellation contract: %v %v", doc, err)
	}
	if failure.Diagnostic.Reason != "canceled" || failure.Diagnostic.URL != "" || failure.Diagnostic.Timing.CleanupMS != 0 {
		t.Fatalf("fabricated outcome: %+v", failure.Diagnostic)
	}
}

func TestDocumentFailureReasonSeparatesCleanupContextErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  string
	}{
		{"pure cleanup deadline", &documentCleanupFailure{context.DeadlineExceeded}, "cleanup_failed"},
		{"pure cleanup cancellation", &documentCleanupFailure{context.Canceled}, "cleanup_failed"},
		{"navigation failure and cleanup deadline", errors.Join(errors.New("navigation failed"), &documentCleanupFailure{context.DeadlineExceeded}), "cleanup_failed"},
		{"navigation failure and wrapped cleanup deadline", errors.Join(errors.New("navigation failed"), fmt.Errorf("outer: %w", &documentCleanupFailure{fmt.Errorf("close: %w", context.DeadlineExceeded)})), "cleanup_failed"},
		{"request deadline and cleanup deadline", errors.Join(fmt.Errorf("read: %w", context.DeadlineExceeded), &documentCleanupFailure{context.DeadlineExceeded}), "timeout"},
		{"request cancellation and cleanup deadline", errors.Join(fmt.Errorf("read: %w", context.Canceled), &documentCleanupFailure{context.DeadlineExceeded}), "canceled"},
		{"request deadline and cleanup failure", errors.Join(context.DeadlineExceeded, &documentCleanupFailure{errors.New("close failed")}), "timeout"},
		{"policy and cleanup deadline", errors.Join(&publicNetworkPolicyFailure{errors.New("network byte budget")}, &documentCleanupFailure{context.DeadlineExceeded}), "network_policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := documentFailureReason(tc.cause); got != tc.want {
				t.Fatalf("reason=%s want=%s cause=%v", got, tc.want, tc.cause)
			}
		})
	}
}

func TestDocumentCleanupContextReceiptStage(t *testing.T) {
	for _, failedRead := range []bool{false, true} {
		t.Run(fmt.Sprint(failedRead), func(t *testing.T) {
			attempt := newDocumentAttempt("https://example.com/")
			attempt.advance("navigation")
			attempt.beginCleanup(failedRead)
			var cause error = &documentCleanupFailure{context.DeadlineExceeded}
			wantStage := "cleanup"
			if failedRead {
				cause = errors.Join(errors.New("navigation failed"), cause)
				wantStage = "navigation"
			}
			doc, err := attempt.settle(nil, cause)
			failure := err.(*DocumentAcquisitionError)
			if doc != nil || failure.Diagnostic.Reason != "cleanup_failed" || failure.Diagnostic.Stage != wantStage || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("wrong cleanup provenance: %v %+v", doc, failure)
			}
		})
	}
}

func TestDocumentTransportFailureReason(t *testing.T) {
	transport := &publicNetworkTransportFailure{errors.New("upstream refused")}
	for _, tc := range []struct {
		name  string
		cause error
		want  string
	}{
		{"transport", transport, "network_transport"},
		{"transport and cleanup", errors.Join(transport, &documentCleanupFailure{context.DeadlineExceeded}), "network_transport"},
		{"request deadline and transport", errors.Join(context.DeadlineExceeded, transport), "timeout"},
		{"request cancellation and transport", errors.Join(context.Canceled, transport), "canceled"},
		{"policy and transport", errors.Join(&publicNetworkPolicyFailure{errors.New("byte budget")}, transport), "network_policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := documentFailureReason(tc.cause); got != tc.want {
				t.Fatalf("reason=%s want=%s cause=%v", got, tc.want, tc.cause)
			}
			if !errors.Is(tc.cause, transport.error) {
				t.Fatal("transport cause lost")
			}
		})
	}
}
