package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	dwb "github.com/brightman-ai/deepwork-browser"
	"github.com/brightman-ai/deepwork-browser/browser"
	btest "github.com/brightman-ai/deepwork-browser/browser/testing"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func main() {
	if len(os.Args) != 2 {
		panic("evidence directory required")
	}
	evidence, err := filepath.Abs(os.Args[1])
	must(err)
	must(os.MkdirAll(evidence, 0755))
	var receipts []string
	var receiptsMu sync.Mutex
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			data, err := io.ReadAll(r.Body)
			must(err)
			receiptsMu.Lock()
			receipts = append(receipts, string(data))
			receiptsMu.Unlock()
			w.WriteHeader(204)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>HTTP real editor</title><style>body{font:20px system-ui;margin:30px}label,textarea{display:block}textarea{width:500px;height:100px;font:inherit}button{font:inherit;margin:20px}</style><h1>HTTP real editor</h1><label for="draft">Draft note</label><textarea id="draft">nonempty original</textarea><button onclick="fetch('/receipt',{method:'POST',body:document.getElementById('draft').value});document.getElementById('saved').textContent=document.getElementById('draft').value">Save note</button><pre id="saved"></pre>`)
		fmt.Fprint(w, `<label for="private">Password</label><input id="private" type="password" value="intrinsic-private-sentinel"><div hidden data-testid="hidden-control">Hidden control</div>`)
		fmt.Fprint(w, `<div id="pair"><button id="first" onclick="window.lastPair='first'">Same action</button><button id="second" onclick="window.lastPair='second'">Same action</button></div><button onclick="const p=document.getElementById('pair');p.insertBefore(p.lastElementChild,p.firstElementChild)">Swap order</button>`)
	}))
	defer fixture.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	core, err := browser.NewBrowserCore(ctx, fmt.Sprintf("parent-http-%d", time.Now().UnixNano()), browser.WithMode(browser.ModeHeadless), browser.WithViewport(1280, 800))
	must(err)
	defer core.Close(context.Background())
	session := core.(browser.SessionCore)
	core.(browser.ScenarioInteractionCapable).SetInteractionScenario(browser.ScenarioAppTestExplore)
	core.SetPolicy(browser.SessionPolicy{RemoteWrites: browser.RemoteWriteDeny}, fixture.URL)
	_, _, err = core.ExecuteAction(ctx, browser.Action{Verb: "navigate", URL: fixture.URL}, false, true, browser.InteractionModeVisual)
	must(err)
	initial, err := session.SnapWithSessionMode(ctx, 1)
	must(err)
	info := &browser.SessionInfo{SessionID: "bound-host", BrowserSessionID: "bound-host", BrowserRunID: initial.RuntimeRunID, TargetID: initial.TargetID, PageURL: initial.URL, Scenario: string(browser.ScenarioAppTestExplore), AuthorityState: browser.AuthorityAgentic, Mode: browser.ModeHeadless, ViewportW: 1280, ViewportH: 800, EvidenceRoot: filepath.Join(evidence, "observations"), Policy: browser.SessionPolicy{RemoteWrites: browser.RemoteWriteDeny}}
	persistCount := 0
	failAt := 0
	server, err := dwb.NewServer(dwb.WithRuntimeResolver(func(_ context.Context, id string) (dwb.BoundRuntime, error) {
		if id != info.SessionID {
			return dwb.BoundRuntime{}, fmt.Errorf("unknown host session")
		}
		return dwb.BoundRuntime{Core: session, Info: info, State: func(context.Context) (*browser.SessionInfo, error) { copy := *info; return &copy, nil }, Persist: func(_ context.Context, next *browser.SessionInfo) error {
			persistCount++
			if failAt == persistCount {
				return fmt.Errorf("actual host persistence unavailable")
			}
			copy := *next
			info = &copy
			return nil
		}}, nil
	}))
	must(err)
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	var records []map[string]any
	call := func(label, method, path string, payload any, want int) map[string]any {
		var data []byte
		if payload != nil {
			data, err = json.Marshal(payload)
			must(err)
		}
		req, err := http.NewRequestWithContext(ctx, method, httpServer.URL+path, bytes.NewReader(data))
		must(err)
		req.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(req)
		must(err)
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		must(err)
		var result map[string]any
		must(json.Unmarshal(body, &result))
		records = append(records, map[string]any{"label": label, "http_status": response.StatusCode, "body": result})
		encoded, err := json.MarshalIndent(records, "", "  ")
		must(err)
		must(os.WriteFile(filepath.Join(evidence, "results.json"), encoded, 0600))
		fmt.Printf("%s status=%d\n", label, response.StatusCode)
		if response.StatusCode != want {
			panic(string(body))
		}
		return result
	}
	observe := func(label string) *btest.Observation {
		reply := call(label, "GET", "/observe?id=bound-host", nil, 200)
		obs, err := btest.LoadObservation(reply["observation_path"].(string))
		must(err)
		return obs
	}
	ref := func(obs *btest.Observation, role, name string) string {
		for _, entry := range obs.Structural.Refs {
			if entry.Role == role && entry.Name == name {
				return entry.Ref
			}
		}
		panic("missing visible target: " + name)
	}
	act := func(label string, action browser.Action, want int) map[string]any {
		return call(label, "POST", "/act", map[string]any{"id": "bound-host", "action": action}, want)
	}
	first := observe("before")
	hub, err := core.StartLiveView(ctx)
	must(err)
	frames := hub.Subscribe("runtime-v2-acceptance")
	// Screencast is event-driven. Exercise a real visible caret update after
	// subscribing, rather than relying on an initial frame racing subscription.
	act("liveview-native-focus", browser.Action{Verb: "focus", Target: browser.Target{Kind: "ref", Ref: ref(first, "textbox", "Draft note")}}, 200)
	select {
	case frame := <-frames:
		if frame == nil || len(frame.Data) == 0 {
			panic("actual LiveView delivered no frame")
		}
		frameReceipt, err := json.Marshal(map[string]any{"frame_bytes": len(frame.Data), "timestamp": frame.Timestamp})
		must(err)
		must(os.WriteFile(filepath.Join(evidence, "liveview-frame.json"), frameReceipt, 0600))
	case <-time.After(5 * time.Second):
		panic("actual LiveView frame unavailable")
	}
	hub.Unsubscribe("runtime-v2-acceptance", frames)
	must(core.StopLiveView(ctx))
	first = observe("after-liveview-focus")
	var privateFact map[string]any
	must(core.EvalJS(ctx, browser.ControlProbeReadScript("input[type=password]", false), &privateFact))
	if privateFact["count"] != float64(1) || privateFact["value"] != nil || privateFact["text"] != nil {
		panic("actual fixed probe exposed private value or fabricated empty")
	}
	privacyResult := (&btest.AssertionEngine{}).Evaluate(first, "text_contains('intrinsic-private-sentinel')")
	if privacyResult.Status != btest.StatusBlocked {
		panic("redacted document miss was treated as proven failure")
	}
	visibilityResult := (&btest.AssertionEngine{}).Evaluate(first, "visible(testid='hidden-control')")
	if visibilityResult.Status == btest.StatusPass {
		panic("document presence impersonated visible evidence")
	}
	privacyData, err := json.MarshalIndent(map[string]any{"fixed_probe": privateFact, "redacted_text": privacyResult, "hidden_visibility": visibilityResult}, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(evidence, "privacy-probe-results.json"), privacyData, 0600))
	draft := ref(first, "textbox", "Draft note")
	want := "HTTP 📌 'quote' \"double\"\nlast line\n"
	act("fill", browser.Action{Verb: "fill", Target: browser.Target{Kind: "ref", Ref: draft}, Text: want}, 200)
	after := observe("after-fill")
	act("save", browser.Action{Verb: "click", Target: browser.Target{Kind: "ref", Ref: ref(after, "button", "Save note")}}, 200)
	savedView := observe("after-save")
	if savedView.Scenario != string(browser.ScenarioAppTestExplore) || savedView.Deterministic {
		panic("HTTP canonical owning scenario mismatch")
	}
	act("focus", browser.Action{Verb: "focus", Target: browser.Target{Kind: "ref", Ref: ref(savedView, "textbox", "Draft note")}}, 200)
	focusedView := observe("focus-observed")
	if info.HumanFocusBackendNodeID == 0 {
		panic("HTTP observe lost verified native focus")
	}
	act("observed-key", browser.Action{Verb: "press", Target: browser.Target{Kind: "ref", Ref: ref(focusedView, "textbox", "Draft note")}, Key: "End"}, 200)
	observe("key-observed")
	act("observed-focused-type", browser.Action{Verb: "type", Focused: true, Text: " HTTP focus."}, 200)
	var focusedValue string
	must(core.EvalJS(ctx, "document.getElementById('draft').value", &focusedValue))
	if !strings.Contains(focusedValue, " HTTP focus.") {
		panic("HTTP native focused insertion missing")
	}
	observe("focused-type-observed")
	receiptsMu.Lock()
	savedExactly := len(receipts) == 1 && receipts[0] == want
	receiptsMu.Unlock()
	if !savedExactly {
		panic("actual save receipt mismatch")
	}
	pairBefore := observe("pair-before")
	var pairRefs []btest.RefSummary
	for _, entry := range pairBefore.Structural.Refs {
		if entry.Role == "button" && entry.Name == "Same action" {
			pairRefs = append(pairRefs, entry)
		}
	}
	if len(pairRefs) != 2 {
		panic("pair fixture has no two captured buttons")
	}
	act("swap-order", browser.Action{Verb: "click", Target: browser.Target{Kind: "ref", Ref: ref(pairBefore, "button", "Swap order")}}, 200)
	pairAfter := observe("pair-after")
	newPairRef := ref(pairAfter, "button", "Same action")
	if newPairRef == pairRefs[0].Ref {
		panic("capture token aliased after reorder")
	}
	stalePair := act("swapped-stale-ref", browser.Action{Verb: "click", Target: browser.Target{Kind: "ref", Ref: pairRefs[0].Ref}}, 409)
	if stalePair["outcome"].(map[string]any)["dispatch_state"] != "not-dispatched" {
		panic("reordered stale ref dispatched")
	}
	var pairValue string
	must(core.EvalJS(ctx, "window.lastPair || ''", &pairValue))
	if pairValue != "" {
		panic("stale pair caused input")
	}
	act("swapped-current-ref", browser.Action{Verb: "click", Target: browser.Target{Kind: "ref", Ref: newPairRef}}, 200)
	must(core.EvalJS(ctx, "window.lastPair || ''", &pairValue))
	if pairValue != "second" {
		panic("new observation addressed wrong reordered button")
	}
	one := observe("identity-one")
	two := observe("identity-two")
	if ref(one, "textbox", "Draft note") == ref(two, "textbox", "Draft note") {
		panic("refs reused")
	}
	result := act("stale-ref", browser.Action{Verb: "fill", Target: browser.Target{Kind: "ref", Ref: ref(one, "textbox", "Draft note")}, Text: "wrong"}, 409)
	if result["outcome"].(map[string]any)["dispatch_state"] != "not-dispatched" {
		panic("stale ref dispatch state")
	}
	must(browser.SetSessionAuthority(info, browser.AuthorityHuman))
	act("human-takeover", browser.Action{Verb: "fill", Target: browser.Target{Kind: "ref", Ref: ref(two, "textbox", "Draft note")}, Text: "wrong"}, 409)
	must(browser.SetSessionAuthority(info, browser.AuthorityAgentic))
	current := observe("yield-observe")
	// Fail the host's post-dispatch save, retaining the in-progress fence.
	failAt = persistCount + 2
	result = act("persist-after-dispatch", browser.Action{Verb: "fill", Target: browser.Target{Kind: "ref", Ref: ref(current, "textbox", "Draft note")}, Text: "delivered but persistence failed"}, 503)
	if result["outcome"].(map[string]any)["dispatch_state"] != "unknown" {
		panic("post-input persist failure not unknown")
	}
	_, directOutcome, directErr := core.ExecuteAction(ctx, browser.Action{Verb: "type", Focused: true, Text: "must not insert direct"}, false, true, browser.InteractionModeVisual)
	var directValue string
	probeErr := core.EvalJS(ctx, "document.getElementById('draft').value", &directValue)
	directRecord := map[string]any{"outcome": directOutcome, "error": fmt.Sprint(directErr), "inspector_error": fmt.Sprint(probeErr), "value": directValue}
	directData, jsonErr := json.MarshalIndent(directRecord, "", "  ")
	must(jsonErr)
	must(os.WriteFile(filepath.Join(evidence, "direct-uncertainty-result.json"), directData, 0600))
	fmt.Printf("direct uncertainty phase=%s state=%s error=%v\n", directOutcome.Phase, directOutcome.DispatchState, directErr)
	if directErr == nil || directOutcome.DispatchState != "not-dispatched" {
		panic("direct Go reused completed operation token across unknown persistence")
	}
	if probeErr != nil || directValue != "delivered but persistence failed" {
		panic("direct Go protected original value mismatch")
	}
	failAt = 0
	act("unknown-fence", browser.Action{Verb: "type", Focused: true, Text: "must not insert"}, 409)
	observe("unknown-reconciled")
	call("tabs", "GET", "/tabs?id=bound-host", nil, 200)
	call("javascript-navigation", "POST", "/navigate", map[string]any{"id": "bound-host", "url": "javascript:alert(1)"}, 400)
	call("unknown-action-field", "POST", "/act", map[string]any{"id": "bound-host", "action": map[string]any{"kind": "fill", "target": map[string]any{"kind": "ref", "ref": draft}, "text": "bad", "unexpected": true}}, 400)
	// Direct Go callers use the same provider guard for non-pointer operations.
	must(browser.SetSessionAuthority(info, browser.AuthorityHuman))
	_, outcome, err := core.ExecuteAction(ctx, browser.Action{Verb: "navigate", URL: fixture.URL + "/forbidden"}, false, true, browser.InteractionModeVisual)
	if err == nil || outcome.DispatchState != "not-dispatched" {
		panic("direct Go navigation bypassed Human provider")
	}
	must(browser.SetSessionAuthority(info, browser.AuthorityAgentic))
	info.LastActionOutcome = browser.SessionActionOutcomeUnknown
	_, outcome, err = core.ExecuteAction(ctx, browser.Action{Verb: "reload"}, false, true, browser.InteractionModeVisual)
	if err == nil || outcome.DispatchState != "not-dispatched" {
		panic("direct Go reload bypassed unknown provider")
	}
	observe("direct-provider-reconciled")
	// Borrowed runtime lifetime remains with its host after server closure.
	must(server.Close())
	_, err = session.SnapWithSessionMode(ctx, 20)
	must(err)
	must(os.WriteFile(filepath.Join(evidence, "success.json"), []byte(`{"verified":true,"real_core":true,"borrowed_lifetime_preserved":true}`), 0600))
}
