package testing

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// primitive 是单个断言原语的实现函数。
// 返回 (passed bool, reason string)。
type primitive func(obs *Observation, args string) (bool, string)

// primitives 是所有已注册断言原语的查找表。
var primitives = map[string]primitive{
	"exists":                  evalExists,
	"count":                   evalCount,
	"text_contains":           evalTextContains,
	"url_matches":             evalURLMatches,
	"tab_count":               evalTabCount,
	"active_tab_url_contains": evalActiveTabURLContains,
	"active_tab_matches_url":  evalActiveTabURLContains,
	"console_errors_count":    evalConsoleErrorsCount,
	"network_failures_count":  evalNetworkFailuresCount,
	"latency_lt":              evalLatencyLt,
	"visible":                 evalVisible,
	"gone":                    evalGone,
	"region_non_empty":        evalRegionNonEmpty,
	"region_not_overlapped":   evalRegionNotOverlapped,
	// SUT-side transcript + file oracles
	"transcript_tool_count":    evalTranscriptToolCountStub,
	"transcript_has":           evalTranscriptHasDone,
	"transcript_error_count":   evalTranscriptErrorCountStub,
	"transcript_done_count":    evalTranscriptDoneCountStub,
	"transcript_text_contains": evalTranscriptTextContains,
	"transcript_format_v1":     evalTranscriptFormatV1,
	"file_glob_count":          evalFileGlobCountStub,
}

// transcriptCountFuncs lists the primitives that require a comparison suffix
// (analogous to count/tab_count). Registered here so Evaluate can route them.
var transcriptCountFuncs = map[string]bool{
	"transcript_tool_count":  true,
	"transcript_error_count": true,
	"transcript_done_count":  true,
	"file_glob_count":        true,
}

// evalTranscriptToolCountStub is a placeholder called without comparison — blocked.
func evalTranscriptToolCountStub(obs *Observation, args string) (bool, string) {
	return false, "BLOCKED: transcript_tool_count requires comparison operator (e.g. >= 3)"
}

// evalTranscriptErrorCountStub is a placeholder called without comparison — blocked.
func evalTranscriptErrorCountStub(obs *Observation, args string) (bool, string) {
	return false, "BLOCKED: transcript_error_count requires comparison operator (e.g. == 0)"
}

// evalTranscriptDoneCountStub is a placeholder called without comparison — blocked.
func evalTranscriptDoneCountStub(obs *Observation, args string) (bool, string) {
	return false, "BLOCKED: transcript_done_count requires comparison operator (e.g. >= 1)"
}

// evalFileGlobCountStub is a placeholder called without comparison — blocked.
func evalFileGlobCountStub(obs *Observation, args string) (bool, string) {
	return false, "BLOCKED: file_glob_count requires comparison operator (e.g. >= 3)"
}

// exprPattern 匹配 "funcName(args) [op value]" 形式的表达式。
// group 1: 函数名, group 2: 括号内参数, group 3: 可选比较部分 (">= 1")
var exprPattern = regexp.MustCompile(`^(\w+)\(([^)]*)\)\s*(.*)$`)

// simplePattern 匹配 "name op value" 形式（无括号）的简单表达式。
// group 1: 变量名, group 2: 运算符, group 3: 整数值
var simplePattern = regexp.MustCompile(`^(\w+)\s*(==|>=|<=|!=|>|<)\s*(\d+)$`)

// argPattern 用于从 key='value' 形式的参数中提取键值对。
var argPattern = regexp.MustCompile(`(\w+)='([^']*)'`)

// AssertionEngine 执行 Assertion DSL 断言。所有判定均为确定性（confidence 固定 1.0）。
// 当 Vision 非 nil 时，using 包含 "visual" 的断言会额外触发 VLM 视觉判定。
type AssertionEngine struct {
	Vision *VisionOracle // 可选，nil 表示不启用 visual oracle
}

// Evaluate 解析并执行一条断言表达式，返回 AssertionResult。
// 未匹配的表达式返回 StatusBlocked。
// 若 e.Vision 非 nil 且推断/显式 using 含 "visual"，自动触发 VLM 视觉判定 overlay。
func (e *AssertionEngine) Evaluate(obs *Observation, expr string) *AssertionResult {
	return e.EvaluateWithUsing(obs, expr, nil)
}

