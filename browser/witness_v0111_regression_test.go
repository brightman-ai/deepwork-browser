package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================
// § v0.11.1 Witness 症状回归
//   S6  坐标动作静默假成功 → 命中回报
//   S2  一次失败 act 作废整份 observation → 校验期失败不作废
//   S3  "当前页面无 button 元素" 事实错误 → 三分法如实报
//   S10 observe 前被遮挡者静默剔除 → 计数 + 可审计
// ============================================================

// serveCoordinateHitFixture 托管 tests/coordinate-hit-fixture/index.html:
// 一个自由按钮 + 一个中心被 div 完全盖住的按钮。
func serveCoordinateHitFixture(t *testing.T) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "tests", "coordinate-hit-fixture", "index.html"))
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

func openCoordinateHitSession(t *testing.T, name string) (SessionCore, *httptest.Server, context.Context) {
	t.Helper()
	requireChromeForPool(t)
	srv := serveCoordinateHitFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	core, err := NewBrowserCore(ctx, fmt.Sprintf("%s-%d", name, time.Now().UnixNano()), WithMode(ModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close(context.Background()) })
	core.SetPolicy(SessionPolicy{RemoteWrites: RemoteWriteDeny}, srv.URL)
	core.(ScenarioInteractionCapable).SetInteractionScenario(ScenarioAppTestExplore)
	if _, err := core.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	return core.(SessionCore), srv, ctx
}

// [S6] 坐标点击必须回报"这一像素实际归谁"。
// 现场症状: 意图点导航、实际点掉遮罩、hash 纹丝未动, 工具回报 success:true
// 且输出里没有任何命中字段 —— 一个只看 exit code 的验收脚本会稳定假阳。
// 契约: 不拦截(人类同样点得到遮罩), 但必须如实回报, 且回报要与页面事实一致。
func TestCoordinateClickReportsWhoActuallyReceivedTheInput(t *testing.T) {
	session, _, ctx := openCoordinateHitSession(t, "coord-hit")
	core := session.(BrowserCore)

	if _, err := session.SnapWithSessionMode(ctx, 1); err != nil {
		t.Fatal(err)
	}

	// (a) 点在被遮罩盖住的按钮中心 → 命中的是遮罩, 不是按钮。
	if _, err := session.ActWithSessionMode(ctx, "click 120,230", false); err != nil {
		t.Fatalf("click 120,230: %v", err)
	}
	hit := core.(ActionFidelityCapable).LastActionFidelity().Hit
	if hit == nil {
		t.Fatal("坐标点击没有回报 hit —— 这正是 S6 的静默假成功")
	}
	if hit.Selector != "#cover" {
		t.Fatalf("hit=%+v, want selector #cover (遮罩)", hit)
	}
	// 工具自述必须与页面事实对得上, 否则回报本身也不可信。
	var actuallyClicked string
	if err := core.EvalJS(ctx, "window.__lastClickedID", &actuallyClicked); err != nil {
		t.Fatalf("EvalJS: %v", err)
	}
	if actuallyClicked != "cover" {
		t.Fatalf("页面实际收到点击的是 %q, 工具却回报 %+v", actuallyClicked, hit)
	}

	// (b) 正对照: 点在自由按钮上 → 回报的就是该按钮本身。
	if _, err := session.ActWithSessionMode(ctx, "click 120,50", false); err != nil {
		t.Fatalf("click 120,50: %v", err)
	}
	hit = core.(ActionFidelityCapable).LastActionFidelity().Hit
	if hit == nil || hit.Selector != "#free" {
		t.Fatalf("free-target hit=%+v, want selector #free", hit)
	}
	if hit.Role != "button" || hit.Name != "Free target" {
		t.Fatalf("hit 缺少可断言的语义身份: %+v", hit)
	}
	if err := core.EvalJS(ctx, "window.__lastClickedID", &actuallyClicked); err != nil {
		t.Fatalf("EvalJS: %v", err)
	}
	if actuallyClicked != "free" {
		t.Fatalf("页面实际收到点击的是 %q, 工具却回报 %+v", actuallyClicked, hit)
	}
}

