package testing

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// fixtureDir returns the absolute path to the testdata directory.
func fixtureDir(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "testdata")
}

func loadFixture(t *testing.T) *TranscriptState {
	t.Helper()
	path := filepath.Join(fixtureDir(t), "sample_transcript.jsonl")
	ts, err := NewTranscriptStateFromFile(path)
	if err != nil {
		t.Fatalf("loadFixture: %v", err)
	}
	return ts
}

// newObsWithTranscript creates a minimal Observation with the fixture transcript attached.
func newObsWithTranscript(t *testing.T) *Observation {
	t.Helper()
	obs := &Observation{Schema: "dw.observe.v1"}
	ts := loadFixture(t)
	obs.SetTranscript(ts)
	return obs
}

// ---------------------------------------------------------------------------
// transcript_tool_count
// ---------------------------------------------------------------------------

func TestTranscriptToolCount_SufficientNavigations(t *testing.T) {
	obs := newObsWithTranscript(t)
	engine := &AssertionEngine{}

	// fixture has 3 browser_navigate tool_start events
	result := engine.Evaluate(obs, "transcript_tool_count('browser_navigate') >= 3")
	if result.Status != StatusPass {
		t.Errorf("expected PASS for >= 3 browser_navigate, got %s: %s", result.Status, result.Reason)
	}
}

func TestTranscriptToolCount_TooMany(t *testing.T) {
	obs := newObsWithTranscript(t)
	engine := &AssertionEngine{}

	// fixture has exactly 3 — so >= 10 should FAIL
	result := engine.Evaluate(obs, "transcript_tool_count('browser_navigate') >= 10")
	if result.Status != StatusFail {
		t.Errorf("expected FAIL for >= 10 browser_navigate, got %s: %s", result.Status, result.Reason)
	}
}

func TestTranscriptToolCount_Equals(t *testing.T) {
	obs := newObsWithTranscript(t)
	engine := &AssertionEngine{}

	result := engine.Evaluate(obs, "transcript_tool_count('browser_navigate') == 3")
	if result.Status != StatusPass {
		t.Errorf("expected PASS for == 3, got %s: %s", result.Status, result.Reason)
	}
}

func TestTranscriptToolCount_MissingTranscript(t *testing.T) {
	obs := &Observation{Schema: "dw.observe.v1"} // no transcript attached
	engine := &AssertionEngine{}

	result := engine.Evaluate(obs, "transcript_tool_count('browser_navigate') >= 1")
	if result.Status != StatusBlocked {
		t.Errorf("expected BLOCKED when transcript is missing, got %s", result.Status)
	}
	if result.Reason == "" {
		t.Error("expected non-empty reason for BLOCKED assertion")
	}
}

// ---------------------------------------------------------------------------
// transcript_has('done')
// ---------------------------------------------------------------------------

func TestTranscriptHasDone_Pass(t *testing.T) {
	obs := newObsWithTranscript(t)
	engine := &AssertionEngine{}

	result := engine.Evaluate(obs, "transcript_has('done')")
	if result.Status != StatusPass {
		t.Errorf("expected PASS for transcript_has('done'), got %s: %s", result.Status, result.Reason)
	}
}

func TestTranscriptHasDone_MissingTranscript(t *testing.T) {
	obs := &Observation{}
	engine := &AssertionEngine{}

	result := engine.Evaluate(obs, "transcript_has('done')")
	if result.Status != StatusBlocked {
		t.Errorf("expected BLOCKED when transcript missing, got %s", result.Status)
	}
}

// ---------------------------------------------------------------------------
// transcript_text_contains
// ---------------------------------------------------------------------------

func TestTranscriptTextContains_Pass(t *testing.T) {
	obs := newObsWithTranscript(t)
	engine := &AssertionEngine{}

	// fixture has text event with "agentic" in it
	result := engine.Evaluate(obs, "transcript_text_contains('agentic')")
	if result.Status != StatusPass {
		t.Errorf("expected PASS for 'agentic', got %s: %s", result.Status, result.Reason)
	}
}