// EvaluateWithUsing 解析并执行一条断言，using 可覆盖推断值。
// 若 using 含 "visual" 且 e.Vision 非 nil，自动触发 VLM 视觉判定 overlay。
func (e *AssertionEngine) EvaluateWithUsing(obs *Observation, expr string, using []string) *AssertionResult {
	expr = strings.TrimSpace(expr)
	result := &AssertionResult{
		Schema:     "dw.check.v1",
		Assertion:  expr,
		Confidence: 1.0,
	}

	// 尝试 funcName(args) [op value] 形式
	if m := exprPattern.FindStringSubmatch(expr); m != nil {
		funcName := m[1]
		funcArgs := m[2]
		comparison := strings.TrimSpace(m[3])

		fn, ok := primitives[funcName]
		if !ok {
			result.Status = StatusBlocked
			result.Reason = fmt.Sprintf("unknown primitive: %q", funcName)
			return result
		}

		// count 类原语需要把比较部分透传进去
		if funcName == "count" || funcName == "tab_count" {
			passed, reason := evalCountWithComparison(obs, funcArgs, comparison, funcName)
			result.Using = inferUsing(funcName)
			result.Status = boolToStatus(passed)
			result.Reason = reason
		} else if transcriptCountFuncs[funcName] {
			// transcript/file count primitives that require a comparison operator
			var passed bool
			var reason string
			switch funcName {
			case "transcript_tool_count":
				passed, reason = evalTranscriptToolCountWithComparison(obs, funcArgs, comparison)
			case "transcript_error_count":
				passed, reason = evalTranscriptErrorCountWithComparison(obs, funcArgs, comparison)
			case "transcript_done_count":
				passed, reason = evalTranscriptDoneCountWithComparison(obs, comparison)
			case "file_glob_count":
				passed, reason = evalFileGlobCountWithComparison(obs, funcArgs, comparison)
			default:
				passed, reason = false, fmt.Sprintf("BLOCKED: unhandled count primitive %q", funcName)
			}
			result.Using = inferUsing(funcName)
			result.Status = boolToStatus(passed)
			result.Reason = reason
		} else {
			passed, reason := fn(obs, funcArgs)
			result.Using = inferUsing(funcName)
			result.Status = boolToStatus(passed)
			result.Reason = reason
		}
	} else if m := simplePattern.FindStringSubmatch(expr); m != nil {
		// 尝试简单比较形式: "tab_count == 2"
		name := m[1]
		op := m[2]
		rhs, _ := strconv.ParseInt(m[3], 10, 64)

		fn, ok := primitives[name]
		if !ok {
			result.Status = StatusBlocked
			result.Reason = fmt.Sprintf("unknown primitive: %q", name)
			return result
		}
		// 对于简单标量原语，将 op+rhs 封装进 args
		passed, reason := fn(obs, fmt.Sprintf("%s %s", op, m[3]))
		_ = rhs
		result.Using = inferUsing(name)
		result.Status = boolToStatus(passed)
		result.Reason = reason
	} else {
		result.Status = StatusBlocked
		result.Reason = fmt.Sprintf("cannot parse assertion expression: %q", expr)
		// Don't return: if caller passes using:[visual], fall through to the visual oracle
		// overlay below so free-form natural-language assertions can be evaluated by VLM.
	}

	// 覆盖 using（若调用方显式传入）——但 inferUsing 是内在类的唯一 SSOT，调用者
	// 只能窄化/一致，不能改类。structural predicate 被标成 acceptance(visual/behavior)
	// → oracle-class 冲突：a11y 断言不得充当 UX 视觉/行为验收的唯一依据。
	if len(using) > 0 {
		if conflict, msg := usingClassConflict(result.Using, using); conflict {
			result.Using = using
			result.OracleWarning = msg
			// Endgame intent (no-compromise): oracle-class conflict is a HARD REJECT
			// by default — an a11y/structural predicate must never be the sole pass
			// basis for a visual/behavioral acceptance. Opt-DOWN escape for migration
			// only: DW_BROWSER_ORACLE_WARN_ONLY=1 downgrades REJECT to a stderr warning.
			if os.Getenv("DW_BROWSER_ORACLE_WARN_ONLY") == "1" {
				fmt.Fprintf(os.Stderr, "[dw-browser] oracle-class warning (WARN_ONLY): %s\n", msg)
			} else {
				result.Status = StatusBlocked
				result.Reason = "REJECT oracle-class: " + msg
				return result
			}
		}
		result.Using = using
	}

	// Primitives that return (false, "BLOCKED: ...") signal missing infrastructure,
	// not a real assertion failure. Promote StatusFail → StatusBlocked so the step
	// runner can distinguish "infra not wired" from "check ran and failed".
	if result.Status == StatusFail && strings.HasPrefix(result.Reason, "BLOCKED:") {
		result.Status = StatusBlocked
	}

	// Visual oracle overlay：若 using 含 "visual" 且 Vision 已配置
	if e.Vision != nil && containsVisual(result.Using) {
		spec := AssertionSpec{Assert: expr}
		result = e.applyVisualOracle(obs, result, spec)
	}

	return result
}

