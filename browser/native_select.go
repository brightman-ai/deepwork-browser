package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

const nativeSelectOptionLimit = 4096
const nativeSelectKeyLimit = 128

// Blink TypeAhead clears its native buffer after event timestamp delta >1s.
// https://chromium.googlesource.com/chromium/src/+/main/third_party/blink/renderer/core/html/forms/type_ahead.cc
const nativeSelectBufferBoundary = 1100 * time.Millisecond

// SelectOption contains observed facts; it does not grant a popup option ref.
type SelectOption struct {
	Label    string `json:"label"`
	Value    string `json:"value"`
	Disabled bool   `json:"disabled"`
	Hidden   bool   `json:"hidden"`
	Selected bool   `json:"selected"`
}

// SelectFacts is the complete bounded control probe shared by execution and observation.
// Complete refers to the internal 4096-option bound, never the display projection.
type SelectFacts struct {
	IsSelect      bool           `json:"is_select"`
	Disabled      bool           `json:"disabled"`
	Hidden        bool           `json:"hidden"`
	Sensitive     bool           `json:"sensitive"`
	Multiple      bool           `json:"multiple"`
	Focused       bool           `json:"focused"`
	Connected     bool           `json:"connected"`
	Complete      bool           `json:"complete"`
	Size          int            `json:"size"`
	SelectedIndex int            `json:"selected_index"`
	OptionCount   int            `json:"option_count"`
	Value         string         `json:"value"`
	Options       []SelectOption `json:"options"`
}

// SelectOptionCapability adds affordance evidence only to the bounded projection.
// Raw SelectFacts.Options stays unchanged for identity/fact comparisons.
type SelectOptionCapability struct {
	SelectOption
	Selectable bool   `json:"selectable"`
	Reason     string `json:"reason,omitempty"`
}

type SelectCapability struct {
	Available       bool                     `json:"available"`
	Execution       string                   `json:"execution"`
	EvidenceScope   string                   `json:"evidence_scope"`
	Reason          string                   `json:"reason,omitempty"`
	Options         []SelectOptionCapability `json:"options"`
	OptionsComplete bool                     `json:"options_complete"`
	OptionCount     int                      `json:"option_count"`
}

type SelectPlan struct {
	Index     int
	MatchKind string
	Character string
	NoOp      bool
}

// NativeSelectProbeJS is read-only and is the sole source of select facts in
// snapshots and actions. Options over the bound are never silently truncated.
const NativeSelectProbeJS = `function() {
 if (!this || String(this.tagName||'').toUpperCase() !== 'SELECT') return {is_select:false,options:[],complete:false};
 const el=this;
 const hidden = n => {for(let p=n;p&&p.nodeType===1;p=p.parentElement){const s=getComputedStyle(p);if(p.hidden||s.display==='none'||s.visibility==='hidden'||s.visibility==='collapse')return true;}return false;};
 let current=el, root=current.getRootNode(), focused=true;
 for(let depth=0;depth<32;depth++) {
  if(!root||root.activeElement!==current){focused=false;break;}
  if(root.nodeType===11&&root.host){current=root.host;root=current.getRootNode();continue;}
  if(root.nodeType===9){const win=root.defaultView;if(!win||win===win.top)break;try{current=win.frameElement;}catch(_){focused=false;break;}if(!current){focused=false;break;}root=current.getRootNode();continue;}
  focused=false;break;
 }
 const count=el.options.length, complete=count<=4096;
 const options=complete?Array.from(el.options,option=>({label:String(option.label||option.textContent||'').trim(),value:String(option.value??''),disabled:!!option.disabled||!!(option.parentElement&&option.parentElement.disabled),hidden:hidden(option),selected:!!option.selected})):[];
 return {sensitive:(` + intrinsicSensitiveControlJS + `)(el),is_select:true,disabled:!!el.disabled||el.matches(':disabled'),hidden:hidden(el),multiple:!!el.multiple,focused,connected:el.isConnected,complete,size:Number(el.size),selected_index:el.selectedIndex,option_count:count,value:String(el.value??''),options};
}`

