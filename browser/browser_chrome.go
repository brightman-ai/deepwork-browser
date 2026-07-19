// Package browser — browser chrome 仿真（spec: docs/product/browser-chrome/）。
//
// 真机上浏览器自身 UI（iOS Safari 底部地址栏/工具条）是 OS 层元素：
// 高度/内容/位置与页面状态（滚动/缩放/内容）无关，仅配色随页面主题。
// 仿真复刻双视口语义：会话视口开到"大视口"(底栏收起态高度 lvh)，
// 底部 [svh, lvh) 区间为 chrome 遮挡带 —— `100vh` 布局的底部固定元素
// 会像真机一样沉入遮挡带。
//
// 不变量（INV-BC）：
//   - 几何唯一源 = devicedata/devices.json 的 browserChrome 字段（挂在
//     FingerprintPreset 上），observe 截图合成 / audit 断言 / act 命中拒绝
//     三个消费者同源取数，本文件不出现第二份设备几何字面量。
//   - chrome 层画在页面外（Go 侧截图合成），不注入 DOM —— 被测页无法
//     探测到仿真存在，且不随页面缩放/滚动变化（逐像素恒定）。
package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"strconv"
	"strings"
)

// BrowserChromeSpec 单设备的浏览器 chrome 几何（CSS px；数据源 devices.json）。
// 校准态：数值推导自 vendored Playwright 视口 + Apple HIG（△），待
// --engine safari Simulator 对标校准后升 ■（改数据不改代码）。
type BrowserChromeSpec struct {
	Style               string `json:"style"` // "safari-bottom-bar"
	ScreenH             int    `json:"screenH"`
	StatusBarH          int    `json:"statusBarH"`
	BottomBarExpandedH  int    `json:"bottomBarExpandedH"`
	BottomBarCollapsedH int    `json:"bottomBarCollapsedH"`
	HomeIndicatorH      int    `json:"homeIndicatorH"`
	// KeyboardH 软键盘总高（屏幕坐标，自屏幕底边起，含 QuickType 候选条；
	// 0 = 该设备无键盘几何 → 键盘仿真不可用）。REQ-BC-12。
	KeyboardH int `json:"keyboardH,omitempty"`
}

// SmallViewportH 底栏展开态布局视口高（= Playwright 视口，svh）。
func (s *BrowserChromeSpec) SmallViewportH() int {
	return s.ScreenH - s.StatusBarH - s.BottomBarExpandedH
}

// LargeViewportH 底栏收起态布局视口高（lvh，= 仿真会话的实际视口高）。
func (s *BrowserChromeSpec) LargeViewportH() int {
	return s.ScreenH - s.StatusBarH - s.BottomBarCollapsedH
}

// OcclusionBandH 遮挡带高度（视口底部被 chrome 盖住的区间）。
func (s *BrowserChromeSpec) OcclusionBandH() int {
	return s.LargeViewportH() - s.SmallViewportH()
}

// KeyboardInsetH 软键盘盖住视口底部的高度（CSS px，视口坐标系）。
// 键盘自屏幕底边起 KeyboardH；视口底边在屏幕 statusBarH+lvh 处，其下还有
// bottomBarCollapsedH（收起态底栏/home 区）不属于视口 → 视口内被盖 =
// KeyboardH - BottomBarCollapsedH。KeyboardH=0 → 0（无键盘几何）。
func (s *BrowserChromeSpec) KeyboardInsetH() int {
	if s.KeyboardH <= 0 {
		return 0
	}
	return s.KeyboardH - s.BottomBarCollapsedH
}

// KeyboardTopY 键盘态遮挡区上沿（视口 CSS y；仅 KeyboardInsetH>0 有意义）。
func (s *BrowserChromeSpec) KeyboardTopY() int {
	return s.LargeViewportH() - s.KeyboardInsetH()
}

