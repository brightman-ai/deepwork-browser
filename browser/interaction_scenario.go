package browser

// SetInteractionScenario derives the observation/action model from the
// business scenario. The method intentionally accepts Scenario rather than a
// boolean so callers cannot construct an unsupported hybrid posture.
func (impl *browserCoreImpl) SetInteractionScenario(s Scenario) {
	policy := ScenarioInteractionPolicy(s)
	impl.mu.Lock()
	defer impl.mu.Unlock()
	if impl.snapEngine != nil {
		impl.snapEngine.seeToClick = policy.SeeToClick
	}
	if impl.actEngine != nil {
		impl.actEngine.setSeeToClick(policy.SeeToClick)
	}
}

// SetObserveAll toggles the observe-only census. It never changes the visible
// ref selection or action authority; it only asks the next Snapshot to include
// a separate, ref-less role/name inventory.
func (impl *browserCoreImpl) SetObserveAll(all bool) {
	impl.mu.Lock()
	defer impl.mu.Unlock()
	if impl.snapEngine != nil {
		impl.snapEngine.observeAll = all
	}
}
