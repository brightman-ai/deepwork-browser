package pdftool

import (
	"testing"
)

func TestNormalizeWord(t *testing.T) {
	cases := map[string]string{
		"café":  "café", // NFKC keeps accented letters
		"–":     "-",
		"—":     "-",
		"−":     "-",
		"‘ok’":  "'ok'",
		"“x”":   "\"x\"",
		"ﬁ":     "fi", // fi ligature folds under NFKC
	}
	for in, want := range cases {
		if got := NormalizeWord(in); got != want {
			t.Fatalf("NormalizeWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindPhrase(t *testing.T) {
	words := []Word{
		{0, 0, 10, 10, "Instant"}, {12, 0, 30, 10, "readouts"}, {32, 0, 40, 10, "take"},
		{0, 12, 8, 22, NormalizeWord("24–52")}, {10, 12, 20, 22, "ms"}, // en-dash folded at Words() time
		{22, 12, 40, 22, "end."},
	}
	if hit := FindPhrase(words, "Instant readouts take 24–52 ms"); len(hit) != 5 {
		t.Fatalf("cross-line phrase: hit %d words, want 5", len(hit))
	}
	if hit := FindPhrase(words, "Instant readouts take 24-52 ms"); len(hit) != 5 {
		t.Fatalf("dash-folded phrase: hit %d words, want 5", len(hit))
	}
	if hit := FindPhrase(words, "readouts take"); len(hit) != 2 {
		t.Fatalf("mid phrase: hit %d words, want 2", len(hit))
	}
	if hit := FindPhrase(words, "Instant take"); hit != nil {
		t.Fatalf("non-contiguous phrase must not match, got %v", hit)
	}
	if hit := FindPhrase(words, "nonexistent"); hit != nil {
		t.Fatalf("missing phrase must return nil")
	}
	if hit := FindPhrase(words, "  "); hit != nil {
		t.Fatalf("blank phrase must return nil")
	}
}

func TestWordParsing(t *testing.T) {
	// Words() 本体走子进程；bbox 正则在此用样例 XML 验。
	doc := `<word xMin="72.000000" yMin="60.120000" xMax="90.340000" yMax="72.560000">readouts</word>
<word xMin="95.0" yMin="60.0" xMax="104.2" yMax="72.0">take</word>`
	got := wordRe.FindAllStringSubmatch(doc, -1)
	if len(got) != 2 {
		t.Fatalf("parsed %d words, want 2", len(got))
	}
	if got[0][5] != "readouts" || got[1][5] != "take" {
		t.Fatalf("word texts wrong: %v", got)
	}
}

func TestFindPhrasePunctuationTolerance(t *testing.T) {
	words := []Word{
		{0, 0, 10, 10, "ms"}, {12, 0, 20, 10, "at"}, {22, 0, 40, 10, "0.8B"}, {42, 0, 60, 10, "parameters,"},
	}
	if hit := FindPhrase(words, "ms at 0.8B parameters"); len(hit) != 4 {
		t.Fatalf("trailing-comma word must match: hit %d, want 4", len(hit))
	}
	if hit := FindPhrase(words, "ms at"); len(hit) != 2 {
		t.Fatalf("plain prefix: hit %d, want 2", len(hit))
	}
}
