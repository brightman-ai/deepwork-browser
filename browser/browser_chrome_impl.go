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
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
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
	// ApplyViewportFacts 把视口事实推到页面（REQ-BC-11/12）：svh 单位差
	// override（跨导航复位，须 open/attach/导航后重放）、safe-area 注入、
	// 当前文档 shim 兜底、键盘态推入；并挂 frameNavigated 监听在导航提交点
	// 即时重放 override（收窄首屏 svh 窗口）。幂等。
	// install=true（仅 open 调用）额外注册 AddScriptToEvaluateOnNewDocument——
	// 注册在浏览器内跨 CLI 连接持久（评审实测 8 连接 → 8 份），attach 重注册
	// 会无界堆积，故 attach 必须传 false。
	ApplyViewportFacts(ctx context.Context, keyboard bool, install bool) error
	// KeyboardVisible 当前软键盘态（act "keyboard"/焦点自动同步之后由引擎追踪）。
	KeyboardVisible() bool
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
	if preset.BrowserChrome.KeyboardInsetH() > 0 {
		impl.actEngine.setKeyboardController(impl.chromeKeyboardCtl)
	}
	return true
}

// ApplyViewportFacts 见 BrowserChromeCapable。调用点：open（install=true，启用后、
// 首导航前）、attach（install=false；replayViewportProfile 之后——
// SetDeviceMetricsOverride 会踩掉 svh override）、Navigate 成功后（无锁核心）。
func (impl *browserCoreImpl) ApplyViewportFacts(ctx context.Context, keyboard bool, install bool) error {
	impl.mu.RLock()
	sim := impl.chromeSim
	impl.mu.RUnlock()
	if sim == nil {
		return nil
	}
	targetCtx := impl.currentCtx()
	runCtx, cancel := deriveTargetContext(ctx, targetCtx)
	defer cancel()
	impl.actEngine.restoreKeyboard(keyboard && sim.KeyboardInsetH() > 0)
	impl.armViewportFactsReplayOnNav()
	if err := applyViewportFactsCDP(runCtx, sim, keyboard, install); err != nil {
		return err
	}
	return nil
}

// armViewportFactsReplayOnNav 挂 frameNavigated 监听：主 frame 导航提交即重放
// svh/safe-area override（CDP override 在新文档提交时复位——探针实测 ■；等到
// Navigate 收尾才重放会给首屏留一个 "CSS svh=lvh 而 shim=svh" 的不一致窗口，
// 评审用 units-probe 加载期抓获）。监听随本 CLI 进程存亡，每次 attach 重挂。
func (impl *browserCoreImpl) armViewportFactsReplayOnNav() {
	impl.mu.Lock()
	if impl.vfNavArmed {
		impl.mu.Unlock()
		return
	}
	impl.vfNavArmed = true
	impl.mu.Unlock()
	targetCtx := impl.currentCtx()
	chromedp.ListenTarget(targetCtx, func(ev interface{}) {
		e, ok := ev.(*page.EventFrameNavigated)
		if !ok || e.Frame == nil || e.Frame.ParentID != "" {
			return
		}
		impl.mu.RLock()
		sim := impl.chromeSim
		impl.mu.RUnlock()
		if sim == nil {
			return
		}
		// 事件回调禁阻塞：goroutine 内重放，仅 override（shim 经注册已随新文档生效）。
		go func() {
			runCtx, cancel := context.WithTimeout(targetCtx, 3*time.Second)
			defer cancel()
			if err := chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
				return viewportFactsOverrides(c, sim)
			})); err != nil {
				log.Printf("[BROWSER] viewport facts replay on nav failed: %v", err)
			}
		}()
	})
}

// viewportFactsOverrides svh 单位差 + safe-area 两条 CDP override（重放最小集）。
func viewportFactsOverrides(actCtx context.Context, sim *BrowserChromeSpec) error {
	if err := emulation.SetSmallViewportHeightDifferenceOverride(int64(sim.OcclusionBandH())).Do(actCtx); err != nil {
		return fmt.Errorf("svh difference override: %w", err)
	}
	ind := int64(sim.HomeIndicatorH)
	if err := emulation.SetSafeAreaInsetsOverride(&emulation.SafeAreaInsets{Bottom: ind, BottomMax: ind}).Do(actCtx); err != nil {
		return fmt.Errorf("safe-area insets override: %w", err)
	}
	return nil
}

