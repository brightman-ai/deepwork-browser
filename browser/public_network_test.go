package browser

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func publicTestEnvironment(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
}
func publicTestOptions() PublicNetworkOptions {
	return PublicNetworkOptions{NetworkRoute: DocumentNetworkDirect, Timeout: 3 * time.Second, MaxBytes: 1 << 20, MaxRequests: 10}
}
func publicTestGuard(t *testing.T, opts PublicNetworkOptions) *PublicNetworkGuard {
	t.Helper()
	publicTestEnvironment(t)
	g, e := StartPublicNetworkGuard(context.Background(), opts)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { g.Close() })
	return g
}
func publicClient(g *PublicNetworkGuard) *http.Client {
	u, _ := url.Parse(g.ProxyURL())
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}, Timeout: 2 * time.Second}
}
func TestPublicDocumentIPPolicy(t *testing.T) {
	for _, ip := range []string{"0.0.0.0", "10.1.2.3", "100.64.0.1", "127.0.0.1", "169.254.169.254", "172.16.0.1", "192.168.1.1", "192.0.2.1", "198.18.1.1", "198.51.100.2", "203.0.113.1", "224.0.0.1", "255.255.255.255", "::", "::1", "::ffff:127.0.0.1", "::ffff:192.168.0.1", "fe80::1", "fc00::1", "ff02::1", "64:ff9b::a00:1", "2001:db8::1", "2002:a00:1::1", "3ffe::1", "3fff::1", "4000::1", "3000::1", "2d00::1", "2000::1", "2001:1000::1", "2001:6000::1", "2420::1", "2622::1"} {
		if IsPublicDocumentIP(netip.MustParseAddr(ip)) {
			t.Errorf("accepted %s", ip)
		}
	}
	for _, ip := range []string{"1.1.1.1", "8.8.8.8", "::ffff:8.8.8.8", "2606:4700:4700::1111", "2001:4860:4860::8888", "2410::1", "2a10::1", "2630::1"} {
		if !IsPublicDocumentIP(netip.MustParseAddr(ip)) {
			t.Errorf("rejected %s", ip)
		}
	}
}
func TestValidatePublicDocumentURL(t *testing.T) {
	for _, raw := range []string{"file:///etc/passwd", "data:text/plain,x", "ftp://example.com", "https://u:p@example.com", "http://127.0.0.1", "http://[::ffff:127.0.0.1]", "http://[fe80::1%25en0]", "http://example.com:22", "http://example.com:0", "http:///x"} {
		if _, e := ValidatePublicDocumentURL(raw, nil); e == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if _, e := ValidatePublicDocumentURL("https://example.com/path", nil); e != nil {
		t.Fatal(e)
	}
	if _, e := ValidatePublicDocumentURL("https://example.com:8443", []uint16{8443}); e != nil {
		t.Fatal(e)
	}
}
func TestPublicNetworkPinsValidatedAddress(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "public.test" {
			t.Errorf("host = %s", r.Host)
		}
		fmt.Fprint(w, "document")
	}))
	defer origin.Close()
	originAddr := strings.TrimPrefix(origin.URL, "http://")
	var lookups, dials atomic.Int64
	opts := publicTestOptions()
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		lookups.Add(1)
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	opts.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials.Add(1)
		if address != "8.8.8.8:80" {
			t.Errorf("dial was not pinned: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, originAddr)
	}
	g := publicTestGuard(t, opts)
	resp, e := publicClient(g).Get("http://public.test/")
	if e != nil {
		t.Fatal(e)
	}
	body, e := io.ReadAll(resp.Body)
	resp.Body.Close()
	if e != nil || string(body) != "document" {
		t.Fatalf("body %q err %v", body, e)
	}
	if lookups.Load() != 1 || dials.Load() != 1 {
		t.Fatalf("lookup/dial = %d/%d", lookups.Load(), dials.Load())
	}
	if g.Err() != nil {
		t.Fatal(g.Err())
	}
}
func TestPublicNetworkRejectsMixedDNSBeforeDial(t *testing.T) {
	var dials atomic.Int64
	opts := publicTestOptions()
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("::ffff:127.0.0.1")}, nil
	}
	opts.dialContext = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, fmt.Errorf("must not dial")
	}
	g := publicTestGuard(t, opts)
	resp, e := publicClient(g).Get("http://mixed.test/")
	if e != nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("mixed DNS response %v %v", resp, e)
	}
	resp.Body.Close()
	if dials.Load() != 0 {
		t.Fatal("dialed mixed public/private DNS")
	}
	assertPublicDenied(t, g, 1)
}
func TestPublicNetworkRedirectRejectsPrivateHop(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://127.0.0.1/private", 302) }))
	defer origin.Close()
	var dials atomic.Int64
	opts := publicTestOptions()
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, n, strings.TrimPrefix(origin.URL, "http://"))
	}
	g := publicTestGuard(t, opts)
	resp, _ := publicClient(g).Get("http://public.test/")
	if resp != nil {
		resp.Body.Close()
	}
	if dials.Load() != 1 {
		t.Fatalf("redirect reached dial: %d", dials.Load())
	}
	assertPublicDenied(t, g, 1)
}
func TestPublicNetworkConnectPinsAndCloses(t *testing.T) {
	echo, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer echo.Close()
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		c, e := echo.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	opts := publicTestOptions()
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("2606:4700:4700::1111")}, nil
	}
	opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
		if a != "[2606:4700:4700::1111]:443" {
			t.Errorf("CONNECT unpinned %s", a)
		}
		return (&net.Dialer{}).DialContext(ctx, n, echo.Addr().String())
	}
	g := publicTestGuard(t, opts)
	u, _ := url.Parse(g.ProxyURL())
	c, e := net.Dial("tcp", u.Host)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	fmt.Fprint(c, "CONNECT public.test:443 HTTP/1.1\r\nHost: public.test:443\r\n\r\n")
	reader := bufio.NewReader(c)
	resp, e := http.ReadResponse(reader, &http.Request{Method: "CONNECT"})
	if e != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT response %v %v", resp, e)
	}
	fmt.Fprint(c, "hello")
	p := make([]byte, 5)
	if _, e = io.ReadFull(reader, p); e != nil || string(p) != "hello" {
		t.Fatalf("echo %q %v", p, e)
	}
	g.Close()
	select {
	case <-echoDone:
	case <-time.After(time.Second):
		t.Fatal("CONNECT upstream leaked after Close")
	}
}
func TestPublicNetworkBudgets(t *testing.T) {
	t.Run("requests", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
		defer origin.Close()
		opts := publicTestOptions()
		opts.MaxRequests = 1
		opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, strings.TrimPrefix(origin.URL, "http://"))
		}
		g := publicTestGuard(t, opts)
		client := publicClient(g)
		first, e := client.Get("http://public.test/")
		if e != nil {
			t.Fatal(e)
		}
		_, _ = io.Copy(io.Discard, first.Body)
		first.Body.Close()
		second, _ := client.Get("http://public.test/second")
		if second != nil {
			second.Body.Close()
		}

		if e := g.Err(); e == nil || !strings.Contains(e.Error(), "request budget") {
			t.Fatalf("err %v", e)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "too much response") }))
		defer origin.Close()
		opts := publicTestOptions()
		opts.MaxBytes = 4
		opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, strings.TrimPrefix(origin.URL, "http://"))
		}
		g := publicTestGuard(t, opts)
		resp, _ := publicClient(g).Get("http://public.test")
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if len(b) > 4 {
				t.Fatalf("budget exceeded on wire: %d", len(b))
			}
		}
		if e := g.Err(); e == nil || !strings.Contains(e.Error(), "byte budget") {
			t.Fatalf("err %v", e)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		opts := publicTestOptions()
		opts.Timeout = 20 * time.Millisecond
		g := publicTestGuard(t, opts)
		select {
		case <-g.serveDone:
		case <-time.After(time.Second):
			t.Fatal("deadline did not close proxy")
		}
		if g.Err() != context.DeadlineExceeded {
			t.Fatalf("err %v", g.Err())
		}
	})
}
func TestPublicNetworkRejectsUnsupportedEnvironmentProxy(t *testing.T) {
	publicTestEnvironment(t)
	t.Setenv("HTTPS_PROXY", "socks5://proxy.test:3128")
	opts := publicTestOptions()
	opts.NetworkRoute = DocumentNetworkSystem
	if g, e := StartPublicNetworkGuard(context.Background(), opts); e == nil {
		g.Close()
		t.Fatal("silently bypassed upstream proxy")
	}
}

