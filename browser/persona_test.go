package browser

import "strings"

import "testing"

// TestPersona_FingerprintSSOT — 每个 persona 的 Fingerprint facet 必须指向
// BuiltinPresets 里真实存在的指纹(身份指纹单一 SSOT,无悬挂引用)。
// = INV-1/INV-3 的机械守门:persona 不含硬件数据、只引指纹 ID。
func TestPersona_FingerprintSSOT(t *testing.T) {
	for id, p := range Personas {
		if p.Fingerprint == "" {
			t.Errorf("persona %q 缺 Fingerprint", id)
			continue
		}
		if _, ok := BuiltinPresets[NormalizePresetID(p.Fingerprint)]; !ok {
			t.Errorf("persona %q 引用了不存在的指纹 %q(违反单一指纹 SSOT)", id, p.Fingerprint)
		}
	}
}

// TestPersona_OldFingerprintIDsStillValid — 旧 6 个 stealth 指纹 ID 仍可作
// persona 解析,且身份指纹有效(REQ-09 迁移零回退)。
func TestPersona_OldFingerprintIDsStillValid(t *testing.T) {
	old := []string{
		PresetWindowsChrome, PresetLinuxChrome, PresetMacOSChrome,
		PresetAndroidChrome, PresetMacOSSafariUA, PresetIPhoneSafariUA,
	}
	for _, id := range old {
		p, err := ResolvePersona(id)
		if err != nil {
			t.Errorf("旧指纹 ID %q 无法解析为 persona: %v", id, err)
			continue
		}
		if !p.Stealth {
			t.Errorf("旧指纹 persona %q 应为 stealth posture", id)
		}
		if _, err := ValidatePresetID(p.FingerprintID()); err != nil {
			t.Errorf("persona %q 的指纹 ID 未通过 ValidatePresetID: %v", id, err)
		}
	}
}

// TestPersona_ShellUASuffix — 壳 facet 追加 in-app token;企业微信 UA 必须
// 同含 wxwork 与 micromessenger,且 wxwork 在前(匹配 detectInAppBrowser 顺序,
// 保证被测 app 识别为"企业微信"而非"微信")。预置 REQ-01/REQ-02。
func TestPersona_ShellUASuffix(t *testing.T) {
	wx, err := ResolvePersona("wechat-iphone")
	if err != nil {
		t.Fatalf("wechat-iphone 解析失败: %v", err)
	}
	wxUA := wx.EffectiveUserAgent()
	if !strings.Contains(wxUA, "iPhone") || !strings.Contains(wxUA, "MicroMessenger") {
		t.Errorf("wechat-iphone UA 应含 iPhone + MicroMessenger,实得: %q", wxUA)
	}

	wc, err := ResolvePersona("wecom-android")
	if err != nil {
		t.Fatalf("wecom-android 解析失败: %v", err)
	}
	wcUA := strings.ToLower(wc.EffectiveUserAgent())
	iWxwork := strings.Index(wcUA, "wxwork")
	iMM := strings.Index(wcUA, "micromessenger")
	if iWxwork < 0 || iMM < 0 {
		t.Errorf("wecom-android UA 应同含 wxwork 与 micromessenger,实得: %q", wcUA)
	} else if iWxwork > iMM {
		t.Errorf("wecom-android UA 中 wxwork 应排在 micromessenger 前(否则被识别为微信),实得: %q", wcUA)
	}
}

// TestPersona_UnknownFailsClosed — 未知 persona 显式失败,不静默回退。
func TestPersona_UnknownFailsClosed(t *testing.T) {
	if _, err := ResolvePersona("no-such-persona"); err == nil {
		t.Error("未知 persona 应报错(fail-closed),却返回 nil error")
	}
}
