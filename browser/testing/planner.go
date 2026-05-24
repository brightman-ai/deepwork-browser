package testing

import (
	"context"
	"encoding/json"
	"fmt"
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
// DW_BROWSER_LLM_MODEL (default: gemma4:26b-a4b)
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
	return &PlanResult{Goal: goal, Steps: parsed.Steps}, nil
}

// buildPlanPrompt 构建规划 prompt。
func buildPlanPrompt(goal string, structural *StructuralState) string {
	a11yText := ""
	if structural != nil {
		a11yText = structural.Text
	}
	return fmt.Sprintf(
		`You are a browser automation planner. Given the current page's accessibility tree and a user goal, decompose the goal into a sequence of browser actions.

Available actions: click, fill, press, scroll, select, back, forward, wait
Selector formats: @rN (ref), #testid, role='name', role:"name"

Current page accessibility tree:
%s

Goal: %s

Respond in JSON: {"steps": [{"description": "...", "action": "click #sidebar-toggle", "wait": "optional wait condition"}]}`,
		a11yText,
		goal,
	)
}