func TestPublicNetworkConnectRejectsDestinations(t *testing.T) {
	for _, dest := range []string{"127.0.0.1:443", "[::ffff:127.0.0.1]:443", "8.8.8.8:22", "mixed.test:443"} {
		t.Run(dest, func(t *testing.T) {
			var dials atomic.Int64
			opts := publicTestOptions()
			opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("169.254.169.254")}, nil
			}
			opts.dialContext = func(context.Context, string, string) (net.Conn, error) {
				dials.Add(1)
				return nil, fmt.Errorf("must not dial")
			}
			g := publicTestGuard(t, opts)
			u, _ := url.Parse(g.ProxyURL())
			c, e := net.Dial("tcp", u.Host)
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", dest, dest)
			resp, e := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: "CONNECT"})
			if e != nil || resp.StatusCode != http.StatusForbidden {
				t.Fatalf("denied CONNECT response %v %v", resp, e)
			}
			resp.Body.Close()
			if dials.Load() != 0 {
				t.Fatal("rejected CONNECT reached dial")
			}
			assertPublicDenied(t, g, 1)
		})
	}
}
func TestPublicNetworkRevalidatesDNSOnNextConnection(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer origin.Close()
	var lookups, dials atomic.Int64
	opts := publicTestOptions()
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		if lookups.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, n, strings.TrimPrefix(origin.URL, "http://"))
	}
	g := publicTestGuard(t, opts)
	client := publicClient(g)
	resp, e := client.Get("http://rebinding.test/")
	if e != nil {
		t.Fatal(e)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	resp, _ = client.Get("http://rebinding.test/next")
	if resp != nil {
		resp.Body.Close()
	}
	if lookups.Load() != 2 || dials.Load() != 1 {
		t.Fatalf("lookup/dial %d/%d", lookups.Load(), dials.Load())
	}
	assertPublicDenied(t, g, 1)
}

