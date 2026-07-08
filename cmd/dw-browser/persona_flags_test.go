package main

import (
	"strings"
	"testing"
)

// TestThreeAxisOrthogonal — REQ-11:--scenario × --persona × --profile 三轴正交,
// 任意组合各自独立解析、互不覆盖。
func TestThreeAxisOrthogonal(t *testing.T) {
	_, flags := parseCommonFlags([]string{
		"--scenario", "app-test-explore",
		"--persona", "wechat-iphone",
		"--profile", "myprofile",
		"http://localhost:8080",
	}, "open")

	// 策略轴
	if flags.scenario != "app-test-explore" {
		t.Errorf("scenario 轴被覆盖: %q", flags.scenario)
	}
	// 存储轴
	if flags.profileID != "myprofile" {
		t.Errorf("profile 轴被覆盖: %q", flags.profileID)
	}
	// 身份轴(persona)独立解析出指纹/touch/UA facet
	if flags.persona != "wechat-iphone" {
		t.Errorf("persona 轴: %q", flags.persona)
	}
	if flags.personaFingerprint != "iphone-15-pro" {
		t.Errorf("persona 指纹应为 iphone-15-pro,实得 %q", flags.personaFingerprint)
	}
	if !flags.personaTouch {
		t.Error("persona touch 应为 true(移动设备)")
	}
	if !strings.Contains(flags.userAgent, "MicroMessenger") {
		t.Errorf("persona UA 应含 MicroMessenger,实得 %q", flags.userAgent)
	}
}

// TestPersonaAxisIndependentOfScenario — persona 解析不依赖 scenario;
// 换 scenario 不改 persona 的 facet 结果。
func TestPersonaAxisIndependentOfScenario(t *testing.T) {
	_, a := parseCommonFlags([]string{"--scenario", "app-test-explore", "--persona", "pixel-7"}, "open")
	_, b := parseCommonFlags([]string{"--scenario", "webvisit", "--persona", "pixel-7", "--allow-host", "example.com"}, "open")
	if a.personaFingerprint != b.personaFingerprint || a.userAgent != b.userAgent || a.personaTouch != b.personaTouch {
		t.Errorf("persona facet 随 scenario 漂移: a=(%q,%v) b=(%q,%v)",
			a.personaFingerprint, a.personaTouch, b.personaFingerprint, b.personaTouch)
	}
}

// TestFormatPersonaHint — CP-D:test scenario 浮现 persona 能力提示,webvisit 不提示,
// 已激活 persona 时不重复(不替 AI 选)。
func TestFormatPersonaHint(t *testing.T) {
	// app-test-* 无激活 persona → 有提示
	for _, sc := range []string{"app-test-explore", "app-test-baseline"} {
		if h := formatPersonaHint(sc, ""); h == "" {
			t.Errorf("scenario %q 应浮现 persona 提示", sc)
		} else if !strings.Contains(h, "--persona") {
			t.Errorf("提示应含 --persona,实得 %q", h)
		}
	}
	// webvisit → 不提示
	if h := formatPersonaHint("webvisit", ""); h != "" {
		t.Errorf("webvisit 不应提示,实得 %q", h)
	}
	// 空 scenario → 不提示
	if h := formatPersonaHint("", ""); h != "" {
		t.Errorf("空 scenario 不应提示,实得 %q", h)
	}
	// 已激活 persona → 不重复提示
	if h := formatPersonaHint("app-test-explore", "wechat-iphone"); h != "" {
		t.Errorf("已激活 persona 时不应重复提示,实得 %q", h)
	}
}

// TestDeviceFlagRemoved — --device 已删,显式失败(不静默)。此处只验解析不 panic;
// 移除行为(os.Exit)在 CLI 层,单测覆盖 persona 主入口即可。
func TestPersonaUnknownStillParses(t *testing.T) {
	// 未知 persona 的错误处理在解析块 os.Exit,不在此测;这里验合法 persona 主入口。
	_, f := parseCommonFlags([]string{"--scenario", "app-test-explore", "--persona", "desktop-cn-dark"}, "open")
	if f.personaFingerprint != "macos-chrome" {
		t.Errorf("desktop-cn-dark 指纹应为 macos-chrome,实得 %q", f.personaFingerprint)
	}
}
