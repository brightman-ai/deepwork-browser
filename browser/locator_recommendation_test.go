package browser

import (
	"strings"
	"testing"
)

func TestRecommendationsAreCanonicalAndPreserveQuotedNames(t *testing.T) {
	for _, name := range []string{"Save", "Quoted \"choice\" and 'single'", "Name [with] bracket", "Path\\folder 中文"} {
		ref := ElementRef{Role: "button", Name: name, MatchCount: 3, Ref: "@ocapture-r17"}
		locator := RecommendedLocatorForElement(ref)
		if strings.Contains(locator, "nth=") {
			t.Fatalf("global ordinal fabricated %s", locator)
		}
		if err := (Action{Verb: "click", Target: Target{Kind: "locator", Locator: locator}}).ValidateTemplate(); err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseSelector(locator)
		if err != nil || parsed.SType != SelectorCanonical || parsed.Role != "button" || parsed.Name != name || parsed.NameOp != "=" || parsed.Nth != 0 {
			t.Fatalf("canonical quote roundtrip failed %q: %+v %v", locator, parsed, err)
		}
	}
	ref := ElementRef{Role: "button", TestID: "quoted\"id\\with\nline"}
	locator := RecommendedLocatorForElement(ref)
	parsed, err := ParseSelector(locator)
	if err != nil || parsed.SType != SelectorCSS || !strings.HasPrefix(parsed.Raw, "[data-testid=") {
		t.Fatalf("CSS testid locator invalid %q %+v %v", locator, parsed, err)
	}
	if err := (Action{Verb: "click", Target: Target{Kind: "locator", Locator: locator}}).ValidateTemplate(); err != nil {
		t.Fatal(err)
	}
	if got := RecommendedLocatorForElement(ElementRef{Role: "textbox"}); got != "role=textbox" {
		t.Fatalf("bare role suggested %s", got)
	}
}