func TestPublicNetworkPolicyErrorAfterClose(t *testing.T) {
	t.Run("clean close", func(t *testing.T) {
		g := publicTestGuard(t, publicTestOptions())
		g.Close()
		if e := g.PolicyError(); e != nil {
			t.Fatalf("normal Close became policy failure: %v", e)
		}
	})
	t.Run("retained deny receipt", func(t *testing.T) {
		g := publicTestGuard(t, publicTestOptions())
		resp, _ := publicClient(g).Get("http://127.0.0.1/")
		if resp != nil {
			resp.Body.Close()
		}
		g.Close()
		assertPublicDenied(t, g, 1)
	})
}

func TestPublicNetworkCloseInflightHTTPIsClean(t *testing.T) {
	started := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "partial")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer origin.Close()
	opts := publicTestOptions()
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, strings.TrimPrefix(origin.URL, "http://"))
	}
	g := publicTestGuard(t, opts)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r, _ := publicClient(g).Get("http://public.test/")
		if r != nil {
			_, _ = io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream did not start")
	}
	g.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("HTTP client leaked after Close")
	}
	if e := g.PolicyError(); e != nil {
		t.Fatalf("normal inflight Close became failure: %v", e)
	}
}

func TestPublicNetworkSnapshotsPortPolicy(t *testing.T) {
	var dials atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "unexpected") }))
	defer origin.Close()
	ports := []uint16{443}
	opts := publicTestOptions()
	opts.AllowedPorts = ports
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, n, strings.TrimPrefix(origin.URL, "http://"))
	}
	g := publicTestGuard(t, opts)
	ports[0] = 8443
	resp, _ := publicClient(g).Get("http://public.test:8443/")
	if resp != nil {
		resp.Body.Close()
	}
	g.Close()
	if dials.Load() != 0 {
		t.Fatal("caller mutated the running guard's allowed-port policy")
	}
	assertPublicDenied(t, g, 1)
}