// EvaluateVisualOnly evaluates a free-form assertion directly with the visual
// oracle. It is used by CLI paths where --using=visual intentionally means
// natural-language visual judgment rather than Assertion DSL parsing.
func (e *AssertionEngine) EvaluateVisualOnly(obs *Observation, expr string, using []string) *AssertionResult {
	expr = strings.TrimSpace(expr)
	if len(using) == 0 {
		using = []string{"visual"}
	}
	result := &AssertionResult{
		Schema:     "dw.check.v1",
		Assertion:  expr,
		Using:      using,
		Status:     StatusBlocked,
		Confidence: 1.0,
		Reason:     "visual oracle pending",
	}
	if e.Vision == nil {
		result.Reason = "visual oracle: not configured"
		return result
	}
	return e.applyVisualOracle(obs, result, AssertionSpec{Assert: expr, Using: using})
}

// EvaluateAll 执行一组来自 baseline YAML 的断言规格，返回结果列表。
func (e *AssertionEngine) EvaluateAll(obs *Observation, specs []AssertionSpec) []AssertionResult {
	results := make([]AssertionResult, 0, len(specs))
	for _, spec := range specs {
		r := e.Evaluate(obs, spec.Assert)
		r.ID = spec.ID
		// 如果 spec 指定了 using，覆盖推断值
		if len(spec.Using) > 0 {
			r.Using = spec.Using
		}
		// 如果 using 包含 "visual" 且 Vision oracle 已启用，触发 VLM 视觉判定
		if e.Vision != nil && containsVisual(r.Using) {
			r = e.applyVisualOracle(obs, r, spec)
		}
		results = append(results, *r)
	}
	return results
}

// containsVisual 检查 using 列表中是否包含 "visual"。
func containsVisual(using []string) bool {
	for _, u := range using {
		if strings.EqualFold(strings.TrimSpace(u), "visual") {
			return true
		}
	}
	return false
}

// applyVisualOracle 调用 VLM 进行视觉判定，结果为 VISUAL_SUSPECT（非 FAIL）。
// Ollama 不可达时 graceful degradation：返回 BLOCKED，不 crash。
func (e *AssertionEngine) applyVisualOracle(obs *Observation, r *AssertionResult, spec AssertionSpec) *AssertionResult {
	if obs.Visual == nil || obs.Visual.ScreenshotPath == "" {
		r.Status = StatusBlocked
		r.Reason = "visual oracle: no screenshot available in Observation.Visual"
		return r
	}

	screenshot, err := os.ReadFile(obs.Visual.ScreenshotPath)
	if err != nil {
		r.Status = StatusBlocked
		r.Reason = fmt.Sprintf("visual oracle: cannot read screenshot %q: %v", obs.Visual.ScreenshotPath, err)
		return r
	}

	vctx := VisionContext{
		PageURL:  obs.Page.URL,
		Viewport: fmt.Sprintf("%dx%d", obs.Page.ViewportW, obs.Page.ViewportH),
		Expected: spec.Assert,
	}
	if obs.Structural != nil {
		vctx.A11ySummary = obs.Structural.Text
	}

	vr, err := e.Vision.Check(context.Background(), screenshot, spec.Assert, vctx)
	if err != nil {
		r.Status = StatusBlocked
		r.Reason = fmt.Sprintf("visual oracle: %v", err)
		return r
	}

	// VLM 判定完成：无论原始判定结果，视觉不通过时降为 VISUAL_SUSPECT（非 FAIL）
	if !vr.Pass {
		r.Status = StatusVisualSuspect
		r.Confidence = vr.Confidence
		r.Reason = fmt.Sprintf("visual oracle: %s (confidence=%.2f, observations=%v)",
			vr.Category, vr.Confidence, vr.Observations)
	} else {
		// VLM 确认通过：结构化原语已有明确结果时保留原状态；
		// free-form visual-only 断言从 BLOCKED 提升为 PASS。
		if r.Status == "" || r.Status == StatusBlocked {
			r.Status = StatusPass
		}
		r.Confidence = vr.Confidence
		r.Reason = fmt.Sprintf("visual oracle: %s (confidence=%.2f, observations=%v)",
			vr.Category, vr.Confidence, vr.Observations)
	}
	return r
}

