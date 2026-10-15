package browser

import "testing"

func TestInitialRenderProgressiveFactsRemainIncomplete(t *testing.T) {
	engine := newSnapshotEngine()
	engine.refTable["e1"] = 1
	probe := pageLoadProbe{ReadyState: "loading", VisibilityState: "visible", PendingStylesheets: 1}
	snap := engine.progressiveFallbackFromProbe(probe, "https://example.test", "Waiting", "initial_render_pending", nil)
	if !snap.Progressive || snap.LoadState != loadStateLoading || snap.DocumentTextComplete || len(snap.Refs) != 0 {
		t.Fatalf("initial render exposed complete/actionable facts: %+v", snap)
	}
	if len(engine.refTable) != 0 || snap.Diagnostics["pending_stylesheets"] != 1 || snap.Diagnostics["paint_count"] != 0 {
		t.Fatalf("loading evidence or old refs retained: %+v", snap)
	}
}

func TestDocumentTextDistinguishesAbsentAndEmptyBody(t *testing.T) {
	if _, err := (documentTextProbe{}).textResult(); err == nil {
		t.Fatal("absent body must not provide captured empty document content")
	}
	for _, text := range []string{"", "\n", "Visible content"} {
		got, err := (documentTextProbe{HasBody: true, Text: text}).textResult()
		if err != nil || got != text {
			t.Fatalf("body text %q became %q, %v", text, got, err)
		}
	}
}
