package dwbrowser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
	btest "github.com/brightman-ai/deepwork-browser/browser/testing"
)

// BoundRuntime borrows a host-owned runtime and its current session facts.
// The server never closes it or creates a session store. With no Persist,
// Info is explicitly an in-memory mutable host-owned binding; the host must
// coordinate external access. State may instead reread the host's own store.
type BoundRuntime struct {
	Core    browser.SessionCore
	Info    *browser.SessionInfo
	State   func(context.Context) (*browser.SessionInfo, error)
	Persist func(context.Context, *browser.SessionInfo) error
}
type RuntimeResolver func(context.Context, string) (BoundRuntime, error)

func WithRuntimeResolver(resolve RuntimeResolver) Option {
	return func(s *Server) { s.runtimeResolver = resolve }
}

func writeHTTPResult(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeHTTPError(w http.ResponseWriter, status int, out browser.Outcome, err error) {
	writeHTTPResult(w, status, map[string]interface{}{"success": false, "outcome": out, "error": err.Error()})
}
func parameterError(w http.ResponseWriter, err error) {
	writeHTTPError(w, http.StatusBadRequest, browser.Outcome{Phase: "validation", DispatchState: "not-dispatched"}, err)
}
func decodeHTTPJSON(r *http.Request, dst interface{}) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid request: %w", err)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("request must contain exactly one JSON object")
	}
	return nil
}
func validateHTTPID(id string) error {
	if id == "" || strings.TrimSpace(id) != id {
		return fmt.Errorf("explicit session id required")
	}
	return nil
}
func (s *Server) resolveRuntime(ctx context.Context, id string) (BoundRuntime, error) {
	if s.runtimeResolver == nil {
		return BoundRuntime{}, fmt.Errorf("no browser runtime is bound")
	}
	bound, err := s.runtimeResolver(ctx, id)
	if err != nil {
		return BoundRuntime{}, err
	}
	if bound.Core == nil || bound.Info == nil {
		return BoundRuntime{}, fmt.Errorf("bound runtime unavailable")
	}
	if bound.Info.SessionID != id {
		return BoundRuntime{}, fmt.Errorf("runtime/session binding mismatch")
	}
	if bound.State != nil {
		info, err := bound.State(ctx)
		if err != nil {
			return BoundRuntime{}, err
		}
		if info == nil || info.SessionID != id {
			return BoundRuntime{}, fmt.Errorf("runtime state binding mismatch")
		}
		bound.Info = info
	}
	if capable, ok := bound.Core.(browser.ScenarioInteractionCapable); ok {
		scenario, err := browser.NormalizeScenario(bound.Info.Scenario)
		if err != nil {
			return BoundRuntime{}, err
		}
		capable.SetInteractionScenario(scenario)
	}
	bound.Core.SetPolicy(bound.Info.Policy, bound.Info.PageURL)
	if guard, ok := bound.Core.(interface {
		SetSessionAuthorityProvider(func(context.Context) (*browser.SessionInfo, error))
	}); ok {
		provider := bound.State
		if provider == nil {
			provider = func(context.Context) (*browser.SessionInfo, error) { return bound.Info, nil }
		}
		guard.SetSessionAuthorityProvider(provider)
	}
	return bound, nil
}
func persistHTTPRuntime(ctx context.Context, bound BoundRuntime) error {
	if bound.Persist != nil {
		return bound.Persist(ctx, bound.Info)
	}
	return nil
}

