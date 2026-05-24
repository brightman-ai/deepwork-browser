// Package safari wraps the ax-bridge.swift native binary which reads
// iOS Simulator accessibility trees via the macOS AXUIElement API.
package safari

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// AXNode 对应 ax-bridge.swift 的 AXNodeJSON。
type AXNode struct {
	Role       string   `json:"role"`
	Label      *string  `json:"label"`
	Value      *string  `json:"value"`
	Identifier *string  `json:"identifier"`
	Traits     []string `json:"traits"`
	Frame      AXFrame  `json:"frame"`
	Visible    bool     `json:"visible"`
	Enabled    bool     `json:"enabled"`
	Focused    bool     `json:"focused"`
	Children   []AXNode `json:"children"`
	Path       string   `json:"path"`
}

// AXFrame 对应 ax-bridge.swift 的 FrameJSON。
type AXFrame struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// AXQueryInfo 对应 ax-bridge.swift 的 QueryJSON（嵌套在 QueryResultJSON.query 中）。
type AXQueryInfo struct {
	Identifier *string  `json:"identifier"`
	Label      *string  `json:"label"`
	Text       *string  `json:"text"`
	Role       *string  `json:"role"`
	Traits     []string `json:"traits"`
}

// AXQueryResult 对应 ax-bridge.swift 的 QueryResultJSON。
type AXQueryResult struct {
	Matches   []AXNode    `json:"matches"`
	Total     int         `json:"total"`
	Query     AXQueryInfo `json:"query"`
	Ambiguous bool        `json:"ambiguous"`
}

// AXPressResult 对应 ax-bridge.swift 的 PressResponseJSON。
type AXPressResult struct {
	OK          bool     `json:"ok"`
	Code        string   `json:"code"`
	Path        string   `json:"path"`
	Actions     []string `json:"actions"`
	Role        *string  `json:"role"`
	Identifier  *string  `json:"identifier"`
	Label       *string  `json:"label"`
	Message     *string  `json:"message"`
	AXErrorCode *int32   `json:"axErrorCode"`
}

// AXQueryOpts 查询选项。
type AXQueryOpts struct {
	Label      string
	Identifier string
	Role       string
	Text       string
}

// AXBridge 封装 ax-bridge.swift 的调用。
type AXBridge struct {
	mu         sync.Mutex
	binaryPath string
}

// NewAXBridge 创建 AXBridge。
func NewAXBridge() *AXBridge {
	return &AXBridge{}
}

// EnsureCompiled 确保 ax-bridge.swift 已编译为 binary。
// 首次调用时编译，后续复用缓存。若已编译产物比源码新则跳过重新编译。
func (b *AXBridge) EnsureCompiled() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.binaryPath != "" {
		if _, err := os.Stat(b.binaryPath); err == nil {
			return b.binaryPath, nil
		}
	}

	srcPath := findAXBridgeSource()
	if srcPath == "" {
		return "", fmt.Errorf("ax-bridge: source file not found")
	}

	cacheDir := filepath.Join(os.TempDir(), "dw-browser-safari")
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return "", fmt.Errorf("ax-bridge: cannot create cache dir: %w", err)
	}
	binaryPath := filepath.Join(cacheDir, "ax-bridge")

	// 若已编译产物比源码新，直接复用
	if binStat, err := os.Stat(binaryPath); err == nil {
		if srcStat, err := os.Stat(srcPath); err == nil {
			if binStat.ModTime().After(srcStat.ModTime()) {
				b.binaryPath = binaryPath
				return binaryPath, nil
			}
		}
	}

	cmd := exec.Command("swiftc", "-O", "-o", binaryPath, srcPath,
		"-framework", "ApplicationServices", "-framework", "Foundation")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ax-bridge compile failed: %w\n%s", err, string(out))
	}

	b.binaryPath = binaryPath
	return binaryPath, nil
}

