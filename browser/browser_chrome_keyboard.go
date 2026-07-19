// Package browser — iOS 软键盘截图合成绘制（spec: docs/product/browser-chrome/）。
//
// 意符级复刻：只画版面骨架（候选条/键位色块占位），不渲染字符（无字体依赖），
// 不做抗锯齿（既有 fillRect/fillRoundedRect 整数扫描线原语，逐字节确定性）。
// 所有尺寸从传入的 band 矩形按比例推导 —— 键盘几何跟随 band 大小/位置，
// 不出现绝对像素字面量，与 browser_chrome.go 的既有绘制原语保持同一纪律。
package browser

import (
	"image"
	"image/color"
)

// drawKeyboard 在 band 矩形内绘制 iOS 软键盘意符（顶部 QuickType 候选条 +
// 三行字母键 + 底行[123/space/return]）。纯函数确定性：同输入逐字节同输出。
// pal 提供主题配色（深色页深键盘）。所有尺寸按 band 比例推导，无绝对像素字面量。
func drawKeyboard(img *image.RGBA, band image.Rectangle, pal chromePalette) {
	if band.Dx() <= 0 || band.Dy() <= 0 {
		return
	}

	// 越界防护：一律先与 band 求交再落笔 —— 即便某段比例推导在极小 band
	// 下算出溢出矩形，band 外像素也恒不受影响（TestDrawKeyboardBounds 的
	// 不变量在绘制入口就锁死，不依赖后续每处算术都精确不溢出）。
	put := func(r image.Rectangle, c color.RGBA) {
		fillRect(img, r.Intersect(band), c)
	}
	putRounded := func(r image.Rectangle, radius int, c color.RGBA) {
		fillRoundedRect(img, r.Intersect(band), radius, c)
	}

	x0, y0 := band.Min.X, band.Min.Y
	w, h := band.Dx(), band.Dy()

	// 键盘背景：从 pal.Bar 派生。深色页 pal.Bar 已深，再 darken 仍是深键盘；
	// 浅色页 pal.Bar 偏白，darken 后是 iOS 键盘那种浅灰底 —— 不判断主题
	// 深浅、直接派生，保持纯函数。
	keyboardBG := darken(pal.Bar, 8)
	put(band, keyboardBG)

	sideMargin := maxInt(1, w*2/100)
	keyGap := maxInt(1, w*1/100)

	// ---- 顶部 QuickType 候选条：band 高度的前 12%，3 个等宽圆角占位条 ----
	candTop := y0
	candBottom := y0 + h*12/100
	candPadY := maxInt(1, (candBottom-candTop)*20/100)
	pillTop := candTop + candPadY
	pillBottom := candBottom - candPadY
	if pillBottom < pillTop {
		pillBottom = pillTop
	}

	candGap := maxInt(1, w*2/100)
	candW := (w - 2*sideMargin - 2*candGap) / 3
	candRadius := maxInt(1, (pillBottom-pillTop)/4)
	cx := x0 + sideMargin
	for i := 0; i < 3; i++ {
		putRounded(image.Rect(cx, pillTop, cx+candW, pillBottom), candRadius, pal.Pill)
		cx += candW + candGap
	}

	// ---- 中部三行字母键：12%~78% ----
	lettersTop := candBottom
	lettersBottom := y0 + h*78/100
	rowsAreaH := lettersBottom - lettersTop
	rowGap := maxInt(1, rowsAreaH*4/100)
	rowH := (rowsAreaH - rowGap*2) / 3
	keyRadius := maxInt(1, rowH*20/100)

	row1Y0 := lettersTop
	row1Y1 := row1Y0 + rowH
	row2Y0 := row1Y1 + rowGap
	row2Y1 := row2Y0 + rowH
	row3Y0 := row2Y1 + rowGap
	row3Y1 := row3Y0 + rowH

	// row1：10 键，占满 [sideMargin, w-sideMargin]，键间留缝透出背景。
	row1Gaps := keyGap * 9
	keyW := (w - 2*sideMargin - row1Gaps) / 10
	cx = x0 + sideMargin
	for i := 0; i < 10; i++ {
		putRounded(image.Rect(cx, row1Y0, cx+keyW, row1Y1), keyRadius, pal.Pill)
		cx += keyW + keyGap
	}

	// row2：9 键，左右各缩进半个键位（对齐 row1 键缝，iOS 键盘惯例）。
	indent2 := sideMargin + (keyW+keyGap)/2
	cx = x0 + indent2
	for i := 0; i < 9; i++ {
		putRounded(image.Rect(cx, row2Y0, cx+keyW, row2Y1), keyRadius, pal.Pill)
		cx += keyW + keyGap
	}

	// row3：左 shift + 7 字母键 + 右 delete，两侧功能键画成稍宽圆角块。
	lettersW3 := keyW*7 + keyGap*6
	funcTotalW := (w - 2*sideMargin) - lettersW3 - keyGap*2
	funcKeyW := funcTotalW / 2
	if funcKeyW < keyW {
		funcKeyW = keyW
	}
	cx = x0 + sideMargin
	putRounded(image.Rect(cx, row3Y0, cx+funcKeyW, row3Y1), keyRadius, pal.Pill) // shift
	cx += funcKeyW + keyGap
	for i := 0; i < 7; i++ {
		putRounded(image.Rect(cx, row3Y0, cx+keyW, row3Y1), keyRadius, pal.Pill)
		cx += keyW + keyGap
	}
	rightFuncX := x0 + w - sideMargin - funcKeyW
	if rightFuncX < cx {
		rightFuncX = cx
	}
	putRounded(image.Rect(rightFuncX, row3Y0, rightFuncX+funcKeyW, row3Y1), keyRadius, pal.Pill) // delete

	// ---- 底行：左 [123] 小块 / 中 space 条(~50%宽) / 右 [return] 块：78%~90% ----
	bottomTop := lettersBottom
	bottomBottom := y0 + h*90/100
	bottomRadius := maxInt(1, (bottomBottom-bottomTop)*20/100)

	spaceW := w * 50 / 100
	sideSpace := w - 2*sideMargin - spaceW - 2*keyGap
	leftW := sideSpace / 2
	rightW := sideSpace - leftW
	if leftW < 1 {
		leftW = 1
	}
	if rightW < 1 {
		rightW = 1
	}

	cx = x0 + sideMargin
	putRounded(image.Rect(cx, bottomTop, cx+leftW, bottomBottom), bottomRadius, pal.Pill) // 123
	cx += leftW + keyGap
	putRounded(image.Rect(cx, bottomTop, cx+spaceW, bottomBottom), bottomRadius, pal.Pill) // space
	cx += spaceW + keyGap
	rightX := x0 + w - sideMargin - rightW
	if rightX < cx {
		rightX = cx
	}
	putRounded(image.Rect(rightX, bottomTop, rightX+rightW, bottomBottom), bottomRadius, pal.Pill) // return

	// 底部 ~10%（90%~100%）：home indicator 区，只留背景 —— 已在函数开头
	// 整 band 填色时覆盖，indicator 本身由外层调用方画（不在此函数职责内）。
}
