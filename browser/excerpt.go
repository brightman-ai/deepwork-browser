package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
)

// Excerpt 是观察级只读命令的数据面：从已打开页面按 spec 采集目标矩形与
// 短语高亮矩形（页面 CSS 像素坐标），配合全页 PNG 截图由 Go 离线合成，
// 全程不改 DOM、不需要写权限、不作废观测。
//
// 命令层不接收调用方 JavaScript——spec 只含选择器/短语，采集脚本是内建
// 只读模板，因此没有任意执行面，也不经 EvaluateAct 写门。

// ExcerptBoxStyle 是单个摘录的红框样式覆盖。
type ExcerptBoxStyle struct {
	Color string `json:"color,omitempty"` // "#rrggbb"
	Width int    `json:"width,omitempty"` // CSS px
}

// ExcerptSpec 是一条摘录的输入契约。
//
// targets 语法（与既有参考实现一致的前缀式）：
//   - "id:X"    → document.getElementById(X)（arXiv 段落/表格 id 含点，免转义）
//   - "title:X" → id 元素内的 .ltx_title（arXiv 标题）
//   - 裸值      → CSS selector
//
// highlight 短语在 targets 解析出的元素并集文本内匹配：空白归一（\s+）、
// 跳过 MathML annotation/annotation-xml/math 及 script/style、可跨内联节点。
type ExcerptSpec struct {
	Name           string           `json:"name"`
	Targets        []string         `json:"targets"`
	Highlight      []string         `json:"highlight,omitempty"`
	BoxStyle       *ExcerptBoxStyle `json:"box_style,omitempty"`
	HighlightColor string           `json:"highlight_color,omitempty"` // "#rrggbb"
	CropMargin     *int             `json:"crop_margin,omitempty"`     // CSS px
}

// ExcerptRect 是页面坐标（CSS px）矩形。
type ExcerptRect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// ExcerptHighlight 是一个短语命中的全部行矩形。
type ExcerptHighlight struct {
	Phrase string        `json:"phrase"`
	Rects  []ExcerptRect `json:"rects"`
}

// ExcerptCollectItem 是单条 spec 的采集结果。
type ExcerptCollectItem struct {
	Name              string             `json:"name"`
	Box               *ExcerptRect       `json:"box"` // 目标并集矩形；nil = 无任何 target 解析成功
	Highlights        []ExcerptHighlight `json:"highlights"`
	MissingTargets    []string           `json:"missing_targets"`
	MissingHighlights []string           `json:"missing_highlights"`
	PendingImages     int                `json:"pending_images"`
}

// ExcerptCollect 是一次采集的整体返回。
type ExcerptCollect struct {
	DPR        float64              `json:"dpr"`
	PageW      float64              `json:"page_w"`
	PageH      float64              `json:"page_h"`
	URL        string               `json:"url"`
	Title      string               `json:"title"`
	FixedRects []ExcerptRect        `json:"fixed_rects"`
	Items      []ExcerptCollectItem `json:"items"`
}

