// Package browser — check/uncheck 勾选切换回归 (headless).
//
// 病根 (Coordinator 读码实锤): executeToggleCheckbox 对语义/element ref 走
// document.querySelector(ref) 兜底，永远查不到节点 → isChecked 恒 false →
//   - uncheck 永不点击 = 报 success 的 no-op 假阳 (生产 2 次复现)
//   - check 恒点击, 已勾选时误反转 (隐性双跳)
//
// 终局修复: 读真实 checked 走"与 executeClick 同源的节点解析路径"(dom.ResolveNode +
// runtime.CallFunctionOn), 失败/非可勾选控件 fail-loud.
//
// 环境门控: 无 Chrome 时 t.Skip (requireChromeForPool)。
// headless: PoolConfig{Mode: BrowserModeHeadless} (pool_v2_test.go 模板)。
package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// checkboxRegressionPage — 含 native checkbox(默认 checked) + 若干可交互元素
// (保证 a11y 快照成型, 语义 locator 可解析) + 一个非可勾选 <div>。
const checkboxRegressionPage = `<!DOCTYPE html>
<html lang="zh"><head><meta charset="utf-8"><title>勾选切换回归</title></head>
<body>
  <h1>勾选切换回归</h1>
  <nav aria-label="主导航">
    <a href="#a" aria-label="占位链接一">链接一</a>
    <a href="#b" aria-label="占位链接二">链接二</a>
  </nav>
  <main>
    <input id="cb" type="checkbox" aria-label="选我" checked>
    <button type="button" aria-label="占位按钮">占位按钮</button>
    <input type="text" aria-label="占位输入" placeholder="占位">
    <div id="notcheck" data-testid="notcheck" role="button" tabindex="0" aria-label="假框">我不是复选框</div>
  </main>
</body></html>`

