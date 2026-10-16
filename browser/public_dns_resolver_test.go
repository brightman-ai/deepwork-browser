package browser

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDNSResolverEndpointContract(t *testing.T) {
	for _, raw := range []string{"", "system", "https://1.1.1.1/dns-query", "https://1.1.1.1:443/dns-query", "https://[2606:4700:4700::1111]/dns-query"} {
		if _, err := ValidateDNSResolver(raw); err != nil {
			t.Errorf("valid resolver rejected %s: %v", raw, err)
		}
	}
	for _, raw := range []string{"auto", "http://1.1.1.1/dns-query", "https://dns.google/dns-query", "https://127.0.0.1/dns-query", "https://[::ffff:127.0.0.1]/dns-query", "https://192.0.2.1/dns-query", "https://1.1.1.1:8443/dns-query", "https://1.1.1.1:/dns-query", "https://user:secret@1.1.1.1/dns-query", "https://1.1.1.1/dns-query?x=1", "https://1.1.1.1/dns-query?", "https://1.1.1.1/dns-query#", "https://1.1.1.1/other"} {
		if _, err := ValidateDNSResolver(raw); err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("unsafe resolver accepted/leaked %s: %v", raw, err)
		}
	}
}

func dnsTestName(s string) dnsmessage.Name { return dnsmessage.MustNewName(s) }
func dnsTestAddress(name dnsmessage.Name, typ dnsmessage.Type) dnsmessage.Resource {
	h := dnsmessage.ResourceHeader{Name: name, Type: typ, Class: dnsmessage.ClassINET, TTL: 60}
	if typ == dnsmessage.TypeA {
		return dnsmessage.Resource{Header: h, Body: &dnsmessage.AResource{A: [4]byte{1, 1, 1, 1}}}
	}
	return dnsmessage.Resource{Header: h, Body: &dnsmessage.AAAAResource{AAAA: netip.MustParseAddr("2606:4700:4700::1111").As16()}}
}
func dnsTestResponse(q dnsmessage.Message) dnsmessage.Message {
	return dnsmessage.Message{Header: dnsmessage.Header{ID: q.ID, Response: true, RecursionAvailable: true}, Questions: q.Questions, Answers: []dnsmessage.Resource{dnsTestAddress(q.Questions[0].Name, q.Questions[0].Type)}}
}
func dnsTestCNAME(owner, target string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: dnsTestName(owner), Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET}, Body: &dnsmessage.CNAMEResource{CNAME: dnsTestName(target)}}
}
func dnsTestSOA(zone string) dnsmessage.Resource {
	return dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: dnsTestName(zone), Type: dnsmessage.TypeSOA, Class: dnsmessage.ClassINET}, Body: &dnsmessage.SOAResource{NS: dnsTestName("ns." + zone), MBox: dnsTestName("hostmaster." + zone)}}
}

