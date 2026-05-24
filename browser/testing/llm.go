package testing

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Role 标识 LLM 客户端的用途，决定读取哪组环境变量。
type Role string

const (
	RoleLLM    Role = "LLM"    // Planner / NL 命令
	RoleVision Role = "VISION" // 视觉判定
)

// LLMClient 是统一的 LLM/VLM 调用客户端。
// 拥有全部 provider 检测、HTTP、认证、JSON 提取机制。
// vision.go 和 planner.go 都是它的薄包装。
//
// Provider 自动检测：endpoint 含 "/v1" 或 DW_BROWSER_{role}_PROVIDER=openai
// 时使用 OpenAI-compatible /chat/completions；否则使用 Ollama /api/generate。
type LLMClient struct {
	endpoint string
	model    string
	apiKey   string
	role     Role
	http     *http.Client
}

// NewLLMClient 创建客户端。配置优先级：显式参数 > 环境变量（按 role） > 默认值。
// 显式参数传空字符串表示"回退到环境变量或默认值"。
// Vision role 的 apiKey 若为空，回退到 DW_BROWSER_LLM_API_KEY（vision/llm 常共用 key）。
func NewLLMClient(role Role, endpoint, model, apiKey string) *LLMClient {
	prefix := "DW_BROWSER_" + string(role) + "_"
	if endpoint == "" {
		endpoint = os.Getenv(prefix + "URL")
	}
	if endpoint == "" {
		endpoint = "http://127.0.0.1:11434"
	}
	if model == "" {
		model = os.Getenv(prefix + "MODEL")
	}
	if model == "" {
		model = "gemma4:26b-a4b"
	}
	if apiKey == "" {
		apiKey = os.Getenv(prefix + "API_KEY")
	}
	if apiKey == "" && role == RoleVision {
		apiKey = os.Getenv("DW_BROWSER_LLM_API_KEY") // vision 回退到通用 LLM key
	}
	return &LLMClient{
		endpoint: endpoint,
		model:    model,
		apiKey:   apiKey,
		role:     role,
		http:     &http.Client{Timeout: 60 * time.Second},
	}
}

// Model 返回配置的模型名（用于诊断输出）。
func (c *LLMClient) Model() string { return c.model }

// isOpenAI 判断是否使用 OpenAI-compatible 协议。
func (c *LLMClient) isOpenAI() bool {
	return strings.Contains(c.endpoint, "/v1") ||
		os.Getenv("DW_BROWSER_"+string(c.role)+"_PROVIDER") == "openai"
}

// Complete 发送单轮 prompt，返回模型输出的原始文本。
// images 可选：nil 表示纯文本；非空时作为多模态图片输入（OpenAI 协议和 Ollama 均支持）。
// 调用方负责用 ExtractJSON + json.Unmarshal 解析领域结构。
func (c *LLMClient) Complete(ctx context.Context, prompt string, images [][]byte) (string, error) {
	if c.isOpenAI() {
		return c.completeOpenAI(ctx, prompt, images)
	}
	return c.completeOllama(ctx, prompt, images)
}

// --- internal request/response types ---

type ollamaGenerateRequest struct {
	Model  string   `json:"model"`
	Prompt string   `json:"prompt"`
	Images []string `json:"images,omitempty"`
	Stream bool     `json:"stream"`
	Format string   `json:"format"`
}

type ollamaGenerateResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

type openAIImageURL struct {
	URL string `json:"url"`
}

type openAIContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
}

type openAIMessage struct {
	Role    string              `json:"role"`
	Content []openAIContentPart `json:"content"`
}

type openAIChatRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// completeOllama uses Ollama /api/generate.
func (c *LLMClient) completeOllama(ctx context.Context, prompt string, images [][]byte) (string, error) {
	req := ollamaGenerateRequest{
		Model:  c.model,
		Prompt: prompt,
		Stream: false,
		Format: "json",
	}
	for _, img := range images {
		req.Images = append(req.Images, base64.StdEncoding.EncodeToString(img))
	}

	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("LLM: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/api/generate", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("LLM: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("LLM endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM returned status %d: %s", resp.StatusCode, readErrBody(resp))
	}

	var ollamaResp ollamaGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return "", fmt.Errorf("LLM: decode response: %w", err)
	}
	return ollamaResp.Response, nil
}

// completeOpenAI uses OpenAI-compatible /chat/completions.
// Content is always sent as an array (uniform encoding for text+image).
func (c *LLMClient) completeOpenAI(ctx context.Context, prompt string, images [][]byte) (string, error) {
	parts := []openAIContentPart{{Type: "text", Text: prompt}}
	for _, img := range images {
		parts = append(parts, openAIContentPart{
			Type: "image_url",
			ImageURL: &openAIImageURL{
				URL: "data:image/png;base64," + base64.StdEncoding.EncodeToString(img),
			},
		})
	}

	chatReq := openAIChatRequest{
		Model: c.model,
		Messages: []openAIMessage{
			{Role: "user", Content: parts},
		},
		Stream: false,
	}

	bodyBytes, err := json.Marshal(chatReq)
	if err != nil {
		return "", fmt.Errorf("LLM: marshal openai request: %w", err)
	}

	base := strings.TrimRight(c.endpoint, "/")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("LLM: create openai request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
		httpReq.Header.Set("HTTP-Referer", "https://github.com/brightman-ai/deepwork-browser")
		httpReq.Header.Set("X-Title", "dw-browser-testing")
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("LLM openai endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("LLM openai endpoint returned status %d: %s", resp.StatusCode, readErrBody(resp))
	}

	var chatResp openAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return "", fmt.Errorf("LLM: decode openai response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("LLM: openai response contained no choices")
	}
	return chatResp.Choices[0].Message.Content, nil
}

// readErrBody 读取非 2xx 响应体（截断至 512 字节），用于错误诊断。
// 云端 API（OpenRouter/OpenAI）通常在响应体中返回结构化错误原因。
func readErrBody(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil || len(body) == 0 {
		return "(no response body)"
	}
	return strings.TrimSpace(string(body))
}

// ExtractJSON 从模型输出中提取 JSON 对象字符串。
// 去除 markdown ```json / ``` 包裹，并提取第一个 { 到最后一个 }。
// 兜底处理 OpenRouter 免费模型在 JSON 前后附加额外文本的情况。
func ExtractJSON(s string) string {
	// Strip markdown code fences
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		// remove first line (```json or ```)
		idx := strings.Index(s, "\n")
		if idx >= 0 {
			s = s[idx+1:]
		}
		// remove trailing ```
		if end := strings.LastIndex(s, "```"); end >= 0 {
			s = s[:end]
		}
		s = strings.TrimSpace(s)
	}

	// Extract first { ... last }
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}
