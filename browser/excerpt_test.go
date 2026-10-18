package browser

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func solidPNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return buf.Bytes()
}

// REQ-EXC-01: 合成是纯绘制——红框贴 box 边、高亮为 α 叠加、裁剪含 margin、
// 区外像素不动。
func TestCompositeExcerptDrawing(t *testing.T) {
	src := solidPNG(t, 200, 120, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	style := DefaultExcerptStyle()
	box := ExcerptRect{X: 20, Y: 10, W: 50, H: 30}
	hl := []ExcerptRect{{X: 25, Y: 15, W: 30, H: 6}}
	out, w, h, err := CompositeExcerpt(src, box, hl, style)
	if err != nil {
		t.Fatalf("composite: %v", err)
	}
	// margin 14: crop = (6, 0) .. (84, 54) → 78x54（上方 margin 被图界夹掉）
	if w != 78 || h != 54 {
		t.Fatalf("crop dims = %dx%d, want 78x54", w, h)
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	cropOrigin := image.Pt(6, 0)
	at := func(x, y int) color.RGBA {
		r, g, b, a := img.At(x-cropOrigin.X, y-cropOrigin.Y).RGBA()
		return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
	}
	// box 顶边中点 (45,10)：应为红框色。
	if c := at(45, 10); c.R < 0xe0 || c.G != 0 || c.B != 0 {
		t.Fatalf("box top edge pixel = %v, want red", c)
	}
	// 高亮内部 (30,18)：白底 × 黄 α=115 混合。
	c := at(30, 18)
	a := float64(style.HlAlpha) / 255
	wantG := uint8(255*(1-a) + float64(style.HlColor.G)*a + 0.5)
	wantB := uint8(255*(1-a) + float64(style.HlColor.B)*a + 0.5)
	if c.R != 255 || abs(int(c.G)-int(wantG)) > 2 || abs(int(c.B)-int(wantB)) > 2 {
		t.Fatalf("highlight pixel = %v, want ~(%d %d %d)", c, 255, wantG, wantB)
	}
	// box 外、高亮外的白区 (30,8)。
	if c := at(30, 8); c.R != 255 || c.G != 255 || c.B != 255 {
		t.Fatalf("untouched pixel = %v, want white", c)
	}
}

// REQ-EXC-02: DPR 缩放正确（截图 px = CSS px × dpr）。
func TestCompositeExcerptDPR(t *testing.T) {
	src := solidPNG(t, 400, 240, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	style := DefaultExcerptStyle()
	style.DPR = 2
	box := ExcerptRect{X: 20, Y: 10, W: 50, H: 30}
	_, w, h, err := CompositeExcerpt(src, box, nil, style)
	if err != nil {
		t.Fatalf("composite: %v", err)
	}
	if w != 156 || h != 108 { // css (6..84, 0..54) × dpr2
		t.Fatalf("dpr crop dims = %dx%d, want 156x108", w, h)
	}
}

func TestParseHexColor(t *testing.T) {
	c, err := ParseHexColor("#e00000")
	if err != nil || c.R != 0xe0 || c.G != 0 || c.B != 0 {
		t.Fatalf("parse #e00000 = %v %v", c, err)
	}
	for _, bad := range []string{"e00000", "#e00", "#gg0000", ""} {
		if _, err := ParseHexColor(bad); err == nil {
			t.Fatalf("parse %q should fail", bad)
		}
	}
}

func TestRectsOverlap(t *testing.T) {
	a := ExcerptRect{X: 0, Y: 0, W: 10, H: 10}
	if !RectsOverlap(a, ExcerptRect{X: 5, Y: 5, W: 10, H: 10}) {
		t.Fatal("overlapping rects reported disjoint")
	}
	if RectsOverlap(a, ExcerptRect{X: 10, Y: 0, W: 5, H: 5}) {
		t.Fatal("edge-adjacent rects must not count as overlap")
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
