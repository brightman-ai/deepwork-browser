package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/brightman-ai/deepwork-browser/browser"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDocumentCLIContract(t *testing.T) {
	for _, args := range [][]string{
		{"https://example.com/", "--timeout=2s", "--max-html-bytes", "1234", "--max-network-bytes=5678"},
		{"--timeout", "2s", "--max-html-bytes=1234", "--max-network-bytes", "5678", "--", "https://example.com/"},
		{"https://example.com/", "--timeout=2s", "--max-html-bytes=1234", "--max-network-bytes=5678", "--network-route=system"},
	} {
		r, err := parseDocumentRequest(args)
		if err != nil || r.NetworkRoute != browser.NetworkRouteSystem || r.URL != "https://example.com/" || r.Timeout != 2*time.Second || r.MaxHTMLBytes != 1234 || r.MaxNetworkBytes != 5678 {
			t.Fatalf("request %+v err %v", r, err)
		}
	}
	for _, args := range [][]string{
		nil, {"https://example.com/", "second"},
		{"http://127.0.0.1/"}, {"file:///etc/passwd"},
		{"https://example.com/", "--id", "existing"},
		{"https://example.com/", "--allow-host", "example.com"},
		{"https://example.com/", "--mode", "visible"},
		{"https://example.com/", "--timeout", "0s"},
		{"https://example.com/", "--max-html-bytes", "-1"},
		{"https://example.com/", "--max-network-bytes", "0"},
		{"https://example.com/", "--max-html-bytes", "999999999999999999999"},
		{"https://example.com/", "--network-route", "http://proxy.example"},
	} {
		if _, err := parseDocumentRequest(args); err == nil {
			t.Fatalf("unsafe/invalid document arguments accepted: %v", args)
		}
	}
	if direct, err := parseDocumentRequest([]string{"https://example.com/", "--network-route", "direct"}); err != nil || direct.NetworkRoute != browser.NetworkRouteDirect {
		t.Fatalf("explicit direct route lost: %+v %v", direct, err)
	}
}

func TestDocumentFailureDiagnosticTransport(t *testing.T) {
	cause := context.DeadlineExceeded
	diagnostic := browser.DocumentFailureDiagnostic{Schema: browser.DocumentFailureSchema, RequestedURL: "https://example.com/start", Stage: "navigation", Reason: "timeout", Policy: browser.DocumentPolicy{NetworkRouting: browser.NetworkRouting{RequestedNetworkRoute: browser.NetworkRouteSystem, EffectiveTransport: "system-http-proxy-numeric-CONNECT", NetworkRouteSource: "macos-system", NumericTargetPinning: true, TrustedProxyDelegation: true}, Network: "page-requests-public-only"}, Cleanup: browser.DocumentCleanup{NetworkClosed: true}, Timing: browser.DocumentAcquisitionTiming{NavigationMS: 123, CleanupMS: 5, ElapsedMS: 128}}
	result, err := documentFailureResult(browser.DocumentRequest{URL: diagnostic.RequestedURL}, &browser.DocumentAcquisitionError{Diagnostic: diagnostic, Cause: cause})
	if !errors.Is(err, cause) {
		t.Fatalf("lost cause: %v", err)
	}
	path := filepath.Join(t.TempDir(), "result.json")
	if code := emitCLIResult(result, err, path); code != exitRunErr {
		t.Fatalf("failure exit=%d", code)
	}
	wire, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var got map[string]any
	if json.Unmarshal(wire, &got) != nil {
		t.Fatal(string(wire))
	}
	partial, ok := got["partial_result"].(map[string]any)
	if !ok || got["success"] != false || partial["schema"] != browser.DocumentFailureSchema || partial["usable"] != false || partial["requested_url"] != diagnostic.RequestedURL || partial["stage"] != "navigation" || partial["reason"] != "timeout" || partial["tool_version"] != version {
		t.Fatalf("lost failure receipt: %s", wire)
	}
	for _, key := range []string{"content", "representation", "url"} {
		if _, ok := partial[key]; ok {
			t.Fatalf("failure exposed fabricated/consumable field %s: %s", key, wire)
		}
	}
	if partial["policy"] == nil || partial["cleanup"] == nil || partial["timing"] == nil {
		t.Fatalf("receipt missing metadata: %s", wire)
	}
	policy := partial["policy"].(map[string]any)
	if policy["requested_network_route"] != "system" || policy["effective_transport"] != "system-http-proxy-numeric-CONNECT" || policy["network_route_source"] != "macos-system" || policy["numeric_target_pinning"] != true || policy["trusted_proxy_delegation"] != true {
		t.Fatalf("transport stripped network route receipt: %s", wire)
	}
}

func TestDocumentFailureJoinedUnsupportedCannotReroute(t *testing.T) {
	unsupported := &browser.UnsupportedDocumentContentError{URL: "https://example.com/feed", ContentType: "application/rss+xml", Reroutable: true}
	// Even an inner unsupported with permission must not grant a joined outcome
	// permission; this path deliberately exercises the direct-type CLI boundary.
	result, err := documentFailureResult(browser.DocumentRequest{URL: unsupported.URL}, errors.Join(unsupported, errors.New("cleanup failed")))
	if err == nil || result.Payload != nil {
		t.Fatalf("joined failure granted reroute payload: %+v %v", result, err)
	}
	pure, err := documentFailureResult(browser.DocumentRequest{URL: unsupported.URL}, unsupported)
	if err == nil || pure.Payload == nil {
		t.Fatalf("pure unsupported lost metadata: %+v %v", pure, err)
	}
}

func TestDocumentModeContract(t *testing.T) {
	for _, mode := range []browser.BrowserMode{browser.ModeHeadless, browser.ModeHeaded} {
		request, err := parseDocumentRequest([]string{"https://example.com/", "--mode", string(mode)})
		if err != nil || request.Mode != mode {
			t.Fatalf("mode %q: %+v %v", mode, request, err)
		}
	}
	for _, mode := range []string{"visible", "unknown", ""} {
		if _, err := parseDocumentRequest([]string{"https://example.com/", "--mode", mode}); err == nil {
			t.Fatalf("accepted document mode %q", mode)
		}
	}
	request, err := parseDocumentRequest([]string{"https://example.com/"})
	if err != nil || request.Mode != browser.ModeHeadless {
		t.Fatalf("document default: %+v %v", request, err)
	}
	for _, definition := range commandDefinitions() {
		if definition.Path != "document" {
			continue
		}
		for _, flag := range definition.Flags {
			if flag.Name == "--mode" {
				if len(flag.Values) != 2 || flag.Values[0] != "headless" || flag.Values[1] != "headed" {
					t.Fatalf("document inherited interactive modes: %+v", flag)
				}
				return
			}
		}
	}
	t.Fatal("document mode missing from command contract")
}
