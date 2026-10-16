package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
	btest "github.com/brightman-ai/deepwork-browser/browser/testing"
	"gopkg.in/yaml.v3"
)

func validateDeclaredRequest(args []string) error {
	d, a, e := lookupCommand(args)
	if e != nil {
		return e
	}
	return validateCommandArgs(d, a)
}
func TestCommandContractPreservesAdvancedCapabilitiesAndScopes(t *testing.T) {
	valid := [][]string{
		{"open", "http://localhost", "--id", "task", "--scenario", "app-test-explore", "--mode", "headless"},
		{"observe", "--id", "task", "--content", "table", "--health", "--verbose"},
		{"cookie-import", "--id", "task", "--browser", "chrome", "--domain", "example.com"},
		{"audit", "--id", "task", "--suite", "a11y", "--check", "focus-appearance", "--threshold", "85", "--format", "json"},
		{"act", "fill", "css=textarea", "--id", "task", "--text", ""},
		{"journey", "--file", "test.yaml", "--id", "task"},
		{"journey", "--file", "test.yaml", "--scenario", "app-test-baseline", "--viewport", "1280x720"},
		{"wait", "--id", "task", "--assert", "text_contains('saved')", "--timeout", "5s"},
		{"wait", "--id", "task", "--duration", "50ms"},
	}
	for _, args := range valid {
		if e := validateDeclaredRequest(args); e != nil {
			t.Fatalf("supported contract rejected %v: %v", args, e)
		}
	}
	invalid := [][]string{
		{"open", "http://localhost", "--scenario", "app-test-explore"},
		{"open", "http://localhost", "--id", "task", "--scenario", "app-test-explore", "--mode", "human"},
		{"observe", "--id", "task", "--viewport", "880x720"},
		{"observe", "--id", "task", "--evidence", "elsewhere"},
		{"observe", "--id", "task", "--content", "unknown"},
		{"cookie-import", "--id", "task", "--file", "cookies.json"},
		{"audit", "--id", "task", "--format", "unknown"},
		{"act", "fill", "css=textarea", "--id", "task"},
		{"act", "type", "--focused", "--id", "task"},
		{"act", "fill", "css=textarea", "--id", "task", "--text", "x", "--text-file", "x.txt"},
		{"act", "click", "--id", "task", "--x", "0"},
		{"act", "click", "--id", "task", "--unit", "css-px"},
		{"journey", "--file", "test.yaml", "--id", "task", "--scenario", "app-test-baseline"},
		{"wait", "--id", "task", "--assert", "exists(role='button')"},
		{"wait", "--id", "task", "--duration", "50ms", "--timeout", "5s"},
		{"session", "start", "--id", "task"}, {"session", "close", "--id", "task"}, {"htr", "acquire", "--id", "task"}, {"htr", "attach", "--id", "task"}, {"personas"}, {"test", "old.yaml"}, {"layout", "http://localhost"},
	}
	for _, args := range invalid {
		if e := validateDeclaredRequest(args); e == nil {
			t.Fatalf("obsolete/ambiguous/wrong-scope request accepted: %v", args)
		}
	}
}
func TestProjectionTreatsFlagLookingTextAsLiteralData(t *testing.T) {
	for _, text := range []string{"--help", "-h", "--output", "--verbose", "中文\n尾行\n"} {
		args := []string{"act", "fill", "css=textarea", "--id", "task", "--text", text, "--verbose", "--lock-wait", "1s", "--output", "result.json"}
		if wantsHelp(args[1:]) {
			t.Fatalf("literal text %q became help", text)
		}
		d, a, e := lookupCommand(args)
		if e != nil {
			t.Fatal(e)
		}
		if e = validateCommandArgs(d, a); e != nil {
			t.Fatal(e)
		}
		domain, _, out := projectCommandArguments(d, a)
		if out != "result.json" {
			t.Fatalf("text captured as output: %q", out)
		}
		action, _, e := parseTypedActionArgs(append([]string{"fill"}, domain...))
		if e != nil {
			t.Fatal(e)
		}
		if action.Text != text {
			t.Fatalf("literal input changed: %q != %q", action.Text, text)
		}
	}
}
func TestGeneratedContractsUseExecutableDefinitions(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range commandDefinitions() {
		if seen[d.Path] || d.Handler == nil {
			t.Fatalf("duplicate or unrouted declaration: %s", d.Path)
		}
		seen[d.Path] = true
	}
	if _, e := json.Marshal(capabilityDocument()); e != nil {
		t.Fatal(e)
	}
	if e := exampleJourneySpec().Validate(); e != nil {
		t.Fatalf("protocol's typed YAML example invalid: %v", e)
	}
	encoded, e := yaml.Marshal(exampleJourneySpec())
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "journey.yaml")
	if e = os.WriteFile(path, encoded, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = btest.LoadSpec(path); e != nil {
		t.Fatalf("generated example fails actual loader: %v", e)
	}
	for _, shell := range []string{"zsh", "bash"} {
		script, e := generatedCompletion(shell)
		if e != nil {
			t.Fatal(e)
		}
		path := filepath.Join(t.TempDir(), "completion."+shell)
		if e = os.WriteFile(path, []byte(script), 0600); e != nil {
			t.Fatal(e)
		}
		if out, e := exec.Command(shell, "-n", path).CombinedOutput(); e != nil {
			t.Fatalf("%s generated syntax: %v %s", shell, e, out)
		}
		for _, token := range []string{"takeover", "--text-file", "--domain", "--suite", "--content"} {
			if !strings.Contains(script, token) {
				t.Fatalf("%s completion drops executable capability %s", shell, token)
			}
		}
	}
}

