package audit

import _ "embed"

//go:embed scripts/stacking-context.js
var stackingContextScript string

//go:embed scripts/scroll-lock.js
var scrollLockScript string

// RegisterLayout 注册 layout category 的 checks 到 Registry。
func RegisterLayout(r *Registry) {
	r.Register(Check{
		ID:          "stacking-context",
		Category:    "layout",
		Tags:        nil,
		Severity:    SeverityMedium,
		Description: "Fixed/sticky elements have z-index conflicts or create unexpected stacking contexts",
		Script:      stackingContextScript,
	})
	r.Register(Check{
		ID:          "scroll-lock",
		Category:    "layout",
		Tags:        nil,
		Severity:    SeverityMedium,
		Description: "Page scroll is locked (overflow:hidden) without a visible modal or dialog",
		Script:      scrollLockScript,
	})
}
