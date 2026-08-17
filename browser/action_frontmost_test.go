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

// 后台 target 会静默吞掉指针输入:CDP 把 Input.dispatchMouseEvent 写进连接了,
// 渲染器却永远不 ack。v0.11.3 只在滚轮路径上修了这一处;这里验证修复已经上提到
// 所有输入动作的公共入口 —— 而且"发生过前台切换"这件事被如实上报,不是闷声修好。

func serveFrontmostFixture(t *testing.T) *httptest.Server {
	t.Helper()
	const page = `<!doctype html><html><head><meta charset="utf-8"><title>frontmost</title>
<style>body{margin:0;font:16px sans-serif}
#btn{position:absolute;left:40px;top:60px;width:200px;height:60px}
#hoverme{position:absolute;left:40px;top:160px;width:200px;height:60px;background:#eee}
#hoverme.hot{background:#0a0}</style></head>
<body>
<button id="btn" onclick="document.title='clicked'">按我</button>
<div id="hoverme" onmouseover="this.className='hot';document.title='hovered'">悬停我</div>
</body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// openFrontmostSession 开一个会话并把它推到后台:再造一个 page target,新 target
// 成为浏览器前台,原来的会话 target 就落到后台 —— 这正是 mux-host 多 tab 下
// 另一个操作者开一页时,你这一页的处境。
func openFrontmostSession(t *testing.T, backgrounded bool) (SessionCore, *browserCoreImpl, context.Context) {
	t.Helper()
	requireChromeForPool(t)
	srv := serveFrontmostFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	core, err := NewBrowserCore(ctx, fmt.Sprintf("frontmost-%d", time.Now().UnixNano()), WithMode(ModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close(context.Background()) })
	core.SetPolicy(SessionPolicy{RemoteWrites: RemoteWriteDeny}, srv.URL)
	core.(ScenarioInteractionCapable).SetInteractionScenario(ScenarioAppTestExplore)
	if _, err := core.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	impl := core.(*browserCoreImpl)

	if backgrounded {
		if _, err := targetGraphCreatePage(impl.browserCtx, "about:blank", 15*time.Second); err != nil {
			t.Fatalf("create second page target: %v", err)
		}
		// 别猜,验证前提确实成立:探针说我们已经不在前台了,后面的断言才有意义。
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if !targetLooksFrontmost(impl.currentCtx()) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if targetLooksFrontmost(impl.currentCtx()) {
			t.Skip("Environment Gate: 本机 Chrome 新建 target 后原 target 仍报 visible — 造不出后台 target 前提")
		}
	}

	session := core.(SessionCore)
	if _, err := session.SnapWithSessionMode(ctx, 1); err != nil {
		t.Fatal(err)
	}
	return session, impl, ctx
}

// click 落在后台 target 上时:必须成功,而且必须带 brought_to_front 标记。
// 标记不是装饰 —— 它是"这次派发之前前台归属被动过"的一手证据,没有它,
// 共享浏览器下的互扰只能靠猜。
func TestClickOnBackgroundTargetSucceedsAndReportsBroughtToFront(t *testing.T) {
	session, impl, ctx := openFrontmostSession(t, true)

	started := time.Now()
	if _, err := session.ActWithSessionMode(ctx, "click 140,90", false); err != nil {
		t.Fatalf("后台 target 上 click 失败: %v", err)
	}
	elapsed := time.Since(started)

	report := impl.LastActionFidelity()
	if !report.BroughtToFront {
		t.Fatal("后台 target 上派发 click 却没有 brought_to_front —— 要么没提前台(那会挂), 要么提了不报(证据丢失)")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("click 用了 %s —— 后台 target 的输入仍在挂", elapsed)
	}

	var title string
	if err := impl.EvalJS(ctx, "document.title", &title); err != nil {
		t.Fatal(err)
	}
	if title != "clicked" {
		t.Fatalf("document.title=%q, want clicked —— 输入没有真的落到页面上", title)
	}
}

// hover 是这次上提最直接的受益者:v0.11.3 只有滚轮路径调了 ensureTargetFrontmost,
// hover 在后台 target 上要挂满 5s 的 ack 预算才返回。上提之后它应当和前台一样快。
func TestHoverOnBackgroundTargetNoLongerHangs(t *testing.T) {
	session, impl, ctx := openFrontmostSession(t, true)

	started := time.Now()
	if _, err := session.ActWithSessionMode(ctx, "hoverat 140,190", false); err != nil {
		t.Fatalf("后台 target 上 hover 失败: %v", err)
	}
	elapsed := time.Since(started)

	if !impl.LastActionFidelity().BroughtToFront {
		t.Fatal("hover 没有走前台确保 —— 上提没有覆盖到 hover")
	}
	// 修复前的症状是挂满 5s 才返回。留足余量但守住量级差别。
	if elapsed > 4*time.Second {
		t.Fatalf("hover 用了 %s —— 后台 target 上仍在挂(修复前症状是 5s)", elapsed)
	}

	var title string
	if err := impl.EvalJS(ctx, "document.title", &title); err != nil {
		t.Fatal(err)
	}
	if title != "hovered" {
		t.Fatalf("document.title=%q, want hovered —— hover 事件没有真的送达", title)
	}
}

// 反面同样重要:已经在前台就不该再报 brought_to_front。这个字段一旦见谁都亮,
// 它就不再是证据了 —— 共享浏览器下的互扰恰恰要靠"它什么时候亮"来分辨。
//
// 用"第二次动作"而不是"第一次动作"作判据:一个刚起的 Chrome 里谁是前台由启动
// 时序决定(实测 headless chromium 的初始 about:blank 常常压在会话 target 前面),
// 拿它当前提是在赌环境。第一次动作之后前台归属是我们自己刚刚确立的事实,
// 第二次还报切换就一定是 bug。
func TestSecondActionOnAlreadyFrontTargetDoesNotReportBroughtToFront(t *testing.T) {
	session, impl, ctx := openFrontmostSession(t, false)

	if _, err := session.ActWithSessionMode(ctx, "click 140,90", false); err != nil {
		t.Fatalf("第一次 click 失败: %v", err)
	}
	if _, err := session.ActWithSessionMode(ctx, "click 140,90", false); err != nil {
		t.Fatalf("第二次 click 失败: %v", err)
	}
	if impl.LastActionFidelity().BroughtToFront {
		t.Fatal("target 已经在前台(上一次动作刚提上来)却仍报 brought_to_front —— 这个字段就没有信息量了")
	}
}

// 前台确保收口在动作派发的公共入口:所有真实输入动词都必须经过它,
// 不派发输入的动词不该为它抢浏览器级临界区。
func TestInputDispatchOpsCoverEveryRealInputVerb(t *testing.T) {
	mustDispatch := []string{
		"click", "clickxy", "clickat", "dblclickat", "rclickat",
		"tap", "tapat", "tapxy", "hover", "hoverat",
		"wheelat", "scroll", "dragat", "swipeat",
		"press", "fill", "fillsecret", "type", "typetext", "select",
		"check", "uncheck",
	}
	for _, op := range mustDispatch {
		if !inputDispatchOps[op] {
			t.Errorf("输入动词 %q 不在输入派发窗口里 —— 它在后台 target 上会静默挂死", op)
		}
	}
	// 导航/纯读类动作不碰指针键盘,不该占浏览器级输入临界区。
	for _, op := range []string{"back", "forward", "scrollinto", "scrollto", "zoom", "keyboard"} {
		if inputDispatchOps[op] {
			t.Errorf("非输入动词 %q 也在抢输入临界区 —— 会无谓地阻塞共享浏览器上的别人", op)
		}
	}
}

// 上提之后滚轮路径不该再各自调一次 bringToFront:单一调用点是这次重构的重点,
// 双调会让同一次动作发两条 Page.bringToFront,也让"发生了切换"这个事实变成两次。
func TestWheelPathsNoLongerCallEnsureTargetFrontmostThemselves(t *testing.T) {
	source := readPackageSource(t, "action_engine.go")
	calls := strings.Count(source, "ensureTargetFrontmost(ctx)")
	if calls != 1 {
		t.Fatalf("ensureTargetFrontmost(ctx) 在 action_engine.go 里出现 %d 次, want 1(公共入口单一调用点)", calls)
	}
}
