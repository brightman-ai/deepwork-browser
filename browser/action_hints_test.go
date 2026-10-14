package browser

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAllowedActionsRespectCapturedStateAndDefinition(t *testing.T) {
	base := ElementRef{Observed: true, VisibilityKnown: true, VisibleInViewport: true, Interactable: true, ControlStateKnown: true, Editable: true, SelectionSupported: true, Focusable: true}
	has := func(actions []string, kind string) bool {
		for _, action := range actions {
			if action == kind {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct {
		name     string
		change   func(*ElementRef)
		wantText bool
	}{{"editable", func(*ElementRef) {}, true}, {"readonly", func(r *ElementRef) { r.ReadOnly = true }, false}, {"disabled", func(r *ElementRef) { r.Disabled = true }, false}, {"sensitive", func(r *ElementRef) { r.Sensitive = true }, false}, {"temporal", func(r *ElementRef) { r.SelectionSupported = false }, false}, {"unknown", func(r *ElementRef) { r.ControlStateKnown = false }, false}} {
		ref := base
		tc.change(&ref)
		actions := AllowedActionsForElement(ref, ScenarioAppTestExplore)
		if has(actions, "fill") != tc.wantText || has(actions, "type") != tc.wantText {
			t.Errorf("%s text hints %v", tc.name, actions)
		}
		for _, kind := range actions {
			if _, known := LookupActionDefinition(kind); !known {
				t.Errorf("hint %q absent canonical definition", kind)
			}
		}
	}
	if actions := AllowedActionsForElement(base, ""); len(actions) != 0 {
		t.Errorf("unknown scene claimed actions %v", actions)
	}
	unobserved := base
	unobserved.Observed = false
	if len(AllowedActionsForElement(unobserved, ScenarioAppTestExplore)) != 0 {
		t.Fatal("unobserved target claimed actions")
	}
}

func TestProviderAuthorityGuardCoversPageActions(t *testing.T) {
	for _, outcome := range []string{"human", SessionActionOutcomeUnknown, SessionActionOutcomeInProgress} {
		for _, request := range []Action{{Verb: "navigate", URL: "https://example.com"}, {Verb: "reload"}, {Verb: "back"}, {Verb: "forward"}, {Verb: "zoom", Value: "2"}, {Verb: "keyboard", Value: "show"}} {
			engine := newActionEngine(newSnapshotEngine())
			impl := &browserCoreImpl{browserCtx: context.Background(), snapEngine: engine.snapEngine, actEngine: engine, takeoverCtrl: newTakeoverController(context.Background())}
			impl.SetInteractionScenario(ScenarioAppTestBaseline)
			impl.SetPolicy(SessionPolicy{RemoteWrites: RemoteWriteAllow}, "http://127.0.0.1/")
			info := &SessionInfo{AuthorityState: AuthorityAgentic, LastActionOutcome: outcome, LastActionOperationID: "prior"}
			if outcome == "human" {
				info.AuthorityState = AuthorityHuman
				info.LastActionOutcome = SessionActionOutcomeConfirmed
			}
			calls := 0
			impl.SetSessionAuthorityProvider(func(context.Context) (*SessionInfo, error) { calls++; return info, nil })
			_, result, err := impl.ExecuteAction(context.Background(), request, false, true, InteractionModeVisual)
			if err == nil || result.DispatchState != "not-dispatched" || result.Phase != "authority" || calls != 1 {
				t.Fatalf("%s/%s bypassed source guard: %+v calls=%d err=%v", request.Verb, outcome, result, calls, err)
			}
			if outcome == "human" && !errors.Is(err, ErrTakeoverActive) {
				t.Errorf("Human error lost %v", err)
			}
			if outcome != "human" && !strings.Contains(err.Error(), "unknown") {
				t.Errorf("recovery error lost %v", err)
			}
		}
	}
}

func TestOperationTokenIsConsumedByOneExecuteAction(t *testing.T) {
	engine := newActionEngine(newSnapshotEngine())
	impl := &browserCoreImpl{browserCtx: context.Background(), snapEngine: engine.snapEngine, actEngine: engine, takeoverCtrl: newTakeoverController(context.Background())}
	impl.SetInteractionScenario(ScenarioAppTestBaseline)
	info := &SessionInfo{AuthorityState: AuthorityAgentic, LastActionOutcome: SessionActionOutcomeInProgress, LastActionOperationID: "current-operation"}
	impl.SetSessionAuthorityProvider(func(context.Context) (*SessionInfo, error) { return info, nil })
	impl.SetSessionOperationID("current-operation")
	_, first, err := impl.ExecuteAction(context.Background(), Action{Verb: "reload"}, false, true, InteractionModeVisual)
	if err == nil || first.Phase == "authority" {
		t.Fatalf("current token did not reach driver boundary: %+v %v", first, err)
	}
	_, second, err := impl.ExecuteAction(context.Background(), Action{Verb: "reload"}, false, true, InteractionModeVisual)
	if err == nil || second.Phase != "authority" || second.DispatchState != "not-dispatched" {
		t.Fatalf("stale token reused after return: %+v %v", second, err)
	}
	if info.LastActionOperationID != "current-operation" {
		t.Fatal("persisted trace operation erased")
	}
}

func TestViewportDoubleAndDragAlwaysCheckExactCaptureIdentity(t *testing.T) {
	for _, request := range []Action{{Verb: "click", Count: 2, Target: Target{Kind: "point", Point: &Point{X: 0, Y: 0, Unit: "css-px", Frame: "viewport", ObservationID: "current"}}}, {Verb: "drag", Target: Target{Kind: "point", Point: &Point{X: 0, Y: 0, Unit: "css-px", Frame: "viewport", ObservationID: "current"}}, End: &Point{X: 2, Y: 2, Unit: "css-px", Frame: "viewport", ObservationID: "current"}}} {
		lowered, err := request.executionRequest()
		if err != nil {
			t.Fatal(err)
		}
		if !actionRequiresObservation(lowered, false, false) || !actionRequiresObservation(lowered, true, false) {
			t.Fatalf("%s skipped capture guard", lowered.Op)
		}
		engine := newActionEngine(newSnapshotEngine())
		impl := &browserCoreImpl{browserCtx: context.Background(), snapEngine: engine.snapEngine, actEngine: engine, takeoverCtrl: newTakeoverController(context.Background())}
		impl.SetSessionAuthorityProvider(func(context.Context) (*SessionInfo, error) {
			return &SessionInfo{AuthorityState: AuthorityAgentic, ObservationID: "current", ObservationValid: true}, nil
		})
		// Old identities must stop before document/viewport probes or any driver input.
		_, _, _, err = engine.enterInputDispatchWindowForRequest(context.Background(), true, "old", false)
		if err == nil || !strings.Contains(err.Error(), "stale observation_id") {
			t.Fatalf("%s old capture accepted: %v", lowered.Op, err)
		}
		_, _, _, err = engine.enterInputDispatchWindowForRequest(context.Background(), true, "current", false)
		if err == nil || strings.Contains(err.Error(), "stale observation_id") {
			t.Fatalf("%s current ID did not pass identity comparison: %v", lowered.Op, err)
		}
	}
}
