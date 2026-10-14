package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
	"gopkg.in/yaml.v3"
	"math"
	"net/url"
	"strings"
)

// Action is the canonical request shared by CLI, journey and embedded adapters.
// Text is never tokenized, trimmed, or included in an outcome.
type Action struct {
	Verb            string  `json:"kind" yaml:"kind"`
	Target          Target  `json:"target,omitempty" yaml:"target,omitempty"`
	Text            string  `json:"text,omitempty" yaml:"text,omitempty"`
	Sensitive       bool    `json:"sensitive,omitempty" yaml:"sensitive,omitempty"`
	Focused         bool    `json:"focused,omitempty" yaml:"focused,omitempty"`
	Checked         *bool   `json:"checked,omitempty" yaml:"checked,omitempty"`
	URL             string  `json:"url,omitempty" yaml:"url,omitempty"`
	Key             string  `json:"key,omitempty" yaml:"key,omitempty"`
	Value           string  `json:"value,omitempty" yaml:"value,omitempty"`
	Button          string  `json:"button,omitempty" yaml:"button,omitempty"`
	Count           int     `json:"count,omitempty" yaml:"count,omitempty"`
	Direction       string  `json:"direction,omitempty" yaml:"direction,omitempty"`
	Steps           int     `json:"steps,omitempty" yaml:"steps,omitempty"`
	DeltaX          float64 `json:"delta_x,omitempty" yaml:"delta_x,omitempty"`
	DeltaY          float64 `json:"delta_y,omitempty" yaml:"delta_y,omitempty"`
	End             *Point  `json:"end,omitempty" yaml:"end,omitempty"`
	wireTextMissing bool
	wireFields      []string
}
type Target struct {
	Kind    string `json:"kind,omitempty" yaml:"kind,omitempty"`
	Ref     string `json:"ref,omitempty" yaml:"ref,omitempty"`
	Locator string `json:"locator,omitempty" yaml:"locator,omitempty"`
	Point   *Point `json:"point,omitempty" yaml:"point,omitempty"`
}
type Point struct {
	ObservationID string  `json:"observation_id,omitempty" yaml:"observation_id,omitempty"`
	X             float64 `json:"x" yaml:"x"`
	Y             float64 `json:"y" yaml:"y"`
	Unit          string  `json:"unit" yaml:"unit"`
	Frame         string  `json:"frame" yaml:"frame"`
}
type Outcome struct {
	Phase         string `json:"phase"`
	DispatchState string `json:"dispatch_state"`
	Recovery      string `json:"recovery,omitempty"`
}
type ActionStageError struct {
	Phase, State string
	Cause        error
}

func (e *ActionStageError) Error() string { return e.Cause.Error() }
func (e *ActionStageError) Unwrap() error { return e.Cause }