// ---------------------------------------------------------------------------
// 原语实现
// ---------------------------------------------------------------------------

// evalExists 检查 obs.Structural.Refs 中是否存在匹配 role/name/testid 的元素。
// args 形如: role='button', name='Research'
func evalExists(obs *Observation, args string) (bool, string) {
	if obs.Structural == nil {
		return false, "BLOCKED: structural layer missing"
	}
	filters := parseKV(args)
	if len(filters) == 0 {
		return false, fmt.Sprintf("unsupported selector %q (use role=/name=/testid=/text=)", strings.TrimSpace(args))
	}
	for _, ref := range obs.Structural.Refs {
		if refMatches(ref, filters) {
			return true, fmt.Sprintf("found element matching %s", args)
		}
	}
	// Document-presence fallback: a testid on a non-interactable element
	// (section/card) is proven by the document inventory, not the action census.
	if testid, ok := filters["testid"]; ok && testid != "" {
		for _, present := range obs.Structural.DocumentTestIDs {
			if present == testid {
				return true, fmt.Sprintf("document testid presence contains %q", testid)
			}
		}
	}
	return false, fmt.Sprintf("no element matching %s", args)
}

// evalVisible 在 P1 阶段与 exists 等价（均基于 A11y 树，不做视觉判定）。
func evalVisible(obs *Observation, args string) (bool, string) {
	return evalExists(obs, args)
}

// evalGone 是 exists 的反面：元素不存在则通过。
func evalGone(obs *Observation, args string) (bool, string) {
	if obs.Structural == nil {
		return false, "BLOCKED: structural layer missing"
	}
	filters := parseKV(args)
	if len(filters) == 0 {
		return false, fmt.Sprintf("unsupported selector %q (use role=/name=/testid=/text=)", strings.TrimSpace(args))
	}
	for _, ref := range obs.Structural.Refs {
		if refMatches(ref, filters) {
			return false, fmt.Sprintf("element still present: %s", args)
		}
	}
	// gone() on a testid needs the document inventory: absence from the action
	// census alone never proves absence from the document. An unpopulated
	// inventory means unproven, not gone.
	if testid, ok := filters["testid"]; ok && testid != "" {
		if obs.Structural.DocumentTestIDs == nil {
			return false, fmt.Sprintf("BLOCKED: document testid inventory missing; cannot prove %q gone", testid)
		}
		for _, present := range obs.Structural.DocumentTestIDs {
			if present == testid {
				return false, fmt.Sprintf("document testid presence still contains %q", testid)
			}
		}
	}
	return true, fmt.Sprintf("element gone: %s", args)
}

// evalCount 计算匹配元素数量并与阈值比较。
// 此函数作为占位，实际由 evalCountWithComparison 调用。
func evalCount(obs *Observation, args string) (bool, string) {
	// 不应被直接调用；由 Evaluate 路由到 evalCountWithComparison
	return false, "BLOCKED: count requires comparison operator"
}

// evalCountWithComparison 是 count 原语的完整实现，包含比较运算。
func evalCountWithComparison(obs *Observation, args, comparison, funcName string) (bool, string) {
	if funcName == "tab_count" {
		// tab_count 不需要 structural
		return evalTabCountWithComparison(obs, comparison, "tab_count")
	}
	if obs.Structural == nil {
		return false, "BLOCKED: structural layer missing"
	}
	filters := parseKV(args)
	if len(filters) == 0 {
		return false, fmt.Sprintf("unsupported selector %q (use role=/name=/testid=/text=)", strings.TrimSpace(args))
	}
	count := 0
	for _, ref := range obs.Structural.Refs {
		if refMatches(ref, filters) {
			count++
		}
	}
	return compareInt(int64(count), comparison,
		fmt.Sprintf("count(%s)", args))
}

