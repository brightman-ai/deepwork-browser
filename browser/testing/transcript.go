// transcript.go — SUT-side transcript oracle and file-glob oracle.
// These primitives read artifacts written by the deepwork app (JSONL transcripts,
// workspace files). They are fully deterministic and never invoke LLM.
package testing

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Observation extension storage
// ---------------------------------------------------------------------------

// obsExtension holds SUT-side artifact state attached to an Observation.
// It is not serialised.
type obsExtension struct {
	transcript   *TranscriptState
	workspaceDir string
}

// SetTranscript attaches transcript state to an Observation.
func (obs *Observation) SetTranscript(ts *TranscriptState) {
	if obs.ext == nil {
		obs.ext = &obsExtension{}
	}
	obs.ext.transcript = ts
}

// SetWorkspaceDir sets the workspace directory used by file_glob assertions.
func (obs *Observation) SetWorkspaceDir(dir string) {
	if obs.ext == nil {
		obs.ext = &obsExtension{}
	}
	obs.ext.workspaceDir = dir
}

// ---------------------------------------------------------------------------
// Transcript schema types (deepwork native JSONL)
// ---------------------------------------------------------------------------

// transcriptLine is the minimal shape of one JSONL line.
type transcriptLine struct {
	Type       string          `json:"type"`
	Format     string          `json:"format"`
	Workstream *workstreamNode `json:"workstream"`
}

type workstreamNode struct {
	Kind    string    `json:"kind"`    // tool_start | tool_result | text | status | done | error
	Tool    *toolNode `json:"tool,omitempty"`
	Content string    `json:"content,omitempty"` // for kind==text
}

type toolNode struct {
	Name    string `json:"name"`
	IsError bool   `json:"is_error,omitempty"`
}

// ---------------------------------------------------------------------------
// Session-under-test binding
// ---------------------------------------------------------------------------

// TranscriptBinding resolves the JSONL file for the session-under-test.
// Exactly one dw-*.jsonl file with mtime >= StartTS must exist; otherwise all
// transcript_* asserts fail with a clear error (no silent green).
type TranscriptBinding struct {
	Path    string    // absolute path after successful resolution
	StartTS time.Time // journey start timestamp (set before the first step runs)

	resolved bool
	err      error
}

// transcriptsDir returns the directory where the app writes transcript files.
func transcriptsDir() string {
	home := os.Getenv("DEEPWORK_HOME")
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".deepwork")
	}
	return filepath.Join(home, "transcripts", "sessions")
}

// Resolve scans the transcripts directory for JSONL files with mtime >= StartTS
// and selects the one with the NEWEST mtime. This supports multi-turn sessions
// where turn-2 writes a NEW file (e.g. dw-732) after turn-1's file (dw-731).
// On each call Resolve re-scans and re-selects the newest file — it does NOT
// permanently cache the result — so the binding automatically follows the
// session's current file as new turns begin.
//
// Caching semantics:
//   - 0 match      → NOT cached; caller may retry on the next observation
//                    so that the binding resolves once the SUT has created its file.
//   - 1+ matches   → select the one with the newest mtime; update b.Path.
//                    Never fail on multiple matches (multi-turn is expected).
//   - dir read err → NOT cached; may be transient.
func (b *TranscriptBinding) Resolve() error {
	dir := transcriptsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Transient read error — do not cache, allow retry.
		return fmt.Errorf("transcript binding: cannot read transcripts dir %s: %w", dir, err)
	}

	var newestPath string
	var newestMtime time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "dw-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		info, infoErr := e.Info()
		if infoErr != nil {
			continue
		}
		mt := info.ModTime()
		if mt.Before(b.StartTS) {
			continue
		}
		if newestPath == "" || mt.After(newestMtime) {
			newestPath = filepath.Join(dir, name)
			newestMtime = mt
		}
	}

	if newestPath == "" {
		// Session file not yet written — do NOT cache so we retry on the next observation.
		return fmt.Errorf("transcript binding: 0 dw-*.jsonl files with mtime >= %v in %s; session not started yet",
			b.StartTS.Format(time.RFC3339), dir)
	}

	// Update path to the newest file (re-resolved on every call for multi-turn).
	b.resolved = true
	b.Path = newestPath
	b.err = nil
	return nil
}

