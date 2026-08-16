package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/dom"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

func TestScenarioInteractionPolicyFidelityTable(t *testing.T) {
	tests := []struct {
		scenario Scenario
		want     InteractionFidelity
	}{
		{ScenarioAppTestExplore, InteractionFidelityStrictHuman},
		{ScenarioAppTestBaseline, InteractionFidelityDual},
		{ScenarioWebVisit, InteractionFidelityUtility},
	}
	for _, tc := range tests {
		policy := ScenarioInteractionPolicy(tc.scenario)
		if !policy.SeeToClick || policy.Fidelity != tc.want {
			t.Fatalf("ScenarioInteractionPolicy(%q)=%+v, want see-to-click %q", tc.scenario, policy, tc.want)
		}
	}
	if got := ScenarioInteractionPolicy(""); got.SeeToClick || got.Fidelity != "" {
		t.Fatalf("unknown scenario must remain fail-closed: %+v", got)
	}
}

func TestContentQuadCentroidAndFivePointSampling(t *testing.T) {
	small := []float64{0, 0, 10, 0, 10, 10, 0, 10}
	large := []float64{20, 30, 220, 30, 220, 130, 20, 130}
	quad, centroid, ok := largestContentQuad([][]float64{small, large})
	if !ok || len(quad) != 8 || centroid.X != 120 || centroid.Y != 80 {
		t.Fatalf("largest quad centroid=(%+v,%v,%v)", quad, centroid, ok)
	}
	points := fivePointSamples(quad, centroid)
	want := []actionPoint{{120, 80}, {70, 55}, {170, 55}, {170, 105}, {70, 105}}
	if len(points) != len(want) {
		t.Fatalf("samples=%+v", points)
	}
	for i := range want {
		if points[i] != want[i] {
			t.Fatalf("sample[%d]=%+v, want %+v", i, points[i], want[i])
		}
	}
}