// evalTabCountWithComparison 处理 tab_count 的比较（从 behavior 层读取）。
func evalTabCountWithComparison(obs *Observation, comparison, label string) (bool, string) {
	if obs.Behavior == nil {
		return false, "BLOCKED: behavior layer missing"
	}
	return compareInt(int64(obs.Behavior.TabCount), comparison, label)
}

// evalTabCount 处理简单形式 "tab_count op value"（args = "op value"）。
func evalTabCount(obs *Observation, args string) (bool, string) {
	if obs.Behavior == nil {
		return false, "BLOCKED: behavior layer missing"
	}
	return compareInt(int64(obs.Behavior.TabCount), args, "tab_count")
}

// evalTextContains 检查 obs.Structural.Text 是否包含目标字符串。
// args 形如: 'hello'
func evalTextContains(obs *Observation, args string) (bool, string) {
	if obs.Structural == nil {
		return false, "BLOCKED: structural layer missing"
	}
	target := stripQuotes(args)
	if strings.Contains(obs.Structural.Text, target) {
		return true, fmt.Sprintf("A11y text contains %q", target)
	}
	if obs.Structural.DocumentText != "" {
		if strings.Contains(obs.Structural.DocumentText, target) {
			return true, fmt.Sprintf("rendered document text contains %q", target)
		}
		return false, fmt.Sprintf("complete rendered document text has no witness containing %q", target)
	}
	return false, fmt.Sprintf("BLOCKED: incomplete structural text has no witness containing %q", target)
}

// evalURLMatches 检查当前页面 URL 是否包含 pattern。
// args 形如: '/portal/topic'
func evalURLMatches(obs *Observation, args string) (bool, string) {
	pattern := stripQuotes(args)
	if strings.Contains(obs.Page.URL, pattern) {
		return true, fmt.Sprintf("url %q contains pattern %q", obs.Page.URL, pattern)
	}
	return false, fmt.Sprintf("url %q does not contain pattern %q", obs.Page.URL, pattern)
}

// evalActiveTabURLContains 检查当前活跃 tab 的 URL 是否包含目标字符串。
// args 形如: 'example.com'
func evalActiveTabURLContains(obs *Observation, args string) (bool, string) {
	if obs.Behavior == nil {
		return false, "BLOCKED: behavior layer missing"
	}
	target := stripQuotes(args)
	// 优先从 Behavior.URL（活跃 tab）判断
	if strings.Contains(obs.Behavior.URL, target) {
		return true, fmt.Sprintf("active tab url %q contains %q", obs.Behavior.URL, target)
	}
	// 再遍历 tabs 找 active 的
	for _, tab := range obs.Behavior.Tabs {
		if tab.Active && strings.Contains(tab.URL, target) {
			return true, fmt.Sprintf("active tab url %q contains %q", tab.URL, target)
		}
	}
	return false, fmt.Sprintf("active tab url %q does not contain %q", obs.Behavior.URL, target)
}

// evalConsoleErrorsCount 检查控制台 ERROR 级别消息数量。
// 仅计 level=="error" — warnings 是操作性日志，不代表用户可感知错误。
// args 形如 "== 0"（来自简单表达式路由）。
func evalConsoleErrorsCount(obs *Observation, args string) (bool, string) {
	if obs.Telemetry == nil {
		return false, "BLOCKED: telemetry layer missing"
	}
	var count int64
	for _, e := range obs.Telemetry.ConsoleErrors {
		if e.Level == "error" {
			count++
		}
	}
	return compareInt(count, args, "console_errors_count")
}

// evalNetworkFailuresCount 检查网络失败数量并与阈值比较。
func evalNetworkFailuresCount(obs *Observation, args string) (bool, string) {
	if obs.Telemetry == nil {
		return false, "BLOCKED: telemetry layer missing"
	}
	count := int64(len(obs.Telemetry.NetworkFailures))
	return compareInt(count, args, "network_failures_count")
}

// evalLatencyLt 检查 obs.Behavior.LatencyMs < threshold。
// args 形如: '3000'
func evalLatencyLt(obs *Observation, args string) (bool, string) {
	if obs.Behavior == nil {
		return false, "BLOCKED: behavior layer missing"
	}
	threshold, err := strconv.ParseInt(strings.TrimSpace(stripQuotes(args)), 10, 64)
	if err != nil {
		return false, fmt.Sprintf("cannot parse latency threshold from %q: %v", args, err)
	}
	if obs.Behavior.LatencyMs < threshold {
		return true, fmt.Sprintf("latency %dms < %dms", obs.Behavior.LatencyMs, threshold)
	}
	return false, fmt.Sprintf("latency %dms >= %dms", obs.Behavior.LatencyMs, threshold)
}

