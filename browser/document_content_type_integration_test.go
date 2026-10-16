//go:build integration

package browser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// Real Chrome receives actual repeated HTTP fields through the production
// guard. Only the validated public fixture destination's final dial is routed
// to this test's owned local listener; no policy or parsing implementation is mocked.
func TestDocumentDuplicateContentTypeLive(t *testing.T) {
	cases := []struct {
		name, first, second, want string
		conflict                  bool
		differentCase             bool
	}{
		{name: "equivalent", first: "text/html; charset=UTF-8", second: "text/html; charset=UTF-8", want: "text/html; charset=utf-8"},
		{name: "case-equivalent", first: "TEXT/HTML; CHARSET=\"UTF-8\"", second: "text/html; charset=utf-8", want: "text/html; charset=utf-8", differentCase: true},
		{name: "missing-charset-and-extra", first: "text/html; x-extra=KeepCase", second: "text/html; charset=UTF-8", want: "text/html; charset=utf-8; x-extra=KeepCase"},
		{name: "media-conflict", first: "text/html", second: "application/rss+xml", conflict: true, differentCase: true},
		{name: "charset-conflict", first: "text/html; charset=UTF-8", second: "text/html; charset=ISO-8859-1", conflict: true},
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, tc := range cases {
			if r.URL.Path == "/"+tc.name {
				w.Header()["Content-Type"] = []string{tc.first}
				if tc.differentCase {
					w.Header()["content-type"] = []string{tc.second}
				} else {
					w.Header().Add("Content-Type", tc.second)
				}
				fmt.Fprint(w, "<!doctype html><p>Duplicate header fixture</p>")
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer fixture.Close()
	publicIP := netip.MustParseAddr("93.184.215.14")
	opts := PublicNetworkOptions{
		lookupIP: func(ctx context.Context, host string) ([]netip.Addr, error) {
			if host == "headers.example" {
				return []netip.Addr{publicIP}, nil
			}
			return nil, fmt.Errorf("fixture does not resolve %s", host)
		},
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != "93.184.215.14:80" {
				return nil, fmt.Errorf("unexpected fixture destination %s", address)
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", fixture.Listener.Addr().String())
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := fetchDocumentWithNetwork(context.Background(), DocumentRequest{URL: "http://headers.example/" + tc.name, Timeout: 15 * time.Second}, opts)
			if tc.conflict {
				var invalid *DocumentContentTypeError
				var unsupported *UnsupportedDocumentContentError
				if doc != nil || !errors.As(err, &invalid) || errors.As(err, &unsupported) {
					t.Fatalf("conflicting HTTP fields did not fail closed: doc=%v error=%v", doc != nil, err)
				}
				if !strings.Contains(invalid.Value, tc.first) || !strings.Contains(invalid.Value, tc.second) {
					t.Fatalf("observed conflicting metadata lost: %q", invalid.Value)
				}
				t.Logf("typed conflicting Content-Type rejection: %q", invalid.Value)
				return
			}
			if err != nil || doc == nil {
				t.Fatalf("equivalent repeated HTTP fields rejected: %v", err)
			}
			if doc.ContentType != tc.want || !strings.Contains(doc.Content, "Duplicate header fixture") {
				t.Fatalf("normalization/DOM failed: type=%q bytes=%d", doc.ContentType, len(doc.Content))
			}
			if !doc.Cleanup.BrowserClosed || !doc.Cleanup.ProfileRemoved || !doc.Cleanup.NetworkClosed {
				t.Fatalf("cleanup incomplete: %+v", doc.Cleanup)
			}
			t.Logf("type=%q bytes=%d cleanup=%+v", doc.ContentType, len(doc.Content), doc.Cleanup)
		})
	}
}