func ProbeSelectFacts(ctx context.Context, obj *runtime.RemoteObject, out *SelectFacts) error {
	result, exc, err := runtime.CallFunctionOn(NativeSelectProbeJS).WithObjectID(obj.ObjectID).WithReturnByValue(true).Do(ctx)
	if err != nil {
		return err
	}
	if exc != nil {
		return fmt.Errorf("native select probe: %s", exc.Text)
	}
	if result == nil || len(result.Value) == 0 {
		return fmt.Errorf("native select probe returned no facts")
	}
	return json.Unmarshal(result.Value, out)
}

func selectCharacter(label string) (string, error) {
	label = strings.TrimSpace(label)
	r, _ := utf8.DecodeRuneInString(label)
	if label == "" || r == utf8.RuneError || r > 0xFFFF || !unicode.IsPrint(r) || unicode.IsSpace(r) {
		return "", fmt.Errorf("label has no supported printable BMP typeahead character")
	}
	return string(r), nil
}

func selectControlReason(f SelectFacts, baselineElement bool) string {
	switch {
	case !f.IsSelect:
		return "target is not a native select"
	case !f.Connected:
		return "select is detached"
	case !f.Complete || f.OptionCount > nativeSelectOptionLimit || len(f.Options) != f.OptionCount:
		return "select exceeds complete 4096-option bound"
	case f.Disabled:
		return "select is disabled"
	case !baselineElement && f.Hidden:
		return "select is hidden"
	case !baselineElement && (f.Multiple || f.Size > 1):
		return "multiple/listbox select is unsupported"
	case f.OptionCount == 0:
		return "select has no options"
	}
	return ""
}

// PlanSelectCapability projects facts within 20 options / 2KiB, explicitly
// declaring whether this display contains every option. Execution uses Facts.
func PlanSelectCapability(f SelectFacts, baselineElement bool) SelectCapability {
	c := SelectCapability{Execution: "native-keys", EvidenceScope: "keyboard-selection", OptionCount: f.OptionCount, Options: []SelectOptionCapability{}}
	if baselineElement {
		c.Execution = "synthetic"
		c.EvidenceScope = "functional-state"
	}
	c.Reason = selectControlReason(f, baselineElement)
	hasInputPath := false
	used := 2 // JSON array brackets; include commas below.
	for index, o := range f.Options {
		plan, err := planSelectOption(f, index, baselineElement)
		reason := ""
		if c.Reason != "" {
			reason = c.Reason
		} else if err != nil {
			reason = err.Error()
		} else {
			c.Available = true
			if !plan.NoOp {
				hasInputPath = true
			}
		}
		o.Selected = index == f.SelectedIndex
		projected := SelectOptionCapability{SelectOption: o, Selectable: reason == "", Reason: reason}
		if projected.Selectable && plan.NoOp {
			projected.Reason = "already selected; state-only no-op"
		}
		encoded, _ := json.Marshal(projected)
		cost := len(encoded)
		if len(c.Options) > 0 {
			cost++
		}
		if len(c.Options) < 20 && used+cost <= 2048 {
			c.Options = append(c.Options, projected)
			used += cost
		} else { // Keep scanning full facts for capability even after display fills.
			used = 2049
		}
	}
	if c.Reason == "" && !c.Available {
		c.Reason = "select has no selectable option with a supported label"
	}
	if c.Available && !hasInputPath {
		c.Execution = "no-op"
		c.EvidenceScope = "state-only"
		c.Reason = "only the current option is supported as a state-only no-op"
	}
	c.OptionsComplete = f.Complete && len(c.Options) == f.OptionCount
	return c
}

func planSelectOption(f SelectFacts, index int, baselineElement bool) (SelectPlan, error) {
	o := f.Options[index]
	p := SelectPlan{Index: index, NoOp: f.SelectedIndex == index && f.Value == o.Value}
	if o.Disabled {
		return SelectPlan{}, fmt.Errorf("target option is disabled")
	}
	if !baselineElement && o.Hidden {
		return SelectPlan{}, fmt.Errorf("target option is hidden")
	}
	if !baselineElement && !p.NoOp {
		var err error
		p.Character, err = selectCharacter(o.Label)
		if err != nil {
			return SelectPlan{}, err
		}
	}
	return p, nil
}