func (a Action) Validate() error { _, err := a.executionRequest(); return err }
func (a Action) executionRequest() (*actionRequest, error) {
	fail := func(s string) (*actionRequest, error) { return nil, fmt.Errorf("%w: %s", ErrActFailed, s) }

	if (a.Verb == "fill" || a.Verb == "type") && a.wireTextMissing {
		return fail("text is required; explicit empty text is allowed")
	}
	definition, known := LookupActionDefinition(a.Verb)
	if !known {
		return fail("unknown action verb; use structured kind, target and text fields")
	}
	allowedFields := map[string]bool{}
	for _, field := range definition.Fields {
		allowedFields[field] = true
	}
	present := map[string]bool{"target": a.Target.Kind != "" || a.Target.Point != nil || a.Target.Ref != "" || a.Target.Locator != "", "text": a.Text != "", "sensitive": a.Sensitive, "focused": a.Focused, "checked": a.Checked != nil, "url": a.URL != "", "key": a.Key != "", "value": a.Value != "", "button": a.Button != "", "count": a.Count != 0, "direction": a.Direction != "", "steps": a.Steps != 0, "delta_x": a.DeltaX != 0, "delta_y": a.DeltaY != 0, "end": a.End != nil}
	for _, field := range a.wireFields {
		if field != "kind" && !allowedFields[field] {
			return fail("field " + field + " is unavailable for this action kind")
		}
	}
	for field, set := range present {
		if set && !allowedFields[field] {
			return fail("field " + field + " is unavailable for this action kind")
		}
	}
	if a.Target.Kind != "" {
		kindAllowed := false
		for _, kind := range definition.TargetKinds {
			if kind == a.Target.Kind {
				kindAllowed = true
			}
		}
		if !kindAllowed {
			return fail("target kind is unavailable for this action")
		}
	}
	if a.Target.Point != nil {
		switch a.Verb {
		case "click", "tap", "hover", "scroll", "drag", "swipe":
		default:
			return fail("point is unavailable for this action kind")
		}
	}
	if a.Steps < 0 || a.Steps > 100 {
		return fail("scroll steps must be 0 (default) or 1..100")
	}
	if math.IsNaN(a.DeltaX) || math.IsNaN(a.DeltaY) || math.IsInf(a.DeltaX, 0) || math.IsInf(a.DeltaY, 0) {
		return fail("finite scroll delta required")
	}
	p := &actionRequest{Op: a.Verb, Value: a.Text, Direction: a.Direction, Steps: a.Steps, DeltaX: a.DeltaX, DeltaY: a.DeltaY}
	switch a.Target.Kind {
	case "":
		if a.Target.Ref != "" || a.Target.Locator != "" || a.Target.Point != nil {
			return fail("target kind required")
		}
	case "ref":
		if !strings.HasPrefix(a.Target.Ref, "@o") || a.Target.Locator != "" {
			return fail("ref target requires observation token")
		}
		p.Ref = a.Target.Ref
	case "locator":
		if a.Target.Locator == "" || a.Target.Ref != "" {
			return fail("locator required")
		}
		if !strings.HasPrefix(a.Target.Locator, "css=") && !strings.HasPrefix(a.Target.Locator, "role=") {
			return fail("locator requires explicit css= or canonical role= syntax")
		}
		p.Ref = a.Target.Locator
	case "point":
		if a.Target.Point == nil || a.Target.Ref != "" || a.Target.Locator != "" {
			return fail("point target required")
		}
	default:
		return fail("unknown target kind")
	}
	if a.Target.Point != nil {
		q := a.Target.Point
		if math.IsNaN(q.X) || math.IsNaN(q.Y) || math.IsInf(q.X, 0) || math.IsInf(q.Y, 0) {
			return fail("finite coordinates required")
		}
		if q.Frame == "viewport" && q.Unit == "css-px" && a.Target.Kind == "point" {
		} else if q.Frame == "target" && q.Unit == "percent" && p.Ref != "" && q.X >= 0 && q.X <= 100 && q.Y >= 0 && q.Y <= 100 {
		} else {
			return fail("coordinates require viewport css-px or target percent")
		}
		p.CoordX, p.CoordY = q.X, q.Y
		if q.Unit == "percent" {
			p.CoordX /= 100
			p.CoordY /= 100
		}
	}

	captureID := ""
	if a.Target.Kind == "ref" {
		var err error
		captureID, err = observationIDFromRef(a.Target.Ref)
		if err != nil {
			return nil, err
		}
		p.ObservationID = captureID
	}
	if a.Target.Point != nil {
		requested := a.Target.Point.ObservationID
		if captureID != "" {
			if requested != "" && requested != captureID {
				return fail("point observation_id does not match ref")
			}
			requested = captureID
		}
		if requested == "" || strings.TrimSpace(requested) != requested {
			return fail("point requires observation_id from the current capture")
		}
		p.ObservationID = requested
	}
	point := a.Target.Point != nil
	switch a.Verb {
	case "click":
		if a.Count == 2 && a.Button == "right" {
			return fail("right click supports count 1")
		}
		if a.Count != 0 && a.Count != 1 && a.Count != 2 {
			return fail("click count must be 1 or 2")
		}
		if a.Button != "" && a.Button != "left" && a.Button != "right" {
			return fail("click button must be left or right")
		}
		if a.Count == 2 || a.Button == "right" {
			if !point {
				if p.Ref == "" {
					return fail("click requires target")
				}
				p.CoordX, p.CoordY = 0.5, 0.5
			}
			if a.Button == "right" {
				p.Op = "rclickat"
			} else {
				p.Op = "dblclickat"
				if point && p.Ref == "" {
					p.Op = "dblclickxy"
				}
			}
		} else if point {
			if p.Ref == "" {
				p.Op = "clickxy"
			} else {
				p.Op = "clickat"
			}
		}
	case "tap":
		if point {
			p.Op = "tapat"
		}
	case "hover":
		if point {
			p.Op = "hoverat"
		}
	case "scroll":
		if point {
			if a.Direction != "" {
				if a.DeltaX != 0 || a.DeltaY != 0 {
					return fail("scroll direction and delta are mutually exclusive")
				}
				switch a.Direction {
				case "down":
					p.DeltaY = humanWheelStepCSSPixels
				case "up":
					p.DeltaY = -humanWheelStepCSSPixels
				case "left":
					p.DeltaX = -humanWheelStepCSSPixels
				case "right":
					p.DeltaX = humanWheelStepCSSPixels
				default:
					return fail("invalid scroll direction")
				}
			}
			if p.DeltaX == 0 && p.DeltaY == 0 {
				return fail("coordinate scroll requires direction or nonzero delta")
			}
			p.Op = "wheelat"
		} else {
			p.ScrollTarget = p.Ref
			p.Ref = ""
			if a.Direction != "up" && a.Direction != "down" && a.Direction != "left" && a.Direction != "right" {
				return fail("scroll direction required")
			}
		}
	case "drag", "swipe":
		if !point || a.End == nil {
			return fail("gesture requires start and end")
		}
		if a.End.Unit != a.Target.Point.Unit || a.End.Frame != a.Target.Point.Frame {
			return fail("gesture coordinate frames must match")
		}
		if math.IsNaN(a.End.X) || math.IsNaN(a.End.Y) || math.IsInf(a.End.X, 0) || math.IsInf(a.End.Y, 0) || a.End.X < 0 || a.End.Y < 0 {
			return fail("finite nonnegative gesture endpoint required")
		}
		if a.End.Unit == "percent" && (a.End.X > 100 || a.End.Y > 100) {
			return fail("gesture endpoint percentage out of range")
		}

		endpointID := a.End.ObservationID
		if endpointID == "" && captureID != "" {
			endpointID = captureID
		}
		if endpointID == "" || endpointID != p.ObservationID {
			return fail("gesture endpoint requires the same observation_id")
		}
		p.CoordX2, p.CoordY2 = a.End.X, a.End.Y
		if a.End.Unit == "percent" {
			p.CoordX2 /= 100
			p.CoordY2 /= 100
		}
		p.Op = a.Verb + "at"
		if a.Verb == "drag" && p.Ref == "" {
			p.Op = "dragxy"
		}
	case "fill":
		if a.Sensitive {
			p.Op = "fillsecret"
		}
	case "type":
		if a.Focused {
			if p.Ref != "" || point {
				return fail("focused type cannot have a target")
			}
			p.Op = "typetext"
		}
	case "set-checked":
		if a.Checked == nil {
			return fail("checked boolean required")
		}
		if *a.Checked {
			p.Op = "check"
		} else {
			p.Op = "uncheck"
		}
	case "scrollto":
		p.Op = "scrollinto"
	case "press":
		if a.Key == "" {
			return fail("key required")
		}
		if err := validatePressKeySyntax(a.Key); err != nil {
			return nil, err
		}
		p.Value = a.Key
	case "select":
		p.Value = a.Value
	case "zoom", "keyboard":
		if a.Verb == "zoom" {
			if _, err := parseZoomFactor(a.Value); err != nil {
				return nil, err
			}
		} else if a.Value != "show" && a.Value != "hide" {
			return fail("keyboard requires show or hide")
		}
		p.Value = a.Value
		if a.Value == "" {
			return fail("value required")
		}
	case "navigate":
		if err := ValidateNavigationURL(a.URL); err != nil {
			return nil, err
		}
		p.Value = a.URL
	case "reload", "back", "forward", "focus":
	default:
		return fail("unknown action verb; use structured kind, target and text fields")
	}
	switch p.Op {
	case "click", "tap", "hover", "fill", "fillsecret", "type", "check", "uncheck", "scrollinto", "focus", "select":
		if p.Ref == "" {
			return fail("action requires ref or locator target")
		}
	}
	return p, nil
}