func TestPublicNetworkConnectByteBudgetIncludesBothDirections(t *testing.T) {
	echo, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer echo.Close()
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		c, e := echo.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	opts := publicTestOptions()
	opts.MaxBytes = 8
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, echo.Addr().String())
	}
	g := publicTestGuard(t, opts)
	u, _ := url.Parse(g.ProxyURL())
	c, e := net.Dial("tcp", u.Host)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprint(c, "CONNECT public.test:443 HTTP/1.1\r\nHost: public.test:443\r\n\r\n")
	r := bufio.NewReader(c)
	resp, e := http.ReadResponse(r, &http.Request{Method: "CONNECT"})
	if e != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT %v %v", resp, e)
	}
	fmt.Fprint(c, "hello")
	body, _ := io.ReadAll(r)
	g.Close()
	if len(body) != 0 {
		t.Fatalf("forwarded echo beyond combined budget: %q", body)
	}
	if e := g.PolicyError(); e == nil || !strings.Contains(e.Error(), "byte budget") {
		t.Fatalf("combined byte budget error = %v", e)
	}
	if g.Stats().Bytes != 10 {
		t.Fatalf("bidirectional attempted byte count = %d, want 10", g.Stats().Bytes)
	}
	select {
	case <-echoDone:
	case <-time.After(time.Second):
		t.Fatal("budget cancellation leaked CONNECT upstream")
	}
}

func TestPublicNetworkCloseCancelsResolver(t *testing.T) {
	started := make(chan struct{})
	opts := publicTestOptions()
	opts.lookupIP = func(ctx context.Context, _ string) ([]netip.Addr, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	opts.dialContext = func(context.Context, string, string) (net.Conn, error) {
		t.Error("dial after canceled resolution")
		return nil, fmt.Errorf("must not dial")
	}
	g := publicTestGuard(t, opts)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r, _ := publicClient(g).Get("http://waiting.test/")
		if r != nil {
			r.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("resolver did not start")
	}
	g.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled resolver/client leaked")
	}
	if e := g.PolicyError(); e != nil {
		t.Fatalf("Close during DNS became policy failure: %v", e)
	}
}

func TestPublicNetworkFallsBackWithinValidatedDNSSet(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "fallback") }))
	defer origin.Close()
	opts := publicTestOptions()
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("2606:4700:4700::1111"), netip.MustParseAddr("8.8.8.8")}, nil
	}
	var attempts []string
	opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
		attempts = append(attempts, a)
		if len(attempts) == 1 {
			return nil, fmt.Errorf("IPv6 route unavailable")
		}
		return (&net.Dialer{}).DialContext(ctx, n, strings.TrimPrefix(origin.URL, "http://"))
	}
	g := publicTestGuard(t, opts)
	r, e := publicClient(g).Get("http://fallback.test/")
	if e != nil {
		t.Fatal(e)
	}
	body, e := io.ReadAll(r.Body)
	r.Body.Close()
	g.Close()
	if e != nil || string(body) != "fallback" {
		t.Fatalf("fallback response %q %v", body, e)
	}
	if len(attempts) != 2 || attempts[0] != "[2606:4700:4700::1111]:80" || attempts[1] != "8.8.8.8:80" {
		t.Fatalf("unvalidated/unpinned attempts: %v", attempts)
	}
	if e := g.PolicyError(); e != nil {
		t.Fatalf("successful public fallback marked failed: %v", e)
	}
}

