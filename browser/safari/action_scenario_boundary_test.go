package safari

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func TestPublicActionRejectsUnboundScenarioBeforeTransport(t *testing.T) {
	for _, scenario := range []browser.Scenario{"", "unknown"} {
		core := &SafariBrowserCore{}
		core.SetInteractionScenario(scenario)
		_, out, err := core.ExecuteAction(context.Background(), browser.Action{Verb: "fill", Target: browser.Target{Kind: "locator", Locator: "css=textarea"}, Text: "replacement"}, false, true, browser.InteractionModeVisual)
		if !errors.Is(err, browser.ErrActFailed) || !strings.Contains(err.Error(), "scenario") || out.Phase != "validation" || out.DispatchState != "not-dispatched" {
			t.Fatalf("unbound scenario did not fail before WebDriver: outcome=%+v err=%v", out, err)
		}
	}
}