// ExecuteAction enters the same validation, input window and dispatch path as
// legacy callers; it never serializes an action or invokes the string parser.
func (impl *browserCoreImpl) ExecuteAction(ctx context.Context, a Action, observe, sessionMode bool, mode InteractionMode) (*Snapshot, Outcome, error) {
	impl.actEngine.beginActionFidelity()
	impl.actEngine.markPreDispatchFailure()

	impl.actEngine.fidelityMu.Lock()
	operationID := impl.actEngine.operationID
	impl.actEngine.operationID = ""
	impl.actEngine.fidelityMu.Unlock()
	out := Outcome{Phase: "validation", DispatchState: "not-dispatched"}
	p, err := a.executionRequest()
	if err != nil {
		return nil, out, err
	}
	impl.mu.RLock()
	interactionPolicy := impl.actEngine.interactionPolicy
	impl.mu.RUnlock()
	if err := interactionPolicy.Validate(); err != nil {
		return nil, out, err
	}
	if impl.takeoverCtrl.IsTakeover() {
		return nil, out, ErrTakeoverActive
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if a.Sensitive {
		impl.snapEngine.redactFormValues = true
	}
	if err := impl.checkActionPolicy(ctx, a); err != nil {
		return nil, out, err
	}
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	runCtx, cancel := deriveTargetContext(ctx, impl.currentCtx())
	runCtx = context.WithValue(runCtx, actionOperationIDKey{}, operationID)
	defer cancel()
	snap, err := runActionWithTimeout(runCtx, actionExecutionTimeout, a.Verb, func(c context.Context) (*Snapshot, error) {
		return impl.actEngine.executeRequestWithInteractionMode(c, p, observe, sessionMode, mode)
	})
	if err == nil {
		return snap, Outcome{Phase: "complete", DispatchState: "confirmed"}, nil
	}
	out = Outcome{Phase: "dispatch", DispatchState: "unknown", Recovery: "observe before any further input; do not replay"}
	if !impl.actEngine.lastActionFidelity().Dispatched {
		out = Outcome{Phase: "validation", DispatchState: "not-dispatched"}
	}
	var stage *ActionStageError
	if errors.As(err, &stage) {
		out.Phase = stage.Phase
		out.DispatchState = stage.State
	}
	return snap, out, err
}

// SetSessionAuthoritySource rechecks persisted control ownership after the input
// lock is acquired, before target activation or dispatch.

func (impl *browserCoreImpl) SetSessionOperationID(id string) {
	impl.actEngine.fidelityMu.Lock()
	impl.actEngine.operationID = id
	impl.actEngine.fidelityMu.Unlock()
}
func (impl *browserCoreImpl) SetSessionAuthoritySource(sessionID string) {
	impl.SetSessionAuthorityProvider(func(context.Context) (*SessionInfo, error) { return LoadSession(sessionID) })
}

// SetSessionAuthorityProvider binds host-owned state to the same locked input
// guard as CLI persistence. The provider must return current control/fence facts
// and respect ctx; it does not transfer runtime or storage ownership.
func (impl *browserCoreImpl) SetSessionAuthorityProvider(provider func(context.Context) (*SessionInfo, error)) {
	impl.snapEngine.ownerSource = provider
	if provider == nil {
		impl.actEngine.authorityCheck = nil
		return
	}
	impl.actEngine.authorityCheck = func(ctx context.Context, needsGeometry bool, captureID string) error {
		info, err := provider(ctx)
		if err != nil {
			return err
		}
		if err := SessionAllowsAIInput(info); err != nil {
			return err
		}
		operationID, _ := ctx.Value(actionOperationIDKey{}).(string)
		if info.LastActionOutcome == SessionActionOutcomeUnknown || (info.LastActionOutcome == SessionActionOutcomeInProgress && (operationID == "" || info.LastActionOperationID != operationID)) {
			return fmt.Errorf("previous action delivery unknown; observe before further input")
		}

		if needsGeometry {
			if captureID != "" && captureID != info.ObservationID {
				return fmt.Errorf("stale observation_id; observe and use its current capture identity")
			}
			if err := ValidateSessionObservation(info); err != nil {
				return err
			}
			identity, err := impl.sessionDocumentIdentityOnTarget(ctx)
			if err != nil {
				return err
			}
			if info.ObservationDocumentID == 0 || identity != info.ObservationDocumentID {
				return fmt.Errorf("stale observation: document changed; observe before input")
			}
			var url string
			if err := chromedp.Run(ctx, chromedp.Evaluate("window.location.href", &url)); err != nil {
				return err
			}
			if url == "" || url != info.ObservationPageURL {
				return fmt.Errorf("stale observation: page URL changed; observe before input")
			}
			var viewport ViewportFacts
			if err := chromedp.Run(ctx, chromedp.Evaluate(viewportFactsJS, &viewport)); err != nil {
				return err
			}
			if viewport.ScrollX != info.ObservationScrollX || viewport.ScrollY != info.ObservationScrollY || viewport.DevicePixelRatio != info.ObservationDPR {
				return fmt.Errorf("stale observation: scroll or pixel scale changed; observe before input")
			}

		}
		return nil
	}
}

func (impl *browserCoreImpl) sessionDocumentIdentityOnTarget(ctx context.Context) (int64, error) {
	var id int64
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		root, err := dom.GetDocument().Do(c)
		if err != nil {
			return err
		}
		if root == nil {
			return fmt.Errorf("document identity unavailable")
		}
		id = int64(root.BackendNodeID)
		return nil
	}))
	return id, err
}
func (impl *browserCoreImpl) SessionDocumentIdentity(ctx context.Context) (int64, error) {
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	run, cancel := deriveTargetContext(ctx, impl.currentCtx())
	defer cancel()
	return impl.sessionDocumentIdentityOnTarget(run)
}

