package testing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EvidenceStore manages on-disk evidence for a single journey execution.
type EvidenceStore struct {
	dir     string
	actions []replayAction
}

type replayAction struct {
	StepID  string
	Command string // full dw-browser command line
}

// NewEvidenceStore creates the evidence directory tree and returns the store.
func NewEvidenceStore(dir string) (*EvidenceStore, error) {
	if err := os.MkdirAll(filepath.Join(dir, "steps"), 0o755); err != nil {
		return nil, fmt.Errorf("evidence store: mkdir: %w", err)
	}
	return &EvidenceStore{dir: dir}, nil
}

// SaveStepEvidence persists before/after observations, diff and assertion
// results for a single step. Any of before, after, diff may be nil.
func (e *EvidenceStore) SaveStepEvidence(stepID string, before, after *Observation, diff *Diff, checks []AssertionResult) error {
	dir := filepath.Join(e.dir, "steps", stepID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("step %s: mkdir: %w", stepID, err)
	}

	write := func(name string, v any) error {
		data, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Errorf("step %s: marshal %s: %w", stepID, name, err)
		}
		return os.WriteFile(filepath.Join(dir, name), data, 0o644)
	}

	if before != nil {
		if err := write("before.observe.json", before); err != nil {
			return err
		}
	}
	if after != nil {
		if err := write("after.observe.json", after); err != nil {
			return err
		}
	}
	if diff != nil {
		if err := write("diff.json", diff); err != nil {
			return err
		}
	}
	if checks != nil {
		if err := write("checks.json", checks); err != nil {
			return err
		}
	}
	return nil
}

// SaveScreenshot writes raw PNG bytes to steps/{stepID}/screenshot-{phase}.png.
// phase must be "before" or "after".
func (e *EvidenceStore) SaveScreenshot(stepID, phase string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	dir := filepath.Join(e.dir, "steps", stepID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("screenshot %s/%s: mkdir: %w", stepID, phase, err)
	}
	name := fmt.Sprintf("screenshot-%s.png", phase)
	return os.WriteFile(filepath.Join(dir, name), data, 0o644)
}

// RecordAction appends a dw-browser command to the replay log.
func (e *EvidenceStore) RecordAction(stepID, command string) {
	e.actions = append(e.actions, replayAction{StepID: stepID, Command: command})
}

// SaveTrace writes the full JourneyResult to trace.json.
func (e *EvidenceStore) SaveTrace(result *JourneyResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("trace: marshal: %w", err)
	}
	return os.WriteFile(filepath.Join(e.dir, "trace.json"), data, 0o644)
}

// GenerateReplay writes a self-contained replay.sh that re-runs the journey.
func (e *EvidenceStore) GenerateReplay(sessionID string) error {
	var b strings.Builder

	journeyID := sessionID
	b.WriteString("#!/bin/bash\n")
	fmt.Fprintf(&b, "# Replay script for journey: %s\n", journeyID)
	fmt.Fprintf(&b, "# Generated: %s\n", time.Now().UTC().Format(time.RFC3339))
	b.WriteString("set -e\n\n")
	b.WriteString(`SESSION_ID="${1:-replay-$(date +%s)}"` + "\n")
	b.WriteString(`dw-browser session start --id "$SESSION_ID" --kind test --mode headless` + "\n")

	for _, a := range e.actions {
		fmt.Fprintf(&b, "\n# Step: %s\n", a.StepID)
		fmt.Fprintf(&b, "%s\n", a.Command)
	}

	b.WriteString(`\ndw-browser session close --id "$SESSION_ID"` + "\n")

	path := filepath.Join(e.dir, "replay.sh")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		return fmt.Errorf("replay: write: %w", err)
	}
	return nil
}

// GenerateReport writes a human-readable markdown report to report.md.
func (e *EvidenceStore) GenerateReport(result *JourneyResult) error {
	var b strings.Builder

	fmt.Fprintf(&b, "# Journey Report: %s\n", result.Name)
	fmt.Fprintf(&b, "**Status**: %s\n", result.Status)
	fmt.Fprintf(&b, "**Duration**: %dms\n", result.DurationMs)
	fmt.Fprintf(&b, "**Date**: %s\n\n", time.Now().UTC().Format(time.RFC3339))

	b.WriteString("## Steps\n")
	for i, step := range result.Steps {
		fmt.Fprintf(&b, "\n### %d. %s [%s]\n", i+1, step.StepID, step.Status)
		fmt.Fprintf(&b, "- Action: %s\n", step.Action)

		total := len(step.Checks)
		passed := 0
		for _, c := range step.Checks {
			if c.Status == StatusPass {
				passed++
			}
		}
		fmt.Fprintf(&b, "- Checks: %d/%d passed\n", passed, total)
		fmt.Fprintf(&b, "- Latency: %dms\n", step.LatencyMs)

		for _, c := range step.Checks {
			if c.Status != StatusPass {
				fmt.Fprintf(&b, "- Failed: %q — %s\n", c.Assertion, c.Reason)
				fmt.Fprintf(&b, "  - Evidence: steps/%s/screenshot-after.png\n", step.StepID)
			}
		}
		if step.Error != "" {
			fmt.Fprintf(&b, "- Error: %s\n", step.Error)
		}
	}

	return os.WriteFile(filepath.Join(e.dir, "report.md"), []byte(b.String()), 0o644)
}
