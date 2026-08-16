package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const visibleAreaThreshold = 0.5

// elementVisibilityProbe is shared by observe filtering and the immediate
// pre-click guard. Box coordinates are viewport CSS pixels.
type elementVisibilityProbe struct {
	Box          Rect    `json:"box"`
	AreaRatio    float64 `json:"area_ratio"`
	StyleVisible bool    `json:"style_visible"`
	HitTarget    bool    `json:"hit_target"`
	Occluder     string  `json:"occluder"`
}

type actionPoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type humanTargetGeometry struct {
	Aim       actionPoint
	Quad      []float64
	TargetBox Rect
	AimSource string
}

type hitCoverageProbe struct {
	Hits      []bool   `json:"hits"`
	Occluders []string `json:"occluders"`
}

func (p hitCoverageProbe) count() int {
	count := 0
	for _, hit := range p.Hits {
		if hit {
			count++
		}
	}
	return count
}

func (p hitCoverageProbe) String() string {
	return fmt.Sprintf("%d/%d", p.count(), len(p.Hits))
}

func (p hitCoverageProbe) centerHit() bool {
	return len(p.Hits) > 0 && p.Hits[0]
}

func (p hitCoverageProbe) centerOccluder() string {
	if len(p.Occluders) == 0 {
		return ""
	}
	return p.Occluders[0]
}

func visibleAreaRatio(box Rect, viewportWidth, viewportHeight float64) float64 {
	if box.Width <= 0 || box.Height <= 0 || viewportWidth <= 0 || viewportHeight <= 0 {
		return 0
	}
	left := math.Max(box.X, 0)
	top := math.Max(box.Y, 0)
	right := math.Min(box.X+box.Width, viewportWidth)
	bottom := math.Min(box.Y+box.Height, viewportHeight)
	if right <= left || bottom <= top {
		return 0
	}
	return ((right - left) * (bottom - top)) / (box.Width * box.Height)
}

func probeIsVisible(p elementVisibilityProbe) bool {
	return p.Box.Width > 0 && p.Box.Height > 0 &&
		p.AreaRatio > visibleAreaThreshold && p.StyleVisible && p.HitTarget
}

func rectFromQuad(quad []float64) (Rect, bool) {
	if len(quad) < 8 {
		return Rect{}, false
	}
	minX, maxX := quad[0], quad[0]
	minY, maxY := quad[1], quad[1]
	for i := 2; i+1 < len(quad); i += 2 {
		x, y := quad[i], quad[i+1]
		minX = math.Min(minX, x)
		maxX = math.Max(maxX, x)
		minY = math.Min(minY, y)
		maxY = math.Max(maxY, y)
	}
	box := Rect{X: minX, Y: minY, Width: maxX - minX, Height: maxY - minY}
	return box, box.Width > 0 && box.Height > 0
}

func polygonAreaAndCentroid(quad []float64) (float64, actionPoint, bool) {
	if len(quad) < 8 || len(quad)%2 != 0 {
		return 0, actionPoint{}, false
	}
	var twiceArea, cxNumerator, cyNumerator float64
	points := len(quad) / 2
	for i := 0; i < points; i++ {
		j := (i + 1) % points
		x1, y1 := quad[i*2], quad[i*2+1]
		x2, y2 := quad[j*2], quad[j*2+1]
		cross := x1*y2 - x2*y1
		twiceArea += cross
		cxNumerator += (x1 + x2) * cross
		cyNumerator += (y1 + y2) * cross
	}
	if math.Abs(twiceArea) < 1e-9 {
		return 0, actionPoint{}, false
	}
	centroid := actionPoint{
		X: cxNumerator / (3 * twiceArea),
		Y: cyNumerator / (3 * twiceArea),
	}
	return math.Abs(twiceArea) / 2, centroid, true
}

