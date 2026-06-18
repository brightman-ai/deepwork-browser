package main

import (
	"errors"
	"github.com/brightman-ai/deepwork-browser/browser"
	"os"
	"strings"
	"testing"
)

// ============================================================
// § isPointerAction 单元测试 [BUG-FIX: session new-tab tracking]
// ============================================================

// TestIsPointerAction 验证 isPointerAction 正确识别会触发新 tab 的 pointer/touch 操作。
//
// 根因: runActSession 在 click 触发 target=_blank 新标签后未更新 session TargetID，
// 导致后续 snap/act 仍连接到旧标签。
// 修复: click 前快照已知 targets，click 后扫描新 target → 更新 session 文件。
func TestIsPointerAction(t *testing.T) {
	tests := []struct {
		action string
		want   bool
	}{
		// pointer/touch 操作 → 需要 new-tab 检测
		{"click @r7", true},
		{"clickat #browser-liveview 92% 8%", true},
		{"click button:'确认'", true},
		{"click link:'Github'", true},
		{"click #btn", true},
		{"tap button:'接管'", true},
		{"tapat #browser-liveview 92% 8%", true},
		{"CLICK @r3", true}, // case-insensitive
		{"Click @r3", true}, // case-insensitive
		{"click", true},     // bare click

		// 非 pointer/touch 操作 → 不触发 new-tab 检测
		{"fill @r7 'hello'", false},
		{"type textbox:'name' 'value'", false},
		{"press Enter", false},
		{"scroll down", false},
		{"hover @r3", false},
		{"back", false},
		{"forward", false},
		{"focus @r5", false},
		{"", false},    // empty string
		{"   ", false}, // whitespace only
	}

	for _, tc := range tests {
		got := isPointerAction(tc.action)
		if got != tc.want {
			t.Errorf("isPointerAction(%q) = %v, want %v", tc.action, got, tc.want)
		}
	}
}

func TestSelectSessionTargetIDPrefersExactURLOverBlankTabs(t *testing.T) {
	targets := []map[string]interface{}{
		{
			"type":                 "page",
			"url":                  browser.ChromeInitialPageURL,
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/blank-1",
		},
		{
			"type":                 "page",
			"url":                  "http://127.0.0.1:8077/studio",
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/studio-1",
		},
	}

	got := selectSessionTargetID(targets, "http://127.0.0.1:8077/studio", "fallback-1")
	if got != "studio-1" {
		t.Fatalf("selectSessionTargetID() = %q, want studio-1", got)
	}
}

func TestSelectSessionTargetIDFallsBackToInitialTargetBeforeBlankTab(t *testing.T) {
	targets := []map[string]interface{}{
		{
			"type":                 "page",
			"url":                  browser.ChromeInitialPageURL,
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/blank-1",
		},
		{
			"type":                 "page",
			"url":                  browser.ChromeInitialPageURL,
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/blank-2",
		},
	}

	got := selectSessionTargetID(targets, "http://127.0.0.1:8077/studio", "initial-123")
	if got != "initial-123" {
		t.Fatalf("selectSessionTargetID() = %q, want initial-123", got)
	}
}

