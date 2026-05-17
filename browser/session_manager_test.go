package browser

import "testing"

func TestSelectAttachablePageTargetPrefersUserPageOriginMatch(t *testing.T) {
	targets := map[string]interface{}{
		{
			"type": "page"
			"url": ChromeInitialPageURL
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/blank-1"
		}
		{
			"type": "page"
			"url": "http://example.com/other"
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/other-1"
		}
		{
			"type": "page"
			"url": "http://127.0.0.1:8077/studio"
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/studio-1"
		}
	}

	got := SelectAttachablePageTarget(targets, "http://127.0.0.1:8077/ws/2", "")
	if got.ID != "studio-1" {
		t.Fatalf("SelectAttachablePageTarget id = %q, want studio-1", got.ID)
	}
	if got.URL != "http://127.0.0.1:8077/studio" {
		t.Fatalf("SelectAttachablePageTarget url = %q", got.URL)
	}
}

func TestSelectAttachablePageTargetSkipsChromeInitialPageWhenUserPageExists(t *testing.T) {
	targets := map[string]interface{}{
		{
			"type": "page"
			"url": ChromeInitialPageURL
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/blank-1"
		}
		{
			"type": "page"
			"url": "https://example.org/app"
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/app-1"
		}
	}

	got := SelectAttachablePageTarget(targets, "", "")
	if got.ID != "app-1" {
		t.Fatalf("SelectAttachablePageTarget id = %q, want app-1", got.ID)
	}
	if got.URL != "https://example.org/app" {
		t.Fatalf("SelectAttachablePageTarget url = %q", got.URL)
	}
}

func TestBrowserSessionKindDefaultsAreDomainNeutral(t *testing.T) {
	cases := struct {
		kind BrowserSessionKind
		wantOwner string
		wantMode BrowserMode
		wantIso string
	}{
		{SessionKindTask, SessionOwnerAgent, ModeHeadless, SessionIsolationEphemeral}
		{SessionKindInteractive, SessionOwnerHuman, ModeHeaded, SessionIsolationDedicated}
		{SessionKindService, SessionOwnerService, ModeHeaded, SessionIsolationDedicated}
		{SessionKindDebug, SessionOwnerHuman, ModeVisible, SessionIsolationDedicated}
		{SessionKindTest, SessionOwnerAgent, ModeHeadless, SessionIsolationEphemeral}
	}

	for _, tc := range cases {
		got := DefaultsForBrowserSessionKind(tc.kind)
		if got.Kind != tc.kind || got.Owner != tc.wantOwner || got.Mode != tc.wantMode || got.Isolation != tc.wantIso {
			t.Fatalf("DefaultsForBrowserSessionKind(%q)=%+v", tc.kind, got)
		}
	}
	if got := NormalizeBrowserSessionKind("portal", SessionKindTask); got != SessionKindTask {
		t.Fatalf("portal must not be a public kind, got %q", got)
	}
	if got := NormalizeBrowserSessionKind("webchat", SessionKindService); got != SessionKindService {
		t.Fatalf("webchat must not be a public kind, got %q", got)
	}
}

func TestNormalizeSessionInfoBackfillsBrowserSessionContract(t *testing.T) {
	info := &SessionInfo{
		SessionID: "s1"
		ChromePID: 1234
		ProfileDir: "/tmp/dw-browser-sessions/profile-a"
	}

	NormalizeSessionInfo(info)

	if info.BrowserSessionID != "browser-session-s1" {
		t.Fatalf("BrowserSessionID=%q", info.BrowserSessionID)
	}
	if info.SessionKind != SessionKindTask || info.Owner != SessionOwnerAgent || info.Mode != ModeHeadless {
		t.Fatalf("unexpected defaults: %+v", info)
	}
	if info.ProfileID != "profile-a" {
		t.Fatalf("ProfileID=%q", info.ProfileID)
	}
	if info.BrowserRunID == "" {
		t.Fatal("BrowserRunID not backfilled")
	}
}
