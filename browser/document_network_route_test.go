package browser

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDocumentNetworkRouteConfiguration(t *testing.T) {
	publicTestEnvironment(t)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1080")
	route, err := resolveDocumentNetworkRoute("")
	if err != nil || route.httpsProxy != "127.0.0.1:1080" || route.httpProxy != "" || route.receipt.NetworkRouteSource != "environment" || !route.receipt.TrustedProxyDelegation {
		t.Fatalf("route %+v %v", route, err)
	}
	for _, bad := range []string{"socks5://127.0.0.1:1080", "https://proxy.example:443", "http://user:secret@proxy.example", "http://proxy.example/path", "http://proxy.example:0"} {
		t.Setenv("HTTPS_PROXY", bad)
		if _, err := resolveDocumentNetworkRoute(DocumentNetworkSystem); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe proxy accepted or credential exposed: %v", err)
		}
		if direct, err := resolveDocumentNetworkRoute(DocumentNetworkDirect); err != nil || direct.httpsProxy != "" || direct.receipt.TrustedProxyDelegation {
			t.Fatalf("direct did not ignore env %+v %v", direct, err)
		}
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1080")
	t.Setenv("https_proxy", "http://127.0.0.1:1081")
	if _, err := resolveDocumentNetworkRoute(DocumentNetworkSystem); err == nil {
		t.Fatal("conflicting case variants accepted")
	}
}

func TestDocumentNetworkRouteStaticSystemSettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings documentSystemProxySettings
		reject   bool
	}{
		{"http-and-socks", documentSystemProxySettings{HTTPEnabled: true, HTTPSEnabled: true, SOCKSEnabled: true, HTTPHost: "127.0.0.1", HTTPSHost: "127.0.0.1", HTTPPort: 1080, HTTPSPort: 1081}, false},
		{"pac", documentSystemProxySettings{PACEnabled: true}, true},
		{"discovery", documentSystemProxySettings{AutoDiscoveryEnabled: true}, true},
		{"socks-only", documentSystemProxySettings{SOCKSEnabled: true}, true},
		{"invalid-port", documentSystemProxySettings{HTTPEnabled: true, HTTPHost: "proxy.example", HTTPPort: 0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &documentRouteSnapshot{}
			err := s.applySystemSettings(tc.settings)
			if (err != nil) != tc.reject {
				t.Fatalf("route %+v err %v", s, err)
			}
			if !tc.reject && (s.httpProxy != "127.0.0.1:1080" || s.httpsProxy != "127.0.0.1:1081") {
				t.Fatalf("scheme-specific routes lost: %+v", s)
			}
		})
	}
}

func tunnelFixture(t *testing.T, serve func(net.Conn)) func(context.Context, string, string) (net.Conn, error) {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); defer server.Close(); serve(server) }()
	t.Cleanup(func() {
		client.Close()
		server.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("owned proxy fixture did not exit")
		}
	})
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "127.0.0.1:1080" {
			t.Errorf("unexpected transport endpoint %s", address)
		}
		return client, nil
	}
}