// excerptCollectorJS 只读采集模板：一次 Runtime.evaluate 吃整个 spec 数组。
// 不 splitText、不建 span、不加节点——高亮矩形用 document.createRange +
// Range.getClientRects() 直接取（跨内联节点天然支持）。
const excerptCollectorJS = `(() => {
const spec = __EXCERPT_SPEC_JSON__;
const sx = window.scrollX || 0, sy = window.scrollY || 0;
const dpr = window.devicePixelRatio || 1;
const de = document.documentElement, body = document.body;
const pageW = Math.max(de ? de.scrollWidth : 0, body ? body.scrollWidth : 0);
const pageH = Math.max(de ? de.scrollHeight : 0, body ? body.scrollHeight : 0);
const fixedRects = [];
const pendingImgs = [];
for (const el of document.querySelectorAll('body *')) {
  const cs = getComputedStyle(el);
  if ((cs.position === 'fixed' || cs.position === 'sticky') && cs.display !== 'none' && cs.visibility !== 'hidden') {
    const r = el.getBoundingClientRect();
    if (r.width > 1 && r.height > 1) fixedRects.push({x: r.left + sx, y: r.top + sy, w: r.width, h: r.height});
  }
}
for (const im of document.images) {
  if (!im.complete || im.naturalWidth === 0) {
    const r = im.getBoundingClientRect();
    pendingImgs.push({x: r.left + sx, y: r.top + sy, w: r.width, h: r.height});
  }
}
const SKIP = {annotation: 1, 'annotation-xml': 1, math: 1, script: 1, style: 1, noscript: 1, template: 1};
function resolveTarget(t) {
  if (t.startsWith('id:')) return document.getElementById(t.slice(3));
  if (t.startsWith('title:')) {
    const el = document.getElementById(t.slice(6));
    if (!el) return null;
    return el.querySelector('.ltx_title') || el;
  }
  try { return document.querySelector(t); } catch (e) { return null; }
}
function textNodes(root) {
  const w = document.createTreeWalker(root, NodeFilter.SHOW_TEXT, { acceptNode: n => {
    for (let p = n.parentElement; p && p !== root; p = p.parentElement) {
      if (SKIP[p.tagName.toLowerCase()]) return NodeFilter.FILTER_REJECT;
    }
    return NodeFilter.FILTER_ACCEPT;
  }});
  const out = []; let n; while ((n = w.nextNode())) out.push(n); return out;
}
const items = [];
for (const it of spec) {
  const roots = [], missingTargets = [];
  for (const t of (it.targets || [])) {
    const el = resolveTarget(t);
    if (el) roots.push(el); else missingTargets.push(t);
  }
  let box = null;
  if (roots.length) {
    let t0 = 1e9, l0 = 1e9, b0 = -1e9, r0 = -1e9;
    for (const el of roots) {
      const r = el.getBoundingClientRect();
      t0 = Math.min(t0, r.top); l0 = Math.min(l0, r.left);
      b0 = Math.max(b0, r.bottom); r0 = Math.max(r0, r.right);
    }
    box = {x: l0 + sx, y: t0 + sy, w: r0 - l0, h: b0 - t0};
  }
  const highlights = [], missingHighlights = [];
  const phrases = it.highlight || [];
  if (roots.length && phrases.length) {
    const nodes = [];
    for (const el of roots) { for (const n of textNodes(el)) nodes.push(n); }
    let all = ''; const map = [];
    for (const n of nodes) { map.push([n, all.length]); all += n.data; }
    for (const phrase of phrases) {
      const esc = phrase.replace(/[.*+?^${}()|[\]\\]/g, '\\$&').replace(/\s+/g, '\\s+');
      const m = new RegExp(esc).exec(all);
      if (!m) { missingHighlights.push(phrase); continue; }
      const s = m.index, e = s + m[0].length, rects = [];
      for (const [n, off] of map) {
        const end = off + n.data.length;
        if (end <= s || off >= e) continue;
        const a = Math.max(s, off), b = Math.min(e, end);
        const rg = document.createRange();
        rg.setStart(n, a - off); rg.setEnd(n, b - off);
        for (const cr of rg.getClientRects()) {
          if (cr.width > 0 && cr.height > 0) rects.push({x: cr.left + sx, y: cr.top + sy, w: cr.width, h: cr.height});
        }
      }
      if (rects.length) highlights.push({phrase: phrase, rects: rects});
      else missingHighlights.push(phrase);
    }
  } else {
    for (const phrase of phrases) missingHighlights.push(phrase);
  }
  items.push({
    name: it.name,
    box: box,
    highlights: highlights,
    missing_targets: missingTargets,
    missing_highlights: missingHighlights,
    pending_images: pendingImgs.filter(p => box && p.x < box.x + box.w && p.x + p.w > box.x && p.y < box.y + box.h && p.y + p.h > box.y).length
  });
}
return {dpr: dpr, page_w: pageW, page_h: pageH, url: location.href, title: document.title, fixed_rects: fixedRects, items: items};
})()`