func largestContentQuad(quads [][]float64) ([]float64, actionPoint, bool) {
	var (
		bestQuad     []float64
		bestCentroid actionPoint
		bestArea     float64
	)
	for _, quad := range quads {
		area, centroid, ok := polygonAreaAndCentroid(quad)
		if ok && area > bestArea {
			bestArea = area
			bestQuad = append([]float64(nil), quad...)
			bestCentroid = centroid
		}
	}
	return bestQuad, bestCentroid, len(bestQuad) >= 8
}

func rectangleQuad(box Rect) []float64 {
	return []float64{
		box.X, box.Y,
		box.X + box.Width, box.Y,
		box.X + box.Width, box.Y + box.Height,
		box.X, box.Y + box.Height,
	}
}

// fivePointSamples returns the aim point followed by four points halfway from
// the centroid toward each visual quad corner. Every sample stays inside a
// convex content quad while exposing narrow/partially occluded hit regions.
func fivePointSamples(quad []float64, centroid actionPoint) []actionPoint {
	points := []actionPoint{centroid}
	for i := 0; i+1 < len(quad) && len(points) < 5; i += 2 {
		points = append(points, actionPoint{
			X: centroid.X + (quad[i]-centroid.X)*0.5,
			Y: centroid.Y + (quad[i+1]-centroid.Y)*0.5,
		})
	}
	return points
}

func contentQuadsForMeta(ctx context.Context, ref *ElementRef) ([][]float64, error) {
	var quads [][]float64
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(execCtx context.Context) error {
		if ref == nil {
			return fmt.Errorf("element metadata unavailable")
		}
		if ref.BackendNodeID != 0 {
			got, err := dom.GetContentQuads().WithBackendNodeID(cdp.BackendNodeID(ref.BackendNodeID)).Do(execCtx)
			if err != nil {
				return err
			}
			for _, quad := range got {
				quads = append(quads, append([]float64(nil), quad...))
			}
			return nil
		}
		obj, err := resolveElementObjectForMeta(execCtx, ref)
		if err != nil {
			return err
		}
		defer func() { _ = runtime.ReleaseObject(obj.ObjectID).Do(execCtx) }()
		got, err := dom.GetContentQuads().WithObjectID(obj.ObjectID).Do(execCtx)
		if err != nil {
			return err
		}
		for _, quad := range got {
			quads = append(quads, append([]float64(nil), quad...))
		}
		return nil
	}))
	return quads, err
}

func resolveHumanTargetGeometry(ctx context.Context, ref *ElementRef, fallback Rect) humanTargetGeometry {
	if quads, err := contentQuadsForMeta(ctx, ref); err == nil {
		if quad, centroid, ok := largestContentQuad(quads); ok {
			return humanTargetGeometry{Aim: centroid, Quad: quad, TargetBox: fallback, AimSource: AimSourceContentQuadCentroid}
		}
	}
	quad := rectangleQuad(fallback)
	return humanTargetGeometry{
		Aim:       actionPoint{X: fallback.X + fallback.Width*0.5, Y: fallback.Y + fallback.Height*0.5},
		Quad:      quad,
		TargetBox: fallback,
		AimSource: AimSourceBBoxCenter,
	}
}

