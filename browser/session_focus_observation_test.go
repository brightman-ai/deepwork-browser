package browser

import "testing"

func TestObservationCommitRevalidatesPriorNativeFocus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*SessionInfo, *Snapshot)
		want   bool
	}{
		{"same focused node and document", func(*SessionInfo, *Snapshot) {}, true},
		{"same document hash navigation", func(_ *SessionInfo, s *Snapshot) { s.URL += "#saved" }, true},
		{"focus moved", func(_ *SessionInfo, s *Snapshot) { s.Refs[0].HumanFocusVerified = false }, false},
		{"other node focused", func(_ *SessionInfo, s *Snapshot) { s.Refs[0].BackendNodeID = 74 }, false},
		{"document changed at same URL", func(_ *SessionInfo, s *Snapshot) { s.DocumentID++ }, false},
		{"runtime changed", func(i *SessionInfo, s *Snapshot) { i.BrowserRunID = "other-run"; s.RuntimeRunID = "other-run" }, false},
		{"target changed", func(i *SessionInfo, s *Snapshot) { i.TargetID = "other-target"; s.TargetID = "other-target" }, false},
		{"Human takeover", func(i *SessionInfo, _ *Snapshot) { i.AuthorityState = AuthorityHuman }, false},
		{"unknown dispatch", func(i *SessionInfo, _ *Snapshot) { i.LastActionOutcome = SessionActionOutcomeUnknown }, false},
		{"in progress dispatch", func(i *SessionInfo, _ *Snapshot) { i.LastActionOutcome = "in_progress" }, false},
		{"DOM focus has no prior native proof", func(i *SessionInfo, _ *Snapshot) { i.HumanFocusBackendNodeID = 0 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &SessionInfo{SessionID: "observe-focus-" + NewObservationID(), BrowserRunID: "run", TargetID: "target", PageURL: "http://localhost/", SnapEpoch: 5, HumanFocusEpoch: 4, HumanFocusBackendNodeID: 73, HumanFocusPageURL: "http://localhost/", ObservationDocumentID: 9, ObservationRunID: "run", ObservationTargetID: "target", AuthorityState: AuthorityAgentic, LastActionOutcome: "confirmed"}
			snap := &Snapshot{ObservationID: "fresh", DocumentID: 9, RuntimeRunID: "run", TargetID: "target", URL: info.PageURL, Refs: []ElementRef{{Ref: "@ofresh-r1", BackendNodeID: 73, HumanFocusVerified: true}}}
			tc.mutate(info, snap)
			// The production Bind→Apply→Save→Load order must not overwrite old document
			// identity before deciding whether the existing native proof survives.
			if err := BindSessionObservation(info, snap, "fresh", "fresh.json"); err != nil {
				t.Fatal(err)
			}
			if err := SaveObservedSession(info, snap, SessionRefsFromSnapshot(snap, true)); err != nil {
				t.Fatal(err)
			}
			defer DeleteSession(info.SessionID)
			loaded, err := LoadSession(info.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if got := loaded.HumanFocusBackendNodeID > 0; got != tc.want {
				t.Fatalf("native proof survived=%v want=%v: %+v", got, tc.want, loaded)
			}
			if tc.want && (loaded.HumanFocusEpoch != 5 || loaded.HumanFocusPageURL != snap.URL) {
				t.Fatalf("revalidated native proof not advanced with capture: %+v", loaded)
			}
		})
	}
}

func TestApplyObservedSessionWithoutBindingRevalidatesNativeFocus(t *testing.T) {
	info := &SessionInfo{BrowserRunID: "run", TargetID: "target", SnapEpoch: 2, HumanFocusEpoch: 1, HumanFocusBackendNodeID: 73, ObservationDocumentID: 9, ObservationRunID: "run", ObservationTargetID: "target", AuthorityState: AuthorityAgentic}
	snap := &Snapshot{RuntimeRunID: "run", TargetID: "target", DocumentID: 9, URL: "http://localhost/#new", Refs: []ElementRef{{BackendNodeID: 73, HumanFocusVerified: true}}}
	if err := ApplyObservedSession(info, snap, nil); err != nil {
		t.Fatal(err)
	}
	if info.HumanFocusEpoch != 2 || info.HumanFocusPageURL != snap.URL {
		t.Fatalf("adapter observation lost revalidated focus: %+v", info)
	}
}