// [S6] hoverat / tapxy 与 click x,y 走同一条命中回报路径 —— 整个坐标动作族
// 都必须回报, 不能只修一个入口。
func TestCoordinateHoverAndTapXYAlsoReportHit(t *testing.T) {
	session, _, ctx := openCoordinateHitSession(t, "coord-hit-more")
	core := session.(BrowserCore)
	if _, err := session.SnapWithSessionMode(ctx, 1); err != nil {
		t.Fatal(err)
	}

	var viewportW, viewportH float64
	if err := core.EvalJS(ctx, "window.innerWidth", &viewportW); err != nil {
		t.Fatal(err)
	}
	if err := core.EvalJS(ctx, "window.innerHeight", &viewportH); err != nil {
		t.Fatal(err)
	}
	if viewportW <= 0 || viewportH <= 0 {
		t.Fatalf("viewport %.0fx%.0f", viewportW, viewportH)
	}

	for _, tc := range []struct{ action, wantSelector string }{
		// 悬停在被盖住的按钮中心 → 命中遮罩
		{action: "hoverat 120,230", wantSelector: "#cover"},
		// 比例坐标点击自由按钮 → 命中按钮本体
		{action: fmt.Sprintf("tapxy %.6f %.6f", 120/viewportW, 50/viewportH), wantSelector: "#free"},
	} {
		if _, err := session.ActWithSessionMode(ctx, tc.action, false); err != nil {
			t.Fatalf("%s: %v", tc.action, err)
		}
		hit := core.(ActionFidelityCapable).LastActionFidelity().Hit
		if hit == nil || hit.Selector != tc.wantSelector {
			t.Fatalf("%s hit=%+v, want selector %s", tc.action, hit, tc.wantSelector)
		}
	}
}