// ValidateNavigationURL permits document navigation, never in-document script
// evaluation. Data documents support the existing local HTML calibration pages.
func ValidateNavigationURL(raw string) error {
	fail := func() error {
		return fmt.Errorf("%w: navigation requires http, https, local file, about:blank or a data HTML/text document", ErrActFailed)
	}
	if raw == "" || strings.TrimSpace(raw) != raw {
		return fail()
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fail()
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" {
			return fail()
		}
		return nil
	case "file":
		if (u.Host != "" && u.Host != "localhost") || u.Path == "" || !strings.HasPrefix(u.Path, "/") {
			return fail()
		}
		return nil
	case "about":
		if u.Opaque != "blank" {
			return fail()
		}
		return nil
	case "data":
		media, _, ok := strings.Cut(u.Opaque, ",")
		if !ok {
			return fail()
		}
		mime := strings.ToLower(strings.Split(media, ";")[0])
		switch mime {
		case "text/html", "text/plain", "application/xhtml+xml":
			return nil
		default:
			return fail()
		}
	default:
		return fail()
	}
}

// WithText binds text from an explicit field or authorized text_file without
// losing its exact bytes, including an empty value or trailing newline.
func (a Action) WithText(text string) Action { a.Text = text; a.wireTextMissing = false; return a }
func (a Action) TextWasProvided() bool       { return !a.wireTextMissing }