func TestPublicDNSCompleteAnswersAndNegativeProof(t *testing.T) {
	question := dnsmessage.Question{Name: dnsTestName("asset.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	query := dnsmessage.Message{Header: dnsmessage.Header{ID: 17}, Questions: []dnsmessage.Question{question}}
	for _, tc := range []struct {
		name   string
		change func(*dnsmessage.Message)
		valid  bool
		count  int
	}{
		{"address", func(*dnsmessage.Message) {}, true, 1},
		{"bare-nodata", func(m *dnsmessage.Message) { m.Answers = nil }, true, 0},
		{"cname-complete", func(m *dnsmessage.Message) {
			m.Answers = []dnsmessage.Resource{dnsTestCNAME("asset.example.", "cdn.example."), dnsTestAddress(dnsTestName("cdn.example."), dnsmessage.TypeA)}
		}, true, 1},
		{"cname-nodata-soa", func(m *dnsmessage.Message) {
			m.Answers = []dnsmessage.Resource{dnsTestCNAME("asset.example.", "cdn.example.")}
			m.Authorities = []dnsmessage.Resource{dnsTestSOA("example.")}
		}, true, 0},
		{"bad-id", func(m *dnsmessage.Message) { m.ID++ }, false, 0},
		{"not-response", func(m *dnsmessage.Message) { m.Response = false }, false, 0},
		{"wrong-opcode", func(m *dnsmessage.Message) { m.OpCode = 1 }, false, 0},
		{"rcode", func(m *dnsmessage.Message) { m.RCode = dnsmessage.RCodeNameError }, false, 0},
		{"truncated", func(m *dnsmessage.Message) { m.Truncated = true }, false, 0},
		{"wrong-question", func(m *dnsmessage.Message) {
			m.Questions = []dnsmessage.Question{{Name: dnsTestName("other.example."), Type: question.Type, Class: question.Class}}
		}, false, 0},
		{"wrong-family", func(m *dnsmessage.Message) {
			m.Questions = []dnsmessage.Question{{Name: question.Name, Type: dnsmessage.TypeAAAA, Class: question.Class}}
		}, false, 0},
		{"wrong-class", func(m *dnsmessage.Message) {
			m.Questions = []dnsmessage.Question{{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassCHAOS}}
		}, false, 0},
		{"wrong-answer-class", func(m *dnsmessage.Message) { m.Answers[0].Header.Class = dnsmessage.ClassCHAOS }, false, 0},
		{"two-questions", func(m *dnsmessage.Message) {
			m.Questions = append(append([]dnsmessage.Question{}, m.Questions...), question)
		}, false, 0},
		{"cname-incomplete", func(m *dnsmessage.Message) {
			m.Answers = []dnsmessage.Resource{dnsTestCNAME("asset.example.", "cdn.example.")}
		}, false, 0},
		{"cname-wrong-zone", func(m *dnsmessage.Message) {
			m.Answers = []dnsmessage.Resource{dnsTestCNAME("asset.example.", "cdn.example.")}
			m.Authorities = []dnsmessage.Resource{dnsTestSOA("notexample.")}
		}, false, 0},
		{"cname-loop", func(m *dnsmessage.Message) {
			m.Answers = []dnsmessage.Resource{dnsTestCNAME("asset.example.", "cdn.example."), dnsTestCNAME("cdn.example.", "asset.example.")}
		}, false, 0},
		{"duplicate-cname", func(m *dnsmessage.Message) {
			m.Answers = []dnsmessage.Resource{dnsTestCNAME("asset.example.", "cdn.example."), dnsTestCNAME("asset.example.", "cdn.example.")}
		}, false, 0},
		{"unrelated-address", func(m *dnsmessage.Message) {
			m.Answers = append(m.Answers, dnsTestAddress(dnsTestName("other.example."), dnsmessage.TypeA))
		}, false, 0},
		{"unrelated-cname", func(m *dnsmessage.Message) {
			m.Answers = append(m.Answers, dnsTestCNAME("other.example.", "cdn.example."))
		}, false, 0},
		{"cname-and-address", func(m *dnsmessage.Message) {
			m.Answers = append(m.Answers, dnsTestCNAME("asset.example.", "cdn.example."))
		}, false, 0},
		{"referral-not-nodata", func(m *dnsmessage.Message) {
			m.Answers = nil
			m.Authorities = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: dnsTestName("example."), Type: dnsmessage.TypeNS, Class: dnsmessage.ClassINET}, Body: &dnsmessage.NSResource{NS: dnsTestName("ns.example.")}}}
		}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := dnsTestResponse(query)
			tc.change(&m)
			wire, err := m.Pack()
			if err != nil {
				t.Fatal(err)
			}
			ips, err := validatePublicDNSAnswer(wire, 17, question)
			if (err == nil) != tc.valid || len(ips) != tc.count {
				t.Fatalf("answer validation ips=%v err=%v", ips, err)
			}
		})
	}
}