// ---------------------------------------------------------------------------
// 内部工具函数
// ---------------------------------------------------------------------------

// parseKV 从 "role='button', name='Research'" 形式的字符串解析 key→value 映射。
func parseKV(args string) map[string]string {
	m := make(map[string]string)
	for _, match := range argPattern.FindAllStringSubmatch(args, -1) {
		m[match[1]] = match[2]
	}
	return m
}

// refMatches 检查 RefSummary 是否满足所有过滤条件。
func refMatches(ref RefSummary, filters map[string]string) bool {
	for k, v := range filters {
		switch k {
		case "role":
			if !strings.EqualFold(ref.Role, v) {
				return false
			}
		case "name":
			if !strings.EqualFold(ref.Name, v) {
				return false
			}
		case "testid":
			if ref.TestID != v {
				return false
			}
		case "text":
			// text 过滤：Name 中包含即可（用于 gone(text='Loading')）
			if !strings.Contains(ref.Name, v) {
				return false
			}
		default:
			// 未知 key 忽略（宽容解析）
		}
	}
	return true
}

// compareInt 将 lhs 与 "op rhs" 形式的比较字符串做整数比较。
// comparison 形如 ">= 1"、"== 0"、"< 5"。
// label 用于可读 reason。
func compareInt(lhs int64, comparison, label string) (bool, string) {
	comparison = strings.TrimSpace(comparison)
	if comparison == "" {
		return false, fmt.Sprintf("BLOCKED: %s requires comparison operator", label)
	}
	// 解析 op 和 rhs
	var op, rhsStr string
	parts := strings.SplitN(comparison, " ", 2)
	if len(parts) == 2 {
		op = strings.TrimSpace(parts[0])
		rhsStr = strings.TrimSpace(parts[1])
	} else {
		// 尝试无空格形式: "==0"
		for _, candidate := range []string{"==", ">=", "<=", "!=", ">", "<"} {
			if strings.HasPrefix(comparison, candidate) {
				op = candidate
				rhsStr = strings.TrimPrefix(comparison, candidate)
				break
			}
		}
	}
	rhs, err := strconv.ParseInt(rhsStr, 10, 64)
	if err != nil {
		return false, fmt.Sprintf("cannot parse comparison rhs from %q: %v", comparison, err)
	}

	var passed bool
	switch op {
	case "==":
		passed = lhs == rhs
	case "!=":
		passed = lhs != rhs
	case ">=":
		passed = lhs >= rhs
	case "<=":
		passed = lhs <= rhs
	case ">":
		passed = lhs > rhs
	case "<":
		passed = lhs < rhs
	default:
		return false, fmt.Sprintf("unknown comparison operator: %q", op)
	}

	if passed {
		return true, fmt.Sprintf("%s: %d %s %d", label, lhs, op, rhs)
	}
	return false, fmt.Sprintf("expected %s %s %d, got %d", label, op, rhs, lhs)
}

