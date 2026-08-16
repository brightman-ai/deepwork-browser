package testing

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
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

// InteractionModeActionExecutor is the optional journey extension for the
// explicit per-step mode: element escape hatch. The default visual path keeps
// using ActionExecutor.Execute.
type InteractionModeActionExecutor interface {
	ExecuteWithMode(ctx context.Context, action string, mode browser.InteractionMode) error
}

// Runner executes BDD journeys defined by JourneySpec.
type Runner struct {
	executor          ActionExecutor
	engine            AssertionEngine
	evidence          *EvidenceStore
	transcript        *TranscriptBinding // resolved at journey start; nil when not configured
	startTS           time.Time          // captured before the first step
	failFast          bool
	screenshotTimeout time.Duration
}

const journeyScreenshotTimeout = 12 * time.Second

// NewRunner creates a journey runner writing evidence to evidenceDir.
func NewRunner(executor ActionExecutor, evidenceDir string) (*Runner, error) {
	store, err := NewEvidenceStore(evidenceDir)
	if err != nil {
		return nil, fmt.Errorf("journey runner: %w", err)
	}
	return &Runner{
		executor:          executor,
		evidence:          store,
		screenshotTimeout: journeyScreenshotTimeout,
	}, nil
}

// SetFailFast stops scheduling later journey/mutation steps after the first
// failure. Recovery steps still run so a failed journey can restore its SUT.
func (r *Runner) SetFailFast(enabled bool) {
	r.failFast = enabled
}

// SetVision configures the VisionOracle on the runner's AssertionEngine.
// Call after NewRunner to enable visual oracle for journey step checks.
func (r *Runner) SetVision(v *VisionOracle) {
	r.engine.Vision = v
}

