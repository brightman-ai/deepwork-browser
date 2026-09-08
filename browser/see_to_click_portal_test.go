package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// servePortalOverlayFixture 托管 tests/portal-overlay-fixture/index.html:
// 一个 body 级 portal —— pointer-events:none 的罩层套 pointer-events:auto 的面板,
// 面板里既有真能点的控件, 也有一组必须落选的阴性对照。
func servePortalOverlayFixture(t *testing.T) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "tests", "portal-overlay-fixture", "index.html"))
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

// TestPortalPanelUnderPointerEventsNoneScrimStaysAddressable 钉死一个把整棵抽屉
// 判死的可见性缺陷。
//
// 现场症状 (deepwork-terminal 文件面板, 2026-09-08): 抽屉里 33 个控件一个 @rN 都
// 拿不到, 全被计入 offscreen。Witness 于是只能靠坐标点, "点了没反应"再也分不清是
// 产品坏了还是自己点偏了 —— 一整条旅程的验收就此失明。
//
// 根因: 可见性探针沿**祖先链**判 pointer-events / visibility。这两个都是**继承**
// 属性, 子元素可以把祖先的 none/hidden 重新打开, 而
// `.rd-scrim{pointer-events:none}` 套 `.rd-panel{pointer-events:auto}` 正是浮层
// 的标准写法。祖先链判定把一整棵真能点的子树判成不可见。
//
// 契约: 继承属性只看元素自身的**计算值**(它已经是继承 + 覆盖后的最终值), 而
// display / opacity / inert / aria-hidden 这些子树撤不掉的仍走祖先链。
func TestPortalPanelUnderPointerEventsNoneScrimStaysAddressable(t *testing.T) {
	requireChromeForPool(t)
	srv := servePortalOverlayFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	core, err := NewBrowserCore(ctx, fmt.Sprintf("portal-overlay-%d", time.Now().UnixNano()), WithMode(ModeHeadless))
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
	visible := make(map[string]bool, len(snap.Refs))
	for i := range snap.Refs {
		if snap.Refs[i].TestID != "" {
			visible[snap.Refs[i].TestID] = true
		}
	}
	seen := make([]string, 0, len(visible))
	for id := range visible {
		seen = append(seen, id)
	}
	sort.Strings(seen)
	inventory := strings.Join(seen, ", ")

	// 面板里的控件必须拿到句柄 —— 它们和页面上那个自由按钮一样, 人真的点得到。
	for _, testID := range []string{
		"page-button",
		"panel-search",
		"panel-row",
		"panel-fullscreen",
		"unveiled-button", // 祖先 visibility:hidden, 自己撤回 visible → 浏览器照样渲染
	} {
		if !visible[testID] {
			t.Errorf("%q is missing from the visible set; observe saw only: %s", testID, inventory)
		}
	}

	// 阴性对照: 修好的是判据, 不是把判据删了。
	for _, tc := range []struct{ testID, why string }{
		{"ghost-button", "pointer-events:none 未被子树收回, 真的点不到"},
		{"faded-button", "祖先 opacity:0, 子元素撤不掉"},
		{"aria-hidden-button", "祖先 aria-hidden=true"},
		{"inert-button", "祖先 inert"},
	} {
		if visible[tc.testID] {
			t.Errorf("%q must stay out of the visible set (%s), but observe minted a ref for it", tc.testID, tc.why)
		}
	}

	if snap.VisibleInteractableCount < 5 {
		t.Errorf("visible=%d offscreen=%d — the portal subtree is still being swallowed: %s",
			snap.VisibleInteractableCount, snap.OffscreenInteractableCount, inventory)
	}
}
