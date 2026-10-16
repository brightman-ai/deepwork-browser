//go:build integration

package browser

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Exercise Chrome's actual failed HTTPS CONNECT navigation, where response
// headers are unavailable to CDP. The owned resolver and proxy remain real.
func TestDocumentDoHHTTPSFailureLive(t *testing.T) {
	f := newOwnedDOHFixtureForNames(t, func(w http.ResponseWriter, _ *http.Request, q dnsmessage.Message) {
		m := dnsTestResponse(q)
		m.RCode = dnsmessage.RCodeServerFailure
		body, err := m.Pack()
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(body)
	}, false, func(string) bool {
		// Chrome's own background requests share the same public guard. Return
		// SERVFAIL for those as well; only failed main-navigation attribution is
		// asserted below. Resource-only tests retain their strict hostname check.
		return true
	})
	doc, err := fetchDocumentWithNetwork(context.Background(), DocumentRequest{
		URL: "https://asset.example/article", DNSResolver: f.resolver,
		NetworkRoute: NetworkRouteSystem, Mode: ModeHeadless, Timeout: 15 * time.Second,
	}, f.options.network)
	var failure *DocumentAcquisitionError
	if doc != nil || !errors.As(err, &failure) {
		t.Fatalf("expected typed failed document, got doc=%v err=%v", doc, err)
	}
	d := failure.Diagnostic
	if d.Reason != "dns_resolution" || d.Policy.DNSResolver != f.resolver ||
		!d.Cleanup.BrowserClosed || !d.Cleanup.ProfileRemoved || !d.Cleanup.NetworkClosed {
		t.Fatalf("actual Chrome navigation lost DNS cause/cleanup: %+v (%v)", d, err)
	}
	if f.originHits.Load() != 0 || f.queries.Load() == 0 {
		t.Fatalf("unexpected acquisition: queries=%d origin=%d", f.queries.Load(), f.originHits.Load())
	}
}
