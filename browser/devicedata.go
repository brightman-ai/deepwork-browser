// Package browser — 设备指纹的唯一数据源(INV-3 单一 SSOT)。
//
// 设备硬件参数(viewport/DPR/UA/mobile/touch)是客观事实,不手搓维护,而是
// vendored 自 Playwright deviceDescriptorsSource.json(Apache-2.0,持续维护)。
// browser/devicedata/devices.json 是子集(几何/UA/mobile/touch 逐字取自 Playwright;
// platform/vendor/webgl/maxTouchPoints/languages 为 dw 补充的指纹字段)。
//
// 本文件的 init() 是设备指纹的**唯一生成点**:把 JSON 载入为 FingerprintPreset 并
// 注册进 BuiltinPresets。fingerprint.go 里不再手写任何移动设备字面量(无第二份表)。
package browser

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed devicedata/devices.json
var deviceDataJSON []byte

type deviceFingerprintFile struct {
	Meta    map[string]string            `json:"_meta"`
	Devices map[string]deviceFingerprint `json:"devices"`
}

type deviceFingerprint struct {
	PlaywrightSource  string  `json:"playwright_source"`
	UserAgent         string  `json:"userAgent"`
	ViewportW         int     `json:"viewportW"`
	ViewportH         int     `json:"viewportH"`
	DeviceScaleFactor float64 `json:"deviceScaleFactor"`
	Mobile            bool    `json:"mobile"`
	Touch             bool    `json:"touch"`
	Platform          string  `json:"platform"`
	Vendor            string  `json:"vendor"`
	WebGLVendor       string  `json:"webglVendor"`
	WebGLRenderer     string  `json:"webglRenderer"`
	MaxTouchPoints    int     `json:"maxTouchPoints"`
	Languages         string  `json:"languages"`
}

// loadDeviceFingerprints 从 vendored Playwright 子集构建设备指纹。
// = 设备指纹唯一生成点(INV-3)。
func loadDeviceFingerprints() (map[string]*FingerprintPreset, error) {
	var f deviceFingerprintFile
	if err := json.Unmarshal(deviceDataJSON, &f); err != nil {
		return nil, fmt.Errorf("device data: %w", err)
	}
	out := make(map[string]*FingerprintPreset, len(f.Devices))
	for id, d := range f.Devices {
		out[id] = &FingerprintPreset{
			ID:                id,
			Name:              d.PlaywrightSource,
			UserAgent:         d.UserAgent,
			Platform:          d.Platform,
			Vendor:            d.Vendor,
			Languages:         d.Languages,
			WebGLVendor:       d.WebGLVendor,
			WebGLRenderer:     d.WebGLRenderer,
			ViewportW:         d.ViewportW,
			ViewportH:         d.ViewportH,
			DeviceScaleFactor: d.DeviceScaleFactor,
			Mobile:            d.Mobile,
			Touch:             d.Touch,
			MaxTouchPoints:    d.MaxTouchPoints,
		}
	}
	return out, nil
}

func init() {
	devs, err := loadDeviceFingerprints()
	if err != nil {
		// 嵌入数据在编译期固定;解析失败 = 程序错误,fail-fast。
		panic("browser: load device fingerprints: " + err.Error())
	}
	for id, fp := range devs {
		BuiltinPresets[id] = fp
	}
}
