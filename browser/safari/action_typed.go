package safari

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func (c *SafariBrowserCore) SetInteractionScenario(s browser.Scenario) {
	c.mu.Lock()
	c.interaction = browser.ScenarioInteractionPolicy(s)
	c.mu.Unlock()
}
func (c *SafariBrowserCore) SetObserveAll(bool) {}
func (c *SafariBrowserCore) LastActionFidelity() browser.ActionFidelityReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastFidelity
}
func (c *SafariBrowserCore) SetSessionAuthoritySource(id string) {
	c.mu.Lock()
	c.authoritySession = id
	c.mu.Unlock()
}

// Safari consumes the canonical Action, shared semantic validation and policy.
// WebDriver element operations remain explicit functional/synthetic evidence;
// this adapter does not claim an unverified physical editing path.
func (c *SafariBrowserCore) ExecuteAction(ctx context.Context, a browser.Action, observe, sessionMode bool, mode browser.InteractionMode) (*browser.Snapshot, browser.Outcome, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ctx = bounded
	out := browser.Outcome{Phase: "validation", DispatchState: "not-dispatched"}
	if err := a.Validate(); err != nil {
		return nil, out, err
	}
	normalized, err := browser.NormalizeInteractionMode(string(mode))
	if err != nil {
		return nil, out, err
	}
	c.mu.Lock()
	policy, authority := c.interaction, c.authoritySession
	c.lastFidelity = browser.ActionFidelityReport{Fidelity: c.interaction.Fidelity}
	c.mu.Unlock()
	if err := policy.Validate(); err != nil {
		return nil, out, err
	}
	movement := a.Verb == "navigate" || a.Verb == "reload" || a.Verb == "back" || a.Verb == "forward"
	if !movement && (policy.StrictHuman() || (policy.Fidelity == browser.InteractionFidelityDual && normalized != browser.InteractionModeElement)) {
		return nil, out, fmt.Errorf("%w: Safari physical action fidelity unavailable; explicit element mode is required in a baseline scenario", browser.ErrActFailed)
	}
	if a.Verb == "click" && (a.Count == 2 || a.Button == "right") {
		return nil, out, fmt.Errorf("Safari mouse variant unavailable")
	}
	if a.Verb == "scroll" && a.Target.Kind != "" {
		return nil, out, fmt.Errorf("Safari targeted scroll unavailable")
	}
	if a.Target.Point != nil || a.Focused {
		return nil, out, fmt.Errorf("%w: Safari coordinate/focused action unavailable", browser.ErrActFailed)
	}
	switch a.Verb {
	case "click", "fill", "type", "scroll", "navigate", "reload", "back", "forward":
	default:
		return nil, out, fmt.Errorf("%w: Safari action %s unavailable", browser.ErrActFailed, a.Verb)
	}
	if c.wdc == nil {
		return nil, out, fmt.Errorf("Safari runtime unavailable")
	}
	lock, err := browser.AcquireBrowserInputLock("safari:"+c.wdc.baseURL, browser.DefaultBrowserInputLockWait)
	if err != nil {
		return nil, out, err
	}
	defer lock.Release()
	if authority != "" {
		info, err := browser.LoadSession(authority)
		if err != nil {
			return nil, out, err
		}
		if err := browser.SessionAllowsAIInput(info); err != nil {
			return nil, out, err
		}
	}
	if !movement && browser.IsMutatingOp(a.Verb) {
		c.policyMu.RLock()
		pol, lastURL := c.policy, c.lastURL
		c.policyMu.RUnlock()
		live, err := c.wdc.CurrentURL(ctx)
		if err == nil && strings.TrimSpace(live) != "" {
			lastURL = live
		}
		if decision := pol.EvaluateAct(a.Verb, browser.URLOrigin(lastURL)); !decision.Allowed {
			return nil, out, fmt.Errorf("dw-browser: %s", decision.Reason)
		}
	}
	if a.Target.Kind == "ref" {
		c.mu.Lock()
		captured := c.observationDocumentID
		c.mu.Unlock()
		live, err := c.SessionDocumentIdentity(ctx)
		if err != nil || captured == 0 || captured != live {
			return nil, out, fmt.Errorf("stale Safari observation; observe before input")
		}
	}
	target := a.Target.Ref
	if a.Target.Kind == "locator" {
		target = a.Target.Locator
	}
	var el *Element
	if a.Verb == "click" || a.Verb == "fill" || a.Verb == "type" {
		el, err = c.resolveElement(ctx, target)
		if err != nil {
			return nil, out, err
		}
	}
	expectedText := a.Text
	if a.Verb == "fill" || a.Verb == "type" {
		raw, err := c.wdc.ExecuteScript(ctx, `var e=arguments[0];return {editable:/^(INPUT|TEXTAREA)$/.test(e.tagName),disabled:!!e.disabled,readonly:!!e.readOnly,password:e.type==='password',masked:e.type==='password'&&String(getComputedStyle(e).webkitTextSecurity||'none')!=='none',value:String(e.value||''),focused:document.activeElement===e,prefix:String(e.value||'').slice(0,e.selectionStart),suffix:String(e.value||'').slice(e.selectionEnd)}`, map[string]string{w3cElementKey: el.ID})
		if err != nil {
			return nil, out, err
		}
		var state struct {
			Editable, Disabled, Readonly, Password, Masked bool
			Value                                          string
			Focused                                        bool
			Prefix, Suffix                                 string
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			return nil, out, err
		}
		if !state.Editable || state.Disabled || state.Readonly {
			return nil, out, fmt.Errorf("%w: Safari target is not editable", browser.ErrActFailed)
		}
		if a.Verb == "type" {
			expectedText = state.Value + a.Text
			if state.Focused {
				expectedText = state.Prefix + a.Text + state.Suffix
			}
		}
		if a.Sensitive && !state.Masked {
			return nil, out, fmt.Errorf("%w: sensitive fill requires a masked password destination", browser.ErrActFailed)
		}
		if state.Password && !a.Sensitive {
			return nil, out, browser.ErrPasswordField
		}
	}
	out = browser.Outcome{Phase: "dispatch", DispatchState: "unknown", Recovery: "observe before any further input; do not replay"}
	defer c.revokeObservation()
	c.mu.Lock()
	c.lastFidelity.Dispatched = true
	c.lastFidelity.Synthetic = !movement
	c.lastFidelity.SyntheticNote = "Safari WebDriver element/script operation; physical pointer and verified keyboard-selection editing are unverified"
	c.mu.Unlock()
	switch a.Verb {
	case "click":

		err = c.wdc.ClickElement(ctx, *el)
	case "fill", "type":
		if a.Verb == "fill" {
			if err = c.wdc.ClearElement(ctx, *el); err != nil {
				return nil, browser.Outcome{Phase: "delete", DispatchState: "unknown", Recovery: out.Recovery}, err
			}
		}
		err = c.wdc.SendKeys(ctx, *el, a.Text)
		if err == nil {
			got, probeErr := c.wdc.ElementAttribute(ctx, *el, "value")
			if probeErr != nil || got != expectedText {
				return nil, browser.Outcome{Phase: "verify", DispatchState: "partial", Recovery: out.Recovery}, fmt.Errorf("%w: Safari editable value mismatch", browser.ErrActFailed)
			}
		}
	case "scroll":
		err = c.executeScroll(ctx, a.Direction, a.Steps)
	case "navigate":
		err = c.wdc.Navigate(ctx, a.URL)
	case "reload":
		err = c.wdc.sessionPost(ctx, "/refresh", nil, nil)
	case "back":
		err = c.wdc.sessionPost(ctx, "/back", nil, nil)
	case "forward":
		err = c.wdc.sessionPost(ctx, "/forward", nil, nil)
	}
	if err != nil {
		return nil, out, err
	}
	if !observe {
		return nil, browser.Outcome{Phase: "complete", DispatchState: "confirmed"}, nil
	}
	select {
	case <-ctx.Done():
		return nil, out, ctx.Err()
	case <-time.After(wdSettleAfterAction):
	}
	snap, err := c.captureSnapshot(ctx, sessionMode)
	if err != nil {
		return nil, out, err
	}
	return snap, browser.Outcome{Phase: "complete", DispatchState: "confirmed"}, nil
}

