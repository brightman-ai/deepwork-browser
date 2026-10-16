package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func selectFactsFixture() SelectFacts {
	return SelectFacts{IsSelect: true, Connected: true, Complete: true, OptionCount: 3, SelectedIndex: 0, Value: "a", Options: []SelectOption{{Label: "Alpha", Value: "a"}, {Label: "全文识别", Value: "full"}, {Label: "Duplicate", Value: "dup"}}}
}

func TestSelectPlannerPreflightAndProjection(t *testing.T) {
	f := selectFactsFixture()
	p, err := PlanSelect(f, "full", false)
	if err != nil || p.Index != 1 || p.Character != "全" || p.NoOp {
		t.Fatalf("plan=%+v err=%v", p, err)
	}
	p, err = PlanSelect(f, "a", false)
	if err != nil || !p.NoOp {
		t.Fatalf("no-op=%+v err=%v", p, err)
	}
	tests := []struct {
		name   string
		mutate func(*SelectFacts)
	}{
		{"disabled", func(f *SelectFacts) { f.Disabled = true }},
		{"hidden", func(f *SelectFacts) { f.Hidden = true }},
		{"multiple", func(f *SelectFacts) { f.Multiple = true }},
		{"listbox", func(f *SelectFacts) { f.Size = 3 }},
		{"incomplete", func(f *SelectFacts) { f.Complete = false }},
		{"huge", func(f *SelectFacts) { f.OptionCount = 4097 }},
		{"target hidden", func(f *SelectFacts) { f.Options[1].Hidden = true }},
		{"target disabled", func(f *SelectFacts) { f.Options[1].Disabled = true }},
		{"empty label", func(f *SelectFacts) { f.Options[1].Label = "  " }},
		{"non BMP", func(f *SelectFacts) { f.Options[1].Label = "😀 label" }},
	}
	for _, change := range []func(*SelectFacts){func(f *SelectFacts) { f.Hidden = true }, func(f *SelectFacts) { f.Multiple = true; f.Size = 4 }, func(f *SelectFacts) { f.Options[1].Hidden = true; f.Options[1].Label = "" }} {
		f := selectFactsFixture()
		change(&f)
		if _, err := PlanSelect(f, "full", true); err != nil {
			t.Fatalf("explicit baseline functionality removed: %v", err)
		}
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := selectFactsFixture()
			tc.mutate(&f)
			if _, err := PlanSelect(f, "full", false); err == nil {
				t.Fatal("unsafe native target allowed")
			}
		})
	}
	for _, label := range []string{"", "😀 current"} {
		f := SelectFacts{IsSelect: true, Connected: true, Complete: true, OptionCount: 1, SelectedIndex: 0, Value: "a", Options: []SelectOption{{Label: label, Value: "a"}}}
		p, err := PlanSelect(f, "a", false)
		if err != nil || !p.NoOp || p.Character != "" {
			t.Fatalf("state-only noop=%+v err=%v", p, err)
		}
		c := PlanSelectCapability(f, false)
		if !c.Available || c.Execution != "no-op" || c.EvidenceScope != "state-only" {
			t.Fatalf("noop-only capability=%+v", c)
		}
	}
	// A value can identify the second duplicate label; native cycling is allowed.
	f = selectFactsFixture()
	f.Options = append(f.Options, SelectOption{Label: "Duplicate", Value: "dup2"})
	f.OptionCount++
	p, err = PlanSelect(f, "dup2", false)
	if err != nil || p.Index != 3 || p.Character != "D" {
		t.Fatalf("duplicate native plan=%+v err=%v", p, err)
	}
	f = selectFactsFixture()
	f.Options[1].Label = "Alpha Two"
	if _, err = PlanSelect(f, "full", false); err != nil {
		t.Fatalf("shared native prefix unnecessarily rejected: %v", err)
	}
	for i := 3; i < 30; i++ {
		f.Options = append(f.Options, SelectOption{Label: fmt.Sprint(i), Value: fmt.Sprint(i)})
	}
	f.OptionCount = len(f.Options)
	c := PlanSelectCapability(f, false)
	if !c.Available || c.OptionsComplete || len(c.Options) > 20 || len(c.Options) == 0 || !c.Options[0].Selected || f.Options[0].Selected {
		t.Fatalf("projection=%+v mutated=%+v", c, f.Options[0])
	}
	f.Options[0].Label = strings.Repeat("x", 3000)
	c = PlanSelectCapability(f, false)
	if len(c.Options) != 0 || c.OptionsComplete {
		t.Fatalf("byte bound not enforced: %+v", c)
	}
	f.Complete = false
	c = PlanSelectCapability(f, false)
	if c.Available {
		t.Fatal("incomplete options advertised available")
	}
}