func TestPublicDNSWireFramingRejectsMalformedMessages(t *testing.T) {
	q := dnsmessage.Question{Name: dnsTestName("asset.example."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	m := dnsTestResponse(dnsmessage.Message{Header: dnsmessage.Header{ID: 17}, Questions: []dnsmessage.Question{q}})
	valid, _ := m.Pack()
	for _, wire := range [][]byte{nil, valid[:11], valid[:len(valid)-1], append(append([]byte{}, valid...), 0), append([]byte{}, valid...)} {
		if len(wire) == len(valid) { // Last case: an A record declares five bytes but has only four.
			wire[len(wire)-6] = 0
			wire[len(wire)-5] = 5
		}
		if _, err := validatePublicDNSAnswer(wire, 17, q); err == nil {
			t.Fatalf("malformed DNS framing accepted (%d bytes)", len(wire))
		}
	}
}

type ownedDOHFixture struct {
	resolver   string
	options    resourceFetchOptions
	queries    atomic.Int64
	originHits atomic.Int64
	query      func(http.ResponseWriter, *http.Request, dnsmessage.Message)
}

func newOwnedDOHFixture(t *testing.T, query func(http.ResponseWriter, *http.Request, dnsmessage.Message), wrongIP bool) *ownedDOHFixture {
	return newOwnedDOHFixtureForNames(t, query, wrongIP, func(name string) bool { return name == "asset.example." })
}

func newOwnedDOHFixtureForNames(t *testing.T, query func(http.ResponseWriter, *http.Request, dnsmessage.Message), wrongIP bool, allowedName func(string) bool) *ownedDOHFixture {
	t.Helper()
	publicTestEnvironment(t)
	f := &ownedDOHFixture{resolver: "https://8.8.8.8/dns-query", query: query}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.originHits.Add(1)
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/final", 302)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "origin entity\n")
	}))
	t.Cleanup(origin.Close)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP("8.8.8.8")
	if wrongIP {
		ip = net.ParseIP("9.9.9.9")
	}
	certTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{ip}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certTemplate, certTemplate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv}))
	if err != nil {
		t.Fatal(err)
	}
	doh := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.queries.Add(1)
		if r.Method != "POST" || r.URL.Path != "/dns-query" || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/dns-message" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("not private RFC8484 POST: %s %s %v", r.Method, r.URL, r.Header)
		}
		wire, err := io.ReadAll(io.LimitReader(r.Body, publicDNSMessageLimit+1))
		if err != nil {
			t.Error(err)
			return
		}
		var q dnsmessage.Message
		if err := q.Unpack(wire); err != nil || len(q.Questions) != 1 {
			t.Errorf("invalid question %v", err)
			return
		}
		if !allowedName(q.Questions[0].Name.String()) {
			t.Errorf("resolver received more/different than target hostname: %v", q.Questions)
		}
		if query != nil {
			query(w, r, q)
			return
		}
		m := dnsTestResponse(q)
		body, _ := m.Pack()
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(body)
	}))
	doh.Config.ErrorLog = log.New(io.Discard, "", 0)
	doh.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	doh.StartTLS()
	t.Cleanup(doh.Close)
	var mu sync.Mutex
	owned := map[net.Conn]bool{}
	var handlers sync.WaitGroup
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, port, err := net.SplitHostPort(r.Host)
		ip, ipErr := netip.ParseAddr(host)
		if r.Method != "CONNECT" || err != nil || ipErr != nil || !IsPublicIP(ip) {
			t.Errorf("not numeric CONNECT: %s %s", r.Method, r.Host)
			w.WriteHeader(400)
			return
		}
		address := strings.TrimPrefix(origin.URL, "http://")
		if host == "8.8.8.8" && port == "443" {
			address = strings.TrimPrefix(doh.URL, "https://")
		} else if (host != "1.1.1.1" && host != "2606:4700:4700::1111") || port != "80" {
			t.Errorf("unexpected origin target %s", r.Host)
			w.WriteHeader(400)
			return
		}
		upstream, err := net.Dial("tcp", address)
		if err != nil {
			t.Error(err)
			return
		}
		client, b, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			t.Error(err)
			return
		}
		handlers.Add(1)
		mu.Lock()
		owned[client] = true
		owned[upstream] = true
		mu.Unlock()
		defer func() {
			client.Close()
			upstream.Close()
			mu.Lock()
			delete(owned, client)
			delete(owned, upstream)
			mu.Unlock()
			handlers.Done()
		}()
		fmt.Fprint(b, "HTTP/1.1 200 Connection Established\r\n\r\n")
		b.Flush()
		done := make(chan struct{})
		go func() { io.Copy(upstream, b); upstream.Close(); close(done) }()
		io.Copy(client, upstream)
		client.Close()
		upstream.Close()
		<-done
	}))
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Cleanup(func() {
		proxy.Close()
		mu.Lock()
		for c := range owned {
			c.Close()
		}
		mu.Unlock()
		done := make(chan struct{})
		go func() { handlers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("owned proxy handlers did not join")
		}
	})
	f.options.network.resolverRootCAs = x509.NewCertPool()
	f.options.network.resolverRootCAs.AddCert(doh.Certificate())
	f.options.network.lookupIP = func(context.Context, string) ([]netip.Addr, error) {
		t.Error("DoH silently fell back to system lookup")
		return nil, errors.New("system lookup forbidden")
	}
	return f
}

