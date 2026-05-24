package testing

import (
	"context"
	"encoding/json"
	"fmt"
)

// VisionOracle 通过 VLM（Ollama 或 OpenAI-compatible）进行视觉判定。
// Provider 和 endpoint 由 LLMClient 统一管理（环境变量 DW_BROWSER_VISION_*）。
type VisionOracle struct {
	client *LLMClient
}

// NewVisionOracle 从环境变量创建 VisionOracle（向后兼容）。
// DW_BROWSER_VISION_URL (default: http://127.0.0.1:11434)
// DW_BROWSER_VISION_MODEL (default: gemma4:26b-a4b)
func NewVisionOracle() *VisionOracle {
	return &VisionOracle{client: NewLLMClient(RoleVision, "", "", "")}
}

// NewVisionOracleWithConfig 创建 VisionOracle，优先使用传入参数，fallback 到环境变量，再 fallback 到默认值。
// 传入空字符串表示"使用环境变量或默认值"。
func NewVisionOracleWithConfig(endpoint, model string) *VisionOracle {
	return &VisionOracle{client: NewLLMClient(RoleVision, endpoint, model, "")}
}

// VisionContext 提供额外上下文帮助 VLM 判断
type VisionContext struct {
	PageURL     string `json:"page_url,omitempty"`
	Viewport    string `json:"viewport,omitempty"`
	A11ySummary string `json:"a11y_summary,omitempty"`
	Expected    string `json:"expected,omitempty"`
}

// VisionResult — VLM 判定结果
type VisionResult struct {
	Pass         bool     `json:"pass"`
	Confidence   float64  `json:"confidence"`
	Category     string   `json:"category,omitempty"`     // "visual_mismatch", "layout_ok", etc.
	Observations []string `json:"observations,omitempty"`
}

// Check 发送截图+断言到 VLM，返回判定结果。
// 自动根据 endpoint 选择 Ollama 或 OpenAI-compatible 协议（由 LLMClient 决定）。
func (v *VisionOracle) Check(ctx context.Context, screenshot []byte, assertion string, vctx VisionContext) (*VisionResult, error) {
	prompt := buildVisionPrompt(assertion, vctx)
	raw, err := v.client.Complete(ctx, prompt, [][]byte{screenshot})
	if err != nil {
		return nil, fmt.Errorf("vision: %w", err)
	}
	var result VisionResult
	if err := json.Unmarshal([]byte(ExtractJSON(raw)), &result); err != nil {
		return nil, fmt.Errorf("vision: parse VLM JSON response: %w", err)
	}
	return &result, nil
}

// buildVisionPrompt 构建视觉判定 prompt。
func buildVisionPrompt(assertion string, vctx VisionContext) string {
	return fmt.Sprintf(
		"You are a UI testing oracle. Given this screenshot of a web page, evaluate the following assertion:\n\nAssertion: %s\n\nPage URL: %s\nExpected: %s\n\nRespond in JSON format: {\"pass\": true/false, \"confidence\": 0.0-1.0, \"category\": \"...\", \"observations\": [\"...\"]}",
		assertion,
		vctx.PageURL,
		vctx.Expected,
	)
}
