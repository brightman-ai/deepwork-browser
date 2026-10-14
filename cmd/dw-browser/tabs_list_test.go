package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func TestTabsListReportsOnlyPageTargetsFromDevToolsList(t *testing.T) {
	page := map[string]interface{}{"type": "page", "id": "article-page", "url": "http://localhost/article", "title": "Article", "webSocketDebuggerUrl": "ws://localhost/devtools/page/article-page"}
	blank := map[string]interface{}{"type": "page", "id": "blank-page", "url": "about:blank", "title": ""}
	transient := []map[string]interface{}{
		{"type": "browser_ui", "id": "browser-ui", "url": "chrome://browser-ui/"},
		{"type": "service_worker", "id": "extension-worker", "url": "chrome-extension://extension/worker.js"},
		{"type": "iframe", "id": "iframe", "url": "http://localhost/frame"},
	}
	for _, tc := range []struct {
		name      string
		raw, want []map[string]interface{}
	}{
		{"mixed includes blank page", append(append([]map[string]interface{}{}, transient...), page, blank), []map[string]interface{}{page, blank}},
		{"no pages", transient, []map[string]interface{}{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/json/list", http.StatusTemporaryRedirect)
			})
			mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(tc.raw); err != nil {
					t.Error(err)
				}
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			_, portText, err := net.SplitHostPort(server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			port, err := strconv.Atoi(portText)
			if err != nil {
				t.Fatal(err)
			}
			id := "tabs-list-fixture-" + browser.NewObservationID()
			info := &browser.SessionInfo{SessionID: id, DebugPort: port, TargetID: "article-page", BrowserRunID: "fixture-run", Mode: browser.ModeHeadless}
			if err := browser.SaveSession(info); err != nil {
				t.Fatal(err)
			}
			defer browser.DeleteSession(id)
			result, err := runTabsListResult([]string{"--id", id})
			if err != nil {
				t.Fatal(err)
			}
			if result.Code != exitOK {
				t.Fatalf("exit code %d", result.Code)
			}
			payload, ok := result.Payload.(map[string]any)
			if !ok {
				t.Fatalf("unexpected payload %T", result.Payload)
			}
			if !reflect.DeepEqual(payload["targets"], tc.want) {
				t.Fatalf("page output = %#v, want %#v", payload["targets"], tc.want)
			}
			if payload["current_target_id"] != "article-page" || payload["runtime_run_id"] != "fixture-run" {
				t.Fatalf("session identity lost: %#v", payload)
			}
			raw, err := browser.FetchChromeTargets(port)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(raw, tc.raw) {
				t.Fatal("diagnostic raw target API was filtered")
			}
		})
	}
}
