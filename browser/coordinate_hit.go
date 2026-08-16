package browser

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// Coordinate actions bypass the a11y/locator model on purpose: canvas widgets
// and custom controls can only be reached by pixels. Bypassing the model must
// not also bypass the evidence. A pixel action therefore runs the same hit test
// the @rN click path runs (document.elementFromPoint, descending shadow roots)
// and reports the owner of that pixel as `hit`.
//
// It deliberately does NOT block: a human really can click a backdrop, and a
// tool that refuses would be lying about what a human can do. It reports, and
// the agent asserts.
const coordinateHitProbeJSTemplate = `(() => {
	const x = %.4f, y = %.4f;
	const clean = value => (value || '').replace(/\s+/g, ' ').trim().slice(0, 80);
	let node = document.elementFromPoint(x, y);
	for (let depth = 0; node && node.shadowRoot && depth < 16; depth++) {
		const inner = node.shadowRoot.elementFromPoint(x, y);
		if (!inner || inner === node) break;
		node = inner;
	}
	if (!node || node.nodeType !== 1) return {x: x, y: y, empty: true};
	const tag = String(node.tagName || '').toLowerCase();
	const implicitRole = () => {
		switch (tag) {
			case 'button': return 'button';
			case 'a': return node.hasAttribute('href') ? 'link' : '';
			case 'select': return 'combobox';
			case 'textarea': return 'textbox';
			case 'option': return 'option';
			case 'img': return 'img';
			case 'input': {
				const t = (node.getAttribute('type') || 'text').toLowerCase();
				if (t === 'checkbox') return 'checkbox';
				if (t === 'radio') return 'radio';
				if (t === 'button' || t === 'submit' || t === 'reset') return 'button';
				if (t === 'range') return 'slider';
				return 'textbox';
			}
			default: return '';
		}
	};
	const role = clean(node.getAttribute('role')) || implicitRole();
	const name = clean(node.getAttribute('aria-label') || node.getAttribute('title') ||
		node.getAttribute('alt') || node.getAttribute('placeholder') || node.innerText);
	let selector = '';
	const testid = clean(node.getAttribute('data-testid'));
	if (node.id) selector = '#' + node.id;
	else if (testid) selector = '[data-testid="' + testid.replace(/"/g, '\\"') + '"]';
	else {
		const cls = clean(typeof node.className === 'string' ? node.className : '').split(' ').filter(Boolean);
		selector = tag + (cls.length ? '.' + cls.slice(0, 3).join('.') : '');
	}
	const out = {x: x, y: y, role: role, name: name, selector: selector, tag: tag};
	if (tag === 'iframe' || tag === 'frame') {
		out.note = 'point lands on a frame element; the element inside the frame is not resolvable from this realm';
	}
	return out;
})()`

// probeCoordinateHit answers "who owns this pixel right now?" using the page's
// own hit test — the same authority a real mouse event obeys.
func probeCoordinateHit(ctx context.Context, x, y float64) (*CoordinateHit, error) {
	var raw []byte
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(execCtx context.Context) error {
		result, exc, evalErr := runtime.Evaluate(fmt.Sprintf(coordinateHitProbeJSTemplate, x, y)).
			WithReturnByValue(true).
			Do(execCtx)
		if evalErr != nil {
			return evalErr
		}
		if exc != nil {
			return fmt.Errorf("coordinate hit probe failed: %s", exc.Text)
		}
		if result == nil || len(result.Value) == 0 {
			return fmt.Errorf("coordinate hit probe returned no value")
		}
		raw = append([]byte(nil), result.Value...)
		return nil
	}))
	if err != nil {
		return nil, err
	}
	var hit CoordinateHit
	if err := json.Unmarshal(raw, &hit); err != nil {
		return nil, fmt.Errorf("decode coordinate hit probe: %w", err)
	}
	return &hit, nil
}

// recordCoordinateHit probes the dispatch point and files the result on the
// action report. A probe failure is reported as a note rather than swallowed —
// silence here is exactly the failure mode this exists to remove — but it never
// aborts the action, because the hit report is evidence, not a gate.
func (e *actionEngine) recordCoordinateHit(ctx context.Context, x, y float64) {
	hit, err := probeCoordinateHit(ctx, x, y)
	if err != nil {
		hit = &CoordinateHit{X: x, Y: y, Note: "hit probe unavailable: " + err.Error()}
	}
	e.updateActionFidelity(func(report *ActionFidelityReport) {
		report.Hit = hit
	})
}
