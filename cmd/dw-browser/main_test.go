package main

import (
	"errors"
	"github.com/brightman-ai/deepwork-browser/browser"
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
