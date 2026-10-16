package browser

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const SystemDNSResolver = "system"
const publicDNSMessageLimit = 65535

type publicDNSFailureObservation struct {
	err      error
	observed time.Time
}

func publicDNSAuthority(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return ""
	}
	return net.JoinHostPort(strings.TrimSuffix(strings.ToLower(host), "."), port)
}

// This bounded per-job ledger contains failures only, never answer/IP caching.
// It is consulted only after a failed main navigation, never to poison a page
// merely because an optional resource failed DNS.
func (g *PublicNetworkGuard) observeDNSFailure(address string, err error) {
	var dns *DNSResolutionError
	if !errors.As(err, &dns) {
		var denied *publicDestinationError
		if !errors.As(err, &denied) {
			return
		}
		err = &publicNetworkPolicyFailure{err}
	}
	key := publicDNSAuthority(address)
	if key == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dnsFailures[key] = publicDNSFailureObservation{err: err, observed: time.Now()}
}

func (g *PublicNetworkGuard) clearDNSFailure(address string) {
	g.mu.Lock()
	delete(g.dnsFailures, publicDNSAuthority(address))
	g.mu.Unlock()
}

func (g *PublicNetworkGuard) dnsFailureForURL(raw string, since time.Time) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	key := publicDNSAuthority(net.JoinHostPort(u.Hostname(), port))
	g.mu.Lock()
	defer g.mu.Unlock()
	failure, ok := g.dnsFailures[key]
	if !ok || failure.observed.Before(since) {
		return nil
	}
	return failure.err
}

func dnsResolverForReceipt(raw string) string {
	canonical, err := ValidateDNSResolver(raw)
	if err != nil {
		return "invalid"
	}
	return canonical
}

