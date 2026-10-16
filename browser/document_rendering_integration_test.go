//go:build integration

package browser

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// Opt-in real browser acceptance, including a headful hidden target. The
// production proxy/runtime/read/cleanup execute unchanged; only public fixture
// DNS and the final numeric dial are mapped to this owned HTTP listener.
// Run under the macOS window/focus monitor used for headed acceptance.
func TestDocumentRenderingModesLive(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><p id="article">initial</p><script>document.getElementById('article').textContent='rendered document fixture';fetch('http://127.0.0.1:1/private').catch(()=>{});</script>`)
	}))
	defer fixture.Close()
	opts := PublicNetworkOptions{
		lookupIP: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host != "rendering.example" {
				return nil, fmt.Errorf("unexpected fixture host %s", host)
			}
			return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
		},
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "93.184.215.14:80" {
				return nil, fmt.Errorf("unexpected fixture dial %s", address)
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", fixture.Listener.Addr().String())
		},
	}
	for _, mode := range []BrowserMode{ModeHeadless, ModeHeaded} {
		t.Run(string(mode), func(t *testing.T) {
			doc, err := fetchDocumentWithNetwork(context.Background(), DocumentRequest{URL: "http://rendering.example/article", Mode: mode, NetworkRoute: DocumentNetworkDirect, Timeout: 20 * time.Second}, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(doc.Content, `>rendered document fixture</p>`) {
				t.Fatalf("site JavaScript DOM was not read: %s", doc.Content)
			}
			if doc.Rendering.Mode != mode || doc.Rendering.Surface != documentTargetSurface(mode) || doc.Policy.Profile != documentProfilePolicy(mode) {
				t.Fatalf("inconsistent rendering receipt: %+v %+v", doc.Rendering, doc.Policy)
			}
			if doc.Policy.Network != "page-requests-public-only" || doc.Policy.BlockedRequests == 0 {
				t.Fatalf("private destination guard lost: %+v", doc.Policy)
			}
			if !doc.Cleanup.BrowserClosed || !doc.Cleanup.ProfileRemoved || !doc.Cleanup.NetworkClosed {
				t.Fatalf("cleanup incomplete: %+v", doc.Cleanup)
			}
		})
	}
}
