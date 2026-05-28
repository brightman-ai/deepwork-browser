package testing

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	browser "github.com/brightman-ai/deepwork-browser/browser"
)

// Planner 将自然语言目标分解为 dw-browser 操作序列。
// Provider 和 endpoint 由 LLMClient 统一管理（环境变量 DW_BROWSER_LLM_*）。
type Planner struct {
	client *LLMClient
}

// PlannedStep 是一个计划中的操作步骤
type PlannedStep struct {
	Description string `json:"description"`    // 步骤描述
	Action      string `json:"action"`         // dw-browser act 命令，如 "click #sidebar-toggle"
	Wait        string `json:"wait,omitempty"` // 等待条件，如 "gone text Loading"
}

// PlanResult 是完整的执行计划
type PlanResult struct {
	Goal  string        `json:"goal"`
	Steps []PlannedStep `json:"steps"`
}

// NewPlanner 从环境变量创建 Planner（向后兼容）。
// DW_BROWSER_LLM_URL (default: http://127.0.0.1:11434)
// DW_BROWSER_LLM_MODEL (default: gemma4:26b-a4b; OpenRouter example: google/gemma-4-26b-a4b-it)
func NewPlanner() *Planner {
	return &Planner{client: NewLLMClient(RoleLLM, "", "", "")}
}

// NewPlannerWithConfig 创建 Planner，优先使用传入参数，fallback 到环境变量，再 fallback 到默认值。
// 传入空字符串表示"使用环境变量或默认值"。
func NewPlannerWithConfig(endpoint, model string) *Planner {
	return &Planner{client: NewLLMClient(RoleLLM, endpoint, model, "")}
}

// Plan 将目标分解为操作步骤。
// 自动根据 endpoint 选择 Ollama 或 OpenAI-compatible 协议（由 LLMClient 决定）。
func (p *Planner) Plan(ctx context.Context, goal string, structural *StructuralState) (*PlanResult, error) {
	raw, err := p.client.Complete(ctx, buildPlanPrompt(goal, structural), nil)
	if err != nil {
		return nil, fmt.Errorf("planner: %w", err)
	}
	var parsed struct {
		Steps []PlannedStep `json:"steps"`
	}
	if err := json.Unmarshal([]byte(ExtractJSON(raw)), &parsed); err != nil {
		return nil, fmt.Errorf("planner: parse LLM JSON response: %w", err)
	}
	plan := &PlanResult{Goal: goal, Steps: parsed.Steps}
	NormalizePlan(plan)
	if err := ValidatePlan(plan); err != nil {
		return nil, fmt.Errorf("planner produced invalid plan: %w", err)
	}
	return plan, nil
}

// NormalizePlan removes common LLM filler values while preserving the executable
// action contract enforced by ValidatePlan.
func NormalizePlan(plan *PlanResult) {
	if plan == nil {
		return
	}
	for i := range plan.Steps {
		plan.Steps[i].Action = strings.TrimSpace(plan.Steps[i].Action)
		plan.Steps[i].Wait = normalizePlannerWait(plan.Steps[i].Wait)
	}
}

// ValidatePlan enforces the dw-browser action contract before a generated plan
// reaches mutation execution. The planner may use "wait"/"noop" as no-op action
// markers only when the real condition is expressed in the step Wait field.
func ValidatePlan(plan *PlanResult) error {
	if plan == nil {
		return fmt.Errorf("nil plan")
	}
	if len(plan.Steps) == 0 {
		return fmt.Errorf("plan has no steps")
	}
	for i, step := range plan.Steps {
		action := strings.TrimSpace(step.Action)
		if action == "" {
			return fmt.Errorf("step %d has empty action", i+1)
		}
		if wait := strings.TrimSpace(step.Wait); wait != "" && !isPlannerWaitCondition(wait) {
			return fmt.Errorf("step %d wait %q is not supported (use milliseconds, visible/gone/text/url)", i+1, wait)
		}
		switch strings.ToLower(action) {
		case "wait", "noop", "none":
			if strings.TrimSpace(step.Wait) == "" {
				return fmt.Errorf("step %d action %q requires wait condition", i+1, action)
			}
			continue
		}
		if err := validatePlannerSelectorContract(action); err != nil {
			return fmt.Errorf("step %d action %q violates selector contract: %w", i+1, action, err)
		}
		if _, err := browser.ParseAction(action); err != nil {
			return fmt.Errorf("step %d action %q is not valid dw-browser act syntax: %w", i+1, action, err)
		}
	}
	return nil
}

func validatePlannerSelectorContract(action string) error {
	fields := strings.Fields(action)
	if len(fields) < 2 {
		return nil
	}
	op := strings.ToLower(fields[0])
	switch op {
	case "click", "fill", "type", "select", "hover", "focus":
	default:
		return nil
	}
	selector := strings.TrimSpace(fields[1])
	lower := strings.ToLower(selector)
	if strings.HasPrefix(lower, "role='") || strings.HasPrefix(lower, "role=\"") {
		return fmt.Errorf("role=<name> is ambiguous; use @rN, #testid, role=TYPE[name=\"...\"] or textbox:'name'")
	}
	return nil
}

func isPlannerWaitCondition(wait string) bool {
	wait = strings.TrimSpace(wait)
	if wait == "" {
		return true
	}
	allDigits := true
	for _, r := range wait {
		if r < '0' || r > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return true
	}
	for _, prefix := range []string{"visible ", "gone ", "text ", "url "} {
		if strings.HasPrefix(wait, prefix) && strings.TrimSpace(strings.TrimPrefix(wait, prefix)) != "" {
			return true
		}
	}
	return false
}

func normalizePlannerWait(wait string) string {
	wait = strings.TrimSpace(wait)
	lower := strings.ToLower(wait)
	switch lower {
	case "", "none", "no", "no wait", "n/a", "na", "null":
		return ""
	default:
		if strings.HasPrefix(lower, "visible text ") {
			return "text " + strings.TrimSpace(wait[len("visible text "):])
		}
		return wait
	}
}

// buildPlanPrompt 构建规划 prompt。
func buildPlanPrompt(goal string, structural *StructuralState) string {
	a11yText := ""
	if structural != nil {
		a11yText = structural.Text
	}
	return fmt.Sprintf(
		`You are a browser automation planner. Given the current page's accessibility tree and a user goal, decompose the goal into executable dw-browser actions.

Action grammar (must be exact):
- click <selector>
- fill <selector> '<text>'
- type <selector> '<text>'
- press <key>
- press <selector> <key>
- scroll up|down
- select <selector> '<value>'
- back
- forward
- wait/noop/none only with a concrete "wait" field

Selector formats: @rN (ref), #testid, textbox:'name', button:'name', link:'name', role=TYPE[name="..."]
Wait field formats: numeric milliseconds like "2000", "visible #selector", "gone #selector", "text Example Domain", "url example.com"

Rules:
- Every action must be directly executable by dw-browser act.
- fill/type/select must include the value in quotes. Never output "fill #input" without text.
- Use the current accessibility tree selectors; prefer #testid, then @rN, then role-name shorthand.
- Never output role='textbox' or role='button'. That means "role named textbox", not "a textbox". Use textbox:'accessible name' or @rN.
- Put waiting in the "wait" field, not as prose inside action.

Current page accessibility tree:
%s

Goal: %s

Respond only in JSON:
{"steps": [{"description": "enter example.com in address bar", "action": "fill #browser-url-input 'https://example.com'", "wait": "optional wait condition"}]}`,
		a11yText,
		goal,
	)
}