// ValidateDNSResolver selects only an explicit numeric HTTPS RFC 8484 endpoint.
// No hostname bootstrap, credentials, alternate port, query or fallback exists.
func ValidateDNSResolver(raw string) (string, error) {
	if raw == "" || raw == SystemDNSResolver {
		return SystemDNSResolver, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Path != "/dns-query" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || strings.HasSuffix(u.Host, ":") || (u.Port() != "" && u.Port() != "443") {
		return "", errors.New("DNS resolver requires system or https://PUBLIC_NUMERIC_IP/dns-query on port 443 without credentials, query or fragment")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !IsPublicIP(ip) {
		return "", errors.New("DNS resolver endpoint must be a literal public IP")
	}
	ip = ip.Unmap()
	host := ip.String()
	if ip.Is6() {
		host = "[" + host + "]"
	}
	return "https://" + host + "/dns-query", nil
}

// DNSResolutionError survives the local guard's DNS provenance marker and the
// acquisition wrapper. Resolver and Host are explicit; only the query hostname
// is sent to the resolver, never the origin URL/path/body.
type DNSResolutionError struct {
	Resolver string
	Host     string
	Family   string
	Reason   string
	Cause    error
}

func (e *DNSResolutionError) Error() string {
	message := fmt.Sprintf("DNS resolver %s failed for %s (%s)", e.Resolver, e.Host, e.Reason)
	if e.Family != "" {
		message += " [" + e.Family + "]"
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}
func (e *DNSResolutionError) Unwrap() error { return e.Cause }

func withDNSFailureContext(err error, resolver string) error {
	var dns *DNSResolutionError
	if !errors.As(err, &dns) || dns.Resolver != "" {
		return err
	}
	return &DNSResolutionError{Resolver: resolver, Host: dns.Host, Family: dns.Family, Reason: dns.Reason, Cause: err}
}

func newPublicDNSClient(g *PublicNetworkGuard) *http.Client {
	proxy, _ := url.Parse(g.ProxyURL())
	t := &http.Transport{Proxy: http.ProxyURL(proxy), DisableKeepAlives: true, MaxResponseHeaderBytes: 64 << 10, ResponseHeaderTimeout: g.opts.Timeout,
		OnProxyConnectResponse: func(_ context.Context, _ *url.URL, _ *http.Request, r *http.Response) error {
			return publicProxyResponseFailure(r.Header)
		},
	}
	if g.opts.resolverRootCAs != nil {
		t.TLSClientConfig = &tls.Config{RootCAs: g.opts.resolverRootCAs, MinVersion: tls.VersionTLS12}
	}
	g.dnsTransport = t
	return &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (g *PublicNetworkGuard) resolvePublicDNS(ctx context.Context, host string) ([]netip.Addr, error) {
	if g.opts.DNSResolver == SystemDNSResolver {
		var ips []netip.Addr
		var err error
		if g.opts.lookupIP != nil {
			ips, err = g.opts.lookupIP(ctx, host)
		} else {
			ips, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
		if err != nil {
			return nil, &DNSResolutionError{Resolver: SystemDNSResolver, Host: host, Reason: "resolution_failed", Cause: err}
		}
		return ips, nil
	}
	var all []netip.Addr
	for _, family := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		ips, err := g.queryPublicDNS(ctx, host, family)
		if err != nil {
			return nil, err
		}
		all = append(all, ips...)
	}
	if len(all) == 0 {
		return nil, &DNSResolutionError{Resolver: g.opts.DNSResolver, Host: host, Reason: "empty_answers", Cause: errors.New("both A and AAAA returned NODATA")}
	}
	return all, nil
}

func (g *PublicNetworkGuard) queryPublicDNS(ctx context.Context, host string, family dnsmessage.Type) (ips []netip.Addr, resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = &DNSResolutionError{Resolver: g.opts.DNSResolver, Host: host, Family: family.String(), Reason: "resolution_failed", Cause: resultErr}
		}
	}()
	name, err := dnsmessage.NewName(strings.TrimSuffix(host, ".") + ".")
	if err != nil {
		return nil, err
	}
	var token [2]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	question := dnsmessage.Question{Name: name, Type: family, Class: dnsmessage.ClassINET}
	query := dnsmessage.Message{Header: dnsmessage.Header{ID: binary.BigEndian.Uint16(token[:]), RecursionDesired: true}, Questions: []dnsmessage.Question{question}}
	wire, err := query.Pack()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.opts.DNSResolver, bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	response, err := g.dnsClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("DoH HTTP status %d", response.StatusCode)
	}
	contentTypes := response.Header.Values("Content-Type")
	if len(contentTypes) == 0 {
		return nil, errors.New("DoH response requires application/dns-message")
	}
	for _, value := range contentTypes {
		typ, _, err := mime.ParseMediaType(value)
		if err != nil || typ != "application/dns-message" {
			return nil, errors.New("DoH response requires application/dns-message")
		}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, publicDNSMessageLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > publicDNSMessageLimit {
		return nil, errors.New("DoH response exceeds DNS message byte limit")
	}
	return validatePublicDNSAnswer(body, query.Header.ID, question)
}

func canonicalDNSName(n dnsmessage.Name) string { return strings.ToLower(n.String()) }
func dnsZoneContains(zone, name string) bool {
	return zone == "." || name == zone || strings.HasSuffix(name, "."+zone)
}

func validatePublicDNSAnswer(wire []byte, id uint16, question dnsmessage.Question) ([]netip.Addr, error) {
	var message dnsmessage.Message
	if err := message.Unpack(wire); err != nil {
		return nil, fmt.Errorf("invalid DNS message: %w", err)
	}
	if err := validateDNSWireFraming(wire); err != nil {
		return nil, err
	}
	if message.ID != id || !message.Response || message.OpCode != 0 || message.Truncated || message.RCode != dnsmessage.RCodeSuccess {
		return nil, errors.New("DNS response ID/QR/opcode/rcode/truncation mismatch")
	}
	if len(message.Questions) != 1 {
		return nil, errors.New("DNS response requires one echoed question")
	}
	q := message.Questions[0]
	if canonicalDNSName(q.Name) != canonicalDNSName(question.Name) || q.Type != question.Type || q.Class != dnsmessage.ClassINET {
		return nil, errors.New("DNS echoed question mismatch")
	}
	aliases := map[string]string{}
	addresses := map[string][]netip.Addr{}
	for _, answer := range message.Answers {
		if answer.Header.Class != dnsmessage.ClassINET {
			return nil, errors.New("DNS answer class mismatch")
		}
		owner := canonicalDNSName(answer.Header.Name)
		switch body := answer.Body.(type) {
		case *dnsmessage.CNAMEResource:
			if _, exists := aliases[owner]; exists {
				return nil, errors.New("duplicate/conflicting DNS CNAME owner")
			}
			aliases[owner] = canonicalDNSName(body.CNAME)
		case *dnsmessage.AResource:
			if question.Type != dnsmessage.TypeA {
				return nil, errors.New("wrong address family in DNS answer")
			}
			addresses[owner] = append(addresses[owner], netip.AddrFrom4(body.A))
		case *dnsmessage.AAAAResource:
			if question.Type != dnsmessage.TypeAAAA {
				return nil, errors.New("wrong address family in DNS answer")
			}
			addresses[owner] = append(addresses[owner], netip.AddrFrom16(body.AAAA))
		default:
			return nil, errors.New("unexpected DNS answer type")
		}
	}
	current := canonicalDNSName(question.Name)
	seen := map[string]bool{}
	for {
		if seen[current] || len(seen) > 16 {
			return nil, errors.New("DNS CNAME chain loop or excessive depth")
		}
		seen[current] = true
		next, exists := aliases[current]
		if !exists {
			break
		}
		if len(addresses[current]) != 0 {
			return nil, errors.New("DNS owner has both CNAME and address")
		}
		current = next
	}
	for owner := range aliases {
		if !seen[owner] {
			return nil, errors.New("unrelated CNAME in DNS answer")
		}
	}
	for owner := range addresses {
		if owner != current {
			return nil, errors.New("unrelated address in DNS answer")
		}
	}
	for _, rr := range message.Additionals {
		if rr.Header.Type == dnsmessage.TypeOPT && rr.Header.TTL>>24 != 0 {
			return nil, errors.New("DNS extended rcode error")
		}
	}
	if len(addresses[current]) == 0 && len(aliases) > 0 {
		proved := false
		for _, rr := range message.Authorities {
			if _, ok := rr.Body.(*dnsmessage.SOAResource); ok && rr.Header.Class == dnsmessage.ClassINET && dnsZoneContains(canonicalDNSName(rr.Header.Name), current) {
				proved = true
			}
		}
		if !proved {
			return nil, errors.New("DNS CNAME chain lacks final address or matching SOA NODATA proof")
		}
	}
	if len(addresses[current]) == 0 && len(aliases) == 0 && len(message.Authorities) > 0 {
		proved := false
		for _, rr := range message.Authorities {
			if _, ok := rr.Body.(*dnsmessage.SOAResource); ok && rr.Header.Class == dnsmessage.ClassINET && dnsZoneContains(canonicalDNSName(rr.Header.Name), current) {
				proved = true
			}
		}
		if !proved {
			return nil, errors.New("DNS referral or incomplete negative response lacks matching SOA")
		}
	}
	return addresses[current], nil
}

// dnsmessage validates names and RDATA semantics. This small framing pass adds
// exact-message consumption and fixed RDLENGTH checks not exposed by its parser.
func validateDNSWireFraming(wire []byte) error {
	if len(wire) < 12 {
		return errors.New("short DNS header")
	}
	off := 12
	skipName := func(start int) (int, error) {
		for start < len(wire) {
			b := wire[start]
			start++
			if b == 0 {
				return start, nil
			}
			if b&0xc0 == 0xc0 {
				if start >= len(wire) {
					return 0, errors.New("short DNS pointer")
				}
				return start + 1, nil
			}
			if b&0xc0 != 0 || start+int(b) > len(wire) {
				return 0, errors.New("invalid DNS label framing")
			}
			start += int(b)
		}
		return 0, errors.New("unterminated DNS name")
	}
	questions := int(binary.BigEndian.Uint16(wire[4:6]))
	records := int(binary.BigEndian.Uint16(wire[6:8])) + int(binary.BigEndian.Uint16(wire[8:10])) + int(binary.BigEndian.Uint16(wire[10:12]))
	var err error
	for i := 0; i < questions; i++ {
		off, err = skipName(off)
		if err != nil {
			return err
		}
		off += 4
		if off > len(wire) {
			return errors.New("short DNS question")
		}
	}
	for i := 0; i < records; i++ {
		off, err = skipName(off)
		if err != nil {
			return err
		}
		if off+10 > len(wire) {
			return errors.New("short DNS resource header")
		}
		typ := dnsmessage.Type(binary.BigEndian.Uint16(wire[off : off+2]))
		length := int(binary.BigEndian.Uint16(wire[off+8 : off+10]))
		off += 10
		end := off + length
		if end > len(wire) {
			return errors.New("short DNS resource data")
		}
		if (typ == dnsmessage.TypeA && length != 4) || (typ == dnsmessage.TypeAAAA && length != 16) {
			return errors.New("DNS address RDLENGTH mismatch")
		}
		if typ == dnsmessage.TypeCNAME || typ == dnsmessage.TypeSOA {
			tail, e := skipName(off)
			if e != nil {
				return e
			}
			if typ == dnsmessage.TypeSOA {
				tail, e = skipName(tail)
				if e != nil {
					return e
				}
				tail += 20
			}
			if tail != end {
				return errors.New("DNS name/SOA RDLENGTH mismatch")
			}
		}
		off = end
	}
	if off != len(wire) {
		return errors.New("trailing bytes after DNS message")
	}
	return nil
}