func TestDocumentProxyTunnelPreservesBufferedBytesAndLifetime(t *testing.T) {
	dial := tunnelFixture(t, func(c net.Conn) {
		r, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			t.Error(err)
			return
		}
		if r.Method != "CONNECT" || r.Host != "8.8.8.8:443" || r.URL.Host != "8.8.8.8:443" {
			t.Errorf("not numeric CONNECT: %+v", r)
		}
		fmt.Fprint(c, "HTTP/1.1 200 Connection Established\r\n\r\nfirst-tunnel-bytes")
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
			t.Errorf("tunnel input %q %v", b, err)
			return
		}
		fmt.Fprint(c, "pong")
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	c, err := dialDocumentProxyTunnel(ctx, "127.0.0.1:1080", "8.8.8.8:443", dial)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer c.Close()
	cancel() // As in g.dial: a successful tunnel must outlive attempt cancellation.
	b := make([]byte, len("first-tunnel-bytes"))
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "first-tunnel-bytes" {
		t.Fatalf("lost buffered bytes %q %v", b, err)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	b = make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "pong" {
		t.Fatalf("tunnel was cancelled %q %v", b, err)
	}
}

func TestDocumentProxyTunnelRejectsResponseAndHostname(t *testing.T) {
	for _, response := range []string{"HTTP/1.1 407 Proxy Authentication Required\r\n\r\n", "HTTP/1.1 302 Redirect\r\nLocation: http://127.0.0.1/\r\n\r\n", "HTTP/1.1 200 OK\r\nX-Large: " + strings.Repeat("x", 65<<10) + "\r\n\r\n", ""} {
		t.Run(fmt.Sprintf("bytes-%d", len(response)), func(t *testing.T) {
			dial := tunnelFixture(t, func(c net.Conn) { _, _ = http.ReadRequest(bufio.NewReader(c)); fmt.Fprint(c, response) })
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if c, err := dialDocumentProxyTunnel(ctx, "127.0.0.1:1080", "8.8.8.8:443", dial); err == nil {
				c.Close()
				t.Fatal("accepted rejected/incomplete CONNECT")
			}
		})
	}
	for _, target := range []string{"public.example:443", "127.0.0.1:443", "[::ffff:127.0.0.1]:443"} {
		var calls int
		_, err := dialDocumentProxyTunnel(context.Background(), "127.0.0.1:1080", target, func(context.Context, string, string) (net.Conn, error) {
			calls++
			return nil, errors.New("unexpected dial")
		})
		if err == nil || calls != 0 {
			t.Fatalf("unsafe target contacted proxy: %s %d %v", target, calls, err)
		}
	}
}

func TestDocumentProxyTunnelCancellationAndDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			entered := make(chan struct{})
			dial := tunnelFixture(t, func(c net.Conn) {
				_, _ = http.ReadRequest(bufio.NewReader(c))
				close(entered)
				_, _ = io.Copy(io.Discard, c)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if deadline {
				var c context.CancelFunc
				ctx, c = context.WithTimeout(ctx, 30*time.Millisecond)
				defer c()
			}
			done := make(chan error, 1)
			go func() { _, err := dialDocumentProxyTunnel(ctx, "127.0.0.1:1080", "8.8.8.8:443", dial); done <- err }()
			<-entered
			if !deadline {
				cancel()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled handshake succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("handshake ignored cancellation")
			}
		})
	}
}

func TestPublicNetworkSystemProxyKeepsPinnedHTTPBody(t *testing.T) {
	var calls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "CONNECT" || r.Host != "8.8.8.8:80" {
			t.Errorf("unvalidated proxy request: %s %s", r.Method, r.Host)
			w.WriteHeader(400)
			return
		}
		c, b, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		fmt.Fprint(b, "HTTP/1.1 200 Connection Established\r\n\r\n")
		b.Flush()
		origin, err := http.ReadRequest(b.Reader)
		if err != nil {
			t.Error(err)
			return
		}
		body, _ := io.ReadAll(origin.Body)
		origin.Body.Close()
		if origin.URL.IsAbs() || origin.Method != "POST" || origin.Host != "public.example" || string(body) != "query body" || origin.Header.Get("Proxy-Authorization") != "" {
			t.Errorf("origin request changed/leaked: %+v %q", origin, body)
		}
		fmt.Fprint(c, "HTTP/1.1 200 OK\r\nContent-Length: 8\r\nConnection: close\r\n\r\ndocument")
	}))
	defer proxy.Close()
	endpoint := strings.TrimPrefix(proxy.URL, "http://")
	opts := publicTestOptions()
	opts.routeSnapshot = &documentRouteSnapshot{httpProxy: endpoint, httpsProxy: "127.0.0.1:1"}
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	g := publicTestGuard(t, opts)
	resp, err := publicClient(g).Post("http://public.example/", "text/plain", strings.NewReader("query body"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(body) != "document" || calls.Load() != 1 || g.Stats().Bytes < int64(len("query bodydocument")) {
		t.Fatalf("body %q stats %+v calls %d err %v", body, g.Stats(), calls.Load(), err)
	}
}

func TestPublicNetworkSystemProxyRejectsMixedDNSBeforeTransport(t *testing.T) {
	opts := publicTestOptions()
	opts.routeSnapshot = &documentRouteSnapshot{httpProxy: "127.0.0.1:1080"}
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
	}
	var calls atomic.Int64
	opts.dialContext = func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("must not connect")
	}
	g := publicTestGuard(t, opts)
	resp, err := publicClient(g).Get("http://mixed.example/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 || calls.Load() != 0 {
		t.Fatalf("mixed DNS contacted trusted proxy: status %d calls %d", resp.StatusCode, calls.Load())
	}
}

