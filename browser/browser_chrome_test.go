package browser

// browser chrome 仿真单测（spec: docs/product/browser-chrome/ REQ-BC-02/03/05/10 的 unit oracle）。

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// REQ-BC-10: iOS 预设携带 chrome 几何且与视口 SSOT 自洽；无数据设备明确为 nil。
func TestBrowserChromeGeometryFromSSOT(t *testing.T) {
	for _, id := range []string{PresetIPhone14, PresetIPhone15Pro} {
		p := BuiltinPresets[id]
		if p == nil || p.BrowserChrome == nil {
			t.Fatalf("%s: expected browserChrome geometry in SSOT", id)
		}
		bc := p.BrowserChrome
		if bc.SmallViewportH() != p.ViewportH {
			t.Errorf("%s: svh %d != preset viewportH %d", id, bc.SmallViewportH(), p.ViewportH)
		}
		if bc.LargeViewportH() <= bc.SmallViewportH() {
			t.Errorf("%s: lvh %d must exceed svh %d", id, bc.LargeViewportH(), bc.SmallViewportH())
		}
		if bc.OcclusionBandH() <= 0 {
			t.Errorf("%s: occlusion band %d must be > 0", id, bc.OcclusionBandH())
		}
	}
	for _, id := range []string{PresetIPadAir, PresetPixel7, PresetLinuxChrome} {
		if p := BuiltinPresets[id]; p == nil || p.BrowserChrome != nil {
			t.Errorf("%s: expected nil browserChrome (data not yet recorded)", id)
		}
	}
}

// REQ-BC-03: auto/on/off 判定语义。
func TestResolveBrowserChromeMode(t *testing.T) {
	iphone := BuiltinPresets[PresetIPhone14]
	desktop := BuiltinPresets[PresetLinuxChrome]
	cases := []struct {
		name             string
		mode             string
		preset           *FingerprintPreset
		explicitViewport bool
		want             bool
		wantErr          bool
	}{
		{"auto+iphone", "auto", iphone, false, true, false},
		{"空默认=auto", "", iphone, false, true, false},
		{"auto+desktop", "auto", desktop, false, false, false},
		{"auto+iphone+显式viewport", "auto", iphone, true, false, false},
		{"off+iphone", "off", iphone, false, false, false},
		{"on+iphone", "on", iphone, false, true, false},
		{"on+desktop=fail-loud", "on", desktop, false, false, true},
		{"on+显式viewport=fail-loud", "on", iphone, true, false, true},
		{"非法值", "always", iphone, false, false, true},
		{"nil preset auto", "auto", nil, false, false, false},
	}
	for _, c := range cases {
		got, err := ResolveBrowserChromeMode(c.mode, c.preset, c.explicitViewport)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// makeTestShot 生成 lvh 视口尺寸的 PNG 测试截图（内容由 seed 决定，模拟不同页面）。
func makeTestShot(t *testing.T, spec *BrowserChromeSpec, w int, seed uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, spec.LargeViewportH()))
	for y := 0; y < spec.LargeViewportH(); y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x) + seed, G: uint8(y) * seed, B: seed, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// REQ-BC-02 (unit oracle): chrome 层不变性 ——
//  1. 不同页面内容 → 遮挡带像素逐一相同（chrome 不随页面变）；
//  2. 带上方像素 = 原页面像素（chrome 不越界污染页面区）；
//  3. 同输入两次合成 → 逐字节相同（确定性）。
func TestCompositeChromeInvariance(t *testing.T) {
	spec := BuiltinPresets[PresetIPhone14].BrowserChrome
	w := BuiltinPresets[PresetIPhone14].ViewportW

	shotA := makeTestShot(t, spec, w, 3)
	shotB := makeTestShot(t, spec, w, 199)

	outA1, err := CompositeBrowserChrome(shotA, spec, "#1c1c1e", false, false)
	if err != nil {
		t.Fatal(err)
	}
	outA2, _ := CompositeBrowserChrome(shotA, spec, "#1c1c1e", false, false)
	outB, _ := CompositeBrowserChrome(shotB, spec, "#1c1c1e", false, false)

	if !bytes.Equal(outA1, outA2) {
		t.Fatal("determinism: same input must produce byte-identical output")
	}

	imgA, _, err := image.Decode(bytes.NewReader(outA1))
	if err != nil {
		t.Fatal(err)
	}
	imgB, _, _ := image.Decode(bytes.NewReader(outB))
	orig, _, _ := image.Decode(bytes.NewReader(shotA))

	bandTop := spec.SmallViewportH()
	lvh := spec.LargeViewportH()
	for y := bandTop; y < lvh; y += 7 {
		for x := 0; x < w; x += 11 {
			if imgA.At(x, y) != imgB.At(x, y) {
				t.Fatalf("chrome band pixel (%d,%d) differs across page contents — chrome layer must be invariant", x, y)
			}
		}
	}
	for y := 0; y < bandTop; y += 13 {
		for x := 0; x < w; x += 17 {
			if imgA.At(x, y) != orig.At(x, y) {
				t.Fatalf("page pixel (%d,%d) above chrome band was mutated by composite", x, y)
			}
		}
	}
}

