package browser

import "testing"

// TestDeviceFingerprints_LoadedFromVendoredJSON — 设备指纹经 init() 从 vendored
// Playwright 子集载入并注册进 BuiltinPresets(单一 SSOT,INV-3)。
func TestDeviceFingerprints_LoadedFromVendoredJSON(t *testing.T) {
	deviceIDs := []string{
		PresetIPhone14, PresetIPhone15Pro, PresetIPadAir, PresetPixel7, PresetGalaxyS24,
	}
	for _, id := range deviceIDs {
		fp, ok := BuiltinPresets[id]
		if !ok {
			t.Errorf("设备指纹 %q 未从 vendored JSON 载入进 BuiltinPresets", id)
			continue
		}
		if fp.UserAgent == "" || fp.ViewportW == 0 || fp.ViewportH == 0 || fp.DeviceScaleFactor == 0 {
			t.Errorf("设备指纹 %q 字段不完整: %+v", id, fp)
		}
		if !fp.Mobile || !fp.Touch {
			t.Errorf("设备指纹 %q 应为移动+触摸设备", id)
		}
		// 必须是合法 preset(流入 pool/mux 的 PresetID 管道)
		if _, err := ValidatePresetID(id); err != nil {
			t.Errorf("设备指纹 %q 未通过 ValidatePresetID: %v", id, err)
		}
	}
}

// TestDeviceFingerprints_PlaywrightAuthoritativeValues — 校验取自 Playwright 的
// 权威几何值(而非 CP1 手搓漂移值:iPhone14 曾误作 390x844,Pixel7 曾误作 412x915)。
func TestDeviceFingerprints_PlaywrightAuthoritativeValues(t *testing.T) {
	cases := []struct {
		id      string
		w, h    int
		dpr     float64
	}{
		{PresetIPhone14, 390, 664, 3},
		{PresetIPhone15Pro, 393, 659, 3},
		{PresetPixel7, 412, 839, 2.625},
		{PresetGalaxyS24, 360, 780, 3},
	}
	for _, c := range cases {
		fp := BuiltinPresets[c.id]
		if fp == nil {
			t.Errorf("%s 未载入", c.id)
			continue
		}
		if fp.ViewportW != c.w || fp.ViewportH != c.h || fp.DeviceScaleFactor != c.dpr {
			t.Errorf("%s 几何值与 Playwright 权威源不符: 得 %dx%d dpr=%v, 期望 %dx%d dpr=%v",
				c.id, fp.ViewportW, fp.ViewportH, fp.DeviceScaleFactor, c.w, c.h, c.dpr)
		}
	}
}
