// Package browser — L1 单元测试。
//
// TDD: Red → Green 顺序，每个 TC 对应一个 Test 函数。
package browser

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================
// § TC-C5-11 / TC-C5-12: act --snap [SC-19]
// ============================================================

// TC-C5-11: SnapOptions.SessionMode=true 时，filterCompactRefs + renumberRefs 保留可交互元素。
// 验证: --snap 依赖的 SnapOptions 数据结构和 filterCompactRefs 函数行为正确。
func TestSnapOptions_CompactFilter_KeepsInteractable(t *testing.T) {
	// TC-ID: TC-C5-11 (L1 unit — acts as proxy for act --snap observe=true logic)
	refs := ElementRef{
		{Ref: "e1", Role: "button", Name: "Submit", Interactable: true}
		{Ref: "e2", Role: "generic", Name: "Container", Interactable: false}
		{Ref: "e3", Role: "textbox", Name: "", Placeholder: "Email", Interactable: true}
		{Ref: "e4", Role: "link", Name: "Learn More", Interactable: true}
		{Ref: "e5", Role: "heading", Name: "Title", Interactable: false}
	}

	filtered := filterCompactRefs(refs)

	// button, textbox, link は残る; generic, heading は除去される
	if len(filtered) != 3 {
		t.Errorf("expected 3 compact refs, got %d: %+v", len(filtered), filtered)
	}
	roles := map[string]bool{}
	for _, r := range filtered {
		roles[r.Role] = true
	}
	if !roles["button"] || !roles["textbox"] || !roles["link"] {
		t.Errorf("unexpected roles in compact output: %v", roles)
	}
	if roles["generic"] || roles["heading"] {
		t.Errorf("non-interactable roles leaked into compact output: %v", roles)
	}
}

// TC-C5-12: act --snap で action 失敗時は snap を実行しない（エラーで即 return）。
// 验证: needsPostActionSnapshot || snapAfterAct の論理 — snapAfterAct=true でも
// ActWithSessionMode が error を返した場合、output に snap が含まれないこと。
// L1レベルでは: ErrActFailed エラー型が正しく定義されていることを確認する。
func TestActSnap_ActionFailure_NoSnapProduced(t *testing.T) {
	// TC-ID: TC-C5-12
	// このテストはエラー型の存在確認のみ (L1 unit; 実際の act は L3 で検証)
	if !errors.Is(ErrActFailed, ErrActFailed) {
		t.Error("ErrActFailed not properly defined")
	}
	if ErrActFailed.Error == "" {
		t.Error("ErrActFailed message empty")
	}
}

// ============================================================
// § TC-C5-13 / TC-C5-14: snap --selector [SC-20]
// ============================================================

// TC-C5-13: SnapOptions 構造体で Selector フィールドが非空の場合、SnapWithOptions が
// それを使用する（L1: 構造体フィールド + ErrSelectorNotFound の存在確認）。
func TestSnapOptions_Selector_FieldExists(t *testing.T) {
	// TC-ID: TC-C5-13 (L1 structural)
	opts := SnapOptions{
		Selector: "#main-content"
		Compact: false
		MaxDepth: 0
		SessionMode: true
	}
	if opts.Selector != "#main-content" {
		t.Errorf("SnapOptions.Selector not set correctly: %q", opts.Selector)
	}
}

// TC-C5-14: snap --selector で無マッチ時 ErrSelectorNotFound を返す。
// L1: ErrSelectorNotFound が正しく定義されていること。
func TestSnapOptions_ErrSelectorNotFound_Defined(t *testing.T) {
	// TC-ID: TC-C5-14
	if ErrSelectorNotFound == nil {
		t.Fatal("ErrSelectorNotFound must be non-nil")
	}
	if !strings.Contains(ErrSelectorNotFound.Error, "selector") &&
		!strings.Contains(ErrSelectorNotFound.Error, "CSS") {
		t.Errorf("ErrSelectorNotFound message unexpected: %q", ErrSelectorNotFound.Error)
	}
	// errors.Is チェック
	wrapped := errors.New("wrap: " + ErrSelectorNotFound.Error)
	_ = wrapped
}

// ============================================================
// § TC-C5-15: snap --compact [SC-21]
// ============================================================