func TestResourceDoHSharedNumericTransportAndFreshSnapshots(t *testing.T) {
	f := newOwnedDOHFixture(t, nil, false)
	for i := 0; i < 2; i++ {
		r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "http://asset.example/private/path?not-for-dns=1", DNSResolver: f.resolver}, f.options)
		if err != nil {
			t.Fatal(err)
		}
		if string(r.Body) != "origin entity\n" || r.Policy.DNSResolver != f.resolver || r.Policy.NetworkRequests != 3 || r.Policy.NetworkBytes <= int64(r.Bytes) || !r.Cleanup.NetworkClosed {
			t.Fatalf("DoH accounting/body facts wrong: %+v", r)
		}
	}
	if f.queries.Load() != 4 || f.originHits.Load() != 2 {
		t.Fatalf("snapshot was cached/partial: queries=%d origins=%d", f.queries.Load(), f.originHits.Load())
	}
}

func TestResourceDoHRedirectGetsFreshSnapshot(t *testing.T) {
	f := newOwnedDOHFixture(t, nil, false)
	r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "http://asset.example/redirect", DNSResolver: f.resolver}, f.options)
	if err != nil {
		t.Fatal(err)
	}
	if f.queries.Load() != 4 || f.originHits.Load() != 2 || r.Policy.NetworkRequests != 6 || r.URL != "http://asset.example/final" {
		t.Fatalf("redirect reused DNS or bypassed guard: queries=%d origins=%d resource=%+v", f.queries.Load(), f.originHits.Load(), r)
	}
}

func TestResourceDoHFamilyCompletenessAndPublicGate(t *testing.T) {
	for _, mode := range []string{"ipv6-only", "ipv4-only", "both-empty", "aaaa-error", "private-a", "private-aaaa", "referral", "cname-soa", "cname-incomplete"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedDOHFixture(t, func(w http.ResponseWriter, r *http.Request, q dnsmessage.Message) {
				m := dnsTestResponse(q)
				typ := q.Questions[0].Type
				switch mode {
				case "ipv6-only":
					if typ == dnsmessage.TypeA {
						m.Answers = nil
					}
				case "ipv4-only":
					if typ == dnsmessage.TypeAAAA {
						m.Answers = nil
					}
				case "both-empty":
					m.Answers = nil
				case "aaaa-error":
					if typ == dnsmessage.TypeAAAA {
						m.RCode = dnsmessage.RCodeServerFailure
						m.Answers = nil
					}
				case "private-a":
					if typ == dnsmessage.TypeA {
						m.Answers = append(m.Answers, dnsmessage.Resource{Header: m.Answers[0].Header, Body: &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}}})
					}
				case "private-aaaa":
					if typ == dnsmessage.TypeAAAA {
						m.Answers = append(m.Answers, dnsmessage.Resource{Header: m.Answers[0].Header, Body: &dnsmessage.AAAAResource{AAAA: netip.MustParseAddr("::ffff:10.0.0.1").As16()}})
					}
				case "referral":
					if typ == dnsmessage.TypeAAAA {
						m.Answers = nil
						m.Authorities = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: dnsTestName("example."), Type: dnsmessage.TypeNS, Class: dnsmessage.ClassINET}, Body: &dnsmessage.NSResource{NS: dnsTestName("ns.example.")}}}
					}
				case "cname-soa", "cname-incomplete":
					if typ == dnsmessage.TypeAAAA {
						m.Answers = []dnsmessage.Resource{dnsTestCNAME("asset.example.", "cdn.example.")}
						if mode == "cname-soa" {
							m.Authorities = []dnsmessage.Resource{dnsTestSOA("example.")}
						}
					}
				}
				body, _ := m.Pack()
				w.Header().Set("Content-Type", "application/dns-message")
				w.Write(body)
			}, false)
			r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "http://asset.example/data", DNSResolver: f.resolver}, f.options)
			if mode == "ipv6-only" || mode == "ipv4-only" || mode == "cname-soa" {
				if err != nil || r == nil {
					t.Fatalf("legal completed family rejected: %v", err)
				}
				return
			}
			reason := "dns_resolution"
			if strings.HasPrefix(mode, "private-") {
				reason = "network_policy"
			}
			d := requireResourceFailure(t, r, err, reason)
			if f.originHits.Load() != 0 || f.queries.Load() != 2 || d.BodyBytesRead != 0 || d.Policy.DNSResolver != f.resolver {
				t.Fatalf("origin dialed before complete/public snapshot: queries=%d hits=%d diag=%+v", f.queries.Load(), f.originHits.Load(), d)
			}
			if reason == "dns_resolution" {
				var dns *DNSResolutionError
				if !errors.As(err, &dns) || dns.Resolver != f.resolver {
					t.Fatalf("DNS provenance lost: %v", err)
				}
			}
		})
	}
}

