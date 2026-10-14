package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestInteractionPolicyValidationPreservesRecognizedScenarios(t *testing.T) {
	for _, scenario := range []Scenario{ScenarioAppTestExplore, ScenarioAppTestBaseline, ScenarioWebVisit} {
		if err := ScenarioInteractionPolicy(scenario).Validate(); err != nil {
			t.Fatalf("recognized %s posture rejected: %v", scenario, err)
		}
	}
	for _, policy := range []InteractionPolicy{{}, {SeeToClick: true, Fidelity: "unknown"}, {Fidelity: InteractionFidelityUtility}} {
		if err := policy.Validate(); !errors.Is(err, ErrActFailed) {
			t.Fatalf("unrecognized hybrid posture accepted: %+v err=%v", policy, err)
		}
	}
}

func TestPublicActionRejectsUnboundScenarioBeforeTransport(t *testing.T) {
	for _, scenario := range []Scenario{"", "unknown"} {
		t.Run(string(scenario), func(t *testing.T) {
			core := &browserCoreImpl{snapEngine: newSnapshotEngine()}
			core.actEngine = newActionEngine(core.snapEngine)
			core.SetInteractionScenario(scenario)
			// No takeover controller or CDP transport: a safe public boundary must
			// reject before touching either, rather than reaching a setter path.
			var out Outcome
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("reached transport/controller: %v", r)
					}
				}()
				_, out, err = core.ExecuteAction(context.Background(), Action{Verb: "fill", Target: Target{Kind: "locator", Locator: "css=textarea"}, Text: "replacement"}, false, true, InteractionModeVisual)
			}()
			if !errors.Is(err, ErrActFailed) || !strings.Contains(err.Error(), "scenario") || out.Phase != "validation" || out.DispatchState != "not-dispatched" {
				t.Fatalf("unbound scenario did not fail at public validation boundary: outcome=%+v err=%v", out, err)
			}
		})
	}
}
