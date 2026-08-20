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

// TestMapKeyName_SpaceEmitsSpaceRune 回归: "press Space" 必须发真空格 rune(" ")，
// 而非把字面词 "Space"（5 个字符）逐字派发。空格 rune 经 kb.Encode 映射到
// CDP code="Space"/VK=32，才能 toggle 已聚焦的原生 checkbox（生产实测假阳根因）。
// 修前: mapKeyName("Space")=="Space"（因 canonicalKeyName 无 space 分支）→ RED。
func TestMapKeyName_SpaceEmitsSpaceRune(t *testing.T) {
	for _, in := range []string{"Space", "space", "spacebar", "SPACE"} {
		if got := mapKeyName(in); got != " " {
			t.Fatalf("mapKeyName(%q) = %q, want %q (single space rune)", in, got, " ")
		}
	}
	// canonicalKeyName 归一到 "Space"（供 dispatchModifierCombo 等下游复用）。
	if got := canonicalKeyName("spacebar"); got != "Space" {
		t.Fatalf("canonicalKeyName(spacebar) = %q, want Space", got)
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

// 2026-08-20: 功能键/方向键/导航键曾被 mapKeyName 映射成 Selenium 私有区码点 (\ue0xx),
// 而底层 chromedp 的键表不认它们 → 页面收到的 KeyboardEvent.key 是 "Unidentified"。
// 实测证据: probe 页 window keydown 记录器下, Enter/Escape/Delete/Backspace/Tab/字母 全对,
// F1–F12 / Arrow* / Home / End / PageDown / Insert 全部 Unidentified。
// 这批必须走 CDP 显式 key/code/virtualKeyCode 派发; 已经正常的那批不许改道 (它们依赖 rune
// 通道的 char 事件产生默认编辑行为: Space toggle checkbox、Enter 在 textarea 里换行)。
func TestNeedsPreciseKeyDispatchCoversBrokenKeysOnly(t *testing.T) {
	broken := []string{
		"ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight",
		"Home", "End", "PageUp", "PageDown", "Insert",
		"F1", "F2", "F5", "F12",
	}
	for _, k := range broken {
		if !needsPreciseKeyDispatch(canonicalKeyName(k)) {
			t.Fatalf("needsPreciseKeyDispatch(%q) = false, 该键走 rune 通道会发出 Unidentified", k)
		}
	}
	// 这批当前是好的 —— 走的是真实控制字符, 改道会丢 char 事件 (默认编辑行为)。
	working := []string{"Enter", "Tab", "Space", "Escape", "Backspace", "Delete", "a", "1"}
	for _, k := range working {
		if needsPreciseKeyDispatch(canonicalKeyName(k)) {
			t.Fatalf("needsPreciseKeyDispatch(%q) = true, 不该接管正常工作的键", k)
		}
	}
}

// 反向验证这批键的 CDP 三元组确实齐备 —— 缺 vk 时 Chrome 对方向键/功能键的默认行为会不完整。
func TestPreciseDispatchKeysHaveFullCDPTriple(t *testing.T) {
	for _, k := range []string{"ArrowRight", "Home", "PageDown", "Insert", "F2"} {
		canonical := canonicalKeyName(k)
		if got := keyEventKeyName(canonical); got != canonical {
			t.Fatalf("keyEventKeyName(%q) = %q, want %q", canonical, got, canonical)
		}
		if got := codeForKey(canonical); got != canonical {
			t.Fatalf("codeForKey(%q) = %q, want %q", canonical, got, canonical)
		}
		if vk := getVirtualKeyCode(canonical); vk == 0 {
			t.Fatalf("getVirtualKeyCode(%q) = 0, CDP 会缺 windowsVirtualKeyCode", canonical)
		}
	}
}

// 校验层与执行层必须认同一份"支持哪些单键"。曾经 Insert 在 needsPreciseKeyDispatch/VK 表里
// 都有, 却被 validatePressKeySyntax 拒掉 —— 两张表各说各话, 症状是动作在派发前就失败。
func TestPressValidationAcceptsEveryPreciseDispatchKey(t *testing.T) {
	for _, k := range []string{
		"ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight",
		"Home", "End", "PageUp", "PageDown", "Insert",
		"F1", "F2", "F5", "F12",
	} {
		if !needsPreciseKeyDispatch(canonicalKeyName(k)) {
			t.Fatalf("%q 应走精确派发", k)
		}
		if err := validatePressKeySyntax(k); err != nil {
			t.Fatalf("validatePressKeySyntax(%q) rejected a key the executor supports: %v", k, err)
		}
	}
}