func TestResourceDoHProtocolTLSAndByteLimits(t *testing.T) {
	for _, mode := range []string{"tls-ip-san", "redirect", "http-error", "wrong-content-type", "duplicate-content-type", "oversized", "truncated-dns", "rcode", "wrong-question", "garbage"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedDOHFixture(t, func(w http.ResponseWriter, r *http.Request, q dnsmessage.Message) {
				m := dnsTestResponse(q)
				w.Header().Set("Content-Type", "application/dns-message")
				switch mode {
				case "redirect":
					w.Header().Set("Location", "https://1.1.1.1/dns-query")
					w.WriteHeader(302)
					return
				case "http-error":
					w.WriteHeader(500)
					return
				case "wrong-content-type":
					w.Header().Set("Content-Type", "text/html")
				case "duplicate-content-type":
					w.Header().Add("Content-Type", "text/html")
				case "oversized":
					io.WriteString(w, strings.Repeat("x", publicDNSMessageLimit+1))
					return
				case "truncated-dns":
					m.Truncated = true
				case "rcode":
					m.RCode = dnsmessage.RCodeServerFailure
				case "wrong-question":
					m.Questions = []dnsmessage.Question{{Name: dnsTestName("other.example."), Type: q.Questions[0].Type, Class: dnsmessage.ClassINET}}
				}
				body, _ := m.Pack()
				if mode == "garbage" {
					body = append(body, 0)
				}
				w.Write(body)
			}, mode == "tls-ip-san")
			r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "http://asset.example/data", DNSResolver: f.resolver}, f.options)
			d := requireResourceFailure(t, r, err, "dns_resolution")
			var dns *DNSResolutionError
			if !errors.As(err, &dns) || dns.Resolver != f.resolver || d.BodyBytesRead != 0 || f.originHits.Load() != 0 {
				t.Fatalf("DoH failure lost source/origin isolation: diag=%+v err=%v", d, err)
			}
			if mode == "oversized" && d.Policy.NetworkBytes <= publicDNSMessageLimit {
				t.Fatalf("DoH bytes omitted from shared network accounting: %+v", d)
			}
			if mode == "tls-ip-san" && f.queries.Load() != 0 {
				t.Fatal("invalid IP certificate reached DNS HTTP handler")
			}
			if mode != "tls-ip-san" && f.queries.Load() != 1 {
				t.Fatalf("failed DoH family triggered fallback/retry: %d", f.queries.Load())
			}
		})
	}
}