func (c *SafariBrowserCore) SessionDocumentIdentity(ctx context.Context) (int64, error) {
	if c.wdc == nil {
		return 0, fmt.Errorf("Safari runtime unavailable")
	}
	raw, err := c.wdc.ExecuteScript(ctx, "return performance.timeOrigin")
	if err != nil {
		return 0, err
	}
	var origin float64
	if err := json.Unmarshal(raw, &origin); err != nil {
		return 0, err
	}
	if origin <= 0 {
		return 0, fmt.Errorf("Safari document identity unavailable")
	}
	return int64(origin * 1000), nil
}
func (c *SafariBrowserCore) revokeObservation() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observationID = ""
	c.observationDocumentID = 0
	for key, ref := range c.refs {
		ref.Observed = false
		c.refs[key] = ref
	}
}
func (c *SafariBrowserCore) observeSnapshot(ctx context.Context, session bool) (*browser.Snapshot, error) {
	c.revokeObservation()
	snap, err := c.captureSnapshot(ctx, session)
	if err != nil {
		return nil, err
	}
	documentID, err := c.SessionDocumentIdentity(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := c.wdc.ExecuteScript(ctx, "return {width:innerWidth,height:innerHeight,dpr:devicePixelRatio,scroll_x:scrollX,scroll_y:scrollY}")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &snap.Viewport); err != nil {
		return nil, err
	}
	snap.ObservationID = browser.NewObservationID()
	snap.DocumentID = documentID
	digest := sha256.Sum256([]byte(c.wdc.baseURL + "/" + c.wdc.SessionID()))
	snap.RuntimeRunID = hex.EncodeToString(digest[:])
	snap.TargetID = c.wdc.SessionID()
	c.mu.Lock()
	authority := c.authoritySession
	c.mu.Unlock()
	if authority != "" {
		if info, err := browser.LoadSession(authority); err == nil {
			snap.RuntimeRunID = info.BrowserRunID
		}
	}
	var lines []string
	for i := range snap.Refs {
		ref := &snap.Refs[i]
		ref.Ref = fmt.Sprintf("@o%s-r%d", snap.ObservationID, i+1)
		ref.Observed = ref.VisibleInViewport && ref.VisibilityKnown
		lines = append(lines, fmt.Sprintf("%s %s %q", ref.Ref, ref.Role, ref.Name))
	}
	snap.Text = strings.Join(lines, "\n")
	c.updateRefs(snap)
	c.mu.Lock()
	c.observationID = snap.ObservationID
	c.observationDocumentID = documentID
	c.mu.Unlock()
	return snap, nil
}
