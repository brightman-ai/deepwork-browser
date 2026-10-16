package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func TestSelectCommandPreservesExplicitEmptySelectors(t *testing.T) {
	for _, field := range []string{"label", "value"} {
		for _, suffix := range [][]string{{"--" + field, ""}, {"--" + field + "="}} {
			argv := append([]string{"act", "select", "css=#choice", "--id", "task"}, suffix...)
			d, args, err := lookupCommand(argv)
			if err != nil {
				t.Fatal(err)
			}
			if err = validateCommandArgs(d, args); err != nil {
				t.Fatalf("explicit empty %s rejected: %v", field, err)
			}
			domain, _, _ := projectCommandArguments(d, args)
			action, _, err := parseTypedActionArgs(append([]string{"select"}, domain...))
			if err != nil {
				t.Fatal(err)
			}
			if action.Option == nil || action.Target.Locator != "css=#choice" {
				t.Fatalf("selector lost: %+v", action)
			}
			var value *string
			if field == "label" {
				value = action.Option.Label
			} else {
				value = action.Option.Value
			}
			if value == nil || *value != "" {
				t.Fatalf("explicit empty %s lost: %+v", field, action.Option)
			}
			facts := browser.SelectFacts{IsSelect: true, Connected: true, Complete: true, OptionCount: 2, Options: []browser.SelectOption{{Label: "", Value: "", Disabled: true}, {Label: "Alpha", Value: "alpha"}}}
			if _, err = browser.PlanSelect(facts, action.Option, false); err == nil || !strings.Contains(err.Error(), "disabled") {
				t.Fatalf("empty selector bypassed option-state validation: %v", err)
			}
			facts.Options[0].Disabled = false
			if plan, err := browser.PlanSelect(facts, action.Option, false); err != nil || plan.Index != 0 {
				t.Fatalf("valid empty option rejected: %+v %v", plan, err)
			}
		}
	}
	if err := validateDeclaredRequest([]string{"act", "zoom", "--id", "task", "--value", ""}); err == nil {
		t.Fatal("select empty exception leaked into zoom")
	}
}

func TestSelectRejectsAmbiguousCLISelectorsBeforeDispatch(t *testing.T) {
	for _, suffix := range [][]string{
		{}, {"--label", "", "--value", ""}, {"--label=", "--value=full"},
		{"--value=", "--label=完整内容"}, {"--label=", "--label", "完整内容"},
		{"--value", "full", "--value="},
	} {
		args := append([]string{"select", "css=#choice", "--id", "task"}, suffix...)
		if _, _, err := parseTypedActionArgs(args); err == nil {
			t.Fatalf("typed parser accepted ambiguity: %q", args)
		}
		if err := validateDeclaredRequest(append([]string{"act"}, args...)); err == nil {
			t.Fatalf("declaration accepted ambiguity: %q", args)
		}
	}
	if err := validateDeclaredRequest([]string{"act", "select", "--id", "task", "--label", "完整内容"}); err == nil {
		t.Fatal("missing control TARGET accepted")
	}
}

func TestSelectCLIProjectsExactFieldWithoutGuessing(t *testing.T) {
	for _, tc := range []struct{ flag, input, want string }{
		{"--label", "value:full", `"option":{"label":"value:full"}`},
		{"--value", "完整内容", `"option":{"value":"完整内容"}`},
	} {
		action, _, err := parseTypedActionArgs([]string{"select", "@obs-r1", tc.flag, tc.input, "--id", "task"})
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(action)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), tc.want) {
			t.Fatalf("selector changed or projected into legacy field: %s", data)
		}
	}
}

func TestSelectContractExplainsTargetAndExactSelectors(t *testing.T) {
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
		for _, d := range projected.Commands {
			if d.Path != "act select" {
				continue
			}
			found = true
			if len(d.Arguments) != 1 || d.Arguments[0].Name != "TARGET" || !strings.Contains(d.Arguments[0].Description, "observe") || d.MinArgs != 1 {
				t.Fatalf("missing target contract: %+v", d)
			}
			if !strings.Contains(d.Usage, "--label LABEL | --value VALUE") || len(d.Examples) != 2 || !strings.Contains(d.Guidance, "observe again") {
				t.Fatalf("missing select workflow: %+v", d)
			}
			count := 0
			for _, f := range d.Flags {
				if f.Name == "--option" {
					t.Fatal("nested option leaked as a CLI flag")
				}
				if f.Name == "--label" || f.Name == "--value" {
					count++
					if !f.AllowEmpty || !strings.Contains(f.Description, "only") || !strings.Contains(f.Description, "fallback") {
						t.Fatalf("inexact selector documentation: %+v", f)
					}
				}
				if f.Name == "--id" && !strings.Contains(f.Description, "Session") {
					t.Fatal("session flag unexplained")
				}
			}
			if count != 2 || len(d.Rules) != 1 || len(d.Rules[0].ExactlyOne) != 2 {
				t.Fatalf("missing exclusive selector declaration: %+v", d)
			}
		}
		if !found {
			t.Fatal("act select absent")
		}
	}
	protocol := generatedProtocol()
	if strings.Contains(protocol, "resolves an option label or value") || !strings.Contains(protocol, "matches only the exact DOM option value") {
		t.Fatal("protocol retains ambiguous select syntax")
	}
	ref := generatedCommandReference()
	if !strings.Contains(ref, "Control ref returned by observe") || !strings.Contains(ref, "Exact complete displayed option label only") {
		t.Fatal("generated reference omits discoverability metadata")
	}
}