func TestTranscriptTextContains_CaseInsensitive(t *testing.T) {
	obs := newObsWithTranscript(t)
	engine := &AssertionEngine{}

	result := engine.Evaluate(obs, "transcript_text_contains('AGENTIC')")
	if result.Status != StatusPass {
		t.Errorf("expected PASS for case-insensitive 'AGENTIC', got %s: %s", result.Status, result.Reason)
	}
}

func TestTranscriptTextContains_Fail(t *testing.T) {
	obs := newObsWithTranscript(t)
	engine := &AssertionEngine{}

	result := engine.Evaluate(obs, "transcript_text_contains('xyzNeverExists_9999')")
	if result.Status != StatusFail {
		t.Errorf("expected FAIL for non-existent text, got %s", result.Status)
	}
}

// ---------------------------------------------------------------------------
// transcript_format_v1
// ---------------------------------------------------------------------------

func TestTranscriptFormatV1_Pass(t *testing.T) {
	obs := newObsWithTranscript(t)
	engine := &AssertionEngine{}

	result := engine.Evaluate(obs, "transcript_format_v1()")
	if result.Status != StatusPass {
		t.Errorf("expected PASS for transcript_format_v1(), got %s: %s", result.Status, result.Reason)
	}
}

// ---------------------------------------------------------------------------
// transcript session binding: 0 matches → FAIL, >1 matches → FAIL
// ---------------------------------------------------------------------------

func TestTranscriptBinding_ZeroMatches(t *testing.T) {
	// Use a start time in the far future so no file can match.
	binding := &TranscriptBinding{
		StartTS: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	err := binding.Resolve()
	if err == nil {
		t.Error("expected error for 0 matching transcript files, got nil")
	}
	if binding.Path != "" {
		t.Errorf("expected empty Path on failure, got %q", binding.Path)
	}
}

func TestTranscriptBinding_ManualPath(t *testing.T) {
	// Directly load from path (bypasses binding, used in unit tests).
	path := filepath.Join(fixtureDir(t), "sample_transcript.jsonl")
	ts, err := NewTranscriptStateFromFile(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ts.FilePath != path {
		t.Errorf("expected FilePath %q, got %q", path, ts.FilePath)
	}
	if !ts.parsed.hasDone() {
		t.Error("fixture should have a 'done' event")
	}
}

// ---------------------------------------------------------------------------
// transcript_error_count
// ---------------------------------------------------------------------------

func TestTranscriptErrorCount_OneError(t *testing.T) {
	obs := newObsWithTranscript(t)
	engine := &AssertionEngine{}

	// fixture has 1 is_error=true tool_result
	result := engine.Evaluate(obs, "transcript_error_count() == 1")
	if result.Status != StatusPass {
		t.Errorf("expected PASS for == 1 error, got %s: %s", result.Status, result.Reason)
	}
}

func TestTranscriptErrorCount_ZeroExpected_Fail(t *testing.T) {
	obs := newObsWithTranscript(t)
	engine := &AssertionEngine{}

	// fixture has 1 error → "== 0" should FAIL
	result := engine.Evaluate(obs, "transcript_error_count() == 0")
	if result.Status != StatusFail {
		t.Errorf("expected FAIL for == 0 (fixture has 1 error), got %s: %s", result.Status, result.Reason)
	}
}

// ---------------------------------------------------------------------------
// using inference
// ---------------------------------------------------------------------------

func TestInferUsing_TranscriptPrimitives(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"transcript_tool_count", "transcript"},
		{"transcript_has", "transcript"},
		{"transcript_error_count", "transcript"},
		{"transcript_text_contains", "transcript"},
		{"transcript_format_v1", "transcript"},
		{"file_glob_count", "file"},
	}
	for _, tc := range cases {
		got := inferUsing(tc.name)
		if len(got) == 0 || got[0] != tc.want {
			t.Errorf("inferUsing(%q) = %v, want [%q]", tc.name, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// parseViewportSize
// ---------------------------------------------------------------------------

func TestParseViewportSize(t *testing.T) {
	w, h, err := parseViewportSize("1280x800")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w != 1280 || h != 800 {
		t.Errorf("expected 1280x800, got %dx%d", w, h)
	}

	_, _, err2 := parseViewportSize("badformat")
	if err2 == nil {
		t.Error("expected error for bad viewport format")
	}
}