// Validate 校验几何自洽（devicedata 载入期 fail-fast 用）。
// presetViewportH = 同预设的 viewportH，必须等于 SmallViewportH —— 保证
// browserChrome 与既有视口数据不出现两套矛盾几何。
func (s *BrowserChromeSpec) Validate(presetViewportH int) error {
	if s.Style != "safari-bottom-bar" {
		return fmt.Errorf("browserChrome: unsupported style %q", s.Style)
	}
	if s.SmallViewportH() != presetViewportH {
		return fmt.Errorf("browserChrome: screenH-statusBarH-bottomBarExpandedH=%d != preset viewportH=%d (SSOT 矛盾)",
			s.SmallViewportH(), presetViewportH)
	}
	if s.OcclusionBandH() <= 0 {
		return fmt.Errorf("browserChrome: occlusion band %d <= 0", s.OcclusionBandH())
	}
	if s.HomeIndicatorH < 0 || s.HomeIndicatorH > s.BottomBarExpandedH {
		return fmt.Errorf("browserChrome: homeIndicatorH %d out of range", s.HomeIndicatorH)
	}
	if s.KeyboardH != 0 {
		inset := s.KeyboardInsetH()
		if inset <= s.OcclusionBandH() || inset >= s.LargeViewportH() {
			return fmt.Errorf("browserChrome: keyboardH %d → viewport inset %d out of range (must cover more than chrome band %d, less than lvh %d)",
				s.KeyboardH, inset, s.OcclusionBandH(), s.LargeViewportH())
		}
	}
	return nil
}

// --browser-chrome 模式常量。
const (
	BrowserChromeAuto = "auto"
	BrowserChromeOn   = "on"
	BrowserChromeOff  = "off"
)

// ResolveBrowserChromeMode 判定会话是否启用 chrome 仿真。
//   - off → 关。
//   - on  → 强制开；预设无 chrome 数据或用户显式 --viewport（几何归设备，任意
//     视口无数据可依）→ fail-loud，不静默降级。
//   - auto(默认/空) → 预设带 chrome 数据且未显式改视口 → 开；否则关。
func ResolveBrowserChromeMode(mode string, preset *FingerprintPreset, explicitViewport bool) (bool, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = BrowserChromeAuto
	}
	hasData := preset != nil && preset.BrowserChrome != nil
	switch mode {
	case BrowserChromeOff:
		return false, nil
	case BrowserChromeOn:
		if !hasData {
			return false, fmt.Errorf("--browser-chrome=on: preset has no browser chrome geometry (supported: iOS device presets)")
		}
		if explicitViewport {
			return false, fmt.Errorf("--browser-chrome=on conflicts with explicit --viewport: chrome geometry is device-defined")
		}
		return true, nil
	case BrowserChromeAuto:
		return hasData && !explicitViewport, nil
	default:
		return false, fmt.Errorf("invalid --browser-chrome %q (allowed: auto/on/off)", mode)
	}
}

// OcclusionZone 遮挡矩形（CSS px，视口坐标系 = 默认截图坐标系/DPR 折算前）。
type OcclusionZone struct {
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
	State string `json:"state"` // "expanded" | "collapsed"
	Desc  string `json:"desc"`
}

// OcclusionZones 按状态给出遮挡矩形集。
// expanded（默认/最坏态）：底部 [svh, lvh) 整带被底栏盖住。
// keyboard=true（REQ-BC-12）：软键盘替代底栏（真机行为），遮挡区扩大为
// [KeyboardTopY, lvh)——单矩形，键盘区已覆盖 chrome 带。
// collapsed：收起态底栏位于视口之外（布局视口即 lvh）→ 无页面遮挡（几何事实，
// 保留状态位以便未来非 iOS 顶栏模型扩展）。
func (s *BrowserChromeSpec) OcclusionZones(viewportW int, keyboard bool) []OcclusionZone {
	if keyboard && s.KeyboardInsetH() > 0 {
		return []OcclusionZone{{
			X: 0, Y: s.KeyboardTopY(), W: viewportW, H: s.KeyboardInsetH(),
			State: "keyboard", Desc: "soft keyboard (act \"keyboard hide\" to dismiss)",
		}}
	}
	return []OcclusionZone{{
		X: 0, Y: s.SmallViewportH(), W: viewportW, H: s.OcclusionBandH(),
		State: "expanded", Desc: "safari bottom bar (address pill + toolbar)",
	}}
}

