package browser

// This proxy controls TCP destinations of requests sent through it. It is not an
// OS network sandbox: its caller must also disable browser proxy bypasses, QUIC,
// WebRTC UDP and other direct transports. HTTPS bytes are opaque; a CONNECT is
// one request for accounting purposes, and its encrypted bytes count in both directions.
import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PublicNetworkErrorHeader identifies responses synthesized by this proxy. It
// must be stripped from origin responses so a site cannot forge that signal.
const PublicNetworkErrorHeader = "X-DW-Proxy-Error"

func publicProxyError(w http.ResponseWriter, message string, status int) {
	w.Header().Set(PublicNetworkErrorHeader, "1")
	http.Error(w, message, status)
}

type PublicNetworkOptions struct {
	Timeout     time.Duration
	MaxBytes    int64
	MaxRequests int64
	// AllowedPorts overrides the default TCP ports 80 and 443.
	AllowedPorts []uint16
	// Private resolver/dial seams are for deterministic unit tests.
	// Production callers must leave these nil.
	lookupIP    func(context.Context, string) ([]netip.Addr, error)
	dialContext func(context.Context, string, string) (net.Conn, error)
}

// PublicNetworkStats counts attempted proxy requests/tunnels and HTTP body /
// bidirectional CONNECT payload chunks offered to the budget boundary. Bytes is
// not wire traffic: it excludes HTTP headers and may exceed MaxBytes by rejected
// chunks or overcount partial/failed writes. HTTP counts the transport response
// body in both directions (response possibly decompressed); CONNECT stays encrypted.
type PublicNetworkStats struct {
	Requests int64
	// DeniedRequests counts policy-rejected proxy requests/tunnels, excluding
	// ordinary upstream DNS/connect/disconnect errors.
	DeniedRequests int64
	Bytes          int64
}
type PublicNetworkGuard struct {
	handlers       sync.WaitGroup
	closing        bool
	ctx            context.Context
	cancel         context.CancelFunc
	opts           PublicNetworkOptions
	listener       net.Listener
	server         *http.Server
	transport      *http.Transport
	requests       atomic.Int64
	deniedRequests atomic.Int64
	bytes          atomic.Int64
	mu             sync.Mutex
	err            error
	conns          map[*publicTrackedConn]struct{}
	connections    sync.WaitGroup
	serveDone      chan struct{}
	closeOnce      sync.Once
	closeDone      chan struct{}
}

// Policy sources: IANA IPv4/IPv6 Special-Purpose Address Registries, RFC 6890,
// and the current IANA IPv6 global-unicast allocation (2000::/3). Exceptions
// inside special-use blocks are intentionally denied conservatively.
// Non-public special-use ranges are denied conservatively, including reserved,
// documentation, benchmark, transition and NAT64 ranges. Unmap before checking.
var publicDeniedPrefixes = func() []netip.Prefix {
	ranges := []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "3ffe::/16", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "ff00::/8"}
	out := make([]netip.Prefix, 0, len(ranges))
	for _, r := range ranges {
		out = append(out, netip.MustParsePrefix(r))
	}
	return out
}()

// Snapshot of ALLOCATED global-unicast prefixes from IANA's IPv6 Unicast
// Address Assignments registry, retrieved 2026-10-16:
// https://www.iana.org/assignments/ipv6-unicast-address-assignments/ipv6-unicast-address-assignments.csv
// Unassigned/reserved space (including within 2000::/3) stays fail-closed. New
// allocations require an intentional policy update. Special-use checks below
// still take precedence, including 2001:db8 inside an allocated APNIC block.
var publicIPv6AllocatedPrefixes = func() []netip.Prefix {
	ranges := []string{
		"2001:200::/23", "2001:400::/23", "2001:600::/23", "2001:800::/22", "2001:c00::/23", "2001:e00::/23",
		"2001:1200::/23", "2001:1400::/22", "2001:1800::/23", "2001:1a00::/23", "2001:1c00::/22", "2001:2000::/19",
		"2001:4000::/23", "2001:4200::/23", "2001:4400::/23", "2001:4600::/23", "2001:4800::/23", "2001:4a00::/23", "2001:4c00::/23", "2001:5000::/20",
		"2001:8000::/19", "2001:a000::/20", "2001:b000::/20", "2003::/18", "2400::/12", "2410::/12", "2600::/12", "2610::/23", "2620::/23", "2630::/12", "2800::/12", "2a00::/12", "2a10::/12", "2c00::/12",
	}
	out := make([]netip.Prefix, 0, len(ranges))
	for _, r := range ranges {
		out = append(out, netip.MustParsePrefix(r))
	}
	return out
}()

func IsPublicDocumentIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() {
		return false
	}
	if ip.Is6() {
		allocated := false
		for _, p := range publicIPv6AllocatedPrefixes {
			if p.Contains(ip) {
				allocated = true
				break
			}
		}
		if !allocated {
			return false
		}
	}
	for _, p := range publicDeniedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
func publicPortAllowed(port uint16, allowed []uint16) bool {
	if len(allowed) == 0 {
		return port == 80 || port == 443
	}
	for _, p := range allowed {
		if p == port && p != 0 {
			return true
		}
	}
	return false
}

// ValidatePublicDocumentURL validates syntax/literal IP only. DNS is validated
// again when the proxy dials; this preflight never substitutes for that check.
func ValidatePublicDocumentURL(raw string, allowedPorts []uint16) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil || u.Hostname() == "" || strings.Contains(u.Hostname(), "%") {
		return nil, errors.New("document URL requires http(s), hostname and no credentials or zone")
	}
	port := uint64(80)
	if u.Scheme == "https" {
		port = 443
	}
	if u.Port() != "" {
		port, err = strconv.ParseUint(u.Port(), 10, 16)
		if err != nil {
			return nil, errors.New("invalid destination port")
		}
	}
	if !publicPortAllowed(uint16(port), allowedPorts) {
		return nil, errors.New("destination port is not allowed")
	}
	if ip, e := netip.ParseAddr(u.Hostname()); e == nil && !IsPublicDocumentIP(ip) {
		return nil, errors.New("destination IP is not public")
	}
	return u, nil
}
func StartPublicNetworkGuard(parent context.Context, opts PublicNetworkOptions) (*PublicNetworkGuard, error) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		if os.Getenv(key) != "" {
			return nil, fmt.Errorf("public document network guard does not support upstream proxy environment %s", key)
		}
	}
	if opts.Timeout <= 0 || opts.MaxBytes <= 0 || opts.MaxRequests <= 0 {
		return nil, errors.New("positive timeout, byte and request budgets are required")
	}
	// Freeze caller-owned policy slices for this job. A later caller mutation
	// must not silently change destination authorization or race active requests.
	opts.AllowedPorts = append([]uint16(nil), opts.AllowedPorts...)
	ctx, cancel := context.WithTimeout(parent, opts.Timeout)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		cancel()
		return nil, err
	}
	g := &PublicNetworkGuard{ctx: ctx, cancel: cancel, opts: opts, listener: ln, conns: make(map[*publicTrackedConn]struct{}), serveDone: make(chan struct{}), closeDone: make(chan struct{})}
	g.transport = &http.Transport{Proxy: nil, DialContext: g.dial, DisableKeepAlives: true, ForceAttemptHTTP2: false, ResponseHeaderTimeout: opts.Timeout, MaxResponseHeaderBytes: 64 << 10}
	g.server = &http.Server{Handler: g, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		defer close(g.serveDone)
		if err := g.server.Serve(&publicTrackedListener{Listener: ln, g: g}); err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
			g.fail(fmt.Errorf("public network proxy server: %w", err))
		}
	}()
	go func() { <-ctx.Done(); g.Close() }()
	return g, nil
}
func (g *PublicNetworkGuard) ProxyURL() string { return "http://" + g.listener.Addr().String() }
func (g *PublicNetworkGuard) Err() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return g.err
	}
	return g.ctx.Err()
}

// PolicyError returns the first global guard failure (such as a byte/request
// budget), excluding individually blocked requests and normal cancellation.
// Call after Close to obtain the settled handler result.
func (g *PublicNetworkGuard) PolicyError() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}
func (g *PublicNetworkGuard) Stats() PublicNetworkStats {
	return PublicNetworkStats{Requests: g.requests.Load(), DeniedRequests: g.deniedRequests.Load(), Bytes: g.bytes.Load()}
}
func (g *PublicNetworkGuard) fail(err error) {
	g.mu.Lock()
	// Closing active HTTP handlers may report cancellation/closed sockets.
	// These are cleanup results, rather than a failed destination or budget.
	if g.closing && (errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed)) {
		g.mu.Unlock()
		return
	}
	if g.err == nil {
		g.err = err
	}
	g.mu.Unlock()
	g.cancel()
}