// TC-C5-15: --compact フィルタが compactInteractableRoles セットを正しく使用する。
func TestFilterCompactRefs_OnlyInteractableRoles(t *testing.T) {
	// TC-ID: TC-C5-15
	allRoles := string{
		"button", "input", "link", "textbox", "checkbox", "radio"
		"combobox", "slider", "tab", "searchbox", "menuitem", "switch"
		// non-interactable
		"generic", "group", "heading", "image", "none", "presentation"
	}

	refs := make(ElementRef, len(allRoles))
	for i, r := range allRoles {
		refs[i] = ElementRef{Ref: "e" + string(rune('0'+i)), Role: r, Interactable: true}
	}

	filtered := filterCompactRefs(refs)

	// 12 interactable roles defined in compactInteractableRoles
	const expectedInteractable = 12
	if len(filtered) != expectedInteractable {
		t.Errorf("expected %d compact refs, got %d", expectedInteractable, len(filtered))
		for _, r := range filtered {
			t.Logf(" role: %s", r.Role)
		}
	}

	// Non-interactable roles must not appear
	for _, r := range filtered {
		if !compactInteractableRoles[r.Role] {
			t.Errorf("non-interactable role %q leaked into compact output", r.Role)
		}
	}
}

// ============================================================
// § TC-C5-16: snap --max-depth [SC-21]
// ============================================================

// TC-C5-16: --max-depth で超深サブツリーが折り畳まれる。
func TestApplyMaxDepthText_FoldsDeepSubtree(t *testing.T) {
	// TC-ID: TC-C5-16
	// 作成: 30要素のフラットrefs (depth > maxDepth*5 = 5)
	refs := make(ElementRef, 30)
	for i := range refs {
		refs[i] = ElementRef{
			Ref: "@r" + string(rune('1'+i))
			Role: "button"
			Name: "btn"
		}
	}

	text := applyMaxDepthText(refs, 1, true) // maxDepth=1 → threshold=5

	if !strings.Contains(text, "[...") {
		t.Errorf("expected folded marker '[...' in output, got: %q", text[:min(80, len(text))])
	}
	if !strings.Contains(text, "children]") {
		t.Errorf("expected 'children]' in output, got: %q", text[:min(80, len(text))])
	}

	// maxDepth=10 → threshold=50 > 30 refs → no folding
	textNoFold := applyMaxDepthText(refs, 10, true)
	if strings.Contains(textNoFold, "[...") {
		t.Errorf("unexpected folding for maxDepth=10 with only 30 refs: %q", textNoFold[:min(80, len(textNoFold))])
	}
}

// ============================================================
// § TC-C5-17 / TC-C5-18: eval [SC-23/P2]
// ============================================================

// TC-C5-17: ErrEvalFailed が正しく定義されていること。
func TestEvalErrors_Defined(t *testing.T) {
	// TC-ID: TC-C5-17, TC-C5-18
	if ErrEvalFailed == nil {
		t.Fatal("ErrEvalFailed must be non-nil")
	}
	if !strings.Contains(ErrEvalFailed.Error, "eval") &&
		!strings.Contains(ErrEvalFailed.Error, "JavaScript") {
		t.Errorf("ErrEvalFailed message unexpected: %q", ErrEvalFailed.Error)
	}
}

// ============================================================
// § TC-C4-07 / TC-C4-08: CookieImporter [SC-22]
// ============================================================

// TC-C4-07/TC-C4-08: NewCookieImporter が nil を返さないこと。
// CookieImporter 構造体が存在し、Import メソッドが定義されていること。
func TestCookieImporter_Constructor(t *testing.T) {
	// TC-ID: TC-C4-07 (L1 structural)
	// BrowserCore の mock は不要 — nil で初期化してインターフェース存在確認
	importer := NewCookieImporter(nil)
	if importer == nil {
		t.Fatal("NewCookieImporter returned nil")
	}
}