// REQ-BC-09 (system oracle 的 unit 半边): annotate 只影响遮挡带（红描边），默认截图无标注。
func TestCompositeAnnotateOutline(t *testing.T) {
	spec := BuiltinPresets[PresetIPhone14].BrowserChrome
	w := BuiltinPresets[PresetIPhone14].ViewportW
	shot := makeTestShot(t, spec, w, 42)

	plain, _ := CompositeBrowserChrome(shot, spec, "", false, false)
	annotated, _ := CompositeBrowserChrome(shot, spec, "", true, false)
	if bytes.Equal(plain, annotated) {
		t.Fatal("annotate=true must draw the occlusion outline (outputs identical)")
	}
	imgP, _, _ := image.Decode(bytes.NewReader(plain))
	imgN, _, _ := image.Decode(bytes.NewReader(annotated))
	for y := 0; y < spec.SmallViewportH()-1; y += 5 {
		for x := 0; x < w; x += 7 {
			if imgP.At(x, y) != imgN.At(x, y) {
				t.Fatalf("annotate mutated page pixel (%d,%d) above the band", x, y)
			}
		}
	}
}

// 主题取色：深色页 → 深色底栏（IMG_5171 黑底栏场景），浅色/未知 → 浅色缺省。
func TestPaletteForTheme(t *testing.T) {
	dark := paletteForTheme("#000000")
	if lum := int(dark.Bar.R) + int(dark.Bar.G) + int(dark.Bar.B); lum > 200 {
		t.Errorf("dark page must yield dark bar, got %v", dark.Bar)
	}
	light := paletteForTheme("rgb(255, 255, 255)")
	if lum := int(light.Bar.R) + int(light.Bar.G) + int(light.Bar.B); lum < 500 {
		t.Errorf("light page must yield light bar, got %v", light.Bar)
	}
	fallback := paletteForTheme("not-a-color")
	if fallback.Bar.A != 255 {
		t.Errorf("unparseable theme must fall back to opaque default")
	}
}

// REQ-BC-05 (unit oracle): 视觉视口投影 —— 遮挡判定的坐标数学。
func TestProjectPageYToScreen(t *testing.T) {
	// scale=1 无平移：恒等
	if got := ProjectPageYToScreen(700, 0, 1); got != 700 {
		t.Errorf("identity: got %v", got)
	}
	// zoom 2x 无平移：页面 y=350 投影到屏幕 700（遮挡带内）
	if got := ProjectPageYToScreen(350, 0, 2); got != 700 {
		t.Errorf("zoom2: got %v", got)
	}
	// zoom 2x + 视口平移到页面 y=300：页面 y=650 → (650-300)*2 = 700
	if got := ProjectPageYToScreen(650, 300, 2); got != 700 {
		t.Errorf("zoom2+pan: got %v", got)
	}
	// scale<=0 容错按 1
	if got := ProjectPageYToScreen(500, 0, 0); got != 500 {
		t.Errorf("zero-scale fallback: got %v", got)
	}
}

// zoom 参数解析：范围 [1,5]，reset=1，越界/非法 fail-loud。
func TestParseZoomFactor(t *testing.T) {
	for _, ok := range []struct {
		in   string
		want float64
	}{{"reset", 1}, {"1", 1}, {"2", 2}, {"1.5", 1.5}, {"5", 5}} {
		got, err := parseZoomFactor(ok.in)
		if err != nil || got != ok.want {
			t.Errorf("parseZoomFactor(%q) = %v, %v; want %v", ok.in, got, err, ok.want)
		}
	}
	for _, bad := range []string{"0.5", "0", "6", "abc", "-1"} {
		if _, err := parseZoomFactor(bad); err == nil {
			t.Errorf("parseZoomFactor(%q) must fail", bad)
		}
	}
}

// act "zoom" 解析进入 op 表；错误语法 fail-loud（fill 语法同款纪律）。
func TestParseActionZoom(t *testing.T) {
	pa, err := ParseAction("zoom 2")
	if err != nil || pa.Op != "zoom" || pa.Value != "2" {
		t.Fatalf("ParseAction zoom 2: %+v, %v", pa, err)
	}
	if pa, err = ParseAction("zoom reset"); err != nil || pa.Value != "reset" {
		t.Fatalf("ParseAction zoom reset: %+v, %v", pa, err)
	}
	if _, err = ParseAction("zoom"); err == nil || !strings.Contains(err.Error(), "zoom") {
		t.Fatalf("bare zoom must fail with guidance, got %v", err)
	}
}

// --- REQ-BC-11/12 视口事实 + 键盘态 ---