// ---- 视口事实 shim（REQ-BC-11/12：页面感知的视口 = 真机 bars-expanded 态）----
//
// 真机上 chrome/键盘不是"画在页面上的像素"，而是"改变页面所依据的视口"并触发
// visualViewport resize 事件。Chrome 引擎里 layout viewport/dvh/vh 锁死会话视口
// （必须留在 lvh，否则 100vh 病灶隐身），能对齐的通道：
//   - visualViewport.height：实例 getter patch = real - bottomInset/scale（zoom 自洽）
//   - window.innerHeight：= real - chromeInset/scale（iOS 语义：随底栏/缩放，不随键盘）
//   - 100svh / env(safe-area-inset-*)：CDP 原生 override（browser_chrome_impl.go）
// 残留假阳通道（引擎级不可修，判定层豁免 + 机读块如实声明）：100dvh、fixed-bottom。
//
// 幂等（guard on __dwViewportFacts）：AddScriptToEvaluateOnNewDocument 每次 attach
// 重注册 + evaluate-now 兜底，重复执行零效果。仅 top frame。真实事件流不拦截：
// 只 patch getter，Chrome 自身的 vv resize（缩放等）自然穿透且读值已折算。
const viewportFactsShimJSTmpl = `(function () {
  if (window !== window.top) return;
  if (window.__dwViewportFacts) return;
  var vv = window.visualViewport;
  if (!vv) return;
  var proto = Object.getPrototypeOf(vv);
  var realH = Object.getOwnPropertyDescriptor(proto, 'height').get;
  var state = { chromeInset: %d, kbInset: 0 };
  function bottomInset() { return Math.max(state.chromeInset, state.kbInset); }
  Object.defineProperty(vv, 'height', {
    configurable: true,
    get: function () {
      var s = vv.scale || 1;
      return Math.max(0, realH.call(vv) - bottomInset() / s);
    }
  });
  Object.defineProperty(window, 'innerHeight', {
    configurable: true,
    get: function () {
      var s = vv.scale || 1;
      return Math.max(0, Math.round(realH.call(vv) - state.chromeInset / s));
    }
  });
  window.__dwViewportFacts = function (patch) {
    if (patch && typeof patch.kbInset === 'number' && patch.kbInset !== state.kbInset) {
      state.kbInset = patch.kbInset;
      vv.dispatchEvent(new Event('resize'));
    }
    // layoutH = 布局视口高（lvh，未仿真的引擎真相）——内部工具 JS 专用出口：
    // 页面看仿真事实（innerHeight=svh），坐标/快照工具必须看布局真相（截图高=lvh），
    // 两界不可混（评审抓获 tapxy 用 patched innerHeight 作基准 → 落点偏高约一成）。
    return { chromeInset: state.chromeInset, kbInset: state.kbInset,
             layoutH: Math.round(realH.call(vv) * (vv.scale || 1)) };
  };
})();`

// LayoutViewportHeightJSExpr 内部工具 JS 取布局视口高的统一表达式：
// chrome 仿真会话经 shim 出口取 lvh（innerHeight 已被 patch 成 svh，不可用作
// 坐标基准）；非仿真会话回退 innerHeight（原契约）。
const LayoutViewportHeightJSExpr = `(window.__dwViewportFacts ? window.__dwViewportFacts().layoutH : (window.innerHeight || 0))`

// ViewportFactsShimJS 按设备几何实例化 shim（chromeInset = 遮挡带高）。
func (s *BrowserChromeSpec) ViewportFactsShimJS() string {
	return fmt.Sprintf(viewportFactsShimJSTmpl, s.OcclusionBandH())
}

// keyboardStateJSTmpl 键盘态推入页面：更新 kbInset（shim 内部派发 vv resize），
// show 时若聚焦元素沉在键盘区下则 scrollIntoView（真机 Safari 卷动语义——真实
// 布局滚动，渲染与报告坐标一致；不合成 offsetTop 假值）。
// 参数：%d kbInset（0=hide）、%d 键盘区上沿 KeyboardTopY。
const keyboardStateJSTmpl = `(function () {
  if (!window.__dwViewportFacts) return 'no-shim';
  window.__dwViewportFacts({ kbInset: %d });
  var kbTop = %d;
  if (kbTop > 0) {
    var ae = document.activeElement;
    if (ae && ae !== document.body && ae !== document.documentElement) {
      var r = ae.getBoundingClientRect();
      if (r.bottom > kbTop) { try { ae.scrollIntoView({ block: 'center', behavior: 'instant' }); } catch (e) {} }
    }
  }
  return 'ok';
})()`

// KeyboardStateJS 生成键盘态推入脚本。show=false → kbInset 0 且不卷动。
func (s *BrowserChromeSpec) KeyboardStateJS(show bool) string {
	if !show {
		return fmt.Sprintf(keyboardStateJSTmpl, 0, 0)
	}
	return fmt.Sprintf(keyboardStateJSTmpl, s.KeyboardInsetH(), s.KeyboardTopY())
}

