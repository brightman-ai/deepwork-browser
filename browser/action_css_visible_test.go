package browser

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCSSBackendMappingRequiresUniqueCurrentObservedCapability(t *testing.T) {
	snapshot := newSnapshotEngine()
	engine := newActionEngine(snapshot)
	visible := &ElementRef{Ref: "@ocapture-r1", BackendNodeID: 11, Observed: true, VisibilityKnown: true, VisibleInViewport: true, Role: "textbox", Name: "Editor"}
	other := &ElementRef{Ref: "@ocapture-r2", BackendNodeID: 22, Observed: true, VisibilityKnown: true, VisibleInViewport: true, Role: "textbox", Name: "Other"}
	snapshot.refMeta = map[string]*ElementRef{visible.Ref: visible, "#editor": visible, other.Ref: other, "@ocapture-r3": {Ref: "@ocapture-r3", BackendNodeID: 33, Observed: true, VisibilityKnown: true, VisibleInViewport: false}, "@ocapture-r4": {Ref: "@ocapture-r4", BackendNodeID: 44, Observed: false, VisibilityKnown: true, VisibleInViewport: true}}
	if ref, err := engine.matchObservedCSSNodes("#editor", []int64{11}); err != nil || ref != visible.Ref {
		t.Fatalf("visible CSS mapping failed %s %v", ref, err)
	}
	if ref, err := engine.matchObservedCSSNodes("#editor", []int64{11, 11}); err != nil || ref != visible.Ref {
		t.Fatalf("duplicate identity index broke unique target %s %v", ref, err)
	}
	for _, ids := range [][]int64{nil, {33}, {44}, {55}, {11, 33}, {11, 44}} {
		if _, err := engine.matchObservedCSSNodes("textarea", ids); err == nil || !errors.Is(err, ErrRefNotFound) {
			t.Fatalf("unseen/hidden CSS identities accepted %v: %v", ids, err)
		}
	}
	if _, err := engine.matchObservedCSSNodes("textarea", []int64{11, 22}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("multiple observed CSS targets not ambiguous: %v", err)
	}
	snapshot.revokeObservationAuthority()
	if _, err := engine.matchObservedCSSNodes("#editor", []int64{11}); err == nil {
		t.Fatal("revoked CSS capability reused")
	}
}

// The coordinator runs this serial real browser regression; no tested runtime
// is mocked, and CSS only maps existing observed identities before native input.
func TestVisualCSSLocatorOnlyActsOnCurrentCapturedNodes(t *testing.T) {
	requireChromeForPool(t)
	page := `<!doctype html><meta charset="utf-8"><style>textarea{display:block;width:350px;height:90px;margin:15px}</style><textarea id="editor" data-testid="editor" class="entry" aria-label="Editor">original</textarea><textarea id="other" class="entry" aria-label="Other">other original</textarea><textarea id="hidden" style="display:none" aria-label="Hidden">hidden original</textarea>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	}))
	defer server.Close()
	setup, stop := context.WithTimeout(context.Background(), 150*time.Second)
	defer stop()
	lifetime, finishSetup := fixtureRuntimeLifetime(t, setup)
	core, err := NewBrowserCore(lifetime, fmt.Sprintf("css-visible-%d", time.Now().UnixNano()), WithMode(ModeHeadless), WithViewport(1280, 800))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close(context.Background())
	core.(ScenarioInteractionCapable).SetInteractionScenario(ScenarioAppTestExplore)
	core.SetPolicy(SessionPolicy{RemoteWrites: RemoteWriteDeny}, server.URL)
	// First navigation/capture are setup; they cannot spend the editor phase.
	if _, _, err := core.ExecuteAction(setup, Action{Verb: "navigate", URL: server.URL}, false, true, InteractionModeVisual); err != nil {
		t.Fatal(err)
	}
	initial, err := core.(SessionCore).SnapWithSessionMode(setup, 1)
	if err != nil {
		t.Fatal(err)
	}
	requireFixtureCapture(t, initial)
	impl := core.(*browserCoreImpl)
	requireRuntimeInputBinding(t, core, initial, BrowserInputKey(impl.wsURL, impl.chromePID))
	finishSetup()
	stop()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	observe := func() {
		t.Helper()
		snap, err := core.(SessionCore).SnapWithSessionMode(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		requireFixtureCapture(t, snap)
	}
	for _, locator := range []string{"css=#hidden", "css=.entry", "css=textarea"} {
		_, out, err := core.ExecuteAction(ctx, Action{Verb: "fill", Target: Target{Kind: "locator", Locator: locator}, Text: "must not replace"}, false, true, InteractionModeVisual)
		if err == nil || out.DispatchState != "not-dispatched" {
			t.Fatalf("hidden/ambiguous %s dispatched: %+v %v", locator, out, err)
		}
	}
	_, out, err := core.ExecuteAction(ctx, Action{Verb: "fill", Target: Target{Kind: "locator", Locator: "css=#editor"}, Text: "CSS native replacement 中文\n"}, false, true, InteractionModeVisual)
	if err != nil || out.DispatchState != "confirmed" {
		t.Fatalf("authorized visible CSS fill failed %+v %v", out, err)
	}
	observe()
	_, out, err = core.ExecuteAction(ctx, Action{Verb: "fill", Target: Target{Kind: "locator", Locator: `css=[data-testid="editor"]`}, Text: "recommended CSS replacement"}, false, true, InteractionModeVisual)
	if err != nil || out.DispatchState != "confirmed" {
		t.Fatalf("recommended CSS fill failed %+v %v", out, err)
	}
	var value string
	if err := core.EvalJS(ctx, `document.querySelector('#editor').value`, &value); err != nil {
		t.Fatal(err)
	}
	if value != "recommended CSS replacement" {
		t.Fatal("CSS typed fill did not preserve exact final value")
	}
	if err := core.EvalJS(ctx, `document.querySelector('#hidden').value`, &value); err != nil {
		t.Fatal(err)
	}
	if value != "hidden original" {
		t.Fatal("hidden CSS target mutated")
	}
}