// EnableTranscriptTracking tells the runner to resolve and attach the
// session-under-test transcript to every Observation before assertion evaluation.
// startTS should be time.Now() captured before the first journey step runs.
// The binding is resolved lazily on first use (after the first step navigates
// to the app and the SUT starts writing the transcript).
func (r *Runner) EnableTranscriptTracking(startTS time.Time) {
	r.startTS = startTS
	r.transcript = &TranscriptBinding{StartTS: startTS}
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
	for section, steps := range map[string][]StepSpec{"journey": spec.Journey, "recovery": spec.Recovery} {
		for i := range steps {
			if _, err := browser.NormalizeInteractionMode(string(steps[i].Mode)); err != nil {
				return nil, fmt.Errorf("parse spec %s: %s step %q: %w", path, section, steps[i].ID, err)
			}
		}
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

	// TASK A: capture start timestamp before the first step so the transcript
	// binding can find files written after this moment.
	start := time.Now()
	if r.transcript == nil {
		// Auto-enable transcript tracking; resolved lazily on first use.
		r.transcript = &TranscriptBinding{StartTS: start}
	}
	r.startTS = start

	// Workspace dir for file_glob assertions.
	workspaceDir := spec.Environment.WorkspaceDir

	// Collect baseline invariant checks.
	var baselineChecks []AssertionSpec
	if spec.Baseline != nil {
		baselineChecks = spec.Baseline.Invariants
	}

	// Execute journey steps.
	allPassed := true
	for _, step := range spec.Journey {
		sr := r.runStep(ctx, step, baselineChecks, workspaceDir)
		result.Steps = append(result.Steps, sr)
		if sr.Status != StatusPass {
			allPassed = false
			if r.failFast {
				break
			}
		}
	}

	// Execute recovery steps (no baseline checks).
	for _, step := range spec.Recovery {
		sr := r.runStep(ctx, step, nil, workspaceDir)
		result.Recovery = append(result.Recovery, sr)
		if sr.Status != StatusPass {
			allPassed = false
			if r.failFast {
				break
			}
		}
	}

	// TASK C: execute mutations (was parsed but never run).
	if len(spec.Mutations) > 0 && !(r.failFast && !allPassed) {
		mutResults, mutErr := r.executeMutations(ctx, spec.Mutations, baselineChecks, workspaceDir)
		if mutErr != nil {
			allPassed = false
			// Record as a synthetic step so the error is visible in the report.
			result.Steps = append(result.Steps, StepResult{
				StepID: "mutations",
				Status: StatusFail,
				Error:  mutErr.Error(),
			})
		}
		result.Steps = append(result.Steps, mutResults...)
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
func (r *Runner) runStep(ctx context.Context, step StepSpec, baselineChecks []AssertionSpec, workspaceDir string) StepResult {
	sr := StepResult{StepID: step.ID}
	start := time.Now()

	// 1. Before observation.
	before, beforeErr := r.observeWithExt(ctx, step.ID, "before", workspaceDir)
	if beforeErr != nil {
		sr.Status = StatusFail
		sr.Error = "before observation: " + beforeErr.Error()
		sr.LatencyMs = time.Since(start).Milliseconds()
		_ = r.evidence.SaveStepEvidence(step.ID, before, nil, nil, nil)
		return sr
	}

	// 2. Execute action — visual is the default. element is an explicit
	// per-step escape hatch and must be supported rather than silently ignored.
	mode, modeErr := browser.NormalizeInteractionMode(string(step.Mode))
	if modeErr != nil {
		sr.Status = StatusFail
		sr.Error = modeErr.Error()
		sr.LatencyMs = time.Since(start).Milliseconds()
		return sr
	}
	var actionErr error
	if mode == browser.InteractionModeElement {
		if executor, ok := r.executor.(InteractionModeActionExecutor); ok {
			actionErr = executor.ExecuteWithMode(ctx, step.Do, mode)
		} else {
			actionErr = fmt.Errorf("journey step %q requests mode: element but the action executor does not support step interaction modes", step.ID)
		}
	} else {
		actionErr = r.executor.Execute(ctx, step.Do)
	}
	if actionErr != nil {
		sr.Status = StatusFail
		sr.Error = actionErr.Error()
		sr.LatencyMs = time.Since(start).Milliseconds()
		return sr
	}
	sr.Action = step.Do
	r.evidence.RecordAction(step.ID, step.Do)

	// 3. Wait if specified.
	if step.Wait != nil {
		var waitErr error
		if strings.HasPrefix(step.Wait.Until, "transcript_") {
			waitErr = r.waitTranscriptCondition(ctx, step.Wait.Until, step.Wait.TimeoutMs)
		} else {
			waitErr = r.executor.Wait(ctx, step.Wait.Until, step.Wait.TimeoutMs)
		}
		if waitErr != nil {
			sr.Status = StatusFail
			sr.Error = "wait timeout: " + waitErr.Error()
			sr.LatencyMs = time.Since(start).Milliseconds()
			return sr
		}
	}

	// 4. After observation.
	after, afterErr := r.observeWithExt(ctx, step.ID, "after", workspaceDir)
	if afterErr != nil {
		sr.Status = StatusFail
		sr.Error = "after observation: " + afterErr.Error()
		sr.LatencyMs = time.Since(start).Milliseconds()
		_ = r.evidence.SaveStepEvidence(step.ID, before, after, nil, nil)
		return sr
	}

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
	// Rules:
	//   - required=true + StatusFail   → step FAILS.
	//   - required=true + StatusBlocked → step FAILS (required postcondition could not be verified
	//     = infrastructure wiring problem; must surface loudly, never silently pass).
	//   - required=false + any status   → optional; does not fail the step.
	//   - advisory perceptual oracle (using:[visual]) + StatusBlocked → skip (graceful degrade),
	//     regardless of required, because VLM unavailability is expected in headless/CI.
	sr.LatencyMs = time.Since(start).Milliseconds()
	sr.Status = StatusPass
	for i, c := range sr.Checks {
		if c.Status == StatusFail || c.Status == StatusBlocked {
			// Determine required flag: step.Check checks are indexed 0..len(step.Check)-1;
			// baseline checks appended afterwards default to required=true.
			isRequired := true
			if i < len(step.Check) {
				isRequired = step.Check[i].Required
			}
			if !isRequired {
				continue // optional check, skip
			}

			// Advisory visual/VLM oracle BLOCKED → graceful degrade (skip), never hard fail.
			// Identified by: using contains "visual" AND status is BLOCKED.
			if c.Status == StatusBlocked && containsVisualUsing(c.Using) {
				continue
			}

			// Required check that is FAIL or BLOCKED → fail the step.
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
// Structural/behavior/telemetry extensions are best-effort. Screenshot is the
// primary user-perception evidence and is bounded + required: losing it fails
// the step instead of hanging forever or producing a fake-green journey.
func (r *Runner) observe(ctx context.Context, stepID, phase string) (*Observation, error) {
	return r.observeWithExt(ctx, stepID, phase, "")
}

// observeWithExt gathers an Observation and attaches SUT-side extensions
// (transcript state, workspace dir) for use by transcript_* and file_glob primitives.
func (r *Runner) observeWithExt(ctx context.Context, stepID, phase string, workspaceDir string) (*Observation, error) {
	snap, _ := r.executor.Snapshot(ctx)
	// FIX-J4: wait until the SPA a11y tree is populated (settle-wait, up to ~6s).
	for i := 0; i < 6 && (snap == nil || len(snap.Refs) == 0); i++ {
		time.Sleep(1 * time.Second)
		snap, _ = r.executor.Snapshot(ctx)
	}
	screenshotData, screenshotErr := r.captureScreenshot(ctx)
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

	// TASK A: attach TranscriptState so transcript_* primitives can read the SUT file.
	// We attempt lazy resolution; failures are surfaced by individual primitives (BLOCKED).
	if r.transcript != nil {
		ts, err := LoadTranscriptState(r.transcript)
		if err == nil {
			obs.SetTranscript(ts)
		}
		// On error: transcript field stays nil → primitives return BLOCKED with clear message.
	}

	// Attach workspace dir for file_glob primitives.
	if workspaceDir != "" {
		obs.SetWorkspaceDir(workspaceDir)
	}

	if screenshotErr != nil {
		return obs, fmt.Errorf("screenshot %s/%s: %w", stepID, phase, screenshotErr)
	}
	if len(screenshotData) == 0 {
		return obs, fmt.Errorf("screenshot %s/%s: empty image", stepID, phase)
	}
	return obs, nil
}

func (r *Runner) captureScreenshot(ctx context.Context) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := r.screenshotTimeout
	if timeout <= 0 {
		timeout = journeyScreenshotTimeout
	}
	shotCtx, shotCancel := context.WithCancel(ctx)
	defer shotCancel()
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := r.executor.Screenshot(shotCtx)
		done <- result{data: data, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case captured := <-done:
		return captured.data, captured.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		shotCancel()
		return nil, fmt.Errorf("journey screenshot timed out after %s", timeout)
	}
}

// ---------------------------------------------------------------------------
// TASK C: executeMutations
// ---------------------------------------------------------------------------

// MutationExecutor is an optional extension of ActionExecutor for viewport resize.
// Implemented by oneshotActionExecutor in testing.go when BrowserCore supports it.
type MutationExecutor interface {
	// ResizeViewport sets the browser viewport to the given dimensions.
	ResizeViewport(ctx context.Context, width, height int) error
}

// executeMutations runs each MutationStep after the main journey.
// Returns a slice of StepResults (one per mutation) and any fatal error.
func (r *Runner) executeMutations(ctx context.Context, mutations []MutationStep, layoutChecks []AssertionSpec, workspaceDir string) ([]StepResult, error) {
	var results []StepResult

	for _, m := range mutations {
		sr := StepResult{StepID: "mutation:" + m.Name}
		start := time.Now()

		var execErr error
		switch {
		case m.Name == "back_forward_recovery":
			execErr = r.executor.Execute(ctx, "back")
			if execErr == nil {
				execErr = r.executor.Execute(ctx, "forward")
			}
			sr.Action = "back then forward"

		case m.Name == "reload":
			// Reload via JS: window.location.reload() sent as an act command.
			execErr = r.executor.Execute(ctx, "javascript:window.location.reload()")
			if execErr != nil {
				// Fallback: some engines accept a "reload" keyword.
				execErr = r.executor.Execute(ctx, "reload")
			}
			sr.Action = "reload (F5)"

		case strings.HasPrefix(m.Name, "viewport:") || m.Name == "viewport":
			// Supports both "viewport:1280x800" (name encoding) and
			// mapping form {"viewport": "1280x800"}.
			sizeStr := ""
			if strings.HasPrefix(m.Name, "viewport:") {
				sizeStr = strings.TrimPrefix(m.Name, "viewport:")
			} else {
				// mapping form: Params["viewport"] = "1280x800" or Params["width"]/["height"]
				sizeStr = m.Params[m.Name] // e.g. Params["viewport"] = "1280x800"
				if sizeStr == "" {
					sizeStr = m.Params["size"]
				}
			}
			// Named presets.
			switch sizeStr {
			case "mobile":
				sizeStr = "375x812"
			case "tablet":
				sizeStr = "768x1024"
			case "desktop":
				sizeStr = "1440x900"
			}
			w, h, parseErr := parseViewportSize(sizeStr)
			if parseErr != nil {
				execErr = fmt.Errorf("mutation viewport: %w", parseErr)
			} else if me, ok := r.executor.(MutationExecutor); ok {
				execErr = me.ResizeViewport(ctx, w, h)
				sr.Action = fmt.Sprintf("resize viewport to %dx%d", w, h)
			} else {
				// ActionExecutor doesn't implement MutationExecutor — best-effort via JS.
				execErr = r.executor.Execute(ctx, fmt.Sprintf(
					"javascript:window.resizeTo(%d,%d)", w, h))
				sr.Action = fmt.Sprintf("resize viewport to %dx%d (JS fallback)", w, h)
			}

		case m.Name == "rapid_double_click":
			selector := m.Params["selector"]
			if selector == "" {
				execErr = fmt.Errorf("mutation rapid_double_click: missing 'selector' param")
			} else {
				execErr = r.executor.Execute(ctx, fmt.Sprintf("click %s", selector))
				if execErr == nil {
					execErr = r.executor.Execute(ctx, fmt.Sprintf("click %s", selector))
				}
				sr.Action = fmt.Sprintf("rapid double click %s", selector)
			}

		default:
			return results, fmt.Errorf("mutation: unknown mutation name %q (supported: back_forward_recovery, reload, viewport:<WxH>, rapid_double_click)", m.Name)
		}

		if execErr != nil {
			sr.Status = StatusFail
			sr.Error = execErr.Error()
			sr.LatencyMs = time.Since(start).Milliseconds()
			results = append(results, sr)
			continue
		}

		// Re-run layout/structural checks declared in the baseline after each mutation.
		if len(layoutChecks) > 0 {
			obs, observeErr := r.observeWithExt(ctx, sr.StepID, "after-mutation", workspaceDir)
			if observeErr != nil {
				sr.Status = StatusFail
				sr.Error = "after mutation observation: " + observeErr.Error()
				sr.LatencyMs = time.Since(start).Milliseconds()
				results = append(results, sr)
				continue
			}
			for _, inv := range layoutChecks {
				using := inv.Using
				// Only re-run layout and structural checks.
				isLayout := false
				for _, u := range using {
					if u == "layout" || u == "structural" || u == "behavior" {
						isLayout = true
						break
					}
				}
				if !isLayout {
					continue
				}
				cr := r.engine.EvaluateWithUsing(obs, inv.Assert, using)
				cr.ID = "mutation-check:" + inv.ID
				sr.Checks = append(sr.Checks, *cr)
			}
		}

		sr.LatencyMs = time.Since(start).Milliseconds()
		sr.Status = StatusPass
		for _, c := range sr.Checks {
			if c.Status == StatusFail {
				sr.Status = StatusFail
				break
			}
		}
		results = append(results, sr)
	}

	return results, nil
}

// waitTranscriptCondition polls a transcript oracle condition every ~2-3 s until
// it evaluates true or timeoutMs elapses. condition must start with "transcript_".
//
// Supported conditions:
//   - transcript_has('done')          — true when ≥1 workstream.kind=="done" event exists
//   - transcript_done_count() >= N    — true when done event count satisfies the comparison
//
// Each poll re-resolves the TranscriptBinding (so it detects the JSONL once written)
// and re-parses the file to pick up new lines.
func (r *Runner) waitTranscriptCondition(ctx context.Context, condition string, timeoutMs int) error {
	if timeoutMs <= 0 {
		timeoutMs = 30000
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)

	// Ensure we have a binding to work with.
	if r.transcript == nil {
		r.transcript = &TranscriptBinding{StartTS: r.startTS}
	}

	pollInterval := 2500 * time.Millisecond
	var lastReason string
	for {
		// Re-resolve and re-parse on every poll so we pick up new JSONL content.
		ts, err := LoadTranscriptState(r.transcript)
		if err == nil {
			// Build a minimal Observation with only the transcript attached.
			obs := &Observation{}
			obs.SetTranscript(ts)

			// Evaluate the condition using the assertion engine.
			result := (&AssertionEngine{}).Evaluate(obs, condition)
			if result.Status == StatusPass {
				return nil
			}
			lastReason = result.Reason
		} else {
			lastReason = fmt.Sprintf("transcript not yet available: %v", err)
		}

		if time.Now().After(deadline) {
			break
		}

		// Sleep up to pollInterval, but wake early if ctx is cancelled.
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait transcript condition %q cancelled: %w", condition, ctx.Err())
		case <-time.After(pollInterval):
		}

		if time.Now().After(deadline) {
			break
		}
	}

	return fmt.Errorf("wait transcript condition %q timed out after %dms (last: %s)", condition, timeoutMs, lastReason)
}

// containsVisualUsing reports whether the using slice contains "visual".
// Used by runStep to identify advisory VLM/perceptual oracles that may be gracefully skipped.
func containsVisualUsing(using []string) bool {
	for _, u := range using {
		if u == "visual" {
			return true
		}
	}
	return false
}

// parseViewportSize parses "WxH" (e.g. "1280x800") into (width, height).
func parseViewportSize(s string) (int, int, error) {
	parts := strings.SplitN(s, "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid viewport size %q (expected WxH, e.g. 1280x800)", s)
	}
	w, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || w <= 0 {
		return 0, 0, fmt.Errorf("invalid viewport width %q in %q", parts[0], s)
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || h <= 0 {
		return 0, 0, fmt.Errorf("invalid viewport height %q in %q", parts[1], s)
	}
	return w, h, nil
}
