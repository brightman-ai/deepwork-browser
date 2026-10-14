package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// This contract is exercised on actual constructor-produced cores by the
// serialized browser regressions, rather than a mock runtime/struct fixture.
func requireRuntimeInputBinding(t *testing.T, core BrowserCore, snap *Snapshot, want string) {
	t.Helper()
	impl := core.(*browserCoreImpl)
	digest := sha256.Sum256([]byte(want))
	wantRunID := hex.EncodeToString(digest[:])
	if want == "" || impl.runtimeInputKey != want || impl.actEngine.browserInputKey != want || impl.snapEngine.runtimeRunID != wantRunID || snap.RuntimeRunID != wantRunID {
		t.Fatalf("constructor failed shared input/capture identity binding: key_present=%t engine_match=%t runtime=%q capture=%q", want != "", impl.actEngine.browserInputKey == want, impl.snapEngine.runtimeRunID, snap.RuntimeRunID)
	}
	if impl.targetTracker != nil && impl.targetTracker.BrowserInputKey() != want {
		t.Fatal("tracker input identity differs from AI/Human core identity")
	}
}

// The runtime owns a cancellable lifetime. Setup remains bounded separately:
// its deadline tears down an incomplete runtime but cannot kill later input.
func fixtureRuntimeLifetime(t *testing.T, setup context.Context) (context.Context, func()) {
	t.Helper()
	lifetime, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stopSetupWatchdog := context.AfterFunc(setup, cancel)
	t.Cleanup(func() { stopSetupWatchdog() })
	return lifetime, func() {
		t.Helper()
		if !stopSetupWatchdog() || setup.Err() != nil || lifetime.Err() != nil {
			t.Fatalf("fixture setup deadline/lifetime expired: setup=%v lifetime=%v", setup.Err(), lifetime.Err())
		}
	}
}

func requireFixtureCapture(t *testing.T, snap *Snapshot) {
	t.Helper()
	if snap == nil || snap.ObservationID == "" || snap.DocumentID == 0 || snap.Progressive || len(snap.Refs) == 0 {
		t.Fatalf("fixture setup has no usable captured refs: %+v", snap)
	}
	for _, ref := range snap.Refs {
		if ref.Observed && ref.VisibilityKnown && ref.VisibleInViewport && ref.BackendNodeID != 0 {
			return
		}
	}
	t.Fatal("fixture setup has no observed visible backend identity")
}

// These helpers build typed fixtures and call the real runtime. They perform no
// string parsing, state preparation, simulation, or fallback.
func fixtureRef(ref string) Target         { return Target{Kind: "ref", Ref: ref} }
func fixtureLocator(locator string) Target { return Target{Kind: "locator", Locator: locator} }
func fixtureViewport(x, y float64) Target {
	return Target{Kind: "point", Point: &Point{X: x, Y: y, Unit: "css-px", Frame: "viewport", ObservationID: "fixture"}}
}
func fixtureRelative(locator string, x, y float64) Target {
	return Target{Kind: "locator", Locator: locator, Point: &Point{X: x, Y: y, Unit: "percent", Frame: "target", ObservationID: "fixture"}}
}
func fixtureEnd(x, y float64) *Point {
	return &Point{X: x, Y: y, Unit: "percent", Frame: "target", ObservationID: "fixture"}
}
func fixtureChecked(value bool) *bool { return &value }
func executeFixtureAction(ctx context.Context, core BrowserCore, request Action, observe, session bool) (*Snapshot, error) {

	if impl, ok := core.(*browserCoreImpl); ok && impl.snapEngine.authority != nil {
		id := impl.snapEngine.authority.ID
		if request.Target.Point != nil && request.Target.Point.ObservationID == "fixture" {
			point := *request.Target.Point
			point.ObservationID = id
			request.Target.Point = &point
		}
		if request.End != nil && request.End.ObservationID == "fixture" {
			end := *request.End
			end.ObservationID = id
			request.End = &end
		}
	}
	snap, _, err := core.(TypedActionCapable).ExecuteAction(ctx, request, observe, session, InteractionModeVisual)
	return snap, err
}
func executeFixtureEngine(ctx context.Context, engine *actionEngine, request Action, observe, session bool, mode InteractionMode) (*Snapshot, error) {
	engine.beginActionFidelity()
	parsed, err := request.executionRequest()
	if err != nil {
		return nil, engine.failedBeforeDispatch(err)
	}
	return engine.executeRequestWithInteractionMode(ctx, parsed, observe, session, mode)
}