func TestPublicNetworkStalledCandidateLeavesDeadlineForFallback(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "fallback") }))
	defer origin.Close()
	opts := publicTestOptions()
	opts.Timeout = time.Second
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("2606:4700:4700::1111"), netip.MustParseAddr("8.8.8.8")}, nil
	}
	var attempts atomic.Int64
	opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
		if attempts.Add(1) == 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if a != "8.8.8.8:80" {
			t.Errorf("fallback is not pinned: %s", a)
		}
		return (&net.Dialer{}).DialContext(ctx, n, strings.TrimPrefix(origin.URL, "http://"))
	}
	g := publicTestGuard(t, opts)
	r, e := publicClient(g).Get("http://fallback.test/")
	if e != nil {
		t.Fatal(e)
	}
	body, e := io.ReadAll(r.Body)
	r.Body.Close()
	g.Close()
	if e != nil || string(body) != "fallback" || attempts.Load() != 2 {
		t.Fatalf("stalled first candidate consumed all attempts: body %q attempts %d err %v", body, attempts.Load(), e)
	}
	if e := g.PolicyError(); e != nil {
		t.Fatalf("successful fallback became failure: %v", e)
	}
}

// Model a finite slow outbound socket cleanup, without mocking the proxy or its
// handler. The test always releases the socket, so its shutdown worker can join.
type publicDelayedCloseConn struct {
	net.Conn
	release <-chan struct{}
	started chan struct{}
	once    sync.Once
}

func (c *publicDelayedCloseConn) Close() error {
	c.once.Do(func() { close(c.started) })
	<-c.release
	return c.Conn.Close()
}

func TestPublicNetworkCloseContextBoundsWaitAndContinuesCleanup(t *testing.T) {
	echo, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer echo.Close()
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		c, e := echo.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	closing := make(chan struct{})
	opts := publicTestOptions()
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
		c, e := (&net.Dialer{}).DialContext(ctx, n, echo.Addr().String())
		if e != nil {
			return nil, e
		}
		return &publicDelayedCloseConn{Conn: c, release: release, started: closing}, nil
	}
	g := publicTestGuard(t, opts)
	u, _ := url.Parse(g.ProxyURL())
	c, e := net.Dial("tcp", u.Host)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprint(c, "CONNECT public.test:443 HTTP/1.1\r\nHost: public.test:443\r\n\r\n")
	r := bufio.NewReader(c)
	resp, e := http.ReadResponse(r, &http.Request{Method: "CONNECT"})
	if e != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT %v %v", resp, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if e := g.CloseContext(ctx); e != context.DeadlineExceeded {
		t.Fatalf("slow cleanup should respect caller deadline, got %v", e)
	}
	select {
	case <-closing:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not begin socket cleanup")
	}
	select {
	case <-g.closeDone:
		t.Fatal("shutdown reported settled before socket cleanup")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	settled, settledCancel := context.WithTimeout(context.Background(), time.Second)
	defer settledCancel()
	if e := g.CloseContext(settled); e != nil {
		t.Fatalf("later caller could not join cleanup: %v", e)
	}
	if e := g.Close(); e != nil {
		t.Fatalf("repeated Close: %v", e)
	}
	if e := g.PolicyError(); e != nil {
		t.Fatalf("normal cleanup became policy error: %v", e)
	}
	select {
	case <-echoDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup left upstream running")
	}
}

func TestPublicNetworkCloseContextNormalAndIdempotent(t *testing.T) {
	g := publicTestGuard(t, publicTestOptions())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e := g.CloseContext(ctx); e != nil {
		t.Fatal(e)
	}
	if e := g.CloseContext(ctx); e != nil {
		t.Fatalf("repeated CloseContext: %v", e)
	}
	if e := g.Close(); e != nil {
		t.Fatalf("Close after CloseContext: %v", e)
	}
	select {
	case <-g.closeDone:
	default:
		t.Fatal("successful CloseContext returned before join")
	}
}