// findAXBridgeSource 查找 ax-bridge.swift 源码路径。
// 优先使用与本 Go 文件相邻的 native/ 子目录，回退到工作目录。
func findAXBridgeSource() string {
	// 方法1: 相对于此 Go 文件的路径（runtime.Caller 返回编译时路径）
	_, thisFile, _, ok := runtime.Caller(0)
	if ok {
		candidate := filepath.Join(filepath.Dir(thisFile), "native", "ax-bridge.swift")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	// 方法2: 从工作目录查找
	if wd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(wd, "internal", "browser", "safari", "native", "ax-bridge.swift")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// run 执行 ax-bridge binary 并返回 stdout 内容。
// 若 binary 以非零状态退出且 stdout 含 JSON 错误，将其解析后返回。
func (b *AXBridge) run(ctx context.Context, args ...string) ([]byte, error) {
	binPath, err := b.EnsureCompiled()
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, binPath, args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			// 尝试从 stdout 解析 JSON 错误体
			if len(out) > 0 {
				var errJSON struct {
					Error string `json:"error"`
					Code  string `json:"code"`
				}
				if json.Unmarshal(out, &errJSON) == nil && errJSON.Error != "" {
					return nil, fmt.Errorf("ax-bridge: %s (%s)", errJSON.Error, errJSON.Code)
				}
			}
			stderr := string(exitErr.Stderr)
			return nil, fmt.Errorf("ax-bridge exited %d: %s", exitErr.ExitCode(), stderr)
		}
		return nil, fmt.Errorf("ax-bridge: %w", err)
	}
	return out, nil
}

// Dump 获取设备的完整 AX 树。maxDepth ≤ 0 时使用默认值 10。
func (b *AXBridge) Dump(ctx context.Context, deviceUDID string, maxDepth int) (*AXNode, error) {
	if maxDepth <= 0 {
		maxDepth = 10
	}
	args := []string{"dump", "--device", deviceUDID, "--max-depth", fmt.Sprintf("%d", maxDepth)}
	out, err := b.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var node AXNode
	if err := json.Unmarshal(out, &node); err != nil {
		return nil, fmt.Errorf("ax-bridge dump: JSON parse error: %w", err)
	}
	return &node, nil
}

// Inspect 检查指定路径（如 "0/2/1"）的元素并返回其子树。
func (b *AXBridge) Inspect(ctx context.Context, deviceUDID, path string) (*AXNode, error) {
	out, err := b.run(ctx, "inspect", "--device", deviceUDID, "--path", path)
	if err != nil {
		return nil, err
	}
	var node AXNode
	if err := json.Unmarshal(out, &node); err != nil {
		return nil, fmt.Errorf("ax-bridge inspect: JSON parse error: %w", err)
	}
	return &node, nil
}

// Press 在指定路径的元素上执行 AXPress。
// 即使元素不支持 AXPress（ok=false），也会返回结构化结果而非 error，
// 便于调用方决策回退策略（如坐标点击）。
func (b *AXBridge) Press(ctx context.Context, deviceUDID, path string) (*AXPressResult, error) {
	out, err := b.run(ctx, "press", "--device", deviceUDID, "--path", path)
	if err != nil {
		// press 命令对于 PRESS_NOT_ACTIONABLE / PRESS_FAILED 仍以 exit 0 输出 JSON，
		// 只有桥接级错误（权限、模拟器未运行）才 exit 非零。
		// 若 stdout 仍有内容，尝试解析后透传给调用方。
		if len(out) > 0 {
			var result AXPressResult
			if json.Unmarshal(out, &result) == nil {
				return &result, nil
			}
		}
		return nil, err
	}
	var result AXPressResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("ax-bridge press: JSON parse error: %w", err)
	}
	return &result, nil
}

// Query 查询匹配条件的元素列表。
func (b *AXBridge) Query(ctx context.Context, deviceUDID string, opts AXQueryOpts) (*AXQueryResult, error) {
	args := []string{"query", "--device", deviceUDID}
	if opts.Label != "" {
		args = append(args, "--label", opts.Label)
	}
	if opts.Identifier != "" {
		args = append(args, "--id", opts.Identifier)
	}
	if opts.Role != "" {
		args = append(args, "--role", opts.Role)
	}
	if opts.Text != "" {
		args = append(args, "--text", opts.Text)
	}
	out, err := b.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var result AXQueryResult
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("ax-bridge query: JSON parse error: %w", err)
	}
	return &result, nil
}

// IsPermissionError 判断 error 是否为 macOS Accessibility 权限问题。
func IsPermissionError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "permission") ||
		strings.Contains(s, "AX_PERMISSION_DENIED") ||
		strings.Contains(s, "not trusted")
}
