package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenBeforeObserveClickIsRejected(t *testing.T) {
	snap := &Snapshot{
		SeeToClick: true,
		Refs: []ElementRef{{
			Ref:               "@r1",
			BackendNodeID:     17,
			Role:              "button",
			NameFull:          "Continue",
			BBox:              Rect{X: 10, Y: 10, Width: 80, Height: 30},
			VisibilityKnown:   true,
			VisibleInViewport: true,
		}},
	}

	snapEngine := newSnapshotEngine()
	actEngine := newActionEngine(snapEngine)
	impl := &browserCoreImpl{snapEngine: snapEngine, actEngine: actEngine}
	impl.SetInteractionScenario(ScenarioAppTestExplore)
	impl.RestoreRefsFromSession(SessionRefsFromSnapshot(snap, false))

	err := actEngine.executeSeeToClick(context.Background(), "@r1")
	if err == nil {
		t.Fatal("click before observe unexpectedly succeeded")
	}
	if !errors.Is(err, ErrRefNotFound) || !strings.Contains(err.Error(), "not in the visible set from the most recent observe") {
		t.Fatalf("click error=%v, want explicit missing-observation rejection", err)
	}
}

func TestSeeToClickVisibleSetFiltersOffscreenAndHalfVisible(t *testing.T) {
	const viewportW, viewportH = 100.0, 100.0
	tests := []struct {
		name string
		box  Rect
		want bool
	}{
		{name: "fully visible", box: Rect{X: 10, Y: 10, Width: 20, Height: 20}, want: true},
		{name: "offscreen", box: Rect{X: 10, Y: 110, Width: 20, Height: 20}, want: false},
		{name: "exactly half is excluded", box: Rect{X: -50, Y: 0, Width: 100, Height: 100}, want: false},
		{name: "more than half is included", box: Rect{X: -49, Y: 0, Width: 100, Height: 100}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ratio := visibleAreaRatio(tc.box, viewportW, viewportH)
			probe := elementVisibilityProbe{
				Box:          tc.box,
				AreaRatio:    ratio,
				StyleVisible: true,
				HitTarget:    true,
			}
			if got := probeIsVisible(probe); got != tc.want {
				t.Fatalf("probeIsVisible(box=%+v ratio=%.3f)=%t, want %t", tc.box, ratio, got, tc.want)
			}
		})
	}
	if got := visibleAreaRatio(Rect{X: -50, Y: 0, Width: 100, Height: 100}, viewportW, viewportH); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("half-visible ratio=%v, want 0.5", got)
	}
}

