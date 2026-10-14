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
