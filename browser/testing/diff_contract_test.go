package testing

import "testing"

func TestSemanticDiffIgnoresCapturedRefIdentityWithoutMutatingFacts(t *testing.T) {
	before := &Observation{ID: "before", Structural: &StructuralState{Text: "[@obefore-r1 textbox 'Draft']", RefsCount: 1, Refs: []RefSummary{{Ref: "@obefore-r1"}}}}
	after := &Observation{ID: "after", Structural: &StructuralState{Text: "[@oafter-r1 textbox 'Draft']", RefsCount: 1, Refs: []RefSummary{{Ref: "@oafter-r1"}}}}
	diff := ComputeDiff(before, after)
	if len(diff.Structural.AddedText) != 0 || len(diff.Structural.RemovedText) != 0 {
		t.Fatalf("capture namespace produced false semantic change: %+v", diff.Structural)
	}
	if before.Structural.Text != "[@obefore-r1 textbox 'Draft']" || after.Structural.Text != "[@oafter-r1 textbox 'Draft']" {
		t.Fatal("raw captured text was normalized in place")
	}
	after.Structural.Text = "[@oafter-r1 textbox 'Saved draft']"
	diff = ComputeDiff(before, after)
	if len(diff.Structural.AddedText) != 1 || len(diff.Structural.RemovedText) != 1 {
		t.Fatalf("real captured name change disappeared: %+v", diff.Structural)
	}
}
func TestSemanticDiffRetainsUnknownRefLookingText(t *testing.T) {
	before := &Observation{ID: "before", Structural: &StructuralState{Text: "[@ounknown-before-r1 textbox 'Draft']"}}
	after := &Observation{ID: "after", Structural: &StructuralState{Text: "[@ounknown-after-r1 textbox 'Draft']"}}
	diff := ComputeDiff(before, after)
	if len(diff.Structural.AddedText) != 1 || len(diff.Structural.RemovedText) != 1 {
		t.Fatalf("uncaptured token text was silently erased: %+v", diff.Structural)
	}
}
