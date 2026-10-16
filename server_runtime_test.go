package dwbrowser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
	"github.com/brightman-ai/deepwork-browser/browser/safari"
)

func TestHTTPRejectsMalformedBeforeResolvingRuntime(t *testing.T) {
	calls := 0
	server, err := NewServer(WithRuntimeResolver(func(context.Context, string) (BoundRuntime, error) {
		calls++
		return BoundRuntime{}, fmt.Errorf("unavailable")
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, body string }{
		{"/act", `{"id":"x","action":"fill @r1 'old'"}`},
		{"/act", `{"id":"x","action":{"kind":"fill","target":{"kind":"ref","ref":"@ofixture-r1"}}}`},
		{"/act", `{"id":"x","action":{"kind":"click","target":{"kind":"point","point":{"x":1,"y":2,"unit":"css-px","frame":"viewport"}}}}`},
		{"/act", `{"id":"x","action":{"kind":"reload","count":0}}`},
		{"/act", `{"id":"x","action":{"kind":"reload"},"scenario":"webvisit"}`},
		{"/navigate", `{"id":"x","url":"javascript:document.body.remove()"}`},
		{"/navigate", `{"url":"https://example.com"}`},
		{"/navigate", `{"id":"x","url":"https://example.com"} {}`},
	} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s code=%d body=%s", tc.path, recorder.Code, recorder.Body.String())
		}
		var result map[string]interface{}
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatalf("malformed requests resolved runtime %d times", calls)
	}
}
func TestHTTPUnavailableRuntimeIsNeverSuccessfulEmptyTabs(t *testing.T) {
	server, err := NewServer()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, body string }{{"GET", "/tabs?id=x", ""}, {"GET", "/observe?id=x", ""}, {"POST", "/navigate", `{"id":"x","url":"https://example.com"}`}, {"POST", "/act", `{"id":"x","action":{"kind":"reload"}}`}} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s code=%d body=%s", tc.path, recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), `"tabs":[]`) || strings.Contains(recorder.Body.String(), `"success":true`) {
			t.Fatalf("unavailable runtime fabricated success %s", recorder.Body.String())
		}
	}
}