type actionWire Action

func (a Action) MarshalJSON() ([]byte, error) {
	wire := actionWire(a)
	var text *string
	if a.Verb == "fill" || a.Verb == "type" {
		if !a.wireTextMissing {
			text = &a.Text
		}
	} else if a.Text != "" {
		text = &a.Text
	}
	var target *Target
	if a.Target.Kind != "" || a.Target.Ref != "" || a.Target.Locator != "" || a.Target.Point != nil {
		target = &a.Target
	}
	var value *string
	if a.Verb == "select" || a.Value != "" {
		value = &a.Value
	}
	return json.Marshal(struct {
		*actionWire
		Target *Target `json:"target,omitempty"`
		Text   *string `json:"text,omitempty"`
		Value  *string `json:"value,omitempty"`
	}{&wire, target, text, value})
}
func (a *Action) UnmarshalJSON(data []byte) error {
	var wire actionWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*a = Action(wire)
	for field := range fields {
		a.wireFields = append(a.wireFields, field)
	}
	value, present := fields["text"]
	a.wireTextMissing = !present
	if present && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return fmt.Errorf("text must be a string, not null")
	}
	return nil
}
func (a Action) MarshalYAML() (interface{}, error) {
	data, err := a.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var fields map[string]interface{}
	err = json.Unmarshal(data, &fields)
	return fields, err
}
func (a *Action) UnmarshalYAML(node *yaml.Node) error {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	if err := encoder.Encode(node); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	decoder := yaml.NewDecoder(&buffer)
	decoder.KnownFields(true)
	var wire actionWire
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	*a = Action(wire)
	a.wireTextMissing = true
	for i := 0; i+1 < len(node.Content); i += 2 {
		a.wireFields = append(a.wireFields, node.Content[i].Value)
		if node.Content[i].Value == "text" {
			if node.Content[i+1].Tag != "!!str" {
				return fmt.Errorf("text must be a string")
			}
			a.wireTextMissing = false
		}
	}
	return nil
}

