package skills

import (
	"os"
	"path/filepath"
	"testing"
)

const fixtureSkillMD = `---
name: discourse
description: Discourse community forum skill
domain: meta.discourse.org
dependencies:
  - dw-browser
actions: [capture-thread, publish-reply]
verified_at: 2026-01-01
---

# Discourse

## capture-thread
intent: Capture current thread

` + "```dw-browser" + `
snap --session $SESSION
act --session $SESSION "scroll to bottom"
` + "```" + `

- Threads may be paginated; scroll before capture.

## publish-reply
intent: Publish a prepared reply

` + "```dw-browser" + `
act --session $SESSION "click button:'Reply'"
act --session $SESSION "type '$REPLY_TEXT'"
act --session $SESSION "click button:'Submit'"
` + "```" + `

- Requires Human confirmation before submitting.
`

func TestParseRoundtrip(t *testing.T) {
	doc, err := Parse([]byte(fixtureSkillMD))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	if doc.Name != "discourse" {
		t.Errorf("Name=%q, want discourse", doc.Name)
	}
	if doc.Description != "Discourse community forum skill" {
		t.Errorf("Description=%q", doc.Description)
	}
	if doc.Domain != "meta.discourse.org" {
		t.Errorf("Domain=%q, want meta.discourse.org", doc.Domain)
	}
	if doc.VerifiedAt != "2026-01-01" {
		t.Errorf("VerifiedAt=%q, want 2026-01-01", doc.VerifiedAt)
	}
	if len(doc.Dependencies) != 1 || doc.Dependencies[0] != "dw-browser" {
		t.Errorf("Dependencies=%v", doc.Dependencies)
	}
	if len(doc.Actions) != 2 {
		t.Fatalf("Actions len=%d, want 2", len(doc.Actions))
	}

	capture := doc.Actions[0]
	if capture.Name != "capture-thread" {
		t.Errorf("Actions[0].Name=%q, want capture-thread", capture.Name)
	}
	if capture.Intent != "Capture current thread" {
		t.Errorf("capture.Intent=%q", capture.Intent)
	}
	if len(capture.Recipe) != 2 {
		t.Errorf("capture.Recipe len=%d, want 2: %v", len(capture.Recipe), capture.Recipe)
	}
	if len(capture.Gotchas) != 1 {
		t.Errorf("capture.Gotchas len=%d, want 1", len(capture.Gotchas))
	}

	publish := doc.Actions[1]
	if publish.Name != "publish-reply" {
		t.Errorf("Actions[1].Name=%q, want publish-reply", publish.Name)
	}
	if publish.Intent != "Publish a prepared reply" {
		t.Errorf("publish.Intent=%q", publish.Intent)
	}
	if len(publish.Recipe) != 3 {
		t.Errorf("publish.Recipe len=%d, want 3: %v", len(publish.Recipe), publish.Recipe)
	}
}

func TestParseNoFrontmatter(t *testing.T) {
	_, err := Parse([]byte("# Just a plain markdown doc\n\nNo frontmatter.\n"))
	if err == nil {
		t.Fatal("expected error for missing frontmatter, got nil")
	}
}

func TestExtractSection(t *testing.T) {
	body := `
## alpha
intent: do alpha

## beta
intent: do beta
`
	section, found := ExtractSection(body, "alpha")
	if !found {
		t.Fatal("expected alpha section to be found")
	}
	if section == "" {
		t.Fatal("expected non-empty alpha section")
	}

	_, found = ExtractSection(body, "gamma")
	if found {
		t.Fatal("expected gamma section to NOT be found")
	}
}

func TestDeriveDomain(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"https://chatgpt.com/chat", "chatgpt.com"},
		{"https://www.github.com/foo", "github.com"},
		{"chatgpt.com", "chatgpt.com"},
		{"https://meta.discourse.org/t/123", "meta.discourse.org"},
		{"", ""},
	}
	for _, tc := range cases {
		got := DeriveDomain(tc.input)
		if got != tc.want {
			t.Errorf("DeriveDomain(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestResolveGeneric(t *testing.T) {
	// With an empty temp dir as root, any URL should return MatchGeneric
	tmp := t.TempDir()
	b, err := Resolve("https://example.com", tmp)
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if b.Status != MatchGeneric {
		t.Errorf("Status=%q, want generic", b.Status)
	}
	if b.Domain != "example.com" {
		t.Errorf("Domain=%q, want example.com", b.Domain)
	}
}

func TestResolveExact(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "chatgpt.com")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(fixtureSkillMD), 0644); err != nil {
		t.Fatal(err)
	}

	b, err := Resolve("https://chatgpt.com/chat", root)
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if b.Status != MatchExact {
		t.Errorf("Status=%q, want exact", b.Status)
	}
	if b.Domain != "chatgpt.com" {
		t.Errorf("Domain=%q, want chatgpt.com", b.Domain)
	}
	if len(b.Actions) != 2 {
		t.Errorf("Actions len=%d, want 2", len(b.Actions))
	}
}

func TestResolveDiscourseEmbedded(t *testing.T) {
	// Regression guard: meta.discourse.org must resolve from embedded corpus.
	tmp := t.TempDir() // empty disk root — forces embed fallback
	b, err := Resolve("https://meta.discourse.org/t/some-topic/123", tmp)
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if b.Status != MatchExact {
		t.Errorf("meta.discourse.org: Status=%q, want exact (embed hit)", b.Status)
	}
	if b.Domain != "meta.discourse.org" {
		t.Errorf("meta.discourse.org: Domain=%q, want meta.discourse.org", b.Domain)
	}
}

func TestResolveMultiRoot_PrivateOverride(t *testing.T) {
	// Private root has "example-com" skill; public root also has one.
	// Private should win (first-match).
	private := t.TempDir()
	public := t.TempDir()

	writeSkill := func(root, domain, name string) {
		dir := filepath.Join(root, domain)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		content := "---\nname: " + name + "\ndomain: example.com\nactions: [go]\n---\n\n## go\nintent: " + name + "\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	writeSkill(private, "example.com", "private-skill")
	writeSkill(public, "example.com", "public-skill")

	b, err := Resolve("https://example.com", private, public)
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if b.Status != MatchExact {
		t.Errorf("Status=%q, want exact", b.Status)
	}
	// Should come from private root
	if len(b.Actions) == 0 || b.Actions[0].Intent != "private-skill" {
		t.Errorf("expected private skill to win, got actions: %v", b.Actions)
	}
}
