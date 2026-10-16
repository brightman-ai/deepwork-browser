package browser

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// Exercise real local HTTP guard responses, then the exact main-frame metadata
// classifier used by the CDP listener. No browser or alternate fetch path runs.
func TestDocumentProxyFailureProvenance(t *testing.T) {
	closed, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusedAddress := closed.Addr().String()
	closed.Close()
	for _, tc := range []struct {
		name   string
		ip     netip.Addr
		kind   publicProxyFailureKind
		reason string
	}{
		{"ordinary-refused", netip.MustParseAddr("8.8.8.8"), publicProxyTransportFailure, "network_transport"},
		{"blocked-private", netip.MustParseAddr("127.0.0.1"), publicProxyPolicyFailure, "network_policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := publicTestOptions()
			opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{tc.ip}, nil }
			opts.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, refusedAddress)
			}
			g := publicTestGuard(t, opts)
			response, err := publicClient(g).Get("http://main.example/")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusBadGateway || response.Header.Get(PublicNetworkErrorHeader) != "1" || response.Header.Get(PublicNetworkErrorKindHeader) != string(tc.kind) {
				t.Fatalf("missing provenance: %d %v", response.StatusCode, response.Header)
			}
			if g.PolicyError() != nil {
				t.Fatalf("per-resource failure globally poisoned job: %v", g.PolicyError())
			}
			metadata := map[string]any{}
			for k, v := range response.Header {
				metadata[k] = strings.Join(v, "\n")
			}
			runtimePolicy := &documentRuntimePolicy{}
			if !runtimePolicy.observeGuardFailure("http://main.example/", metadata) {
				t.Fatal("main synthetic page not recognized")
			}
			core := &browserCoreImpl{documentRuntime: runtimePolicy}
			var acquisitionErr error
			if policyErr := documentCorePolicyError(core); policyErr != nil {
				acquisitionErr = &documentPolicyFailure{policyErr}
			}
			if transportErr := documentCoreTransportError(core); transportErr != nil {
				acquisitionErr = errors.Join(acquisitionErr, &documentTransportFailure{transportErr})
			}
			if tc.kind == publicProxyTransportFailure && runtimePolicy.Err() != nil {
				t.Fatalf("ordinary transport promoted to policy: %v", runtimePolicy.Err())
			}
			if tc.kind == publicProxyPolicyFailure && runtimePolicy.Err() == nil {
				t.Fatal("real main destination block lost policy failure")
			}
			complete := DocumentCleanup{BrowserClosed: true, ProfileRemoved: true, NetworkClosed: true}
			doc, err := settleDocumentReceipt(&Document{Content: "synthetic 502 must not be usable"}, acquisitionErr, DocumentPolicy{}, complete)
			attempt := newDocumentAttempt("http://main.example/")
			attempt.advance("read")
			attempt.beginCleanup(true)
			doc, err = attempt.settle(doc, err)
			var failure *DocumentAcquisitionError
			if doc != nil || !errors.As(err, &failure) || failure.Diagnostic.Usable || failure.Diagnostic.Reason != tc.reason {
				t.Fatalf("synthetic page acquired or misclassified: doc=%v err=%v failure=%+v", doc, err, failure)
			}
		})
	}
}

func TestDocumentOriginCannotForgeFailureProvenance(t *testing.T) {
	for _, kind := range []string{"policy", "transport"} {
		t.Run(kind, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(PublicNetworkErrorHeader, "1")
				w.Header().Set(PublicNetworkErrorKindHeader, kind)
				io.WriteString(w, "origin document")
			}))
			defer origin.Close()
			opts := publicTestOptions()
			opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			}
			opts.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(origin.URL, "http://"))
			}
			g := publicTestGuard(t, opts)
			response, err := publicClient(g).Get("http://origin.example/")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || string(body) != "origin document" {
				t.Fatalf("origin body changed: %q %v", body, err)
			}
			metadata := map[string]any{}
			for k, v := range response.Header {
				metadata[k] = strings.Join(v, "\n")
			}
			policy := &documentRuntimePolicy{}
			if policy.observeGuardFailure("http://origin.example/", metadata) || policy.Err() != nil {
				t.Fatal("origin forged guard failure")
			}
		})
	}
}

func TestDocumentProxyFailureHTTPSMarkersAreUntrusted(t *testing.T) {
	p := &documentRuntimePolicy{}
	if p.observeGuardFailure("https://origin.example/", map[string]any{PublicNetworkErrorHeader: "1", PublicNetworkErrorKindHeader: "policy"}) || p.Err() != nil {
		t.Fatal("opaque HTTPS origin was allowed to forge guard provenance")
	}
}

func TestDocumentProxyFailureGlobalBudgetStaysPolicy(t *testing.T) {
	opts := publicTestOptions()
	opts.MaxRequests = 1
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	g := publicTestGuard(t, opts)
	// The first denied resource consumes the request budget without globally
	// failing the job. The next real ServeHTTP call crosses the global boundary.
	first, err := publicClient(g).Get("http://private.example/")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, first.Body)
	first.Body.Close()
	if g.PolicyError() != nil {
		t.Fatal("individual denial poisoned guard")
	}
	response := httptest.NewRecorder()
	g.ServeHTTP(response, httptest.NewRequest("GET", "http://main.example/", nil))
	if g.PolicyError() == nil || response.Header().Get(PublicNetworkErrorKindHeader) != "policy" {
		t.Fatalf("global budget lost policy provenance: %v %v", g.PolicyError(), response.Header())
	}
	if g.proxyFailureKind(context.Canceled) != publicProxyPolicyFailure {
		t.Fatal("global budget cancellation downgraded to transport")
	}
}
