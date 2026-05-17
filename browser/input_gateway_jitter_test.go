package browser

// input_gateway_jitter_test.go — 回归测试:
//   1. humanJitter 分布在 [3ms, 30ms] 范围内
//   2. mousePressed 前的 drift 逻辑存在 (source-level doc-lock)
//
// 背景: Cloudflare Turnstile 反爬终局修复 — CDP 零抖动输入在 <100 事件内
//       能被 behavior analysis 识别。修复引入 Gaussian 抖动 + 落点漂移。

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestHumanJitter_Distribution 验证 humanJitter 在 1000 次采样中全部落在 [3ms, 30ms] 区间。
// 同时检查均值大致在 [8ms, 16ms] 区间 (期望 ~12ms),保证 Gaussian 参数正常。
func TestHumanJitter_Distribution(t *testing.T) {
	const samples = 1000
	const lower = 3 * time.Millisecond
	const upper = 30 * time.Millisecond

	var sum time.Duration
	for i := 0; i < samples; i++ {
		d := humanJitter()
		if d < lower {
			t.Errorf("humanJitter()=%v below lower bound %v (iter %d)", d, lower, i)
		}
		if d > upper {
			t.Errorf("humanJitter()=%v above upper bound %v (iter %d)", d, upper, i)
		}
		sum += d
	}

	mean := sum / samples
	if mean < 8*time.Millisecond || mean > 16*time.Millisecond {
		t.Errorf("humanJitter mean=%v outside expected band [8ms, 16ms] — Gaussian params drifted?", mean)
	}
	t.Logf("TestHumanJitter_Distribution: samples=%d mean=%v bounds=[%v, %v]", samples, mean, lower, upper)
}

// TestHumanJitter_NotConstant 验证 humanJitter 不是常量 (Cloudflare 检测零方差 timing)。
// 取 100 个样本,要求至少有 20 个不同的纳秒值 (Gaussian 连续分布,重复极少)。
func TestHumanJitter_NotConstant(t *testing.T) {
	seen := make(map[time.Duration]struct{})
	for i := 0; i < 100; i++ {
		seen[humanJitter()] = struct{}{}
	}
	if len(seen) < 20 {
		t.Errorf("humanJitter produced only %d unique values in 100 samples — too deterministic (Cloudflare-risk)", len(seen))
	}
}

// TestInputGateway_MouseHasButtonsBitmask 验证 dispatchMouseEvent 设置了 WithButtons 位掩码。
// headed Chrome (Wayland) 严格要求 buttons 参数,否则点击静默无效。
func TestInputGateway_MouseHasButtonsBitmask(t *testing.T) {
	b, err := os.ReadFile("input_gateway.go")
	if err != nil {
		t.Fatalf("read input_gateway.go: %v", err)
	}
	src := string(b)

	markers := []string{
		"WithButtons(buttons)", // 位掩码传递
		"humanJitter()",        // jitter 调用
	}
	for _, m := range markers {
		if !strings.Contains(src, m) {
			t.Errorf("input_gateway.go missing required marker: %q", m)
		}
	}
}

func TestInputGateway_TextInsertionUsesInsertTextFastPath(t *testing.T) {
	b, err := os.ReadFile("input_gateway.go")
	if err != nil {
		t.Fatalf("read input_gateway.go: %v", err)
	}
	src := string(b)

	markers := []string{
		`event.Event == "insertText"`,
		"input.InsertText(event.Text)",
	}
	for _, m := range markers {
		if !strings.Contains(src, m) {
			t.Errorf("input_gateway.go missing insertText fast-path marker: %q", m)
		}
	}
}
