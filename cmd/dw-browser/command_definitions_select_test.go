package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func TestSelectCommandRequiresExplicitValueAndPreservesEmptyOption(t *testing.T) {
	missing := []string{"act", "select", "css=#choice", "--id", "task"}
	definition, args, err := lookupCommand(missing)
	if err != nil {
		t.Fatal(err)
	}
	if err = validateCommandArgs(definition, args); err == nil || !strings.Contains(err.Error(), "requires --value") {
		t.Fatalf("missing value passed predispatch declaration validation: %v", err)
	}
	if !slices.Contains(definition.Required, "--value") {
		t.Fatal("required value missing from SSOT")
	}
	for _, suffix := range [][]string{{"--value", ""}, {"--value="}} {
		argv := append(append([]string{}, missing...), suffix...)
		d, args, err := lookupCommand(argv)
		if err != nil {
			t.Fatal(err)
		}
		if err = validateCommandArgs(d, args); err != nil {
			t.Fatalf("explicit empty option rejected: %v", err)
		}
		domain, _, _ := projectCommandArguments(d, args)
		action, _, err := parseTypedActionArgs(append([]string{"select"}, domain...))
		if err != nil {
			t.Fatal(err)
		}
		if action.Verb != "select" || action.Value != "" || action.Target.Locator != "css=#choice" {
			t.Fatalf("empty value changed: %+v", action)
		}
		facts := browser.SelectFacts{IsSelect: true, Connected: true, Complete: true, OptionCount: 2, Options: []browser.SelectOption{{Label: "Choose one", Value: "", Disabled: true}, {Label: "Alpha", Value: "alpha"}}}
		if _, err = browser.PlanSelect(facts, action.Value, false); err == nil || !strings.Contains(err.Error(), "disabled") {
			t.Fatalf("explicit empty value did not reach real option-state validation: %v", err)
		}
		facts.Options[0].Disabled = false
		if plan, err := browser.PlanSelect(facts, action.Value, false); err != nil || plan.Index != 0 {
			t.Fatalf("valid empty option rejected: plan=%+v err=%v", plan, err)
		}
	}
	// The empty-value exception belongs solely to select, not other --value flags.
	if err := validateDeclaredRequest([]string{"act", "zoom", "--id", "task", "--value", ""}); err == nil {
		t.Fatal("select's empty-value exception leaked into zoom")
	}
}

func TestSelectValuePresenceContractProjectsIntoHelpAndCapabilities(t *testing.T) {
	for _, document := range []map[string]any{generatedHelp("act select"), capabilityDocument()} {
		encoded, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		var projected struct {
			Commands []CommandDefinition `json:"commands"`
		}
		if err = json.Unmarshal(encoded, &projected); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, definition := range projected.Commands {
			if definition.Path != "act select" {
				continue
			}
			found = true
			if !slices.Contains(definition.Required, "--value") {
				t.Fatalf("help/capabilities omit required select value: %s", encoded)
			}
			allowed := false
			for _, flag := range definition.Flags {
				if flag.Name == "--value" {
					allowed = flag.AllowEmpty
				}
			}
			if !allowed {
				t.Fatal("help/capabilities reject explicit empty option")
			}
		}
		if !found {
			t.Fatal("act select absent from projection")
		}
	}
}