func TestActionEngine_ToggleCheckbox_SemanticLocator_Regression(t *testing.T) {
	requireChromeForPool(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(checkboxRegressionPage))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool := NewBrowserPool(PoolConfig{DataDir: t.TempDir(), MaxTabs: 2, Mode: BrowserModeHeadless})
	defer pool.Shutdown(context.Background())

	handle, err := pool.AcquireTab(ctx, AcquireTabRequest{
		IdentityKey: pool.DefaultIdentity(),
		WorkspaceID: "checkbox-toggle-regression",
		Role:        RoleAgent,
	})
	if err != nil {
		t.Fatalf("AcquireTab: %v", err)
	}
	core, ok := pool.GetCore(handle.TargetID)
	if !ok {
		t.Fatalf("GetCore(%s) miss", handle.TargetID)
	}
	core.(ScenarioInteractionCapable).SetInteractionScenario(ScenarioAppTestBaseline)

	if _, err := core.Navigate(ctx, srv.URL); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// 前提: a11y 快照须暴露 checkbox ref, 否则语义 locator 无从解析。
	snap, err := core.Snap(ctx)
	if err != nil {
		t.Fatalf("Snap: %v", err)
	}
	if !hasRoleRef(snap, "checkbox") {
		t.Fatalf("a11y 快照未暴露 checkbox ref (type=%s refs=%d) — 语义 locator 前提不满足",
			snap.SnapshotType, len(snap.Refs))
	}
	impl := core.(*browserCoreImpl)
	entry := pool.entries[handle.IdentityKey]
	wantInputKey := BrowserInputKey("", entry.execChromePID)
	if entry.chromeHandle != nil {
		wantInputKey = BrowserInputKey(entry.chromeHandle.WSURL(), entry.chromeHandle.PID())
	}
	requireRuntimeInputBinding(t, core, snap, wantInputKey)
	if impl.chromePID != 0 {
		t.Fatal("borrowed pooled input identity transferred process kill ownership")
	}

	// waitChecked 轮询断言 #cb.checked 终态 (对齐 Postcondition-First, 抗异步 settle)。
	waitChecked := func(want bool, phase string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		var last bool
		for time.Now().Before(deadline) {
			if err := core.EvalJS(ctx, `document.querySelector('#cb').checked === true`, &last); err != nil {
				t.Fatalf("%s: EvalJS read checked: %v", phase, err)
			}
			if last == want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("%s: #cb.checked = %v, want %v", phase, last, want)
	}

	// snapThenAct: 每次语义 act 前 Snap 刷新 refTable (observe→act 纪律)。
	snapThenAct := func(action Action) error {
		if _, err := core.Snap(ctx); err != nil {
			return err
		}
		_, err := executeFixtureAction(ctx, core, action, false, false)
		if err != nil {
			t.Logf("checkbox action %+v fidelity=%+v err=%v", action, core.(ActionFidelityCapable).LastActionFidelity(), err)
		}
		return err
	}

	// 前置态: #cb 初始 checked=true。
	waitChecked(true, "precondition")

	// Phase A — uncheck 语义 locator (生产 no-op 假阳核心): 修前 checked 仍 true → RED。
	if err := snapThenAct(Action{Verb: "set-checked", Target: fixtureLocator("role=checkbox[name*=\"选我\"]"), Checked: fixtureChecked(false)}); err != nil {
		t.Fatalf("Phase A act uncheck: %v", err)
	}
	waitChecked(false, "Phase A: uncheck checkbox:'选我'")

	// Phase B — check 语义 locator → true。
	if err := snapThenAct(Action{Verb: "set-checked", Target: fixtureLocator("role=checkbox[name*=\"选我\"]"), Checked: fixtureChecked(true)}); err != nil {
		t.Fatalf("Phase B act check: %v", err)
	}
	waitChecked(true, "Phase B: check checkbox:'选我'")

	// Phase C — 已 checked 再 check 保持 true (防双跳): 修前 check 恒点击→翻回 false → RED。
	if err := snapThenAct(Action{Verb: "set-checked", Target: fixtureLocator("role=checkbox[name*=\"选我\"]"), Checked: fixtureChecked(true)}); err != nil {
		t.Fatalf("Phase C act check(idempotent): %v", err)
	}
	waitChecked(true, "Phase C: idempotent check (double-jump guard)")

	// Phase D — 对非可勾选 <div> uncheck 须返回"不是可勾选控件"错误, 而非静默 no-op 冒充成功。
	// 修前: CSS 路径 querySelector('div').checked===true 为 false, isChecked==wantChecked(false) → 返回 nil 假成功 → RED。
	if _, err := core.Snap(ctx); err != nil {
		t.Fatalf("Phase D fresh capture: %v", err)
	}
	_, derr := executeFixtureAction(ctx, core, Action{Verb: "set-checked", Target: fixtureLocator("css=[data-testid=\"notcheck\"]"), Checked: fixtureChecked(false)}, false, false)
	if derr == nil {
		t.Fatal("Phase D: uncheck 非可勾选 <div> 应返回错误, 而非静默成功")
	}
	if !strings.Contains(derr.Error(), "可勾选") {
		t.Fatalf("Phase D: 期望'不是可勾选控件'错误, 实际: %v", derr)
	}

	// Phase E — press Space toggle 已聚焦 checkbox: 修前发字面词 "Space" 不 toggle → RED。
	// Acquire real focus before resetting state so the click's toggle cannot
	// substitute for the Space event being tested below.
	if err := snapThenAct(Action{Verb: "focus", Target: fixtureLocator("role=checkbox[name*=\"选我\"]")}); err != nil {
		t.Fatalf("Phase E native focus: %v", err)
	}
	focusReport := core.(ActionFidelityCapable).LastActionFidelity()
	if !focusReport.Dispatched || focusReport.Synthetic || !focusReport.FocusUpdated || focusReport.FocusedBackend == 0 {
		t.Fatalf("Phase E native focus proof missing: %+v", focusReport)
	}
	var checkboxFocused bool
	if err := core.EvalJS(ctx, `document.activeElement === document.querySelector('#cb')`, &checkboxFocused); err != nil || !checkboxFocused {
		t.Fatalf("Phase E native focus not confirmed by page: focused=%t err=%v", checkboxFocused, err)
	}
	t.Logf("Phase E verified native focus fidelity=%+v", focusReport)
	if err := core.EvalJS(ctx, `(() => { document.querySelector('#cb').checked = true; return true; })()`, new(bool)); err != nil {
		t.Fatalf("Phase E reset checked: %v", err)
	}
	waitChecked(true, "Phase E: reset checked")
	if err := snapThenAct(Action{Verb: "press", Target: fixtureLocator("role=checkbox[name*=\"选我\"]"), Key: "Space"}); err != nil {
		t.Fatalf("Phase E act press Space: %v", err)
	}
	waitChecked(false, "Phase E: press Space toggles focused checkbox")

	t.Log("PASS: uncheck(no-op修复) / check / idempotent(双跳防护) / div-error(fail-loud) / press-Space 全绿")
}

// hasRoleRef 判定快照 refs 是否含指定 ARIA role 的元素。
func hasRoleRef(snap *Snapshot, role string) bool {
	if snap == nil {
		return false
	}
	for _, r := range snap.Refs {
		if r.Role == role {
			return true
		}
	}
	return false
}
