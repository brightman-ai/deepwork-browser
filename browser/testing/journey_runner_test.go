package testing

import (
	"context"
	"errors"
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

func (e *modeRunnerTestExecutor) ExecuteWithMode(_ context.Context, _ string, mode browser.InteractionMode) error {
	e.modes = append(e.modes, mode)
	return e.executeErr
}

func (e *runnerTestExecutor) Execute(context.Context, string) error {
	e.executeCount.Add(1)
	return e.executeErr
}

func (*runnerTestExecutor) Wait(context.Context, string, int) error { return nil }

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
	return []byte("jpeg"), nil
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

	result, err := runner.Run(context.Background(), &JourneySpec{
		ID:   "fail-fast",
		Name: "fail fast",
		Journey: []StepSpec{
			{ID: "first", Do: "click e1"},
			{ID: "second", Do: "click e1"},
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
	if result.Status != StatusFail {
		t.Fatalf("status = %s, want FAIL", result.Status)
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
	result, err := runner.Run(context.Background(), &JourneySpec{
		ID:      "screenshot-timeout",
		Name:    "screenshot timeout",
		Journey: []StepSpec{{ID: "first", Do: "click e1"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("runner returned after %s, want <= 1s", elapsed)
	}
	if result.Status != StatusFail || len(result.Steps) != 1 {
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

	result, err := runner.Run(context.Background(), &JourneySpec{
		ID:   "interaction-modes",
		Name: "interaction modes",
		Journey: []StepSpec{
			{ID: "visual-default", Do: "click button:'Continue'"},
			{ID: "element-escape", Do: "click button:'Below fold'", Mode: browser.InteractionModeElement},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != StatusPass {
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

	result, err := runner.Run(context.Background(), &JourneySpec{
		ID:      "unsupported-element-mode",
		Name:    "unsupported element mode",
		Journey: []StepSpec{{ID: "escape", Do: "click #target", Mode: browser.InteractionModeElement}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != StatusFail || len(result.Steps) != 1 {
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
	valid := []byte("version: 1\nid: modes\nname: modes\nenvironment:\n  base_url: http://example.test\n  mode: headless\njourney:\n  - id: escape\n    do: click '#target'\n    mode: element\nevidence: {}\n")
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