// stripQuotes 去除字符串首尾的单引号或双引号。
func stripQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') ||
			(s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// boolToStatus 将 bool 转换为 PASS/FAIL 状态。
func boolToStatus(passed bool) Status {
	if passed {
		return StatusPass
	}
	return StatusFail
}

// acceptanceUsingClasses 是"UX 验收"类的 oracle——断言用户*感知到的结果*
// （visual）或*行为结果*（behavior），而非结构性 DOM 事实。structural predicate
// （a11y exists/visible/gone/count/text_contains）不得被重标成这些类，否则一个
// DOM 检查就冒充了视觉/行为验收。
var acceptanceUsingClasses = map[string]bool{
	"visual":   true,
	"behavior": true,
}

// usingClassConflict 判断调用者传入的 using 是否把 structural/locator predicate
// 非法改标成 acceptance 类（visual/behavior）。inferUsing 是内在类的唯一 SSOT，
// 调用者 using 只能"窄化/一致"，不能"改类"。
//
// 泛化(不限 inferred 只有一个类)：只要 predicate 的内在类里*含* structural（a11y
// DOM 事实/locator 断言），且调用者标了 acceptance 类，就冲突——无论 inferUsing
// 返回几个类。冲突时返回可读原因。
func usingClassConflict(inferred, callerUsing []string) (bool, string) {
	structural := false
	for _, c := range inferred {
		if strings.EqualFold(strings.TrimSpace(c), "structural") {
			structural = true
			break
		}
	}
	if !structural {
		return false, ""
	}
	for _, u := range callerUsing {
		if acceptanceUsingClasses[strings.ToLower(strings.TrimSpace(u))] {
			return true, fmt.Sprintf(
				"structural predicate cannot serve as visual/behavioral acceptance (inferUsing=%s, caller using=%s)",
				strings.Join(inferred, ","), strings.Join(callerUsing, ","))
		}
	}
	return false, ""
}

// inferUsing 根据原语名称推断依赖的 Observation 层。
func inferUsing(funcName string) []string {
	switch funcName {
	case "exists", "visible", "gone", "count", "text_contains":
		return []string{"structural"}
	case "url_matches":
		return []string{"page"}
	case "tab_count", "active_tab_url_contains", "active_tab_matches_url", "latency_lt":
		return []string{"behavior"}
	case "console_errors_count", "network_failures_count":
		return []string{"telemetry"}
	case "region_non_empty", "region_not_overlapped":
		return []string{"layout"}
	case "transcript_tool_count", "transcript_has", "transcript_error_count",
		"transcript_done_count", "transcript_text_contains", "transcript_format_v1":
		return []string{"transcript"}
	case "file_glob_count":
		return []string{"file"}
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Region 原语实现
// ---------------------------------------------------------------------------

// evalRegionNonEmpty 检查指定 data-region 元素的尺寸是否非零。
// args 形如: 'main-workspace'
func evalRegionNonEmpty(obs *Observation, args string) (bool, string) {
	if obs.Visual == nil {
		return false, "BLOCKED: visual/layout layer missing"
	}
	id := stripQuotes(args)
	for _, r := range obs.Visual.Regions {
		if r.ID == id {
			if r.Rect.Width > 0 && r.Rect.Height > 0 {
				return true, fmt.Sprintf("region %q is non-empty (%dx%d at %d,%d)", id, r.Rect.Width, r.Rect.Height, r.Rect.X, r.Rect.Y)
			}
			return false, fmt.Sprintf("region %q has zero size (%dx%d)", id, r.Rect.Width, r.Rect.Height)
		}
	}
	return false, fmt.Sprintf("region %q not found", id)
}

// evalRegionNotOverlapped 检查两个 data-region 元素的矩形是否不重叠（通过则 pass）。
// args 形如: 'browser-sidebar', 'main-workspace'
func evalRegionNotOverlapped(obs *Observation, args string) (bool, string) {
	if obs.Visual == nil {
		return false, "BLOCKED: visual/layout layer missing"
	}

	// Parse two comma-separated quoted ids.
	parts := strings.SplitN(args, ",", 2)
	if len(parts) != 2 {
		return false, fmt.Sprintf("region_not_overlapped: expected two comma-separated ids, got %q", args)
	}
	idA := stripQuotes(parts[0])
	idB := stripQuotes(parts[1])

	var rectA, rectB *Rect
	for i := range obs.Visual.Regions {
		r := &obs.Visual.Regions[i]
		switch r.ID {
		case idA:
			rectA = &r.Rect
		case idB:
			rectB = &r.Rect
		}
	}
	if rectA == nil {
		return false, fmt.Sprintf("region %q not found", idA)
	}
	if rectB == nil {
		return false, fmt.Sprintf("region %q not found", idB)
	}

	// Zero-size regions are treated as non-overlapping.
	if rectA.Width == 0 || rectA.Height == 0 || rectB.Width == 0 || rectB.Height == 0 {
		return true, fmt.Sprintf("regions %q and %q do not overlap (one or both are zero-size)", idA, idB)
	}

	// AABB overlap test.
	overlaps := rectA.X < rectB.X+rectB.Width &&
		rectA.X+rectA.Width > rectB.X &&
		rectA.Y < rectB.Y+rectB.Height &&
		rectA.Y+rectA.Height > rectB.Y

	if overlaps {
		return false, fmt.Sprintf("regions %q (%d,%d %dx%d) and %q (%d,%d %dx%d) overlap",
			idA, rectA.X, rectA.Y, rectA.Width, rectA.Height,
			idB, rectB.X, rectB.Y, rectB.Width, rectB.Height)
	}
	return true, fmt.Sprintf("regions %q and %q do not overlap", idA, idB)
}
