//go:build integration

package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSessionAttachClose_DoesNotCloseExistingPageTarget verifies that
// NewBrowserCoreFromSession only detaches from an existing target on Close,
// instead of closing the page that the session mode is reusing.
func TestSessionAttachClose_DoesNotCloseExistingPageTarget(t *testing.T) {
	requireChrome(t)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Session Attach</title></head><body><h1>session-attach-ok</h1></body></html>`))
	}))
	defer ts.Close()

	profileID := "test-session-attach-" + itoa(int(time.Now().UnixNano()%100000))
	launcher := NewChromeLauncher()
	wsURL, pid, err := launcher.Launch(ctx, profileID, true)
	if err != nil {
		t.Skipf("Environment Gate: Chrome launch failed (%v) — skip L2 test", err)
		return
	}
	defer func() { _ = launcher.Kill(pid) }()
	defer func() {
		cacheDir := os.Getenv("XDG_CACHE_HOME")
		if cacheDir == "" {
			cacheDir = "/tmp"
		}
		_ = os.RemoveAll(filepath.Join(cacheDir, "dw-browser-data", profileID))
	}()

	debugURL, err := url.Parse(strings.Replace(wsURL, "ws://", "http://", 1))
	if err != nil {
		t.Fatalf("parse wsURL: %v", err)
	}
	debugPort := 0
	if _, err := fmt.Sscanf(debugURL.Host, "127.0.0.1:%d", &debugPort); err != nil {
		t.Fatalf("extract debug port from %q: %v", debugURL.Host, err)
	}

	impl, err := NewBrowserCoreFromSession(ctx, wsURL, "", DefaultPresetID())
	if err != nil {
		t.Fatalf("NewBrowserCoreFromSession() failed: %v", err)
	}

	snap, err := impl.Navigate(ctx, ts.URL)
	if err != nil {
		t.Fatalf("Navigate() failed: %v", err)
	}
	if snap == nil || snap.URL == "" {
		t.Fatalf("Navigate() returned empty snapshot: %#v", snap)
	}

	targetsBeforeClose, err := FetchChromeTargets(debugPort)
	if err != nil {
		t.Fatalf("FetchChromeTargets(before close) failed: %v", err)
	}
	if !hasNonBlankTargetForURL(targetsBeforeClose, ts.URL) {
		t.Fatalf("expected target list before close to contain %q, got %#v", ts.URL, targetsBeforeClose)
	}

	if err := impl.Close(context.Background()); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	targetsAfterClose, err := FetchChromeTargets(debugPort)
	if err != nil {
		t.Fatalf("FetchChromeTargets(after close) failed: %v", err)
	}
	if !hasNonBlankTargetForURL(targetsAfterClose, ts.URL) {
		t.Fatalf("session Close() should not close the existing page target; targets after close = %#v", targetsAfterClose)
	}
}

func hasNonBlankTargetForURL(targets []map[string]interface{}, wantURL string) bool {
	want := strings.TrimRight(wantURL, "/")
	for _, t := range targets {
		tType, _ := t["type"].(string)
		tURL, _ := t["url"].(string)
		if tType == "page" && strings.TrimRight(tURL, "/") == want && tURL != "" && tURL != "about:blank" {
			return true
		}
	}
	return false
}
