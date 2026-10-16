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

type NetworkRoute string

const (
	NetworkRouteSystem NetworkRoute = "system"
	NetworkRouteDirect NetworkRoute = "direct"
)

// NetworkRouting describes destination authorization separately from
// transport trust. A configured proxy is trusted to honor numeric CONNECT.
type NetworkRouting struct {
	DNSResolver            string       `json:"dns_resolver"`
	RequestedNetworkRoute  NetworkRoute `json:"requested_network_route"`
	EffectiveTransport     string       `json:"effective_transport"`
	NetworkRouteSource     string       `json:"network_route_source"`
	NumericTargetPinning   bool         `json:"numeric_target_pinning"`
	TrustedProxyDelegation bool         `json:"trusted_proxy_delegation"`
}

type publicRouteSnapshot struct {
	httpProxy, httpsProxy string
	receipt               NetworkRouting
}

type publicSystemProxySettings struct {
	HTTPEnabled, HTTPSEnabled, SOCKSEnabled, PACEnabled, AutoDiscoveryEnabled bool
	HTTPHost, HTTPSHost                                                       string
	HTTPPort, HTTPSPort                                                       int
}

func normalizeNetworkRoute(route NetworkRoute) (NetworkRoute, error) {
	if route == "" {
		return NetworkRouteSystem, nil
	}
	if route != NetworkRouteSystem && route != NetworkRouteDirect {
		return route, errors.New("network route requires system or direct")
	}
	return route, nil
}

func resolveNetworkRoute(route NetworkRoute) (*publicRouteSnapshot, error) {
	route, err := normalizeNetworkRoute(route)
	s := &publicRouteSnapshot{receipt: NetworkRouting{DNSResolver: SystemDNSResolver, RequestedNetworkRoute: route, EffectiveTransport: "not-started", NetworkRouteSource: "unresolved", NumericTargetPinning: true}}
	if err != nil {
		return s, err
	}
	if route == NetworkRouteDirect {
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
			return s, errors.New("system network route does not support NO_PROXY exclusions; use explicit direct or static proxy configuration")
		}
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
			if values[key] != "" {
				values[key], err = parsePublicProxyURL(values[key])
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
		settings, source, readErr := readSystemProxySettings()
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

func (s *publicRouteSnapshot) applySystemSettings(c publicSystemProxySettings) error {
	if c.PACEnabled || c.AutoDiscoveryEnabled {
		return errors.New("system network route does not support PAC or automatic proxy discovery")
	}
	if c.SOCKSEnabled && !c.HTTPEnabled && !c.HTTPSEnabled {
		return errors.New("system network route does not support SOCKS-only configuration")
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
		endpoint, err := parsePublicProxyURL("http://" + net.JoinHostPort(p.host, strconv.Itoa(p.port)))
		if err != nil {
			return err
		}
		*p.destination = endpoint
	}
	return nil
}

func parsePublicProxyURL(raw string) (string, error) {
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

type publicBufferedTunnel struct {
	net.Conn
	reader *bufio.Reader
}

func (c *publicBufferedTunnel) Read(p []byte) (int, error) { return c.reader.Read(p) }

// The limit applies only while parsing the proxy response, never tunnel data.
type publicProxyHeaderReader struct {
	conn      net.Conn
	remaining int64
	headers   bool
}

func (r *publicProxyHeaderReader) Read(p []byte) (int, error) {
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

func dialPublicProxyTunnel(ctx context.Context, proxyAddress, numericTarget string, dial func(context.Context, string, string) (net.Conn, error)) (conn net.Conn, resultErr error) {
	host, _, parseErr := net.SplitHostPort(numericTarget)
	ip, ipErr := netip.ParseAddr(host)
	if parseErr != nil || ipErr != nil || !IsPublicIP(ip) {
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
	headerReader := &publicProxyHeaderReader{conn: owned, remaining: 64 << 10, headers: true}
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
	return &publicBufferedTunnel{Conn: owned, reader: reader}, nil
}
