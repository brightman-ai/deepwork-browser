package testing

import (
	"github.com/brightman-ai/deepwork-browser/browser"
	"strings"
	"testing"
)

func TestValidatePlanRejectsIncompleteFill(t *testing.T) {
	_, err := ParsePlan([]byte(`{"steps":[{"action":{"kind":"fill","target":{"kind":"locator","locator":"css=#browser-url-input"}}}]}`))

	if err == nil {
		t.Fatal("ValidatePlan should reject fill without value")
	}
	if !strings.Contains(err.Error(), "requires explicit text field") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidatePlanAllowsNoopWithWait(t *testing.T) {
	plan := &PlanResult{Goal: "wait", Steps: []PlannedStep{{
		Description: "wait for page title",
		Action:      nil,
		Wait:        &WaitSpec{Assert: "text_contains('Example Domain')", TimeoutMs: 1000},
	}}}

	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("ValidatePlan rejected noop wait: %v", err)
	}
}

func TestValidatePlanRejectsUnsupportedWaitCondition(t *testing.T) {
	plan := &PlanResult{Goal: "wait", Steps: []PlannedStep{{
		Description: "unsupported prose wait",
		Action:      testClick("css=#browser-go-btn"),
		Wait:        &WaitSpec{Assert: "page load", TimeoutMs: 1000},
	}}}

	err := ValidatePlan(plan)
	if err == nil {
		t.Fatal("ValidatePlan should reject unsupported wait condition")
	}
	if !strings.Contains(err.Error(), "wait") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidatePlanAllowsExecutableWaitCondition(t *testing.T) {
	plan := &PlanResult{Goal: "navigate", Steps: []PlannedStep{{
		Description: "go",
		Action:      testClick("css=#browser-go-btn"),
		Wait:        &WaitSpec{Assert: "url_matches('example.com')", TimeoutMs: 1000},
	}}}

	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("ValidatePlan rejected valid wait: %v", err)
	}
}

func TestValidatePlanRejectsAmbiguousRoleEqualsSelector(t *testing.T) {
	plan := &PlanResult{Goal: "fill search", Steps: []PlannedStep{{
		Description: "ambiguous role selector",
		Action:      &browser.Action{Verb: "fill", Target: browser.Target{Kind: "locator", Locator: "role='textbox'"}, Text: "remote-assist"},
	}}}

	err := ValidatePlan(plan)
	if err == nil {
		t.Fatal("ValidatePlan should reject role='textbox'")
	}
	if !strings.Contains(err.Error(), "selector contract") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidatePlanAllowsCanonicalRoleSelector(t *testing.T) {
	plan := &PlanResult{Goal: "click", Steps: []PlannedStep{{
		Description: "canonical role selector",
		Action:      testClick("role=button[name=\"生成测试摘要\"]"),
	}}}

	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("ValidatePlan rejected canonical selector: %v", err)
	}
}

func TestNormalizePlanClearsNoWaitFiller(t *testing.T) {
	plan := &PlanResult{Goal: "navigate", Steps: []PlannedStep{{
		Description: "enter url",
		Action:      &browser.Action{Verb: "fill", Target: browser.Target{Kind: "locator", Locator: "css=#browser-url-input"}, Text: " https://example.com\n "},
		Wait:        nil,
	}}}

	NormalizePlan(plan)
	if plan.Steps[0].Action.Text != " https://example.com\n " {
		t.Fatalf("text was not preserved: %+v", plan.Steps[0].Action)
	}
	if plan.Steps[0].Wait != nil {
		t.Fatalf("wait filler was not cleared: %+v", plan.Steps[0].Wait)
	}
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("normalized plan should validate: %v", err)
	}
}

func TestNormalizePlanConvertsVisibleTextWait(t *testing.T) {
	plan := &PlanResult{Goal: "summary", Steps: []PlannedStep{{
		Description: "click summary",
		Action:      testClick("css=#copy-summary"),
		Wait:        &WaitSpec{Assert: "text_contains('生成测试摘要')", TimeoutMs: 1000},
	}}}

	NormalizePlan(plan)
	if plan.Steps[0].Wait.Assert != "text_contains('生成测试摘要')" {
		t.Fatalf("wait was not normalized: %+v", plan.Steps[0].Wait)
	}
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("normalized visible text wait should validate: %v", err)
	}
}

func TestBuildPlanPromptDocumentsExecutableFillSyntax(t *testing.T) {
	prompt := buildPlanPrompt("go to example.com", &StructuralState{Text: "[@r1 textbox '输入 URL...' #browser-url-input]"})

	for _, want := range []string{
		"fill/type requires explicit text",
		"css=#editor",
		"Every action must be directly executable",
		"Never output role='textbox'",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q\n%s", want, prompt)
		}
	}
}
