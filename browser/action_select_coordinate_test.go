package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMatchNativeSelectOptionPriorityAndDiagnostics(t *testing.T) {
	options := []SelectOption{
		{Label: "Value wins", Value: "TencentDB-Agent-Memory"},
		{Label: "TencentDB-Agent-Memory", Value: "/home/ubuntu/code/stwork/TencentDB-Agent-Memory"},
		{Label: "Alpha Pipeline", Value: "alpha"},
		{Label: "Alpine Pipeline", Value: "alpine"},
		{Label: "Gamma Unique", Value: "gamma"},
	}

	tests := []struct {
		name      string
		query     string
		wantIndex int
		wantKind  string
	}{
		{name: "exact value wins before colliding label", query: "TencentDB-Agent-Memory", wantIndex: 0, wantKind: "value"},
		{name: "exact visible label", query: "Gamma Unique", wantIndex: 4, wantKind: "label"},
		{name: "unique visible label prefix", query: "Gamma", wantIndex: 4, wantKind: "label-prefix"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := matchNativeSelectOption(options, tc.query)
			if err != nil {
				t.Fatalf("matchNativeSelectOption(%q): %v", tc.query, err)
			}
			if got.Index != tc.wantIndex || got.Kind != tc.wantKind {
				t.Fatalf("match = %+v, want index=%d kind=%q", got, tc.wantIndex, tc.wantKind)
			}
		})
	}

	t.Run("ambiguous prefix lists label-to-value choices", func(t *testing.T) {
		_, err := matchNativeSelectOption(options, "Al")
		if err == nil {
			t.Fatal("ambiguous prefix unexpectedly matched")
		}
		for _, want := range []string{`"Alpha Pipeline"→"alpha"`, `"Alpine Pipeline"→"alpine"`, "ambiguous"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q missing %q", err, want)
			}
		}
	})

	t.Run("missing choice lists every option", func(t *testing.T) {
		_, err := matchNativeSelectOption(options, "Missing")
		if err == nil {
			t.Fatal("missing choice unexpectedly matched")
		}
		for _, option := range options {
			want := fmt.Sprintf("%q→%q", option.Label, option.Value)
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q missing %q", err, want)
			}
		}
	})
}

func TestTypedRequestAbsoluteHoverAndWheel(t *testing.T) {
	hover, err := (Action{Verb: "hover", Target: fixtureViewport(414, 314)}).executionRequest()
	if err != nil {
		t.Fatalf("TypedRequest hoverat: %v", err)
	}
	if hover.Op != "hoverat" || hover.Ref != "" || hover.CoordX != 414 || hover.CoordY != 314 {
		t.Fatalf("hoverat parsed as %+v", hover)
	}

	wheel, err := (Action{Verb: "scroll", Target: fixtureViewport(1200, 800), Direction: "down", Steps: 6}).executionRequest()
	if err != nil {
		t.Fatalf("TypedRequest wheelat: %v", err)
	}
	if wheel.Op != "wheelat" || wheel.Ref != "" || wheel.CoordX != 1200 || wheel.CoordY != 800 {
		t.Fatalf("wheelat parsed as %+v", wheel)
	}
	if wheel.Direction != "down" || wheel.Steps != 6 || wheel.DeltaY != humanWheelStepCSSPixels || wheel.DeltaX != 0 {
		t.Fatalf("wheelat direction/delta = %+v", wheel)
	}

	legacy, err := (Action{Verb: "scroll", Target: fixtureRelative("css=#chart", 50, 50), DeltaY: -240}).executionRequest()
	if err != nil {
		t.Fatalf("TypedRequest selector-relative wheelat: %v", err)
	}
	if legacy.Ref != "css=#chart" || legacy.CoordX != 0.5 || legacy.CoordY != 0.5 || legacy.DeltaY != -240 {
		t.Fatalf("selector-relative wheelat regressed: %+v", legacy)
	}
}

const nativeSelectPage = `<!doctype html><html><body>
<button aria-label="Overview">Overview</button>
<label>Project <select aria-label="Project" id="project">
  <option value="ubuntu">Ubuntu</option>
  <option value="/home/ubuntu/code/stwork/TencentDB-Agent-Memory">TencentDB-Agent-Memory</option>
  <option value="alpha">Alpha Pipeline</option>
  <option value="alpine">Alpine Pipeline</option>
  <option value="gamma">Gamma Unique</option>
</select></label>
<script>
window.actionEvents = [];
document.querySelector('#project').addEventListener('input', e => actionEvents.push('input:' + e.target.value));
document.querySelector('#project').addEventListener('change', e => actionEvents.push('change:' + e.target.value));
</script></body></html>`

