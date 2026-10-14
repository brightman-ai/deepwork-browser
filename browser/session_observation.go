package browser

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// BindSessionObservation binds existing capture capabilities to persisted session metadata.
// It never issues a second token namespace. Call before writing the canonical observation; persisted refs and its public
// projection must have exactly the same identities.
func BindSessionObservation(info *SessionInfo, snap *Snapshot, observationID, path string) error {
	if info == nil || snap == nil || strings.TrimSpace(observationID) == "" {
		return fmt.Errorf("observation binding: session, snapshot and observation id required")
	}
	if info.ObservationID == observationID {
		return fmt.Errorf("observation binding: observation id reused")
	}
	if snap.ObservationID == "" || snap.ObservationID != observationID {
		return fmt.Errorf("observation binding: capture identity missing or mismatched")
	}
	if snap.RuntimeRunID != "" && snap.RuntimeRunID != info.BrowserRunID {
		return fmt.Errorf("observation binding: runtime generation mismatch")
	}
	if snap.TargetID != "" && snap.TargetID != info.TargetID {
		return fmt.Errorf("observation binding: target mismatch")
	}
	for _, ref := range snap.Refs {
		if !strings.HasPrefix(ref.Ref, "@o"+observationID+"-r") {
			return fmt.Errorf("observation binding: ref does not belong to capture")
		}
	}
	revalidateSessionHumanFocus(info, snap)
	info.ObservationDocumentID = snap.DocumentID
	info.ObservationScrollX, info.ObservationScrollY, info.ObservationDPR = snap.Viewport.ScrollX, snap.Viewport.ScrollY, snap.Viewport.DevicePixelRatio

	info.ObservationValid = true
	info.ObservationPageURL = snap.URL
	info.ObservationID, info.ObservationPath = observationID, path
	info.ObservationRunID, info.ObservationTargetID = info.BrowserRunID, info.TargetID
	info.ObservationViewportW, info.ObservationViewportH = info.ViewportW, info.ViewportH
	return nil
}

// InvalidateSessionObservation revokes all page-derived capabilities while
// retaining the last artifact identity for diagnosis.
func InvalidateSessionObservation(info *SessionInfo) {
	if info == nil {
		return
	}
	RevokeSessionObservationGeometry(info)
	info.HumanFocusBackendNodeID = 0
	info.HumanFocusPageURL = ""
	info.HumanFocusEpoch = 0
}

func ValidateSessionObservation(info *SessionInfo) error {
	if info == nil || info.ObservationID == "" || !info.ObservationValid {
		return fmt.Errorf("observation authority unavailable; run observe --id <id>")
	}
	if info.ObservationPageURL != info.PageURL || info.ObservationRunID != info.BrowserRunID || info.ObservationTargetID != info.TargetID || info.ObservationViewportW != info.ViewportW || info.ObservationViewportH != info.ViewportH {
		return fmt.Errorf("stale observation: runtime, target or viewport changed; run observe --id <id>")
	}
	return nil
}

func SessionAllowsAIInput(info *SessionInfo) error {
	if info == nil {
		return fmt.Errorf("input authority: missing session")
	}
	switch info.AuthorityState {
	case AuthorityHuman:
		return ErrTakeoverActive
	case AuthorityAgentic, AuthorityShared:
		return nil
	default:
		return fmt.Errorf("input authority: unknown authority state %q", info.AuthorityState)
	}
}

func SetSessionAuthority(info *SessionInfo, authority string) error {
	if info == nil {
		return fmt.Errorf("authority: missing session")
	}
	switch authority {
	case AuthorityHuman, AuthorityAgentic, AuthorityShared:
	default:
		return fmt.Errorf("authority: unsupported state %q", authority)
	}
	if authority == AuthorityHuman && info.AuthorityState != AuthorityHuman {
		info.AuthorityPreviousOwner = info.Owner
		info.Owner = SessionOwnerHuman
	} else if authority != AuthorityHuman && info.AuthorityState == AuthorityHuman {
		info.Owner = info.AuthorityPreviousOwner
		if info.Owner == "" {
			info.Owner = DefaultsForBrowserSessionKind(info.SessionKind).Owner
		}
		info.AuthorityPreviousOwner = ""
	}
	info.AuthorityState = authority
	InvalidateSessionObservation(info)
	return nil
}

// RestoreSessionAuthority restores the existing takeover controller rather than
// constructing an independent input policy. The persisted source is also read
// again by the dispatch guard under the runtime input lock.
func RestoreSessionAuthority(ctx context.Context, core BrowserCore, info *SessionInfo) error {
	if info == nil {
		return fmt.Errorf("authority restore: missing session")
	}
	if info.AuthorityState != AuthorityHuman && info.AuthorityState != AuthorityAgentic && info.AuthorityState != AuthorityShared {
		return fmt.Errorf("authority restore: unknown authority state %q", info.AuthorityState)
	}
	if source, ok := core.(interface{ SetSessionAuthoritySource(string) }); ok {
		source.SetSessionAuthoritySource(info.SessionID)
	}
	if info.AuthorityState == AuthorityHuman {
		return core.EnableTakeover(ctx)
	}
	return core.DisableTakeover(ctx)
}

// RevokeSessionObservationGeometry revokes page evidence after terminal input
// while retaining a physical focus proof for a following focused-input step.
// Navigation, unknown dispatch, target changes and takeover use full invalidation.
func RevokeSessionObservationGeometry(info *SessionInfo) {
	if info == nil {
		return
	}
	info.ObservationValid = false
	info.Refs = nil
}

// ApplySessionIntervention records provenance without storing debug scripts,
// credentials or text. Persistence remains the owning adapter's responsibility.
func ApplySessionIntervention(info *SessionInfo, operationID, kind string) error {
	if info == nil || operationID == "" || kind == "" {
		return fmt.Errorf("intervention requires session, operation id and kind")
	}
	InvalidateSessionObservation(info)
	info.InterventionCount++
	info.LastInterventionOperationID = operationID
	info.LastInterventionKind = kind
	info.LastInterventionAt = time.Now().UTC().Format(time.RFC3339Nano)
	info.LastActionOutcome = SessionActionOutcomeUnknown
	return nil
}
