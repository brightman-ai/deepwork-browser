package main

import (
	"strings"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func TestAttachedRuntimeIdentityGuardMatrix(t *testing.T) {
	info := &browser.SessionInfo{RuntimeID: "rt-1", BrowserMuxHostID: "mux-1", BrowserSessionID: "bs-1", WSURL: "ws://127.0.0.1/devtools", BrowserRunID: "run-1", ChromePID: 123, DisplayID: 7, Mode: browser.ModeHeaded}
	state := &browser.BrowserMuxHostState{RuntimeID: "rt-1", MuxHostID: "mux-1", BrowserSessionID: "bs-1", WSURL: info.WSURL, BrowserRunID: "run-1", ChromePID: 123, DisplayID: 7, Mode: browser.ModeHeaded}
	if err := validateAttachedRuntimeIdentity(info, state); err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name string
		edit func(*browser.BrowserMuxHostState)
	}{
		{"runtime", func(s *browser.BrowserMuxHostState) { s.RuntimeID = "rt-2" }},
		{"mux", func(s *browser.BrowserMuxHostState) { s.MuxHostID = "mux-2" }},
		{"session", func(s *browser.BrowserMuxHostState) { s.BrowserSessionID = "bs-2" }},
		{"ws", func(s *browser.BrowserMuxHostState) { s.WSURL = "ws://other" }},
		{"run", func(s *browser.BrowserMuxHostState) { s.BrowserRunID = "run-2" }},
		{"pid", func(s *browser.BrowserMuxHostState) { s.ChromePID = 456 }},
		{"display", func(s *browser.BrowserMuxHostState) { s.DisplayID = 8 }},
		{"mode", func(s *browser.BrowserMuxHostState) { s.Mode = browser.ModeVisible }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			copy := *state
			mutation.edit(&copy)
			if err := validateAttachedRuntimeIdentity(info, &copy); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
				t.Fatalf("mutation accepted: %v", err)
			}
		})
	}
	for _, name := range []string{"runtime_id", "browser_mux_host_id", "ws_url"} {
		copy := *info
		switch name {
		case "runtime_id":
			copy.RuntimeID = ""
		case "browser_mux_host_id":
			copy.BrowserMuxHostID = ""
		case "ws_url":
			copy.WSURL = ""
		}
		if err := validateAttachedRuntimeIdentity(&copy, state); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("missing %s accepted: %v", name, err)
		}
	}
}

func TestAttachedRuntimeIdentityGuardRequiresAttachFacts(t *testing.T) {
	state := &browser.BrowserMuxHostState{RuntimeID: "rt-1", MuxHostID: "mux-1", BrowserSessionID: "bs-1", WSURL: "ws://127.0.0.1/devtools", BrowserRunID: "run-1", ChromePID: 123, DisplayID: 7, Mode: browser.ModeHeaded}
	for name, mutate := range map[string]func(*browser.SessionInfo){
		"runtime": func(i *browser.SessionInfo) { i.RuntimeID = "" },
		"mux":     func(i *browser.SessionInfo) { i.BrowserMuxHostID = "" },
		"ws":      func(i *browser.SessionInfo) { i.WSURL = "" },
	} {
		t.Run(name, func(t *testing.T) {
			info := &browser.SessionInfo{RuntimeID: "rt-1", BrowserMuxHostID: "mux-1", BrowserSessionID: "bs-1", WSURL: state.WSURL, BrowserRunID: "run-1", ChromePID: 123, DisplayID: 7, Mode: browser.ModeHeaded}
			mutate(info)
			if err := validateAttachedRuntimeIdentity(info, state); err == nil || !strings.Contains(err.Error(), "missing") {
				t.Fatalf("missing attach fact accepted: %v", err)
			}
		})
	}
}
