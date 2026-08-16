package browser

import "testing"

func TestSessionRefsFromSnapshotIsTheCompletePersistenceBoundary(t *testing.T) {
	snap := &Snapshot{
		SeeToClick: true,
		Refs: []ElementRef{
			{
				Ref:           "@r1",
				BackendNodeID: 91,
				Locator: NodeLocator{
					Engine:    EngineSafari,
					AXPath:    "0.2.1",
					StableKey: "button:save",
				},
				AXPath:            "0.2.1",
				Role:              "button",
				NameFull:          "Save",
				TestID:            "save",
				Placeholder:       "unused",
				BBox:              Rect{X: 1, Y: 2, Width: 30, Height: 40},
				VisibilityKnown:   true,
				VisibleInViewport: true,
			},
			{Ref: "@r2", BackendNodeID: 92, VisibilityKnown: true, VisibleInViewport: false},
			{Ref: "@r3", BackendNodeID: 93, VisibilityKnown: false, VisibleInViewport: true},
		},
	}

	refs := SessionRefsFromSnapshot(snap, true)
	if len(refs) != 1 {
		t.Fatalf("refs=%+v, want only the visible ref", refs)
	}
	got := refs[0]
	if got.Ref != "@r1" || got.BackendNodeID != 91 || got.Role != "button" || got.Name != "Save" ||
		got.TestID != "save" || got.Placeholder != "unused" || got.Locator.AXPath != "0.2.1" ||
		got.AXPath != "0.2.1" || got.StableKey != "button:save" || got.BBox == nil ||
		!got.Visible || !got.Observed {
		t.Fatalf("incomplete SessionRef mapping: %+v", got)
	}

	unobserved := SessionRefsFromSnapshot(snap, false)
	if len(unobserved) != 1 || unobserved[0].Observed {
		t.Fatalf("observed parameter was not honored: %+v", unobserved)
	}
}

func TestSelectAttachablePageTargetPrefersUserPageOriginMatch(t *testing.T) {
	targets := []map[string]interface{}{
		{
			"type":                 "page",
			"url":                  ChromeInitialPageURL,
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/blank-1",
		},
		{
			"type":                 "page",
			"url":                  "http://example.com/other",
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/other-1",
		},
		{
			"type":                 "page",
			"url":                  "http://127.0.0.1:8077/studio",
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/studio-1",
		},
	}

	got := SelectAttachablePageTarget(targets, "http://127.0.0.1:8077/ws/2", "")
	if got.ID != "studio-1" {
		t.Fatalf("SelectAttachablePageTarget() id = %q, want studio-1", got.ID)
	}
	if got.URL != "http://127.0.0.1:8077/studio" {
		t.Fatalf("SelectAttachablePageTarget() url = %q", got.URL)
	}
}

func TestSelectAttachablePageTargetSkipsChromeInitialPageWhenUserPageExists(t *testing.T) {
	targets := []map[string]interface{}{
		{
			"type":                 "page",
			"url":                  ChromeInitialPageURL,
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/blank-1",
		},
		{
			"type":                 "page",
			"url":                  "https://example.org/app",
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/app-1",
		},
	}

	got := SelectAttachablePageTarget(targets, "", "")
	if got.ID != "app-1" {
		t.Fatalf("SelectAttachablePageTarget() id = %q, want app-1", got.ID)
	}
	if got.URL != "https://example.org/app" {
		t.Fatalf("SelectAttachablePageTarget() url = %q", got.URL)
	}
}

func TestBrowserSessionKindDefaultsAreDomainNeutral(t *testing.T) {
	cases := []struct {
		kind      BrowserSessionKind
		wantOwner string
		wantMode  BrowserMode
		wantIso   string
	}{
		{SessionKindTask, SessionOwnerAgent, ModeHeadless, SessionIsolationEphemeral},
		{SessionKindInteractive, SessionOwnerHuman, ModeHeaded, SessionIsolationDedicated},
		{SessionKindService, SessionOwnerService, ModeHeaded, SessionIsolationDedicated},
		{SessionKindDebug, SessionOwnerHuman, ModeVisible, SessionIsolationDedicated},
		{SessionKindTest, SessionOwnerAgent, ModeHeadless, SessionIsolationEphemeral},
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
		SessionID:  "s1",
		ChromePID:  1234,
		ProfileDir: "/tmp/dw-browser-sessions/profile-a",
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

func TestNormalizeSessionInfoRevokesRefsAcrossUnknownActionBoundary(t *testing.T) {
	for _, outcome := range []string{SessionActionOutcomeInProgress, SessionActionOutcomeUnknown} {
		info := &SessionInfo{
			SessionID:         "action-fence-" + outcome,
			LastActionOutcome: outcome,
			Refs: []SessionRef{{
				Ref:      "@r1",
				Visible:  true,
				Observed: true,
			}},
		}
		NormalizeSessionInfo(info)
		if len(info.Refs) != 0 {
			t.Fatalf("outcome=%q retained stale refs: %+v", outcome, info.Refs)
		}
	}

	confirmed := &SessionInfo{
		SessionID:         "action-fence-confirmed",
		LastActionOutcome: SessionActionOutcomeConfirmed,
		Refs:              []SessionRef{{Ref: "@r1", Visible: true, Observed: true}},
	}
	NormalizeSessionInfo(confirmed)
	if len(confirmed.Refs) != 1 {
		t.Fatalf("confirmed outcome revoked valid refs: %+v", confirmed.Refs)
	}
}

func TestSaveObservedSessionAtomicallyReconcilesFreshRefs(t *testing.T) {
	sessionID := "observe-reconcile-" + t.Name()
	t.Cleanup(func() { _ = DeleteSession(sessionID) })

	info := &SessionInfo{
		SessionID:         sessionID,
		PageURL:           "https://before.example/",
		SnapEpoch:         6,
		LastActionOutcome: SessionActionOutcomeUnknown,
		Refs:              []SessionRef{{Ref: "@r-old", Observed: true}},
	}
	NormalizeSessionInfo(info)
	if len(info.Refs) != 0 {
		t.Fatalf("unknown generation retained stale refs: %+v", info.Refs)
	}

	info.SnapEpoch++
	fresh := []SessionRef{{Ref: "@r1", BackendNodeID: 42, Visible: true, Observed: true}}
	if err := SaveObservedSession(info, &Snapshot{URL: "https://after.example/"}, fresh); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadSession(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SnapEpoch != 7 || loaded.PageURL != "https://after.example/" ||
		loaded.LastActionOutcome != SessionActionOutcomeReconciled ||
		len(loaded.Refs) != 1 || loaded.Refs[0].Ref != "@r1" {
		t.Fatalf("observation fields did not commit as one generation: %+v", loaded)
	}
	if fresh[0].Ref = "@mutated"; info.Refs[0].Ref == fresh[0].Ref {
		t.Fatal("SaveObservedSession retained caller-owned refs backing storage")
	}
}
