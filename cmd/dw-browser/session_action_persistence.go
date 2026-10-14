package main

import (
	"fmt"
	"github.com/brightman-ai/deepwork-browser/browser"
)

// executePersistedSessionAction fences the existing typed dispatch port. It
// neither resolves a target nor retries input; canonical Outcome decides state.
func executePersistedSessionAction(info *browser.SessionInfo, request browser.Action, setOperation func(string), execute func() (*browser.Snapshot, browser.Outcome, browser.ActionFidelityReport, error)) (browser.Outcome, error) {
	reject := func(err error) (browser.Outcome, error) {
		return browser.Outcome{Phase: "validation", DispatchState: "not-dispatched"}, err
	}
	if info == nil {
		return reject(fmt.Errorf("missing session"))
	}
	latest, err := browser.LoadSession(info.SessionID)
	if err != nil {
		return reject(err)
	}
	*info = *latest
	if err := browser.SessionAllowsAIInput(info); err != nil {
		return reject(err)
	}
	if info.LastActionOutcome == browser.SessionActionOutcomeUnknown || info.LastActionOutcome == browser.SessionActionOutcomeInProgress {
		return reject(fmt.Errorf("prior action delivery unknown; observe before further input; do not replay"))
	}
	if err := request.Validate(); err != nil {
		return reject(err)
	}
	if request.Target.Kind == "ref" || request.Target.Point != nil {
		if err := browser.ValidateSessionObservation(info); err != nil {
			return reject(err)
		}
	}
	info.LastActionOperationID = "operation-" + browser.NewObservationID()
	if request.Sensitive {
		info.RedactFormValues = true
	}
	setOperation(info.LastActionOperationID)
	fence := *info
	fence.LastActionOutcome = browser.SessionActionOutcomeInProgress
	fence.Refs = nil
	if err := browser.SaveSession(&fence); err != nil {
		info.LastActionOutcome = browser.SessionActionOutcomeUnknown
		browser.InvalidateSessionObservation(info)
		return browser.Outcome{Phase: "persistence", DispatchState: "unknown", Recovery: "inspect session persistence before input; do not replay"}, fmt.Errorf("persist action fence: %w", err)
	}
	snap, outcome, report, dispatchErr := execute()
	// Core dispatch has released its own input window. Reacquire the same
	// runtime lock only for recording; this never raises or activates a target.
	terminalLock, lockErr := acquireSessionRuntimeInputLock(info)
	if lockErr != nil {
		return uncertainSessionActionPersistence(info, fmt.Errorf("lock terminal outcome: %w", lockErr))
	}
	defer terminalLock.Release()
	current, loadErr := browser.LoadSession(info.SessionID)
	if loadErr != nil {
		return uncertainSessionActionPersistence(info, fmt.Errorf("read terminal fence: %w", loadErr))
	}
	if current.RuntimeID != info.RuntimeID || current.BrowserRunID != info.BrowserRunID || current.LastActionOperationID != info.LastActionOperationID || current.LastActionOutcome != browser.SessionActionOutcomeInProgress {
		return uncertainSessionActionPersistence(info, fmt.Errorf("terminal runtime or operation fence changed; newer actor state retained"))
	}
	controlChanged := current.AuthorityState != info.AuthorityState || current.Owner != info.Owner || current.AuthorityPreviousOwner != info.AuthorityPreviousOwner
	info.AuthorityState, info.Owner, info.AuthorityPreviousOwner = current.AuthorityState, current.Owner, current.AuthorityPreviousOwner
	info.RedactFormValues = info.RedactFormValues || current.RedactFormValues
	switch outcome.DispatchState {
	case "not-dispatched":
		applyFailedActionOutcome(info, true)
	case "confirmed":
		navigated := false
		if snap != nil {
			navigated = applyPostActionSnapshot(info, snap)
		}
		if request.Verb == "navigate" || request.Verb == "reload" || request.Verb == "back" || request.Verb == "forward" {
			navigated = true
		}
		reconcileSessionHumanFocus(info, report, navigated)
		if navigated {
			browser.InvalidateSessionObservation(info)
		} else {
			browser.RevokeSessionObservationGeometry(info)
		}
		info.LastActionOutcome = browser.SessionActionOutcomeConfirmed
	default:
		applyFailedActionOutcome(info, false)
		browser.InvalidateSessionObservation(info)
		if dispatchErr == nil {
			dispatchErr = fmt.Errorf("dispatch completion unproven: %q", outcome.DispatchState)
		}
		outcome.DispatchState = "unknown"
	}
	if controlChanged {
		browser.InvalidateSessionObservation(info)
	}
	if err := browser.SaveSession(info); err != nil {
		return uncertainSessionActionPersistence(info, fmt.Errorf("persist action outcome: %w", err))
	}
	return outcome, dispatchErr
}

func uncertainSessionActionPersistence(info *browser.SessionInfo, err error) (browser.Outcome, error) {
	info.LastActionOutcome = browser.SessionActionOutcomeUnknown
	browser.InvalidateSessionObservation(info)
	return browser.Outcome{Phase: "persistence", DispatchState: "unknown", Recovery: "observe before further input; do not replay"}, err
}
