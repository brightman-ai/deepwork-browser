package dwbrowser

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
	btest "github.com/brightman-ai/deepwork-browser/browser/testing"
)

// This adapter fixture supplies captured facts only; no browser is launched.
type observationContractCore struct {
	browser.SessionCore
	snap *browser.Snapshot
	shot []byte
}

func (*observationContractCore) SetPolicy(browser.SessionPolicy, string) {}
func (c *observationContractCore) SnapWithSessionMode(context.Context, int) (*browser.Snapshot, error) {
	return c.snap, nil
}
func (c *observationContractCore) Screenshot(context.Context, bool) ([]byte, error) {
	return c.shot, nil
}

func TestHTTPObservationRevalidatesNativeFocusAndBindsOwningScenario(t *testing.T) {
	for _, verified := range []bool{true, false} {
		t.Run(map[bool]string{true: "native-focus-retained", false: "focus-moved-revoked"}[verified], func(t *testing.T) {
			var shot bytes.Buffer
			if err := png.Encode(&shot, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
				t.Fatal(err)
			}
			info := &browser.SessionInfo{
				SessionID: "host", Scenario: string(browser.ScenarioAppTestBaseline), AuthorityState: browser.AuthorityAgentic,
				BrowserRunID: "run", TargetID: "target", PageURL: "http://localhost/", EvidenceRoot: t.TempDir(),
				ViewportW: 2, ViewportH: 2, SnapEpoch: 1, Policy: browser.SessionPolicy{Deterministic: true},
				ObservationID: "prior", ObservationDocumentID: 9, ObservationRunID: "run", ObservationTargetID: "target",
				HumanFocusBackendNodeID: 73, HumanFocusEpoch: 1, HumanFocusPageURL: "http://localhost/",
			}
			core := &observationContractCore{shot: shot.Bytes(), snap: &browser.Snapshot{
				ObservationID: "fresh", RuntimeRunID: "run", TargetID: "target", DocumentID: 9, URL: info.PageURL,
				Viewport: browser.ViewportFacts{Width: 2, Height: 2},
				Refs: []browser.ElementRef{{Ref: "@ofresh-r1", BackendNodeID: 73, Role: "textbox", Observed: true,
					VisibilityKnown: true, VisibleInViewport: true, ControlStateKnown: true, Editable: true,
					SelectionSupported: true, HumanFocusVerified: verified}},
			}}
			server, err := NewServer(WithRuntimeResolver(func(context.Context, string) (BoundRuntime, error) {
				return BoundRuntime{Core: core, Info: info}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/observe?id=host", nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if (info.HumanFocusBackendNodeID == 73) != verified {
				t.Errorf("focus provenance disagrees with actual captured focus: %+v", info)
			}
			var projection struct {
				Path string `json:"observation_path"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &projection); err != nil {
				t.Fatal(err)
			}
			obs, err := btest.LoadObservation(projection.Path)
			if err != nil {
				t.Fatal(err)
			}
			if obs.Scenario != info.Scenario || !obs.Deterministic {
				t.Errorf("owning scenario/policy missing: scenario=%q deterministic=%t", obs.Scenario, obs.Deterministic)
			}
			if !slices.Contains(obs.Structural.Refs[0].AllowedActions, "fill") {
				t.Errorf("actual captured editable control lost fill hint: %+v", obs.Structural.Refs[0])
			}
		})
	}
}