func PlanSelect(f SelectFacts, query string, baselineElement bool) (SelectPlan, error) {
	if reason := selectControlReason(f, baselineElement); reason != "" {
		return SelectPlan{}, fmt.Errorf("%w: %s", ErrActFailed, reason)
	}
	match, err := matchNativeSelectOption(f.Options, query)
	if err != nil {
		return SelectPlan{}, err
	}
	p, err := planSelectOption(f, match.Index, baselineElement)
	if err != nil {
		return SelectPlan{}, fmt.Errorf("%w: %v", ErrActFailed, err)
	}
	p.MatchKind = match.Kind
	return p, nil
}

type nativeSelectMatch struct {
	Index int
	Kind  string
}

func formatNativeSelectOptions(options []SelectOption) string {
	if len(options) == 0 {
		return "<none>"
	}
	items := []string{}
	used := 0
	for _, o := range options {
		item := fmt.Sprintf("%q→%q", o.Label, o.Value)
		if o.Disabled {
			item += " (disabled)"
		}
		if o.Hidden {
			item += " (hidden)"
		}
		if len(items) >= 20 || used+len(item) > 2048 {
			break
		}
		items = append(items, item)
		used += len(item)
	}
	result := strings.Join(items, ", ")
	if len(items) < len(options) {
		result += fmt.Sprintf(" [incomplete: shown %d of %d]", len(items), len(options))
	}
	return result
}

// matchNativeSelectOption implements the CLI contract in strict priority:
// exact value, exact visible label, then unique visible-label prefix.
func matchNativeSelectOption(options []SelectOption, query string) (nativeSelectMatch, error) {
	for index, option := range options {
		if option.Value == query {
			return nativeSelectMatch{Index: index, Kind: "value"}, nil
		}
	}
	for index, option := range options {
		if option.Label == query {
			return nativeSelectMatch{Index: index, Kind: "label"}, nil
		}
	}
	prefixMatches := make([]int, 0, 2)
	for index, option := range options {
		if strings.HasPrefix(option.Label, query) {
			prefixMatches = append(prefixMatches, index)
		}
	}
	if len(prefixMatches) == 1 {
		index := prefixMatches[0]
		return nativeSelectMatch{Index: index, Kind: "label-prefix"}, nil
	}
	if len(prefixMatches) > 1 {
		matched := make([]SelectOption, 0, len(prefixMatches))
		for _, index := range prefixMatches {
			matched = append(matched, options[index])
		}
		return nativeSelectMatch{}, fmt.Errorf("%w: label prefix %q is ambiguous (%s); options: %s",
			ErrActFailed, query, formatNativeSelectOptions(matched), formatNativeSelectOptions(options))
	}
	return nativeSelectMatch{}, fmt.Errorf("%w: no option matched %q by exact value, exact label, or unique label prefix; options: %s",
		ErrActFailed, query, formatNativeSelectOptions(options))
}

