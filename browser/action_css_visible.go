package browser

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// resolveActionTarget lets an explicit CSS locator identify an already observed
// node. The query grants no authority and never performs focus/input/setters.
func (e *actionEngine) resolveActionTarget(ctx context.Context, selector string, session, element bool, op string) (string, error) {
	parsed, err := ParseSelector(selector)
	if err != nil {
		return "", err
	}
	if parsed.SType != SelectorCSS || element || !e.seeToClick {
		return e.resolveSemanticSelectorForModeOp(selector, session, element, op)
	}
	ids, err := visibleCSSBackendIDs(ctx, parsed.Raw)
	if err != nil {
		return "", e.failedBeforeDispatch(fmt.Errorf("%w: CSS lookup failed: %v", ErrRefNotFound, err))
	}
	return e.matchObservedCSSNodes(parsed.Raw, ids)
}
func visibleCSSBackendIDs(ctx context.Context, selector string) ([]int64, error) {
	var ids []int64
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		array, exception, err := runtime.Evaluate(`Array.from(document.querySelectorAll(` + strconv.Quote(selector) + `))`).Do(c)
		if err != nil {
			return err
		}
		if exception != nil || array == nil || array.ObjectID == "" {
			return fmt.Errorf("selector query unavailable")
		}
		defer func() { _ = runtime.ReleaseObject(array.ObjectID).Do(c) }()
		properties, _, _, exception, err := runtime.GetProperties(array.ObjectID).WithOwnProperties(true).Do(c)
		if err != nil {
			return err
		}
		if exception != nil {
			return fmt.Errorf("selector query result unavailable")
		}
		for _, property := range properties {
			if _, err := strconv.Atoi(property.Name); err != nil || property.Value == nil || property.Value.ObjectID == "" {
				continue
			}
			node, err := dom.DescribeNode().WithObjectID(property.Value.ObjectID).Do(c)
			_ = runtime.ReleaseObject(property.Value.ObjectID).Do(c)
			if err != nil {
				return err
			}
			if node == nil || node.BackendNodeID == 0 {
				return fmt.Errorf("selector node identity unavailable")
			}
			ids = append(ids, int64(node.BackendNodeID))
		}
		return nil
	}))
	return ids, err
}
func (e *actionEngine) matchObservedCSSNodes(selector string, ids []int64) (string, error) {
	if len(ids) == 0 {
		return "", e.failedBeforeDispatch(fmt.Errorf("%w: CSS locator has no current match", ErrRefNotFound))
	}
	byBackend := map[int64]*ElementRef{}
	for key, meta := range e.snapEngine.refMeta {
		if strings.HasPrefix(key, "#") || meta == nil || !meta.Observed || !meta.VisibilityKnown || !meta.VisibleInViewport || meta.BlockedByModal || meta.BackendNodeID == 0 {
			continue
		}
		byBackend[meta.BackendNodeID] = meta
	}
	var matches []*ElementRef
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		meta, authorized := byBackend[id]
		if !authorized {
			return "", e.failedBeforeDispatch(fmt.Errorf("%w: CSS locator includes a hidden or unobserved node; observe before input", ErrRefNotFound))
		}
		matches = append(matches, meta)
	}
	if len(matches) != 1 {
		return "", e.failedBeforeDispatch(buildAmbiguousError("css="+selector, matches))
	}
	return matches[0].Ref, nil
}
