package browser

import (
	"context"
	"fmt"
	"strings"

	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/chromedp"
)

type observationAuthority struct {
	ID, RuntimeRunID, TargetID, URL string
	DocumentID                      int64
	Width, Height                   float64
	ScrollX, ScrollY, DPR           float64
}

func (e *snapshotEngine) revokeObservationAuthority() {
	e.authority = nil
	for _, meta := range e.refMeta {
		meta.Observed = false
	}
}

func (e *snapshotEngine) authorizeSnapshot(ctx context.Context, snap *Snapshot) (*Snapshot, error) {
	if snap == nil {
		return nil, fmt.Errorf("observation snapshot unavailable")
	}
	if e.ownerSource != nil {
		info, err := e.ownerSource(ctx)
		if err != nil {
			return nil, err
		}
		if info != nil && info.BrowserRunID != "" {
			e.runtimeRunID = info.BrowserRunID
		}
	}
	id := NewObservationID()
	snap.ObservationID = id
	snap.RuntimeRunID = e.runtimeRunID
	snap.TargetID = currentTargetID(ctx)
	var documentID int64
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		root, err := dom.GetDocument().Do(c)
		if err != nil {
			return err
		}
		if root == nil {
			return fmt.Errorf("document identity unavailable")
		}
		documentID = int64(root.BackendNodeID)
		return nil
	}))
	if err != nil {
		e.revokeObservationAuthority()
		if len(snap.Refs) == 0 {
			return snap, nil
		}
		return nil, fmt.Errorf("observation document identity: %w", err)
	}
	snap.DocumentID = documentID
	// Unconfigured library captures still need real viewport/visibility facts for
	// capability refs. Explicit element-mode locators remain a separate path.
	if !e.seeToClick && len(snap.Refs) > 0 {
		refs, viewport, visible, occluded, err := enrichSeeToClickRefs(ctx, snap.Refs)
		if err != nil {
			return nil, err
		}
		snap.Refs = refs
		snap.Viewport = viewport
		snap.VisibleInteractableCount = visible
		e.occluded = occluded
	}
	if snap.Viewport.Width <= 0 || snap.Viewport.Height <= 0 {
		if err := chromedp.Run(ctx, chromedp.Evaluate(viewportFactsJS, &snap.Viewport)); err != nil {
			return nil, err
		}
	}

	if err := e.captureControlStates(ctx, snap.Refs); err != nil {
		return nil, err
	}
	e.bindCapturedSnapshot(snap)
	return snap, nil
}

// bindCapturedSnapshot replaces the capability map exactly once for this
// capture. Census entries and private element candidates never receive refs.
func (e *snapshotEngine) bindCapturedSnapshot(snap *Snapshot) {
	e.refTable = make(map[string]int64, len(snap.Refs))
	e.refMeta = make(map[string]*ElementRef, len(snap.Refs)*2)
	for i := range snap.Refs {
		meta := &snap.Refs[i]
		meta.Ref = fmt.Sprintf("@o%s-r%d", snap.ObservationID, i+1)
		meta.Observed = meta.VisibilityKnown && meta.VisibleInViewport && !snap.Progressive
		meta.RecommendedLocator = computeRecommendedLocator(meta)
		e.refTable[meta.Ref] = meta.BackendNodeID
		privateMeta := *meta
		e.refMeta[meta.Ref] = &privateMeta
		if meta.TestID != "" {
			e.refMeta["#"+meta.TestID] = &privateMeta
		}
	}
	snap.Text = buildCompactTextWithMode(snap.Refs, true)
	snap.TokenEst = estimateTokens(snap.Text)
	e.authority = &observationAuthority{ID: snap.ObservationID, RuntimeRunID: snap.RuntimeRunID, TargetID: snap.TargetID, URL: snap.URL, DocumentID: snap.DocumentID, Width: snap.Viewport.Width, Height: snap.Viewport.Height, ScrollX: snap.Viewport.ScrollX, ScrollY: snap.Viewport.ScrollY, DPR: snap.Viewport.DevicePixelRatio}
}

func (e *snapshotEngine) validateObservationAuthority(ctx context.Context, captureID string) error {
	authority := e.authority
	if authority == nil || authority.ID == "" {
		return fmt.Errorf("%w: observation authority unavailable; observe before input", ErrRefNotFound)
	}
	if captureID != "" && captureID != authority.ID {
		return fmt.Errorf("%w: stale observation_id; observe before input", ErrRefNotFound)
	}
	if authority.TargetID != currentTargetID(ctx) || authority.RuntimeRunID != e.runtimeRunID {
		return fmt.Errorf("%w: stale target/runtime observation; observe before input", ErrRefNotFound)
	}
	var docID int64
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		root, err := dom.GetDocument().Do(c)
		if err != nil {
			return err
		}
		if root == nil {
			return fmt.Errorf("document unavailable")
		}
		docID = int64(root.BackendNodeID)
		return nil
	}))
	if err != nil {
		return err
	}
	var facts struct {
		URL                   string
		Width, Height         float64
		ScrollX, ScrollY, DPR float64
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`({URL:location.href,Width:innerWidth,Height:`+LayoutViewportHeightJSExpr+`,ScrollX:scrollX,ScrollY:scrollY,DPR:devicePixelRatio})`, &facts)); err != nil {
		return err
	}
	if docID != authority.DocumentID || facts.URL != authority.URL || facts.Width != authority.Width || facts.Height != authority.Height || facts.ScrollX != authority.ScrollX || facts.ScrollY != authority.ScrollY || facts.DPR != authority.DPR {
		return fmt.Errorf("%w: stale document or viewport observation; observe before input", ErrRefNotFound)
	}
	return nil
}

func actionRequiresObservation(p *actionRequest, elementMode, seeToClick bool) bool {
	if p.ObservationID != "" {
		return true
	}
	if p.Op == "typetext" {
		return false
	}
	if strings.HasPrefix(p.Ref, "@o") {
		return true
	}
	if elementMode {
		return false
	}
	return seeToClick && (p.Ref != "" || p.ScrollTarget != "" || p.Op == "scroll")
}