func probeMetaHitCoverage(ctx context.Context, ref *ElementRef, geometry humanTargetGeometry) (hitCoverageProbe, error) {
	var coverage hitCoverageProbe
	points := fivePointSamples(geometry.Quad, geometry.Aim)
	pointsJSON, err := json.Marshal(points)
	if err != nil {
		return coverage, err
	}
	targetBoxJSON, err := json.Marshal(geometry.TargetBox)
	if err != nil {
		return coverage, err
	}
	err = chromedp.Run(ctx, chromedp.ActionFunc(func(execCtx context.Context) error {
		obj, resolveErr := resolveElementObjectForMeta(execCtx, ref)
		if resolveErr != nil {
			return fmt.Errorf("resolve target object: %w", resolveErr)
		}
		defer func() { _ = runtime.ReleaseObject(obj.ObjectID).Do(execCtx) }()
		fn := `function() {
			const el = this;
			const points = ` + string(pointsJSON) + `;
			const globalBox = ` + string(targetBoxJSON) + `;
			const localBox = el.getBoundingClientRect();
			// DOM.getContentQuads/getBoxModel coordinates are relative to the top
			// viewport, while elementFromPoint in an iframe realm consumes that
			// frame's local viewport coordinates. Map through the live border box;
			// this also handles scaled iframe content without DOM mutation.
			const scaleX = globalBox.width > 0 ? localBox.width / globalBox.width : 1;
			const scaleY = globalBox.height > 0 ? localBox.height / globalBox.height : 1;
			const localPoint = point => ({
				x: localBox.left + (point.x - globalBox.x) * scaleX,
				y: localBox.top + (point.y - globalBox.y) * scaleY
			});
			const ownedHit = node => {
				for (let n = node; n; ) {
					if (n === el) return true;
					if (n.parentElement) { n = n.parentElement; continue; }
					const root = typeof n.getRootNode === 'function' ? n.getRootNode() : null;
					n = root && root.host ? root.host : null;
				}
				return false;
			};
			const clean = value => (value || '').replace(/\s+/g, ' ').trim().slice(0, 80);
			const describe = node => {
				if (!node || node.nodeType !== 1) return 'empty hit region';
				const role = clean(node.getAttribute('role'));
				const name = clean(node.getAttribute('aria-label') || node.getAttribute('title') || node.innerText);
				if (role && name) return role + '[name="' + name.replace(/"/g, '\\"') + '"]';
				const testid = clean(node.getAttribute('data-testid'));
				if (testid) return '[data-testid="' + testid.replace(/"/g, '\\"') + '"]';
				if (node.id) return '#' + node.id;
				return String(node.tagName || 'element').toLowerCase();
			};
			const root = typeof el.getRootNode === 'function' ? el.getRootNode() : document;
			const hits = [], occluders = [];
			for (const point of points) {
				const local = localPoint(point);
				const node = root && typeof root.elementFromPoint === 'function'
					? root.elementFromPoint(local.x, local.y)
					: document.elementFromPoint(local.x, local.y);
				const owned = ownedHit(node);
				hits.push(owned);
				occluders.push(owned ? '' : describe(node));
			}
			return {hits, occluders};
		}`
		result, exc, callErr := runtime.CallFunctionOn(fn).
			WithObjectID(obj.ObjectID).
			WithReturnByValue(true).
			Do(execCtx)
		if callErr != nil {
			return fmt.Errorf("call target hit probe: %w", callErr)
		}
		if exc != nil {
			return fmt.Errorf("hit coverage probe failed: %s", exc.Text)
		}
		if result == nil || len(result.Value) == 0 {
			return fmt.Errorf("hit coverage probe returned no value")
		}
		return json.Unmarshal(result.Value, &coverage)
	}))
	return coverage, err
}

var viewportFactsJS = `(() => ({
	width: window.innerWidth || 0,
	height: ` + LayoutViewportHeightJSExpr + `,
	dpr: window.devicePixelRatio || 1,
	scroll_x: window.scrollX || 0,
	scroll_y: window.scrollY || 0
}))()`

