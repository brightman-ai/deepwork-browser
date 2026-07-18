package browser

// AI-native 意图别名单测（REQ-AE-01/02/05，delta: 20260718-233404-cli-ai-ergonomics-brow）。

import "testing"

// REQ-AE-01: mobile 别名 = iPhone 15 Pro fidelity 全家桶（含 chrome 仿真数据）。
func TestPersonaAliasMobile(t *testing.T) {
	p, err := ResolvePersona(PersonaAliasMobile)
	if err != nil {
		t.Fatal(err)
	}
	if p.Stealth {
		t.Error("mobile alias must be fidelity posture (Stealth=false)")
	}
	if p.FingerprintID() != PresetIPhone15Pro {
		t.Errorf("mobile alias fingerprint = %q, want %q", p.FingerprintID(), PresetIPhone15Pro)
	}
	fp := BuiltinPresets[p.FingerprintID()]
	if fp == nil || fp.BrowserChrome == nil {
		t.Error("mobile alias must resolve to a preset with browser chrome geometry (auto sim)")
	}
	if !fp.Touch || !fp.Mobile {
		t.Error("mobile alias must be touch+mobile")
	}
}

// REQ-AE-02: desktop 别名 = 平台默认桌面指纹、fidelity 姿态。
func TestPersonaAliasDesktop(t *testing.T) {
	p, err := ResolvePersona(PersonaAliasDesktop)
	if err != nil {
		t.Fatal(err)
	}
	if p.Stealth {
		t.Error("desktop alias must be fidelity posture (Stealth=false) — 测本地 app 不需要反检测")
	}
	if p.FingerprintID() != NormalizePresetID(DefaultPresetID()) {
		t.Errorf("desktop alias fingerprint = %q, want platform default %q", p.FingerprintID(), DefaultPresetID())
	}
	fp := BuiltinPresets[p.FingerprintID()]
	if fp == nil || fp.Mobile {
		t.Error("desktop alias must resolve to a desktop preset")
	}
}

// REQ-AE-05: 别名列 PersonaOrder 首位（AI 列表首见即最优路径）。
func TestPersonaAliasOrderFirst(t *testing.T) {
	if len(PersonaOrder) < 2 || PersonaOrder[0] != PersonaAliasMobile || PersonaOrder[1] != PersonaAliasDesktop {
		t.Errorf("PersonaOrder must start with [mobile, desktop], got %v", PersonaOrder[:2])
	}
}
