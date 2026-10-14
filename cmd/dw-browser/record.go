package main

import (
	"encoding/json"
	"fmt"
	"github.com/brightman-ai/deepwork-browser/browser"
	"os"
	"path/filepath"
	"time"
)

const recordSessionsDir = "/tmp/dw-browser-sessions"
const cliTraceSchema = "dw.trace.v2"

type CLIRecordState struct {
	Schema    string          `json:"schema"`
	SessionID string          `json:"session_id"`
	Recording bool            `json:"recording"`
	Domain    string          `json:"domain"`
	StartURL  string          `json:"start_url"`
	StartTime time.Time       `json:"start_time"`
	Steps     []CLIRecordStep `json:"steps"`
}
type CLIRecordStep struct {
	Seq           int             `json:"seq"`
	Action        browser.Action  `json:"action"`
	Outcome       browser.Outcome `json:"outcome"`
	ObservationID string          `json:"observation_id,omitempty"`
	RuntimeRunID  string          `json:"runtime_run_id"`
	URL           string          `json:"url,omitempty"`
	TimestampMs   int64           `json:"timestamp_ms"`
	TextRedacted  bool            `json:"text_redacted,omitempty"`
}
type CLIRecordTrace struct {
	Schema     string          `json:"schema"`
	SessionID  string          `json:"session_id"`
	Domain     string          `json:"domain"`
	StartURL   string          `json:"start_url"`
	StartTime  time.Time       `json:"start_time"`
	DurationMs int64           `json:"duration_ms"`
	Steps      []CLIRecordStep `json:"steps"`
}

func recordStateFilePath(id string) string {
	return filepath.Join(recordSessionsDir, id+"-record.json")
}
func loadRecordState(id string) (*CLIRecordState, error) {
	data, e := os.ReadFile(recordStateFilePath(id))
	if e != nil {
		return nil, e
	}
	var state CLIRecordState
	if e = json.Unmarshal(data, &state); e != nil {
		return nil, e
	}
	if state.Schema != cliTraceSchema || state.SessionID != id {
		return nil, fmt.Errorf("record state schema/session mismatch; migrate old traces explicitly")
	}
	return &state, nil
}
func saveRecordState(state *CLIRecordState) error {
	if e := os.MkdirAll(recordSessionsDir, 0755); e != nil {
		return e
	}
	data, e := json.MarshalIndent(state, "", "  ")
	if e != nil {
		return e
	}
	return writeResultFile(recordStateFilePath(state.SessionID), data)
}

// appendRecordAction consumes the same typed request/outcome used by input.
// Missing recording is optional; corrupt/failed active recording is not silent.
func appendRecordAction(info *browser.SessionInfo, request browser.Action, outcome browser.Outcome) error {
	state, e := loadRecordState(info.SessionID)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	if !state.Recording {
		return nil
	}
	redacted := request.Sensitive
	if redacted {
		request.Text = ""
	}
	state.Steps = append(state.Steps, CLIRecordStep{Seq: len(state.Steps) + 1, Action: request, Outcome: outcome, ObservationID: info.ObservationID, RuntimeRunID: info.BrowserRunID, URL: info.PageURL, TimestampMs: time.Now().UnixMilli(), TextRedacted: redacted})
	return saveRecordState(state)
}
func runRecordStartResult(args []string) (CommandResult, error) {
	info, e := loadSessionArgs(args, "record start")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	state, e := loadRecordState(info.SessionID)
	if e == nil && state.Recording {
		return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("recording already active"))
	}
	if e != nil && !os.IsNotExist(e) {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	state = &CLIRecordState{Schema: cliTraceSchema, SessionID: info.SessionID, Recording: true, Domain: extractDomain(info.PageURL), StartURL: info.PageURL, StartTime: time.Now(), Steps: []CLIRecordStep{}}
	if e = saveRecordState(state); e != nil {
		return CommandResult{}, deliveryCommandError(e)
	}
	return commandSuccess(map[string]any{"session_id": info.SessionID, "recording": true, "schema": cliTraceSchema}), nil
}
func buildCLIRecordTrace(state *CLIRecordState) CLIRecordTrace {
	return CLIRecordTrace{Schema: cliTraceSchema, SessionID: state.SessionID, Domain: state.Domain, StartURL: state.StartURL, StartTime: state.StartTime, DurationMs: time.Since(state.StartTime).Milliseconds(), Steps: state.Steps}
}
func recordResult(args []string, stop bool) (CommandResult, error) {
	_, f, e := parseCommonFlagsE(args, "record")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	state, e := loadRecordState(f.sessionID)
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	trace := buildCLIRecordTrace(state)
	if stop {
		if e = os.Remove(recordStateFilePath(f.sessionID)); e != nil {
			return CommandResult{}, deliveryCommandError(e)
		}
	}
	return commandSuccess(trace), nil
}
func runRecordStopResult(args []string) (CommandResult, error)   { return recordResult(args, true) }
func runRecordExportResult(args []string) (CommandResult, error) { return recordResult(args, false) }