func TestSelectOptionCapabilityMatchesPlannerWithoutMutatingFacts(t *testing.T) {
	for _, baseline := range []bool{false, true} {
		f := SelectFacts{IsSelect: true, Connected: true, Complete: true, OptionCount: 6, SelectedIndex: 0, Value: "current", Options: []SelectOption{{Label: "😀 current", Value: "current"}, {Label: "", Value: "empty"}, {Label: "😀 other", Value: "emoji"}, {Label: "Normal", Value: "normal"}, {Label: "Hidden", Value: "hidden", Hidden: true}, {Label: "Disabled", Value: "disabled", Disabled: true}}}
		before, _ := json.Marshal(f)
		c := PlanSelectCapability(f, baseline)
		if len(c.Options) != len(f.Options) {
			t.Fatalf("fixture projection truncated: %+v", c)
		}
		for i, o := range c.Options {
			_, err := PlanSelect(f, f.Options[i].Value, baseline)
			if o.Selectable != (err == nil) {
				t.Fatalf("baseline=%t option=%+v planerr=%v", baseline, o, err)
			}
			if !o.Selectable && o.Reason == "" {
				t.Fatalf("missing unavailability reason: %+v", o)
			}
		}
		after, _ := json.Marshal(f)
		if string(before) != string(after) {
			t.Fatal("capability projection mutated raw facts")
		}
		if !c.Options[0].Selectable || !strings.Contains(c.Options[0].Reason, "state-only") {
			t.Fatalf("current unsupported character missing noop scope: %+v", c.Options[0])
		}
	}
}

