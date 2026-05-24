package audit

import _ "embed"

//go:embed scripts/focus-appearance.js
var focusAppearanceScript string

//go:embed scripts/axe-core-wrapper.js
var axeCoreWrapperScript string

// RegisterA11y 注册 a11y category 的 checks 到 Registry。
func RegisterA11y(r *Registry) {
	r.Register(Check{
		ID:          "focus-appearance",
		Category:    "a11y",
		Tags:        []string{"focus", "wcag"},
		Severity:    SeverityHigh,
		Description: "Focus indicators must be visible (WCAG 2.4.11)",
		Script:      focusAppearanceScript,
		Params:      map[string]any{"minOutlineWidth": 2},
	})

	r.Register(Check{
		ID:          "axe-core",
		Category:    "a11y",
		Tags:        []string{"wcag", "aria"},
		Severity:    SeverityHigh,
		Description: "WCAG 2.2 automated checks via axe-core",
		Script:      axeCoreWrapperScript,
	})
}