func assertPublicDenied(t *testing.T, g *PublicNetworkGuard, want int64) {
	t.Helper()
	if got := g.Stats().DeniedRequests; got != want {
		t.Fatalf("denied request count %d, want %d", got, want)
	}
	if e := g.PolicyError(); e != nil {
		t.Fatalf("successfully blocked request killed the job: %v", e)
	}
}

func TestPublicNetworkDeniedResourceDoesNotKillHealthyDocument(t *testing.T) {
	for _, mode := range []string{"private DNS", "ordinary upstream error", "DNS failure"} {
		t.Run(mode, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "healthy document") }))
			defer origin.Close()
			var dials atomic.Int64
			opts := publicTestOptions()
			opts.lookupIP = func(_ context.Context, host string) ([]netip.Addr, error) {
				if host == "blocked.test" && mode == "DNS failure" {
					return nil, fmt.Errorf("upstream DNS unavailable")
				}
				if host == "blocked.test" && mode == "private DNS" {
					return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("2001::1")}, nil
				}
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			}
			opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
				if dials.Add(1) == 1 && mode == "ordinary upstream error" {
					return nil, fmt.Errorf("upstream connection refused")
				}
				return (&net.Dialer{}).DialContext(ctx, n, strings.TrimPrefix(origin.URL, "http://"))
			}
			g := publicTestGuard(t, opts)
			client := publicClient(g)
			blocked, e := client.Get("http://blocked.test/")
			if e != nil || blocked.StatusCode != http.StatusBadGateway {
				t.Fatalf("rejected request response %v %v", blocked, e)
			}
			blocked.Body.Close()
			if mode == "private DNS" && dials.Load() != 0 {
				t.Fatal("private DNS answer reached dial")
			}
			healthy, e := client.Get("http://healthy.test/")
			if e != nil {
				t.Fatal(e)
			}
			body, e := io.ReadAll(healthy.Body)
			healthy.Body.Close()
			if e != nil || healthy.StatusCode != 200 || string(body) != "healthy document" {
				t.Fatalf("healthy document failed after blocked resource: %q %v", body, e)
			}
			if e := g.Err(); e != nil {
				t.Fatalf("per-request failure canceled guard: %v", e)
			}
			g.Close()
			wantDenied := int64(0)
			wantDials := int64(2)
			if mode == "DNS failure" {
				wantDials = 1
			}
			if mode == "private DNS" {
				wantDenied = 1
				wantDials = 1
			}
			assertPublicDenied(t, g, wantDenied)
			if dials.Load() != wantDials {
				t.Fatalf("dials %d, want %d", dials.Load(), wantDials)
			}
		})
	}
}

func TestPublicNetworkUnexpectedProxyServerFailureIsFatal(t *testing.T) {
	g := publicTestGuard(t, publicTestOptions())
	_ = g.listener.Close()
	select {
	case <-g.serveDone:
	case <-time.After(time.Second):
		t.Fatal("proxy server did not report listener failure")
	}
	if e := g.PolicyError(); e == nil || !strings.Contains(e.Error(), "proxy server") {
		t.Fatalf("unexpected server failure was not fatal: %v", e)
	}
	g.Close()
	if g.Stats().DeniedRequests != 0 {
		t.Fatal("control-plane failure counted as policy-denied request")
	}
}

