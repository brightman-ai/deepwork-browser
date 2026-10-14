package testing

import (
	"bytes"
	"encoding/json"
	"github.com/brightman-ai/deepwork-browser/browser"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestObservationFileContract(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "obs.json")
	for _, raw := range []string{`{}`, `{"url":"https://example.test"}`, `{"schema":"future"}`, `{"schema":"dw.observe.v1"}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadObservation(path); err == nil {
			t.Fatalf("invalid artifact accepted: %s", raw)
		}
	}
	obs := BuildObservation("s", &browser.Snapshot{URL: "https://example.test"}, nil, "", nil, nil)
	if err := WriteObservation(path, obs); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadObservation(path)
	if err != nil || loaded.ID != obs.ID {
		t.Fatalf("canonical roundtrip: %v", err)
	}
	refPath := filepath.Join(dir, "ref.json")
	data, _ := json.Marshal(map[string]string{"schema": "dw.observe.ref.v2", "observation_path": "obs.json", "observation_id": obs.ID})
	if err := os.WriteFile(refPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadObservation(refPath); err != nil {
		t.Fatal(err)
	}
	obs.ID = "wrong"
	if err := WriteObservation(path, obs); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadObservation(refPath); err == nil {
		t.Fatal("mismatched identity accepted")
	}
}

func TestScreenshotFailureCannotReuseOldEvidence(t *testing.T) {
	dir := t.TempDir()
	shotPath := filepath.Join(dir, "screen.png")
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	snap := &browser.Snapshot{URL: "https://example.test"}
	good := BuildObservation("s", snap, encoded.Bytes(), shotPath, nil, nil)
	if good.Visual == nil || good.Channels["visual"].Status != "captured" {
		t.Fatal("valid PNG was not captured")
	}
	broken := BuildObservation("s", snap, nil, shotPath, nil, nil)
	if broken.Visual != nil || broken.Channels["visual"].Status != "unavailable" {
		t.Fatal("old image falsely counted as fresh capture")
	}
	blockedPath := filepath.Join(dir, "file")
	if err := os.WriteFile(blockedPath, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	broken = BuildObservation("s", snap, encoded.Bytes(), filepath.Join(blockedPath, "screen.png"), nil, nil)
	if broken.Visual != nil || broken.Channels["visual"].Status != "unavailable" {
		t.Fatal("write failure falsely counted as capture")
	}
	canonical := filepath.Join(dir, "obs.json")
	if err := WriteObservation(canonical, good); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(shotPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadObservation(canonical); err == nil {
		t.Fatal("missing evidence accepted")
	}
	restored := BuildObservation("s", snap, encoded.Bytes(), shotPath, nil, nil)
	if err := WriteObservation(canonical, restored); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadObservation(canonical); err != nil {
		t.Fatalf("restored evidence: %v", err)
	}
}

func TestCheckMissingChannelsAndExitClassification(t *testing.T) {
	obs := BuildObservation("s", &browser.Snapshot{URL: "https://example.test"}, nil, "", nil, nil)
	obs.Telemetry = &TelemetryState{ConsoleErrors: []ConsoleEntry{}, NetworkFailures: []NetworkEntry{}}
	engine := AssertionEngine{}
	for _, expr := range []string{"console_errors_count == 0", "network_failures_count == 0", "not_implemented()", "exists(unsupported='x')", "exists(role='button' garbage)", "url_matches('example.test') == 0"} {
		result := engine.Evaluate(obs, expr)
		if result.Status != StatusBlocked || CheckExitCode([]AssertionResult{*result}) != 2 {
			t.Fatalf("missing evidence false success: %#v", result)
		}
	}
	pass := engine.Evaluate(obs, "url_matches('example.test')")
	fail := engine.Evaluate(obs, "url_matches('missing.test')")
	if CheckExitCode([]AssertionResult{*pass}) != 0 || CheckExitCode([]AssertionResult{*fail}) != 1 || CheckExitCode(nil) != 2 {
		t.Fatal("incorrect check exit classification")
	}
	obs.Channels["console"] = ChannelState{Status: "captured"}
	if result := engine.Evaluate(obs, "console_errors_count == 0"); result.Status != StatusPass {
		t.Fatalf("captured empty console: %#v", result)
	}
}

func TestObservationRejectsInconsistentCapturedFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "obs.json")
	for _, corrupt := range []func(*Observation){
		func(obs *Observation) { obs.Structural.RefsCount++ },
		func(obs *Observation) { obs.Channels["content"] = ChannelState{Status: "unknown"} },
		func(obs *Observation) { obs.Channels["transcript"] = ChannelState{Status: "captured"} },
		func(obs *Observation) {
			obs.Files = map[string]FileEvidence{"*.txt": {ChannelState: ChannelState{Status: "captured"}}}
		},
	} {
		obs := BuildObservation("s", &browser.Snapshot{URL: "https://example.test"}, nil, "", nil, nil)
		corrupt(obs)
		if err := WriteObservation(path, obs); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadObservation(path); err == nil {
			t.Fatal("inconsistent canonical evidence accepted")
		}
	}
}

func TestCanonicalActionHintsRespectCapturedControlState(t *testing.T) {
	base := browser.ElementRef{Ref: "@ocapture-r1", Role: "textbox", ControlStateKnown: true, Interactable: true, Observed: true, VisibilityKnown: true, VisibleInViewport: true, Editable: true, SelectionSupported: true}
	readonly, disabled, nativeSelect := base, base, base
	readonly.Ref, readonly.ReadOnly = "@ocapture-r2", true
	disabled.Ref, disabled.Disabled = "@ocapture-r3", true
	nativeSelect.Ref, nativeSelect.Role, nativeSelect.Editable, nativeSelect.NativeSelect = "@ocapture-r4", "combobox", false, true
	snap := &browser.Snapshot{URL: "https://example.test", ObservationID: "capture", Refs: []browser.ElementRef{base, readonly, disabled, nativeSelect}}
	obs := BuildObservation("s", snap, nil, "", nil, nil)
	for _, ref := range obs.Structural.Refs {
		if len(ref.AllowedActions) != 0 {
			t.Fatal("unknown scenario advertised target actions")
		}
	}
	obs.BindActionHints(snap, browser.ScenarioAppTestBaseline)
	path := filepath.Join(t.TempDir(), "obs.json")
	if err := WriteObservation(path, obs); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadObservation(path)
	if err != nil {
		t.Fatal(err)
	}
	refs := loaded.Structural.Refs
	if !slices.Contains(refs[0].AllowedActions, "fill") || slices.Contains(refs[1].AllowedActions, "fill") || slices.Contains(refs[2].AllowedActions, "click") || !slices.Contains(refs[3].AllowedActions, "select") {
		t.Fatalf("canonical hints ignored captured controls: %+v", refs)
	}
}
