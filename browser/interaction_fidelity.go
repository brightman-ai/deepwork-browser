package browser

import "context"

const (
	AimSourceContentQuadCentroid = "content-quad-centroid"
	AimSourceBBoxCenter          = "bbox-center-fallback"
)

// ActionFidelityReport is optional metadata for the most recently completed
// action. BrowserCore remains unchanged; CLI callers discover this capability
// through ActionFidelityCapable.
type ActionFidelityReport struct {
	Fidelity       InteractionFidelity `json:"fidelity,omitempty"`
	Synthetic      bool                `json:"synthetic,omitempty"`
	SyntheticNote  string              `json:"synthetic_note,omitempty"`
	HumanPath      []string            `json:"human_path,omitempty"`
	AimSource      string              `json:"aim_source,omitempty"`
	HitCoverage    string              `json:"hit_coverage,omitempty"`
	FocusUpdated   bool                `json:"-"`
	FocusedBackend int64               `json:"-"`
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

// HitAuditFinding is emitted only for a visible ref whose five-point sample is
// not fully owned by the target.
type HitAuditFinding struct {
	Ref         string `json:"ref"`
	Role        string `json:"role,omitempty"`
	Name        string `json:"name,omitempty"`
	HitCoverage string `json:"hit_coverage"`
	AimSource   string `json:"aim_source,omitempty"`
}

// HitAuditCapable is the optional observe --hit-audit extension.
type HitAuditCapable interface {
	AuditHitCoverage(ctx context.Context, refs []ElementRef) ([]HitAuditFinding, error)
}
