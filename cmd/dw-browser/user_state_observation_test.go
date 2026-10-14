package main

import (
	"github.com/brightman-ai/deepwork-browser/browser"
	"testing"
)

func TestUserStateActiveTabURLUsesCanonicalCapture(t *testing.T) {
	snap := &browser.Snapshot{URL: "http://localhost/editor#saved", PageTitle: "Saved editor"}
	state := buildUserState(snap)
	if state["active_tab_url"] != snap.URL || state["url"] != snap.URL {
		t.Fatalf("projection diverged from captured URL: %#v", state)
	}
}