func TestActionEngineNativeSelectWithoutPicker(t *testing.T) {
	requireChromeForPool(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(nativeSelectPage))
	}))
	defer srv.Close()

	// Constructor/navigation/first capture follow the launcher's 120s startup
	// contract. They must not consume the existing 45s action phase below.
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer setupCancel()
	lifetime, finishSetup := fixtureRuntimeLifetime(t, setupCtx)
	core, err := NewBrowserCore(lifetime, fmt.Sprintf("select-coordinate-%d", time.Now().UnixNano()), WithMode(BrowserModeHeadless))
	if err != nil {
		t.Fatalf("NewBrowserCore: %v", err)
	}
	defer func() { _ = core.Close(context.Background()) }()
	core.(ScenarioInteractionCapable).SetInteractionScenario(ScenarioAppTestBaseline)
	core.SetPolicy(SessionPolicy{RemoteWrites: RemoteWriteAllow}, "")

	if _, err := core.Navigate(setupCtx, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	initial, err := core.Snap(setupCtx)
	if err != nil {
		t.Fatalf("Snap: %v", err)
	}
	requireFixtureCapture(t, initial)
	finishSetup()
	setupCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	assertWithin := func(action Action, wantErr string, max time.Duration) {
		t.Helper()
		capture, err := core.Snap(ctx)
		if err != nil {
			t.Fatalf("capture before %+v: %v", action, err)
		}
		requireFixtureCapture(t, capture)
		started := time.Now()
		// This functional baseline exercises explicit element auto-focus/select
		// behavior. Visual strict input is covered by the separate native proofs.
		_, _, actErr := core.ExecuteAction(ctx, action, false, false, InteractionModeElement)
		elapsed := time.Since(started)
		t.Logf("act %+v elapsed=%s err=%v", action, elapsed, actErr)
		if elapsed > max {
			t.Fatalf("%+v took %s, want <= %s", action, elapsed, max)
		}
		if wantErr == "" && actErr != nil {
			t.Fatalf("%+v: %v", action, actErr)
		}
		if wantErr != "" && (actErr == nil || !strings.Contains(actErr.Error(), wantErr)) {
			t.Fatalf("%+v error = %v, want substring %q", action, actErr, wantErr)
		}
	}
	assertValue := func(want string) {
		t.Helper()
		var got string
		if err := core.EvalJS(ctx, `document.querySelector('#project').value`, &got); err != nil {
			t.Fatalf("read select value: %v", err)
		}
		if got != want {
			t.Fatalf("select value = %q, want %q", got, want)
		}
	}
	assertFocused := func(selector string) {
		t.Helper()
		var got bool
		if err := core.EvalJS(ctx, `document.activeElement === document.querySelector(`+fmt.Sprintf("%q", selector)+`)`, &got); err != nil {
			t.Fatalf("read active element: %v", err)
		}
		if !got {
			t.Fatalf("active element is not %s", selector)
		}
	}

	assertWithin(Action{Verb: "focus", Target: fixtureLocator("role=button[name*=\"Overview\"]")}, "", 2*time.Second)
	assertWithin(Action{Verb: "focus", Target: fixtureLocator("role=combobox[name*=\"Project\"]")}, "", 2*time.Second)
	assertFocused("#project")
	assertWithin(Action{Verb: "focus", Target: fixtureLocator("role=button[name*=\"Overview\"]")}, "", 2*time.Second)
	assertWithin(Action{Verb: "press", Target: fixtureLocator("role=combobox[name*=\"Project\"]"), Key: "ArrowDown"}, "", 2*time.Second)
	assertFocused("#project")
	// Press is a native key only; it makes no selected-value guarantee.
	assertWithin(Action{Verb: "press", Target: fixtureLocator("role=combobox[name*=\"Project\"]"), Key: "Enter"}, "", 2*time.Second)
	assertWithin(Action{Verb: "press", Target: fixtureLocator("role=combobox[name*=\"Project\"]"), Key: "Escape"}, "", 2*time.Second)

	if err := core.EvalJS(ctx, `(() => { document.querySelector('#project').selectedIndex = 0; return true; })()`, new(bool)); err != nil {
		t.Fatalf("reset select: %v", err)
	}
	assertWithin(Action{Verb: "select", Target: fixtureLocator("role=combobox[name*=\"Project\"]"), Value: "TencentDB-Agent-Memory"}, "", 2*time.Second)
	assertValue("/home/ubuntu/code/stwork/TencentDB-Agent-Memory")
	assertWithin(Action{Verb: "select", Target: fixtureLocator("role=combobox[name*=\"Project\"]"), Value: "gamma"}, "", 2*time.Second)
	assertValue("gamma")
	assertWithin(Action{Verb: "select", Target: fixtureLocator("role=combobox[name*=\"Project\"]"), Value: "Gamma"}, "", 2*time.Second)
	assertValue("gamma")
	assertWithin(Action{Verb: "select", Target: fixtureLocator("role=combobox[name*=\"Project\"]"), Value: "Al"}, `"Alpha Pipeline"→"alpha"`, 2*time.Second)
	assertWithin(Action{Verb: "select", Target: fixtureLocator("role=combobox[name*=\"Project\"]"), Value: "Missing"}, `"TencentDB-Agent-Memory"→"/home/ubuntu/code/stwork/TencentDB-Agent-Memory"`, 2*time.Second)

	var events []string
	if err := core.EvalJS(ctx, `window.actionEvents`, &events); err != nil {
		t.Fatalf("read select events: %v", err)
	}
	if len(events) < 2 || events[0] != "input:/home/ubuntu/code/stwork/TencentDB-Agent-Memory" || events[1] != "change:/home/ubuntu/code/stwork/TencentDB-Agent-Memory" {
		t.Fatalf("select events = %v, want bubbling input/change from explicit baseline select", events)
	}
}

