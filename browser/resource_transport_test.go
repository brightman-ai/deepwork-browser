package browser

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResourceSystemProxyUsesNumericConnectForHTTPAndTLS(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			publicTestEnvironment(t)
			want := []byte{0x89, 'P', 'N', 'G', 0xff, 0, '\n'}
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.IsAbs() {
					t.Error("not origin-form GET")
				}
				w.Header().Set("Content-Type", "image/png")
				w.Write(want)
			})
			var origin *httptest.Server
			scheme, port := "http", "80"
			if secure {
				origin = httptest.NewTLSServer(handler)
				scheme, port = "https", "443"
			} else {
				origin = httptest.NewServer(handler)
			}
			defer origin.Close()
			proxyDone := make(chan struct{})
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(proxyDone)
				if r.Method != "CONNECT" || r.Host != "8.8.8.8:"+port {
					t.Errorf("proxy was authorized hostname/non-CONNECT: %s %s", r.Method, r.Host)
					w.WriteHeader(400)
					return
				}
				upstream, err := net.Dial("tcp", strings.TrimPrefix(strings.TrimPrefix(origin.URL, "https://"), "http://"))
				if err != nil {
					t.Error(err)
					return
				}
				defer upstream.Close()
				client, b, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer client.Close()
				fmt.Fprint(b, "HTTP/1.1 200 Connection Established\r\n\r\n")
				b.Flush()
				done := make(chan struct{})
				go func() { io.Copy(upstream, b); upstream.Close(); close(done) }()
				io.Copy(client, upstream)
				client.Close()
				upstream.Close()
				<-done
			}))
			defer proxy.Close()
			t.Setenv("ALL_PROXY", proxy.URL)
			opts := resourceFetchOptions{network: PublicNetworkOptions{lookupIP: func(context.Context, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			}}}
			if secure {
				opts.rootCAs = x509.NewCertPool()
				opts.rootCAs.AddCert(origin.Certificate())
			}
			r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: scheme + "://example.com/image"}, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(r.Body, want) || r.ContentType != "image/png" || r.Policy.RequestedNetworkRoute != NetworkRouteSystem || !r.Policy.TrustedProxyDelegation || !r.Policy.NumericTargetPinning || r.Policy.NetworkRouteSource != "environment" || r.Policy.EffectiveTransport != "system-http-proxy-numeric-CONNECT" || !r.Cleanup.NetworkClosed {
				t.Fatalf("system route/body facts lost: %+v", r)
			}
			select {
			case <-proxyDone:
			case <-time.After(time.Second):
				t.Fatal("owned proxy tunnel did not finish")
			}
		})
	}
}

func TestResourceInvalidGzipChecksumNeverSucceeds(t *testing.T) {
	var encoded bytes.Buffer
	z := gzip.NewWriter(&encoded)
	z.Write([]byte("decoded exact prefix\n"))
	z.Close()
	compressed := encoded.Bytes()
	compressed[len(compressed)-5] ^= 0xff
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(compressed)
	}))
	defer origin.Close()
	r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "http://resource.example/", NetworkRoute: NetworkRouteDirect}, resourceFixtureOptions(origin.URL))
	d := requireResourceFailure(t, r, err, "network_transport")
	if d.BodyBytesRead != int64(len("decoded exact prefix\n")) {
		t.Fatalf("checksum failure lost decoded consumption: %+v", d)
	}
}

type resourceDelayedCloseConn struct {
	net.Conn
	release <-chan struct{}
	settled chan struct{}
	once    sync.Once
}

func (c *resourceDelayedCloseConn) Close() error {
	<-c.release
	err := c.Conn.Close()
	c.once.Do(func() { close(c.settled) })
	return err
}

func TestResourceCleanupFailureWithholdsSuccessfulBody(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "complete entity") }))
	defer origin.Close()
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	settled := make(chan struct{})
	opts := resourceFixtureOptions(origin.URL)
	opts.rootCAs = x509.NewCertPool()
	opts.rootCAs.AddCert(origin.Certificate())
	address := strings.TrimPrefix(origin.URL, "https://")
	opts.network.dialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &resourceDelayedCloseConn{Conn: c, release: release, settled: settled}, nil
	}
	done := make(chan struct {
		r   *Resource
		err error
	}, 1)
	go func() {
		r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "https://example.com/", NetworkRoute: NetworkRouteDirect}, opts)
		done <- struct {
			r   *Resource
			err error
		}{r, err}
	}()
	select {
	case result := <-done:
		var failure *ResourceAcquisitionError
		if result.r != nil || !errors.As(result.err, &failure) || failure.Diagnostic.Reason != "cleanup_failed" || failure.Diagnostic.Cleanup.NetworkClosed || failure.Diagnostic.BodyBytesRead != int64(len("complete entity")) || failure.Diagnostic.Stage != "cleanup" {
			t.Fatalf("unclean acquisition became success: %v %v", result.r, result.err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("resource cleanup did not respect independent five-second bound")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("owned delayed socket did not finish cleanup")
	}
}