func (e *actionEngine) executeSelect(ctx context.Context, ref, query string, baselineElement bool) error {
	e.updateActionFidelity(func(r *ActionFidelityReport) { r.Execution = "none" })
	if baselineElement {
		return e.executeSelectSynthetic(ctx, ref, query)
	}
	var obj *runtime.RemoteObject
	err := e.withResolvedActionElement(ctx, ref, func(c context.Context, resolved *runtime.RemoteObject) error {
		// Retain the same resolved realm/object for the multi-stage native operation;
		// the common resolver owns refs, CSS, iframe and shadow routing.
		var exc *runtime.ExceptionDetails
		var err error
		obj, exc, err = runtime.CallFunctionOn(`function(){return this}`).WithObjectID(resolved.ObjectID).Do(c)
		if exc != nil {
			return fmt.Errorf("retain native select target failed")
		}
		if err == nil && (obj == nil || obj.ObjectID == "") {
			return fmt.Errorf("native select target is unavailable")
		}
		return err
	})
	if err != nil {
		return e.failedBeforeDispatch(err)
	}
	defer func() {
		_ = chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error { return runtime.ReleaseObject(obj.ObjectID).Do(c) }))
	}()
	var original, after SelectFacts
	probe := func(out *SelectFacts) error {
		return chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error { return ProbeSelectFacts(c, obj, out) }))
	}
	if err = probe(&original); err != nil {
		return e.failedBeforeDispatch(err)
	}
	redact := (e.snapEngine != nil && e.snapEngine.redactFormValues) || original.Sensitive
	plan, err := PlanSelect(original, query, false)
	if err != nil {
		return e.failedBeforeDispatch(selectPublicError(err, redact))
	}
	evidence := SelectionEvidence{Before: SelectionState{Index: original.SelectedIndex, Value: original.Value}, After: SelectionState{Index: original.SelectedIndex, Value: original.Value}, Note: "native typeahead is non-atomic; intermediate choices may emit trusted input/change; no popup visibility evidence"}
	save := func() {
		copy := publicSelectionEvidence(evidence, redact)
		e.updateActionFidelity(func(r *ActionFidelityReport) { r.Selection = &copy })
	}
	save()
	if plan.NoOp {
		e.markPreDispatchFailure()
		evidence.Verified = true
		save()
		e.updateActionFidelity(func(r *ActionFidelityReport) { r.Execution = "no-op"; r.EvidenceScope = "state-only" })
		return nil
	}
	// Retain the observed option node identities. A same-looking replacement is
	// a changed control, not permission to replay on a new target.
	var optionNodes *runtime.RemoteObject
	err = chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		var exc *runtime.ExceptionDetails
		var err error
		optionNodes, exc, err = runtime.CallFunctionOn(`function(){return Array.from(this.options)}`).WithObjectID(obj.ObjectID).Do(c)
		if exc != nil {
			return fmt.Errorf("retain option identities: %s", exc.Text)
		}
		return err
	}))
	if err != nil {
		return e.failedBeforeDispatch(err)
	}
	defer func() {
		_ = chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error { return runtime.ReleaseObject(optionNodes.ObjectID).Do(c) }))
	}()
	check := func() error {
		if err := probe(&after); err != nil {
			return err
		}
		redact = redact || after.Sensitive
		evidence.After = SelectionState{Index: after.SelectedIndex, Value: after.Value}
		save()
		if !after.Connected || !after.Focused || after.Disabled || after.Hidden || after.Multiple || after.Size > 1 || !after.Complete {
			return fmt.Errorf("select detached, hidden, disabled or focus/control mode changed")
		}
		if !sameSelectOptions(original.Options, after.Options) {
			return fmt.Errorf("select options changed")
		}
		var same bool
		if err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
			res, exc, err := runtime.CallFunctionOn(`function(before){return this.options.length===before.length&&Array.from(this.options).every((o,i)=>o===before[i])}`).WithObjectID(obj.ObjectID).WithArguments([]*runtime.CallArgument{{ObjectID: optionNodes.ObjectID}}).WithReturnByValue(true).Do(c)
			if err != nil {
				return err
			}
			if exc != nil {
				return fmt.Errorf("option identity probe: %s", exc.Text)
			}
			return json.Unmarshal(res.Value, &same)
		})); err != nil {
			return err
		}
		if !same {
			return fmt.Errorf("select option nodes changed")
		}
		if evidence.KeySteps == 0 {
			if after.SelectedIndex != original.SelectedIndex || after.Value != original.Value {
				return fmt.Errorf("selection changed before typeahead")
			}
			// An unchanged disabled/hidden placeholder or selectedIndex=-1 is a valid
			// starting state. Only actual navigation must land on a usable option.
			return nil
		}
		if after.SelectedIndex < 0 || after.SelectedIndex >= len(after.Options) {
			return fmt.Errorf("select has no coherent selected option")
		}
		selected := after.Options[after.SelectedIndex]
		if selected.Hidden || selected.Disabled || selected.Value != after.Value {
			return fmt.Errorf("native navigation touched hidden/disabled or incoherent option")
		}
		return nil
	}
	partial := func(stage string, err error) error {
		evidence.Partial = e.lastActionFidelity().Dispatched
		evidence.Note += "; stopped at " + stage
		save()
		return fmt.Errorf("%w: native select stopped at %s (selection may be partial; no setter fallback): %w", ErrActFailed, stage, selectPublicError(err, redact))
	}
	if err = e.executeSeeToClick(ctx, ref); err != nil {
		e.updateActionFidelity(func(r *ActionFidelityReport) {
			if r.Dispatched {
				r.HumanPath = []string{"mouse_click_attempted"}
			} else {
				r.HumanPath = nil
			}
		})
		return partial("real click", err)
	}
	e.updateActionFidelity(func(r *ActionFidelityReport) { r.HumanPath = []string{"mouse_click"} })
	if err = check(); err != nil {
		return partial("click focus verification", err)
	}
	e.updateActionFidelity(func(r *ActionFidelityReport) {
		r.HumanPath = append(r.HumanPath, "active_element_verified")
		r.Execution = "native-keys"
	})
	if err = chromedp.Run(ctx, chromedp.KeyEvent(kb.Escape)); err != nil {
		return partial("Escape", err)
	}
	e.updateActionFidelity(func(r *ActionFidelityReport) { r.HumanPath = append(r.HumanPath, "Input.dispatchKeyEvent:Escape") })
	if err = check(); err != nil {
		return partial("Escape verification", err)
	}
	timer := time.NewTimer(nativeSelectBufferBoundary)
	select {
	case <-ctx.Done():
		timer.Stop()
		return partial("native typeahead buffer boundary", ctx.Err())
	case <-timer.C:
	}
	e.updateActionFidelity(func(r *ActionFidelityReport) { r.HumanPath = append(r.HumanPath, "native_typeahead_buffer_boundary") })
	if err = check(); err != nil {
		return partial("buffer boundary verification", err)
	}
	seen := map[int]bool{}
	for step := 0; step < nativeSelectKeyLimit; step++ {
		if err = ctx.Err(); err != nil {
			return partial("key deadline", err)
		}
		evidence.KeySteps++
		save()
		if err = chromedp.Run(ctx, chromedp.KeyEvent(plan.Character)); err != nil {
			return partial("typeahead key", err)
		}
		e.updateActionFidelity(func(r *ActionFidelityReport) { r.HumanPath = append(r.HumanPath, "Input.dispatchKeyEvent:typeahead") })
		if err = check(); err != nil {
			return partial("typeahead verification", err)
		}
		if after.SelectedIndex == plan.Index && after.Value == original.Options[plan.Index].Value {
			evidence.Verified = true
			save()
			// Preserve the existing real-focus proof for later targeted native press.
			_ = chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
				node, err := dom.DescribeNode().WithObjectID(obj.ObjectID).Do(c)
				if err == nil && node != nil {
					e.recordHumanFocus(int64(node.BackendNodeID))
				}
				return err
			}))
			e.updateActionFidelity(func(r *ActionFidelityReport) {
				r.EvidenceScope = "keyboard-selection"
				r.HumanPath = append(r.HumanPath, "same_control_options_focus_value_verified")
			})
			return nil
		}
		if seen[after.SelectedIndex] {
			return partial("native cycle", fmt.Errorf("target was not reached before selection repeated"))
		}
		seen[after.SelectedIndex] = true
	}
	return partial("key bound", fmt.Errorf("target not reached within %d native keys", nativeSelectKeyLimit))
}