// editableFocusProbeJS 焦点自动同步探针：activeElement 是否文本可编辑
// （真机语义：用户点输入框 → 键盘弹起；点别处失焦 → 键盘收起）。
const editableFocusProbeJS = `(function () {
  var ae = document.activeElement;
  if (!ae || ae === document.body || ae === document.documentElement) return false;
  var tag = (ae.tagName || '').toLowerCase();
  if (tag === 'textarea' || tag === 'select') return true;
  if (ae.isContentEditable) return true;
  if (tag !== 'input') return false;
  var t = (ae.getAttribute('type') || 'text').toLowerCase();
  return ['button','checkbox','radio','submit','reset','file','range','color','image','hidden'].indexOf(t) < 0;
})()`

// ---- 主题取色 ----

// browserChromeThemeProbeJS 读取页面主题色（theme-color meta 优先，缺则 body 背景）。
// 一次性 evaluate，非持久注入 —— 页面感知不到仿真存在。
const browserChromeThemeProbeJS = `(function(){
  var m = document.querySelector('meta[name="theme-color"]');
  var c = (m && m.getAttribute('content')) || '';
  if (!c) {
    var el = document.body || document.documentElement;
    c = el ? getComputedStyle(el).backgroundColor : '';
  }
  return c || '';
})()`

// parseCSSColor 解析 #rgb/#rrggbb/rgb()/rgba() 为 RGBA；解析失败返回 ok=false。
func parseCSSColor(s string) (color.RGBA, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "transparent" {
		return color.RGBA{}, false
	}
	if strings.HasPrefix(s, "#") {
		hexStr := s[1:]
		if len(hexStr) == 3 {
			hexStr = string([]byte{hexStr[0], hexStr[0], hexStr[1], hexStr[1], hexStr[2], hexStr[2]})
		}
		if len(hexStr) != 6 {
			return color.RGBA{}, false
		}
		v, err := strconv.ParseUint(hexStr, 16, 32)
		if err != nil {
			return color.RGBA{}, false
		}
		return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 255}, true
	}
	if strings.HasPrefix(s, "rgb") {
		open := strings.IndexByte(s, '(')
		close := strings.IndexByte(s, ')')
		if open < 0 || close <= open {
			return color.RGBA{}, false
		}
		parts := strings.Split(s[open+1:close], ",")
		if len(parts) < 3 {
			return color.RGBA{}, false
		}
		var ch [3]uint8
		for i := 0; i < 3; i++ {
			n, err := strconv.ParseFloat(strings.TrimSpace(parts[i]), 64)
			if err != nil || n < 0 || n > 255 {
				return color.RGBA{}, false
			}
			ch[i] = uint8(n)
		}
		if len(parts) >= 4 { // rgba 全透明视为无色
			if a, err := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64); err == nil && a == 0 {
				return color.RGBA{}, false
			}
		}
		return color.RGBA{R: ch[0], G: ch[1], B: ch[2], A: 255}, true
	}
	return color.RGBA{}, false
}

// chromePalette 由页面主题色导出 chrome 配色（真机行为：底栏随页面取色）。
type chromePalette struct {
	Bar       color.RGBA // 底栏背景
	Pill      color.RGBA // 地址 pill / 按钮
	PillInner color.RGBA // pill 内地址域
	Indicator color.RGBA // home indicator
	Hairline  color.RGBA // 顶部分隔线
}

func paletteForTheme(themeCSS string) chromePalette {
	base, ok := parseCSSColor(themeCSS)
	dark := false
	if ok {
		// 相对亮度（近似）：< 128 视为深色页
		lum := 0.299*float64(base.R) + 0.587*float64(base.G) + 0.114*float64(base.B)
		dark = lum < 128
	}
	if !ok {
		base = color.RGBA{R: 0xF2, G: 0xF2, B: 0xF7, A: 255} // iOS 浅色系统底
	}
	if dark {
		return chromePalette{
			Bar:       base,
			Pill:      lighten(base, 28),
			PillInner: lighten(base, 44),
			Indicator: color.RGBA{R: 255, G: 255, B: 255, A: 153},
			Hairline:  lighten(base, 60),
		}
	}
	return chromePalette{
		Bar:       base,
		Pill:      color.RGBA{R: 255, G: 255, B: 255, A: 255},
		PillInner: darken(base, 10),
		Indicator: color.RGBA{R: 0, G: 0, B: 0, A: 153},
		Hairline:  darken(base, 40),
	}
}