// applyViewportFactsCDP 无锁核心（runCtx 已是 target 上下文）。Navigate 持有
// impl.mu.RLock 时直接调本函数——不可回调 ApplyViewportFacts（RWMutex 不可重入）。
// register 仅 open 传 true：AddScriptToEvaluateOnNewDocument 在浏览器内跨 CLI
// 连接持久，重复注册无界堆积（shim 幂等故无正确性 bug，但内存/每导航开销随
// 命令数增长——评审实测 8 注册 → 单导航执行 8 遍）。
func applyViewportFactsCDP(runCtx context.Context, sim *BrowserChromeSpec, keyboard bool, register bool) error {
	err := chromedp.Run(runCtx, chromedp.ActionFunc(func(actCtx context.Context) error {
		if err := viewportFactsOverrides(actCtx, sim); err != nil {
			return err
		}
		if register {
			if _, err := page.AddScriptToEvaluateOnNewDocument(sim.ViewportFactsShimJS()).Do(actCtx); err != nil {
				return fmt.Errorf("register viewport facts shim: %w", err)
			}
		}
		// 当前文档兜底（AddScript 只对未来导航生效；attach 到旧会话时也补装）；shim 幂等。
		var res interface{}
		if err := chromedp.Evaluate(sim.ViewportFactsShimJS(), &res).Do(actCtx); err != nil {
			return fmt.Errorf("install viewport facts shim in current document: %w", err)
		}
		var kb string
		if err := chromedp.Evaluate(sim.KeyboardStateJS(keyboard), &kb).Do(actCtx); err != nil {
			return fmt.Errorf("push keyboard state: %w", err)
		}
		return nil
	}))
	if err != nil {
		return fmt.Errorf("apply viewport facts: %w", err)
	}
	return nil
}

// KeyboardVisible 见 BrowserChromeCapable。
func (impl *browserCoreImpl) KeyboardVisible() bool {
	return impl.actEngine.KeyboardVisible()
}

// chromeKeyboardCtl 键盘态控制器（actionEngine 经 setKeyboardController 持有；
// act "keyboard show|hide" 与焦点自动同步共用）。ctx 已是 action 目标上下文。
func (impl *browserCoreImpl) chromeKeyboardCtl(ctx context.Context, show bool) error {
	impl.mu.RLock()
	sim := impl.chromeSim
	impl.mu.RUnlock()
	if sim == nil || sim.KeyboardInsetH() <= 0 {
		return fmt.Errorf("%w: keyboard simulation unavailable (no keyboard geometry for this preset)", ErrActFailed)
	}
	var res string
	if err := chromedp.Run(ctx, chromedp.Evaluate(sim.KeyboardStateJS(show), &res)); err != nil {
		return fmt.Errorf("%w: push keyboard state: %v", ErrActFailed, err)
	}
	if res == "no-shim" {
		return fmt.Errorf("%w: viewport facts shim missing in page (reopen session with current dw-browser)", ErrActFailed)
	}
	return nil
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
// 键盘区命中（REQ-BC-12）先判且**无任何豁免**：软键盘对所有页面都是真遮挡
// （dvh/safe-area/fixed-bottom 在真机上同样被键盘盖住——fixed 元素沉在键盘下
// 正是经典病灶类）。
func (impl *browserCoreImpl) chromePointerGuard(ctx context.Context, x, y float64) error {
	impl.mu.RLock()
	sim := impl.chromeSim
	impl.mu.RUnlock()
	if sim == nil {
		return nil
	}
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(fmt.Sprintf(chromeGuardProbeJSTmpl, x, y, sim.LargeViewportH()), &raw)); err != nil || raw == "" {
		return nil
	}
	var vv chromeGuardProbe
	if err := json.Unmarshal([]byte(raw), &vv); err != nil {
		return nil
	}
	screenY := ProjectPageYToScreen(y, vv.OffsetTop, vv.Scale)
	if impl.actEngine.KeyboardVisible() {
		if kbTop := float64(sim.KeyboardTopY()); screenY >= kbTop {
			return fmt.Errorf("%w: point (%.0f,%.0f) is occluded by the soft keyboard (screen y=%.0f in zone [%d,%d), page_scale=%.2f) — a real user cannot tap here while the keyboard is open; act \"keyboard hide\" first, or scroll the target into view",
				ErrActFailed, x, y, screenY, sim.KeyboardTopY(), sim.LargeViewportH(), vv.Scale)
		}
	}
	if vv.Dvh || vv.SafeArea {
		return nil
	}
	// fixed-bottom 豁免（REQ-BC-05 MODIFIED(2)）：真机 bars-expanded 时 layout
	// viewport=svh，fixed 底锚元素在底栏上方可见——chrome 带命中为双视口模型假阳
	// （引擎级残留通道，与 protected 豁免同哲学：压假阳，假绿由 audit warn 兜底）。
	if vv.FixedBottom {
		return nil
	}
	// 判定无上界：投影 ≥ svh 的点，要么在 chrome 遮挡带内 [svh,lvh)，要么在可视
	// 区之外（缩放/平移态投影越过视口底）——当前状态下真实用户都点不到。
	// 上界曾写成 `< lvh`，zoom 2 时点投影 1454 越过带被放行 = 假通过（实测抓获）。
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
