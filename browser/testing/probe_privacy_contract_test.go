package testing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCSSProbeUnavailablePrivacyBlocksBeforeAnyRuntimeRead(t *testing.T) {
	for _, spec := range []ProbeSpec{{ID: "editor", Kind: "css", Selector: "#editor"}, {ID: "document", Kind: "text"}} {
		read := false
		fact, err := CollectProbe(context.Background(), spec, func(string, any) error { read = true; return nil }, nil, func() (bool, error) { return true, errors.New("latest session unavailable") })
		if err == nil || read || fact.Value != nil || fact.Text != nil || fact.Status != "unavailable" {
			t.Fatalf("privacy provenance silently became readable empty value: %+v %v read=%t", fact, err, read)
		}
	}
}
func TestRedactedControlValueCannotPassEmptyExpectedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "expected.txt")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	obs := &Observation{Probes: map[string]ProbeFact{"editor": {ChannelState: ChannelState{Status: "captured"}, Value: nil}}}
	expression := fmt.Sprintf("probe_value_equals_file(id='editor',file='%s')", path)
	engine := AssertionEngine{}
	if result := engine.Evaluate(obs, expression); result.Status != StatusBlocked {
		t.Fatalf("redacted value fabricated empty-file success: %+v", result)
	}
	empty := ""
	obs.Probes["editor"] = ProbeFact{ChannelState: ChannelState{Status: "captured"}, Value: &empty}
	if result := engine.Evaluate(obs, expression); result.Status != StatusPass {
		t.Fatalf("proven ordinary empty value was blocked: %+v", result)
	}
	if err := os.WriteFile(path, []byte("ordinary tail\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ordinary := "ordinary tail\n"
	obs.Probes["editor"] = ProbeFact{ChannelState: ChannelState{Status: "captured"}, Value: &ordinary}
	if result := engine.Evaluate(obs, expression); result.Status != StatusPass {
		t.Fatalf("ordinary captured value lost exact file comparison: %+v", result)
	}
}

func TestProtectedProbeTextCannotClaimCapturedEmptyText(t *testing.T) {
	count := 1
	obs := &Observation{Probes: map[string]ProbeFact{"container": {ChannelState: ChannelState{Status: "captured"}, Count: &count}}}
	engine := AssertionEngine{}
	for _, expression := range []string{"probe_text_contains(id='container',text='')", "probe_text_contains(id='container',text='missing')"} {
		if result := engine.Evaluate(obs, expression); result.Status != StatusBlocked {
			t.Fatalf("protected container text masqueraded as captured empty text: %+v", result)
		}
	}
	if result := engine.Evaluate(obs, "probe_count('container') == 1"); result.Status != StatusPass {
		t.Fatalf("protected text discarded independent captured count: %+v", result)
	}
	empty := ""
	obs.Probes["container"] = ProbeFact{ChannelState: ChannelState{Status: "captured"}, Text: &empty}
	if result := engine.Evaluate(obs, "probe_text_contains(id='container',text='missing')"); result.Status != StatusFail {
		t.Fatalf("proven ordinary empty text did not support an honest absence: %+v", result)
	}
}
