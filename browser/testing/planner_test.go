package testing

import (
	"strings"
	"testing"
)

func TestValidatePlanRejectsIncompleteFill(t *testing.T) {
	plan := &PlanResult{Goal: "navigate", Steps: []PlannedStep{{
		Description: "missing fill value",
		Action:      "fill #browser-url-input",
	}}}

	err := ValidatePlan(plan)
	if err == nil {
		t.Fatal("ValidatePlan should reject fill without value")
	}
	if !strings.Contains(err.Error(), "not valid dw-browser act syntax") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidatePlanAllowsNoopWithWait(t *testing.T) {
	plan := &PlanResult{Goal: "wait", Steps: []PlannedStep{{
		Description: "wait for page title",
		Action:      "wait",
		Wait:        "text Example Domain",
	}}}

	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("ValidatePlan rejected noop wait: %v", err)
	}
}

func TestValidatePlanRejectsUnsupportedWaitCondition(t *testing.T) {
	plan := &PlanResult{Goal: "wait", Steps: []PlannedStep{{
		Description: "unsupported prose wait",
		Action:      "click #browser-go-btn",
		Wait:        "page load",
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
		Action:      "click #browser-go-btn",
		Wait:        "url example.com",
	}}}

	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("ValidatePlan rejected valid wait: %v", err)
	}
}

func TestValidatePlanRejectsAmbiguousRoleEqualsSelector(t *testing.T) {
	plan := &PlanResult{Goal: "fill search", Steps: []PlannedStep{{
		Description: "ambiguous role selector",
		Action:      "fill role='textbox' 'remote-assist'",
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
		Action:      "click role=button[name=\"生成测试摘要\"]",
	}}}

	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("ValidatePlan rejected canonical selector: %v", err)
	}
}

func TestNormalizePlanClearsNoWaitFiller(t *testing.T) {
	plan := &PlanResult{Goal: "navigate", Steps: []PlannedStep{{
		Description: "enter url",
		Action:      " fill #browser-url-input 'https://example.com' ",
		Wait:        "none",
	}}}

	NormalizePlan(plan)
	if plan.Steps[0].Action != "fill #browser-url-input 'https://example.com'" {
		t.Fatalf("action was not trimmed: %q", plan.Steps[0].Action)
	}
	if plan.Steps[0].Wait != "" {
		t.Fatalf("wait filler was not cleared: %q", plan.Steps[0].Wait)
	}
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("normalized plan should validate: %v", err)
	}
}

func TestNormalizePlanConvertsVisibleTextWait(t *testing.T) {
	plan := &PlanResult{Goal: "summary", Steps: []PlannedStep{{
		Description: "click summary",
		Action:      "click #copy-summary",
		Wait:        "visible text 生成测试摘要",
	}}}

	NormalizePlan(plan)
	if plan.Steps[0].Wait != "text 生成测试摘要" {
		t.Fatalf("wait was not normalized: %q", plan.Steps[0].Wait)
	}
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("normalized visible text wait should validate: %v", err)
	}
}

func TestBuildPlanPromptDocumentsExecutableFillSyntax(t *testing.T) {
	prompt := buildPlanPrompt("go to example.com", &StructuralState{Text: "[@r1 textbox '输入 URL...' #browser-url-input]"})

	for _, want := range []string{
		"fill <selector> '<text>'",
		"fill #browser-url-input 'https://example.com'",
		"Every action must be directly executable",
		"Never output role='textbox'",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q\n%s", want, prompt)
		}
	}
}
