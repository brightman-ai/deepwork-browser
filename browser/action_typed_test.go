package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/chromedp/cdproto/input"
	"gopkg.in/yaml.v3"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"
)

func TestTypedActionTextAndCoordinates(t *testing.T) {
	text := "with 'quote' \"double\" 中文\nlast\n"
	p, err := (Action{Verb: "fill", Target: Target{Kind: "ref", Ref: "@obs-r1"}, Text: text}).executionRequest()
	if err != nil || p.Value != text {
		t.Fatalf("text corrupted %q: %v", p.Value, err)
	}
	for _, verb := range []string{"click", "tap", "hover", "scroll", "drag", "swipe"} {
		a := Action{Verb: verb, Target: Target{Kind: "point", Point: &Point{X: 10, Y: 20, Unit: "css-px", Frame: "viewport", ObservationID: "fixture"}}}
		if verb == "scroll" {
			a.DeltaY = 120
		}
		if verb == "drag" || verb == "swipe" {
			a.End = &Point{X: 30, Y: 40, Unit: "css-px", Frame: "viewport", ObservationID: "fixture"}
		}
		if err := a.Validate(); err != nil {
			t.Errorf("%s: %v", verb, err)
		}
	}
	for _, a := range []Action{{Verb: "fill", Target: Target{Kind: "ref", Ref: "@r1"}, URL: "https://example.com"}, {Verb: "drag", Target: Target{Kind: "point", Point: &Point{X: 1, Y: 1, Unit: "css-px", Frame: "viewport", ObservationID: "fixture"}}, End: &Point{X: math.NaN(), Unit: "css-px", Frame: "viewport", ObservationID: "fixture"}}, {Verb: "fill"}, {Verb: "check"}, {Verb: "set-checked"}, {Verb: "navigate"}, {Verb: "type", Focused: true, Target: Target{Kind: "ref", Ref: "@r1"}}, {Verb: "click", Target: Target{Kind: "point", Point: &Point{Unit: "percent", Frame: "viewport"}}}} {
		if a.Validate() == nil {
			t.Errorf("invalid accepted %+v", a)
		}
	}
}