// elementVisibilityProbeFunction runs in the target element's own realm. The
// center-point hit test accepts the element itself or one of its descendants;
// anything else is reported as an explicit occluder for fail-loud diagnostics.
var elementVisibilityProbeFunction = `function() {
	const el = this;
	const r = el.getBoundingClientRect();
	const vw = window.innerWidth || 0;
	const vh = ` + LayoutViewportHeightJSExpr + `;
	const left = Math.max(r.left, 0);
	const top = Math.max(r.top, 0);
	const right = Math.min(r.right, vw);
	const bottom = Math.min(r.bottom, vh);
	const area = Math.max(0, r.width) * Math.max(0, r.height);
	const visibleArea = Math.max(0, right-left) * Math.max(0, bottom-top);
	const areaRatio = area > 0 ? visibleArea / area : 0;
	let styleVisible = area > 0;
	for (let n = el; styleVisible && n && n.nodeType === 1; ) {
		const s = window.getComputedStyle(n);
		if (s.display === 'none' || s.visibility === 'hidden' || s.visibility === 'collapse' ||
			parseFloat(s.opacity || '1') <= 0.01 || s.pointerEvents === 'none' ||
			n.hasAttribute('inert') || n.getAttribute('aria-hidden') === 'true') {
			styleVisible = false;
		}
		if (n.parentElement) { n = n.parentElement; continue; }
		const root = typeof n.getRootNode === 'function' ? n.getRootNode() : null;
		n = root && root.host ? root.host : null;
	}
	const cx = r.left + r.width / 2;
	const cy = r.top + r.height / 2;
	let hit = null;
	if (styleVisible && areaRatio > 0.5 && cx >= 0 && cy >= 0 && cx < vw && cy < vh) {
		const root = typeof el.getRootNode === 'function' ? el.getRootNode() : document;
		hit = root && typeof root.elementFromPoint === 'function' ? root.elementFromPoint(cx, cy) : document.elementFromPoint(cx, cy);
	}
	const ownedHit = node => {
		for (let n = node; n; ) {
			if (n === el) return true;
			if (n.parentElement) { n = n.parentElement; continue; }
			const root = typeof n.getRootNode === 'function' ? n.getRootNode() : null;
			n = root && root.host ? root.host : null;
		}
		return false;
	};
	const hitTarget = !!hit && ownedHit(hit);
	const clean = value => (value || '').replace(/\s+/g, ' ').trim().slice(0, 80);
	const describe = node => {
		if (!node || node.nodeType !== 1) return 'another element';
		const role = clean(node.getAttribute('role'));
		const name = clean(node.getAttribute('aria-label') || node.getAttribute('title') || node.innerText);
		if (role && name) return role + '[name="' + name.replace(/"/g, '\\"') + '"]';
		const testid = clean(node.getAttribute('data-testid'));
		if (testid) return '[data-testid="' + testid.replace(/"/g, '\\"') + '"]';
		if (node.id) return '#' + node.id;
		const cls = clean(typeof node.className === 'string' ? node.className : '').split(' ')[0];
		return node.tagName.toLowerCase() + (cls ? '.' + cls : '');
	};
	return {
		box: {x:r.left, y:r.top, width:r.width, height:r.height},
		area_ratio: areaRatio,
		style_visible: styleVisible,
		hit_target: hitTarget,
		occluder: hitTarget ? '' : describe(hit)
	};
}`

func resolveElementObjectByBackendID(ctx context.Context, backendNodeID int64) (*runtime.RemoteObject, error) {
	if backendNodeID == 0 {
		return nil, fmt.Errorf("backend node id is unavailable")
	}
	obj, err := dom.ResolveNode().WithBackendNodeID(cdp.BackendNodeID(backendNodeID)).Do(ctx)
	if err != nil {
		return nil, err
	}
	if obj == nil || obj.ObjectID == "" {
		return nil, fmt.Errorf("resolved node has no object id")
	}
	return obj, nil
}

func resolveElementObjectByExpression(ctx context.Context, expression string) (*runtime.RemoteObject, error) {
	obj, exc, err := runtime.Evaluate(expression).Do(ctx)
	if err != nil {
		return nil, err
	}
	if exc != nil {
		return nil, fmt.Errorf("element expression failed: %s", exc.Text)
	}
	if obj == nil || obj.ObjectID == "" {
		return nil, fmt.Errorf("element not found")
	}
	return obj, nil
}

func resolveElementObjectForMeta(ctx context.Context, ref *ElementRef) (*runtime.RemoteObject, error) {
	if ref == nil {
		return nil, fmt.Errorf("element metadata unavailable")
	}
	if ref.BackendNodeID != 0 {
		return resolveElementObjectByBackendID(ctx, ref.BackendNodeID)
	}
	if ref.TestID == "" {
		return nil, fmt.Errorf("element %q has neither backend node id nor testid", ref.Ref)
	}
	expr := `Array.from(document.querySelectorAll('[data-testid]')).find(el => el.getAttribute('data-testid') === ` + strconv.Quote(ref.TestID) + `) || null`
	return resolveElementObjectByExpression(ctx, expr)
}

