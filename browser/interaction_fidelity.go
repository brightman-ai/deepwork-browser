package browser

import (
	"context"
	"encoding/json"
)

const (
	AimSourceContentQuadCentroid = "content-quad-centroid"
	AimSourceBBoxCenter          = "bbox-center-fallback"
)

// SelectionState omits values under the existing form/control privacy policy.
type SelectionState struct {
	Index         int    `json:"index"`
	Value         string `json:"value"`
	ValueRedacted bool   `json:"value_redacted,omitempty"`
}

// MarshalJSON distinguishes a verified empty value from withheld private data.
func (s SelectionState) MarshalJSON() ([]byte, error) {
	if s.ValueRedacted {
		return json.Marshal(struct {
			Index         int  `json:"index"`
			ValueRedacted bool `json:"value_redacted"`
		}{s.Index, true})
	}
	return json.Marshal(struct {
		Index int    `json:"index"`
		Value string `json:"value"`
	}{s.Index, s.Value})
}

// SelectionEvidence describes the final observed state and non-atomic native
// attempts. KeySteps counts selecting key dispatch attempts, excluding Escape.
type SelectionEvidence struct {
	Before   SelectionState `json:"before"`
	After    SelectionState `json:"after"`
	Verified bool           `json:"verified"`
	KeySteps int            `json:"key_steps"`
	Partial  bool           `json:"partial,omitempty"`
	Note     string         `json:"note,omitempty"`
}

// ActionFidelityReport keeps requested policy separate from actual execution.
// It is exposed through ActionFidelityCapable without widening BrowserCore.
type ActionFidelityReport struct {
	InteractionMode InteractionMode    `json:"interaction_mode,omitempty"`
	Execution       string             `json:"execution,omitempty"`
	EvidenceScope   string             `json:"evidence_scope,omitempty"`
	Selection       *SelectionEvidence `json:"selection,omitempty"`

	Fidelity      InteractionFidelity `json:"requested_fidelity,omitempty"`
	Synthetic     bool                `json:"synthetic,omitempty"`
	SyntheticNote string              `json:"synthetic_note,omitempty"`
	HumanPath     []string            `json:"human_path,omitempty"`
	AimSource     string              `json:"aim_source,omitempty"`
	HitCoverage   string              `json:"hit_coverage,omitempty"`
	// Hit is the element the browser's own hit test resolves at the dispatch
	// point of a coordinate action. It is reported, never enforced: a human can
	// legitimately click a backdrop, so the tool must not decide intent — it
	// must only tell the caller who actually received the input.
	Hit *CoordinateHit `json:"hit,omitempty"`
	// BroughtToFront is true when this action had to raise its own page target
	// to the browser front before dispatching input. It is evidence, not noise:
	// a background target silently swallows pointer input, so "we had to switch"
	// is exactly what distinguishes a healthy dispatch from the class of hangs
	// that used to look like a mysterious timeout.
	BroughtToFront bool `json:"brought_to_front,omitempty"`
	// InputContention is set when the front-most target is being flipped back
	// and forth, i.e. more than one operator is driving the same browser
	// instance. Input is still serialized (the browser-level input lock holds),
	// but the two operators disturb each other's rhythm.
	InputContention string `json:"input_contention,omitempty"`
	// Dispatched is false only when the action provably failed before any input
	// reached the page (parse / locator resolution / see-to-click validation /
	// pointer guard). Callers use it to decide whether the page may have
	// changed, so it defaults to true and is narrowed only where proven.
	Dispatched     bool  `json:"-"`
	FocusUpdated   bool  `json:"-"`
	FocusedBackend int64 `json:"-"`
}

// CoordinateHit is the honest answer to "who did my coordinate action actually
// land on?". It mirrors the hit test the @rN click path already performs, so a
// coordinate step can assert on the same evidence a semantic step gets.
type CoordinateHit struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	// Empty is true when the hit test resolves no element at all (outside the
	// document / fully transparent region).
	Empty    bool   `json:"empty,omitempty"`
	Role     string `json:"role,omitempty"`
	Name     string `json:"name,omitempty"`
	Selector string `json:"selector,omitempty"`
	Tag      string `json:"tag,omitempty"`
	// Note carries realm limits the probe cannot see through (e.g. the point
	// lands on a cross-origin iframe, whose inner element is not resolvable).
	Note string `json:"note,omitempty"`
}

// HumanFocusState is persisted by session callers so a strict-human press in
// a later CLI process can prove that the target was focused by an earlier real
// pointer action. Epoch is owned by SessionInfo; the engine only needs the
// current backend-node capability.
type HumanFocusState struct {
	BackendNodeID int64
}

// ActionFidelityCapable exposes action evidence without widening BrowserCore.
type ActionFidelityCapable interface {
	LastActionFidelity() ActionFidelityReport
	RestoreHumanFocus(HumanFocusState)
}

// HitAuditFinding is emitted for a visible ref whose five-point sample is not
// fully owned by the target, and — under --scope all — for an interactable that
// observe itself dropped because something covered its centre.
type HitAuditFinding struct {
	// Ref is empty for occluded-scope findings: those elements were never
	// granted a handle, and the audit must not mint one.
	Ref         string `json:"ref,omitempty"`
	Role        string `json:"role,omitempty"`
	Name        string `json:"name,omitempty"`
	HitCoverage string `json:"hit_coverage"`
	AimSource   string `json:"aim_source,omitempty"`
	// Scope is "visible" for members of the actionable set and "occluded" for
	// elements observe excluded because their centre was covered.
	Scope string `json:"scope,omitempty"`
	// OccludedBy names the element that owns the target's centre point. These
	// findings are the prime "user can see it but cannot click it" candidates.
	OccludedBy string `json:"occluded_by,omitempty"`
	BBox       *Rect  `json:"bbox,omitempty"`
}

// HitAuditCapable is the optional observe --hit-audit extension.
type HitAuditCapable interface {
	AuditHitCoverage(ctx context.Context, refs []ElementRef) ([]HitAuditFinding, error)
}

// OcclusionCensusCapable exposes the elements observe dropped from the visible
// set because their centre was covered at observe time. They are counted in
// every observe (`occluded`) and auditable via --hit-audit --scope all.
type OcclusionCensusCapable interface {
	OccludedInteractableCount() int
	AuditOccludedInteractables(ctx context.Context) ([]HitAuditFinding, error)
}
