package main

import (
	"fmt"
	"github.com/brightman-ai/deepwork-browser/browser"
)

func resolveScenarioE(f *commonFlags, cmd string) (browser.Scenario, browser.SessionPolicy, error) {
	if f.scenario == "" {
		return "", browser.SessionPolicy{}, fmt.Errorf("%s requires --scenario", cmd)
	}
	sc, e := browser.NormalizeScenario(f.scenario)
	if e != nil {
		return "", browser.SessionPolicy{}, e
	}
	if len(f.allowHosts) > 0 && sc != browser.ScenarioWebVisit {
		return "", browser.SessionPolicy{}, fmt.Errorf("--allow-host requires webvisit")
	}
	policy, mode, kind := browser.ScenarioPolicy(sc)
	if sc == browser.ScenarioWebVisit {
		policy.AllowHosts = f.allowHosts
	}
	f.sessionKind = kind
	f.scenario = string(sc)
	if !f.modeExplicit {
		f.mode = mode
		f.headless = mode == browser.ModeHeadless
	}
	return sc, policy, nil
}
