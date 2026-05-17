package browser

import (
	"encoding/json"
	"testing"
)

func TestSessionAuthorityUpdateTargetInfoAllowsClearingURLAndTitle(t *testing.T) {
	a := NewSessionAuthority(1920, 1080)
	a.UpdateTargetInfo("https://chatgpt.com", "ChatGPT", 2)
	a.UpdateTargetInfo("", "", 1)

	snap := a.GetState()
	if snap.URL != "" {
		t.Fatalf("expected URL to be cleared, got %q", snap.URL)
	}
	if snap.Title != "" {
		t.Fatalf("expected Title to be cleared, got %q", snap.Title)
	}
	if snap.TargetCount != 1 {
		t.Fatalf("expected TargetCount=1, got %d", snap.TargetCount)
	}
}

func TestSessionAuthorityCopiesTabsOnWriteAndRead(t *testing.T) {
	a := NewSessionAuthority(1920, 1080)
	tabs := []TabInfo{
		{ID: "tab-a", URL: "https://a.example/", Title: "A", Active: true},
	}
	a.UpdateTargetInfo("https://a.example/", "A", 1, tabs)

	tabs[0].Title = "mutated input"
	snap := a.GetState()
	if snap.Tabs[0].Title != "A" {
		t.Fatalf("state tabs polluted by caller mutation: %q", snap.Tabs[0].Title)
	}

	snap.Tabs[0].Title = "mutated snapshot"
	snap2 := a.GetState()
	if snap2.Tabs[0].Title != "A" {
		t.Fatalf("state tabs polluted by snapshot mutation: %q", snap2.Tabs[0].Title)
	}
}

func TestTabInfoJSONIncludesNonClosableFalse(t *testing.T) {
	body, err := json.Marshal(TabInfo{ID: "root", Title: "Root", URL: "https://example.test/", Active: true, Closable: false})
	if err != nil {
		t.Fatalf("Marshal(TabInfo): %v", err)
	}
	if !json.Valid(body) {
		t.Fatalf("TabInfo JSON is invalid: %s", string(body))
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("Unmarshal(TabInfo): %v", err)
	}
	value, ok := payload["closable"].(bool)
	if !ok || value {
		t.Fatalf("closable=false must be explicit in JSON, got %s", string(body))
	}
}
