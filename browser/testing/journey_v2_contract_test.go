package testing

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/brightman-ai/deepwork-browser/browser"
	"gopkg.in/yaml.v3"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestJourneyV2StrictSchemaAndArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journey.yaml")
	base := "version: 2\nid: strict\nname: strict\njourney:\n  - id: observe\n    kind: observe\nevidence: {}\n"
	cases := []string{"", base + "unknown: true\n", base + "---\nversion: 2\n", strings.Replace(base, "kind: observe", "kind: mystery", 1), strings.Replace(base, "kind: observe", "kind: observe\n    do: click anything", 1), strings.Replace(base, "kind: observe", "kind: check\n    check:\n      - assert: made_up()\n        required: false", 1), strings.Replace(base, "kind: observe", "kind: action\n    action:\n      kind: click\n      unknown: true", 1)}
	for _, source := range cases {
		if err := os.WriteFile(path, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSpec(path); err == nil {
			t.Fatalf("unsupported data accepted: %q", source)
		}
	}
	if err := os.WriteFile(path, []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSpec(path); err != nil {
		t.Fatal(err)
	}
	for _, expr := range []string{"tab_count(ignored) == 0", "count(unknown='x') == 0", "region_not_overlapped('one')", "latency_lt(nope)", "text_contains()", "exists(role='button' extra)", "probe_json_count(id='s') == 0", "probe_value_equals_file(id='editor',file='relative.txt')"} {
		if err := ValidateAssertion(expr); err == nil {
			t.Fatalf("malformed primitive accepted without evidence: %s", expr)
		}
	}
}

func TestJourneyV2DeliveryAndRequiredUnknown(t *testing.T) {
	runner, err := NewRunner(&runnerTestExecutor{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := &JourneySpec{Version: 2, ID: "delivery", Journey: []StepSpec{{ID: "read", Kind: "observe"}}}
	result, err := runner.Run(context.Background(), spec)
	if err != nil || result.Status != StatusDelivered || len(result.ExecutionOnlySteps) != 1 {
		t.Fatalf("delivery represented as acceptance: %+v %v", result, err)
	}
	spec.Journey = append(spec.Journey, StepSpec{ID: "known-url", Kind: "check", Check: []AssertionSpec{{Assert: "url_matches('example.test')"}}})
	result, err = runner.Run(context.Background(), spec)
	if err != nil || result.Status != StatusPass || len(result.VerifiedChecks) != 1 || len(result.ExecutionOnlySteps) != 1 {
		t.Fatalf("mixed assurance not explicit: %+v %v", result, err)
	}
	spec.Journey = append(spec.Journey, StepSpec{ID: "unknown-console", Kind: "check", Check: []AssertionSpec{{Assert: "console_errors_count == 0"}}})
	result, err = runner.Run(context.Background(), spec)
	if err != nil || result.Status != StatusBlocked {
		t.Fatalf("required absent telemetry false success: %+v %v", result, err)
	}
}

func TestJourneyV2EvidenceFailureRedThenRestored(t *testing.T) {
	runner, err := NewRunner(&runnerTestExecutor{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(runner.evidence.Dir(), "trace.json")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	spec := &JourneySpec{Version: 2, ID: "evidence", Journey: []StepSpec{{ID: "read", Kind: "observe"}}, Evidence: EvidenceSpec{Save: []string{"trace", "screenshot"}}}
	result, err := runner.Run(context.Background(), spec)
	if err == nil || result.Status != StatusBlocked {
		t.Fatalf("write failure falsely completed: %+v %v", result, err)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	result, err = runner.Run(context.Background(), spec)
	if err != nil || result.Status != StatusDelivered {
		t.Fatalf("restored delivery did not recover: %+v %v", result, err)
	}
}

func TestJourneyV2ReferenceFilesAreStrictComparisons(t *testing.T) {
	runner, err := NewRunner(&runnerTestExecutor{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing.json")
	spec := &JourneySpec{Version: 2, ID: "references", Journey: []StepSpec{{ID: "read", Kind: "observe"}}, Baseline: &BaselineRef{Files: []string{missing}}}
	if _, err := runner.Run(context.Background(), spec); err == nil {
		t.Fatal("missing reference file silently ignored")
	}
	obs := BuildObservation("reference", &browser.Snapshot{URL: "http://before.test"}, nil, "", nil, nil)
	if err := WriteObservation(missing, obs); err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusDelivered || len(result.Steps[0].ReferenceDiffs) != 1 || !result.Steps[0].ReferenceDiffs[0].Diff.URL.Changed || result.Steps[0].ReferenceDiffs[0].Acceptance != "reference-only" {
		t.Fatalf("reference promoted or not compared: %+v", result)
	}
}

func TestJourneyV2ExplicitSUTProbeAndArtifactFacts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.Write([]byte("not-json"))
			return
		}
		w.Write([]byte(`{"entries":[]}`))
	}))
	defer server.Close()
	fact, err := CollectProbe(context.Background(), ProbeSpec{ID: "service", Kind: "http-json", URL: server.URL, TimeoutMs: 1000}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	obs := BuildObservation("s", &browser.Snapshot{URL: server.URL}, nil, "", nil, nil)
	obs.Probes = map[string]ProbeFact{"service": fact}
	if result := (&AssertionEngine{}).Evaluate(obs, "probe_json_count(id='service', path='entries') == 0"); result.Status != StatusPass {
		t.Fatalf("known empty entries: %+v", result)
	}
	if result := (&AssertionEngine{}).Evaluate(obs, "probe_json_count(id='service', path='missing') == 0"); result.Status != StatusBlocked {
		t.Fatalf("missing JSON path became zero: %+v", result)
	}
	if _, err := CollectProbe(context.Background(), ProbeSpec{ID: "bad", Kind: "http-json", URL: server.URL + "/bad", TimeoutMs: 1000}, nil, nil); err == nil {
		t.Fatal("malformed SUT JSON accepted")
	}
	transcript, err := NewTranscriptStateFromFile(filepath.Join(fixtureDir(t), "sample_transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	obs.SetTranscript(transcript)
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "output.txt"), []byte("actual"), 0600); err != nil {
		t.Fatal(err)
	}
	obs.SetWorkspaceDir(workspace)
	if err := CaptureFileGlob(obs, "*.txt"); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "obs.json")
	if err := WriteObservation(artifact, obs); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadObservation(artifact)
	if err != nil {
		t.Fatal(err)
	}
	for _, expr := range []string{"file_glob_count('*.txt') == 1", "transcript_has('done')"} {
		if result := (&AssertionEngine{}).Evaluate(loaded, expr); result.Status != StatusPass {
			t.Fatalf("canonical SUT facts unavailable: %s %+v", expr, result)
		}
	}
	if err := os.Remove(filepath.Join(workspace, "output.txt")); err != nil {
		t.Fatal(err)
	}
	if result := (&AssertionEngine{}).Evaluate(loaded, "file_glob_count('*.txt') == 1"); result.Status != StatusPass {
		t.Fatal("immutable observation changed after live file removal")
	}
}

func TestJourneyV2AdvisoryAndWaitNeverInventPass(t *testing.T) {
	for _, status := range []Status{StatusPass, StatusFail, StatusBlocked} {
		for _, advicePass := range []bool{true, false} {
			result := ApplyVisualSuggestion(&AssertionResult{Status: status, Confidence: 1}, &VisionResult{Pass: advicePass, Confidence: .7})
			if result.Status != status || result.Confidence != 1 {
				t.Fatal("VLM suggestion altered hard acceptance")
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitForAssertion(ctx, WaitSpec{DurationMs: 1}, nil); err == nil {
		t.Fatal("canceled duration silently completed")
	}
	if err := WaitForAssertion(context.Background(), WaitSpec{Assert: "url_matches('after')", TimeoutMs: 1}, func(context.Context) (*Observation, error) {
		return BuildObservation("s", &browser.Snapshot{URL: "http://before.test"}, nil, "", nil, nil), nil
	}); err == nil {
		t.Fatal("expired wait claimed ready")
	}
}

type pointBindingExecutor struct {
	*runnerTestExecutor
	captures int
	actions  []browser.Action
	omitID   bool
}

func (e *pointBindingExecutor) Snapshot(ctx context.Context) (*browser.Snapshot, error) {
	snap, err := e.runnerTestExecutor.Snapshot(ctx)
	e.captures++
	if !e.omitID {
		snap.ObservationID = fmt.Sprintf("core-capture-%d", e.captures)
	}
	return snap, err
}
func (e *pointBindingExecutor) Execute(_ context.Context, request browser.Action) (browser.Outcome, error) {
	e.actions = append(e.actions, request)
	if err := request.Validate(); err != nil {
		return browser.Outcome{Phase: "validation", DispatchState: "not-dispatched"}, err
	}
	return browser.Outcome{Phase: "complete", DispatchState: "confirmed"}, nil
}

func TestJourneyPointTemplateRoundtripAndFreshDispatchBinding(t *testing.T) {
	point := func(x, y float64) *browser.Point {
		return &browser.Point{X: x, Y: y, Unit: "css-px", Frame: "viewport"}
	}
	spec := &JourneySpec{Version: 2, ID: "point-template", Journey: []StepSpec{
		{ID: "click", Kind: "action", Action: &browser.Action{Verb: "click", Target: browser.Target{Kind: "point", Point: point(10, 20)}}},
		{ID: "drag", Kind: "action", Action: &browser.Action{Verb: "drag", Target: browser.Target{Kind: "point", Point: point(10, 20)}, End: point(30, 40)}},
	}}
	for index, marshal := range []func(any) ([]byte, error){json.Marshal, yaml.Marshal} {
		data, err := marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		var decoded JourneySpec
		if index == 0 {
			err = json.Unmarshal(data, &decoded)
		} else {
			err = DecodeYAML(data, &decoded)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := decoded.Validate(); err != nil {
			t.Fatal(err)
		}
		if decoded.Journey[1].Action.End.X != 30 || decoded.Journey[1].Action.End.ObservationID != "" {
			t.Fatal("typed endpoint did not roundtrip as an unbound template")
		}
	}
	executor := &pointBindingExecutor{runnerTestExecutor: &runnerTestExecutor{}}
	runner, err := NewRunner(executor, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), spec)
	if err != nil || result.Status != StatusDelivered || len(executor.actions) != 2 {
		t.Fatalf("point runner: %+v %v", result, err)
	}
	if executor.actions[0].Target.Point.ObservationID != "core-capture-1" || executor.actions[1].Target.Point.ObservationID != "core-capture-3" || executor.actions[1].End.ObservationID != "core-capture-3" {
		t.Fatalf("dispatch did not use each current capture: %+v", executor.actions)
	}
	if spec.Journey[0].Action.Target.Point.ObservationID != "" || spec.Journey[1].Action.End.ObservationID != "" {
		t.Fatal("runtime binding mutated persistent template")
	}
	replay, err := LoadSpec(filepath.Join(runner.evidence.Dir(), "replay.journey.yaml"))
	if err != nil || replay.Journey[1].Action.End.ObservationID != "" {
		t.Fatalf("replay retained runtime authority: %+v %v", replay, err)
	}
	for _, bind := range []func(){func() { spec.Journey[0].Action.Target.Point.ObservationID = "stale" }, func() { spec.Journey[1].Action.End.ObservationID = "stale" }} {
		bind()
		for _, marshal := range []func(any) ([]byte, error){json.Marshal, yaml.Marshal} {
			data, err := marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "stale.yaml")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadSpec(path); err == nil {
				t.Fatal("persisted runtime binding accepted")
			}
		}
		spec.Journey[0].Action.Target.Point.ObservationID = ""
		spec.Journey[1].Action.End.ObservationID = ""
	}
	executor = &pointBindingExecutor{runnerTestExecutor: &runnerTestExecutor{}, omitID: true}
	runner, err = NewRunner(executor, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err = runner.Run(context.Background(), spec)
	if err != nil || result.Status != StatusBlocked || len(executor.actions) != 0 {
		t.Fatalf("artifact fallback authorized coordinates: %+v %v", result, err)
	}
}

type finalProjectionExecutor struct{ *runnerTestExecutor }

func (e *finalProjectionExecutor) Snapshot(ctx context.Context) (*browser.Snapshot, error) {
	snap, err := e.runnerTestExecutor.Snapshot(ctx)
	snap.ObservationID = "final-projection-capture"
	snap.Refs[0] = browser.ElementRef{Ref: "@ofinal-projection-capture-r1", Role: "textbox", NameShort: "Draft", Value: "Safe captured value.\n", Observed: true, VisibilityKnown: true, VisibleInViewport: true, ControlStateKnown: true, Interactable: true, Editable: true, SelectionSupported: true}
	return snap, err
}
func (*finalProjectionExecutor) ObservationContext() ObservationContext {
	return ObservationContext{Scenario: string(browser.ScenarioAppTestBaseline)}
}
func (*finalProjectionExecutor) PrepareObservation(context.Context, *browser.Snapshot, string, string) error {
	return nil
}
func (*finalProjectionExecutor) CommitObservation(context.Context, *browser.Snapshot) error {
	return nil
}

func TestJourneyFinalStructuralProjectionRetainsHintsAndSafeValue(t *testing.T) {
	runner, err := NewRunner(&finalProjectionExecutor{runnerTestExecutor: &runnerTestExecutor{}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.observe(context.Background(), "read", "before")
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.observationPaths) != 1 {
		t.Fatal("canonical artifact not recorded")
	}
	obs, err := LoadObservation(runner.observationPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	refs := obs.Structural.Refs
	if len(refs) != 1 || !slices.Contains(refs[0].AllowedActions, "fill") || refs[0].Value != "Safe captured value.\n" {
		t.Fatalf("final canonical projection lost actual scenario or sanitized value: %+v", refs)
	}
}