func TestPublicNetworkLocalErrorMarkerCannotBeForgedByOrigin(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(PublicNetworkErrorHeader, "1")
		w.Header().Set(PublicNetworkErrorKindHeader, "policy")
		fmt.Fprint(w, "real origin")
	}))
	defer origin.Close()
	var dials atomic.Int64
	opts := publicTestOptions()
	opts.lookupIP = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host == "private.test" {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, n, strings.TrimPrefix(origin.URL, "http://"))
	}
	g := publicTestGuard(t, opts)
	client := publicClient(g)
	rejected, e := client.Get("http://private.test/")
	if e != nil {
		t.Fatal(e)
	}
	_, _ = io.Copy(io.Discard, rejected.Body)
	rejected.Body.Close()
	if rejected.StatusCode != 502 || rejected.Header.Get(PublicNetworkErrorHeader) != "1" {
		t.Fatalf("local rejection lacks marker: status %d headers %v", rejected.StatusCode, rejected.Header)
	}
	if dials.Load() != 0 {
		t.Fatal("private DNS destination reached dial")
	}
	normal, e := client.Get("http://public.test/")
	if e != nil {
		t.Fatal(e)
	}
	body, e := io.ReadAll(normal.Body)
	normal.Body.Close()
	if e != nil || normal.StatusCode != 200 || string(body) != "real origin" {
		t.Fatalf("origin response changed: %d %q %v", normal.StatusCode, body, e)
	}
	if marker := normal.Header.Get(PublicNetworkErrorHeader); marker != "" {
		t.Fatalf("origin forged local proxy error marker: %q", marker)
	}
	if kind := normal.Header.Get(PublicNetworkErrorKindHeader); kind != "" {
		t.Fatalf("origin forged local proxy error provenance: %q", kind)
	}
	if dials.Load() != 1 {
		t.Fatalf("origin dial count = %d", dials.Load())
	}
	assertPublicDenied(t, g, 1)
}

func TestPublicNetworkAllowsPostQueryAndChargesBothHTTPBodies(t *testing.T) {
	query := `{"query":"{ article { title } }"}`
	answer := `{"data":{"article":{"title":"Article"}}}`
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, e := io.ReadAll(r.Body)
		if e != nil || r.Method != "POST" || string(body) != query {
			t.Errorf("POST query changed: method %s body %q err %v", r.Method, body, e)
		}
		fmt.Fprint(w, answer)
	}))
	defer origin.Close()
	opts := publicTestOptions()
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
		if a != "8.8.8.8:80" {
			t.Errorf("POST destination not pinned: %s", a)
		}
		return (&net.Dialer{}).DialContext(ctx, n, strings.TrimPrefix(origin.URL, "http://"))
	}
	g := publicTestGuard(t, opts)
	response, e := publicClient(g).Post("http://public.test/graphql", "application/json", strings.NewReader(query))
	if e != nil {
		t.Fatal(e)
	}
	body, e := io.ReadAll(response.Body)
	response.Body.Close()
	g.Close()
	if e != nil || response.StatusCode != 200 || string(body) != answer {
		t.Fatalf("POST response %d %q %v", response.StatusCode, body, e)
	}
	if got, want := g.Stats().Bytes, int64(len(query)+len(answer)); got != want {
		t.Fatalf("combined HTTP body count %d, want %d", got, want)
	}
	assertPublicDenied(t, g, 0)
}

func TestPublicNetworkRequestBodyBudgetRejectsBeforeForwardingChunk(t *testing.T) {
	var received atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		received.Add(n)
		fmt.Fprint(w, "response")
	}))
	defer origin.Close()
	opts := publicTestOptions()
	opts.MaxBytes = 4
	opts.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	opts.dialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, strings.TrimPrefix(origin.URL, "http://"))
	}
	g := publicTestGuard(t, opts)
	response, _ := publicClient(g).Post("http://public.test/graphql", "application/json", strings.NewReader("12345678"))
	if response != nil {
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	g.Close()
	origin.Close()
	if n := received.Load(); n > opts.MaxBytes {
		t.Fatalf("forwarded %d request-body bytes beyond %d budget", n, opts.MaxBytes)
	}
	if e := g.PolicyError(); e == nil || !strings.Contains(e.Error(), "byte budget") {
		t.Fatalf("upload budget must be globally fatal: %v", e)
	}
	if g.Stats().DeniedRequests != 0 {
		t.Fatal("global budget counted as per-destination denial")
	}
}
