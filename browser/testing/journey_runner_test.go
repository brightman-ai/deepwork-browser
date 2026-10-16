package testing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

type runnerTestExecutor struct {
	executeCount atomic.Int32
	executeErr   error
	screenshot   func(context.Context) ([]byte, error)
}

type modeRunnerTestExecutor struct {
	*runnerTestExecutor
	modes []browser.InteractionMode
}

func (e *modeRunnerTestExecutor) ExecuteWithMode(_ context.Context, _ browser.Action, mode browser.InteractionMode) (browser.Outcome, error) {
	e.modes = append(e.modes, mode)
	if e.executeErr != nil {
		return browser.Outcome{Phase: "dispatch", DispatchState: "unknown"}, e.executeErr
	}
	return browser.Outcome{Phase: "complete", DispatchState: "confirmed"}, nil
}

func (e *runnerTestExecutor) Execute(context.Context, browser.Action) (browser.Outcome, error) {
	e.executeCount.Add(1)
	if e.executeErr != nil {
		return browser.Outcome{Phase: "dispatch", DispatchState: "unknown"}, e.executeErr
	}
	return browser.Outcome{Phase: "complete", DispatchState: "confirmed"}, nil
}

func (*runnerTestExecutor) Snapshot(context.Context) (*browser.Snapshot, error) {
	return &browser.Snapshot{
		URL:          "http://example.test",
		SnapshotType: "a11y",
		Refs: []browser.ElementRef{{
			Ref:       "e1",
			Role:      "button",
			NameShort: "Continue",
		}},
	}, nil
}

func (e *runnerTestExecutor) Screenshot(ctx context.Context) ([]byte, error) {
	if e.screenshot != nil {
		return e.screenshot(ctx)
	}
	var data bytes.Buffer
	err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	return data.Bytes(), err
}

func (*runnerTestExecutor) GetSessionState(context.Context) (*BehaviorState, error) {
	return &BehaviorState{URL: "http://example.test"}, nil
}

func (*runnerTestExecutor) GetTelemetry(context.Context) (*TelemetryState, error) {
	return &TelemetryState{}, nil
}

func (*runnerTestExecutor) CollectRegions(context.Context) ([]RegionSnap, error) { return nil, nil }

