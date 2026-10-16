package browser

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
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

func resourceFixtureOptions(originURL string) resourceFetchOptions {
	address := strings.TrimPrefix(strings.TrimPrefix(originURL, "https://"), "http://")
	return resourceFetchOptions{network: PublicNetworkOptions{
		lookupIP: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		},
		dialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}}
}

func requireResourceFailure(t *testing.T, r *Resource, err error, reason string) *ResourceFailureDiagnostic {
	t.Helper()
	var failure *ResourceAcquisitionError
	if r != nil || !errors.As(err, &failure) {
		t.Fatalf("expected nil resource and typed failure: %v %v", r, err)
	}
	if failure.Diagnostic.Reason != reason || failure.Diagnostic.Usable || !failure.Diagnostic.Cleanup.NetworkClosed {
		t.Fatalf("wrong failure receipt: %+v cause=%v", failure.Diagnostic, err)
	}
	return &failure.Diagnostic
}

func TestResourceRejectsTruncatedChunkedBody(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		fmt.Fprint(c, "HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\n\r\n8000\r\n"+strings.Repeat("x", 16<<10))
	}))
	defer origin.Close()
	r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "http://resource.example/binary", NetworkRoute: NetworkRouteDirect}, resourceFixtureOptions(origin.URL))
	d := requireResourceFailure(t, r, err, "network_transport")
	if d.BodyBytesRead != 16<<10 || d.StatusCode != 200 || d.Policy.NetworkBytes != 16<<10 {
		t.Fatalf("lost consumed bytes/status: %+v", d)
	}
}

func TestResourceBinaryBytesHashAndAnonymousGET(t *testing.T) {
	want := append([]byte{0x89, 'P', 'N', 'G', 0, 0xff, 0x80}, []byte("字节\n\r\n")...)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Range") != "" {
			t.Errorf("unexpected acquisition capabilities: %s %v", r.Method, r.Header)
		}
		w.Header().Set("Content-Type", "Application/Octet-Stream")
		w.Write(want)
	}))
	defer origin.Close()
	r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "http://resource.example/data", NetworkRoute: NetworkRouteDirect}, resourceFixtureOptions(origin.URL))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(want)
	if !bytes.Equal(r.Body, want) || r.Bytes != int64(len(want)) || r.SHA256 != hex.EncodeToString(digest[:]) || r.ContentType != "application/octet-stream" || r.Representation != "http-body" || !r.Cleanup.NetworkClosed || r.Policy.Client != "http-get" || r.Policy.NetworkBytes != int64(len(want)) {
		t.Fatalf("binary acquisition changed: %+v", r)
	}
	wire, _ := json.Marshal(r)
	var metadata map[string]any
	json.Unmarshal(wire, &metadata)
	if _, ok := metadata["body"]; ok {
		t.Fatal("body entered receipt")
	}
	if _, ok := metadata["content"]; ok {
		t.Fatal("resource became DOM/text")
	}
	if _, ok := metadata["cleanup"].(map[string]any)["browser_closed"]; ok {
		t.Fatal("resource fabricated browser cleanup")
	}
}

func TestResourceTLSAndNoCookieRedirect(t *testing.T) {
	want := []byte{1, 2, 0xff, '\n'}
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("redirect installed cookies")
		}
		if r.URL.Path == "/start" {
			w.Header().Set("Set-Cookie", "secret=not-a-session")
			http.Redirect(w, r, "/end", 302)
			return
		}
		w.Header()["Content-Type"] = nil
		w.Write(want)
	}))
	defer origin.Close()
	opts := resourceFixtureOptions(origin.URL)
	opts.rootCAs = x509.NewCertPool()
	opts.rootCAs.AddCert(origin.Certificate())
	r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "https://example.com/start", NetworkRoute: NetworkRouteDirect}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.URL != "https://example.com/end" || !bytes.Equal(r.Body, want) || r.ContentType != "" || r.Policy.NetworkRequests != 2 || r.Policy.NetworkBytes <= int64(len(want)) {
		t.Fatalf("TLS/redirect facts wrong: %+v", r)
	}
}

func TestResourceStatusAndBodyBudgets(t *testing.T) {
	for _, status := range []int{404, 500, 206} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); io.WriteString(w, "unusable") }))
			defer origin.Close()
			r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "http://resource.example/", NetworkRoute: NetworkRouteDirect}, resourceFixtureOptions(origin.URL))
			d := requireResourceFailure(t, r, err, "http_status")
			if d.StatusCode != status || d.BodyBytesRead != 0 {
				t.Fatalf("status body consumed: %+v", d)
			}
		})
	}
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprint("limit-gzip-", compressed), func(t *testing.T) {
			payload := bytes.Repeat([]byte("bomb"), 1024)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if compressed {
					w.Header().Set("Content-Encoding", "gzip")
					z := gzip.NewWriter(w)
					z.Write(payload)
					z.Close()
				} else {
					w.Write(payload)
				}
			}))
			defer origin.Close()
			r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "http://resource.example/", NetworkRoute: NetworkRouteDirect, MaxBytes: 32}, resourceFixtureOptions(origin.URL))
			d := requireResourceFailure(t, r, err, "body_budget_exceeded")
			if d.BodyBytesRead != 33 || d.Policy.NetworkBytes <= 0 {
				t.Fatalf("decoded consumption not settled: %+v", d)
			}
			wire, _ := json.Marshal(d)
			if bytes.Contains(wire, []byte("bomb")) || bytes.Contains(wire, []byte(`"body"`)) {
				t.Fatalf("failure leaked bytes: %s", wire)
			}
		})
	}
}

