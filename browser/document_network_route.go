package browser

import (
	"bufio"
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
	"time"
)

type DocumentNetworkRoute string

const (
	DocumentNetworkSystem DocumentNetworkRoute = "system"
	DocumentNetworkDirect DocumentNetworkRoute = "direct"
)

// DocumentNetworkRouting describes destination authorization separately from
// transport trust. A configured proxy is trusted to honor numeric CONNECT.
type DocumentNetworkRouting struct {
	RequestedNetworkRoute  DocumentNetworkRoute `json:"requested_network_route"`
	EffectiveTransport     string               `json:"effective_transport"`
	NetworkRouteSource     string               `json:"network_route_source"`
	NumericTargetPinning   bool                 `json:"numeric_target_pinning"`
	TrustedProxyDelegation bool                 `json:"trusted_proxy_delegation"`
}

type documentRouteSnapshot struct {
	httpProxy, httpsProxy string
	receipt               DocumentNetworkRouting
}

type documentSystemProxySettings struct {
	HTTPEnabled, HTTPSEnabled, SOCKSEnabled, PACEnabled, AutoDiscoveryEnabled bool
	HTTPHost, HTTPSHost                                                       string
	HTTPPort, HTTPSPort                                                       int
}

func normalizeDocumentNetworkRoute(route DocumentNetworkRoute) (DocumentNetworkRoute, error) {
	if route == "" {
		return DocumentNetworkSystem, nil
	}
	if route != DocumentNetworkSystem && route != DocumentNetworkDirect {
		return route, errors.New("network route requires system or direct")
	}
	return route, nil
}

func resolveDocumentNetworkRoute(route DocumentNetworkRoute) (*documentRouteSnapshot, error) {
	route, err := normalizeDocumentNetworkRoute(route)
	s := &documentRouteSnapshot{receipt: DocumentNetworkRouting{RequestedNetworkRoute: route, EffectiveTransport: "not-started", NetworkRouteSource: "unresolved", NumericTargetPinning: true}}
	if err != nil {
		return s, err
	}
	if route == DocumentNetworkDirect {
		s.receipt.EffectiveTransport, s.receipt.NetworkRouteSource = "direct", "explicit-direct"
		return s, nil
	}
	values := map[string]string{}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		upper, lower := os.Getenv(key), os.Getenv(strings.ToLower(key))
		if upper != "" && lower != "" && upper != lower {
			s.receipt.NetworkRouteSource = "environment"
			return s, fmt.Errorf("conflicting proxy environment %s", key)
		}
		if upper == "" {
			upper = lower
		}
		values[key] = upper
	}
	if values["HTTP_PROXY"] != "" || values["HTTPS_PROXY"] != "" || values["ALL_PROXY"] != "" {
		s.receipt.NetworkRouteSource = "environment"
		if os.Getenv("NO_PROXY") != "" || os.Getenv("no_proxy") != "" {
			return s, errors.New("system document route does not support NO_PROXY exclusions; use explicit direct or static proxy configuration")
		}
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
			if values[key] != "" {
				values[key], err = parseDocumentProxyURL(values[key])
				if err != nil {
					return s, fmt.Errorf("unsupported proxy configuration %s: %w", key, err)
				}
			}
		}
		s.httpProxy, s.httpsProxy = values["HTTP_PROXY"], values["HTTPS_PROXY"]
		if s.httpProxy == "" {
			s.httpProxy = values["ALL_PROXY"]
		}
		if s.httpsProxy == "" {
			s.httpsProxy = values["ALL_PROXY"]
		}
	} else {
		settings, source, readErr := readDocumentSystemProxySettings()
		s.receipt.NetworkRouteSource = source
		if readErr != nil {
			return s, readErr
		}
		if err := s.applySystemSettings(settings); err != nil {
			return s, err
		}
	}
	s.receipt.EffectiveTransport = "direct"
	if s.httpProxy != "" || s.httpsProxy != "" {
		s.receipt.TrustedProxyDelegation = true
		s.receipt.EffectiveTransport = "system-http-proxy-numeric-CONNECT"
		if s.httpProxy == "" || s.httpsProxy == "" {
			s.receipt.EffectiveTransport = "mixed"
		}
	}
	return s, nil
}