func kbSpec() *BrowserChromeSpec {
	return &BrowserChromeSpec{Style: "safari-bottom-bar", ScreenH: 852, StatusBarH: 59,
		BottomBarExpandedH: 134, BottomBarCollapsedH: 34, HomeIndicatorH: 34, KeyboardH: 336}
}

func TestKeyboardGeometryFromSSOT(t *testing.T) {
	s := kbSpec()
	if got := s.KeyboardInsetH(); got != 302 { // 336 - 34
		t.Fatalf("KeyboardInsetH = %d, want 302", got)
	}
	if got := s.KeyboardTopY(); got != 457 { // 759 - 302
		t.Fatalf("KeyboardTopY = %d, want 457", got)
	}
	if err := s.Validate(659); err != nil {
		t.Fatalf("Validate with keyboardH: %v", err)
	}
	// 无键盘几何 → inset 0（键盘仿真不可用，非报错）
	s2 := *s
	s2.KeyboardH = 0
	if s2.KeyboardInsetH() != 0 {
		t.Fatalf("KeyboardH=0 must yield inset 0")
	}
	// 键盘比 chrome 带还矮 = 数据错误，载入期 fail-fast
	s3 := *s
	s3.KeyboardH = 100
	if err := s3.Validate(659); err == nil {
		t.Fatalf("keyboardH smaller than chrome band must fail Validate")
	}
}

func TestOcclusionZonesKeyboardState(t *testing.T) {
	s := kbSpec()
	normal := s.OcclusionZones(393, false)
	if len(normal) != 1 || normal[0].Y != 659 || normal[0].State != "expanded" {
		t.Fatalf("normal zones wrong: %+v", normal)
	}
	kb := s.OcclusionZones(393, true)
	if len(kb) != 1 || kb[0].Y != 457 || kb[0].H != 302 || kb[0].State != "keyboard" {
		t.Fatalf("keyboard zones wrong: %+v", kb)
	}
	// 无键盘几何时 keyboard=true 退回 chrome 带（不造假区）
	s2 := *s
	s2.KeyboardH = 0
	fallback := s2.OcclusionZones(393, true)
	if len(fallback) != 1 || fallback[0].State != "expanded" {
		t.Fatalf("no-keyboard fallback wrong: %+v", fallback)
	}
}

func TestViewportFactsShimJS(t *testing.T) {
	s := kbSpec()
	js := s.ViewportFactsShimJS()
	for _, want := range []string{"chromeInset: 100", "__dwViewportFacts", "visualViewport", "innerHeight", "dispatchEvent"} {
		if !strings.Contains(js, want) {
			t.Fatalf("shim JS missing %q", want)
		}
	}
	show := s.KeyboardStateJS(true)
	if !strings.Contains(show, "kbInset: 302") || !strings.Contains(show, "457") {
		t.Fatalf("keyboard show JS wrong: %s", show)
	}
	hide := s.KeyboardStateJS(false)
	if !strings.Contains(hide, "kbInset: 0") {
		t.Fatalf("keyboard hide JS wrong: %s", hide)
	}
}

func TestCompositeKeyboardState(t *testing.T) {
	s := kbSpec()
	shot := makeTestShot(t, s, 393, 7)
	plain, err := CompositeBrowserChrome(shot, s, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	kb1, err := CompositeBrowserChrome(shot, s, "", false, true)
	if err != nil {
		t.Fatal(err)
	}
	kb2, _ := CompositeBrowserChrome(shot, s, "", false, true)
	if bytes.Equal(plain, kb1) {
		t.Fatalf("keyboard composite must differ from chrome-bar composite")
	}
	if !bytes.Equal(kb1, kb2) {
		t.Fatalf("keyboard composite must be deterministic")
	}
}

func TestParseActionKeyboard(t *testing.T) {
	for _, val := range []string{"show", "hide"} {
		p, err := ParseAction("keyboard " + val)
		if err != nil || p.Op != "keyboard" || p.Value != val {
			t.Fatalf("keyboard %s parse failed: %+v %v", val, p, err)
		}
	}
	if _, err := ParseAction("keyboard"); err == nil {
		t.Fatalf("bare keyboard must fail with guidance")
	}
	e := newActionEngine(nil)
	if err := e.executeKeyboard(context.Background(), "show"); err == nil || !strings.Contains(err.Error(), "persona mobile") {
		t.Fatalf("keyboard without controller must fail with --persona mobile guidance, got %v", err)
	}
	installed := false
	e.setKeyboardController(func(ctx context.Context, show bool) error { installed = show; return nil })
	if err := e.executeKeyboard(context.Background(), "bogus"); err == nil {
		t.Fatalf("keyboard bogus must fail")
	}
	if err := e.executeKeyboard(context.Background(), "show"); err != nil || !installed || !e.KeyboardVisible() {
		t.Fatalf("keyboard show via controller failed: %v", err)
	}
	if err := e.executeKeyboard(context.Background(), "hide"); err != nil || e.KeyboardVisible() {
		t.Fatalf("keyboard hide via controller failed: %v", err)
	}
}
