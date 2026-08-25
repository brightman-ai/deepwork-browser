package browser

// navigate act 操作与 temporal input 值归一的纯逻辑测试。
// 背景: act 一直缺 navigate(journey DSL 有)——SPA 会话中途换 URL 只能 close+open;
// date/time 类分段控件走 Input.insertText 永远落不进值, 报 mismatch 且值为空。

import (
	"errors"
	"testing"
)

func TestParseAction_Navigate(t *testing.T) {
	for _, tc := range []struct {
		action string
		want   string
	}{
		{"navigate http://localhost:8087/living-work/1", "http://localhost:8087/living-work/1"},
		{"goto https://example.com/a?b=c", "https://example.com/a?b=c"},
		{"navigate 'http://localhost:1234/x'", "http://localhost:1234/x"},
	} {
		parsed, err := ParseAction(tc.action)
		if err != nil {
			t.Fatalf("ParseAction(%q): %v", tc.action, err)
		}
		if parsed.Op != "navigate" || parsed.Value != tc.want {
			t.Fatalf("ParseAction(%q) = %+v, want navigate %q", tc.action, parsed, tc.want)
		}
	}
	if _, err := ParseAction("navigate"); !errors.Is(err, ErrActFailed) {
		t.Fatalf("bare navigate must fail with ErrActFailed, got %v", err)
	}
	if _, err := ParseAction("navigate a b"); !errors.Is(err, ErrActFailed) {
		t.Fatalf("navigate with two args must fail, got %v", err)
	}
}

func TestNormalizeTemporalValue(t *testing.T) {
	for _, tc := range []struct {
		kind, raw, want string
		wantErr         bool
	}{
		{"date", "2026-08-28", "2026-08-28", false},
		{"date", "20260828", "2026-08-28", false},
		{"date", "28/08/2026", "", true},
		{"time", "18:30", "18:30", false},
		{"time", "1830", "18:30", false},
		{"month", "2026-08", "2026-08", false},
		{"month", "202608", "2026-08", false},
		{"week", "2026-W35", "2026-W35", false},
		{"week", "2026w35", "", true},
		{"datetime-local", "2026-08-28T18:30", "2026-08-28T18:30", false},
		{"datetime-local", "2026-08-28 18:30", "", true},
	} {
		got, err := normalizeTemporalValue(tc.kind, tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeTemporalValue(%s, %q) expected error, got %q", tc.kind, tc.raw, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("normalizeTemporalValue(%s, %q) = %q, %v; want %q", tc.kind, tc.raw, got, err, tc.want)
		}
	}
}

func TestTemporalInputKind(t *testing.T) {
	if kind := temporalInputKind("INPUT", "date"); kind != "date" {
		t.Fatalf("INPUT/date should be temporal, got %q", kind)
	}
	if kind := temporalInputKind("INPUT", "text"); kind != "" {
		t.Fatalf("INPUT/text is not temporal, got %q", kind)
	}
	if kind := temporalInputKind("TEXTAREA", "date"); kind != "" {
		t.Fatalf("TEXTAREA never temporal, got %q", kind)
	}
}
