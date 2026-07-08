package browser

import (
	"testing"

	"github.com/chromedp/cdproto/input"
)

func TestMapKeyNameIsCaseInsensitiveForCommonKeys(t *testing.T) {
	// Single keys / plain text only — combos no longer route through mapKeyName
	// (they go through parseKeyCombo + dispatchModifierCombo so the CDP
	// Modifiers bitmask is set; see TestParseKeyCombo).
	tests := map[string]string{
		"enter":        "\r",
		"Return":       "\r",
		"esc":          "",
		"arrowdown":    "",
		"f5":           "",
		"plain-letter": "plain-letter",
	}

	for in, want := range tests {
		if got := mapKeyName(in); got != want {
			t.Fatalf("mapKeyName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseFillSecret 验证 fillsecret 的解析契约（password/敏感字段的显式 opt-in 安全填充）。
// 执行层走 CDP Input.insertText（可信输入事件，穿透 Vue/React 受控输入 + password），
// 默认 fill/type 仍拒绝 password [IR-03] —— 见 TestParseFillSecret 不改变那条不变量。
func TestParseFillSecret(t *testing.T) {
	pa, err := ParseAction("fillsecret #share-code-input 'my-secret-code'")
	if err != nil {
		t.Fatalf("fillsecret parse error: %v", err)
	}
	if pa.Op != "fillsecret" {
		t.Errorf("Op = %q, want fillsecret", pa.Op)
	}
	if pa.Ref != "#share-code-input" {
		t.Errorf("Ref = %q, want #share-code-input", pa.Ref)
	}
	if pa.Value != "my-secret-code" {
		t.Errorf("Value = %q, want my-secret-code", pa.Value)
	}
	// 多词带空格的值（引号内）应完整保留
	if pa2, err := ParseAction("fillsecret @r2 'a b c'"); err != nil || pa2.Value != "a b c" {
		t.Errorf("fillsecret quoted multi-word Value = %q err=%v, want 'a b c'", pa2.Value, err)
	}
	// 缺值 → 报错
	if _, err := ParseAction("fillsecret #x"); err == nil {
		t.Error("fillsecret without value should error")
	}
}

func TestParseKeyCombo(t *testing.T) {
	cases := []struct {
		in       string
		wantMods input.Modifier
		wantBase string
		wantHas  bool
	}{
		// Regression: "press Control+b" must carry the Ctrl bit (2), not insert 'b'.
		{"Control+b", input.ModifierCtrl, "b", true},
		{"Ctrl+a", input.ModifierCtrl, "a", true},
		{"ctrl+enter", input.ModifierCtrl, "Enter", true},
		{"Shift+tab", input.ModifierShift, "Tab", true},
		{"Alt+F4", input.ModifierAlt, "F4", true},
		{"Cmd+k", input.ModifierMeta, "k", true},
		{"Meta+Shift+p", input.ModifierMeta | input.ModifierShift, "p", true},
		{"Control+Shift+1", input.ModifierCtrl | input.ModifierShift, "1", true},
		// Single key: not a combo.
		{"Enter", 0, "", false},
		{"b", 0, "", false},
		// Unknown modifier token: not treated as a combo.
		{"Frobnicate+b", 0, "", false},
	}
	for _, c := range cases {
		mods, base, has := parseKeyCombo(c.in)
		if has != c.wantHas || mods != c.wantMods || base != c.wantBase {
			t.Fatalf("parseKeyCombo(%q) = (%d, %q, %v), want (%d, %q, %v)",
				c.in, mods, base, has, c.wantMods, c.wantBase, c.wantHas)
		}
	}
}

func TestCodeForKey(t *testing.T) {
	cases := map[string]string{
		"b":     "KeyB",
		"a":     "KeyA",
		"1":     "Digit1",
		"Enter": "Enter",
		"F4":    "F4",
	}
	for in, want := range cases {
		if got := codeForKey(in); got != want {
			t.Fatalf("codeForKey(%q) = %q, want %q", in, got, want)
		}
	}
}