func probeResolvedElement(ctx context.Context, obj *runtime.RemoteObject) (elementVisibilityProbe, error) {
	if obj == nil || obj.ObjectID == "" {
		return elementVisibilityProbe{}, fmt.Errorf("element object unavailable")
	}
	defer func() { _ = runtime.ReleaseObject(obj.ObjectID).Do(ctx) }()
	result, exc, err := runtime.CallFunctionOn(elementVisibilityProbeFunction).
		WithObjectID(obj.ObjectID).
		WithReturnByValue(true).
		Do(ctx)
	if err != nil {
		return elementVisibilityProbe{}, err
	}
	if exc != nil {
		return elementVisibilityProbe{}, fmt.Errorf("visibility probe failed: %s", exc.Text)
	}
	if result == nil || len(result.Value) == 0 {
		return elementVisibilityProbe{}, fmt.Errorf("visibility probe returned no value")
	}
	var probe elementVisibilityProbe
	if err := json.Unmarshal(result.Value, &probe); err != nil {
		return elementVisibilityProbe{}, fmt.Errorf("decode visibility probe: %w", err)
	}
	return probe, nil
}

func probeMetaVisibilityNow(ctx context.Context, ref *ElementRef) (elementVisibilityProbe, error) {
	var probe elementVisibilityProbe
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(execCtx context.Context) error {
		var (
			topBox   Rect
			topRatio float64
			hasTop   bool
		)
		if ref != nil && ref.BackendNodeID != 0 {
			model, modelErr := dom.GetBoxModel().WithBackendNodeID(cdp.BackendNodeID(ref.BackendNodeID)).Do(execCtx)
			if modelErr != nil {
				return fmt.Errorf("read current box for %q: %w", ref.Ref, modelErr)
			}
			if model == nil {
				return fmt.Errorf("read current box for %q: box model unavailable", ref.Ref)
			}
			topBox, hasTop = rectFromQuad(model.Border)
			if !hasTop {
				return fmt.Errorf("current box for %q is invalid", ref.Ref)
			}
			viewportObj, exc, viewportErr := runtime.Evaluate(viewportFactsJS).WithReturnByValue(true).Do(execCtx)
			if viewportErr != nil || exc != nil || viewportObj == nil || len(viewportObj.Value) == 0 {
				return fmt.Errorf("read current viewport for %q: %v", ref.Ref, viewportErr)
			}
			var viewport ViewportFacts
			if decodeErr := json.Unmarshal(viewportObj.Value, &viewport); decodeErr != nil {
				return decodeErr
			}
			topRatio = visibleAreaRatio(topBox, viewport.Width, viewport.Height)
		}
		obj, err := resolveElementObjectForMeta(execCtx, ref)
		if err != nil {
			return err
		}
		probe, err = probeResolvedElement(execCtx, obj)
		if err == nil && hasTop {
			probe.Box = topBox
			probe.AreaRatio = math.Min(probe.AreaRatio, topRatio)
		}
		return err
	}))
	return probe, err
}

func probeSelectorVisibilityNow(ctx context.Context, selector string) (elementVisibilityProbe, error) {
	var probe elementVisibilityProbe
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(execCtx context.Context) error {
		expr := `document.querySelector(` + strconv.Quote(selector) + `)`
		obj, err := resolveElementObjectByExpression(execCtx, expr)
		if err != nil {
			return fmt.Errorf("selector %q: %w", selector, err)
		}
		probe, err = probeResolvedElement(execCtx, obj)
		return err
	}))
	return probe, err
}

