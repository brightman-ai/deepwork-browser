package testing

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func TestVisibleRequiresCapturedVisibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		refs []RefSummary
		want Status
	}{
		{"inventory alone", nil, StatusBlocked},
		{"unknown", []RefSummary{{TestID: "target", Visible: true}}, StatusBlocked},
		{"hidden", []RefSummary{{TestID: "target", VisibilityKnown: true}}, StatusFail},
		{"visible", []RefSummary{{TestID: "target", VisibilityKnown: true, Visible: true}}, StatusPass},
		{"hidden and unknown", []RefSummary{{TestID: "target", VisibilityKnown: true}, {TestID: "target"}}, StatusBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs := &Observation{Schema: "dw.observe.v2", Channels: map[string]ChannelState{"structural": {Status: "captured"}}, Structural: &StructuralState{Refs: tc.refs, DocumentTestIDs: []string{"target"}}}
			engine := &AssertionEngine{}
			if result := engine.Evaluate(obs, "visible(testid='target')"); result.Status != tc.want {
				t.Fatalf("visibility: got %s (%s), want %s", result.Status, result.Reason, tc.want)
			}
			if result := engine.Evaluate(obs, "exists(testid='target')"); result.Status != StatusPass {
				t.Fatalf("document presence changed: %s (%s)", result.Status, result.Reason)
			}
			obs.Channels["structural"] = ChannelState{Status: "unavailable"}
			if result := engine.Evaluate(obs, "visible(testid='target')"); result.Status != StatusBlocked {
				t.Fatalf("uncaptured channel must block: %s", result.Status)
			}
		})
	}
}

func TestGenerateReplayValidatesBeforeWriting(t *testing.T) {
	store, err := NewEvidenceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := &JourneySpec{Version: 2, ID: "secret-replay", Journey: []StepSpec{{ID: "fill", Kind: "action", Action: &browser.Action{Verb: "fill", Target: browser.Target{Kind: "locator", Locator: "css=textarea"}, Sensitive: true, Text: "private-inline-value"}}}}
	if err := store.GenerateReplay(spec); err == nil {
		t.Fatal("invalid sensitive inline spec accepted")
	}
	for _, name := range []string{"replay.journey.yaml", "replay.sh"} {
		if _, err := os.Stat(filepath.Join(store.Dir(), name)); !os.IsNotExist(err) {
			t.Fatalf("invalid replay wrote %s: %v", name, err)
		}
	}
	spec.Journey[0].Action = func() *browser.Action { a := spec.Journey[0].Action.WithoutText(); return &a }()
	spec.Journey[0].TextFile = "authorized-secret.txt"
	if err := store.GenerateReplay(spec); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(store.Dir(), "replay.journey.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-inline-value") || !strings.Contains(string(data), "authorized-secret.txt") {
		t.Fatalf("replay must preserve external binding without plaintext: %s", data)
	}
}