type httpActionRequest struct {
	ID     string                  `json:"id"`
	Action *browser.Action         `json:"action"`
	Mode   browser.InteractionMode `json:"mode,omitempty"`
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	var request httpActionRequest
	if err := decodeHTTPJSON(r, &request); err != nil {
		parameterError(w, err)
		return
	}
	if err := validateHTTPID(request.ID); err != nil {
		parameterError(w, err)
		return
	}
	if request.Action == nil {
		parameterError(w, fmt.Errorf("structured action required"))
		return
	}
	if err := request.Action.Validate(); err != nil {
		parameterError(w, err)
		return
	}
	mode, err := browser.NormalizeInteractionMode(string(request.Mode))
	if err != nil {
		parameterError(w, err)
		return
	}
	s.executeHTTPAction(w, r, request.ID, *request.Action, mode)
}
func (s *Server) handleBoundNavigate(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := decodeHTTPJSON(r, &request); err != nil {
		parameterError(w, err)
		return
	}
	if err := validateHTTPID(request.ID); err != nil {
		parameterError(w, err)
		return
	}
	action := browser.Action{Verb: "navigate", URL: request.URL}
	if err := action.Validate(); err != nil {
		parameterError(w, err)
		return
	}
	s.executeHTTPAction(w, r, request.ID, action, browser.InteractionModeVisual)
}
func (s *Server) executeHTTPAction(w http.ResponseWriter, r *http.Request, id string, action browser.Action, mode browser.InteractionMode) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	bound, err := s.resolveRuntime(ctx, id)
	if err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "binding", DispatchState: "not-dispatched"}, err)
		return
	}
	if err := browser.SessionAllowsAIInput(bound.Info); err != nil {
		writeHTTPError(w, http.StatusConflict, browser.Outcome{Phase: "authority", DispatchState: "not-dispatched"}, err)
		return
	}
	if action.Target.Kind == "ref" || action.Target.Point != nil {
		if err := browser.ValidateSessionObservation(bound.Info); err != nil {
			writeHTTPError(w, http.StatusConflict, browser.Outcome{Phase: "validation", DispatchState: "not-dispatched"}, err)
			return
		}
	}
	if mode == browser.InteractionModeElement && browser.ScenarioInteractionPolicy(browser.Scenario(bound.Info.Scenario)).StrictHuman() {
		parameterError(w, fmt.Errorf("element mode unavailable in strict-human scenario"))
		return
	}
	bound.Core.RestoreRefsFromSession(bound.Info.Refs)
	if bound.Info.LastActionOutcome == browser.SessionActionOutcomeUnknown || bound.Info.LastActionOutcome == browser.SessionActionOutcomeInProgress {
		writeHTTPError(w, http.StatusConflict, browser.Outcome{Phase: "recovery", DispatchState: "not-dispatched", Recovery: "observe before further input; do not replay"}, fmt.Errorf("previous action delivery unknown; observe before further input"))
		return
	}
	if action.Sensitive {
		bound.Info.RedactFormValues = true
	}
	prior := *bound.Info
	operationID := browser.NewObservationID()
	bound.Info.LastActionOutcome = browser.SessionActionOutcomeInProgress
	bound.Info.LastActionOperationID = operationID
	if setter, ok := bound.Core.(interface{ SetSessionOperationID(string) }); ok {
		setter.SetSessionOperationID(operationID)
	}
	if err := persistHTTPRuntime(ctx, bound); err != nil {
		*bound.Info = prior
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "fence", DispatchState: "not-dispatched"}, err)
		return
	}
	snap, outcome, actionErr := bound.Core.ExecuteAction(ctx, action, action.Verb == "navigate" || action.Verb == "reload" || action.Verb == "back" || action.Verb == "forward", true, mode)
	if outcome.DispatchState == "not-dispatched" {
		*bound.Info = prior
	} else {
		if actionErr != nil {
			bound.Info.LastActionOutcome = browser.SessionActionOutcomeUnknown
			browser.InvalidateSessionObservation(bound.Info)
		} else {
			bound.Info.LastActionOutcome = browser.SessionActionOutcomeConfirmed
			if snap != nil && snap.URL != "" && snap.URL != bound.Info.PageURL {
				bound.Info.PageURL = snap.URL
				browser.InvalidateSessionObservation(bound.Info)
			} else {
				browser.RevokeSessionObservationGeometry(bound.Info)
			}
			if fidelity, ok := bound.Core.(browser.ActionFidelityCapable); ok {
				report := fidelity.LastActionFidelity()
				if report.FocusUpdated {
					bound.Info.HumanFocusBackendNodeID = report.FocusedBackend
					bound.Info.HumanFocusPageURL = bound.Info.PageURL
				}
			}
		}
	}
	if err := persistHTTPRuntime(ctx, bound); err != nil {
		bound.Info.LastActionOutcome = browser.SessionActionOutcomeUnknown
		browser.InvalidateSessionObservation(bound.Info)
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "persist", DispatchState: "unknown", Recovery: "observe before further input"}, err)
		return
	}
	if actionErr != nil {
		writeHTTPError(w, http.StatusConflict, outcome, actionErr)
		return
	}
	if action.Verb == "navigate" && s.hooks.OnNavigate != nil {
		if err := s.hooks.OnNavigate(ctx, id, action.URL); err != nil {
			writeHTTPError(w, http.StatusInternalServerError, browser.Outcome{Phase: "hook", DispatchState: "confirmed"}, err)
			return
		}
	}
	writeHTTPResult(w, http.StatusOK, map[string]interface{}{"success": true, "session_id": id, "runtime_run_id": bound.Info.BrowserRunID, "operation_id": operationID, "outcome": outcome})
}

