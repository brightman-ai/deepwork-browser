package testing

import (
	"encoding/json"
	"fmt"
	"github.com/brightman-ai/deepwork-browser/browser"
	"gopkg.in/yaml.v3"
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
	Request *RecordedAction
}

// NewEvidenceStore creates the evidence directory tree and returns the store.
func NewEvidenceStore(dir string) (*EvidenceStore, error) {
	var err error
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	dir = filepath.Join(dir, "run-"+NewObservationID())
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

// RecordAction records typed requests, redacting sensitive text.
func (e *EvidenceStore) RecordAction(stepID string, action browser.Action) error {
	e.actions = append(e.actions, replayAction{StepID: stepID, Request: safeAction(&action)})
	data, err := json.MarshalIndent(e.actions, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(e.dir, "actions.json"), data, 0600)
}

// SaveTrace writes the full JourneyResult to trace.json.
func (e *EvidenceStore) SaveTrace(result *JourneyResult) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("trace: marshal: %w", err)
	}
	return os.WriteFile(filepath.Join(e.dir, "trace.json"), data, 0o644)
}

// GenerateReplay preserves the typed specification; replay invokes the only runner.
func (e *EvidenceStore) GenerateReplay(spec *JourneySpec) error {
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("replay specification: %w", err)
	}
	data, err := yaml.Marshal(spec)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(e.dir, "replay.journey.yaml"), data, 0600); err != nil {
		return err
	}
	script := `#!/bin/sh
set -eu
DW_REPLAY_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
exec dw-browser journey --file "$DW_REPLAY_DIR/replay.journey.yaml" --scenario app-test-baseline --evidence "$DW_REPLAY_DIR/replayed"
`
	return os.WriteFile(filepath.Join(e.dir, "replay.sh"), []byte(script), 0755)
}

// GenerateReport writes a human-readable markdown report to report.md.
func (e *EvidenceStore) GenerateReport(result *JourneyResult) error {
	var b strings.Builder

	fmt.Fprintf(&b, "# Journey Report: %s\n", result.Name)
	fmt.Fprintf(&b, "**Status**: %s\n", result.Status)
	fmt.Fprintf(&b, "**Assurance**: %s\n", result.Assurance)
	fmt.Fprintf(&b, "**Listed conditions verified**: %d\n", len(result.VerifiedChecks))
	if len(result.ExecutionOnlySteps) > 0 {
		fmt.Fprintf(&b, "**Execution-only steps**: %s\n", strings.Join(result.ExecutionOnlySteps, ", "))
	}
	fmt.Fprintf(&b, "**Duration**: %dms\n", result.DurationMs)
	fmt.Fprintf(&b, "**Date**: %s\n\n", time.Now().UTC().Format(time.RFC3339))

	b.WriteString("## Steps\n")
	for i, step := range result.Steps {
		fmt.Fprintf(&b, "\n### %d. %s [%s]\n", i+1, step.StepID, step.Status)
		if step.Action != nil {
			data, _ := json.Marshal(step.Action)
			fmt.Fprintf(&b, "- Action: %s\n", data)
		}
		fmt.Fprintf(&b, "- Fidelity: %s\n", step.Fidelity)

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

func (e *EvidenceStore) SaveReferenceDiffs(stepID string, references []ReferenceDiff) error {
	data, err := json.MarshalIndent(references, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(e.dir, "steps", stepID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "reference-diffs.json"), data, 0644)
}