// Real CDP native editing; cancellation must preserve original text.
func TestTypedStrictEditingCancellationAndReplacement(t *testing.T) {
	requireChromeForPool(t)
	page := `<!doctype html><style>textarea,input{display:block;margin:15px;width:300px;height:50px}</style>
 <textarea aria-label="Normal" data-testid="normal">original 中文 text</textarea>
 <textarea aria-label="Cancel key" data-testid="key" onkeydown="if(event.key==='a')event.preventDefault()">original key</textarea>
 <textarea aria-label="Cancel delete" data-testid="delete" onbeforeinput="if(event.inputType==='deleteContentBackward')event.preventDefault()">original delete</textarea>
 <textarea aria-label="Cancel insert" data-testid="insert" onbeforeinput="if(event.inputType==='insertText')event.preventDefault()">original insert</textarea>
 <textarea aria-label="Readonly" data-testid="readonly" readonly>original readonly</textarea>
 <input aria-label="Secret" data-testid="secret" type="password" value="old-secret">
 <script>window.setterCalls=0; const d=Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value');Object.defineProperty(HTMLTextAreaElement.prototype,'value',{get:d.get,set(v){window.setterCalls++;d.set.call(this,v)}})</script>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	}))
	defer srv.Close()

	// Launch/initial CDP attachment follows the existing 120s startup contract.
	// Editor actions retain their engine's 15s budget; setup cannot consume it.
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 150*time.Second)
	lifetime, finishSetup := fixtureRuntimeLifetime(t, setupCtx)
	core, err := NewBrowserCore(lifetime, fmt.Sprintf("typed-edit-%d", time.Now().UnixNano()), WithMode(ModeHeadless), WithViewport(1280, 900))
	if err != nil {
		setupCancel()
		t.Fatal(err)
	}
	defer core.Close(context.Background())
	defer setupCancel()
	core.(ScenarioInteractionCapable).SetInteractionScenario(ScenarioAppTestExplore)
	core.SetPolicy(SessionPolicy{RemoteWrites: RemoteWriteDeny}, srv.URL)
	if _, _, err := core.(TypedActionCapable).ExecuteAction(setupCtx, Action{Verb: "navigate", URL: srv.URL}, false, true, InteractionModeVisual); err != nil {
		t.Fatal(err)
	}
	snap, err := core.(SessionCore).SnapWithSessionMode(setupCtx, 1)
	if err != nil {
		t.Fatal(err)
	}
	requireFixtureCapture(t, snap)
	finishSetup()
	setupCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	t.Logf("initial snapshot type=%s diagnostics=%+v", snap.SnapshotType, snap.Diagnostics)
	if len(snap.Refs) == 0 {
		var facts map[string]interface{}
		if err := core.EvalJS(ctx, "({url:location.href,title:document.title,ready:document.readyState,body:document.body&&document.body.innerHTML,viewport:[innerWidth,innerHeight],controls:document.querySelectorAll('textarea,input').length})", &facts); err != nil {
			t.Logf("page probe unavailable: %v", err)
		} else {
			t.Logf("fixture facts=%+v", facts)
		}
		if evidence := os.Getenv("DW_TYPED_EDIT_EVIDENCE"); evidence != "" {
			image, err := core.Screenshot(ctx, false)
			if err != nil {
				t.Logf("actual screenshot unavailable: %v", err)
			} else if err := os.WriteFile(filepath.Join(evidence, "native-page.jpg"), image, 0600); err != nil {
				t.Fatal(err)
			}
		}
		readyUntil := time.Now().Add(15 * time.Second)
		for len(snap.Refs) == 0 && time.Now().Before(readyUntil) && ctx.Err() == nil {
			time.Sleep(100 * time.Millisecond)
			snap, err = core.(SessionCore).SnapWithSessionMode(ctx, 1)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("fresh snapshot type=%s diagnostics=%+v", snap.SnapshotType, snap.Diagnostics)
		}
	}
	ref := func(id string) string {
		for _, r := range snap.Refs {
			if r.TestID == id {
				return r.Ref
			}
		}
		t.Fatalf("missing ref %s: snapshot=%s text=%s diagnostics=%+v", id, snap.SnapshotType, snap.Text, snap.Diagnostics)
		return ""
	}

	impl := core.(*browserCoreImpl)
	runCtx, cancelRun := deriveTargetContext(ctx, impl.currentCtx())
	defer cancelRun()
	meta, err := impl.actEngine.strictTargetMeta(ref("normal"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.(TypedActionCapable).ExecuteAction(ctx, Action{Verb: "focus", Target: Target{Kind: "ref", Ref: ref("normal")}}, false, true, InteractionModeVisual); err != nil {
		t.Fatal(err)
	}
	modifier := input.ModifierCtrl
	if goruntime.GOOS == "darwin" {
		modifier = input.ModifierMeta
	}
	if err := dispatchModifierCombo(runCtx, modifier, "a"); err != nil {
		t.Fatal(err)
	}
	selected, err := strictEditableStateForMeta(runCtx, meta)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("shortcut-only experiment: full_selection=%v original_preserved=%v", selected.SelectionKnown && selected.Prefix == "" && selected.Suffix == "", selected.Value == "original 中文 text")
	if selected.Value != "original 中文 text" {
		t.Fatal("selection probe changed original text")
	}
	for _, tc := range []struct {
		id, want, phase    string
		success, sensitive bool
	}{
		{"key", "original key", "selection", false, false},
		{"delete", "original delete", "delete", false, false},
		{"insert", "", "verify", false, false},
		{"readonly", "original readonly", "", false, false},
		{"normal", "quotes '中文'\nlast\n", "", true, false},
		{"secret", "new-secret", "", true, true},
	} {
		snap, err = core.(SessionCore).SnapWithSessionMode(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		a := Action{Verb: "fill", Target: Target{Kind: "ref", Ref: ref(tc.id)}, Text: tc.want, Sensitive: tc.sensitive}
		if !tc.success {
			a.Text = "must not replace"
		}
		_, out, err := core.(TypedActionCapable).ExecuteAction(ctx, a, false, true, InteractionModeVisual)
		t.Logf("%s: phase=%s dispatch=%s err=%v", tc.id, out.Phase, out.DispatchState, err)
		if (err == nil) != tc.success {
			t.Fatalf("%s success=%v err=%v", tc.id, tc.success, err)
		}
		if tc.phase != "" && out.Phase != tc.phase {
			t.Errorf("%s phase=%s want %s", tc.id, out.Phase, tc.phase)
		}
		var got string
		if err := core.EvalJS(ctx, fmt.Sprintf("document.querySelector('[data-testid=%s]').value", tc.id), &got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("%s changed incorrectly", tc.id)
		}
	}
	var setters int
	if err := core.EvalJS(ctx, "window.setterCalls", &setters); err != nil {
		t.Fatal(err)
	}
	if setters != 0 {
		t.Fatalf("DOM setter used %d times", setters)
	}
}

func TestTypedActionWireTextPresenceAndStrictFields(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		decode := func(raw string) (Action, error) {
			var a Action
			var err error
			if format == "json" {
				err = json.Unmarshal([]byte(raw), &a)
			} else {
				err = yaml.Unmarshal([]byte(raw), &a)
			}
			return a, err
		}
		raws := []string{`{"kind":"fill","target":{"kind":"ref","ref":"@ofixture-r1"}}`, `{"kind":"fill","target":{"kind":"ref","ref":"@ofixture-r1"},"text":""}`}
		if format == "yaml" {
			raws = []string{"kind: fill\ntarget: {kind: ref, ref: '@ofixture-r1'}\n", "kind: fill\ntarget: {kind: ref, ref: '@ofixture-r1'}\ntext: ''\n"}
		}
		missing, err := decode(raws[0])
		if err != nil {
			t.Fatal(err)
		}
		if missing.Validate() == nil || missing.TextWasProvided() {
			t.Fatalf("%s omitted text accepted", format)
		}
		explicit, err := decode(raws[1])
		if err != nil || explicit.Validate() != nil || !explicit.TextWasProvided() {
			t.Fatalf("%s explicit empty rejected: %+v %v", format, explicit, err)
		}
		var serialized []byte
		if format == "json" {
			serialized, err = json.Marshal(explicit)
		} else {
			serialized, err = yaml.Marshal(explicit)
		}
		if err != nil {
			t.Fatal(err)
		}
		roundTrip, err := decode(string(serialized))
		if err != nil || !roundTrip.TextWasProvided() || roundTrip.Text != "" || roundTrip.Validate() != nil {
			t.Fatalf("%s empty text lost: %s %v", format, serialized, err)
		}
		bound := missing.WithText("中文\n")
		if bound.Validate() != nil || bound.Text != "中文\n" {
			t.Fatalf("explicit binding failed")
		}
		bad := []string{`{"kind":"fill","text":null}`, `{"kind":"click","bogus":true}`, `{"kind":"click","target":{"kind":"point","point":{"x":1,"y":1,"unit":"css-px","frame":"viewport","bogus":1}}}`}
		if format == "yaml" {
			bad = []string{"kind: fill\ntext: null\n", "kind: click\nbogus: true\n", "kind: click\ntarget: {kind: point, point: {x: 1, y: 1, unit: css-px, frame: viewport, bogus: 1}}\n"}
		}
		for _, raw := range bad {
			if _, err := decode(raw); err == nil {
				t.Errorf("%s accepted unknown/null fields %s", format, raw)
			}
		}
	}
}

func TestTypedNavigationSchemeContract(t *testing.T) {
	for _, raw := range []string{"https://example.com/path", "http://127.0.0.1:1234/", "file:///tmp/page.html", "about:blank", "data:text/html;base64,PGgxPkZpeHR1cmU8L2gxPg=="} {
		if err := ValidateNavigationURL(raw); err != nil {
			t.Errorf("valid document URL %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"javascript:document.body.remove()", "JAVASCRIPT:alert(1)", "vbscript:msgbox(1)", "blob:https://example.com/id", "about:srcdoc", "data:application/javascript,alert(1)", "file://remote/path", "https:///missing-host", " https://example.com", "/tmp/page.html"} {
		if err := ValidateNavigationURL(raw); err == nil {
			t.Errorf("script/invalid URL accepted %q", raw)
		}
	}
}

func TestDocumentPresenceNeverExportsCredentialFormValues(t *testing.T) {
	requireChromeForPool(t)
	page := `<!doctype html><meta charset="utf-8"><h1>Visible safe body</h1><textarea data-testid="generic" aria-label="Generic editor">generic-private-value</textarea><input aria-label="Password" type="password" value="password-private-value"><input aria-label="Credential" name="password" value="named-private-value">`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	}))
	defer srv.Close()
	setup, cancelSetup := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancelSetup()
	core, err := NewBrowserCore(setup, fmt.Sprintf("privacy-probe-%d", time.Now().UnixNano()), WithMode(ModeHeadless), WithViewport(1280, 800))
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close(context.Background())
	core.(ScenarioInteractionCapable).SetInteractionScenario(ScenarioAppTestExplore)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, _, err := core.ExecuteAction(ctx, Action{Verb: "navigate", URL: srv.URL}, false, true, InteractionModeVisual); err != nil {
		t.Fatal(err)
	}
	before, err := core.(SessionCore).SnapWithSessionMode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(before.DocumentText, "password-private-value") || strings.Contains(before.DocumentText, "named-private-value") {
		t.Fatal("credential field exported into document facts")
	}
	if !strings.Contains(before.DocumentText, "generic-private-value") || !strings.Contains(before.DocumentText, "Visible safe body") {
		t.Fatal("ordinary rendered facts lost")
	}
	if before.DocumentTextComplete {
		t.Fatal("redacted document falsely complete")
	}
	// This is the same host-owned state callback that CLI persistence restores;
	// no DOM marker/setter, secret registry or tested-runtime mock is used.
	info := &SessionInfo{RedactFormValues: true}
	core.(*browserCoreImpl).SetSessionAuthorityProvider(func(context.Context) (*SessionInfo, error) { return info, nil })
	after, err := core.(SessionCore).SnapWithSessionMode(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"generic-private-value", "password-private-value", "named-private-value"} {
		if strings.Contains(string(data), private) {
			t.Fatal("opted sensitive form value exported into observation")
		}
	}
	if !strings.Contains(after.DocumentText, "Visible safe body") || after.DocumentTextComplete {
		t.Fatal("redacted facts lost body witness or claimed completeness")
	}
}

func TestPointWireRequiresAxesAndKeepsDeliberateZero(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		decode := func(raw string) (Action, error) {
			var request Action
			var err error
			if format == "json" {
				err = json.Unmarshal([]byte(raw), &request)
			} else {
				err = yaml.Unmarshal([]byte(raw), &request)
			}
			return request, err
		}
		zero := `{"kind":"click","target":{"kind":"point","point":{"x":0,"y":0,"unit":"css-px","frame":"viewport","observation_id":"capture"}}}`
		bad := []string{`{"kind":"click","target":{"kind":"point","point":{"y":0,"unit":"css-px","frame":"viewport","observation_id":"capture"}}}`, `{"kind":"click","target":{"kind":"point","point":{"x":0,"unit":"css-px","frame":"viewport","observation_id":"capture"}}}`, `{"kind":"drag","target":{"kind":"point","point":{"x":0,"y":0,"unit":"css-px","frame":"viewport","observation_id":"capture"}},"end":{"y":2,"unit":"css-px","frame":"viewport","observation_id":"capture"}}`}
		if format == "yaml" {
			zero = "kind: click\ntarget: {kind: point, point: {x: 0, y: 0, unit: css-px, frame: viewport, observation_id: capture}}\n"
			bad = []string{"kind: click\ntarget: {kind: point, point: {y: 0, unit: css-px, frame: viewport, observation_id: capture}}\n", "kind: click\ntarget: {kind: point, point: {x: 0, unit: css-px, frame: viewport, observation_id: capture}}\n", "kind: drag\ntarget: {kind: point, point: {x: 0, y: 0, unit: css-px, frame: viewport, observation_id: capture}}\nend: {y: 2, unit: css-px, frame: viewport, observation_id: capture}\n"}
		}
		request, err := decode(zero)
		if err != nil || request.Validate() != nil || request.Target.Point.X != 0 || request.Target.Point.Y != 0 {
			t.Fatalf("%s deliberate zero rejected: %+v %v", format, request, err)
		}
		var encoded []byte
		if format == "json" {
			encoded, err = json.Marshal(request)
		} else {
			encoded, err = yaml.Marshal(request)
		}
		if err != nil {
			t.Fatal(err)
		}
		if roundTrip, err := decode(string(encoded)); err != nil || roundTrip.Validate() != nil {
			t.Fatalf("%s zero roundtrip invalid: %v", format, err)
		}
		for _, raw := range bad {
			if _, err := decode(raw); err == nil {
				t.Fatalf("%s missing axis accepted: %s", format, raw)
			}
		}
	}
}
