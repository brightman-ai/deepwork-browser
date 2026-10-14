package browser

import (
	"errors"
	"testing"
)

func TestObservationTokensNeverAliasAcrossObservations(t *testing.T) {
	info := &SessionInfo{BrowserRunID: "run-a", TargetID: "target-a", ViewportW: 1280, ViewportH: 720}
	info.PageURL = "http://localhost"
	first := &Snapshot{ObservationID: "one", URL: "http://localhost", Refs: []ElementRef{{Ref: "@oone-r1", BackendNodeID: 1}}}
	if err := BindSessionObservation(info, first, "wrong", "wrong.json"); err == nil {
		t.Fatal("mismatched capture id accepted")
	}
	invalid := *first
	invalid.Refs = []ElementRef{{Ref: "@r1"}}
	if err := BindSessionObservation(info, &invalid, "one", "one.json"); err == nil {
		t.Fatal("unscoped ref accepted")
	}
	if err := BindSessionObservation(info, first, "one", "one.json"); err != nil {
		t.Fatal(err)
	}
	second := &Snapshot{ObservationID: "two", URL: first.URL, Refs: []ElementRef{{Ref: "@otwo-r1", BackendNodeID: 2}}}
	if err := BindSessionObservation(info, second, "two", "two.json"); err != nil {
		t.Fatal(err)
	}
	if first.Refs[0].Ref == second.Refs[0].Ref {
		t.Fatal("distinct observations aliased their capability")
	}
	if err := BindSessionObservation(info, second, "two", "two.json"); err == nil {
		t.Fatal("reused observation accepted")
	}
	if err := ValidateSessionObservation(info); err != nil {
		t.Fatal(err)
	}
	revoked := *info
	InvalidateSessionObservation(&revoked)
	if ValidateSessionObservation(&revoked) == nil {
		t.Fatal("revoked screenshot geometry retained authority")
	}
	for _, mutate := range []func(*SessionInfo){func(s *SessionInfo) { s.BrowserRunID = "run-b" }, func(s *SessionInfo) { s.TargetID = "target-b" }, func(s *SessionInfo) { s.ViewportW = 880 }} {
		changed := *info
		mutate(&changed)
		if ValidateSessionObservation(&changed) == nil {
			t.Fatal("changed environment retained authority")
		}
	}
}

func TestSessionAuthorityTakeoverRejectsAndYieldRestoresOwner(t *testing.T) {
	if SessionAllowsAIInput(&SessionInfo{AuthorityState: "unknown"}) == nil {
		t.Fatal("unknown authority state accepted AI input")
	}

	for _, owner := range []string{SessionOwnerAgent, SessionOwnerService} {
		info := &SessionInfo{Owner: owner, AuthorityState: AuthorityAgentic, Refs: []SessionRef{{Ref: "@oone-r1"}}, HumanFocusBackendNodeID: 42}
		if err := SetSessionAuthority(info, AuthorityHuman); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(SessionAllowsAIInput(info), ErrTakeoverActive) {
			t.Fatal("takeover did not reject AI")
		}
		if len(info.Refs) != 0 || info.HumanFocusBackendNodeID != 0 {
			t.Fatal("takeover retained old observation capability")
		}
		if err := SetSessionAuthority(info, AuthorityHuman); err != nil {
			t.Fatal(err)
		}
		if err := SetSessionAuthority(info, AuthorityAgentic); err != nil {
			t.Fatal(err)
		}
		if info.Owner != owner || SessionAllowsAIInput(info) != nil {
			t.Fatalf("yield failed to restore %s: %+v", owner, info)
		}
	}
}

func TestSessionEvidenceAndObservationRoundTrip(t *testing.T) {
	id := "v2-roundtrip-" + NewBrowserRunID("test", 1)
	info := &SessionInfo{SessionID: id, Owner: SessionOwnerAgent, AuthorityState: AuthorityAgentic, EvidenceRoot: "/tmp/task-evidence", BrowserRunID: "run-a", TargetID: "target-a", ViewportW: 880, ViewportH: 720, SnapEpoch: 1}
	defer DeleteSession(id)
	snap := &Snapshot{ObservationID: "unique", URL: "http://localhost", Refs: []ElementRef{{Ref: "@ounique-r1", BackendNodeID: 1, VisibleInViewport: true, VisibilityKnown: true}}}
	if err := BindSessionObservation(info, snap, "unique", "/tmp/task-evidence/unique.json"); err != nil {
		t.Fatal(err)
	}
	if err := SaveObservedSession(info, snap, SessionRefsFromSnapshot(snap, true)); err != nil {
		t.Fatal(err)
	}
	restored, err := LoadSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if restored.EvidenceRoot != info.EvidenceRoot || restored.ObservationID != info.ObservationID || restored.ViewportW != 880 || len(restored.Refs) != 1 {
		t.Fatalf("session environment lost: %+v", restored)
	}
	restored.ViewportW = 1280
	NormalizeSessionInfo(restored)
	if len(restored.Refs) != 0 {
		t.Fatal("stale geometry survived normalization")
	}
}

func TestRevokedGeometryPreservesPhysicalFocusAcrossPersistence(t *testing.T) {
	info := &SessionInfo{SessionID: "focus-persistence-" + NewBrowserRunID("test", 1), BrowserRunID: "run-focus", TargetID: "target-focus", PageURL: "http://localhost/", SnapEpoch: 4, HumanFocusEpoch: 4, HumanFocusBackendNodeID: 73, HumanFocusPageURL: "http://localhost/", ObservationID: "capture-focus", ObservationValid: true, ObservationRunID: "run-focus", ObservationTargetID: "target-focus", ObservationPageURL: "http://localhost/", ObservationViewportW: 1280, ObservationViewportH: 720, ViewportW: 1280, ViewportH: 720}
	RevokeSessionObservationGeometry(info)
	defer DeleteSession(info.SessionID)
	if err := SaveSession(info); err != nil {
		t.Fatal(err)
	}
	restored, err := LoadSession(info.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	info = restored
	if info.HumanFocusBackendNodeID != 73 {
		t.Fatal("terminal geometry revocation erased physical focus on persistence normalization")
	}
	if info.ObservationValid || len(info.Refs) != 0 {
		t.Fatal("geometry capability survived terminal input")
	}
	InvalidateSessionObservation(info)
	NormalizeSessionInfo(info)
	if info.HumanFocusBackendNodeID != 0 {
		t.Fatal("full navigation/intervention invalidation retained physical focus")
	}
}
