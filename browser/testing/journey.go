package testing

import (
	"context"
	"fmt"
	"github.com/brightman-ai/deepwork-browser/browser"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

// ActionExecutor is an adapter to the shared structured action kernel.
type ActionExecutor interface {
	Execute(context.Context, browser.Action) (browser.Outcome, error)
	Snapshot(context.Context) (*browser.Snapshot, error)
	Screenshot(context.Context) ([]byte, error)
	GetSessionState(context.Context) (*BehaviorState, error)
	GetTelemetry(context.Context) (*TelemetryState, error)
	CollectRegions(context.Context) ([]RegionSnap, error)
}
type InteractionModeActionExecutor interface {
	ExecuteWithMode(context.Context, browser.Action, browser.InteractionMode) (browser.Outcome, error)
}
type MutationExecutor interface {
	ResizeViewport(context.Context, int, int) error
}
type ProbeExecutor interface {
	CollectProbe(context.Context, ProbeSpec) (ProbeFact, error)
}
type ObservationBindingExecutor interface {
	PrepareObservation(context.Context, *browser.Snapshot, string, string) error
	CommitObservation(context.Context, *browser.Snapshot) error
}

type Runner struct {
	executor          ActionExecutor
	engine            AssertionEngine
	evidence          *EvidenceStore
	transcript        *TranscriptBinding
	startTS           time.Time
	filePatterns      []string
	failFast          bool
	screenshotTimeout time.Duration
	references        []referenceObservation
	observationPaths  []string
}

const journeyScreenshotTimeout = 12 * time.Second

func NewRunner(executor ActionExecutor, evidenceDir string) (*Runner, error) {
	if executor == nil {
		return nil, fmt.Errorf("action executor required")
	}
	store, err := NewEvidenceStore(evidenceDir)
	if err != nil {
		return nil, err
	}
	return &Runner{executor: executor, evidence: store, screenshotTimeout: journeyScreenshotTimeout}, nil
}
func (r *Runner) SetFailFast(value bool)    { r.failFast = value }
func (r *Runner) SetVision(v *VisionOracle) { r.engine.Vision = v }
func (r *Runner) EnableTranscriptTracking(binding TranscriptBinding) error {
	if (binding.Path == "") == (binding.Dir == "") {
		return fmt.Errorf("transcript requires exactly one explicit path or directory")
	}
	r.transcript = &binding
	return nil
}
func (e *EvidenceStore) Dir() string { return e.dir }

func (r *Runner) Run(ctx context.Context, spec *JourneySpec) (*JourneyResult, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	r.references = nil
	r.observationPaths = nil
	r.filePatterns = nil
	for _, steps := range [][]StepSpec{spec.Journey, spec.Recovery} {
		for _, step := range steps {
			for _, check := range step.Check {
				r.addFilePattern(check.Assert)
			}
			if step.Wait != nil {
				r.addFilePattern(step.Wait.Assert)
			}
		}
	}
	if spec.Baseline != nil {
		for _, check := range spec.Baseline.Invariants {
			r.addFilePattern(check.Assert)
		}
	}
	if provider, ok := r.executor.(interface{ ObservationContext() ObservationContext }); ok && provider.ObservationContext().Deterministic && r.engine.Vision != nil {
		return nil, fmt.Errorf("baseline forbids internal LLM")
	}
	if spec.Baseline != nil {
		for _, path := range spec.Baseline.Files {
			obs, err := LoadObservation(path)
			if err != nil {
				return nil, fmt.Errorf("reference observation: %w", err)
			}
			r.references = append(r.references, referenceObservation{path, obs})
		}
	}
	start := time.Now()
	r.startTS = start
	if spec.Environment.TranscriptPath != "" || spec.Environment.TranscriptDir != "" {
		r.transcript = &TranscriptBinding{Path: spec.Environment.TranscriptPath, Dir: spec.Environment.TranscriptDir, Pattern: spec.Environment.TranscriptPattern, StartTS: start}
	}
	result := &JourneyResult{Schema: "dw.journey.v2", ID: spec.ID, Name: spec.Name, Evidence: r.evidence.Dir(), Status: StatusDelivered}
	var invariants []AssertionSpec
	if spec.Baseline != nil {
		invariants = spec.Baseline.Invariants
	}
	merge := func(status Status) {
		if status == StatusBlocked {
			result.Status = StatusBlocked
		} else if status == StatusFail && result.Status != StatusBlocked {
			result.Status = StatusFail
		} else if status == StatusPass && result.Status == StatusDelivered {
			result.Status = StatusPass
		}
	}
	for _, step := range spec.Journey {
		sr := r.runStep(ctx, step, invariants, spec.Environment.WorkspaceDir)
		result.Steps = append(result.Steps, sr)
		merge(sr.Status)
		if r.failFast && (sr.Status == StatusFail || sr.Status == StatusBlocked) {
			break
		}
	}
	// Recovery remains explicit even when the primary journey is red.
	for _, step := range spec.Recovery {
		sr := r.runStep(ctx, step, nil, spec.Environment.WorkspaceDir)
		result.Recovery = append(result.Recovery, sr)
		merge(sr.Status)
	}
	if !(r.failFast && (result.Status == StatusFail || result.Status == StatusBlocked)) {
		for i, m := range spec.Mutations {
			sr := r.runMutation(ctx, i, m, invariants, spec.Environment.WorkspaceDir)
			result.Steps = append(result.Steps, sr)
			merge(sr.Status)
			if r.failFast && (sr.Status == StatusFail || sr.Status == StatusBlocked) {
				break
			}
		}
	}
	result.Assurance = "delivery and explicitly listed required conditions"
	for _, step := range append(append([]StepResult{}, result.Steps...), result.Recovery...) {
		if step.Status == StatusDelivered {
			result.ExecutionOnlySteps = append(result.ExecutionOnlySteps, step.StepID)
		}
		for _, check := range step.Checks {
			if check.Status == StatusPass {
				result.VerifiedChecks = append(result.VerifiedChecks, step.StepID+":"+check.Assertion)
			}
		}
	}
	result.DurationMs = time.Since(start).Milliseconds()
	for _, persist := range []func() error{func() error { return r.evidence.SaveTrace(result) }, func() error { return r.evidence.GenerateReplay(spec) }, func() error { return r.evidence.GenerateReport(result) }} {
		if err := persist(); err != nil {
			result.Status = StatusBlocked
			return result, fmt.Errorf("journey evidence delivery: %w", err)
		}
	}
	if err := r.verifyRequestedEvidence(spec.Evidence.Save); err != nil {
		result.Status = StatusBlocked
		return result, err
	}
	return result, nil
}
func required(check AssertionSpec) bool { return check.Required == nil || *check.Required }
func safeAction(action *browser.Action) *RecordedAction {
	if action == nil {
		return nil
	}
	record := &RecordedAction{Kind: action.Verb, Target: action.Target, Replayable: !action.Sensitive, Sensitive: action.Sensitive, Redacted: action.Sensitive}
	if !action.Sensitive {
		copy := *action
		record.Request = &copy
	}
	return record
}

func (r *Runner) runStep(ctx context.Context, step StepSpec, invariants []AssertionSpec, workspace string) (sr StepResult) {
	sr.StepID = step.ID
	sr.Status = StatusDelivered
	sr.Action = safeAction(step.Action)
	start := time.Now()
	var before, after *Observation
	var diff *Diff
	defer func() {
		sr.LatencyMs = time.Since(start).Milliseconds()
		if len(sr.ReferenceDiffs) > 0 {
			if err := r.evidence.SaveReferenceDiffs(step.ID, sr.ReferenceDiffs); err != nil {
				sr.Status = StatusBlocked
				sr.Error = "reference diff evidence: " + err.Error()
			}
		}
		if err := r.evidence.SaveStepEvidence(step.ID, before, after, diff, sr.Checks); err != nil {
			sr.Status = StatusBlocked
			sr.Error = "step evidence: " + err.Error()
		}
	}()
	var err error
	before, err = r.observeWithExt(ctx, step.ID, "before", workspace)
	if err != nil {
		sr.Status = StatusBlocked
		sr.Error = "before observation: " + err.Error()
		return
	}
	if step.Action != nil {
		request := *step.Action
		if request.Target.Point != nil || request.End != nil {
			if !before.runtimeCapture || before.ID == "" {
				sr.Status = StatusBlocked
				sr.Error = "coordinate action requires a fresh core-issued observation"
				return
			}
			if request.Target.Point != nil {
				point := *request.Target.Point
				point.ObservationID = before.ID
				request.Target.Point = &point
			}
			if request.End != nil {
				end := *request.End
				end.ObservationID = before.ID
				request.End = &end
			}
		}
		if step.TextFile != "" {
			data, readErr := os.ReadFile(step.TextFile)
			if readErr != nil || !utf8.Valid(data) {
				sr.Status = StatusBlocked
				sr.Error = "text_file unreadable or invalid UTF-8"
				return
			}
			request = request.WithText(string(data))
		}
		if err := request.Validate(); err != nil {
			sr.Status = StatusBlocked
			sr.Error = "runtime action validation: " + err.Error()
			return
		}
		mode, _ := browser.NormalizeInteractionMode(string(step.Mode))
		sr.Fidelity = "visual"
		var outcome browser.Outcome
		if mode == browser.InteractionModeElement {
			sr.Fidelity = "synthetic-element"
			if executor, ok := r.executor.(InteractionModeActionExecutor); ok {
				outcome, err = executor.ExecuteWithMode(ctx, request, mode)
			} else {
				err = fmt.Errorf("action executor does not support step interaction modes")
			}
		} else {
			outcome, err = r.executor.Execute(ctx, request)
		}
		sr.Outcome = &outcome
		if metadata, ok := r.executor.(interface {
			LastActionFidelity() browser.ActionFidelityReport
		}); ok {
			report := metadata.LastActionFidelity()
			sr.ActionFidelity = &report
			if report.Synthetic {
				sr.Fidelity = "synthetic"
			}
		}
		if err == nil && outcome.DispatchState != "confirmed" {
			err = fmt.Errorf("action dispatch completion unproven: %q", outcome.DispatchState)
		}
		if err != nil {
			sr.Status = StatusBlocked
			sr.Error = err.Error()
			return
		}
		if err := r.evidence.RecordAction(step.ID, request); err != nil {
			sr.Status = StatusBlocked
			sr.Error = "action evidence: " + err.Error()
			return
		}
	}
	facts, err := r.collectProbes(ctx, step.Probes, false, nil)
	if err != nil {
		sr.Status = StatusBlocked
		sr.Error = "probe: " + err.Error()
		return
	}
	if step.Wait != nil {
		err = WaitForAssertion(ctx, *step.Wait, func(waitCtx context.Context) (*Observation, error) {
			fresh, err := r.collectProbes(waitCtx, step.Probes, true, facts)
			if err != nil {
				return nil, err
			}
			return r.observeWithProbes(waitCtx, step.ID, "wait", workspace, fresh)
		})
		if err != nil {
			sr.Status = StatusBlocked
			sr.Error = err.Error()
			return
		}
	}
	facts, err = r.collectProbes(ctx, step.Probes, true, facts)
	if err != nil {
		sr.Status = StatusBlocked
		sr.Error = "probe: " + err.Error()
		return
	}
	after, err = r.observeWithProbes(ctx, step.ID, "after", workspace, facts)
	if err != nil {
		sr.Status = StatusBlocked
		sr.Error = "after observation: " + err.Error()
		return
	}
	diff = ComputeDiff(before, after)
	for _, reference := range r.references {
		sr.ReferenceDiffs = append(sr.ReferenceDiffs, ReferenceDiff{ReferencePath: reference.path, ObservationID: reference.observation.ID, Diff: ComputeDiff(reference.observation, after), Acceptance: "reference-only"})
	}
	checks := append(append([]AssertionSpec{}, step.Check...), invariants...)
	for _, check := range checks {
		cr := r.engine.EvaluateWithUsing(after, check.Assert, check.Using)
		cr.ID = check.ID
		sr.Checks = append(sr.Checks, *cr)
		if required(check) {
			switch cr.Status {
			case StatusPass:
				if sr.Status == StatusDelivered {
					sr.Status = StatusPass
				}
			case StatusFail:
				if sr.Status != StatusBlocked {
					sr.Status = StatusFail
				}
			default:
				sr.Status = StatusBlocked
			}
		}
	}
	return
}
func (r *Runner) collectProbes(ctx context.Context, specs []ProbeSpec, readOnly bool, previous map[string]ProbeFact) (map[string]ProbeFact, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	facts := map[string]ProbeFact{}
	executor, ok := r.executor.(ProbeExecutor)
	if !ok {
		return facts, fmt.Errorf("requested probe executor unavailable")
	}
	for _, spec := range specs {
		if readOnly && (spec.Kind == "mutation-start" || spec.Kind == "mutation-collect") {
			facts[spec.ID] = previous[spec.ID]
			continue
		}
		fact, err := executor.CollectProbe(ctx, spec)
		if err != nil {
			fact.ChannelState = ChannelState{Status: "unavailable", Reason: err.Error()}
			facts[spec.ID] = fact
			return facts, err
		}
		if fact.Status != "captured" {
			return facts, fmt.Errorf("probe %s did not capture evidence", spec.ID)
		}
		facts[spec.ID] = fact
	}
	return facts, nil
}
func (r *Runner) observe(ctx context.Context, stepID, phase string) (*Observation, error) {
	return r.observeWithExt(ctx, stepID, phase, "")
}
func (r *Runner) observeWithExt(ctx context.Context, stepID, phase, workspace string) (*Observation, error) {
	return r.observeWithProbes(ctx, stepID, phase, workspace, nil)
}
func (r *Runner) observeWithProbes(ctx context.Context, stepID, phase, workspace string, probes map[string]ProbeFact) (*Observation, error) {
	snap, err := r.executor.Snapshot(ctx)
	if err != nil || snap == nil {
		obs := BuildObservation(stepID, nil, nil, "", nil, nil)
		obs.Channels["structural"] = ChannelState{Status: "unavailable", Reason: fmt.Sprint(err)}
		return obs, fmt.Errorf("snapshot unavailable: %v", err)
	}
	shot, err := r.captureScreenshot(ctx)
	if err != nil {
		return nil, err
	}
	behavior, bErr := r.executor.GetSessionState(ctx)
	telemetry, tErr := r.executor.GetTelemetry(ctx)
	id := snap.ObservationID
	if id == "" {
		id = NewObservationID()
	}
	path := filepath.Join(r.evidence.Dir(), "observations", id+".json")
	obs := BuildObservation(stepID, snap, shot, filepath.Join(r.evidence.Dir(), "observations", id+".png"), behavior, telemetry)
	obs.ID = id
	obs.OperationID = id
	if provider, ok := r.executor.(interface{ ObservationContext() ObservationContext }); ok {
		context := provider.ObservationContext()
		if context.SessionID != "" {
			obs.SessionID = context.SessionID
		}
		if obs.RuntimeRunID == "" {
			obs.RuntimeRunID = context.RuntimeRunID
		}
		if obs.TargetID == "" {
			obs.TargetID = context.TargetID
		}
		obs.Scenario = context.Scenario
		obs.Deterministic = context.Deterministic
	}
	obs.BindActionHints(snap, browser.Scenario(obs.Scenario))
	obs.Probes = probes
	if obs.Visual == nil {
		return obs, fmt.Errorf("screenshot unavailable: %s", obs.Channels["visual"].Reason)
	}
	if obs.Channels["structural"].Status != "captured" {
		if err := WriteObservation(path, obs); err != nil {
			return obs, err
		}
		return obs, fmt.Errorf("structural observation pending: %s", obs.Channels["structural"].Reason)
	}

	if bErr != nil {
		obs.Channels["behavior"] = ChannelState{Status: "unavailable", Reason: bErr.Error()}
	}
	if tErr != nil {
		for _, name := range []string{"console", "network"} {
			obs.Channels[name] = ChannelState{Status: "unavailable", Reason: tErr.Error()}
		}
	}
	if regions, err := r.executor.CollectRegions(ctx); err == nil {
		obs.Visual.Regions = regions
		obs.Channels["layout"] = ChannelState{Status: "captured"}
	} else {
		obs.Channels["layout"] = ChannelState{Status: "unavailable", Reason: err.Error()}
	}
	if r.transcript != nil {
		if transcript, err := LoadTranscriptState(r.transcript); err == nil {
			obs.SetTranscript(transcript)
		}
	}
	if workspace != "" {
		obs.SetWorkspaceDir(workspace)
	}
	for _, pattern := range r.filePatterns {
		if err := CaptureFileGlob(obs, pattern); err != nil {
			if obs.Files == nil {
				obs.Files = map[string]FileEvidence{}
			}
			obs.Files[pattern] = FileEvidence{ChannelState: ChannelState{Status: "unavailable", Reason: err.Error()}}
		}
	}
	if binding, ok := r.executor.(ObservationBindingExecutor); ok {
		if err := binding.PrepareObservation(ctx, snap, id, path); err != nil {
			return obs, err
		}
		obs.Structural = structuralFromSnap(snap)
	}
	// PrepareObservation may refresh the final structural projection. Apply
	// the owning scenario only after that projection, so capability hints survive.
	obs.BindActionHints(snap, browser.Scenario(obs.Scenario))
	if err := WriteObservation(path, obs); err != nil {
		return obs, err
	}
	r.observationPaths = append(r.observationPaths, path)
	if binding, ok := r.executor.(ObservationBindingExecutor); ok {
		if err := binding.CommitObservation(ctx, snap); err != nil {
			return obs, err
		}
	}
	return obs, nil
}
func (r *Runner) captureScreenshot(ctx context.Context) ([]byte, error) {
	timeout := r.screenshotTimeout
	if timeout <= 0 {
		timeout = journeyScreenshotTimeout
	}
	shotCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type captured struct {
		data []byte
		err  error
	}
	done := make(chan captured, 1)
	go func() { data, err := r.executor.Screenshot(shotCtx); done <- captured{data, err} }()
	select {
	case result := <-done:
		return result.data, result.err
	case <-shotCtx.Done():
		return nil, fmt.Errorf("journey screenshot timed out or canceled: %w", shotCtx.Err())
	}
}
func (r *Runner) runMutation(ctx context.Context, index int, m MutationStep, checks []AssertionSpec, workspace string) StepResult {
	id := fmt.Sprintf("mutation-%d-%s", index, m.Kind)
	switch m.Kind {
	case "back-forward":
		first := r.runStep(ctx, StepSpec{ID: id + "-back", Kind: "action", Action: &browser.Action{Verb: "back"}}, nil, workspace)
		if first.Status == StatusBlocked || first.Status == StatusFail {
			return first
		}
		return r.runStep(ctx, StepSpec{ID: id, Kind: "action", Action: &browser.Action{Verb: "forward"}}, checks, workspace)
	case "reload":
		return r.runStep(ctx, StepSpec{ID: id, Kind: "action", Action: &browser.Action{Verb: "reload"}}, checks, workspace)
	case "double-click":
		return r.runStep(ctx, StepSpec{ID: id, Kind: "action", Action: &browser.Action{Verb: "click", Target: m.Target, Count: 2}}, checks, workspace)
	case "viewport":
		executor, ok := r.executor.(MutationExecutor)
		if !ok {
			return StepResult{StepID: id, Status: StatusBlocked, Error: "viewport mutation executor unavailable"}
		}
		if err := executor.ResizeViewport(ctx, m.Width, m.Height); err != nil {
			return StepResult{StepID: id, Status: StatusBlocked, Error: err.Error()}
		}
		return r.runStep(ctx, StepSpec{ID: id, Kind: "observe"}, checks, workspace)
	default:
		return StepResult{StepID: id, Status: StatusBlocked, Error: "unsupported mutation"}
	}
}

func parseViewportSize(value string) (int, int, error) {
	var w, h int
	if _, err := fmt.Sscanf(value, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 || fmt.Sprintf("%dx%d", w, h) != value {
		return 0, 0, fmt.Errorf("invalid viewport size %q", value)
	}
	return w, h, nil
}

type referenceObservation struct {
	path        string
	observation *Observation
}

func (r *Runner) verifyRequestedEvidence(names []string) error {
	for _, name := range names {
		switch name {
		case "observation", "screenshot":
			if len(r.observationPaths) == 0 {
				return fmt.Errorf("requested %s evidence not delivered", name)
			}
			for _, path := range r.observationPaths {
				obs, err := LoadObservation(path)
				if err != nil {
					return err
				}
				if name == "screenshot" && (obs.Visual == nil || obs.Channels["visual"].Status != "captured") {
					return fmt.Errorf("requested screenshot not captured")
				}
			}
		case "trace", "report", "replay":
			file := map[string]string{"trace": "trace.json", "report": "report.md", "replay": "replay.journey.yaml"}[name]
			data, err := os.ReadFile(filepath.Join(r.evidence.Dir(), file))
			if err != nil || len(data) == 0 {
				return fmt.Errorf("requested %s evidence missing: %v", name, err)
			}
		}
	}
	return nil
}

func (r *Runner) addFilePattern(expression string) {
	if match := exprPattern.FindStringSubmatch(expression); match != nil && match[1] == "file_glob_count" {
		pattern := stripQuotes(match[2])
		for _, old := range r.filePatterns {
			if old == pattern {
				return
			}
		}
		r.filePatterns = append(r.filePatterns, pattern)
	}
}