// Close waits until shutdown has joined all tracked connections and handlers.
func (g *PublicNetworkGuard) Close() error { return g.CloseContext(context.Background()) }

// CloseContext starts shutdown once and bounds this caller's wait. A timeout
// does not abandon cancellation: the single shutdown worker continues cleanup,
// and a later Close/CloseContext can wait for its settled result.
func (g *PublicNetworkGuard) CloseContext(ctx context.Context) error {
	g.closeOnce.Do(func() {
		g.mu.Lock()
		g.closing = true
		g.mu.Unlock()
		g.cancel()
		go g.closeWorker()
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-g.closeDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (g *PublicNetworkGuard) closeWorker() {
	defer close(g.closeDone)
	_ = g.server.Close()
	_ = g.listener.Close()
	<-g.serveDone
	g.mu.Lock()
	cs := make([]*publicTrackedConn, 0, len(g.conns))
	for c := range g.conns {
		cs = append(cs, c)
	}
	g.mu.Unlock()
	for _, c := range cs {
		_ = c.Close()
	}
	g.transport.CloseIdleConnections()
	g.connections.Wait()
	g.handlers.Wait()
}

type publicDestinationError struct{ reason string }

func (e *publicDestinationError) Error() string   { return e.reason }
func publicDestinationDenied(reason string) error { return &publicDestinationError{reason: reason} }
func (g *PublicNetworkGuard) deny()               { g.deniedRequests.Add(1) }
func (g *PublicNetworkGuard) countDestinationDenial(err error) {
	var denied *publicDestinationError
	if errors.As(err, &denied) {
		g.deny()
	}
}

func (g *PublicNetworkGuard) dial(ctx context.Context, network, address string) (net.Conn, error) {
	// net/http may detach a dial's request deadline for connection management.
	// Bind actual resolution/dialing to both the caller and the job lifetime.
	deadline, _ := g.ctx.Deadline()
	ctx, cancel := context.WithDeadline(ctx, deadline)
	stop := context.AfterFunc(g.ctx, cancel)
	defer func() { stop(); cancel() }()

	host, portS, err := net.SplitHostPort(address)
	if err != nil {
		return nil, publicDestinationDenied("invalid destination authority")
	}
	port, err := strconv.ParseUint(portS, 10, 16)
	if err != nil || !publicPortAllowed(uint16(port), g.opts.AllowedPorts) {
		return nil, publicDestinationDenied("destination port is not allowed")
	}
	var ips []netip.Addr
	if ip, e := netip.ParseAddr(host); e == nil {
		ips = []netip.Addr{ip}
	} else if g.opts.lookupIP != nil {
		ips, err = g.opts.lookupIP(ctx, host)
	} else {
		ips, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("DNS returned no addresses")
	}
	for _, ip := range ips {
		if !IsPublicDocumentIP(ip) {
			return nil, publicDestinationDenied(fmt.Sprintf("non-public DNS destination for %s", host))
		}
	}
	dial := g.opts.dialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	// The whole answer set was validated before any attempt. Retry only numeric
	// addresses from that snapshot, never a hostname or a new DNS answer. Split
	// the remaining deadline among remaining candidates so an unreachable first
	// address cannot consume the entire job and prevent fallback.
	var dialErrors []error
	for i, ip := range ips {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attemptCtx, attemptCancel := context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(ips)-i))
		conn, err := dial(attemptCtx, "tcp", net.JoinHostPort(ip.Unmap().String(), portS))
		attemptCancel()
		if err == nil {
			return conn, nil
		}
		dialErrors = append(dialErrors, err)
	}
	return nil, errors.Join(dialErrors...)
}
func (g *PublicNetworkGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	if g.closing {
		g.mu.Unlock()
		publicProxyError(w, "proxy closed", 503)
		return
	}
	g.handlers.Add(1)
	g.mu.Unlock()
	defer g.handlers.Done()
	if g.requests.Add(1) > g.opts.MaxRequests {
		g.fail(errors.New("public network request budget exceeded"))
		publicProxyError(w, "request budget exceeded", 502)
		return
	}
	if r.Method == http.MethodConnect {
		g.connect(w, r)
		return
	}
	u, err := ValidatePublicDocumentURL(r.URL.String(), g.opts.AllowedPorts)
	if err != nil || u.Scheme != "http" {
		if err == nil {
			err = errors.New("HTTPS requires CONNECT")
		}
		g.deny()
		publicProxyError(w, "destination rejected", 403)
		return
	}
	req := r.Clone(g.ctx)
	req.RequestURI = ""
	req.Host = u.Host
	// Site scripts may legitimately issue POST queries. The typed read boundary
	// limits caller actions, not the site's HTTP methods. Charge each upload
	// chunk before exposing it to the transport, against the same global budget
	// as response bodies and encrypted CONNECT traffic.
	if req.Body != nil && req.Body != http.NoBody {
		req.Body = &publicBudgetReadCloser{g: g, ReadCloser: req.Body}
		req.GetBody = nil
	}
	stripPublicHopHeaders(req.Header)
	resp, err := g.transport.RoundTrip(req)
	if err != nil {
		g.countDestinationDenial(err)
		publicProxyError(w, "upstream rejected", 502)
		return
	}
	defer resp.Body.Close()
	stripPublicHopHeaders(resp.Header)
	resp.Header.Del(PublicNetworkErrorHeader)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	// A site/resource disconnect is not a global policy failure. The budget
	// writer independently records fatal budget overruns before returning errors.
	_, _ = io.Copy(&publicBudgetWriter{g: g, w: w}, resp.Body)
}
func stripPublicHopHeaders(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, k := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range []string{"Connection", "Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}
func (g *PublicNetworkGuard) connect(w http.ResponseWriter, r *http.Request) {
	if r.URL.User != nil || r.Host != r.URL.Host {
		g.deny()
		publicProxyError(w, "invalid CONNECT", 403)
		return
	}
	upstream, err := g.dial(g.ctx, "tcp", r.Host)
	if err != nil {
		g.countDestinationDenial(err)
		publicProxyError(w, "destination rejected", 403)
		return
	}
	defer upstream.Close()
	h, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	client, buf, err := h.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if _, err = buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err = buf.Flush(); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&publicBudgetWriter{g: g, w: upstream}, buf)
		_ = upstream.Close()
		_ = client.Close()
		close(done)
	}()
	_, _ = io.Copy(&publicBudgetWriter{g: g, w: client}, upstream)
	_ = upstream.Close()
	_ = client.Close()
	<-done
}

type publicBudgetWriter struct {
	g *PublicNetworkGuard
	w io.Writer
}

func (g *PublicNetworkGuard) chargePayload(n int) error {
	if g.bytes.Add(int64(n)) > g.opts.MaxBytes {
		err := errors.New("public network byte budget exceeded")
		g.fail(err)
		return err
	}
	return nil
}
func (w *publicBudgetWriter) Write(p []byte) (int, error) {
	if err := w.g.chargePayload(len(p)); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}

type publicBudgetReadCloser struct {
	io.ReadCloser
	g *PublicNetworkGuard
}

func (r *publicBudgetReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		if budgetErr := r.g.chargePayload(n); budgetErr != nil {
			return 0, budgetErr
		}
	}
	return n, err
}

type publicTrackedListener struct {
	net.Listener
	g *PublicNetworkGuard
}

func (l *publicTrackedListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e != nil {
		return nil, e
	}
	t := &publicTrackedConn{Conn: c, g: l.g}
	l.g.mu.Lock()
	l.g.conns[t] = struct{}{}
	l.g.connections.Add(1)
	l.g.mu.Unlock()
	return t, nil
}

type publicTrackedConn struct {
	net.Conn
	g    *PublicNetworkGuard
	once sync.Once
}

func (c *publicTrackedConn) Close() error {
	e := c.Conn.Close()
	c.once.Do(func() { c.g.mu.Lock(); delete(c.g.conns, c); c.g.mu.Unlock(); c.g.connections.Done() })
	return e
}
