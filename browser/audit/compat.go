package audit

import (
	_ "embed"
)

//go:embed scripts/touch-targets.js
var touchTargetsScript string

//go:embed scripts/auto-zoom.js
var autoZoomScript string

//go:embed scripts/horizontal-overflow.js
var horizontalOverflowScript string

// RegisterCompat 注册 compat category 的 checks 到 Registry。
func RegisterCompat(r *Registry) {
	r.Register(Check{
		ID:          "touch-targets",
		Category:    "compat",
		Tags:        []string{"touch", "ios"},
		Severity:    SeverityCritical,
		Description: "Interactive elements smaller than minimum touch target size",
		Script:      touchTargetsScript,
		Params:      map[string]any{"minSize": 44},
	})

	r.Register(Check{
		ID:          "auto-zoom",
		Category:    "compat",
		Tags:        []string{"ios", "viewport"},
		Severity:    SeverityHigh,
		Description: "Input elements with font-size < 16px trigger iOS Safari auto-zoom on focus",
		Script:      autoZoomScript,
		Params:      map[string]any{"minFontSize": 16},
	})

	r.Register(Check{
		ID:          "horizontal-overflow",
		Category:    "compat",
		Tags:        []string{"layout", "viewport"},
		Severity:    SeverityHigh,
		Description: "Page or elements cause horizontal scrolling beyond viewport width",
		Script:      horizontalOverflowScript,
		Params:      map[string]any{},
	})
}
