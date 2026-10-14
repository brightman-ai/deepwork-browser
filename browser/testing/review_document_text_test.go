package testing

import (
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func TestTextContainsIncompleteEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, text, document, target string
		complete                     bool
		want                         Status
	}{
		{"redacted miss", "", "ordinary page text", "private value", false, StatusBlocked},
		{"truncated miss", "", "partial page text", "later text", false, StatusBlocked},
		{"unavailable miss", "", "", "missing", false, StatusBlocked},
		{"complete miss", "", "ordinary page text", "missing", true, StatusFail},
		{"complete empty miss", "", "", "missing", true, StatusFail},
		{"partial document witness", "", "ordinary page text", "page", false, StatusPass},
		{"compact witness", "button normal", "", "normal", false, StatusPass},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := &browser.Snapshot{Text: tc.text, DocumentText: tc.document, DocumentTextComplete: tc.complete}
			structural := structuralFromSnap(snap)
			if structural.DocumentTextComplete != tc.complete {
				t.Fatal("completeness projection mismatch")
			}
			obs := &Observation{Schema: "dw.observe.v2", Channels: map[string]ChannelState{"structural": {Status: "captured"}}, Structural: structural}
			result := (&AssertionEngine{}).Evaluate(obs, "text_contains('"+tc.target+"')")
			if result.Status != tc.want {
				t.Fatalf("got %s (%s), want %s", result.Status, result.Reason, tc.want)
			}
		})
	}
}