func validateSeeToClickProbe(action, ref string, meta *ElementRef, requireObserved bool, probe elementVisibilityProbe) error {
	if requireObserved && (meta == nil || !meta.Observed || !meta.VisibleInViewport) {
		return fmt.Errorf("%w: %s target %q was not in the visible set from the most recent observe — scroll, then run observe again", ErrRefNotFound, action, ref)
	}
	if probe.Box.Width <= 0 || probe.Box.Height <= 0 || !probe.StyleVisible || probe.AreaRatio <= visibleAreaThreshold {
		return fmt.Errorf("%w: %s target %q is not currently visible (>50%% of its box must be inside the viewport) — scroll, then run observe again", ErrActFailed, action, ref)
	}
	if !probe.HitTarget {
		occluder := strings.TrimSpace(probe.Occluder)
		if occluder == "" {
			occluder = "another element"
		}
		return fmt.Errorf("%w: %s target %q is occluded by %s at its center — scroll or dismiss the overlay, then observe again", ErrActFailed, action, ref, occluder)
	}
	return nil
}

// enrichSeeToClickRefs aligns refs with the screenshot viewport. DOM.getBoxModel
// is one cheap lookup per AX node; expensive object resolution + hit-testing is
// performed only for boxes that already pass the >50% area threshold.
func enrichSeeToClickRefs(ctx context.Context, refs []ElementRef) ([]ElementRef, ViewportFacts, int, error) {
	var viewport ViewportFacts
	if err := chromedp.Run(ctx, chromedp.Evaluate(viewportFactsJS, &viewport)); err != nil {
		return nil, ViewportFacts{}, 0, fmt.Errorf("browser: read see-to-click viewport: %w", err)
	}
	if viewport.DevicePixelRatio <= 0 {
		viewport.DevicePixelRatio = 1
	}
	if viewport.Width <= 0 || viewport.Height <= 0 {
		return nil, viewport, 0, fmt.Errorf("browser: invalid see-to-click viewport %.1fx%.1f", viewport.Width, viewport.Height)
	}

	visibleCount := 0
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(execCtx context.Context) error {
		for i := range refs {
			refs[i].Observed = true
			var (
				probe        elementVisibilityProbe
				topBox       Rect
				topAreaRatio float64
				hasTopBox    bool
			)
			if refs[i].BackendNodeID != 0 {
				model, modelErr := dom.GetBoxModel().
					WithBackendNodeID(cdp.BackendNodeID(refs[i].BackendNodeID)).
					Do(execCtx)
				if modelErr != nil || model == nil {
					if execCtx.Err() != nil {
						return execCtx.Err()
					}
					continue
				}
				box, ok := rectFromQuad(model.Border)
				if !ok {
					continue
				}
				topBox = box
				hasTopBox = true
				probe.Box = box
				probe.AreaRatio = visibleAreaRatio(box, viewport.Width, viewport.Height)
				topAreaRatio = probe.AreaRatio
				refs[i].BBox = box
				refs[i].VisibilityKnown = true
				if probe.AreaRatio <= visibleAreaThreshold {
					continue
				}
			}

			obj, resolveErr := resolveElementObjectForMeta(execCtx, &refs[i])
			if resolveErr != nil {
				if execCtx.Err() != nil {
					return execCtx.Err()
				}
				continue
			}
			var probeErr error
			probe, probeErr = probeResolvedElement(execCtx, obj)
			if probeErr != nil {
				if execCtx.Err() != nil {
					return execCtx.Err()
				}
				continue
			}
			if hasTopBox {
				probe.Box = topBox
				probe.AreaRatio = math.Min(probe.AreaRatio, topAreaRatio)
			}
			refs[i].BBox = probe.Box
			refs[i].VisibilityKnown = true
			refs[i].VisibleInViewport = probeIsVisible(probe)
			if refs[i].VisibleInViewport {
				visibleCount++
			}
		}
		return nil
	}))
	if err != nil {
		return nil, viewport, visibleCount, fmt.Errorf("browser: see-to-click visibility probe: %w", err)
	}

	selected := make([]ElementRef, 0, len(refs))
	for i := range refs {
		if refs[i].VisibleInViewport {
			selected = append(selected, refs[i])
		}
	}
	for i := range selected {
		selected[i].Ref = fmt.Sprintf("e%d", i+1)
	}
	return selected, viewport, visibleCount, nil
}