func (s *documentRouteSnapshot) applySystemSettings(c documentSystemProxySettings) error {
	if c.PACEnabled || c.AutoDiscoveryEnabled {
		return errors.New("system document route does not support PAC or automatic proxy discovery")
	}
	if c.SOCKSEnabled && !c.HTTPEnabled && !c.HTTPSEnabled {
		return errors.New("system document route does not support SOCKS-only configuration")
	}
	for _, p := range []struct {
		enabled     bool
		host        string
		port        int
		destination *string
	}{{c.HTTPEnabled, c.HTTPHost, c.HTTPPort, &s.httpProxy}, {c.HTTPSEnabled, c.HTTPSHost, c.HTTPSPort, &s.httpsProxy}} {
		if !p.enabled {
			continue
		}
		if p.host == "" || p.port < 1 || p.port > 65535 {
			return errors.New("invalid static system HTTP proxy endpoint")
		}
		endpoint, err := parseDocumentProxyURL("http://" + net.JoinHostPort(p.host, strconv.Itoa(p.port)))
		if err != nil {
			return err
		}
		*p.destination = endpoint
	}
	return nil
}

func parseDocumentProxyURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Hostname() == "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(u.Hostname(), "%\r\n") {
		return "", errors.New("only static HTTP proxy endpoints without credentials, paths or zones are supported")
	}
	port := u.Port()
	if port == "" {
		port = "80"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("invalid HTTP proxy port")
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

type documentBufferedTunnel struct {
	net.Conn
	reader *bufio.Reader
}

func (c *documentBufferedTunnel) Read(p []byte) (int, error) { return c.reader.Read(p) }

// The limit applies only while parsing the proxy response, never tunnel data.
type documentProxyHeaderReader struct {
	conn      net.Conn
	remaining int64
	headers   bool
}

func (r *documentProxyHeaderReader) Read(p []byte) (int, error) {
	if r.headers {
		if r.remaining <= 0 {
			return 0, errors.New("system proxy CONNECT response headers exceed 64 KiB")
		}
		if int64(len(p)) > r.remaining {
			p = p[:r.remaining]
		}
	}
	n, err := r.conn.Read(p)
	if r.headers {
		r.remaining -= int64(n)
	}
	return n, err
}

func dialDocumentProxyTunnel(ctx context.Context, proxyAddress, numericTarget string, dial func(context.Context, string, string) (net.Conn, error)) (conn net.Conn, resultErr error) {
	host, _, parseErr := net.SplitHostPort(numericTarget)
	ip, ipErr := netip.ParseAddr(host)
	if parseErr != nil || ipErr != nil || !IsPublicDocumentIP(ip) {
		return nil, publicDestinationDenied("system proxy CONNECT requires a validated numeric public target")
	}
	conn, err := dial(ctx, "tcp", proxyAddress)
	if err != nil {
		return nil, err
	}
	owned := conn
	stop := context.AfterFunc(ctx, func() { _ = owned.Close() })
	defer func() {
		if !stop() || ctx.Err() != nil {
			_ = owned.Close()
			conn = nil
			if resultErr == nil {
				resultErr = ctx.Err()
				if resultErr == nil {
					resultErr = context.Canceled
				}
			}
		}
		if resultErr != nil {
			_ = owned.Close()
			if ctx.Err() != nil {
				resultErr = errors.Join(resultErr, ctx.Err())
			}
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		if err := owned.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	// numericTarget comes only from the guard's fully validated DNS snapshot.
	if _, err := io.WriteString(owned, "CONNECT "+numericTarget+" HTTP/1.1\r\nHost: "+numericTarget+"\r\n\r\n"); err != nil {
		return nil, err
	}
	headerReader := &documentProxyHeaderReader{conn: owned, remaining: 64 << 10, headers: true}
	reader := bufio.NewReader(headerReader)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, fmt.Errorf("system proxy CONNECT response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("system proxy CONNECT rejected with HTTP %d", response.StatusCode)
	}
	headerReader.headers = false
	if err := owned.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return &documentBufferedTunnel{Conn: owned, reader: reader}, nil
}
