package browser

// iOS 软键盘意符绘制单测（drawKeyboard，spec: docs/product/browser-chrome/）。

import (
	"image"
	"image/color"
	"testing"
)

// makeKeyboardTestPal 构造一对浅色/深色主题配色（复用既有 paletteForTheme，
// 与 TestPaletteForTheme 同一取色路径 —— 不在测试里另造第二份配色规则）。
func makeKeyboardTestPals() (light, dark chromePalette) {
	return paletteForTheme("rgb(255, 255, 255)"), paletteForTheme("#000000")
}

// REQ: drawKeyboard 纯函数确定性 —— 同输入两次绘制逐字节相同。
func TestDrawKeyboardDeterministic(t *testing.T) {
	band := image.Rect(0, 0, 390, 260)
	pal := paletteForTheme("#1c1c1e")

	img1 := image.NewRGBA(band)
	img2 := image.NewRGBA(band)
	drawKeyboard(img1, band, pal)
	drawKeyboard(img2, band, pal)

	if len(img1.Pix) != len(img2.Pix) {
		t.Fatalf("pixel buffer length mismatch: %d vs %d", len(img1.Pix), len(img2.Pix))
	}
	for i := range img1.Pix {
		if img1.Pix[i] != img2.Pix[i] {
			t.Fatalf("determinism violated at byte %d: %d vs %d", i, img1.Pix[i], img2.Pix[i])
		}
	}
}

// REQ: drawKeyboard 绘制不越 band 矩形 —— band 外像素（哨兵色）恒不受影响。
func TestDrawKeyboardBounds(t *testing.T) {
	// 画布比 band 大，四周留出哨兵边界。
	full := image.Rect(0, 0, 500, 400)
	band := image.Rect(40, 60, 460, 340)
	sentinel := color.RGBA{R: 0x11, G: 0x22, B: 0x33, A: 0xFF}

	img := image.NewRGBA(full)
	for y := full.Min.Y; y < full.Max.Y; y++ {
		for x := full.Min.X; x < full.Max.X; x++ {
			img.SetRGBA(x, y, sentinel)
		}
	}

	pal := paletteForTheme("#1c1c1e")
	drawKeyboard(img, band, pal)

	for y := full.Min.Y; y < full.Max.Y; y++ {
		for x := full.Min.X; x < full.Max.X; x++ {
			if (image.Point{X: x, Y: y}).In(band) {
				continue
			}
			if got := img.RGBAAt(x, y); got != sentinel {
				t.Fatalf("pixel (%d,%d) outside band was mutated: got %v want sentinel %v", x, y, got, sentinel)
			}
		}
	}
}

// REQ: 浅色 pal 与深色 pal 绘制结果必须不同（键盘随页面主题变色）。
func TestDrawKeyboardThemeVariance(t *testing.T) {
	band := image.Rect(0, 0, 390, 260)
	light, dark := makeKeyboardTestPals()

	imgLight := image.NewRGBA(band)
	imgDark := image.NewRGBA(band)
	drawKeyboard(imgLight, band, light)
	drawKeyboard(imgDark, band, dark)

	if len(imgLight.Pix) != len(imgDark.Pix) {
		t.Fatalf("pixel buffer length mismatch: %d vs %d", len(imgLight.Pix), len(imgDark.Pix))
	}
	same := true
	for i := range imgLight.Pix {
		if imgLight.Pix[i] != imgDark.Pix[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("light and dark theme keyboards must differ but produced byte-identical output")
	}
}