// ---------------------------------------------------------------------------
// TranscriptState — parsed body of the resolved transcript
// ---------------------------------------------------------------------------

// TranscriptState carries the parsed transcript for use by transcript_* primitives.
type TranscriptState struct {
	FilePath string
	parsed   *parsedTranscript
}

// LoadTranscriptState resolves the binding and parses the transcript.
func LoadTranscriptState(binding *TranscriptBinding) (*TranscriptState, error) {
	if err := binding.Resolve(); err != nil {
		return nil, err
	}
	pt, err := parseTranscript(binding.Path)
	if err != nil {
		return nil, err
	}
	return &TranscriptState{FilePath: binding.Path, parsed: pt}, nil
}

// NewTranscriptStateFromFile parses a transcript directly from path (for tests).
func NewTranscriptStateFromFile(path string) (*TranscriptState, error) {
	pt, err := parseTranscript(path)
	if err != nil {
		return nil, err
	}
	return &TranscriptState{FilePath: path, parsed: pt}, nil
}

type parsedTranscript struct {
	lines []transcriptLine
}

func parseTranscript(path string) (*parsedTranscript, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("transcript parse: cannot open %s: %w", path, err)
	}
	defer f.Close()

	var lines []transcriptLine
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var tl transcriptLine
		if decErr := json.Unmarshal([]byte(text), &tl); decErr != nil {
			return nil, fmt.Errorf("transcript parse: %s line %d: %w", path, lineNum, decErr)
		}
		lines = append(lines, tl)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("transcript parse: scan %s: %w", path, err)
	}
	return &parsedTranscript{lines: lines}, nil
}

// ---------------------------------------------------------------------------
// parsedTranscript query methods
// ---------------------------------------------------------------------------

func (pt *parsedTranscript) countToolStarts(toolName string) int {
	n := 0
	for _, l := range pt.lines {
		ws := l.Workstream
		if ws == nil {
			continue
		}
		if ws.Kind == "tool_start" && ws.Tool != nil && ws.Tool.Name == toolName {
			n++
		}
	}
	return n
}

func (pt *parsedTranscript) hasDone() bool {
	for _, l := range pt.lines {
		if l.Workstream != nil && l.Workstream.Kind == "done" {
			return true
		}
	}
	return false
}

func (pt *parsedTranscript) countDone() int {
	n := 0
	for _, l := range pt.lines {
		if l.Workstream != nil && l.Workstream.Kind == "done" {
			n++
		}
	}
	return n
}

func (pt *parsedTranscript) countErrors() int {
	n := 0
	for _, l := range pt.lines {
		ws := l.Workstream
		if ws == nil {
			continue
		}
		if ws.Kind == "error" {
			n++
			continue
		}
		if ws.Kind == "tool_result" && ws.Tool != nil && ws.Tool.IsError {
			n++
		}
	}
	return n
}

func (pt *parsedTranscript) allText() string {
	var sb strings.Builder
	for _, l := range pt.lines {
		ws := l.Workstream
		if ws == nil {
			continue
		}
		if ws.Kind == "text" && ws.Content != "" {
			sb.WriteString(ws.Content)
			sb.WriteByte(' ')
		}
	}
	return sb.String()
}

