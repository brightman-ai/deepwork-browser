// browser chrome 仿真在 browserCoreImpl 上的接线（spec: docs/product/browser-chrome/）。
// 三个消费者共享同一 SSOT（FingerprintPreset.BrowserChrome）：
//   1. Screenshot 合成（browser_core_impl.go Screenshot 钩子 → CompositeBrowserChrome）
//   2. act 遮挡守卫（chromePointerGuard → actionEngine.pointerGuard）
//   3. observe/audit 机读块（BrowserChromeState 供 CLI 取几何）
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

// BrowserChromeCapable 可选能力接口：Chrome 引擎会话支持 chrome 仿真；
// Safari 引擎（真机 Simulator 自带真 chrome）不实现。CLI 经类型断言使用。
type BrowserChromeCapable interface {
	// EnableBrowserChromeSim 按会话指纹预设启用仿真；预设无 chrome 数据返回 false。
	EnableBrowserChromeSim() bool
	// BrowserChromeState 返回 (几何 spec, 基准小视口高 svh, 当前页面缩放, 是否启用)。
	BrowserChromeState() (*BrowserChromeSpec, int, float64, bool)
	// RestorePageScale 恢复跨 CLI 调用镜像的页面缩放（attach 时用）。
	// 必须真正经 CDP 重放：attach 路径的 SetDeviceMetricsOverride（viewport 重放）
	// 会把 page scale 踩回 1，只对齐进程内镜像 = 缩放态静默丢失。幂等。
	RestorePageScale(ctx context.Context, scale float64)
	// PageScale 当前页面缩放（act "zoom" 之后由引擎追踪）。
	PageScale() float64
	// ProbePageProtections 探测页面 dvh/safe-area 防护声明（observe 机读块用）。
	ProbePageProtections(ctx context.Context) PageProtections
}

// EnableBrowserChromeSim 启用 chrome 仿真（open / session attach 时调用一次；
// 会话级模式，中途不漂）。
func (impl *browserCoreImpl) EnableBrowserChromeSim() bool {
	preset := BuiltinPresets[NormalizePresetID(impl.fingerprintPreset)]
	if preset == nil || preset.BrowserChrome == nil {
		return false
	}
	impl.mu.Lock()
	impl.chromeSim = preset.BrowserChrome
	impl.mu.Unlock()
	impl.actEngine.setPointerGuard(impl.chromePointerGuard)
	return true
}

// BrowserChromeState 见 BrowserChromeCapable。
func (impl *browserCoreImpl) BrowserChromeState() (*BrowserChromeSpec, int, float64, bool) {
	impl.mu.RLock()
	sim := impl.chromeSim
	impl.mu.RUnlock()
	if sim == nil {
		return nil, 0, 1, false
	}
	return sim, sim.SmallViewportH(), impl.actEngine.PageScale(), true
}

// RestorePageScale 见 BrowserChromeCapable。
func (impl *browserCoreImpl) RestorePageScale(ctx context.Context, scale float64) {
	impl.actEngine.restorePageScale(scale)
	if scale > 1 {
		targetCtx := impl.currentCtx()
		runCtx, cancel := deriveTargetContext(ctx, targetCtx)
		defer cancel()
		if err := chromedp.Run(runCtx, chromedp.ActionFunc(func(actCtx context.Context) error {
			return emulation.SetPageScaleFactor(scale).Do(actCtx)
		})); err != nil {
			log.Printf("[BROWSER] restore page scale %.2f failed: %v", scale, err)
		}
	}
}

// PageScale 见 BrowserChromeCapable。
func (impl *browserCoreImpl) PageScale() float64 {
	return impl.actEngine.PageScale()
}

// ProbePageProtections 见 BrowserChromeCapable。
func (impl *browserCoreImpl) ProbePageProtections(ctx context.Context) PageProtections {
	return probePageProtections(ctx, impl.EvalJS)
}

// chromePointerGuard 拒绝落在 chrome 遮挡带内的指针动作（REQ-BC-05 fail-loud）。
// 判定在屏幕（视觉视口）坐标系：chrome 是 OS 层，屏幕坐标恒定；页面坐标随
// 缩放/平移变，须经 visualViewport 投影。探测失败保守放行（探测异常 ≠ 遮挡证据，
// 误拦会把环境问题伪装成产品问题）。
// protected 豁免（Human 拍定 2026-07-19，与 audit 压假阳对齐）：页面已声明
// dvh/safe-area 防护 → 放行——双视口仿真下 Chrome 分不出 dvh/lvh，模范页的
// 底部元素"看似被遮"多为模型假阳（真机可见）；假绿风险由 audit protected
// 标记 + Witness 截图 + --engine safari 真机对标兜底。
func (impl *browserCoreImpl) chromePointerGuard(ctx context.Context, x, y float64) error {
	impl.mu.RLock()
	sim := impl.chromeSim
	impl.mu.RUnlock()
	if sim == nil {
		return nil
	}
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(chromeGuardProbeJS, &raw)); err != nil || raw == "" {
		return nil
	}
	var vv chromeGuardProbe
	if err := json.Unmarshal([]byte(raw), &vv); err != nil {
		return nil
	}
	if vv.Dvh || vv.SafeArea {
		return nil
	}
	// 判定无上界：投影 ≥ svh 的点，要么在 chrome 遮挡带内 [svh,lvh)，要么在可视
	// 区之外（缩放/平移态投影越过视口底）——当前状态下真实用户都点不到。
	// 上界曾写成 `< lvh`，zoom 2 时点投影 1454 越过带被放行 = 假通过（实测抓获）。
	screenY := ProjectPageYToScreen(y, vv.OffsetTop, vv.Scale)
	if screenY < float64(sim.SmallViewportH()) {
		return nil
	}
	if screenY < float64(sim.LargeViewportH()) {
		return fmt.Errorf("%w: point (%.0f,%.0f) is occluded by browser chrome (safari bottom bar, screen y=%.0f in band [%d,%d), page_scale=%.2f) — a real user cannot tap here; scroll the target above the bar or fix the layout (use dvh/safe-area-inset)",
			ErrActFailed, x, y, screenY, sim.SmallViewportH(), sim.LargeViewportH(), vv.Scale)
	}
	return fmt.Errorf("%w: point (%.0f,%.0f) projects below the visible viewport (screen y=%.0f >= %d, page_scale=%.2f) — a real user cannot tap here in the current view; scroll/pan the target into view first (or act \"zoom reset\")",
		ErrActFailed, x, y, screenY, sim.LargeViewportH(), vv.Scale)
}
