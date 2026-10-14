package testing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	browser "github.com/brightman-ai/deepwork-browser/browser"
)

// Planner 将自然语言目标分解为 dw-browser 操作序列。
// Provider 和 endpoint 由 LLMClient 统一管理（环境变量 DW_BROWSER_LLM_*）。
type Planner struct {
	client *LLMClient
}

// PlannedStep is an authoring result, never an execution fallback.
type PlannedStep struct {
	Description string          `json:"description"`
	Action      *browser.Action `json:"action,omitempty"`
	Wait        *WaitSpec       `json:"wait,omitempty"`
}
type PlanResult struct {
	Goal  string        `json:"goal,omitempty"`
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
		return nil, err
	}
	plan, err := ParsePlan([]byte(ExtractJSON(raw)))
	if err != nil {
		return nil, err
	}
	plan.Goal = goal
	return plan, nil
}

// ParsePlan rejects unknown fields and absent text independently of Go zero values.
func ParsePlan(data []byte) (*PlanResult, error) {
	var plan PlanResult
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing plan JSON")
	}
	var raw struct {
		Steps []struct {
			Action map[string]json.RawMessage `json:"action"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	for i, step := range plan.Steps {
		if step.Action != nil && (step.Action.Verb == "fill" || step.Action.Verb == "type") {
			if _, ok := raw.Steps[i].Action["text"]; !ok {
				return nil, fmt.Errorf("step %d fill/type requires explicit text field", i+1)
			}
		}
	}
	NormalizePlan(&plan)
	if err := ValidatePlan(&plan); err != nil {
		return nil, err
	}
	return &plan, nil
}
func NormalizePlan(plan *PlanResult) {
	if plan == nil {
		return
	}
	for i := range plan.Steps {
		plan.Steps[i].Description = strings.TrimSpace(plan.Steps[i].Description)
	}
}
func ValidatePlan(plan *PlanResult) error {
	if plan == nil || len(plan.Steps) == 0 {
		return fmt.Errorf("plan requires steps")
	}
	for i, step := range plan.Steps {
		if step.Action == nil && step.Wait == nil {
			return fmt.Errorf("step %d requires action or wait", i+1)
		}
		if step.Action != nil {
			if err := validatePersistentPoints(step.Action.Target.Point, step.Action.End); err != nil {
				return fmt.Errorf("step %d invalid persistent point: %w", i+1, err)
			}
			if err := step.Action.ValidateTemplate(); err != nil {
				return fmt.Errorf("step %d invalid structured action: %w", i+1, err)
			}
			locator := step.Action.Target.Locator
			if strings.HasPrefix(locator, "role='") || strings.HasPrefix(locator, "role=\"") || (step.Action.Target.Kind == "locator" && !strings.HasPrefix(locator, "css=") && !strings.HasPrefix(locator, "role=")) {
				return fmt.Errorf("step %d selector contract: use css= or canonical role=TYPE[name=\"...\"]", i+1)
			}
		}
		if step.Wait != nil {
			if err := step.Wait.Validate(); err != nil {
				return fmt.Errorf("step %d wait: %w", i+1, err)
			}
		}
	}
	return nil
}
func buildPlanPrompt(goal string, structural *StructuralState) string {
	text := ""
	if structural != nil {
		text = structural.Text
	}
	return fmt.Sprintf(`You are a browser journey author. Produce structured Action objects; this command plans only and never executes.
Every action must be directly executable by the shared typed action kernel.
Use kind, target:{kind:"locator",locator:"css=#editor"} or current observation ref, text, url, key and other typed fields.
fill/type requires explicit text, including text:"" for clearing. Preserve text and trailing newlines exactly.
Never output role='textbox'. Use canonical role=button[name="Save"] or css= selector.
Wait is {assert:"text_contains('Saved')",timeout_ms:5000} or {duration_ms:200}; never prose or old visible/text/url grammar.
Do not serialize an action into CLI syntax. Do not invoke a planner as recovery after input failure.
Page facts:
%s
User goal: %s
Respond JSON only:
{"steps":[{"description":"edit existing text","action":{"kind":"fill","target":{"kind":"locator","locator":"css=#editor"},"text":"example"},"wait":{"assert":"text_contains('example')","timeout_ms":5000}}]}`, text, goal)
}
