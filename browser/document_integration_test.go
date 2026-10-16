//go:build integration

package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This suite starts real owned headless Chrome instances, serially. Only DNS
// and the final numeric dial are routed to owned fixtures; the production proxy,
// CDP guards, Chrome networking, DOM reader and cleanup all execute unchanged.
// Run: go test -tags integration ./browser -run '^TestDocumentLive' -count=1 -v
func TestDocumentLive(t *testing.T) {
	var privateHits, proofHits, rebindLookups, fixtureHits atomic.Int64
	udpTrap, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udpTrap.Close()
	go func() {
		buffer := make([]byte, 2048)
		for {
			if _, _, err := udpTrap.ReadFrom(buffer); err != nil {
				return
			}
			privateHits.Add(1)
		}
	}()
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		privateHits.Add(1)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "PRIVATE TRAP")
	}))
	defer trap.Close()
	wsTrap := "ws" + strings.TrimPrefix(trap.URL, "http") + "/socket"
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixtureHits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		switch r.URL.Path {
		case "/proof":
			proofHits.Add(1)
			fmt.Fprint(w, "proof")
		case "/read-api":
			body, err := io.ReadAll(r.Body)
			if err != nil || r.Method != "POST" || string(body) != "query { article }" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, "API article ready")
		case "/post-page":
			fmt.Fprint(w, "<!doctype html><p id='article'>loading</p><script>fetch('/read-api',{method:'POST',body:'query { article }'}).then(r=>r.text()).then(s=>document.getElementById('article').textContent=s)</script><script src='/slow.js'></script>")
		case "/slow.js":
			w.Header().Set("Content-Type", "application/javascript")
			select {
			case <-r.Context().Done():
				return
			case <-time.After(800 * time.Millisecond):
			}
			fmt.Fprint(w, "window.gateReleased=true;")
		case "/hang.js":
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		case "/redirect":
			http.Redirect(w, r, trap.URL+"/redirect", http.StatusFound)
		case "/script":
			fmt.Fprintf(w, "<!doctype html><script src='%s/script'></script><p>script</p>", trap.URL)
		case "/websocket":
			fmt.Fprintf(w, "<!doctype html><script>window.ws=new WebSocket(%s)</script><script src='/slow.js'></script><p>websocket attempted</p>", strconv.Quote(wsTrap))
		case "/worker":
			worker := fmt.Sprintf("fetch('http://fixture.example/proof').then(()=>fetch(%s));", strconv.Quote(trap.URL+"/worker"))
			fmt.Fprintf(w, "<!doctype html><script>window.worker=new Worker(URL.createObjectURL(new Blob([%s],{type:'application/javascript'})))</script><script src='/slow.js'></script><p>worker</p>", strconv.Quote(worker))
		case "/worker-close":
			worker := "fetch('http://fixture.example/proof').then(()=>self.close())"
			fmt.Fprintf(w, "<!doctype html><script>window.worker=new Worker(URL.createObjectURL(new Blob([%s],{type:'application/javascript'})))</script><script src='/slow.js'></script><p>worker finished</p>", strconv.Quote(worker))
		case "/webrtc":
			fmt.Fprintf(w, "<!doctype html><script>window.pc=new RTCPeerConnection({iceServers:[{urls:'stun:%s'}]});pc.createDataChannel('probe');pc.createOffer().then(o=>pc.setLocalDescription(o))</script><script src='/slow.js'></script><p>webrtc attempted</p>", udpTrap.LocalAddr())
		case "/frame":
			fmt.Fprint(w, "<!doctype html><iframe src='http://frame.example/child'></iframe><script src='/slow.js'></script><p>frame</p>")
		case "/child":
			fmt.Fprintf(w, "<!doctype html><script>fetch('http://frame.example/proof').then(()=>fetch(%s))</script><p>child</p>", strconv.Quote(trap.URL+"/frame"))
		case "/rebind":
			fmt.Fprint(w, "<!doctype html><script>fetch('/after-rebind')</script><script src='http://fixture.example/slow.js'></script><p>rebind</p>")
		case "/images":
			fmt.Fprintf(w, "<!doctype html><img src='%s/image'><audio autoplay src='%s/audio'></audio><p>no media required</p>", trap.URL, trap.URL)
		case "/large":
			fmt.Fprint(w, "<!doctype html><p>"+strings.Repeat("界", 4096)+"</p>")
		case "/network-large":
			fmt.Fprint(w, "<!doctype html><p>"+strings.Repeat("x", 4<<20)+"</p>")
		case "/rss":
			w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
			fmt.Fprint(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>Fixture feed</title><link>http://fixture.example/</link><description>Feed source, not a browser viewer</description></channel></rss>`)
		case "/xml":
			w.Header().Set("Content-Type", "text/xml; charset=utf-8")
			fmt.Fprint(w, `<?xml version="1.0"?><record><title>This is XML, not necessarily a feed</title></record>`)
		case "/hang":
			fmt.Fprint(w, "<!doctype html><script src='/hang.js'></script><p>blocked</p>")
		default:
			fmt.Fprintf(w, `<!doctype html><html><head><link rel="canonical" href="%s/untrusted"></head><body><p id="result">before</p><script>document.getElementById('result').textContent='JS executed';Object.defineProperty(Element.prototype,'outerHTML',{get(){return 'POISONED'}});document.querySelector=()=>null;</script></body></html>`, trap.URL)
		}
	}))
	defer fixture.Close()
	publicIP := netip.MustParseAddr("93.184.215.14")
	options := PublicNetworkOptions{
		lookupIP: func(ctx context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "fixture.example", "frame.example":
				return []netip.Addr{publicIP}, nil
			case "rebind.example":
				if rebindLookups.Add(1) > 1 {
					return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
				}
				return []netip.Addr{publicIP}, nil
			case "private.example":
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			default:
				return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			}
		},
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address == publicIP.String()+":80" {
				return (&net.Dialer{}).DialContext(ctx, "tcp", strings.TrimPrefix(fixture.URL, "http://"))
			}
			// All other destinations still undergo production public validation.
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	profiles := func() map[string]bool {
		paths, err := filepath.Glob(filepath.Join(home, ".deepwork", "browser-cli", "document-*"))
		if err != nil {
			t.Fatal(err)
		}
		result := make(map[string]bool)
		for _, path := range paths {
			result[path] = true
		}
		return result
	}
	for _, tc := range []struct {
		name    string
		host    string
		path    string
		failure string
		proof   bool
		denied  bool
		noPage  bool
		html    int64
		network int64
		timeout time.Duration
	}{
		{name: "isolated-js-canonical", path: "/ok"},
		{name: "site-js-post-content", path: "/post-page"},
		{name: "redirect-private", path: "/redirect", failure: "any"},
		{name: "script-private", path: "/script", denied: true},
		{name: "websocket-loopback-bypass", path: "/websocket"},
		{name: "worker-private", path: "/worker", proof: true, denied: true},
		{name: "worker-normal-close", path: "/worker-close", proof: true},
		{name: "oopif-private", path: "/frame", proof: true, denied: true},
		{name: "webrtc-nonproxy-udp", path: "/webrtc"},
		{name: "main-private-dns", host: "private.example", path: "/unreachable", failure: "main document", noPage: true},
		{name: "dns-rebinding", host: "rebind.example", path: "/rebind", denied: true},
		{name: "images-media-denied", path: "/images"},
		{name: "html-byte-budget", path: "/large", html: 1024, failure: "HTML byte budget"},
		{name: "rss-viewer-not-source", path: "/rss", failure: "unsupported document content type"},
		{name: "xml-not-assumed-feed", path: "/xml", failure: "unsupported document content type"},
		{name: "network-byte-budget", path: "/network-large", network: 1 << 20, failure: "byte budget"},
		{name: "total-deadline", path: "/hang", timeout: 8 * time.Second, failure: "deadline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			beforeProfiles, beforeProof, beforeFixture, beforePrivate := profiles(), proofHits.Load(), fixtureHits.Load(), privateHits.Load()
			defer func() {
				if hits := privateHits.Load() - beforePrivate; hits != 0 {
					t.Errorf("PRIVATE NETWORK BYPASS: %d trap requests in this case", hits)
				}
				for path := range profiles() {
					if !beforeProfiles[path] {
						t.Errorf("owned profile leaked: %s", path)
					}
				}
			}()
			host := tc.host
			if host == "" {
				host = "fixture.example"
			}
			deadline := tc.timeout
			if deadline == 0 {
				deadline = 10 * time.Second
			}
			started := time.Now()
			doc, err := fetchDocumentWithNetwork(context.Background(), DocumentRequest{NetworkRoute: DocumentNetworkDirect, URL: "http://" + host + tc.path, Timeout: deadline, MaxHTMLBytes: tc.html, MaxNetworkBytes: tc.network}, options)
			t.Logf("elapsed=%s error=%v", time.Since(started), err)
			if !tc.noPage && fixtureHits.Load() <= beforeFixture {
				t.Fatalf("fixture never loaded; cannot classify this as a guarded request test: %v", err)
			}
			if tc.failure != "" {
				if err == nil || doc != nil || tc.failure != "any" && !strings.Contains(err.Error(), tc.failure) {
					t.Fatalf("expected %s failure, got document=%v err=%v", tc.failure, doc != nil, err)
				}
				if tc.failure == "unsupported document content type" {
					var unsupported *UnsupportedDocumentContentError
					if !errors.As(err, &unsupported) || !unsupported.Reroutable || !unsupported.Cleanup.BrowserClosed || !unsupported.Cleanup.ProfileRemoved || !unsupported.Cleanup.NetworkClosed || unsupported.Policy.Network != "page-requests-public-only" {
						t.Fatalf("unsupported format lost settled policy/cleanup: %v", err)
					}
				}
			} else {
				if err != nil || doc == nil {
					t.Fatalf("document failed: %v", err)
				}
				if !doc.Cleanup.BrowserClosed || !doc.Cleanup.ProfileRemoved || !doc.Cleanup.NetworkClosed || doc.Policy.Network != "page-requests-public-only" {
					t.Fatalf("invalid receipt %+v", doc)
				}
				if tc.denied && doc.Policy.BlockedRequests < 1 {
					t.Fatal("guarded request had no recorded denial")
				}
				if tc.name == "isolated-js-canonical" && (!strings.Contains(doc.Content, ">JS executed</p>") || strings.HasPrefix(doc.Content, "POISONED") || doc.DeclaredCanonicalURL != trap.URL+"/untrusted") {
					t.Fatalf("isolated DOM/canonical contract failed: %+v", doc)
				}
				if tc.name == "site-js-post-content" && !strings.Contains(doc.Content, ">API article ready</p>") {
					t.Fatal("normal website JavaScript POST query was broken")
				}
			}
			if tc.proof && proofHits.Load() <= beforeProof {
				t.Fatal("child never proved execution: zero hits does not establish a blocked attempt")
			}
			if tc.name == "dns-rebinding" && rebindLookups.Load() < 2 {
				t.Fatal("DNS rebind did not exercise a second resolution")
			}
			if tc.timeout > 0 && time.Since(started) > tc.timeout+5*time.Second {
				t.Fatal("read plus cleanup exceeded budgets")
			}
		})
	}
}
