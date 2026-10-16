package browser

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSelectExactWireContract(t *testing.T) {
	for _, format := range []string{"json", "yaml"} {
		decode := func(raw string) (Action, error) {
			var a Action
			var err error
			if format == "json" {
				err = json.Unmarshal([]byte(raw), &a)
			} else {
				err = yaml.Unmarshal([]byte(raw), &a)
			}
			if err == nil {
				err = a.Validate()
			}
			return a, err
		}
		for _, option := range []string{`{"label":""}`, `{"value":""}`, `{"label":"Alpha"}`, `{"value":"a"}`} {
			raw := `{"kind":"select","target":{"kind":"locator","locator":"css=#choice"},"option":` + option + `}`
			a, err := decode(raw)
			if err != nil {
				t.Fatalf("%s %s: %v", format, raw, err)
			}
			encoded, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err = json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if _, old := fields["value"]; old {
				t.Fatalf("legacy value leaked: %s", encoded)
			}
			if a.Option == nil {
				t.Fatal("option lost")
			}
		}
		for _, tail := range []string{``, `,"value":"a"`, `,"value":""`, `,"value":null`, `,"option":null`, `,"option":{}`, `,"option":{"label":null}`, `,"option":{"value":null}`, `,"option":{"label":null,"value":"a"}`, `,"option":{"label":"A","value":"a"}`, `,"option":{"label":"A","label":"B"}`, `,"option":{"value":"a","value":"b"}`, `,"option":{"label":"A"},"option":{"value":"a"}`, `,"option":{"label":4}`, `,"option":{"value":true}`, `,"option":{"label":[]}`, `,"option":{"unknown":"a"}`, `,"option":{"label":"A","unknown":null}`, `,"option":{"value":"a"},"value":null`} {
			raw := `{"kind":"select","target":{"kind":"locator","locator":"css=#choice"}` + tail + `}`
			if _, err := decode(raw); err == nil {
				t.Fatalf("%s accepted %s", format, raw)
			}
		}
		if format == "yaml" {
			for _, raw := range []string{"kind: select\ntarget: {kind: locator, locator: 'css=#choice'}\noption: {label: 4}", "kind: select\ntarget: {kind: locator, locator: 'css=#choice'}\noption: {value: null}"} {
				if _, err := decode(raw); err == nil {
					t.Fatalf("accepted %s", raw)
				}
			}
		}
	}
	for _, option := range []*SelectOptionSelector{nil, {}, {Label: SelectByLabel("a").Label, Value: SelectByValue("a").Value}} {
		if (Action{Verb: "select", Target: fixtureLocator("css=#choice"), Option: option}).Validate() == nil {
			t.Fatal("invalid in-process selector accepted")
		}
	}
}

func TestSelectExactDuplicateAndFullFacts(t *testing.T) {
	f := selectFactsFixture()
	f.Options = []SelectOption{{Label: "same", Value: "same"}, {Label: "same", Value: "same", Disabled: true, Hidden: true}}
	f.OptionCount = 2
	f.SelectedIndex = 0
	f.Value = "same"
	for _, baseline := range []bool{false, true} {
		for _, selector := range []*SelectOptionSelector{SelectByLabel("same"), SelectByValue("same")} {
			if _, err := PlanSelect(f, selector, baseline); err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("duplicate bypass: %v", err)
			}
		}
		c := PlanSelectCapability(f, baseline)
		if c.Available {
			t.Fatalf("duplicate capability: %+v", c)
		}
		for _, o := range c.Options {
			if o.Selectable || o.Option != nil || o.Reason == "" {
				t.Fatalf("duplicate affordance: %+v", o)
			}
		}
	}
	f = selectFactsFixture()
	for i := 3; i < 4096; i++ {
		f.Options = append(f.Options, SelectOption{Label: fmt.Sprintf("Choice %d", i), Value: fmt.Sprint(i)})
	}
	f.OptionCount = len(f.Options)
	f.Options[4095] = SelectOption{Label: f.Options[0].Label, Value: f.Options[0].Value, Disabled: true, Hidden: true}
	if _, err := PlanSelect(f, SelectByLabel("Alpha"), false); err == nil {
		t.Fatal("duplicate beyond display accepted")
	}
	c := PlanSelectCapability(f, false)
	if c.OptionsComplete || len(c.Options) > 20 || len(c.Options) == 0 {
		t.Fatalf("projection: %+v", c)
	}
	if c.Options[0].Selectable || c.Options[0].Option != nil {
		t.Fatalf("truncated duplicate advertised: %+v", c.Options[0])
	}
	// Duplicate labels use their unique exact value. Every emitted selector executes
	// against the full facts and resolves to its own option.
	f.Options[4095].Value = "last"
	c = PlanSelectCapability(f, false)
	if c.Options[0].Option == nil || c.Options[0].Option.Value == nil {
		t.Fatalf("unique value not advertised: %+v", c.Options[0])
	}
	for i, o := range c.Options {
		if o.Selectable {
			p, err := PlanSelect(f, o.Option, false)
			if err != nil || p.Index != i {
				t.Fatalf("discovery mismatch %d %+v %v", i, p, err)
			}
		}
	}
}