// Uses the actual Safari adapter with no driver, not a mocked browser. The
// recovery branch must return before dispatch or persistence; real bound-core
// delivery/recovery is verified separately by the coordinator's live harness.
func TestHTTPPriorUncertaintyCannotBeOverwrittenByNewFence(t *testing.T) {
	for _, state := range []string{browser.SessionActionOutcomeUnknown, browser.SessionActionOutcomeInProgress} {
		writes := 0
		info := &browser.SessionInfo{SessionID: "x", Scenario: string(browser.ScenarioAppTestExplore), AuthorityState: browser.AuthorityAgentic, LastActionOutcome: state, LastActionOperationID: "prior-operation"}
		server, err := NewServer(WithRuntimeResolver(func(context.Context, string) (BoundRuntime, error) {
			return BoundRuntime{Core: &safari.SafariBrowserCore{}, Info: info, Persist: func(context.Context, *browser.SessionInfo) error { writes++; return nil }}, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/act", strings.NewReader(`{"id":"x","action":{"kind":"type","focused":true,"text":"must not insert"}}`)))
		if recorder.Code != http.StatusConflict {
			t.Fatalf("state=%s code=%d body=%s", state, recorder.Code, recorder.Body.String())
		}
		if writes != 0 || info.LastActionOutcome != state || info.LastActionOperationID != "prior-operation" {
			t.Fatalf("new request erased prior fence: %+v writes=%d", info, writes)
		}
		if !strings.Contains(recorder.Body.String(), `"phase":"recovery"`) || !strings.Contains(recorder.Body.String(), `"dispatch_state":"not-dispatched"`) {
			t.Fatalf("missing safe outcome %s", recorder.Body.String())
		}
	}
}

// The HTTP adapter borrows this host-owned fixture; only its core action facts
// are supplied. No browser or driver is launched by these projection tests.
type actionEvidenceCore struct {
	browser.SessionCore
	outcome          browser.Outcome
	actionErr        error
	executed, closed int
}

func (*actionEvidenceCore) SetPolicy(browser.SessionPolicy, string)     {}
func (*actionEvidenceCore) RestoreRefsFromSession([]browser.SessionRef) {}
func (c *actionEvidenceCore) ExecuteAction(context.Context, browser.Action, bool, bool, browser.InteractionMode) (*browser.Snapshot, browser.Outcome, error) {
	c.executed++
	return nil, c.outcome, c.actionErr
}
func (c *actionEvidenceCore) Close(context.Context) error { c.closed++; return nil }

type actionEvidenceCapableCore struct {
	*actionEvidenceCore
	report browser.ActionFidelityReport
	reads  int
}

func (c *actionEvidenceCapableCore) LastActionFidelity() browser.ActionFidelityReport {
	c.reads++
	return c.report
}
func (*actionEvidenceCapableCore) RestoreHumanFocus(browser.HumanFocusState) {}

func TestHTTPActionPreservesCoreEvidenceAcrossTerminalPaths(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		actionErr                      bool
		persistAt                      int
		hookFailure, withoutCapability bool
		status                         int
		phase                          string
	}{
		{name: "native-select-success", status: http.StatusOK, phase: "complete"},
		{name: "partial-select-failure", actionErr: true, status: http.StatusConflict, phase: "selection"},
		{name: "post-action-persist-failure", persistAt: 2, status: http.StatusServiceUnavailable, phase: "persist"},
		{name: "post-action-hook-failure", hookFailure: true, status: http.StatusInternalServerError, phase: "hook"},
		{name: "no-capability", withoutCapability: true, status: http.StatusOK, phase: "complete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &actionEvidenceCore{outcome: browser.Outcome{Phase: "complete", DispatchState: "confirmed"}}
			if tc.actionErr {
				base.actionErr = fmt.Errorf("select target replaced")
				base.outcome = browser.Outcome{Phase: "selection", DispatchState: "unknown", Recovery: "observe before further input"}
			}
			report := browser.ActionFidelityReport{Fidelity: browser.InteractionFidelityStrictHuman, InteractionMode: browser.InteractionModeVisual, Execution: "native-keys", Selection: &browser.SelectionEvidence{Before: browser.SelectionState{Index: 0, Value: "before"}, After: browser.SelectionState{Index: 1, Value: "after"}, Verified: !tc.actionErr, Partial: tc.actionErr, KeySteps: 2}}
			if !tc.actionErr {
				report.EvidenceScope = "keyboard-selection"
			}
			capable := &actionEvidenceCapableCore{actionEvidenceCore: base, report: report}
			var core browser.SessionCore = capable
			if tc.withoutCapability {
				core = base
			}
			info := &browser.SessionInfo{SessionID: "host", BrowserRunID: "run", Scenario: string(browser.ScenarioAppTestExplore), AuthorityState: browser.AuthorityAgentic}
			writes := 0
			server, err := NewServer(WithRuntimeResolver(func(context.Context, string) (BoundRuntime, error) {
				return BoundRuntime{Core: core, Info: info, Persist: func(context.Context, *browser.SessionInfo) error {
					writes++
					if writes == tc.persistAt {
						return fmt.Errorf("store unavailable")
					}
					return nil
				}}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			body := `{"id":"host","action":{"kind":"select","target":{"kind":"locator","locator":"css=#choice"},"value":"after"}}`
			if tc.hookFailure {
				server.hooks.OnNavigate = func(context.Context, string, string) error { return fmt.Errorf("hook failed") }
				body = `{"id":"host","action":{"kind":"navigate","url":"https://example.test"}}`
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/act", strings.NewReader(body)))
			if response.Code != tc.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var payload struct {
				SessionID    string                        `json:"session_id"`
				RuntimeRunID string                        `json:"runtime_run_id"`
				OperationID  string                        `json:"operation_id"`
				Outcome      browser.Outcome               `json:"outcome"`
				Fidelity     *browser.ActionFidelityReport `json:"action_fidelity"`
			}
			if err = json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.SessionID != "host" || payload.RuntimeRunID != "run" || payload.OperationID == "" || payload.Outcome.Phase != tc.phase {
				t.Fatalf("lost action identity/outcome: %s", response.Body.String())
			}
			if tc.withoutCapability {
				if payload.Fidelity != nil {
					t.Fatal("adapter fabricated action fidelity")
				}
			} else {
				expected, _ := json.Marshal(report)
				actual, _ := json.Marshal(payload.Fidelity)
				if string(expected) != string(actual) || capable.reads != 1 {
					t.Fatalf("changed or reread core evidence: reads=%d body=%s", capable.reads, response.Body.String())
				}
				if !strings.Contains(response.Body.String(), `"requested_fidelity":"strict-human"`) || strings.Contains(response.Body.String(), `"fidelity":`) {
					t.Fatalf("policy mislabeled: %s", response.Body.String())
				}
			}
			if base.executed != 1 || base.closed != 0 {
				t.Fatalf("host-owned core execution=%d close=%d", base.executed, base.closed)
			}
		})
	}
}

func TestHTTPPreflightDoesNotReadPreviousActionEvidence(t *testing.T) {
	for _, fenceFails := range []bool{false, true} {
		base := &actionEvidenceCore{}
		capable := &actionEvidenceCapableCore{actionEvidenceCore: base, report: browser.ActionFidelityReport{Execution: "previous-action"}}
		state := browser.SessionActionOutcomeUnknown
		if fenceFails {
			state = ""
		}
		info := &browser.SessionInfo{SessionID: "host", Scenario: string(browser.ScenarioAppTestExplore), AuthorityState: browser.AuthorityAgentic, LastActionOutcome: state}
		server, err := NewServer(WithRuntimeResolver(func(context.Context, string) (BoundRuntime, error) {
			return BoundRuntime{Core: capable, Info: info, Persist: func(context.Context, *browser.SessionInfo) error { return fmt.Errorf("fence unavailable") }}, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/act", strings.NewReader(`{"id":"host","action":{"kind":"reload"}}`)))
		if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "action_fidelity") || capable.reads != 0 || base.executed != 0 || base.closed != 0 {
			t.Fatalf("preflight inherited old action evidence: %s reads=%d", response.Body.String(), capable.reads)
		}
	}
}