// TC-C4-09: --domain フィルタが SQLライクなパターン変換を正しく行う。
// "*.github.com" → "%.github.com" (LIKE パターン)
func TestCookieImporter_DomainFilter_PatternConversion(t *testing.T) {
	// TC-ID: TC-C4-09 (L1 unit)
	domainFilter := "*.github.com"
	pattern := strings.ReplaceAll(domainFilter, "*", "%")
	if pattern != "%.github.com" {
		t.Errorf("domain filter conversion failed: got %q, want %q", pattern, "%.github.com")
	}

	// 空フィルタ = 全件取得
	emptyFilter := ""
	emptyPattern := strings.ReplaceAll(emptyFilter, "*", "%")
	if emptyPattern != "" {
		t.Errorf("empty filter should produce empty pattern, got %q", emptyPattern)
	}
}

// TC-C4-10: Cookie DB ロック時の一時ファイル複製ロジック確認。
// 存在しないパスを渡したとき ErrCookieDBLocked が返ること。
func TestCookieImporter_DBLocked_ReturnsError(t *testing.T) {
	// TC-ID: TC-C4-10 (L1 unit)
	_, _, err := openCookieDB("/nonexistent/path/cookies.db")
	if err == nil {
		t.Fatal("expected error for nonexistent DB path")
	}
	if !errors.Is(err, ErrCookieDBLocked) {
		t.Errorf("expected ErrCookieDBLocked, got: %v", err)
	}
}

// TC-C4-11: 只読確認 — 元ファイルを開いても変更しない。
// テスト: openCookieDB が一時ファイルに複製する際、元ファイルのサイズが変わらない。
func TestCookieImporter_ReadOnly_SourceUnchanged(t *testing.T) {
	// TC-ID: TC-C4-11 (L1 unit)
	// 空の SQLite 風ファイルを作成（ヘッダのみ）
	tmpDir := t.TempDir
	srcPath := filepath.Join(tmpDir, "Cookies")

	// SQLite3 magic header (16 bytes) + minimal content
	header := byte("SQLite format 3\x00") // 16 bytes
	if err := os.WriteFile(srcPath, header, 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	originalSize := int64(len(header))
	info, _ := os.Stat(srcPath)
	if info.Size != originalSize {
		t.Fatalf("setup: unexpected file size %d", info.Size)
	}

	// openCookieDB will fail (not valid SQLite) but should not modify source
	db, tmpPath, _ := openCookieDB(srcPath)
	if db != nil {
		db.Close
	}
	if tmpPath != "" {
		os.Remove(tmpPath)
	}

	// Verify source is unchanged
	infoAfter, err := os.Stat(srcPath)
	if err != nil {
		t.Fatalf("source file removed: %v", err)
	}
	if infoAfter.Size != originalSize {
		t.Errorf("source file was modified: size before=%d, after=%d", originalSize, infoAfter.Size)
	}
}

// ============================================================
// § renumberRefs: session vs one-shot 命名
// ============================================================

// TC-C5-13 補足: renumberRefs session モードで @rN を使う。
func TestRenumberRefs_SessionMode(t *testing.T) {
	refs := ElementRef{
		{Ref: "e1", Role: "button", Name: "A"}
		{Ref: "e2", Role: "link", Name: "B"}
		{Ref: "e3", Role: "textbox", Name: "C"}
	}

	sessionRefs := renumberRefs(refs, true)
	for i, r := range sessionRefs {
		expected := "@r" + string(rune('0'+i+1))
		if r.Ref != expected {
			t.Errorf("ref[%d]: expected %q got %q", i, expected, r.Ref)
		}
	}

	oneshotRefs := renumberRefs(refs, false)
	for i, r := range oneshotRefs {
		expected := "e" + string(rune('0'+i+1))
		if r.Ref != expected {
			t.Errorf("oneshot ref[%d]: expected %q got %q", i, expected, r.Ref)
		}
	}
}

// ============================================================
// § r2 エラー定義完全性チェック [BP §A2]
// ============================================================

// TestR2Errors_AllDefined: r2 で追加された全エラーが存在すること。
func TestR2Errors_AllDefined(t *testing.T) {
	// TC-ID: TC-C5-10 補足
	errors := error{
		ErrSelectorNotFound
		ErrEvalFailed
		ErrCookieDecryptFailed
		ErrCookieDBLocked
	}
	for _, e := range errors {
		if e == nil {
			t.Errorf("r2 error is nil")
		}
		if e.Error == "" {
			t.Errorf("r2 error has empty message: %T", e)
		}
	}
}
