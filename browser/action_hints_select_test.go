package browser

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestSelectHintsRequireCapturedRuntimeFacts(t *testing.T) {
	ref := ElementRef{NativeSelect: true, Interactable: true, ControlStateKnown: true, Observed: true, VisibilityKnown: true, VisibleInViewport: true}
	for _, engine := range []BrowserEngine{EngineSafari, ""} {
		ref.Locator.Engine = engine
		capability := SelectCapabilityForElement(ref, ScenarioAppTestExplore)
		if capability == nil || capability.Available || capability.Reason == "" || slices.Contains(AllowedActionsForElement(ref, ScenarioAppTestExplore), "select") {
			t.Fatalf("unproven runtime advertised select: %+v", capability)
		}
	}
	ref.Locator.Engine = EngineChrome
	ref.SelectFacts = &SelectFacts{IsSelect: true, Connected: true, Complete: true, OptionCount: 2, Options: []SelectOption{{Label: "Alpha", Value: "a"}, {Label: "Beta", Value: "b"}}}
	capability := SelectCapabilityForElement(ref, ScenarioAppTestBaseline)
	if capability == nil || !capability.Available || capability.Execution != "native-keys" || capability.EvidenceScope != "keyboard-selection" {
		t.Fatalf("baseline default is not native: %+v", capability)
	}
	if !slices.Contains(AllowedActionsForElement(ref, ScenarioAppTestBaseline), "select") {
		t.Fatal("captured native capability missing")
	}
	// Blocking observation geometry revokes execution, while retaining honest option data.
	ref.BlockedByModal = true
	if capability = SelectCapabilityForElement(ref, ScenarioAppTestExplore); capability.Available || capability.OptionCount != 2 {
		t.Fatalf("blocked select=%+v", capability)
	}
	// Internal full option facts must not inflate persisted session refs.
	encoded, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]any
	if err = json.Unmarshal(encoded, &persisted); err != nil {
		t.Fatal(err)
	}
	if _, ok := persisted["SelectFacts"]; ok {
		t.Fatal("full select facts escaped persistence")
	}
}

func TestSelectCapabilityRedactsCapturedOptionsForSensitiveControls(t *testing.T) {
	ref := ElementRef{NativeSelect: true, Interactable: true, ControlStateKnown: true, Observed: true, VisibilityKnown: true, VisibleInViewport: true, Sensitive: true,
		SelectFacts: &SelectFacts{IsSelect: true, Connected: true, Complete: true, OptionCount: 1, Options: []SelectOption{{Label: "private-label", Value: "private-value"}}}}
	capability := SelectCapabilityForElement(ref, ScenarioAppTestExplore)
	if capability == nil || !capability.Available || capability.Execution != "native-keys" || len(capability.Options) > 0 || capability.OptionsComplete || capability.OptionCount != 1 || !strings.Contains(capability.Reason, "privacy") {
		t.Fatalf("sensitive select exposed facts: %+v", capability)
	}
	encoded, err := json.Marshal(capability)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-") {
		t.Fatal("private options escaped capability projection")
	}
	if !slices.Contains(AllowedActionsForElement(ref, ScenarioAppTestExplore), "select") {
		t.Fatal("privacy disclosure disabled privately plannable select")
	}
	ref.Sensitive = false
	ref.SelectValuesRedacted = true
	capability = SelectCapabilityForElement(ref, ScenarioAppTestExplore)
	if !capability.Available || len(capability.Options) != 0 || capability.OptionsComplete || capability.OptionCount != 1 {
		t.Fatalf("global redaction disabled normal select or exposed choices: %+v", capability)
	}
	ref.Interactable = false
	if capability = SelectCapabilityForElement(ref, ScenarioAppTestExplore); capability.Available {
		t.Fatal("noninteractable control granted pointer focus capability")
	}
}