func clamp8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func lighten(c color.RGBA, d int) color.RGBA {
	return color.RGBA{R: clamp8(int(c.R) + d), G: clamp8(int(c.G) + d), B: clamp8(int(c.B) + d), A: 255}
}

func darken(c color.RGBA, d int) color.RGBA {
	return color.RGBA{R: clamp8(int(c.R) - d), G: clamp8(int(c.G) - d), B: clamp8(int(c.B) - d), A: 255}
}

// ---- 截图合成（页面外绘制；INV-BC 不变性的实现点）----

// CompositeBrowserChrome 把 chrome 遮挡带画到截图上并回编码。
//   - shot: 原截图字节（JPEG/PNG 自动识别，按原格式回编码）。
//   - themeCSS: 页面主题色（空 → 浅色缺省）。
//   - annotate: true 时额外画红描边遮挡区（evidence 用；默认截图不带标注）。
//   - keyboard: 键盘态（REQ-BC-12）——软键盘替代 safari 底栏（真机行为），
//     绘制键盘意符占满 [KeyboardTopY, lvh)。
//
// 截图物理像素与 CSS 的比例（DPR）由"图高 / lvh"自动推导 —— 仿真会话视口高
// 恒为 lvh（open 时定死），几何与 DPR 无关。
// 纯函数：同 (spec, 尺寸, theme, annotate, keyboard) → 逐字节相同输出（确定性/不变性）。
func CompositeBrowserChrome(shot []byte, spec *BrowserChromeSpec, themeCSS string, annotate, keyboard bool) ([]byte, error) {
	if spec == nil {
		return shot, nil
	}
	src, format, err := image.Decode(bytes.NewReader(shot))
	if err != nil {
		return nil, fmt.Errorf("browser_chrome: decode screenshot: %w", err)
	}
	b := src.Bounds()
	img := image.NewRGBA(b)
	draw.Draw(img, b, src, b.Min, draw.Src)

	scale := float64(b.Dy()) / float64(spec.LargeViewportH())
	if scale <= 0 {
		scale = 1
	}
	px := func(cssPx int) int { return int(float64(cssPx)*scale + 0.5) }

	bandTop := px(spec.SmallViewportH())
	if keyboard && spec.KeyboardInsetH() > 0 {
		bandTop = px(spec.KeyboardTopY())
	}
	bandBottom := b.Dy() // 视口底 = lvh
	if bandTop >= bandBottom {
		return shot, nil // 图不含遮挡带（异常尺寸），原样返回
	}
	pal := paletteForTheme(themeCSS)
	w := b.Dx()

	if keyboard && spec.KeyboardInsetH() > 0 {
		// 键盘态：键盘意符 + home indicator；safari 底栏被键盘替代不画。
		drawKeyboard(img, image.Rect(0, bandTop, w, bandBottom), pal)
		indW := w * 36 / 100
		indH := maxInt(px(5), 3)
		indTop := bandBottom - px(spec.HomeIndicatorH)/2 - indH/2
		fillRoundedRect(img, image.Rect((w-indW)/2, indTop, (w+indW)/2, indTop+indH), indH/2, pal.Indicator)
		if annotate {
			red := color.RGBA{R: 0xFF, G: 0x3B, B: 0x30, A: 255}
			strokeRect(img, image.Rect(0, bandTop, w, bandBottom), maxInt(2, px(2)), red)
		}
		return encodeShot(img, format)
	}

	// 底栏背景 + 顶部 hairline
	fillRect(img, image.Rect(0, bandTop, w, bandBottom), pal.Bar)
	fillRect(img, image.Rect(0, bandTop, w, bandTop+maxInt(1, px(1))), pal.Hairline)

	// 布局（按遮挡带比例，与 IMG_5170/5171 对标的简化意符）：
	// [<] [>] 左圆钮 | 中部地址 pill（内含地址域）| [...] 右圆钮 | 底部 home indicator
	bandH := bandBottom - bandTop
	pillH := bandH * 46 / 100
	pillTop := bandTop + bandH*12 / 100
	btnR := pillH * 40 / 100

	leftBtnCX := w * 8 / 100
	left2BtnCX := w * 17 / 100
	rightBtnCX := w * 92 / 100
	btnCY := pillTop + pillH/2
	fillCircle(img, leftBtnCX, btnCY, btnR, pal.Pill)
	fillCircle(img, left2BtnCX, btnCY, btnR, pal.Pill)
	fillCircle(img, rightBtnCX, btnCY, btnR, pal.Pill)

	pillLeft := w * 24 / 100
	pillRight := w * 84 / 100
	fillRoundedRect(img, image.Rect(pillLeft, pillTop, pillRight, pillTop+pillH), pillH/2, pal.Pill)
	// 地址域内嵌条
	innerInset := pillH / 4
	fillRoundedRect(img, image.Rect(pillLeft+innerInset*2, pillTop+innerInset, pillRight-innerInset*2, pillTop+pillH-innerInset), (pillH-2*innerInset)/2, pal.PillInner)

	// home indicator
	indW := w * 36 / 100
	indH := maxInt(px(5), 3)
	indTop := bandBottom - bandH*14/100
	fillRoundedRect(img, image.Rect((w-indW)/2, indTop, (w+indW)/2, indTop+indH), indH/2, pal.Indicator)

	if annotate {
		red := color.RGBA{R: 0xFF, G: 0x3B, B: 0x30, A: 255}
		strokeRect(img, image.Rect(0, bandTop, w, bandBottom), maxInt(2, px(2)), red)
	}
	return encodeShot(img, format)
}