func TestSelectReceiptRequestedPolicyAndActualEvidence(t *testing.T) {
	b, err := json.Marshal(ActionFidelityReport{Fidelity: InteractionFidelityStrictHuman, InteractionMode: InteractionModeVisual, Execution: "native-keys", EvidenceScope: "keyboard-selection", Selection: &SelectionEvidence{Before: SelectionState{Index: 0, Value: "a"}, After: SelectionState{Index: 1, Value: "b"}, Verified: true, KeySteps: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["requested_fidelity"] != "strict-human" || fields["execution"] != "native-keys" || fields["fidelity"] != nil {
		t.Fatalf("receipt=%s", b)
	}
}

func TestSelectionStateJSONEmptyAndRedacted(t *testing.T) {
	for _, tc := range []struct {
		state SelectionState
		want  string
	}{{SelectionState{Index: 1, Value: ""}, `{"index":1,"value":""}`}, {SelectionState{Index: 1, Value: "secret", ValueRedacted: true}, `{"index":1,"value_redacted":true}`}} {
		b, err := json.Marshal(tc.state)
		if err != nil || string(b) != tc.want {
			t.Fatalf("state JSON=%s err=%v want=%s", b, err, tc.want)
		}
	}
	var decoded SelectionState
	if err := json.Unmarshal([]byte(`{"index":2,"value":""}`), &decoded); err != nil || decoded.Index != 2 || decoded.Value != "" || decoded.ValueRedacted {
		t.Fatalf("empty unmarshal=%+v err=%v", decoded, err)
	}
}

const nativeSelectExperienceFixture = `<!doctype html><meta charset="utf-8"><style>body{padding:24px}select,input,button{font:18px sans-serif;margin:12px;width:240px;height:38px}</style>
<select id="choice" data-testid="choice" aria-label="Choice"><option value="a">Alpha</option><option value="">全部</option><option value="full">全文识别</option><option value="current">当次识别</option><option value="original">保留原稿</option><option value="f">同名前缀一</option><option value="g">同名前缀二</option><option value="d1">Duplicate</option><option value="d2">Duplicate</option><option hidden value="hidden">Zhidden</option><option disabled value="disabled">Qdisabled</option><optgroup disabled label="Disabled"><option value="groupdisabled">Rdisabled</option></optgroup></select>
<button id="after" aria-label="After">After</button><input id="text" aria-label="Text"><p id="feedback">initial</p><script>
window.events=[];const mode=new URL(location.href).searchParams.get('mode');if(mode==='no-selection')choice.selectedIndex=-1;window.coverControl=()=>{const r=choice.getBoundingClientRect(),cover=document.createElement('div');Object.assign(cover.style,{position:'fixed',left:r.left+'px',top:r.top+'px',width:r.width+'px',height:r.height+'px',zIndex:'999999',background:'black'});document.body.append(cover);return true};
after.addEventListener('click',()=>feedback.textContent='button-clicked');
for(const type of ['keydown','keypress'])choice.addEventListener(type,e=>{if(mode==='block')e.preventDefault()});
for(const type of ['input','change'])choice.addEventListener(type,e=>{events.push({type,trusted:e.isTrusted,value:e.target.value});feedback.textContent=e.target.value;if(type==='change'){if(mode==='replace')e.target.replaceWith(e.target.cloneNode(true));if(mode==='focus')text.focus();if(mode==='options')e.target.appendChild(new Option('New','new'));}});
</script>`

func TestNativeSelectKeyboardExperienceAndFailureBoundaries(t *testing.T) {
	requireChromeForPool(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		page := nativeSelectExperienceFixture
		switch r.URL.Query().Get("mode") {
		case "placeholder":
			page = strings.Replace(page, `<option value="a">Alpha</option>`, `<option disabled hidden selected value="">请选择</option><option value="a">Alpha</option>`, 1)
		case "emoji-noop":
			page = strings.Replace(page, `<option value="a">Alpha</option>`, `<option value="a">😀 current</option>`, 1)
		case "baselinehidden":
			page = strings.Replace(page, `<select id="choice"`, `<select hidden id="choice"`, 1)
		case "baselinelistbox":
			page = strings.Replace(page, `<select id="choice"`, `<select multiple size="3" id="choice"`, 1)
		case "sensitive":
			page = strings.Replace(page, `<select id="choice"`, `<select data-sensitive="true" id="choice"`, 1)
		}
		fmt.Fprint(w, page)
	}))
	defer srv.Close()
	setup, stopSetup := context.WithTimeout(context.Background(), 150*time.Second)
	defer stopSetup()
	lifetime, finishSetup := fixtureRuntimeLifetime(t, setup)
	core, err := NewBrowserCore(lifetime, fmt.Sprintf("native-select-%d", time.Now().UnixNano()), WithMode(BrowserModeHeadless))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = core.Close(context.Background()) }()
	core.(ScenarioInteractionCapable).SetInteractionScenario(ScenarioAppTestExplore)
	core.SetPolicy(SessionPolicy{RemoteWrites: RemoteWriteAllow}, "")
	if _, err = core.Navigate(setup, srv.URL); err != nil {
		t.Fatal(err)
	}
	snap, err := core.(SessionCore).SnapWithSessionMode(setup, 1)
	if err != nil {
		t.Fatal(err)
	}
	requireFixtureCapture(t, snap)
	finishSetup()
	stopSetup()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	selectTarget := func() Target {
		t.Helper()
		snap, err := core.(SessionCore).SnapWithSessionMode(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range snap.Refs {
			if r.TestID == "choice" {
				return fixtureRef(r.Ref)
			}
		}
		t.Fatalf("select absent: %+v", snap.Refs)
		return Target{}
	}
	var lastReport ActionFidelityReport
	actSelect := func(value string, mode InteractionMode) error {
		t.Helper()
		target := selectTarget()
		_, _, err := core.ExecuteAction(ctx, Action{Verb: "select", Target: target, Value: value}, false, true, mode)
		lastReport = core.(ActionFidelityCapable).LastActionFidelity()
		return err
	}
	read := func() (string, []struct {
		Type    string `json:"type"`
		Trusted bool   `json:"trusted"`
		Value   string `json:"value"`
	}) {
		t.Helper()
		var state struct {
			Value  string `json:"value"`
			Events []struct {
				Type    string `json:"type"`
				Trusted bool   `json:"trusted"`
				Value   string `json:"value"`
			} `json:"events"`
		}
		if err := core.EvalJS(ctx, `({value:document.querySelector('#choice').value,events:window.events})`, &state); err != nil {
			t.Fatal(err)
		}
		return state.Value, state.Events
	}
	for _, value := range []string{"full", "", "current", "original", "g", "d2"} {
		if err := actSelect(value, InteractionModeVisual); err != nil {
			t.Fatalf("select %s: %v report=%+v", value, err, lastReport)
		}
		got, events := read()
		if got != value || lastReport.Synthetic || lastReport.Execution != "native-keys" || lastReport.EvidenceScope != "keyboard-selection" || lastReport.Selection == nil || !lastReport.Selection.Verified {
			t.Fatalf("select %s value=%s report=%+v", value, got, lastReport)
		}
		for _, e := range events {
			if !e.Trusted {
				t.Fatalf("native path injected synthetic event: %+v", e)
			}
		}
	}
	if err := actSelect("全部", InteractionModeVisual); err != nil {
		t.Fatal(err)
	}
	if lastReport.Selection == nil || !lastReport.Selection.Verified || lastReport.Selection.After.Value != "" {
		t.Fatalf("empty selection report=%+v", lastReport)
	}
	emptyJSON, err := json.Marshal(lastReport.Selection.After)
	if err != nil || !strings.Contains(string(emptyJSON), `"value":""`) {
		t.Fatalf("empty selection JSON=%s err=%v", emptyJSON, err)
	}
	_, emptyBefore := read()
	if err := actSelect("", InteractionModeVisual); err != nil {
		t.Fatal(err)
	}
	_, emptyAfter := read()
	if len(emptyBefore) != len(emptyAfter) || lastReport.Execution != "no-op" || lastReport.Selection.KeySteps != 0 {
		t.Fatalf("empty noop report=%+v", lastReport)
	}
	if err := actSelect("d2", InteractionModeVisual); err != nil {
		t.Fatal(err)
	}
	_, before := read()
	if err := actSelect("d2", InteractionModeVisual); err != nil {
		t.Fatal(err)
	}
	_, after := read()
	if len(after) != len(before) || lastReport.Execution != "no-op" || lastReport.EvidenceScope != "state-only" || lastReport.Selection.KeySteps != 0 {
		t.Fatalf("no-op emitted events/report=%+v", lastReport)
	}
	for _, bad := range []string{"hidden", "disabled", "groupdisabled"} {
		_, before = read()
		if err := actSelect(bad, InteractionModeVisual); err == nil {
			t.Fatalf("%s accepted", bad)
		}
		_, after = read()
		if len(after) != len(before) || lastReport.Dispatched {
			t.Fatalf("preflight %s dispatched report=%+v", bad, lastReport)
		}
	}
	for _, mode := range []string{"placeholder", "no-selection"} {
		if _, err := core.Navigate(ctx, srv.URL+"?mode="+mode); err != nil {
			t.Fatal(err)
		}
		if err := actSelect("full", InteractionModeVisual); err != nil {
			t.Fatalf("%s initial state should be operable: %v report=%+v", mode, err, lastReport)
		}
		if got, _ := read(); got != "full" || lastReport.Selection == nil || !lastReport.Selection.Verified {
			t.Fatalf("%s value=%q report=%+v", mode, got, lastReport)
		}
	}
	if _, err := core.Navigate(ctx, srv.URL+"?mode=emoji-noop"); err != nil {
		t.Fatal(err)
	}
	if err := actSelect("a", InteractionModeVisual); err != nil {
		t.Fatal(err)
	}
	if _, ev := read(); len(ev) != 0 || lastReport.Execution != "no-op" || lastReport.Selection.KeySteps != 0 {
		t.Fatalf("emoji noop receipt=%+v events=%+v", lastReport, ev)
	}
	// Setup an occluder after observing; success must not be reported as keys.
	blockedTarget := selectTarget()
	if err := core.EvalJS(ctx, `window.coverControl()`, new(bool)); err != nil {
		t.Fatal(err)
	}
	_, _, blockedErr := core.ExecuteAction(ctx, Action{Verb: "select", Target: blockedTarget, Value: "full"}, false, true, InteractionModeVisual)
	blockedReport := core.(ActionFidelityCapable).LastActionFidelity()
	if blockedErr == nil || blockedReport.Execution != "none" || blockedReport.EvidenceScope != "" {
		t.Fatalf("occluded err=%v report=%+v", blockedErr, blockedReport)
	}
	if _, err := core.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	// Next ordinary page operations must still work after native selection.
	_ = selectTarget() // fresh observe for the next independent action
	if _, err := executeFixtureAction(ctx, core, Action{Verb: "click", Target: fixtureLocator(`role=button[name="After"]`)}, false, true); err != nil {
		t.Fatal(err)
	}
	_ = selectTarget() // click invalidates the previous observation
	if _, err := executeFixtureAction(ctx, core, Action{Verb: "fill", Target: fixtureLocator(`role=textbox[name="Text"]`), Text: "next-step"}, false, true); err != nil {
		t.Fatal(err)
	}
	var next string
	if err := core.EvalJS(ctx, `document.querySelector('#text').value`, &next); err != nil || next != "next-step" {
		t.Fatalf("next input=%q err=%v", next, err)
	}
	for _, mode := range []string{"block", "replace", "focus", "options"} {
		if _, err := core.Navigate(ctx, srv.URL+"?mode="+mode); err != nil {
			t.Fatal(err)
		}
		if err := actSelect("full", InteractionModeVisual); err == nil {
			t.Fatalf("%s should stop without fallback", mode)
		}
		got, events := read()
		if lastReport.Synthetic || lastReport.Execution != "native-keys" || lastReport.EvidenceScope != "" || len(lastReport.HumanPath) < 4 || lastReport.Selection == nil || lastReport.Selection.Verified || !lastReport.Selection.Partial {
			t.Fatalf("%s report=%+v", mode, lastReport)
		}
		for _, e := range events {
			if !e.Trusted {
				t.Fatalf("%s used synthetic rescue", mode)
			}
		}
		if mode == "block" && got != "a" {
			t.Fatalf("blocked native changed value: %s", got)
		}
	}
	for _, privacy := range []string{"sensitive", "global-redaction"} {
		url := srv.URL + "?mode=" + privacy
		if _, err := core.Navigate(ctx, url); err != nil {
			t.Fatal(err)
		}
		if privacy == "global-redaction" {
			core.(*browserCoreImpl).snapEngine.redactFormValues = true
		}
		if err := actSelect("full", InteractionModeVisual); err != nil {
			t.Fatalf("privacy blocked native select: %v", err)
		}
		if lastReport.Selection == nil || !lastReport.Selection.Verified || !lastReport.Selection.Before.ValueRedacted || !lastReport.Selection.After.ValueRedacted || lastReport.Selection.Before.Value != "" || lastReport.Selection.After.Value != "" {
			t.Fatalf("privacy receipt leaked values: %+v", lastReport)
		}
		if err := actSelect("private-missing-query", InteractionModeVisual); err == nil || strings.Contains(err.Error(), "private-missing-query") || strings.Contains(err.Error(), "全文识别") {
			t.Fatalf("privacy diagnostics=%v", err)
		}
		if privacy == "global-redaction" {
			core.(*browserCoreImpl).snapEngine.redactFormValues = false
		}
	}
	// Cancellation during the required native buffer boundary must not rescue.
	if _, err := core.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	cancelTarget := selectTarget()
	short, stopShort := context.WithTimeout(ctx, 450*time.Millisecond)
	started := time.Now()
	_, _, cancelErr := core.ExecuteAction(short, Action{Verb: "select", Target: cancelTarget, Value: "full"}, false, true, InteractionModeVisual)
	stopShort()
	lastReport = core.(ActionFidelityCapable).LastActionFidelity()
	if cancelErr == nil || time.Since(started) > 3*time.Second || lastReport.Synthetic {
		t.Fatalf("cancel err=%v elapsed=%s report=%+v", cancelErr, time.Since(started), lastReport)
	}
	if got, _ := read(); got != "a" {
		t.Fatalf("cancelled boundary changed value: %s", got)
	}
	// Baseline synthetic behavior is available only through explicit element mode.
	if _, err := core.Navigate(ctx, srv.URL); err != nil {
		t.Fatal(err)
	}
	core.(ScenarioInteractionCapable).SetInteractionScenario(ScenarioAppTestBaseline)
	if err := actSelect("full", InteractionModeElement); err != nil {
		t.Fatal(err)
	}
	_, events := read()
	if !lastReport.Synthetic || lastReport.Execution != "synthetic" || lastReport.EvidenceScope != "functional-state" || len(events) != 2 || events[0].Trusted || events[1].Trusted {
		t.Fatalf("baseline report=%+v events=%+v", lastReport, events)
	}
	for _, mode := range []string{"baselinehidden", "baselinelistbox"} {
		if _, err := core.Navigate(ctx, srv.URL+"?mode="+mode); err != nil {
			t.Fatal(err)
		}
		if _, err := core.Snap(ctx); err != nil {
			t.Fatal(err)
		}
		_, _, err := core.ExecuteAction(ctx, Action{Verb: "select", Target: fixtureLocator("css=#choice"), Value: "full"}, false, false, InteractionModeElement)
		if err != nil {
			t.Fatalf("%s baseline regression: %v", mode, err)
		}
		if got, _ := read(); got != "full" {
			t.Fatalf("%s baseline value=%s", mode, got)
		}
		r := core.(ActionFidelityCapable).LastActionFidelity()
		if !r.Synthetic || r.Execution != "synthetic" {
			t.Fatalf("%s receipt=%+v", mode, r)
		}
	}

}
