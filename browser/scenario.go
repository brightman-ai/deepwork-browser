package browser

import (
	"fmt"
	"strings"
)

// ============================================================
// § Scenario — the required business entry for session creation (SSOT)
// ============================================================
//
// dw-browser exposes a small, fixed set of business SCENARIOS as the single
// required entry for any session-creating command (open / once / session start).
// A scenario is the human-meaningful "what am I doing" choice; it deterministically
// derives the domain-neutral runtime posture (SessionPolicy), the default render
// mode, and the internal session kind. Raw technical knobs (remote-write policy,
// determinism, session kind) are NOT user-facing flags anymore — they are an
// artifact of the chosen scenario, which prevents mis-configuration.
//
// The three scenarios:
//   - app-test-explore  : Claude agent drives a LOCAL app, thin observe+act loop.
//   - app-test-baseline : deterministic regression of a LOCAL app (internal LLM locked).
//   - webvisit          : visit a real public site, writes gated except --allow-host.

// Scenario is the required business-level identity chosen at session creation.
type Scenario string

const (
	// ScenarioAppTestExplore — agent explores a local app (headless, LLM allowed).
	ScenarioAppTestExplore Scenario = "app-test-explore"
	// ScenarioAppTestBaseline — deterministic local-app regression (LLM hard-locked).
	ScenarioAppTestBaseline Scenario = "app-test-baseline"
	// ScenarioWebVisit — visit a real public site (headed; writes gated per origin).
	ScenarioWebVisit Scenario = "webvisit"
)

// ScenarioValues is the fixed, ordered list of legal scenarios (for help / errors).
var ScenarioValues = []Scenario{
	ScenarioAppTestExplore,
	ScenarioAppTestBaseline,
	ScenarioWebVisit,
}

// InteractionPolicy is the scenario-derived human interaction posture. It is
// deliberately not a CLI flag: every agent-driven scenario uses
// screenshot-aligned refs and forbids locator-driven auto scrolling by
// default.
type InteractionPolicy struct {
	SeeToClick bool
}

// ScenarioInteractionPolicy is the SSOT for scenario interaction semantics.
func ScenarioInteractionPolicy(s Scenario) InteractionPolicy {
	switch s {
	case ScenarioAppTestExplore, ScenarioAppTestBaseline, ScenarioWebVisit:
		return InteractionPolicy{SeeToClick: true}
	default:
		return InteractionPolicy{}
	}
}

// InteractionMode is the fidelity selected by one journey step. Visual is the
// default human model; element is an explicit per-step escape hatch that may
// use locator-driven auto scrolling. It is not a session/global policy knob.
type InteractionMode string

const (
	InteractionModeVisual  InteractionMode = "visual"
	InteractionModeElement InteractionMode = "element"
)

// NormalizeInteractionMode validates a journey step mode. An omitted value is
// the visual default so existing specs automatically gain human fidelity.
func NormalizeInteractionMode(raw string) (InteractionMode, error) {
	switch InteractionMode(strings.ToLower(strings.TrimSpace(raw))) {
	case "", InteractionModeVisual:
		return InteractionModeVisual, nil
	case InteractionModeElement:
		return InteractionModeElement, nil
	default:
		return "", fmt.Errorf("unknown interaction mode %q (must be visual or element)", strings.TrimSpace(raw))
	}
}

// ScenarioValuesString renders the legal scenarios for error/help messages.
func ScenarioValuesString() string {
	parts := make([]string, len(ScenarioValues))
	for i, s := range ScenarioValues {
		parts[i] = string(s)
	}
	return strings.Join(parts, " | ")
}

// NormalizeScenario validates a raw scenario string. Unknown/empty → error that
// lists the three legal values (fail-closed: a session is never created without an
// explicit, recognized scenario).
func NormalizeScenario(raw string) (Scenario, error) {
	switch Scenario(strings.ToLower(strings.TrimSpace(raw))) {
	case ScenarioAppTestExplore:
		return ScenarioAppTestExplore, nil
	case ScenarioAppTestBaseline:
		return ScenarioAppTestBaseline, nil
	case ScenarioWebVisit:
		return ScenarioWebVisit, nil
	default:
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return "", fmt.Errorf("--scenario is required (one of: %s)", ScenarioValuesString())
		}
		return "", fmt.Errorf("unknown scenario %q (must be one of: %s)", trimmed, ScenarioValuesString())
	}
}

// ScenarioPolicy maps a scenario to its runtime posture: the SessionPolicy
// (remote-write gating + determinism lock), the default render BrowserMode (used
// only when the caller did not pass an explicit --mode), and the internal
// BrowserSessionKind. It is the single source of truth for scenario semantics.
//
// The returned policy's AllowHosts is left empty; callers merge --allow-host in
// (it is the webvisit trust-list parameter and harmless for the local scenarios).
func ScenarioPolicy(s Scenario) (SessionPolicy, BrowserMode, BrowserSessionKind) {
	switch s {
	case ScenarioAppTestExplore:
		return SessionPolicy{RemoteWrites: RemoteWriteDeny, Deterministic: false}, ModeHeadless, SessionKindTask
	case ScenarioAppTestBaseline:
		return SessionPolicy{RemoteWrites: RemoteWriteDeny, Deterministic: true}, ModeHeadless, SessionKindTask
	case ScenarioWebVisit:
		return SessionPolicy{RemoteWrites: RemoteWriteDeny, Deterministic: false}, ModeHeaded, SessionKindTask
	default:
		// Fail-closed: an unrecognized scenario gets the safest posture. Callers
		// should have validated via NormalizeScenario before reaching here.
		return DefaultSessionPolicy(), ModeHeadless, SessionKindTask
	}
}