func encodeShot(img *image.RGBA, format string) ([]byte, error) {
	var out bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&out, img)
	default:
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: 80})
	}
	if err != nil {
		return nil, fmt.Errorf("browser_chrome: encode screenshot: %w", err)
	}
	return out.Bytes(), nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func fillRect(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	draw.Draw(img, r.Intersect(img.Bounds()), image.NewUniform(c), image.Point{}, draw.Over)
}

// fillRoundedRect 圆角矩形（逐行扫描，圆角用圆方程收边；无抗锯齿，确定性优先）。
func fillRoundedRect(img *image.RGBA, r image.Rectangle, radius int, c color.RGBA) {
	r = r.Intersect(img.Bounds())
	if r.Empty() {
		return
	}
	h := r.Dy()
	if radius > h/2 {
		radius = h / 2
	}
	if radius > r.Dx()/2 {
		radius = r.Dx() / 2
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		inset := 0
		dyTop := y - r.Min.Y
		dyBottom := r.Max.Y - 1 - y
		if dyTop < radius {
			dy := radius - dyTop
			inset = radius - isqrt(radius*radius-dy*dy)
		} else if dyBottom < radius {
			dy := radius - dyBottom
			inset = radius - isqrt(radius*radius-dy*dy)
		}
		fillRect(img, image.Rect(r.Min.X+inset, y, r.Max.X-inset, y+1), c)
	}
}

func fillCircle(img *image.RGBA, cx, cy, r int, c color.RGBA) {
	for dy := -r; dy <= r; dy++ {
		dx := isqrt(r*r - dy*dy)
		fillRect(img, image.Rect(cx-dx, cy+dy, cx+dx, cy+dy+1), c)
	}
}

func strokeRect(img *image.RGBA, r image.Rectangle, thickness int, c color.RGBA) {
	fillRect(img, image.Rect(r.Min.X, r.Min.Y, r.Max.X, r.Min.Y+thickness), c)
	fillRect(img, image.Rect(r.Min.X, r.Max.Y-thickness, r.Max.X, r.Max.Y), c)
	fillRect(img, image.Rect(r.Min.X, r.Min.Y, r.Min.X+thickness, r.Max.Y), c)
	fillRect(img, image.Rect(r.Max.X-thickness, r.Min.Y, r.Max.X, r.Max.Y), c)
}

// isqrt 整数平方根（floor）。
func isqrt(n int) int {
	if n <= 0 {
		return 0
	}
	x := n
	y := (x + 1) / 2
	for y < x {
		x = y
		y = (x + n/x) / 2
	}
	return x
}

// ---- 视觉视口投影（act 遮挡守卫 / audit 共用语义）----

