package audit

import _ "embed"

//go:embed scripts/cls.js
var clsScript string

// RegisterPerf 注册 perf category 的 checks 到 Registry。
func RegisterPerf(r *Registry) {
	r.Register(Check{
		ID: "cls"
		Category: "perf"
		Tags: nil
		Severity: SeverityHigh
		Description: "Cumulative Layout Shift exceeds threshold"
		Script: clsScript
		Params: map[string]any{"maxCLS": 0.1}
	})
}