func sameSelectOptions(a, b []SelectOption) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		x.Selected = false
		y.Selected = false
		if x != y {
			return false
		}
	}
	return true
}

func (e *actionEngine) executeSelectSynthetic(ctx context.Context, ref, query string) error {
	return e.withResolvedActionElement(ctx, ref, func(c context.Context, obj *runtime.RemoteObject) error {
		var facts SelectFacts
		if err := ProbeSelectFacts(c, obj, &facts); err != nil {
			return e.failedBeforeDispatch(err)
		}
		redact := (e.snapEngine != nil && e.snapEngine.redactFormValues) || facts.Sensitive
		plan, err := PlanSelect(facts, query, true)
		if err != nil {
			return e.failedBeforeDispatch(selectPublicError(err, redact))
		}
		evidence := SelectionEvidence{Before: SelectionState{Index: facts.SelectedIndex, Value: facts.Value}, After: SelectionState{Index: facts.SelectedIndex, Value: facts.Value}, Note: "functional state only; not evidence of human picker operation"}
		if plan.NoOp {
			e.markPreDispatchFailure()
			evidence.Verified = true
			e.updateActionFidelity(func(r *ActionFidelityReport) {
				r.Execution = "no-op"
				r.EvidenceScope = "state-only"
				copy := publicSelectionEvidence(evidence, redact)
				r.Selection = &copy
			})
			return nil
		}
		e.updateActionFidelity(func(r *ActionFidelityReport) {
			r.Execution = "synthetic"
			r.Synthetic = true
			r.SyntheticNote = evidence.Note
			copy := publicSelectionEvidence(evidence, redact)
			r.Selection = &copy
		})
		var applied nativeSelectApplyResult
		if err = applyNativeSelectIndex(c, obj, plan.Index, &applied); err != nil {
			evidence.Partial = true
			copy := publicSelectionEvidence(evidence, redact)
			e.updateActionFidelity(func(r *ActionFidelityReport) { r.Selection = &copy })
			return err
		}
		evidence.After = SelectionState{Index: applied.SelectedIndex, Value: applied.Value}
		evidence.Verified = applied.Applied && applied.SelectedIndex == plan.Index && applied.Value == facts.Options[plan.Index].Value
		evidence.Partial = !evidence.Verified
		copy := publicSelectionEvidence(evidence, redact)
		e.updateActionFidelity(func(r *ActionFidelityReport) {
			r.Selection = &copy
			if evidence.Verified {
				r.EvidenceScope = "functional-state"
			}
		})
		if !evidence.Verified {
			return fmt.Errorf("%w: synthetic select final value/index mismatch", ErrActFailed)
		}
		return nil
	})
}

