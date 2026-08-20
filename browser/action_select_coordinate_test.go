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
	options := []nativeSelectOption{
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

func TestParseActionAbsoluteHoverAndWheel(t *testing.T) {
	hover, err := ParseAction("hoverat 414,314")
	if err != nil {
		t.Fatalf("ParseAction hoverat: %v", err)
	}
	if hover.Op != "hoverat" || hover.Ref != "" || hover.CoordX != 414 || hover.CoordY != 314 {
		t.Fatalf("hoverat parsed as %+v", hover)
	}

	wheel, err := ParseAction("wheelat 1200,800 down 6")
	if err != nil {
		t.Fatalf("ParseAction wheelat: %v", err)
	}
	if wheel.Op != "wheelat" || wheel.Ref != "" || wheel.CoordX != 1200 || wheel.CoordY != 800 {
		t.Fatalf("wheelat parsed as %+v", wheel)
	}
	if wheel.Direction != "down" || wheel.Steps != 6 || wheel.DeltaY != humanWheelStepCSSPixels || wheel.DeltaX != 0 {
		t.Fatalf("wheelat direction/delta = %+v", wheel)
	}

	legacy, err := ParseAction("wheelat css=#chart 50% 50% -240")
	if err != nil {
		t.Fatalf("ParseAction selector-relative wheelat: %v", err)
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

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	core, err := NewBrowserCore(ctx, fmt.Sprintf("select-coordinate-%d", time.Now().UnixNano()), WithMode(BrowserModeHeadless))
	if err != nil {
		t.Fatalf("NewBrowserCore: %v", err)
	}
	defer func() { _ = core.Close(context.Background()) }()
	core.SetPolicy(SessionPolicy{RemoteWrites: RemoteWriteAllow}, "")

	if _, err := core.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if _, err := core.Snap(ctx); err != nil {
		t.Fatalf("Snap: %v", err)
	}

	assertWithin := func(action string, wantErr string, max time.Duration) {
		t.Helper()
		started := time.Now()
		_, actErr := core.Act(ctx, action, false)
		elapsed := time.Since(started)
		t.Logf("act %q elapsed=%s err=%v", action, elapsed, actErr)
		if elapsed > max {
			t.Fatalf("%s took %s, want <= %s", action, elapsed, max)
		}
		if wantErr == "" && actErr != nil {
			t.Fatalf("%s: %v", action, actErr)
		}
		if wantErr != "" && (actErr == nil || !strings.Contains(actErr.Error(), wantErr)) {
			t.Fatalf("%s error = %v, want substring %q", action, actErr, wantErr)
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

	assertWithin("focus button:'Overview'", "", 2*time.Second)
	assertWithin("focus combobox:'Project'", "", 2*time.Second)
	assertFocused("#project")
	assertWithin("focus button:'Overview'", "", 2*time.Second)
	assertWithin("press combobox:'Project' ArrowDown", "", 2*time.Second)
	assertFocused("#project")
	assertValue("/home/ubuntu/code/stwork/TencentDB-Agent-Memory")
	assertWithin("press combobox:'Project' Enter", "blocked to avoid opening the browser picker", 2*time.Second)

	if err := core.EvalJS(ctx, `(() => { document.querySelector('#project').selectedIndex = 0; return true; })()`, new(bool)); err != nil {
		t.Fatalf("reset select: %v", err)
	}
	assertWithin("select combobox:'Project' 'TencentDB-Agent-Memory'", "", 2*time.Second)
	assertValue("/home/ubuntu/code/stwork/TencentDB-Agent-Memory")
	assertWithin("select combobox:'Project' 'gamma'", "", 2*time.Second)
	assertValue("gamma")
	assertWithin("select combobox:'Project' 'Gamma'", "", 2*time.Second)
	assertValue("gamma")
	assertWithin("select combobox:'Project' 'Al'", `"Alpha Pipeline"→"alpha"`, 2*time.Second)
	assertWithin("select combobox:'Project' 'Missing'", `"TencentDB-Agent-Memory"→"/home/ubuntu/code/stwork/TencentDB-Agent-Memory"`, 2*time.Second)

	var events []string
	if err := core.EvalJS(ctx, `window.actionEvents`, &events); err != nil {
		t.Fatalf("read select events: %v", err)
	}
	if len(events) < 2 || events[0] != "input:/home/ubuntu/code/stwork/TencentDB-Agent-Memory" || events[1] != "change:/home/ubuntu/code/stwork/TencentDB-Agent-Memory" {
		t.Fatalf("select events = %v, want bubbling input/change from the first ArrowDown", events)
	}
}

// 视口坐标出口的**对称性**: click/hover/wheel 早就有免 selector 的视口版, 双击与拖拽却只有
// 需要 selector 的 dblclickat/dragat —— canvas 类 UI 上元素不进 a11y 树时那两个根本用不了,
// 于是"双击图元改名""从连接点拖出一条线"这两类核心手势无法用真实鼠标驱动。
// (2026-08-20 实测于 deepwork-teamworkbench album 画布, 合成 pointer 事件驱不动 vue-flow 连线握手。)
func TestParseViewportDoubleClickAndDrag(t *testing.T) {
	dbl, err := ParseAction("dblclick 927,437")
	if err != nil {
		t.Fatalf("dblclick x,y should parse: %v", err)
	}
	if dbl.Op != "dblclickxy" || dbl.Ref != "" || dbl.CoordX != 927 || dbl.CoordY != 437 {
		t.Fatalf("unexpected dblclick parse: %+v", dbl)
	}

	drag, err := ParseAction("drag 100,200 300,400")
	if err != nil {
		t.Fatalf("drag x1,y1 x2,y2 should parse: %v", err)
	}
	if drag.Op != "dragxy" || drag.Ref != "" {
		t.Fatalf("unexpected drag op: %+v", drag)
	}
	if drag.CoordX != 100 || drag.CoordY != 200 || drag.CoordX2 != 300 || drag.CoordY2 != 400 {
		t.Fatalf("unexpected drag coords: %+v", drag)
	}

	// 缺第二个点必须报错, 不能静默当成元素内相对拖拽。
	if _, err := ParseAction("drag 100,200"); err == nil {
		t.Fatal("drag with a single point should fail loudly")
	}

	// 带 selector 的旧形式保持不变 (不是坐标对 → 不抢这条语法)。
	if a, err := ParseAction("dragat css=#chart 20% 50% 80% 50%"); err != nil || a.Op != "dragat" || a.Ref == "" {
		t.Fatalf("dragat with selector must stay intact: %+v err=%v", a, err)
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