// RunExcerptCollector 在给定 core 上执行只读采集，返回全部 spec 的矩形。
func RunExcerptCollector(ctx context.Context, core BrowserCore, specs []ExcerptSpec) (*ExcerptCollect, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("browser: excerpt spec is empty")
	}
	raw, err := json.Marshal(specs)
	if err != nil {
		return nil, fmt.Errorf("browser: excerpt spec marshal: %w", err)
	}
	js := strings.Replace(excerptCollectorJS, "__EXCERPT_SPEC_JSON__", string(raw), 1)
	var out ExcerptCollect
	if err := core.EvalJS(ctx, js, &out); err != nil {
		return nil, fmt.Errorf("browser: excerpt collect failed: %w", err)
	}
	return &out, nil
}

// ExcerptCapable 是支持摘录的 CDP core（全页 PNG 捕获）。Safari core 不实现，
// 命令层据此给出明确错误而不是静默降级。
type ExcerptCapable interface {
	BrowserCore
	CaptureFullPagePNG(ctx context.Context) ([]byte, error)
}

// excerptFullPageTimeout 全页渲染比视口截图慢，给独立预算。
const excerptFullPageTimeout = 15 * time.Second

// CaptureFullPagePNG 用 CaptureBeyondViewport 一帧截整页（PNG，文字锐）。
// 不走 CompositeBrowserChrome——摘录要裸页面，不要 persona chrome 合成。
func (impl *browserCoreImpl) CaptureFullPagePNG(ctx context.Context) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	impl.mu.RLock()
	targetID, targetCtx := impl.currentTargetRef()
	if targetID == "" {
		targetID = targetIDForContext(targetCtx)
	}
	browserWSURL := impl.wsURL
	impl.mu.RUnlock()

	capture := func(c context.Context) ([]byte, error) {
		return page.CaptureScreenshot().
			WithFormat(page.CaptureScreenshotFormatPng).
			WithCaptureBeyondViewport(true).
			Do(c)
	}
	if browserWSURL != "" && targetID != "" {
		data, err := withFreshTargetConnection(ctx, browserWSURL, targetID, excerptFullPageTimeout, capture)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w after %s", errScreenshotTimeout, excerptFullPageTimeout)
		}
		if err != nil {
			return nil, fmt.Errorf("browser: full-page screenshot failed: %w", err)
		}
		return data, nil
	}
	return runScreenshotWithTimeout(ctx, targetCtx, excerptFullPageTimeout, capture)
}

// ExcerptStyle 是合成 defaults（可被单条 spec 覆盖）。
type ExcerptStyle struct {
	BoxColor color.RGBA
	BoxWidth int    // CSS px
	HlColor  color.RGBA
	HlAlpha  uint8  // 高亮叠加不透明度（荧光笔效果：叠在文字上）
	Margin   int    // 裁剪边距 CSS px
	DPR      float64 // 截图像素 / CSS px
}

// DefaultExcerptStyle 是参考实现验证过的默认观感（红 3px 框 + 黄荧光）。
func DefaultExcerptStyle() ExcerptStyle {
	return ExcerptStyle{
		BoxColor: color.RGBA{R: 0xe0, G: 0x00, B: 0x00, A: 0xff},
		BoxWidth: 3,
		HlColor:  color.RGBA{R: 0xff, G: 0xe9, B: 0x4d, A: 0xff},
		HlAlpha:  115, // ~0.45
		Margin:   14,
		DPR:      1,
	}
}

// ParseHexColor 解析 "#rrggbb"。
func ParseHexColor(s string) (color.RGBA, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "#") || len(s) != 7 {
		return color.RGBA{}, fmt.Errorf("browser: invalid color %q (want #rrggbb)", s)
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return color.RGBA{}, fmt.Errorf("browser: invalid color %q: %w", s, err)
	}
	return color.RGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}, nil
}

