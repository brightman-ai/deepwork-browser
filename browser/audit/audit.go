// Package audit 提供引擎无关的浏览器审计框架。
// 支持 Chrome / Safari / Wails WebView 等任何实现 Auditable 接口的引擎。
//
// 铁律 （继承自 browser 包）: 本包零依赖 Deepwork 上下文。
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Auditable 是引擎无关的审计接口。
// browser.BrowserCore（Chrome）和 safari.BrowserCore（Safari）均可实现此接口。
type Auditable interface {
	// EvalJS 在当前活跃页面执行 JavaScript 表达式，结果写入 result 指针。
	EvalJS(ctx context.Context, expr string, result interface{}) error

	// Screenshot 截图，annotate=true 时叠加 Element Ref 标注。
	Screenshot(ctx context.Context, annotate bool) (byte, error)
}

// RunOpts 控制单次审计行为。
type RunOpts struct {
	// Suite 按预定义 suite 名过滤（"a11y" / "compat" / "layout" / "perf" / "touch" / "ios" / "full"）。
	// 与 Checks 同时指定时，取交集。
	Suite string

	// Checks 按 ID 精确指定要运行的 checks。空表示不限制。
	Checks string

	// Threshold 分数低于此值时 Run 返回 ErrBelowThreshold。0 表示不检查。
	Threshold int

	// Context 运行时上下文，用于参数化 checks（如 IsTouch / ViewportW 等）。
	Context *AuditContext

	// URL 目标页面 URL（写入 Report，不触发导航）。
	URL string
}

// ErrBelowThreshold 分数低于 RunOpts.Threshold 时返回。
type ErrBelowThreshold struct {
	Score int
	Threshold int
}

func (e *ErrBelowThreshold) Error string {
	return fmt.Sprintf("audit: score %d below threshold %d", e.Score, e.Threshold)
}

// AuditRunner 编排 checks 执行。
type AuditRunner struct {
	registry *Registry
}

// NewRunner 创建使用指定注册表的 AuditRunner。
func NewRunner(registry *Registry) *AuditRunner {
	return &AuditRunner{registry: registry}
}

// Run 执行审计，返回 Report。
// 流程: 筛选 checks → 参数化 → 逐个 EvalJS → 聚合 Report。
func (r *AuditRunner) Run(ctx context.Context, target Auditable, opts RunOpts) (*Report, error) {
	checks := r.selectChecks(opts)

	report := &Report{
		URL: opts.URL
		Timestamp: time.Now.UTC.Format(time.RFC3339)
	}
	if opts.Context != nil {
		report.Engine = opts.Context.Engine
		report.Device = opts.Context.DeviceName
	}

	for _, check := range checks {
		result := r.runCheck(ctx, target, check, opts.Context)
		report.Checks = append(report.Checks, result)

		switch result.Status {
		case "pass":
			report.Summary.Pass++
		case "fail", "error":
			switch result.Severity {
			case SeverityCritical:
				report.Summary.Critical++
			case SeverityHigh:
				report.Summary.High++
			case SeverityMedium:
				report.Summary.Medium++
			case SeverityLow:
				report.Summary.Low++
			}
		}
	}

	report.Score = computeScore(report.Summary)

	if opts.Threshold > 0 && report.Score < opts.Threshold {
		return report, &ErrBelowThreshold{Score: report.Score, Threshold: opts.Threshold}
	}
	return report, nil
}

// selectChecks 根据 opts.Suite 和 opts.Checks 筛选 checks。
func (r *AuditRunner) selectChecks(opts RunOpts) Check {
	var pool Check

	if opts.Suite != "" {
		suite, ok := Suites[opts.Suite]
		if ok {
			for _, c := range r.registry.All {
				if suite.Filter(c) {
					pool = append(pool, c)
				}
			}
		}
	} else {
		pool = r.registry.All
	}

	if len(opts.Checks) == 0 {
		return pool
	}

	// 按 ID 过滤（取交集）
	idSet := make(map[string]bool, len(opts.Checks))
	for _, id := range opts.Checks {
		idSet[id] = true
	}
	var out Check
	for _, c := range pool {
		if idSet[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

// runCheck 执行单个 check：注入 JS → 解析结果。
func (r *AuditRunner) runCheck(ctx context.Context, target Auditable, check Check, actx *AuditContext) CheckResult {
	base := CheckResult{
		ID: check.ID
		Category: check.Category
		Severity: check.Severity
	}

	if check.Script == "" {
		base.Status = "error"
		base.Message = "check has no script"
		return base
	}

	// 参数化：将 params 序列化为 JS 变量，注入脚本前
	params := ApplyContext(&check, actx)
	script, err := injectParams(check.Script, params)
	if err != nil {
		base.Status = "error"
		base.Message = fmt.Sprintf("param injection failed: %v", err)
		return base
	}

	var raw json.RawMessage
	if err := target.EvalJS(ctx, script, &raw); err != nil {
		base.Status = "error"
		base.Message = fmt.Sprintf("EvalJS error: %v", err)
		return base
	}

	var result CheckResult
	if err := json.Unmarshal(raw, &result); err != nil {
		base.Status = "error"
		base.Message = fmt.Sprintf("result parse error: %v", err)
		return base
	}

	// 确保 ID/Category/Severity 来自 check 定义，防止 JS 篡改
	result.ID = check.ID
	result.Category = check.Category
	result.Severity = check.Severity
	return result
}

// injectParams 将 params 序列化为 JS 变量声明，前置注入脚本。
// 生成格式: "const __params = {...};\n<原脚本>"
func injectParams(script string, params map[string]any) (string, error) {
	if len(params) == 0 {
		return script, nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("const __params = %s;\n%s", b, script), nil
}