func TestMaintainedFixturesAndCorpusUseActualTypedLoaders(t *testing.T) {
	for _, name := range []string{"a11y-deterministic.yaml", "nl-step.yaml"} {
		path := filepath.Join("..", "..", "tests", "browseract-article", "journeys", name)
		if _, err := btest.LoadSpec(path); err != nil {
			t.Fatalf("active fixture %s: %v", name, err)
		}
	}
	paths, err := filepath.Glob(filepath.Join("..", "..", "browser", "skills", "corpus", "*", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(string(data), "```dw-browser-plan\n")
		for _, part := range parts[1:] {
			block, _, ok := strings.Cut(part, "```")
			if !ok {
				t.Fatalf("unterminated typed plan: %s", path)
			}
			if _, err := btest.ParsePlan([]byte(block)); err != nil {
				t.Fatalf("corpus plan %s: %v", path, err)
			}
			count++
		}
	}
	if count == 0 {
		t.Fatal("no typed corpus recipes validated")
	}
}

func TestCanonicalActionSerializationRetainsExplicitEmptyValues(t *testing.T) {
	for _, action := range []browser.Action{
		{Verb: "navigate", URL: "http://localhost/"},
		{Verb: "fill", Target: browser.Target{Kind: "locator", Locator: "css=input"}, Text: ""},
		{Verb: "type", Focused: true, Text: ""},
		{Verb: "select", Target: browser.Target{Kind: "locator", Locator: "css=select"}, Option: browser.SelectByValue("")},
	} {
		for _, format := range []string{"json", "yaml"} {
			var data []byte
			var err error
			var decoded browser.Action
			if format == "json" {
				data, err = json.Marshal(action)
			} else {
				data, err = yaml.Marshal(action)
			}
			if err != nil {
				t.Fatal(err)
			}
			if format == "json" {
				err = json.Unmarshal(data, &decoded)
			} else {
				err = yaml.Unmarshal(data, &decoded)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = decoded.Validate(); err != nil {
				t.Fatalf("%s roundtrip %+v: %s: %v", format, action, data, err)
			}
		}
	}
}

func TestCheckedPublicContractMatchesExecutableGeneration(t *testing.T) {
	files, err := generatedContractArtifacts()
	if err != nil {
		t.Fatal(err)
	}
	for name, expected := range files {
		path := filepath.Join("..", "..", "guide", "runtime-v2", "generated", name)
		actual, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(actual) != string(expected) {
			t.Fatalf("%s stale; regenerate with export-contract", path)
		}
	}
	for name, source := range map[string]string{"_dw-browser": "completion.zsh", "dw-browser.bash": "completion.bash"} {
		actual, err := os.ReadFile(filepath.Join("..", "..", "completions", name))
		if err != nil {
			t.Fatal(err)
		}
		if string(actual) != string(files[source]) {
			t.Fatalf("installed repository completion %s differs from same-version contract", name)
		}
	}
}