type nativeSelectApplyResult struct {
	Applied       bool   `json:"applied"`
	SelectedIndex int    `json:"selected_index"`
	Label         string `json:"label"`
	Value         string `json:"value"`
}

func applyNativeSelectIndex(ctx context.Context, obj *runtime.RemoteObject, index int, out *nativeSelectApplyResult) error {
	fn := `function() {
		const index = ` + strconv.Itoa(index) + `;
		if (!this || String(this.tagName || '').toUpperCase() !== 'SELECT' || index < 0 || index >= this.options.length) {
			return {applied:false, selected_index:-1, label:'', value:''};
		}
		this.selectedIndex = index;
		this.dispatchEvent(new Event('input', {bubbles:true}));
		this.dispatchEvent(new Event('change', {bubbles:true}));
		const selected = this.options[this.selectedIndex];
		return {
			applied:true,
			selected_index:this.selectedIndex,
			label:String(selected ? (selected.label || selected.textContent || '') : '').trim(),
			value:String(this.value ?? '')
		};
	}`
	res, exc, err := runtime.CallFunctionOn(fn).
		WithObjectID(obj.ObjectID).
		WithReturnByValue(true).
		Do(ctx)
	if err != nil {
		return err
	}
	if exc != nil {
		return fmt.Errorf("native select apply failed: %s", exc.Text)
	}
	if res == nil || len(res.Value) == 0 {
		return fmt.Errorf("native select apply returned no value")
	}
	return json.Unmarshal([]byte(res.Value), out)
}

func selectPublicError(err error, redact bool) error {
	if err == nil || !redact {
		return err
	}
	return &redactedSelectError{cause: err}
}
func publicSelectionEvidence(e SelectionEvidence, redact bool) SelectionEvidence {
	if redact {
		e.Before.Value = ""
		e.Before.ValueRedacted = true
		e.After.Value = ""
		e.After.ValueRedacted = true
	}
	return e
}

type redactedSelectError struct{ cause error }

func (e *redactedSelectError) Error() string {
	return "select rejected or interrupted (option diagnostics redacted)"
}
func (e *redactedSelectError) Unwrap() error { return e.cause }