// WithoutText authors a deferred text_file binding. It cannot be executed until
// WithText supplies that binding; it never represents an implicit clear.
func (a Action) WithoutText() Action { a.Text = ""; a.wireTextMissing = true; return a }

type pointWire Point

func (p *Point) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, field := range []string{"x", "y", "unit", "frame"} {
		value, present := fields[field]
		if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("point requires explicit %s", field)
		}
	}
	var wire pointWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	*p = Point(wire)
	return nil
}
func (p *Point) UnmarshalYAML(node *yaml.Node) error {
	fields := map[string]*yaml.Node{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		fields[node.Content[i].Value] = node.Content[i+1]
	}
	for _, field := range []string{"x", "y", "unit", "frame"} {
		value, present := fields[field]
		if !present || value.Tag == "!!null" {
			return fmt.Errorf("point requires explicit %s", field)
		}
	}
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	if err := encoder.Encode(node); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	decoder := yaml.NewDecoder(&buffer)
	decoder.KnownFields(true)
	var wire pointWire
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	*p = Point(wire)
	return nil
}

func observationIDFromRef(ref string) (string, error) {
	if !strings.HasPrefix(ref, "@o") {
		return "", fmt.Errorf("%w: observation capability ref required", ErrRefNotFound)
	}
	index := strings.LastIndex(ref, "-r")
	if index <= 2 || index+2 >= len(ref) {
		return "", fmt.Errorf("%w: malformed observation ref", ErrRefNotFound)
	}
	for _, digit := range ref[index+2:] {
		if digit < '0' || digit > '9' {
			return "", fmt.Errorf("%w: malformed observation ref", ErrRefNotFound)
		}
	}
	return ref[2:index], nil
}

// ValidateTemplate validates persistent shape only. Runtime execution still
// requires captured identities, and callers must reject stored runtime IDs.
func (a Action) ValidateTemplate() error {
	id := "template"
	if a.Target.Kind == "ref" {
		var err error
		id, err = observationIDFromRef(a.Target.Ref)
		if err != nil {
			return err
		}
	}
	if a.Target.Point != nil {
		point := *a.Target.Point
		if point.ObservationID == "" {
			point.ObservationID = id
		}
		a.Target.Point = &point
		id = point.ObservationID
	}
	if a.End != nil {
		end := *a.End
		if end.ObservationID == "" {
			end.ObservationID = id
		}
		a.End = &end
	}
	return a.Validate()
}

type actionOperationIDKey struct{}
