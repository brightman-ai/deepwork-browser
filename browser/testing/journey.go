package testing

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
	"gopkg.in/yaml.v3"
)

// ActionExecutor abstracts browser action execution.
// The CLI layer provides a concrete implementation wrapping BrowserSession.
type ActionExecutor interface {
	// Execute runs a natural-language or structured action (step.Do content).
	Execute(ctx context.Context, action string) error

	// Wait blocks until condition is satisfied or timeout elapses.
	Wait(ctx context.Context, condition string, timeoutMs int) error

	// Snapshot captures a page snapshot for building an Observation.
	Snapshot(ctx context.Context) (*browser.Snapshot, error)

	// Screenshot returns raw PNG bytes.
	Screenshot(ctx context.Context) ([]byte, error)

	// GetSessionState returns current behavioral state (tabs, URL, etc.).
	GetSessionState(ctx context.Context) (*BehaviorState, error)

	// GetTelemetry returns telemetry data (console errors, network failures).
	GetTelemetry(ctx context.Context) (*TelemetryState, error)

	// CollectRegions collects data-region elements and their bounding rects via JS eval.
	CollectRegions(ctx context.Context) ([]RegionSnap, error)
}

// Runner executes BDD journeys defined by JourneySpec.
type Runner struct {
	executor ActionExecutor
	engine   AssertionEngine
	evidence *EvidenceStore
}

// NewRunner creates a journey runner writing evidence to evidenceDir.
func NewRunner(executor ActionExecutor, evidenceDir string) (*Runner, error) {
	store, err := NewEvidenceStore(evidenceDir)
	if err != nil {
		return nil, fmt.Errorf("journey runner: %w", err)
	}
	return &Runner{
		executor: executor,
		evidence: store,
	}, nil
}

// SetVision configures the VisionOracle on the runner's AssertionEngine.
// Call after NewRunner to enable visual oracle for journey step checks.
func (r *Runner) SetVision(v *VisionOracle) {
	r.engine.Vision = v
}

// Dir returns the evidence directory path.
func (e *EvidenceStore) Dir() string {
	return e.dir
}

// LoadSpec parses a JourneySpec from a YAML file.
func LoadSpec(path string) (*JourneySpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load spec %s: %w", path, err)
	}
	var spec JourneySpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse spec %s: %w", path, err)
	}
	return &spec, nil
}

// Run executes the full journey and returns the aggregated result.
func (r *Runner) Run(ctx context.Context, spec *JourneySpec) (*JourneyResult, error) {
	result := &JourneyResult{
		Schema: "dw.journey.v1",
		ID:     spec.ID,
		Name:   spec.Name,
	}
	start := time.Now()

	// Collect baseline invariant checks.
	var baselineChecks []AssertionSpec
	if spec.Baseline != nil {
		baselineChecks = spec.Baseline.Invariants
	}

	// Execute journey steps.
	allPassed := true
	for _, step := range spec.Journey {
		sr := r.runStep(ctx, step, baselineChecks)
		result.Steps = append(result.Steps, sr)
		if sr.Status != StatusPass {
			allPassed = false
		}
	}

	// Execute recovery steps (no baseline checks).
	for _, step := range spec.Recovery {
		sr := r.runStep(ctx, step, nil)
		result.Recovery = append(result.Recovery, sr)
	}

	result.DurationMs = time.Since(start).Milliseconds()
	if allPassed {
		result.Status = StatusPass
	} else {
		result.Status = StatusFail
	}
	result.Evidence = r.evidence.Dir()

	// Persist evidence — log but don't fail the result on IO errors.
	if err := r.evidence.SaveTrace(result); err != nil {
		fmt.Fprintf(os.Stderr, "journey: save trace: %v\n", err)
	}
	if err := r.evidence.GenerateReplay(spec.ID); err != nil {
		fmt.Fprintf(os.Stderr, "journey: generate replay: %v\n", err)
	}
	if err := r.evidence.GenerateReport(result); err != nil {
		fmt.Fprintf(os.Stderr, "journey: generate report: %v\n", err)
	}

	return result, nil
}

// runStep executes a single step and returns its result.
func (r *Runner) runStep(ctx context.Context, step StepSpec, baselineChecks []AssertionSpec) StepResult {
	sr := StepResult{StepID: step.ID}
	start := time.Now()

	// 1. Before observation.
	before := r.observe(ctx, step.ID, "before")

	// 2. Execute action — failure is terminal for this step.
	if err := r.executor.Execute(ctx, step.Do); err != nil {
		sr.Status = StatusFail
		sr.Error = err.Error()
		sr.LatencyMs = time.Since(start).Milliseconds()
		return sr
	}
	sr.Action = step.Do
	r.evidence.RecordAction(step.ID, step.Do)

	// 3. Wait if specified.
	if step.Wait != nil {
		if err := r.executor.Wait(ctx, step.Wait.Until, step.Wait.TimeoutMs); err != nil {
			sr.Status = StatusFail
			sr.Error = "wait timeout: " + err.Error()
			sr.LatencyMs = time.Since(start).Milliseconds()
			return sr
		}
	}

	// 4. After observation.
	after := r.observe(ctx, step.ID, "after")

	// 5. Diff.
	diff := ComputeDiff(before, after)

	// 6. Step-level checks.
	for _, check := range step.Check {
		cr := r.engine.EvaluateWithUsing(after, check.Assert, check.Using)
		cr.ID = check.ID
		sr.Checks = append(sr.Checks, *cr)
	}

	// 7. Baseline invariant checks.
	for _, inv := range baselineChecks {
		cr := r.engine.EvaluateWithUsing(after, inv.Assert, inv.Using)
		cr.ID = "baseline:" + inv.ID
		sr.Checks = append(sr.Checks, *cr)
	}

	// 8. Determine step status.
	sr.LatencyMs = time.Since(start).Milliseconds()
	sr.Status = StatusPass
	for _, c := range sr.Checks {
		if c.Status == StatusFail {
			sr.Status = StatusFail
			break
		}
	}

	// 9. Persist step evidence — non-fatal.
	if err := r.evidence.SaveStepEvidence(step.ID, before, after, diff, sr.Checks); err != nil {
		fmt.Fprintf(os.Stderr, "journey: save step evidence %s: %v\n", step.ID, err)
	}

	return sr
}

// observe gathers a multi-channel Observation for the current page state.
// All sub-calls are best-effort; failures produce a partial Observation.
func (r *Runner) observe(ctx context.Context, stepID, phase string) *Observation {
	snap, _ := r.executor.Snapshot(ctx)
	// FIX-J4: wait until the SPA a11y tree is populated (settle-wait, up to ~6s).
	for i := 0; i < 6 && (snap == nil || len(snap.Refs) == 0); i++ {
		time.Sleep(1 * time.Second)
		snap, _ = r.executor.Snapshot(ctx)
	}
	screenshotData, _ := r.executor.Screenshot(ctx)
	behavior, _ := r.executor.GetSessionState(ctx)
	telemetry, _ := r.executor.GetTelemetry(ctx)

	screenshotPath := ""
	if len(screenshotData) > 0 {
		screenshotPath = ScreenshotPath(r.evidence.Dir(), stepID+"-"+phase)
	}

	obs := BuildObservation(stepID, snap, screenshotData, screenshotPath, behavior, telemetry)

	// Collect layout regions via JS eval (best-effort).
	if regions, err := r.executor.CollectRegions(ctx); err == nil && len(regions) > 0 {
		if obs.Visual == nil {
			obs.Visual = &VisualState{}
		}
		obs.Visual.Regions = regions
	}

	return obs
}
