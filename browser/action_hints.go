package browser

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/go-json-experiment/json/jsontext"
)

// AllowedActionsForElement projects the canonical vocabulary through actual
// captured control state. Hints never create execution authority.
func AllowedActionsForElement(ref ElementRef, scenario Scenario) []string {
	if !ref.Observed || !ref.VisibilityKnown || !ref.VisibleInViewport || ref.BlockedByModal {
		return nil
	}
	if _, err := NormalizeScenario(string(scenario)); err != nil {
		return nil
	}
	var result []string
	for _, definition := range ActionDefinitions() {
		refTarget := false
		for _, kind := range definition.TargetKinds {
			if kind == "ref" {
				refTarget = true
			}
		}
		if !refTarget {
			continue
		}
		allow := false
		switch definition.Kind {
		case "hover", "scrollto":
			allow = true
		case "click", "tap", "drag", "swipe":
			allow = ref.ControlStateKnown && !ref.Disabled && ref.Interactable
		case "focus":
			allow = ref.ControlStateKnown && !ref.Disabled && ref.Focusable
		case "press":
			allow = ref.ControlStateKnown && !ref.Disabled && ref.HumanFocusVerified
		case "fill", "type":
			allow = ref.ControlStateKnown && ref.Editable && ref.SelectionSupported && !ref.Disabled && !ref.ReadOnly && !ref.Sensitive
		case "set-checked":
			allow = ref.ControlStateKnown && ref.Checkable && !ref.Disabled
		case "select":
			capability := SelectCapabilityForElement(ref, scenario)
			allow = capability != nil && capability.Available
		}
		if allow {
			result = append(result, definition.Kind)
		}
	}
	return result
}

// SelectCapabilityForElement describes the default visual select path from captured facts.
// A missing probe never grants native select authority (including Safari).
func SelectCapabilityForElement(ref ElementRef, scenario Scenario) *SelectCapability {
	if !ref.NativeSelect {
		return nil
	}
	capability := SelectCapability{Reason: "native select facts unavailable", Options: []SelectOptionCapability{}}
	if _, err := NormalizeScenario(string(scenario)); err != nil {
		capability.Reason = "interaction scenario unavailable"
		return &capability
	}
	if ref.SelectFacts == nil {
		return &capability
	}
	capability = PlanSelectCapability(*ref.SelectFacts, false)
	if !ref.Observed || !ref.VisibilityKnown || !ref.VisibleInViewport || ref.BlockedByModal || !ref.ControlStateKnown || ref.Disabled || !ref.Interactable {
		capability.Available = false
		capability.Reason = "target is not an observed enabled visible interactable control"
	}
	if ref.SelectValuesRedacted || ref.Sensitive {
		capability.Options = []SelectOptionCapability{}
		capability.OptionsComplete = false
		if capability.Reason != "" {
			capability.Reason += "; "
		}
		capability.Reason += "choices withheld for privacy; use an explicitly known label or value"
	}
	return &capability
}

var controlStateFunction = `function(redactValues){
 const e=this,tag=String(e.tagName||'').toUpperCase(),type=String(e.type||'').toLowerCase();
 const ac=String(e.autocomplete||'').toLowerCase(),name=String(e.name||'').toLowerCase();
 const sensitive=(` + intrinsicSensitiveControlJS + `)(e);
 return {value:(!sensitive&&!redactValues)?String(e.value??''):'',known:true,editable:tag==='INPUT'||tag==='TEXTAREA',readonly:!!e.readOnly,
 disabled:!!e.disabled||e.matches(':disabled')||e.getAttribute('aria-disabled')==='true',
 sensitive,
 checkable:(tag==='INPUT'&&type==='checkbox')||['checkbox','switch'].includes(e.getAttribute('role')),
 native_select:tag==='SELECT',focusable:e.matches('input,textarea,select,button,a[href],[tabindex]'),
 selection_supported:Number.isInteger(e.selectionStart)&&Number.isInteger(e.selectionEnd),focused:e.getRootNode().activeElement===e};
}`

func (e *snapshotEngine) captureControlStates(ctx context.Context, refs []ElementRef) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		for i := range refs {
			refs[i].SelectFacts = nil
			refs[i].SelectValuesRedacted = e.redactFormValues
			object, err := resolveElementObjectForMeta(c, &refs[i])
			if err != nil {
				continue
			}
			if refs[i].BackendNodeID == 0 {
				node, err := dom.DescribeNode().WithObjectID(object.ObjectID).Do(c)
				if err == nil && node != nil {
					refs[i].BackendNodeID = int64(node.BackendNodeID)
				}
			}
			result, exception, err := runtime.CallFunctionOn(controlStateFunction).WithObjectID(object.ObjectID).WithArguments([]*runtime.CallArgument{{Value: jsontext.Value(fmt.Sprintf("%t", e.redactFormValues))}}).WithReturnByValue(true).Do(c)
			var selectFacts *SelectFacts
			if err == nil && exception == nil && result != nil {
				var control struct {
					NativeSelect bool `json:"native_select"`
					Sensitive    bool `json:"sensitive"`
				}
				if json.Unmarshal(result.Value, &control) == nil && control.NativeSelect {
					facts := new(SelectFacts)
					if ProbeSelectFacts(c, object, facts) == nil {
						selectFacts = facts
					}
				}
			}
			_ = runtime.ReleaseObject(object.ObjectID).Do(c)
			if err != nil || exception != nil || result == nil {
				continue
			}
			var state struct {
				Value                                                                         string
				Known, Editable, Readonly, Disabled, Sensitive, Checkable, Focusable, Focused bool
				NativeSelect                                                                  bool `json:"native_select"`
				SelectionSupported                                                            bool `json:"selection_supported"`
			}
			if json.Unmarshal(result.Value, &state) != nil {
				continue
			}
			ref := &refs[i]
			ref.SelectFacts = selectFacts
			ref.SelectValuesRedacted = e.redactFormValues || state.Sensitive
			ref.Value = state.Value
			ref.ControlStateKnown = state.Known
			ref.Editable = state.Editable
			ref.ReadOnly = state.Readonly
			ref.Disabled = state.Disabled
			ref.Sensitive = state.Sensitive
			ref.Checkable = state.Checkable
			ref.NativeSelect = state.NativeSelect
			ref.Focusable = state.Focusable
			ref.SelectionSupported = state.SelectionSupported
			ref.HumanFocusVerified = state.Focused && ref.BackendNodeID != 0 && ref.BackendNodeID == e.humanFocusBackendID.Load()
		}
		return nil
	}))
}