func TestIsTransientBrowserStepError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "context canceled", err: errors.New("context canceled"), want: true},
		{name: "target closed", err: errors.New("target closed"), want: true},
		{name: "session closed", err: errors.New("session closed"), want: true},
		{name: "active target missing", err: errors.New("not attached to an active page target"), want: true},
		{name: "selector missing", err: errors.New("selector not found"), want: false},
	}

	for _, tc := range tests {
		if got := isTransientBrowserStepError(tc.err); got != tc.want {
			t.Fatalf("%s: isTransientBrowserStepError() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNeedsRefRefresh(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "backend node missing", err: errors.New("backend node not found: context canceled"), want: true},
		{name: "ref node missing", err: errors.New("node not found for ref \"e42\""), want: true},
		{name: "stale ref", err: errors.New("ref is stale"), want: true},
		{name: "selector missing", err: errors.New("selector not found"), want: true},
		{name: "plain error", err: errors.New("permission denied"), want: false},
	}

	for _, tc := range tests {
		if got := needsRefRefresh(tc.err); got != tc.want {
			t.Fatalf("%s: needsRefRefresh() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsNLGoalDistinguishesStructuredActions(t *testing.T) {
	tests := []struct {
		action string
		want   bool
	}{
		{action: "click #submit", want: false},
		{action: "fill textbox:'Search' 'deepwork'", want: false},
		{action: "press Enter", want: false},
		{action: "back", want: false},
		{action: "Open settings and inspect provider status", want: true},
		{action: "在浏览器中打开设置并检查 Provider", want: true},
	}

	for _, tc := range tests {
		if got := isNLGoal(tc.action); got != tc.want {
			t.Fatalf("isNLGoal(%q) = %v, want %v", tc.action, got, tc.want)
		}
	}
}

func TestWaitSelectorCountJSSupportsTestIDShorthand(t *testing.T) {
	js := waitSelectorCountJS("#card-remote-assist")
	for _, want := range []string{
		"document.querySelectorAll(selector)",
		"data-testid",
		"CSS.escape(testid)",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("wait selector JS missing %q: %s", want, js)
		}
	}
}

func TestURLSurfacesMatchTopLevelAndBrowserPortalURL(t *testing.T) {
	tests := []struct {
		name     string
		surfaces string
		pattern  string
		want     bool
	}{
		{
			name:     "top level url",
			surfaces: "https://example.com/",
			pattern:  "example.com",
			want:     true,
		},
		{
			name:     "portal chip without scheme",
			surfaces: "http://127.0.0.1:8080/portal/browser\n当前页面: iana.org/help/example-domains",
			pattern:  "https://www.iana.org/help/example-domains",
			want:     true,
		},
		{
			name:     "glob marker stripped",
			surfaces: "https://news.ycombinator.com/news",
			pattern:  "**news.ycombinator.com**",
			want:     true,
		},
		{
			name:     "unrelated",
			surfaces: "https://example.com/",
			pattern:  "iana.org",
			want:     false,
		},
	}

	for _, tc := range tests {
		if got := urlSurfacesMatch(tc.surfaces, tc.pattern); got != tc.want {
			t.Fatalf("%s: urlSurfacesMatch() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestParseLLMFlagsSetsEnvironmentAndLeavesCommandArgs(t *testing.T) {
	t.Setenv("DW_BROWSER_LLM_URL", "")
	t.Setenv("DW_BROWSER_LLM_MODEL", "")
	t.Setenv("DW_BROWSER_LLM_PROVIDER", "")
	t.Setenv("DW_BROWSER_LLM_API_KEY", "")
	t.Setenv("DW_BROWSER_VISION_URL", "")
	t.Setenv("DW_BROWSER_VISION_MODEL", "")
	t.Setenv("DW_BROWSER_VISION_PROVIDER", "")
	t.Setenv("DW_BROWSER_VISION_API_KEY", "")

	_, remaining := parseLLMFlags([]string{
		"--llm-url", "http://llm.local/v1",
		"--llm-provider=openai",
		"--llm-model=planner",
		"--llm-api-key", "planner-key",
		"--vlm-model", "vision",
		"--vlm-api-key=vision-key",
		"--id", "s1",
		"goal text",
	})

	if os.Getenv("DW_BROWSER_LLM_URL") != "http://llm.local/v1" {
		t.Fatalf("DW_BROWSER_LLM_URL = %q", os.Getenv("DW_BROWSER_LLM_URL"))
	}
	if os.Getenv("DW_BROWSER_VISION_URL") != "http://llm.local/v1" {
		t.Fatalf("DW_BROWSER_VISION_URL should default to llm url, got %q", os.Getenv("DW_BROWSER_VISION_URL"))
	}
	if os.Getenv("DW_BROWSER_LLM_PROVIDER") != "openai" || os.Getenv("DW_BROWSER_VISION_PROVIDER") != "openai" {
		t.Fatalf("provider env not set: llm=%q vision=%q", os.Getenv("DW_BROWSER_LLM_PROVIDER"), os.Getenv("DW_BROWSER_VISION_PROVIDER"))
	}
	if os.Getenv("DW_BROWSER_LLM_MODEL") != "planner" || os.Getenv("DW_BROWSER_VISION_MODEL") != "vision" {
		t.Fatalf("model env not set: llm=%q vision=%q", os.Getenv("DW_BROWSER_LLM_MODEL"), os.Getenv("DW_BROWSER_VISION_MODEL"))
	}
	if os.Getenv("DW_BROWSER_LLM_API_KEY") != "planner-key" || os.Getenv("DW_BROWSER_VISION_API_KEY") != "vision-key" {
		t.Fatalf("api key env not set")
	}
	if len(remaining) != 3 || remaining[0] != "--id" || remaining[1] != "s1" || remaining[2] != "goal text" {
		t.Fatalf("remaining args = %#v", remaining)
	}
}

func TestResolveSessionPresetID(t *testing.T) {
	tests := []struct {
		name  string
		flags commonFlags
		want  string
	}{
		{
			name:  "iphone ua maps to safari ua simulation",
			flags: commonFlags{userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"},
			want:  browser.PresetIPhoneSafariUA,
		},
		{
			name:  "mac safari ua maps to safari ua simulation",
			flags: commonFlags{userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.3 Safari/605.1.15"},
			want:  browser.PresetMacOSSafariUA,
		},
		{
			name:  "android ua maps to android chrome",
			flags: commonFlags{userAgent: "Mozilla/5.0 (Linux; Android 14; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36"},
			want:  browser.PresetAndroidChrome,
		},
	}

	for _, tc := range tests {
		if got := resolveSessionPresetID(tc.flags); got != tc.want {
			t.Fatalf("%s: resolveSessionPresetID() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseCommonFlagsUsesKindDefaults(t *testing.T) {
	positional, flags := parseCommonFlags([]string{
		"--id", "svc1",
		"--kind", "service",
		"--service", "webchat/chatgpt",
		"--account", "acct-1",
		"--url", "https://example.com",
	}, "test")
	if len(positional) != 0 {
		t.Fatalf("positional=%v, want empty", positional)
	}
	if flags.sessionID != "svc1" || flags.sessionKind != browser.SessionKindService {
		t.Fatalf("unexpected identity flags: %+v", flags)
	}
	if flags.mode != browser.ModeHeaded || flags.owner != browser.SessionOwnerService || flags.isolation != browser.SessionIsolationDedicated {
		t.Fatalf("unexpected service defaults: %+v", flags)
	}
	if flags.serviceName != "webchat/chatgpt" || flags.accountID != "acct-1" {
		t.Fatalf("service/account not parsed: %+v", flags)
	}
	if flags.url != "https://example.com" {
		t.Fatalf("url=%q", flags.url)
	}
}

func TestParseCommonFlagsExplicitProfileMakesTaskDedicated(t *testing.T) {
	_, flags := parseCommonFlags([]string{
		"--id", "task1",
		"--profile", "kept-profile",
	}, "test")
	if flags.sessionKind != browser.SessionKindTask || flags.mode != browser.ModeHeadless {
		t.Fatalf("unexpected task defaults: %+v", flags)
	}
	if flags.isolation != browser.SessionIsolationDedicated {
		t.Fatalf("explicit profile must not be treated as ephemeral, got %+v", flags)
	}
	if got := defaultProfileID(flags); got != "task-task1" {
		t.Fatalf("defaultProfileID=%q", got)
	}
}