func TestSeeToClickHitTestOcclusionFailsLoud(t *testing.T) {
	meta := &ElementRef{Observed: true, VisibleInViewport: true}
	err := validateSeeToClickProbe("click", "@r3", meta, true, elementVisibilityProbe{
		Box:          Rect{X: 20, Y: 30, Width: 100, Height: 40},
		AreaRatio:    1,
		StyleVisible: true,
		HitTarget:    false,
		Occluder:     `dialog[name="Cookie consent"]`,
	})
	if err == nil {
		t.Fatal("occluded target returned nil error")
	}
	if !errors.Is(err, ErrActFailed) {
		t.Fatalf("error=%v, want ErrActFailed", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "occluded by dialog") || !strings.Contains(msg, "Cookie consent") {
		t.Fatalf("error does not identify the occluder: %v", err)
	}
}

func TestParseActionScrollAtRef(t *testing.T) {
	parsed, err := ParseAction("scroll @r7 down 3")
	if err != nil {
		t.Fatalf("ParseAction: %v", err)
	}
	if parsed.Op != "scroll" || parsed.ScrollTarget != "@r7" || parsed.Direction != "down" || parsed.Steps != 3 {
		t.Fatalf("parsed=%+v", parsed)
	}

	page, err := ParseAction("scroll up")
	if err != nil {
		t.Fatalf("ParseAction page scroll: %v", err)
	}
	if page.Ref != "up" || page.ScrollTarget != "" || page.Direction != "up" || page.Steps != 0 {
		t.Fatalf("page parsed=%+v", page)
	}
}

func TestParseActionClickViewportCSSPixels(t *testing.T) {
	parsed, err := ParseAction("click 320.5,240")
	if err != nil {
		t.Fatalf("ParseAction: %v", err)
	}
	if parsed.Op != "clickxy" || parsed.CoordX != 320.5 || parsed.CoordY != 240 {
		t.Fatalf("parsed=%+v", parsed)
	}
	// A CSS selector list must not be stolen by the coordinate grammar.
	selector, err := ParseAction("click button.primary,a.cta")
	if err != nil {
		t.Fatalf("ParseAction CSS list: %v", err)
	}
	if selector.Op != "click" || selector.Ref != "button.primary,a.cta" {
		t.Fatalf("selector parsed=%+v", selector)
	}
}

func TestScenarioInteractionPolicyAllAgentScenariosUseSeeToClick(t *testing.T) {
	for _, scenario := range []Scenario{ScenarioAppTestExplore, ScenarioAppTestBaseline, ScenarioWebVisit} {
		if !ScenarioInteractionPolicy(scenario).SeeToClick {
			t.Fatalf("scenario %q must enable see-to-click", scenario)
		}
	}
	if ScenarioInteractionPolicy("").SeeToClick {
		t.Fatal("an unknown scenario must remain fail-closed")
	}
	if newActionEngine(newSnapshotEngine()).seeToClick {
		t.Fatal("an unconfigured internal action engine must remain fail-closed")
	}
	snapEngine := newSnapshotEngine()
	actEngine := newActionEngine(snapEngine)
	impl := &browserCoreImpl{snapEngine: snapEngine, actEngine: actEngine}
	for _, scenario := range []Scenario{ScenarioAppTestExplore, ScenarioAppTestBaseline, ScenarioWebVisit} {
		impl.SetInteractionScenario(scenario)
		if !snapEngine.seeToClick || !actEngine.seeToClick {
			t.Fatalf("scenario %q was not wired into both engines", scenario)
		}
	}
}

func TestInteractionUpgradeLeavesScenarioRuntimePoliciesUnchanged(t *testing.T) {
	tests := []struct {
		scenario      Scenario
		deterministic bool
		mode          BrowserMode
	}{
		{scenario: ScenarioAppTestExplore, deterministic: false, mode: ModeHeadless},
		{scenario: ScenarioAppTestBaseline, deterministic: true, mode: ModeHeadless},
		{scenario: ScenarioWebVisit, deterministic: false, mode: ModeHeaded},
	}
	for _, tc := range tests {
		policy, mode, kind := ScenarioPolicy(tc.scenario)
		if policy.RemoteWrites != RemoteWriteDeny || policy.Deterministic != tc.deterministic || mode != tc.mode || kind != SessionKindTask {
			t.Fatalf("ScenarioPolicy(%q)=(%+v,%q,%q)", tc.scenario, policy, mode, kind)
		}
	}
}

func TestCensusTypeCannotExposeElementCapabilities(t *testing.T) {
	refs := []ElementRef{{
		Ref:           "e9",
		BackendNodeID: 42,
		Role:          "button",
		NameFull:      "Below fold",
		TestID:        "below-fold",
	}}
	census := censusFromCandidates(elementCandidatesFromRefs(refs))
	if len(census) != 1 || census[0].Role != "button" || census[0].Name != "Below fold" {
		t.Fatalf("census=%+v", census)
	}
	encoded, err := json.Marshal(census)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, forbidden := range []string{"ref", "backend_node", "testid", "locator", "e9", "below-fold"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("ref-less census leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestElementModeCanResolvePrivateOffscreenCandidateWithoutMintingRef(t *testing.T) {
	snapEngine := newSnapshotEngine()
	snapEngine.elementCandidates = []elementCandidate{{
		BackendNodeID: 42,
		Role:          "button",
		Name:          "Below fold",
	}}
	engine := newActionEngine(snapEngine)
	engine.setSeeToClick(true)

	if _, err := engine.resolveSemanticSelectorForMode("button:'Below fold'", true, false); err == nil {
		t.Fatal("visual mode resolved an unseen candidate")
	}
	got, err := engine.resolveSemanticSelectorForMode("button:'Below fold'", true, true)
	if err != nil {
		t.Fatalf("element mode resolve: %v", err)
	}
	if got != internalBackendNodePrefix+"42" {
		t.Fatalf("element locator=%q", got)
	}
	if len(snapEngine.refMeta) != 0 || len(snapEngine.refTable) != 0 {
		t.Fatal("element-mode resolution minted a public ref")
	}
}

func TestNormalizeInteractionModeDefaultsToVisual(t *testing.T) {
	for _, raw := range []string{"", "visual", " VISUAL "} {
		got, err := NormalizeInteractionMode(raw)
		if err != nil || got != InteractionModeVisual {
			t.Fatalf("NormalizeInteractionMode(%q)=(%q,%v)", raw, got, err)
		}
	}
	if got, err := NormalizeInteractionMode("element"); err != nil || got != InteractionModeElement {
		t.Fatalf("element=(%q,%v)", got, err)
	}
	if _, err := NormalizeInteractionMode("legacy"); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestFallbackViewportFactsPreserveScreenshotCoordinateContract(t *testing.T) {
	got := viewportFactsFromPageLoadProbe(pageLoadProbe{
		ViewportWidth:    393,
		ViewportHeight:   659,
		DevicePixelRatio: 3,
		ScrollX:          4,
		ScrollY:          120,
	})
	if got.Width != 393 || got.Height != 659 || got.DevicePixelRatio != 3 || got.ScrollX != 4 || got.ScrollY != 120 {
		t.Fatalf("viewport=%+v", got)
	}
	if got := viewportFactsFromPageLoadProbe(pageLoadProbe{}); got.DevicePixelRatio != 1 {
		t.Fatalf("default dpr=%v, want 1", got.DevicePixelRatio)
	}
}

// ============================================================
// § 可见集塌缩回归 [BUG-VISIBLE-SET-COLLAPSE]
// ============================================================

// serveStrictHumanFixture 起一个本地 server 托管 tests/strict-human-fixture/index.html
// (含原生 select + checkbox + 大文案 card button)。
func serveStrictHumanFixture(t *testing.T) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "tests", "strict-human-fixture", "index.html"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestVisibleSetKeepsNativeControlsAndIsStableAcrossObserves 钉死症状 1 与症状 2:
//
//	① 原生 select/checkbox 必须拿到 @rN + bbox —— 它们既没有 ContentQuads 语义
//	   也不是 DOM-only 元素, 任何"几何信息缺失就剥夺句柄"的路径都会在这里现形。
//	② 页面完全静止时, 连续两次 observe 的可见集与 total/offscreen 必须逐字节一致;
//	   漂移只允许来自页面自身变化, 不允许来自探测时序。
func TestVisibleSetKeepsNativeControlsAndIsStableAcrossObserves(t *testing.T) {
	requireChromeForPool(t)
	srv := serveStrictHumanFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	core, err := NewBrowserCore(ctx, fmt.Sprintf("visible-set-%d", time.Now().UnixNano()), WithMode(ModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close(context.Background())
	core.SetPolicy(SessionPolicy{RemoteWrites: RemoteWriteDeny}, srv.URL)
	core.(ScenarioInteractionCapable).SetInteractionScenario(ScenarioAppTestExplore)
	if _, err := core.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	sessionCore := core.(SessionCore)

	roleOf := func(snap *Snapshot, testID string) *ElementRef {
		for i := range snap.Refs {
			if snap.Refs[i].TestID == testID {
				return &snap.Refs[i]
			}
		}
		return nil
	}

	first, err := sessionCore.SnapWithSessionMode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	// dom-only-tile 没有 AX 身份 → BackendNodeID=0 → 只有 getBoundingClientRect
	// 这一条几何来源。它和原生控件一起验证"几何来源缺一层就退一层, 不剥夺句柄"。
	for _, tc := range []struct{ testID, role string }{
		{"native-select", "combobox"},
		{"native-check", "checkbox"},
		{"dom-only-tile", "clickable"},
	} {
		ref := roleOf(first, tc.testID)
		if ref == nil {
			t.Fatalf("native %s (%s) is missing from the visible set: %s", tc.role, tc.testID, compactRefRoles(first.Refs))
		}
		if !ref.VisibilityKnown || !ref.VisibleInViewport {
			t.Fatalf("%s visibility stripped: %+v", tc.testID, ref)
		}
		if ref.BBox.Width <= 0 || ref.BBox.Height <= 0 {
			t.Fatalf("%s has no bbox: %+v", tc.testID, ref)
		}
		if !strings.HasPrefix(ref.Ref, "@r") {
			t.Fatalf("%s got no session handle: %q", tc.testID, ref.Ref)
		}
	}

	second, err := sessionCore.SnapWithSessionMode(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.DocumentInteractableCount != second.DocumentInteractableCount ||
		first.VisibleInteractableCount != second.VisibleInteractableCount ||
		first.OffscreenInteractableCount != second.OffscreenInteractableCount {
		t.Fatalf("counts drifted on a static page: first{total:%d visible:%d offscreen:%d} second{total:%d visible:%d offscreen:%d}",
			first.DocumentInteractableCount, first.VisibleInteractableCount, first.OffscreenInteractableCount,
			second.DocumentInteractableCount, second.VisibleInteractableCount, second.OffscreenInteractableCount)
	}
	if first.DocumentInteractableCount != first.VisibleInteractableCount+first.OffscreenInteractableCount {
		t.Fatalf("total != visible + offscreen: %+v", first)
	}
	if got, want := compactRefRoles(second.Refs), compactRefRoles(first.Refs); got != want {
		t.Fatalf("visible set drifted on a static page:\n first=%s\nsecond=%s", want, got)
	}
}

// TestHitAuditSamplesExactlyTheVisibleSet 钉死症状 3 的口径:
// hit_audit 的采样域 = 可见集全量, 不是打印出来的那几个。
func TestHitAuditSamplesExactlyTheVisibleSet(t *testing.T) {
	requireChromeForPool(t)
	srv := serveStrictHumanFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	core, err := NewBrowserCore(ctx, fmt.Sprintf("hit-audit-scope-%d", time.Now().UnixNano()), WithMode(ModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close(context.Background())
	core.SetPolicy(SessionPolicy{RemoteWrites: RemoteWriteDeny}, srv.URL)
	core.(ScenarioInteractionCapable).SetInteractionScenario(ScenarioAppTestExplore)
	if _, err := core.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	snap, err := core.(SessionCore).SnapWithSessionMode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := core.(HitAuditCapable).AuditHitCoverage(ctx, snap.Refs)
	if err != nil {
		t.Fatalf("AuditHitCoverage: %v", err)
	}
	visible := map[string]bool{}
	for _, ref := range snap.Refs {
		if ref.VisibleInViewport && ref.VisibilityKnown {
			visible[ref.Ref] = true
		}
	}
	if len(visible) == 0 {
		t.Fatal("fixture produced an empty visible set")
	}
	for _, f := range findings {
		if !visible[f.Ref] {
			t.Fatalf("hit-audit reported %q which is not in the visible set", f.Ref)
		}
		if f.AimSource == "" {
			t.Fatalf("finding without aim_source: %+v", f)
		}
	}
	// 部分遮挡的 #partial 是 fixture 里唯一"该被点名"的元素。
	var partialRef string
	for _, ref := range snap.Refs {
		if ref.TestID == "partial" {
			partialRef = ref.Ref
		}
	}
	if partialRef == "" {
		t.Fatalf("partial target missing from visible set: %s", compactRefRoles(snap.Refs))
	}
	found := false
	for _, f := range findings {
		if f.Ref == partialRef {
			found = true
		}
	}
	if !found {
		t.Fatalf("occluded target %s not reported by hit-audit: %+v", partialRef, findings)
	}
}

func compactRefRoles(refs []ElementRef) string {
	var sb strings.Builder
	for i := range refs {
		fmt.Fprintf(&sb, "[%s %s %q %.0fx%.0f@%.0f,%.0f vis=%t]", refs[i].Ref, refs[i].Role, refs[i].NameShort,
			refs[i].BBox.Width, refs[i].BBox.Height, refs[i].BBox.X, refs[i].BBox.Y, refs[i].VisibleInViewport)
	}
	return sb.String()
}