func (pt *parsedTranscript) hasFormatV1() bool {
	for _, l := range pt.lines {
		if l.Format == "deepwork.native_transcript.v1" {
			return true
		}
	}
	// Real transcripts (confirmed by inspection) do NOT carry a top-level "format" field.
	// Instead, v1 is identified structurally: presence of workstream.kind events.
	for _, l := range pt.lines {
		if l.Workstream != nil && l.Workstream.Kind != "" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Observation accessor helpers
// ---------------------------------------------------------------------------

func transcriptFromObs(obs *Observation) *TranscriptState {
	if obs == nil || obs.ext == nil {
		return nil
	}
	return obs.ext.transcript
}

func workspaceDirFromObs(obs *Observation) string {
	if obs == nil || obs.ext == nil {
		return ""
	}
	return obs.ext.workspaceDir
}

// ---------------------------------------------------------------------------
// Primitive implementations
// ---------------------------------------------------------------------------

// evalTranscriptHasDone — transcript_has('done').
func evalTranscriptHasDone(obs *Observation, args string) (bool, string) {
	ts := transcriptFromObs(obs)
	if ts == nil {
		return false, "BLOCKED: transcript layer missing (attach TranscriptState via obs.SetTranscript)"
	}
	kind := stripQuotes(args)
	switch kind {
	case "done":
		if ts.parsed.hasDone() {
			return true, "transcript has workstream.kind==\"done\""
		}
		return false, "transcript has no workstream.kind==\"done\" event"
	default:
		return false, fmt.Sprintf("transcript_has: unsupported kind %q (supported: 'done')", kind)
	}
}

// evalTranscriptTextContains — transcript_text_contains('substring').
func evalTranscriptTextContains(obs *Observation, args string) (bool, string) {
	ts := transcriptFromObs(obs)
	if ts == nil {
		return false, "BLOCKED: transcript layer missing"
	}
	target := strings.ToLower(stripQuotes(args))
	text := strings.ToLower(ts.parsed.allText())
	if strings.Contains(text, target) {
		return true, fmt.Sprintf("transcript text contains %q", target)
	}
	return false, fmt.Sprintf("transcript text does not contain %q", target)
}

// evalTranscriptFormatV1 — transcript_format_v1() boolean.
func evalTranscriptFormatV1(obs *Observation, _ string) (bool, string) {
	ts := transcriptFromObs(obs)
	if ts == nil {
		return false, "BLOCKED: transcript layer missing"
	}
	if ts.parsed.hasFormatV1() {
		return true, "transcript follows deepwork native_transcript.v1 structure"
	}
	return false, "transcript does not appear to be deepwork native_transcript.v1 (no workstream events found)"
}

// evalTranscriptToolCountWithComparison — transcript_tool_count('name') op N.
func evalTranscriptToolCountWithComparison(obs *Observation, args, comparison string) (bool, string) {
	ts := transcriptFromObs(obs)
	if ts == nil {
		return false, "BLOCKED: transcript layer missing"
	}
	toolName := stripQuotes(args)
	count := ts.parsed.countToolStarts(toolName)
	return compareInt(int64(count), comparison, fmt.Sprintf("transcript_tool_count(%q)", toolName))
}

// evalTranscriptErrorCountWithComparison — transcript_error_count() op N.
func evalTranscriptErrorCountWithComparison(obs *Observation, _ string, comparison string) (bool, string) {
	ts := transcriptFromObs(obs)
	if ts == nil {
		return false, "BLOCKED: transcript layer missing"
	}
	count := ts.parsed.countErrors()
	return compareInt(int64(count), comparison, "transcript_error_count()")
}

// evalTranscriptDoneCountWithComparison — transcript_done_count() op N.
// Counts workstream lines where kind=="done". Useful for asserting that a
// specific number of AI turns have completed (each turn emits one done event).
func evalTranscriptDoneCountWithComparison(obs *Observation, comparison string) (bool, string) {
	ts := transcriptFromObs(obs)
	if ts == nil {
		return false, "BLOCKED: transcript layer missing"
	}
	count := ts.parsed.countDone()
	return compareInt(int64(count), comparison, "transcript_done_count()")
}

// evalFileGlobCountWithComparison — file_glob_count('pattern') op N.
func evalFileGlobCountWithComparison(obs *Observation, args, comparison string) (bool, string) {
	pattern := stripQuotes(args)
	workspaceDir := workspaceDirFromObs(obs)
	if workspaceDir != "" && !filepath.IsAbs(pattern) {
		pattern = filepath.Join(workspaceDir, pattern)
	}
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return false, fmt.Sprintf("BLOCKED: file_glob_count: invalid pattern %q: %v", pattern, err)
	}
	count := 0
	for _, m := range matches {
		info, statErr := os.Stat(m)
		if statErr == nil && info.Mode()&fs.ModeType == 0 {
			count++
		}
	}
	return compareInt(int64(count), comparison, fmt.Sprintf("file_glob_count(%q)", pattern))
}