func TestStrictHumanFillTypePressSelectAndHitCoverage(t *testing.T) {
	requireChromeForPool(t)

	page := `<!doctype html><meta charset="utf-8"><style>
		body { margin: 0; font: 16px sans-serif; }
		input, select { display:block; margin:16px; width:240px; height:36px; }
		#partial { position:absolute; left:20px; top:220px; width:200px; height:100px; }
		#occluder { position:absolute; left:50px; top:230px; width:40px; height:30px; z-index:5; background:rgba(255,0,0,.2); }
		iframe { position:absolute; left:600px; top:80px; width:360px; height:140px; border:8px solid #ccc; }
	</style>
	<input data-testid="field" aria-label="Field" value="seed">
	<input data-testid="other" aria-label="Other">
	<select data-testid="choice" aria-label="Choice"><option value="a">Alpha</option><option value="b">Beta</option></select>
	<div id="shadow-host"></div>
	<iframe title="Nested editor" src="/frame"></iframe>
	<button id="partial" data-testid="partial">Partial target</button><div id="occluder"></div>
	<script>
	window.__events = [];
	const shadow = document.querySelector('#shadow-host').attachShadow({mode:'open'});
	shadow.innerHTML = '<style>input{display:block;margin:16px;width:240px;height:36px}</style><input data-testid="shadow-field" aria-label="Shadow Field" value="shadow-seed">';
	for (const type of ['pointerdown','mousedown','mouseup','click','focus','keydown','keyup','beforeinput','input','change']) {
		document.addEventListener(type, event => window.__events.push({
			type, trusted:event.isTrusted, target:event.target && event.target.getAttribute && event.target.getAttribute('data-testid') || ''
		}), true);
	}
	const desc = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value');
	Object.defineProperty(HTMLInputElement.prototype, 'value', {
		configurable: desc.configurable, enumerable: desc.enumerable, get: desc.get,
		set(value) { window.__events.push({type:'native-setter', trusted:false, target:this.getAttribute('data-testid') || ''}); return desc.set.call(this, value); }
	});
	</script>`
	framePage := `<!doctype html><meta charset="utf-8"><style>body{margin:0}input{margin:30px;width:240px;height:36px}</style>
		<input data-testid="frame-field" aria-label="Frame Field" value="frame-seed">`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/frame" {
			_, _ = w.Write([]byte(framePage))
			return
		}
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	core, err := NewBrowserCore(ctx, fmt.Sprintf("strict-human-%d", time.Now().UnixNano()), WithMode(ModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close(context.Background())
	core.SetPolicy(SessionPolicy{RemoteWrites: RemoteWriteDeny}, srv.URL)
	capable, ok := core.(ScenarioInteractionCapable)
	if !ok {
		t.Fatal("core lacks ScenarioInteractionCapable")
	}
	capable.SetInteractionScenario(ScenarioAppTestExplore)
	if _, err := core.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	sessionCore, ok := core.(SessionCore)
	if !ok {
		t.Fatal("core lacks SessionCore")
	}
	snap, err := sessionCore.SnapWithSessionMode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	refByTestID := func(testID string) string {
		for _, ref := range snap.Refs {
			if ref.TestID == testID {
				return ref.Ref
			}
		}
		t.Fatalf("testid %q not found in refs: %+v", testID, snap.Refs)
		return ""
	}
	refByName := func(name string) string {
		for _, ref := range snap.Refs {
			if ref.NameFull == name || ref.Name == name {
				return ref.Ref
			}
		}
		t.Fatalf("name %q not found in refs: %+v", name, snap.Refs)
		return ""
	}
	fieldRef := refByTestID("field")
	otherRef := refByTestID("other")
	choiceRef := refByTestID("choice")
	partialRef := refByTestID("partial")
	shadowRef := refByName("Shadow Field")

	if _, err := sessionCore.ActWithSessionMode(ctx, "press "+otherRef+" End", false); err == nil || !strings.Contains(err.Error(), "run click "+otherRef+" first") {
		t.Fatalf("strict press without human focus error=%v", err)
	}
	if _, err := sessionCore.ActWithSessionMode(ctx, "fill "+fieldRef+" 'Human'", false); err != nil {
		t.Fatalf("strict fill: %v", err)
	}
	report := core.(ActionFidelityCapable).LastActionFidelity()
	if report.Fidelity != InteractionFidelityStrictHuman || report.AimSource != AimSourceContentQuadCentroid ||
		len(report.HumanPath) != 6 || report.HumanPath[0] != "mouse_click" || report.HumanPath[4] != "Input.insertText_per_character" {
		t.Fatalf("fill fidelity report=%+v", report)
	}

	if _, err := sessionCore.ActWithSessionMode(ctx, "type "+fieldRef+" '!'", false); err != nil {
		t.Fatalf("strict type: %v", err)
	}
	if _, err := sessionCore.ActWithSessionMode(ctx, "press "+fieldRef+" End", false); err != nil {
		t.Fatalf("strict press after human focus: %v", err)
	}
	if report = core.(ActionFidelityCapable).LastActionFidelity(); !report.FocusUpdated || report.FocusedBackend == 0 {
		t.Fatalf("strict press did not renew human focus proof: %+v", report)
	}

	var state struct {
		Value  string `json:"value"`
		Events []struct {
			Type    string `json:"type"`
			Trusted bool   `json:"trusted"`
			Target  string `json:"target"`
		} `json:"events"`
	}
	if err := core.EvalJS(ctx, `({value:document.querySelector('[data-testid="field"]').value,events:window.__events})`, &state); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(state.Value, "Human") || !strings.Contains(state.Value, "!") {
		t.Fatalf("strict text value=%q", state.Value)
	}
	seenClick, seenTrustedInput := false, false
	for _, event := range state.Events {
		if event.Target != "field" {
			continue
		}
		if event.Type == "native-setter" {
			t.Fatalf("strict path invoked native value setter: %+v", state.Events)
		}
		if event.Type == "click" && event.Trusted {
			seenClick = true
		}
		if event.Type == "input" && event.Trusted {
			seenTrustedInput = true
		}
	}
	if !seenClick || !seenTrustedInput {
		t.Fatalf("strict path lacks trusted click/input evidence: %+v", state.Events)
	}

	if _, err := sessionCore.ActWithSessionMode(ctx, "fill "+shadowRef+" 'Shadow'", false); err != nil {
		t.Fatalf("strict shadow fill: %v", err)
	}
	var nested struct {
		ShadowValue string `json:"shadow_value"`
		NativeSet   bool   `json:"native_set"`
	}
	if err := core.EvalJS(ctx, `({
		shadow_value: document.querySelector('#shadow-host').shadowRoot.querySelector('input').value,
		native_set: window.__events.some(event => event.type === 'native-setter')
	})`, &nested); err != nil {
		t.Fatal(err)
	}
	if nested.ShadowValue != "Shadow" || nested.NativeSet {
		t.Fatalf("nested strict-human paths=%+v", nested)
	}
	report = core.(ActionFidelityCapable).LastActionFidelity()
	if report.Fidelity != InteractionFidelityStrictHuman || report.AimSource != AimSourceContentQuadCentroid ||
		len(report.HumanPath) < 2 || report.HumanPath[1] != "active_element_verified" {
		t.Fatalf("shadow activeElement chain report=%+v", report)
	}

	// The current snapshot source does not flatten child-frame AX trees into
	// public refs. Exercise the iframe branch of the focus verifier directly:
	// resolve the real child-frame input, click its top-viewport ContentQuad,
	// then prove activeElement from child document through frameElement to top.
	impl := core.(*browserCoreImpl)
	impl.mu.RLock()
	targetCtx := impl.currentCtx()
	impl.mu.RUnlock()
	runCtx, runCancel := deriveTargetContext(ctx, targetCtx)
	defer runCancel()
	var frameMeta ElementRef
	var frameAim actionPoint
	if err := chromedp.Run(runCtx, chromedp.ActionFunc(func(execCtx context.Context) error {
		obj, exc, err := cdpruntime.Evaluate(`document.querySelector('iframe').contentDocument.querySelector('[data-testid="frame-field"]')`).Do(execCtx)
		if err != nil {
			return err
		}
		if exc != nil || obj == nil || obj.ObjectID == "" {
			return fmt.Errorf("resolve frame input: exception=%v object=%v", exc, obj)
		}
		defer func() { _ = cdpruntime.ReleaseObject(obj.ObjectID).Do(execCtx) }()
		node, err := dom.DescribeNode().WithObjectID(obj.ObjectID).Do(execCtx)
		if err != nil || node == nil {
			return fmt.Errorf("describe frame input: %w", err)
		}
		quads, err := dom.GetContentQuads().WithBackendNodeID(node.BackendNodeID).Do(execCtx)
		if err != nil {
			return err
		}
		rawQuads := make([][]float64, 0, len(quads))
		for _, quad := range quads {
			rawQuads = append(rawQuads, append([]float64(nil), quad...))
		}
		_, centroid, ok := largestContentQuad(rawQuads)
		if !ok {
			return fmt.Errorf("frame input has no content quad")
		}
		frameMeta.BackendNodeID = int64(node.BackendNodeID)
		frameAim = centroid
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := dispatchMouseClickAt(runCtx, frameAim.X, frameAim.Y); err != nil {
		t.Fatal(err)
	}
	if focused, backendNodeID, err := activeElementMatchesMeta(runCtx, &frameMeta); err != nil || !focused || backendNodeID != frameMeta.BackendNodeID {
		t.Fatalf("iframe activeElement chain=(focused=%t backend=%d err=%v), want backend=%d", focused, backendNodeID, err, frameMeta.BackendNodeID)
	}

	// observe --hit-audit always audits the freshly captured visible set.
	snap, err = sessionCore.SnapWithSessionMode(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	partialRef = refByTestID("partial")
	choiceRef = refByTestID("choice")
	auditor := core.(HitAuditCapable)
	findings, err := auditor.AuditHitCoverage(ctx, snap.Refs)
	if err != nil {
		t.Fatal(err)
	}
	foundPartial := false
	for _, finding := range findings {
		if finding.Ref == partialRef {
			foundPartial = true
			if finding.HitCoverage != "4/5" || finding.AimSource != AimSourceContentQuadCentroid {
				t.Fatalf("partial finding=%+v", finding)
			}
		}
	}
	if !foundPartial {
		t.Fatalf("hit audit missed partial target: %+v", findings)
	}
	if _, err := sessionCore.ActWithSessionMode(ctx, "click "+partialRef, false); err != nil {
		t.Fatalf("partial-center click should succeed: %v", err)
	}
	report = core.(ActionFidelityCapable).LastActionFidelity()
	if report.HitCoverage != "4/5" || report.AimSource != AimSourceContentQuadCentroid {
		t.Fatalf("partial click report=%+v", report)
	}

	if _, err := sessionCore.ActWithSessionMode(ctx, "select "+choiceRef+" 'Beta'", false); err != nil {
		t.Fatalf("strict synthetic select: %v", err)
	}
	report = core.(ActionFidelityCapable).LastActionFidelity()
	if report.Fidelity != InteractionFidelityStrictHuman || !report.Synthetic || !strings.Contains(report.SyntheticNote, "not evidence") {
		t.Fatalf("strict select report=%+v", report)
	}

	capable.SetInteractionScenario(ScenarioWebVisit)
	if _, err := sessionCore.ActWithSessionMode(ctx, "select "+choiceRef+" 'Alpha'", false); err != nil {
		t.Fatalf("utility synthetic select: %v", err)
	}
	report = core.(ActionFidelityCapable).LastActionFidelity()
	if report.Fidelity != InteractionFidelityUtility || !report.Synthetic {
		t.Fatalf("utility select report=%+v", report)
	}
}

func TestStrictHumanRejectsElementMode(t *testing.T) {
	engine := newActionEngine(newSnapshotEngine())
	engine.setInteractionPolicy(ScenarioInteractionPolicy(ScenarioAppTestExplore))
	_, err := engine.ExecuteWithInteractionMode(context.Background(), "click button", false, true, InteractionModeElement)
	if err == nil || !strings.Contains(err.Error(), "unavailable in strict-human") {
		t.Fatalf("strict element mode error=%v", err)
	}
}
