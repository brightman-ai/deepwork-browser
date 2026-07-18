package audit

import (
	_ "embed"
)

//go:embed scripts/browser-chrome-occlusion.js
var browserChromeOcclusionScript string

// RegisterChrome 注册 browser chrome 仿真相关 checks（spec: docs/product/browser-chrome/）。
// zones 参数由 CLI 在会话启用仿真时经 Registry.ByID().Params 注入（几何 SSOT =
// 设备预设 browserChrome 字段）；未注入(空) → 脚本自判"不适用"返回 pass。
func RegisterChrome(r *Registry) {
	r.Register(Check{
		ID:          "browser-chrome-occlusion",
		Category:    "compat",
		Tags:        []string{"ios", "viewport", "chrome"},
		Severity:    SeverityHigh,
		Description: "Interactive/content elements occluded by simulated mobile browser chrome (Safari bottom bar); pages declaring dvh/safe-area are marked protected (false-positive suppression)",
		Script:      browserChromeOcclusionScript,
		Params:      map[string]any{"zones": []any{}},
	})
}