func TestPublicNetworkSystemProxySelectsSchemeNotPort(t *testing.T) {
	var httpCalls, httpsCalls atomic.Int64
	makeProxy := func(calls *atomic.Int64) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Method != "CONNECT" {
				t.Error("proxy received non-CONNECT")
			}
			c, b, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			fmt.Fprint(b, "HTTP/1.1 200 OK\r\n\r\nx")
			b.Flush()
		}))
	}
	hp, sp := makeProxy(&httpCalls), makeProxy(&httpsCalls)
	defer hp.Close()
	defer sp.Close()
	opts := publicTestOptions()
	opts.routeSnapshot = &documentRouteSnapshot{httpProxy: strings.TrimPrefix(hp.URL, "http://"), httpsProxy: strings.TrimPrefix(sp.URL, "http://")}
	g := publicTestGuard(t, opts)
	for _, request := range []struct{ scheme, target string }{{"http", "8.8.8.8:443"}, {"https", "8.8.8.8:80"}} {
		c, err := g.dialScheme(context.Background(), "tcp", request.target, request.scheme)
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 1)
		_, err = io.ReadFull(c, b)
		c.Close()
		if err != nil || string(b) != "x" {
			t.Fatalf("tunnel %q %v", b, err)
		}
	}
	if httpCalls.Load() != 1 || httpsCalls.Load() != 1 {
		t.Fatalf("proxy route guessed port: http=%d https=%d", httpCalls.Load(), httpsCalls.Load())
	}
}

func TestPublicNetworkSystemProxyRetriesOnlyPinnedSnapshot(t *testing.T) {
	var lookups atomic.Int64
	seen := make(chan string, 2)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Host
		if r.Host == "8.8.8.8:443" {
			w.WriteHeader(502)
			return
		}
		if r.Host != "1.1.1.1:443" {
			t.Errorf("unexpected numeric retry %s", r.Host)
			w.WriteHeader(400)
			return
		}
		c, b, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		fmt.Fprint(b, "HTTP/1.1 200 OK\r\n\r\nx")
		b.Flush()
	}))
	defer proxy.Close()
	opts := publicTestOptions()
	opts.routeSnapshot = &documentRouteSnapshot{httpsProxy: strings.TrimPrefix(proxy.URL, "http://")}
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		lookups.Add(1)
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1")}, nil
	}
	g := publicTestGuard(t, opts)
	c, err := g.dialScheme(context.Background(), "tcp", "public.example:443", "https")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if lookups.Load() != 1 || <-seen != "8.8.8.8:443" || <-seen != "1.1.1.1:443" {
		t.Fatal("retry escaped validated snapshot")
	}
}

func TestPublicNetworkSystemProxyFailureNeverDialsTargetDirect(t *testing.T) {
	opts := publicTestOptions()
	opts.routeSnapshot = &documentRouteSnapshot{httpsProxy: "127.0.0.1:1080"}
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("1.1.1.1")}, nil
	}
	var calls atomic.Int64
	opts.dialContext = func(_ context.Context, _ string, address string) (net.Conn, error) {
		calls.Add(1)
		if address != "127.0.0.1:1080" {
			t.Errorf("direct fallback attempted: %s", address)
		}
		return nil, errors.New("proxy unavailable")
	}
	g := publicTestGuard(t, opts)
	if c, err := g.dialScheme(context.Background(), "tcp", "public.example:443", "https"); err == nil {
		c.Close()
		t.Fatal("proxy failure silently succeeded")
	}
	if calls.Load() != 2 {
		t.Fatalf("expected pinned retries through same transport: %d", calls.Load())
	}
}

func TestDocumentNetworkRouteFailureReceiptBeforeBrowser(t *testing.T) {
	publicTestEnvironment(t)
	t.Setenv("ALL_PROXY", "socks5://user:secret@127.0.0.1:1080")
	doc, err := FetchDocument(context.Background(), DocumentRequest{URL: "https://example.com/"})
	var failure *DocumentAcquisitionError
	if doc != nil || !errors.As(err, &failure) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("route failure lost safe receipt: %v", err)
	}
	p := failure.Diagnostic.Policy
	if p.RequestedNetworkRoute != DocumentNetworkSystem || p.NetworkRouteSource != "environment" || p.EffectiveTransport != "not-started" || !p.NumericTargetPinning || p.TrustedProxyDelegation {
		t.Fatalf("unsettled route receipt: %+v", p)
	}
}