// chromeGuardProbeJSTmpl act 守卫单次探针：视觉视口投影参数 + 页面防护声明 +
// fixed-bottom 命中检测一把取（每次受守卫点击一次 evaluate，合并省往返）。
// 参数：%f pageX、%f pageY、%d lvh（布局视口高，Go 侧 SSOT 注入）。
// 注意 vv.scale 是真实值（shim 只 patch height），offsetTop 亦真实。
// fixedBottom（REQ-BC-05 MODIFIED(2)）：命中点所在元素链上存在 position:fixed 且
// 底边锚定布局视口底（|rect.bottom - lvh| <= 2）的祖先 —— 真机 bars-expanded
// 时 layout viewport = svh，此类元素在底栏上方可见，chrome 带命中为模型假阳。
// 基准必须是注入的 lvh：svh 单位 override 会把 documentElement.clientHeight 一并
// 改成 svh，而 fixed 元素实际仍锚定 ICB=lvh（评审实测抓获——用 clientHeight 判定
// 恒不命中 = 豁免死代码）。
const chromeGuardProbeJSTmpl = `(function(){
  var px = %f, py = %f, lvh = %d;
  var v = window.visualViewport || {};
  var css = '';
  try {
    for (var i = 0; i < document.styleSheets.length; i++) {
      try {
        var rules = document.styleSheets[i].cssRules || [];
        for (var j = 0; j < rules.length; j++) css += rules[j].cssText;
      } catch (e) { /* cross-origin sheet: skip */ }
    }
  } catch (e) {}
  css += (document.documentElement.getAttribute('style') || '');
  if (document.body) css += (document.body.getAttribute('style') || '');
  var fixedBottom = false;
  try {
    var el = document.elementFromPoint(px - window.scrollX, py - window.scrollY);
    for (var n = el; n && n !== document.documentElement; n = n.parentElement) {
      var cs = getComputedStyle(n);
      if (cs.position === 'fixed') {
        var r = n.getBoundingClientRect();
        if (Math.abs(r.bottom - lvh) <= 2) { fixedBottom = true; break; }
      }
    }
  } catch (e) {}
  return JSON.stringify({
    scale: v.scale || 1, offsetTop: v.offsetTop || 0, offsetLeft: v.offsetLeft || 0,
    dvh: /\b\d+(\.\d+)?(dvh|svh)\b/.test(css),
    safe_area: /safe-area-inset/.test(css),
    fixed_bottom: fixedBottom
  });
})()`

type chromeGuardProbe struct {
	Scale       float64 `json:"scale"`
	OffsetTop   float64 `json:"offsetTop"`
	Dvh         bool    `json:"dvh"`
	SafeArea    bool    `json:"safe_area"`
	FixedBottom bool    `json:"fixed_bottom"`
}

// ProjectPageYToScreen 把页面坐标 y 投影到屏幕（视觉视口）CSS 坐标。
// chrome 遮挡带在屏幕坐标系恒定（[svh, lvh)），页面坐标随缩放/平移变 ——
// 判"用户能不能点到"必须在屏幕坐标系做。
func ProjectPageYToScreen(pageY, vvOffsetTop, vvScale float64) float64 {
	if vvScale <= 0 {
		vvScale = 1
	}
	return (pageY - vvOffsetTop) * vvScale
}

// pageProtectionsProbeJS 静态扫描页面 CSS 是否已用 dvh/svh 或 safe-area-inset
// （observe browser_chrome.page_protections；audit 压假阳同源逻辑在检查脚本内）。
const pageProtectionsProbeJS = `(function(){
  var css = '';
  try {
    for (var i = 0; i < document.styleSheets.length; i++) {
      try {
        var rules = document.styleSheets[i].cssRules || [];
        for (var j = 0; j < rules.length; j++) css += rules[j].cssText;
      } catch (e) { /* cross-origin sheet: skip */ }
    }
  } catch (e) {}
  css += (document.documentElement.getAttribute('style') || '');
  if (document.body) css += (document.body.getAttribute('style') || '');
  return JSON.stringify({
    dvh: /\b\d+(\.\d+)?(dvh|svh)\b/.test(css),
    safe_area: /safe-area-inset/.test(css)
  });
})()`

// PageProtections 页面自带的 chrome 适配防护声明。
type PageProtections struct {
	Dvh      bool `json:"dvh"`
	SafeArea bool `json:"safe_area"`
}

// probePageProtections 在会话页面上探测防护声明（探测失败返回零值，不阻塞 observe）。
func probePageProtections(ctx context.Context, evalJS func(context.Context, string, interface{}) error) PageProtections {
	var raw string
	var p PageProtections
	if err := evalJS(ctx, pageProtectionsProbeJS, &raw); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &p)
	}
	return p
}
