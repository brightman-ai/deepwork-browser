package browser

import "context"

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
		impl.actEngine.setInteractionPolicy(policy)
	}
}

func (impl *browserCoreImpl) LastActionFidelity() ActionFidelityReport {
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	if impl.actEngine == nil {
		return ActionFidelityReport{}
	}
	return impl.actEngine.lastActionFidelity()
}

func (impl *browserCoreImpl) RestoreHumanFocus(state HumanFocusState) {
	impl.mu.Lock()
	defer impl.mu.Unlock()
	if impl.actEngine != nil {
		impl.actEngine.restoreHumanFocus(state)
	}
}

func (impl *browserCoreImpl) AuditHitCoverage(ctx context.Context, refs []ElementRef) ([]HitAuditFinding, error) {
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	if impl.actEngine == nil {
		return nil, nil
	}
	targetCtx := impl.currentCtx()
	runCtx, cancel := deriveTargetContext(ctx, targetCtx)
	defer cancel()
	return impl.actEngine.auditHitCoverage(runCtx, refs)
}

// OccludedInteractableCount reports how many interactables the most recent
// observation dropped because something covered their centre. It is a plain
// count, not a capability: these elements never receive a ref.
func (impl *browserCoreImpl) OccludedInteractableCount() int {
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	if impl.actEngine == nil {
		return 0
	}
	return impl.actEngine.occludedInteractableCount()
}

// AuditOccludedInteractables re-probes those dropped elements and names who
// covers each one — the "user can see it but cannot click it" shortlist.
func (impl *browserCoreImpl) AuditOccludedInteractables(ctx context.Context) ([]HitAuditFinding, error) {
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	if impl.actEngine == nil {
		return nil, nil
	}
	targetCtx := impl.currentCtx()
	runCtx, cancel := deriveTargetContext(ctx, targetCtx)
	defer cancel()
	return impl.actEngine.auditOccludedInteractables(runCtx)
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