func TestResourceDoHCancelBudgetAndPreDNSPrivateGate(t *testing.T) {
	t.Run("private-origin", func(t *testing.T) {
		f := newOwnedDOHFixture(t, nil, false)
		r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "http://127.0.0.1/", DNSResolver: f.resolver}, f.options)
		var failure *ResourceAcquisitionError
		if r != nil || !errors.As(err, &failure) || failure.Diagnostic.BodyBytesRead != 0 || f.queries.Load() != 0 || f.originHits.Load() != 0 {
			t.Fatalf("private origin used DoH: %v %v", r, err)
		}
	})
	t.Run("network-budget", func(t *testing.T) {
		f := newOwnedDOHFixture(t, nil, false)
		r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "http://asset.example/", DNSResolver: f.resolver, MaxNetworkBytes: 128}, f.options)
		d := requireResourceFailure(t, r, err, "network_policy")
		if d.BodyBytesRead != 0 || d.Policy.NetworkBytes <= 128 || f.originHits.Load() != 0 {
			t.Fatalf("DNS escaped shared byte limit: %+v", d)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		f := newOwnedDOHFixture(t, func(w http.ResponseWriter, r *http.Request, q dnsmessage.Message) { <-r.Context().Done() }, false)
		r, err := fetchResourceWithOptions(context.Background(), ResourceRequest{URL: "http://asset.example/", DNSResolver: f.resolver, Timeout: 60 * time.Millisecond}, f.options)
		d := requireResourceFailure(t, r, err, "timeout")
		if d.BodyBytesRead != 0 || f.originHits.Load() != 0 || d.Policy.DNSResolver != f.resolver {
			t.Fatalf("DoH timeout escaped context: %+v", d)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		entered := make(chan struct{})
		f := newOwnedDOHFixture(t, func(w http.ResponseWriter, r *http.Request, q dnsmessage.Message) {
			close(entered)
			<-r.Context().Done()
		}, false)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := fetchResourceWithOptions(ctx, ResourceRequest{URL: "http://asset.example/", DNSResolver: f.resolver}, f.options)
			done <- err
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("DoH request not entered")
		}
		cancel()
		select {
		case err := <-done:
			var failure *ResourceAcquisitionError
			if !errors.As(err, &failure) || failure.Diagnostic.Reason != "canceled" || failure.Diagnostic.BodyBytesRead != 0 || !failure.Diagnostic.Cleanup.NetworkClosed {
				t.Fatalf("DoH cancel lost cleanup/body facts: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("DoH ignored cancellation")
		}
	})
}

func TestPublicDNSSharedRequestBudget(t *testing.T) {
	f := newOwnedDOHFixture(t, nil, false)
	opts := f.options.network
	opts.DNSResolver = f.resolver
	opts.NetworkRoute = NetworkRouteSystem
	opts.Timeout = time.Second
	opts.MaxBytes = 1 << 20
	opts.MaxRequests = 2
	g, err := StartPublicNetworkGuard(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	response, err := publicClient(g).Get("http://asset.example/")
	if response != nil {
		response.Body.Close()
	}
	if g.PolicyError() == nil || f.originHits.Load() != 0 || g.Stats().Requests != 3 {
		t.Fatalf("DNS requests escaped job budget: stats=%+v policy=%v err=%v", g.Stats(), g.PolicyError(), err)
	}
}

func TestPublicDNSDocumentNavigationAttribution(t *testing.T) {
	f := newOwnedDOHFixture(t, func(w http.ResponseWriter, r *http.Request, q dnsmessage.Message) {
		m := dnsTestResponse(q)
		m.RCode = dnsmessage.RCodeServerFailure
		body, _ := m.Pack()
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(body)
	}, false)
	opts := f.options.network
	opts.DNSResolver = f.resolver
	opts.NetworkRoute = NetworkRouteSystem
	opts.Timeout = time.Second
	opts.MaxBytes = 1 << 20
	opts.MaxRequests = 20
	g, err := StartPublicNetworkGuard(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	started := time.Now()
	response, err := publicClient(g).Get("https://asset.example/")
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("failed TLS tunnel unexpectedly succeeded")
	}
	attributed := documentNavigationError(context.Background(), g, "https://asset.example/", started, err)
	var dns *DNSResolutionError
	if !errors.As(attributed, &dns) || dns.Resolver != f.resolver || documentFailureReason(attributed) != "dns_resolution" {
		t.Fatalf("main HTTPS DNS cause lost: %v", attributed)
	}
	if documentNavigationError(context.Background(), g, "https://asset.example/", started, nil) != nil {
		t.Fatal("optional DNS error poisoned successful main navigation")
	}
	unrelated := documentNavigationError(context.Background(), g, "https://other.example/", started, err)
	if errors.As(unrelated, &dns) {
		t.Fatal("other destination's DNS failure poisoned navigation")
	}
	stale := documentNavigationError(context.Background(), g, "https://asset.example/", time.Now(), err)
	if errors.As(stale, &dns) {
		t.Fatal("stale pre-navigation DNS failure reused")
	}
}