// CompositeExcerpt 把全页 PNG 裁到 box+margin，画高亮与红框，返回 PNG 与
// 输出尺寸。box/highlights 均为页面 CSS 像素坐标。纯函数（同输入逐字节同
// 输出），便于像素级单测。
func CompositeExcerpt(fullPagePNG []byte, box ExcerptRect, highlights []ExcerptRect, style ExcerptStyle) ([]byte, int, int, error) {
	src, _, err := image.Decode(bytes.NewReader(fullPagePNG))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("browser: excerpt decode full-page png: %w", err)
	}
	dpr := style.DPR
	if dpr <= 0 {
		dpr = 1
	}
	bounds := src.Bounds()
	// 裁剪区（CSS px → 截图像素），夹进图内。
	cx0 := clampInt(int(math.Round((box.X-float64(style.Margin))*dpr)), bounds.Min.X, bounds.Max.X)
	cy0 := clampInt(int(math.Round((box.Y-float64(style.Margin))*dpr)), bounds.Min.Y, bounds.Max.Y)
	cx1 := clampInt(int(math.Round((box.X+box.W+float64(style.Margin))*dpr)), bounds.Min.X, bounds.Max.X)
	cy1 := clampInt(int(math.Round((box.Y+box.H+float64(style.Margin))*dpr)), bounds.Min.Y, bounds.Max.Y)
	if cx1 <= cx0 || cy1 <= cy0 {
		return nil, 0, 0, fmt.Errorf("browser: excerpt crop region empty (box %v dpr %v page %v)", box, dpr, bounds)
	}
	w, h := cx1-cx0, cy1-cy0
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), src, image.Pt(cx0, cy0), draw.Src)

	// 高亮：半透明黄叠在文字上。NRGBA=直 alpha（color.RGBA 是预乘约定，
	// 直传 α 色会把通道当预乘值用错）。
	hlUniform := image.NewUniform(color.NRGBA{R: style.HlColor.R, G: style.HlColor.G, B: style.HlColor.B, A: style.HlAlpha})
	for _, r := range highlights {
		hx0 := clampInt(int(math.Round(r.X*dpr)), cx0, cx1)
		hy0 := clampInt(int(math.Round(r.Y*dpr)), cy0, cy1)
		hx1 := clampInt(int(math.Round((r.X+r.W)*dpr)), cx0, cx1)
		hy1 := clampInt(int(math.Round((r.Y+r.H)*dpr)), cy0, cy1)
		if hx1 <= hx0 || hy1 <= hy0 {
			continue
		}
		draw.Draw(dst, image.Rect(hx0-cx0, hy0-cy0, hx1-cx0, hy1-cy0), hlUniform, image.Point{}, draw.Over)
	}

	// 红框：沿 box 边缘画四条实心条（宽度按 dpr 放大，至少 1px）。
	bw := int(math.Round(float64(style.BoxWidth) * dpr))
	if bw < 1 {
		bw = 1
	}
	bx0 := clampInt(int(math.Round(box.X*dpr)), cx0, cx1) - cx0
	by0 := clampInt(int(math.Round(box.Y*dpr)), cy0, cy1) - cy0
	bx1 := clampInt(int(math.Round((box.X+box.W)*dpr)), cx0, cx1) - cx0
	by1 := clampInt(int(math.Round((box.Y+box.H)*dpr)), cy0, cy1) - cy0
	boxUniform := image.NewUniform(style.BoxColor)
	for _, strip := range []image.Rectangle{
		image.Rect(bx0, by0, bx1, by0+bw), // top
		image.Rect(bx0, by1-bw, bx1, by1), // bottom
		image.Rect(bx0, by0, bx0+bw, by1), // left
		image.Rect(bx1-bw, by0, bx1, by1), // right
	} {
		if strip.Dx() <= 0 || strip.Dy() <= 0 {
			continue
		}
		draw.Draw(dst, strip.Intersect(dst.Bounds()), boxUniform, image.Point{}, draw.Over)
	}

	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, 0, 0, fmt.Errorf("browser: excerpt encode png: %w", err)
	}
	return out.Bytes(), w, h, nil
}

// RectsOverlap 报告两个页面坐标矩形是否相交（fixed/sticky 遮挡诊断用）。
func RectsOverlap(a, b ExcerptRect) bool {
	return a.X < b.X+b.W && a.X+a.W > b.X && a.Y < b.Y+b.H && a.Y+a.H > b.Y
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
