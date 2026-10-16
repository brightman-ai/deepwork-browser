package browser

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDocumentRequestDefaultsAndValidation(t *testing.T) {
	r, err := normalizeDocumentRequest(DocumentRequest{URL: "https://example.com/article"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Timeout != 30*time.Second || r.MaxHTMLBytes != 8<<20 || r.MaxNetworkBytes != 32<<20 {
		t.Fatalf("wrong defaults: %+v", r)
	}
	for _, input := range []DocumentRequest{
		{URL: "file:///etc/passwd"}, {URL: "http://127.0.0.1/"}, {URL: "https://[::1]/"}, {URL: "https://user:secret@example.com/"}, {URL: "https://example.com:9222/"},
		{URL: "https://example.com/", Timeout: -time.Second}, {URL: "https://example.com/", MaxHTMLBytes: -1}, {URL: "https://example.com/", MaxNetworkBytes: -1},
	} {
		if _, err := normalizeDocumentRequest(input); err == nil {
			t.Errorf("accepted invalid request %+v", input)
		}
	}
	custom := DocumentRequest{URL: "https://example.com/", Timeout: time.Second, MaxHTMLBytes: 128, MaxNetworkBytes: 1024}
	got, err := normalizeDocumentRequest(custom)
	if err != nil || got != custom {
		t.Fatalf("custom budgets lost: %+v %v", got, err)
	}
}

func TestDocumentFetchRejectsBeforeBrowserStartup(t *testing.T) {
	document, err := FetchDocument(context.Background(), DocumentRequest{URL: "http://169.254.169.254/latest/meta-data/"})
	if err == nil || document != nil {
		t.Fatalf("unsafe request returned document=%+v err=%v", document, err)
	}
}

func TestDocumentQueueTwoSlotsAndDeadline(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireDocumentSlot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := acquireDocumentSlot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	third, err := acquireDocumentSlot(ctx, dir)
	if third != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third slot escaped limit: %v %v", third, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := acquireDocumentSlot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
}

func TestDocumentQueueAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	slot, err := acquireDocumentSlot(ctx, t.TempDir())
	if slot != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller acquired slot: %v %v", slot, err)
	}
}

func TestDocumentJSONContract(t *testing.T) {
	b, err := json.Marshal(Document{Schema: DocumentSchema, RequestedURL: "https://example.com/start", URL: "https://example.com/final", ContentType: "text/html", Readiness: DocumentReadiness{DocumentReadyState: "interactive", DOMReady: true}, Content: "<html></html>", Representation: "dom-html"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"schema", "requested_url", "url", "content_type", "readiness", "content", "representation", "policy", "cleanup"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing %s in %s", key, b)
		}
	}
	if _, ok := got["declared_canonical_url"]; ok {
		t.Error("absent DOM canonical must not become a synthesized authority")
	}
	if strings.Contains(string(b), "tool_version") {
		t.Error("kernel must not invent CLI version")
	}
}

func TestDocumentContentTypeBoundary(t *testing.T) {
	for _, input := range []string{"text/html", "text/html; charset=utf-8", "application/xhtml+xml"} {
		if !documentIsHTML(input) {
			t.Errorf("HTML rejected: %s", input)
		}
	}
	for _, input := range []string{"application/rss+xml", "application/atom+xml", "text/xml", "application/xml", "image/svg+xml", "application/pdf", "text/plain", ""} {
		if documentIsHTML(input) {
			t.Errorf("browser viewer could be mistaken for source: %s", input)
		}
	}
}

func TestUnsupportedReceiptRequiresPureOutcomeAndAllCleanup(t *testing.T) {
	complete := DocumentCleanup{BrowserClosed: true, ProfileRemoved: true, NetworkClosed: true}
	policy := DocumentPolicy{Network: "page-requests-public-only", BlockedRequests: 1}
	for _, tc := range []struct {
		name    string
		cleanup DocumentCleanup
		joined  bool
		allowed bool
	}{
		{"pure unsupported", complete, false, true},
		{"browser cleanup failed", DocumentCleanup{ProfileRemoved: true, NetworkClosed: true}, false, false},
		{"proxy cleanup failed", DocumentCleanup{BrowserClosed: true, ProfileRemoved: true}, false, false},
		{"joined policy or slot failure", complete, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsupported := &UnsupportedDocumentContentError{URL: "https://example.com/feed", ContentType: "application/rss+xml"}
			var resultErr error = unsupported
			if tc.joined {
				resultErr = errors.Join(resultErr, errors.New("enforcement or slot cleanup failed"))
			}
			doc, resultErr := settleDocumentReceipt(nil, resultErr, policy, tc.cleanup)
			if doc != nil || resultErr == nil || unsupported.Reroutable != tc.allowed {
				t.Fatalf("invalid fallback permission doc=%v err=%v unsupported=%+v", doc, resultErr, unsupported)
			}
			if tc.allowed && (unsupported.Cleanup != complete || unsupported.Policy.Network != policy.Network) {
				t.Fatalf("missing settled receipt: %+v", unsupported)
			}
		})
	}
}
