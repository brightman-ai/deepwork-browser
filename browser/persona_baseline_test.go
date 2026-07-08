package browser

import (
	"strings"
	"testing"
)

// TestNamedPersonas_Baseline — CP5:3 个命名保真人格各一条基线断言
// (facet 组合 == 声明)。REQ-10 的确定性基线(selfcheck 夹具的系统 oracle 在
// persona_emulation_integration_test.go)。
func TestNamedPersonas_Baseline(t *testing.T) {
	cases := []struct {
		id          string
		fingerprint string
		shell       ShellKind
		mustUA      []string // effective UA 必含
		colorScheme string
		locale      string
		stealth     bool
		touch       bool
	}{
		{
			id: "wechat-iphone", fingerprint: PresetIPhone15Pro, shell: ShellWeChat,
			mustUA: []string{"iPhone", "MicroMessenger"}, touch: true,
		},
		{
			id: "wecom-android", fingerprint: PresetPixel7, shell: ShellWeCom,
			mustUA: []string{"Android", "wxwork", "MicroMessenger"}, touch: true,
		},
		{
			id: "desktop-cn-dark", fingerprint: PresetMacOSChrome, shell: ShellNone,
			mustUA: []string{"Macintosh"}, colorScheme: "dark", locale: "zh-CN", touch: false,
		},
	}
	for _, c := range cases {
		p, err := ResolvePersona(c.id)
		if err != nil {
			t.Errorf("%s 解析失败: %v", c.id, err)
			continue
		}
		if p.Fingerprint != c.fingerprint {
			t.Errorf("%s 指纹 = %q, 期望 %q", c.id, p.Fingerprint, c.fingerprint)
		}
		if p.Shell.Kind != c.shell {
			t.Errorf("%s 壳 = %q, 期望 %q", c.id, p.Shell.Kind, c.shell)
		}
		if p.Stealth != c.stealth {
			t.Errorf("%s stealth = %v, 期望 %v(fidelity 人格不隐身)", c.id, p.Stealth, c.stealth)
		}
		if p.Touch() != c.touch {
			t.Errorf("%s touch = %v, 期望 %v", c.id, p.Touch(), c.touch)
		}
		ua := p.EffectiveUserAgent()
		for _, sub := range c.mustUA {
			if !strings.Contains(ua, sub) {
				t.Errorf("%s UA 缺 %q: %s", c.id, sub, ua)
			}
		}
		if c.colorScheme != "" && p.Env.ColorScheme != c.colorScheme {
			t.Errorf("%s color-scheme = %q, 期望 %q", c.id, p.Env.ColorScheme, c.colorScheme)
		}
		if c.locale != "" && p.Env.Locale != c.locale {
			t.Errorf("%s locale = %q, 期望 %q", c.id, p.Env.Locale, c.locale)
		}
	}
}

// TestShellScript_Content — GenerateShellScript 对有壳人格产出 bridge + 破损逻辑;
// 无壳返回空(不注入)。确定性核查 shell-realizer 内容(补集成测试的运行时验证)。
func TestShellScript_Content(t *testing.T) {
	none, _ := ResolvePersona(PresetMacOSChrome)
	if s := GenerateShellScript(none); s != "" {
		t.Errorf("无壳人格应返回空 shell script,实得非空")
	}
	wx, _ := ResolvePersona("wechat-iphone")
	s := GenerateShellScript(wx)
	for _, must := range []string{"WeixinJSBridge", "Navigator.prototype.serviceWorker", "clipboard", "Notification"} {
		if !strings.Contains(s, must) {
			t.Errorf("wechat shell script 缺 %q", must)
		}
	}
}