func parseHTTPQueryID(r *http.Request) (string, error) {
	for key := range r.URL.Query() {
		if key != "id" {
			return "", fmt.Errorf("unknown query field %s", key)
		}
	}
	id := r.URL.Query().Get("id")
	return id, validateHTTPID(id)
}
func (s *Server) handleObserve(w http.ResponseWriter, r *http.Request) {
	id, err := parseHTTPQueryID(r)
	if err != nil {
		parameterError(w, err)
		return
	}
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	bound, err := s.resolveRuntime(ctx, id)
	if err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "binding", DispatchState: "not-dispatched"}, err)
		return
	}
	if bound.Info.EvidenceRoot == "" {
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "evidence", DispatchState: "not-dispatched"}, fmt.Errorf("host must configure an evidence root"))
		return
	}
	// Revoke old geometry while retaining prior native focus provenance.
	// BindSessionObservation revalidates that proof against this capture.
	browser.RevokeSessionObservationGeometry(bound.Info)
	if err := persistHTTPRuntime(ctx, bound); err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "persist", DispatchState: "not-dispatched"}, err)
		return
	}
	snap, err := bound.Core.SnapWithSessionMode(ctx, bound.Info.SnapEpoch+1)
	if err != nil || snap == nil || snap.ObservationID == "" {
		if err == nil {
			err = fmt.Errorf("capture identity unavailable")
		}
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "capture", DispatchState: "not-dispatched"}, err)
		return
	}
	if bound.Info.BrowserRunID == "" {
		bound.Info.BrowserRunID = snap.RuntimeRunID
	}
	if bound.Info.TargetID == "" {
		bound.Info.TargetID = snap.TargetID
	}
	if bound.Info.ViewportW == 0 {
		bound.Info.ViewportW = int(snap.Viewport.Width)
	}
	if bound.Info.ViewportH == 0 {
		bound.Info.ViewportH = int(snap.Viewport.Height)
	}
	root, err := filepath.Abs(bound.Info.EvidenceRoot)
	if err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "evidence", DispatchState: "not-dispatched"}, err)
		return
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "evidence", DispatchState: "not-dispatched"}, err)
		return
	}
	path := filepath.Join(root, snap.ObservationID+".json")
	shotPath := filepath.Join(root, snap.ObservationID+".png")
	image, err := bound.Core.Screenshot(ctx, false)
	if err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "screenshot", DispatchState: "not-dispatched"}, err)
		return
	}
	observation := btest.BuildObservation(id, snap, image, shotPath, &btest.BehaviorState{URL: snap.URL, Title: snap.PageTitle, LoadState: snap.LoadState}, nil)
	observation.Scenario = bound.Info.Scenario
	observation.Deterministic = bound.Info.Policy.Deterministic
	observation.BindActionHints(snap, browser.Scenario(bound.Info.Scenario))
	if observation.ID != snap.ObservationID || observation.Visual == nil || observation.Channels["visual"].Status != "captured" {
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "screenshot", DispatchState: "not-dispatched"}, fmt.Errorf("canonical visual capture unavailable"))
		return
	}
	bound.Info.PageURL = snap.URL
	if err := browser.BindSessionObservation(bound.Info, snap, snap.ObservationID, path); err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "binding", DispatchState: "not-dispatched"}, err)
		return
	}
	if err := btest.WriteObservation(path, observation); err != nil {
		browser.InvalidateSessionObservation(bound.Info)
		_ = persistHTTPRuntime(ctx, bound)
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "evidence", DispatchState: "not-dispatched"}, err)
		return
	}
	if err := browser.ApplyObservedSession(bound.Info, snap, browser.SessionRefsFromSnapshot(snap, true)); err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "binding", DispatchState: "not-dispatched"}, err)
		return
	}
	if err := persistHTTPRuntime(ctx, bound); err != nil {
		browser.InvalidateSessionObservation(bound.Info)
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "persist", DispatchState: "not-dispatched"}, err)
		return
	}
	writeHTTPResult(w, http.StatusOK, map[string]interface{}{"schema": "dw.observe.ref.v2", "session_id": id, "observation_id": observation.ID, "observation_path": path, "screenshot": shotPath, "channels": observation.Channels, "runtime_run_id": snap.RuntimeRunID, "target_id": snap.TargetID, "viewport": snap.Viewport})
}
func (s *Server) handleBoundTabs(w http.ResponseWriter, r *http.Request) {
	id, err := parseHTTPQueryID(r)
	if err != nil {
		parameterError(w, err)
		return
	}
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	bound, err := s.resolveRuntime(r.Context(), id)
	if err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "binding", DispatchState: "not-dispatched"}, err)
		return
	}
	tracker := bound.Core.GetTargetTracker()
	if tracker == nil {
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "tabs", DispatchState: "not-dispatched"}, fmt.Errorf("tabs unavailable in bound runtime"))
		return
	}
	if err := tracker.RefreshTargets(); err != nil {
		writeHTTPError(w, http.StatusServiceUnavailable, browser.Outcome{Phase: "tabs", DispatchState: "not-dispatched"}, err)
		return
	}
	writeHTTPResult(w, http.StatusOK, map[string]interface{}{"session_id": id, "tabs": tracker.ListTargets()})
}