// [S10] observe 时中心即被遮挡的可交互元素: 过去被静默剔除, 既不计数、
// 不进 hit_audit、报错也不点名遮挡者 —— "元素消失"在 屏外/被遮挡/未渲染
// 之间不可区分。契约: 计数 + 可审计 + 点名遮挡者, 但不发句柄。
func TestOccludedInteractablesAreCountedAndAuditableWithoutMintingRefs(t *testing.T) {
	session, _, ctx := openCoordinateHitSession(t, "occluded-census")
	core := session.(BrowserCore)

	snap, err := session.SnapWithSessionMode(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := range snap.Refs {
		if snap.Refs[i].TestID == "covered" {
			t.Fatalf("中心被完全遮挡的元素混进了可见集: %+v", snap.Refs[i])
		}
	}

	census, ok := core.(OcclusionCensusCapable)
	if !ok {
		t.Fatal("chrome core 未实现 OcclusionCensusCapable")
	}
	if got := census.OccludedInteractableCount(); got != 1 {
		t.Fatalf("occluded=%d, want 1 (#covered)", got)
	}

	findings, err := census.AuditOccludedInteractables(ctx)
	if err != nil {
		t.Fatalf("AuditOccludedInteractables: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("occluded findings=%+v, want exactly one", findings)
	}
	f := findings[0]
	if f.Ref != "" {
		t.Fatalf("审计给点不到的元素铸了句柄 %q: %+v", f.Ref, f)
	}
	if f.Scope != "occluded" {
		t.Fatalf("scope=%q, want occluded", f.Scope)
	}
	if !strings.Contains(f.OccludedBy, "cover") {
		t.Fatalf("occluded_by=%q, 未点名遮挡者 #cover", f.OccludedBy)
	}
	if f.Name != "Covered target" {
		t.Fatalf("finding 认错了元素: %+v", f)
	}
	if f.BBox == nil || f.BBox.Width <= 0 {
		t.Fatalf("finding 缺 bbox, 无法定位缺陷位置: %+v", f)
	}
}

// [S2] 校验期失败(定位器解析不出来)绝不能作废整份 observation。
// 现场症状: 一次打错的定位器之后, 刚拿到的 @rN 全部失效, 必须重新 observe。
func TestValidationFailureIsMarkedPreDispatchAndKeepsRefsUsable(t *testing.T) {
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
	impl.RestoreRefsFromSession(SessionRefsFromSnapshot(snap, true))

	// fill 一个不存在的 ref: 解析期就失败, 一个字节的输入都没发出去。
	_, err := actEngine.ExecuteWithInteractionMode(context.Background(),
		"fill @r99 'hello'", false, true, InteractionModeVisual)
	if err == nil {
		t.Fatal("fill @r99 unexpectedly succeeded")
	}
	report := actEngine.lastActionFidelity()
	if report.Dispatched {
		t.Fatalf("解析失败却被记成'已派发': %+v (err=%v)", report, err)
	}

	// 原有 @rN 必须照常可用。
	if _, err := actEngine.resolveSemanticSelectorForMode("@r1", true, false); err != nil {
		t.Fatalf("失败的 act 之后 @r1 也不能用了: %v", err)
	}

	// 解析错、无匹配的语义定位器同样是校验期失败。
	for _, action := range []string{"click", "click button:'不存在的东西xyz'"} {
		_, err := actEngine.ExecuteWithInteractionMode(context.Background(), action, false, true, InteractionModeVisual)
		if err == nil {
			t.Fatalf("%q unexpectedly succeeded", action)
		}
		if actEngine.lastActionFidelity().Dispatched {
			t.Fatalf("%q 是校验期失败, 却被记成已派发", action)
		}
	}
	if _, err := actEngine.resolveSemanticSelectorForMode("@r1", true, false); err != nil {
		t.Fatalf("连续校验期失败之后 @r1 失效: %v", err)
	}
}

// [S3] 定位失败的三种成因必须分开说, 且绝不代页面断言。
// 现场症状: DOM 里有 427 个 button, 工具却报 "当前页面无 button 元素"。
func TestLocatorMissDistinguishesMissingObservationFromNoMatch(t *testing.T) {
	newEngine := func(refs []ElementRef) *actionEngine {
		snapEngine := newSnapshotEngine()
		actEngine := newActionEngine(snapEngine)
		impl := &browserCoreImpl{snapEngine: snapEngine, actEngine: actEngine}
		impl.SetInteractionScenario(ScenarioAppTestExplore)
		impl.RestoreRefsFromSession(SessionRefsFromSnapshot(&Snapshot{SeeToClick: true, Refs: refs}, true))
		return actEngine
	}
	visible := func(ref, role, name string) ElementRef {
		return ElementRef{
			Ref: ref, BackendNodeID: int64(len(ref) + len(name)), Role: role, NameFull: name, Name: name,
			BBox: Rect{X: 1, Y: 1, Width: 10, Height: 10}, VisibilityKnown: true, VisibleInViewport: true,
		}
	}

	// (a) 完全没有 observation → 引导 observe, 不得代页面断言。
	_, err := newEngine(nil).resolveSemanticSelectorForMode("button:'记分板'", true, false)
	if err == nil {
		t.Fatal("empty observation resolved a locator")
	}
	if strings.Contains(err.Error(), "当前页面无") {
		t.Fatalf("仍在代页面断言: %v", err)
	}
	for _, want := range []string{"没有有效的", "observe"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("缺少 %q 的引导: %v", want, err)
		}
	}

	// (b) observation 有效但没有该 role → 报可见集的 role 分布, 措辞限定在可见集。
	_, err = newEngine([]ElementRef{visible("@r1", "link", "首页"), visible("@r2", "link", "设置")}).
		resolveSemanticSelectorForMode("button:'记分板'", true, false)
	if err == nil {
		t.Fatal("role-absent locator resolved")
	}
	if strings.Contains(err.Error(), "当前页面无") {
		t.Fatalf("仍在代页面断言: %v", err)
	}
	for _, want := range []string{"可见集", "link 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("缺少 %q: %v", want, err)
		}
	}

	// (c) role 在、name 不匹配 → 给最近似候选, 逐条带 @rN, 不是一坨。
	_, err = newEngine([]ElementRef{
		visible("@r1", "button", "打开项目记忆库 →"),
		visible("@r2", "button", "打开管线对比 →"),
		visible("@r3", "button", "完全无关的东西"),
	}).resolveSemanticSelectorForMode("button:'不在清单里的名字xyz'", true, false)
	if err == nil {
		t.Fatal("name-mismatch locator resolved")
	}
	if !strings.Contains(err.Error(), "最近似候选") {
		t.Fatalf("没有给候选: %v", err)
	}
	if !strings.Contains(err.Error(), "@r1 button '打开项目记忆库 →'") {
		t.Fatalf("候选不可读/未逐条列出: %v", err)
	}
	if lines := strings.Count(err.Error(), "\n  @r"); lines != 3 {
		t.Fatalf("候选应逐行列出 3 条, 实际 %d 行: %v", lines, err)
	}
}

// [S3] 最近似候选必须真的按相似度排序, 否则"给候选"只是换个方式糊。
func TestLocatorCandidateRankingPutsClosestFirst(t *testing.T) {
	ranked := rankLocatorCandidates([]locatorCandidate{
		{Ref: "@r1", Role: "button", Name: "完全无关"},
		{Ref: "@r2", Role: "button", Name: "打开管线对比 →"},
		{Ref: "@r3", Role: "button", Name: "打开项目记忆库 →"},
	}, "打开项目记忆库", 2)
	if len(ranked) != 2 {
		t.Fatalf("ranked=%+v, want 2", ranked)
	}
	if ranked[0].Ref != "@r3" {
		t.Fatalf("最近似的不是第一个: %+v", ranked)
	}
}