// 视口坐标出口的**对称性**: click/hover/wheel 早就有免 selector 的视口版, 双击与拖拽却只有
// 需要 selector 的 dblclickat/dragat —— canvas 类 UI 上元素不进 a11y 树时那两个根本用不了,
// 于是"双击图元改名""从连接点拖出一条线"这两类核心手势无法用真实鼠标驱动。
// (2026-08-20 实测于 deepwork-teamworkbench album 画布, 合成 pointer 事件驱不动 vue-flow 连线握手。)
func TestTypedViewportDoubleClickAndDrag(t *testing.T) {
	dbl, err := (Action{Verb: "click", Count: 2, Target: fixtureViewport(927, 437)}).executionRequest()
	if err != nil {
		t.Fatalf("viewport double click should validate: %v", err)
	}
	if dbl.Op != "dblclickxy" || dbl.Ref != "" || dbl.CoordX != 927 || dbl.CoordY != 437 {
		t.Fatalf("unexpected double click request: %+v", dbl)
	}
	drag, err := (Action{Verb: "drag", Target: fixtureViewport(100, 200), End: &Point{X: 300, Y: 400, Unit: "css-px", Frame: "viewport", ObservationID: "fixture"}}).executionRequest()
	if err != nil {
		t.Fatalf("viewport drag should validate: %v", err)
	}
	if drag.Op != "dragxy" || drag.Ref != "" {
		t.Fatalf("unexpected drag op: %+v", drag)
	}
	if drag.CoordX != 100 || drag.CoordY != 200 || drag.CoordX2 != 300 || drag.CoordY2 != 400 {
		t.Fatalf("unexpected drag coords: %+v", drag)
	}
	if err := (Action{Verb: "drag", Target: fixtureViewport(100, 200)}).Validate(); err == nil {
		t.Fatal("drag with a single point should fail loudly")
	}
	a, err := (Action{Verb: "drag", Target: fixtureRelative("css=#chart", 20, 50), End: fixtureEnd(80, 50)}).executionRequest()
	if err != nil || a.Op != "dragat" || a.Ref == "" {
		t.Fatalf("relative drag must stay intact: %+v err=%v", a, err)
	}
}

// 新动词必须被算作"会派发真实输入" —— 否则不抢前台/输入临界区, 事件可能发到后台 target。
func TestViewportGestureOpsDispatchInput(t *testing.T) {
	for _, op := range []string{"dblclickxy", "dragxy"} {
		if !inputDispatchOps[op] {
			t.Fatalf("op %q must be in inputDispatchOps", op)
		}
		if !IsMutatingOp(op) {
			t.Fatalf("op %q must be classified as a write", op)
		}
	}
}