func TestResourceRedirectsRemainPublicAndBounded(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1/private", "http://mixed.example/private", "/loop"} {
		t.Run(target, func(t *testing.T) {
			var hits atomic.Int64
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); http.Redirect(w, r, target, 302) }))
			defer origin.Close()
			opts := resourceFixtureOptions(origin.URL)
			opts.network.lookupIP = func(_ context.Context, host string) ([]netip.Addr, error) {
				if host == "mixed.example" {
					return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, nil
				}
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			}
			r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "http://resource.example/loop", NetworkRoute: NetworkRouteDirect}, opts)
			reason := "network_policy"
			expectedHits := int64(1)
			if target == "/loop" {
				reason = "redirect_limit"
				expectedHits = 6
			}
			d := requireResourceFailure(t, r, err, reason)
			if hits.Load() != expectedHits || d.BodyBytesRead != 0 {
				t.Fatalf("redirect escaped authorization/consumption: hits=%d diag=%+v", hits.Load(), d)
			}
		})
	}
}

func TestResourceCancellationDeadlineAndConsumedPrefix(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			entered := make(chan struct{})
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !deadline {
					w.Write(bytes.Repeat([]byte("x"), 32<<10))
					w.(http.Flusher).Flush()
				}
				close(entered)
				<-r.Context().Done()
			}))
			defer origin.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request := ResourceRequest{URL: "http://resource.example/", NetworkRoute: NetworkRouteDirect}
			if deadline {
				request.Timeout = 40 * time.Millisecond
			}
			done := make(chan struct {
				r   *Resource
				err error
			}, 1)
			go func() {
				r, err := fetchResourceWithOptions(ctx, request, resourceFixtureOptions(origin.URL))
				done <- struct {
					r   *Resource
					err error
				}{r, err}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("owned origin did not receive request")
			}
			if !deadline {
				time.Sleep(30 * time.Millisecond)
				cancel()
			}
			select {
			case result := <-done:
				reason := "canceled"
				if deadline {
					reason = "timeout"
				}
				d := requireResourceFailure(t, result.r, result.err, reason)
				if deadline && d.BodyBytesRead != 0 {
					t.Fatalf("invented consumed bytes before headers: %+v", d)
				}
				if !deadline && (d.BodyBytesRead <= 0 || d.BodyBytesRead > 32<<10) {
					t.Fatalf("lost consumed prefix: %+v", d)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("resource ignored cancellation")
			}
		})
	}
}

func TestResourceNetworkBudgetAndProxyProvenance(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(bytes.Repeat([]byte("x"), 64<<10)) }))
	defer origin.Close()
	r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "http://resource.example/", NetworkRoute: NetworkRouteDirect, MaxNetworkBytes: 16}, resourceFixtureOptions(origin.URL))
	d := requireResourceFailure(t, r, err, "network_policy")
	if d.Policy.NetworkBytes <= 16 {
		t.Fatalf("guard did not settle offered bytes: %+v", d)
	}
	closed, _ := net.Listen("tcp4", "127.0.0.1:0")
	address := closed.Addr().String()
	closed.Close()
	for _, scheme := range []string{"http", "https"} {
		for _, private := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-private-%t", scheme, private), func(t *testing.T) {
				opts := resourceFixtureOptions("http://" + address)
				if private {
					opts.network.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
						return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
					}
				}
				r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: scheme + "://resource.example/", NetworkRoute: NetworkRouteDirect}, opts)
				reason := "network_transport"
				if private {
					reason = "network_policy"
				}
				d := requireResourceFailure(t, r, err, reason)
				if d.BodyBytesRead != 0 || d.Policy.NetworkBytes != 0 {
					t.Fatalf("invented bytes without upstream: %+v", d)
				}
			})
		}
	}
}

func TestResourceValidationAndExplicitZeroConsumedBytes(t *testing.T) {
	for _, request := range []ResourceRequest{{URL: "file:///etc/passwd"}, {URL: "http://127.0.0.1/"}, {URL: "https://user:secret@example.com/"}, {URL: "https://example.com:22/"}, {URL: "https://example.com/", MaxBytes: -1}, {URL: "https://example.com/", MaxBytes: int64(^uint64(0) >> 1)}, {URL: "https://example.com/", NetworkRoute: "other"}} {
		r, err := FetchResource(context.Background(), request)
		var failure *ResourceAcquisitionError
		if r != nil || !errors.As(err, &failure) || failure.Diagnostic.BodyBytesRead != 0 || failure.Diagnostic.Stage != "validation" {
			t.Fatalf("invalid request crossed boundary: %v %v", r, err)
		}
		wire, _ := json.Marshal(failure.Diagnostic)
		if !bytes.Contains(wire, []byte(`"body_bytes_read":0`)) {
			t.Fatalf("zero consumption omitted: %s", wire)
		}
	}
}

func TestResourceDefaults(t *testing.T) {
	r, err := normalizeResourceRequest(ResourceRequest{URL: "https://example.com/"})
	if err != nil || r.Timeout != 20*time.Second || r.MaxBytes != 8<<20 || r.MaxNetworkBytes != 32<<20 || r.NetworkRoute != NetworkRouteSystem {
		t.Fatalf("wrong resource defaults: %+v %v", r, err)
	}
}