func TestRunnerFailFastStopsAfterFirstFailedStep(t *testing.T) {
	executor := &runnerTestExecutor{executeErr: errors.New("action failed")}
	runner, err := NewRunner(executor, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	runner.SetFailFast(true)

	result, err := runner.Run(context.Background(), &JourneySpec{Version: 2,
		ID:   "fail-fast",
		Name: "fail fast",
		Journey: []StepSpec{
			{ID: "first", Kind: "action", Action: testClick("role=button[name=\"Continue\"]")},
			{ID: "second", Kind: "action", Action: testClick("role=button[name=\"Continue\"]")},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := executor.executeCount.Load(); got != 1 {
		t.Fatalf("Execute count = %d, want 1", got)
	}
	if len(result.Steps) != 1 || result.Steps[0].StepID != "first" {
		t.Fatalf("steps = %+v, want only first", result.Steps)
	}
	if result.Status != StatusBlocked {
		t.Fatalf("status = %s, want BLOCKED", result.Status)
	}
}

func TestRunnerScreenshotTimeoutFailsLoudly(t *testing.T) {
	executor := &runnerTestExecutor{
		screenshot: func(context.Context) ([]byte, error) {
			select {}
		},
	}
	runner, err := NewRunner(executor, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	runner.screenshotTimeout = 20 * time.Millisecond

	started := time.Now()
	result, err := runner.Run(context.Background(), &JourneySpec{Version: 2,
		ID:      "screenshot-timeout",
		Name:    "screenshot timeout",
		Journey: []StepSpec{{ID: "first", Kind: "action", Action: testClick("role=button[name=\"Continue\"]")}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("runner returned after %s, want <= 1s", elapsed)
	}
	if result.Status != StatusBlocked || len(result.Steps) != 1 {
		t.Fatalf("result = %+v, want one failed step", result)
	}
	if got := result.Steps[0].Error; !strings.Contains(got, "journey screenshot timed out") {
		t.Fatalf("step error = %q, want screenshot timeout", got)
	}
}

func TestRunnerDefaultsToVisualAndRoutesExplicitElementMode(t *testing.T) {
	base := &runnerTestExecutor{}
	executor := &modeRunnerTestExecutor{runnerTestExecutor: base}
	runner, err := NewRunner(executor, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	result, err := runner.Run(context.Background(), &JourneySpec{Version: 2,
		ID:   "interaction-modes",
		Name: "interaction modes",
		Journey: []StepSpec{
			{ID: "visual-default", Kind: "action", Action: testClick("role=button[name=\"Continue\"]")},
			{ID: "element-escape", Kind: "action", Action: testClick("role=button[name=\"Below fold\"]"), Mode: browser.InteractionModeElement},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != StatusDelivered {
		t.Fatalf("result=%+v", result)
	}
	if got := base.executeCount.Load(); got != 1 {
		t.Fatalf("default Execute count=%d, want 1", got)
	}
	if len(executor.modes) != 1 || executor.modes[0] != browser.InteractionModeElement {
		t.Fatalf("explicit modes=%v, want [element]", executor.modes)
	}
}

func TestRunnerFailsLoudWhenElementModeIsUnsupported(t *testing.T) {
	executor := &runnerTestExecutor{}
	runner, err := NewRunner(executor, t.TempDir())
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	result, err := runner.Run(context.Background(), &JourneySpec{Version: 2,
		ID:      "unsupported-element-mode",
		Name:    "unsupported element mode",
		Journey: []StepSpec{{ID: "escape", Kind: "action", Action: testClick("css=#target"), Mode: browser.InteractionModeElement}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != StatusBlocked || len(result.Steps) != 1 {
		t.Fatalf("result=%+v", result)
	}
	if got := result.Steps[0].Error; !strings.Contains(got, "does not support step interaction modes") {
		t.Fatalf("error=%q", got)
	}
	if got := executor.executeCount.Load(); got != 0 {
		t.Fatalf("legacy Execute was used as a silent fallback %d times", got)
	}
}

func TestLoadSpecValidatesInteractionMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journey.yaml")
	valid := []byte("version: 2\nid: modes\nname: modes\nenvironment:\n  base_url: http://example.test\n  mode: headless\njourney:\n  - id: escape\n    kind: action\n    action:\n      kind: click\n      target:\n        kind: locator\n        locator: 'css=#target'\n    mode: element\nevidence: {}\n")
	if err := os.WriteFile(path, valid, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	spec, err := LoadSpec(path)
	if err != nil {
		t.Fatalf("LoadSpec valid: %v", err)
	}
	if len(spec.Journey) != 1 || spec.Journey[0].Mode != browser.InteractionModeElement {
		t.Fatalf("mode not parsed: %+v", spec.Journey)
	}

	invalid := []byte(strings.Replace(string(valid), "mode: element", "mode: legacy", 1))
	if err := os.WriteFile(path, invalid, 0o600); err != nil {
		t.Fatalf("WriteFile invalid: %v", err)
	}
	if _, err := LoadSpec(path); err == nil || !strings.Contains(err.Error(), "must be visual or element") {
		t.Fatalf("LoadSpec invalid error=%v", err)
	}
}

func testClick(locator string) *browser.Action {
	return &browser.Action{Verb: "click", Target: browser.Target{Kind: "locator", Locator: locator}}
}

type fidelityRunnerExecutor struct {
	*runnerTestExecutor
	report browser.ActionFidelityReport
}

func (e *fidelityRunnerExecutor) LastActionFidelity() browser.ActionFidelityReport { return e.report }

func TestRunnerPreservesNativeSelectionEvidenceOnFailure(t *testing.T) {
	report := browser.ActionFidelityReport{Fidelity: browser.InteractionFidelityStrictHuman, InteractionMode: browser.InteractionModeVisual, Execution: "native-keys", EvidenceScope: "keyboard-selection", Selection: &browser.SelectionEvidence{KeySteps: 2, Partial: true, Note: "control replaced"}}
	executor := &fidelityRunnerExecutor{runnerTestExecutor: &runnerTestExecutor{executeErr: errors.New("control replaced")}, report: report}
	runner, err := NewRunner(executor, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), &JourneySpec{Version: 2, ID: "native-select-partial", Name: "native select partial", Journey: []StepSpec{{ID: "choose", Kind: "action", Action: testClick("role=button[name=\"Continue\"]")}}})
	if err != nil {
		t.Fatal(err)
	}
	step := result.Steps[0]
	if step.Status != StatusBlocked || step.ActionFidelity == nil || step.ActionFidelity.Selection == nil || !step.ActionFidelity.Selection.Partial || step.ActionFidelity.Execution != "native-keys" {
		t.Fatalf("lost partial facts: %+v", step)
	}
	encoded, err := json.Marshal(step)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"fidelity":`) || !strings.Contains(string(encoded), `"requested_fidelity":"strict-human"`) {
		t.Fatalf("mixed requested/actual evidence: %s", encoded)
	}
	if err := runner.evidence.GenerateReport(result); err != nil {
		t.Fatal(err)
	}
	markdown, err := os.ReadFile(filepath.Join(runner.evidence.dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range []string{"Requested fidelity: strict-human", "Interaction mode: visual", "Execution: native-keys", "Evidence scope: keyboard-selection", `"partial":true`} {
		if !strings.Contains(string(markdown), fact) {
			t.Fatalf("report lost %q: %s", fact, markdown)
		}
	}
}
