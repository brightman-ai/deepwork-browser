package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

type CLIFlag struct {
	Description    string   `json:"description,omitempty"`
	Name           string   `json:"name"`
	TakesValue     bool     `json:"takes_value"`
	Values         []string `json:"values,omitempty"`
	AllowEmpty     bool     `json:"allow_empty,omitempty"`
	ValueType      string   `json:"value_type,omitempty"`
	Infrastructure string   `json:"infrastructure,omitempty"`
}
type FlagRule struct {
	WhenPresent   string   `json:"when_present,omitempty"`
	UnlessPresent string   `json:"unless_present,omitempty"`
	ExactlyOne    []string `json:"exactly_one,omitempty"`
	Requires      []string `json:"requires,omitempty"`
	Forbids       []string `json:"forbids,omitempty"`
}

type CLIArgument struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type CommandDefinition struct {
	Arguments  []CLIArgument                         `json:"arguments,omitempty"`
	Usage      string                                `json:"usage,omitempty"`
	Examples   []string                              `json:"examples,omitempty"`
	Guidance   string                                `json:"guidance,omitempty"`
	Path       string                                `json:"command"`
	Summary    string                                `json:"summary"`
	Flags      []CLIFlag                             `json:"flags"`
	Required   []string                              `json:"required,omitempty"`
	MinArgs    int                                   `json:"min_args"`
	MaxArgs    int                                   `json:"max_args"`
	Diagnostic bool                                  `json:"diagnostic"`
	Handler    func([]string) (CommandResult, error) `json:"-"`
	Rules      []FlagRule                            `json:"rules,omitempty"`
}

func flags(valueNames, boolNames string) []CLIFlag {
	var out []CLIFlag
	for _, n := range strings.Fields(valueNames) {
		out = append(out, CLIFlag{Name: "--" + n, TakesValue: true, ValueType: flagValueType(n), AllowEmpty: n == "text", Values: flagValues(n)})
	}
	for _, n := range strings.Fields(boolNames) {
		out = append(out, CLIFlag{Name: "--" + n})
	}
	return out
}
func creationFlags() []CLIFlag {
	out := flags("scenario mode persona engine safari-device viewport profile evidence allow-host browser-chrome user-agent goal", "stealth")
	for i := range out {
		switch out[i].Name {
		case "--scenario":
			out[i].Values = []string{"app-test-explore", "app-test-baseline", "webvisit"}
		case "--mode":
			out[i].Values = []string{"headless", "headed", "visible"}
		case "--engine":
			out[i].Values = []string{"chrome", "safari"}
		case "--browser-chrome":
			out[i].Values = []string{"auto", "on", "off"}
		}
	}
	return out
}
func commandDefinitions() []CommandDefinition {
	defs := []CommandDefinition{}
	add := func(path, summary string, handler func([]string) (CommandResult, error), opts []CLIFlag, required []string, min, max int, diag bool) {
		opts = append(opts, flags("output lock-wait", "verbose")...)
		for i := range opts {
			switch opts[i].Name {
			case "--output":
				opts[i].Infrastructure = "output"
			case "--verbose":
				opts[i].Infrastructure = "logging"
			case "--lock-wait":
				opts[i].Infrastructure = "locking"
			}
		}
		defs = append(defs, CommandDefinition{Path: path, Summary: summary, Flags: opts, Required: required, MinArgs: min, MaxArgs: max, Diagnostic: diag, Handler: handler})
	}
	id := func(extra []CLIFlag) []CLIFlag { return append(flags("id", ""), extra...) }
	add("help", "Show the same executable command contract", runHelpResult, nil, nil, 0, 2, true)
	add("open", "Create one session; duplicate IDs are rejected", runOpenResult, append(creationFlags(), flags("id", "")...), []string{"--scenario", "--id"}, 1, 1, false)
	add("close", "Close only the named session", runCloseResult, id(flags("", "force")), []string{"--id"}, 0, 0, false)
	add("once", "Capture one canonical UI observation in an owned short-lived session", runOnceResult, append(creationFlags(), flags("screenshot content", "health tree all annotate")...), []string{"--scenario"}, 1, 1, false)
	add("document", "Read public URL DOM in an isolated disposable headless browser; no screenshot or session", runDocumentResult, flags("timeout max-html-bytes max-network-bytes", ""), nil, 1, 1, false)
	add("observe", "Capture a canonical observation and actual screenshot", runObserveResult, id(flags("screenshot content scope top budget", "health tree all hit-audit annotate")), []string{"--id"}, 0, 0, false)
	add("check", "Judge required assertions against one canonical observation", runCheckResult, flags("id observation assert file using vision-url vision-provider vision-model vision-api-key", ""), nil, 0, 0, false)
	add("diff", "Compare two canonical observations", runDiffResult, nil, nil, 2, 2, true)
	add("wait", "Wait for one assertion or a declared duration", runWaitResult, flags("id assert timeout duration", ""), []string{"--id"}, 0, 0, false)
	add("journey", "Run typed deterministic steps through the shared runtime", runJourneyResult, append(creationFlags(), flags("id file base-url", "fail-fast")...), []string{"--file"}, 0, 0, false)
	add("eval", "Run gated JavaScript debugging and revoke old observation authority", runEvalResult, id(nil), []string{"--id"}, 1, 1, false)
	add("cookie-import", "Import native browser cookies with keychain decryption and a domain filter", runCookieImportResult, id(flags("browser domain", "")), []string{"--id"}, 0, 0, false)
	add("audit", "Audit compatibility, accessibility, layout, performance and browser chrome rules", runAuditResult, id(flags("suite check threshold format", "")), []string{"--id"}, 0, 0, false)
	add("session status", "Inspect persisted runtime ownership without recovery", runSessionStatusResult, id(nil), []string{"--id"}, 0, 0, true)
	add("session list", "List persisted sessions", runSessionListResult, nil, nil, 0, 0, true)
	add("session viewport", "Resize the existing runtime and persist its environment", runSessionViewportResult, id(flags("size", "")), []string{"--id", "--size"}, 0, 0, false)
	add("tabs list", "List user page targets without recovering the runtime", runTabsListResult, id(nil), []string{"--id"}, 0, 0, true)
	add("tabs select", "Activate an existing target under the runtime input lock", runTabsSelectResult, id(flags("target", "")), []string{"--id", "--target"}, 0, 0, false)
	add("tabs close", "Close a target and revoke page authority", runTabsCloseResult, id(flags("target", "")), []string{"--id", "--target"}, 0, 0, false)
	add("tabs new", "Create a target in the existing browser", runTabsNewResult, id(flags("url", "")), []string{"--id"}, 0, 0, false)
	add("htr status", "Inspect Human control and runtime state without recovery", runHTRStatusResult, id(nil), []string{"--id"}, 0, 0, true)
	for _, a := range []struct{ name, authority string }{{"takeover", browser.AuthorityHuman}, {"yield", browser.AuthorityAgentic}, {"share", browser.AuthorityShared}} {
		authority, name := a.authority, a.name
		add("htr "+name, "Set Human/AI authority; obsolete observations are revoked", func(args []string) (CommandResult, error) {
			return runHTRSetAuthorityResult(args, authority, "htr "+name)
		}, id(nil), []string{"--id"}, 0, 0, false)
	}
	add("persona list", "List environment personas", runPersonaListResult, nil, nil, 0, 0, true)
	add("profile list", "List persistent and temporary profile directories", func(a []string) (CommandResult, error) { return runProfileResult(append([]string{"list"}, a...)) }, nil, nil, 0, 0, true)
	add("profile import", "Copy a profile into the CLI profile store", func(a []string) (CommandResult, error) { return runProfileResult(append([]string{"import"}, a...)) }, flags("name", ""), nil, 1, 1, false)
	add("profile prune", "Prune task profile directories explicitly", runProfilePruneResult, flags("min-age", "ephemeral dry-run reap-orphaned"), []string{"--ephemeral"}, 0, 0, false)
	add("gc", "Inspect or reclaim unowned browser resources explicitly", runGCResult, flags("older-than", "dry-run all"), nil, 0, 0, false)
	add("skills list", "List installed site recipes", runSkillsListResult, nil, nil, 0, 0, true)
	add("skills read", "Read one site recipe", runSkillsReadResult, flags("action", ""), nil, 1, 1, true)
	add("skills write", "Write a site recipe from declared input", runSkillsWriteResult, flags("action", ""), []string{"--action"}, 1, 1, false)
	add("record start", "Start a structured action trace", runRecordStartResult, id(nil), []string{"--id"}, 0, 0, false)
	add("record stop", "Stop and return the trace", runRecordStopResult, id(nil), []string{"--id"}, 0, 0, false)
	add("record export", "Return the current trace", runRecordExportResult, id(nil), []string{"--id"}, 0, 0, true)
	infra := flags("muxhost-id runtime-id browser-session-id kind profile-id profile-dir chrome-path debug-port owner-pid owner isolation mode goal idle-ttl persona preset width height user-agent service account identity-key", "touch")
	add("muxhost serve", "Run the existing headed host infrastructure", runMuxHostServeResult, infra, nil, 0, 0, false)
	add("muxhost ensure", "Ensure an explicitly named headed host", runMuxHostEnsureResult, infra, nil, 0, 0, false)
	add("muxhost status", "Inspect an existing headed host", runMuxHostStatusResult, flags("muxhost-id runtime-id browser-session-id", ""), nil, 0, 0, true)
	add("muxhost release", "Release an existing headed host lease", runMuxHostReleaseResult, flags("muxhost-id runtime-id browser-session-id", ""), nil, 0, 0, false)
	add("muxhost shutdown", "Shut down the explicitly named headed host", runMuxHostShutdownResult, flags("muxhost-id runtime-id browser-session-id", ""), nil, 0, 0, false)
	add("version", "Return the candidate protocol version", func([]string) (CommandResult, error) {
		return commandSuccess(map[string]any{"version": version, "protocol": "dw.browser.v2"}), nil
	}, nil, nil, 0, 0, true)
	add("capabilities", "Return this executable command and action contract", func([]string) (CommandResult, error) { return commandSuccess(capabilityDocument()), nil }, nil, nil, 0, 0, true)
	add("export-contract", "Generate same-version capabilities, completions and tool-use skill", runExportContractResult, flags("directory", ""), []string{"--directory"}, 0, 0, false)
	add("completion", "Generate shell completion from this command contract", runGeneratedCompletion, nil, nil, 1, 1, true)
	add("protocol", "Generate the same-version tool-use protocol", func([]string) (CommandResult, error) {
		return commandSuccess(map[string]any{"schema": "dw.tool-protocol.v2", "version": version, "content": generatedProtocol()}), nil
	}, nil, nil, 0, 0, true)
	for _, action := range browser.ActionDefinitions() {
		d := action
		opts := id(actionFlags(d))
		add("act "+d.Kind, d.Summary, func(a []string) (CommandResult, error) { return runActResult(append([]string{d.Kind}, a...)) }, opts, []string{"--id"}, 0, 1, false)
	}
	for i := range defs {
		d := &defs[i]
		for fi := range d.Flags {
			if d.Flags[fi].Name == "--content" {
				d.Flags[fi].Values = []string{"text", "list", "table"}
			}
		}
		switch d.Path {
		case "muxhost status", "muxhost release", "muxhost shutdown":
			d.Rules = []FlagRule{{ExactlyOne: []string{"--muxhost-id", "--runtime-id", "--browser-session-id"}}}
		case "muxhost serve", "muxhost ensure":
			for fi := range d.Flags {
				if d.Flags[fi].Name == "--persona" || d.Flags[fi].Name == "--preset" {
					d.Flags[fi].AllowEmpty = true
				}
			}
		case "journey":
			creation := []string{}
			for _, f := range creationFlags() {
				creation = append(creation, f.Name)
			}
			d.Rules = []FlagRule{{WhenPresent: "--id", Forbids: creation}, {UnlessPresent: "--id", Requires: []string{"--scenario"}}}
		case "check":
			d.Rules = []FlagRule{{ExactlyOne: []string{"--id", "--observation"}}, {ExactlyOne: []string{"--assert", "--file"}}}
		case "act select":
			d.MinArgs = 1
			d.Arguments = []CLIArgument{{Name: "TARGET", Description: "Control ref returned by observe, or an explicit css= or role= locator for the select control."}}
			d.Usage = "act select TARGET --id SESSION (--label LABEL | --value VALUE)"
			d.Examples = []string{"dw-browser act select <CONTROL_REF_FOR_来源> --id <SESSION> --label '完整内容'", "dw-browser act select <CONTROL_REF_FOR_来源> --id <SESSION> --value '<EXACT_DOM_VALUE_FROM_OBSERVE>'"}
			d.Guidance = "Placeholders in angle brackets must be replaced. First observe the session, read the control's option labels and DOM values, choose exactly one matching field, execute, then observe again. Do not guess values from labels; duplicate matches fail. Copy a selectable choice's select.options[].option unchanged into Action.option; for CLI map its sole label or value field directly to --label or --value, preserving exact strings including empty strings."
			var selectors []string
			for _, name := range browser.SelectOptionSelectorFields() {
				selectors = append(selectors, "--"+name)
			}
			d.Rules = []FlagRule{{ExactlyOne: selectors}}
			for fi := range d.Flags {
				switch d.Flags[fi].Name {
				case "--label":
					d.Flags[fi].Description = "Exact complete displayed option label only; no DOM-value fallback. Supply exactly one of --label or --value."
					d.Flags[fi].AllowEmpty = true
				case "--value":
					d.Flags[fi].Description = "Exact DOM option value only; no displayed-label fallback. Supply exactly one of --label or --value."
					d.Flags[fi].AllowEmpty = true
				case "--id":
					d.Flags[fi].Description = "Session ID used by observe; this is not the control target."
				}
			}
		case "act fill", "act type":
			d.Rules = []FlagRule{{ExactlyOne: []string{"--text", "--text-file"}}}
		case "wait":
			d.Rules = []FlagRule{{ExactlyOne: []string{"--assert", "--duration"}}, {WhenPresent: "--assert", Requires: []string{"--timeout"}}, {WhenPresent: "--duration", Forbids: []string{"--timeout"}}}
		}
		for _, f := range d.Flags {
			switch f.Name {
			case "--x":
				d.Rules = append(d.Rules, FlagRule{WhenPresent: f.Name, Requires: []string{"--y"}})
			case "--y":
				d.Rules = append(d.Rules, FlagRule{WhenPresent: f.Name, Requires: []string{"--x"}})
			case "--unit", "--frame", "--observation-id":
				d.Rules = append(d.Rules, FlagRule{WhenPresent: f.Name, Requires: []string{"--x", "--y"}})
			case "--end-x":
				d.Rules = append(d.Rules, FlagRule{WhenPresent: f.Name, Requires: []string{"--end-y", "--x", "--y"}})
			case "--end-y":
				d.Rules = append(d.Rules, FlagRule{WhenPresent: f.Name, Requires: []string{"--end-x", "--x", "--y"}})
			}
		}
		for _, f := range d.Flags {
			if f.Name == "--text" {
				d.Rules = append(d.Rules, FlagRule{WhenPresent: "--text", Forbids: []string{"--text-file"}})
			}
		}
	}
	return defs
}

func actionFlags(d browser.ActionDefinition) []CLIFlag {
	var out []CLIFlag
	for _, f := range d.Fields {
		switch f {
		case "target":
			for _, k := range d.TargetKinds {
				if k == "point" {
					out = append(out, flags("x y unit frame observation-id", "")...)
					break
				}
			}
		case "option":
			out = append(out, flags(strings.Join(browser.SelectOptionSelectorFields(), " "), "")...)
		case "text":
			out = append(out, flags("text text-file", "")...)
		case "sensitive", "focused":
			out = append(out, flags("", f)...)
		case "end":
			out = append(out, flags("end-x end-y", "")...)
		default:
			out = append(out, flags(strings.ReplaceAll(f, "_", "-"), "")...)
		}
	}
	return out
}

func lookupCommand(args []string) (CommandDefinition, []string, error) {
	for _, d := range commandDefinitions() {
		parts := strings.Fields(d.Path)
		if len(args) < len(parts) {
			continue
		}
		match := true
		for i, p := range parts {
			if args[i] != p {
				match = false
				break
			}
		}
		if match {
			return d, args[len(parts):], nil
		}
	}
	return CommandDefinition{}, nil, fmt.Errorf("unknown command; use help or capabilities")
}
func validateCommandArgs(d CommandDefinition, args []string) error {
	allowed := map[string]CLIFlag{}
	for _, f := range d.Flags {
		allowed[f.Name] = f
	}
	seen := map[string]bool{}
	endOptions := false
	pos := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" && !endOptions {
			endOptions = true
			continue
		}
		if endOptions || !strings.HasPrefix(a, "-") {
			pos++
			continue
		}
		name, value, inline := strings.Cut(a, "=")
		f, ok := allowed[name]
		if !ok {
			return fmt.Errorf("%s: unknown or unavailable flag %s", d.Path, name)
		}
		if seen[name] && name != "--allow-host" {
			return fmt.Errorf("%s: duplicate %s", d.Path, name)
		}
		seen[name] = true
		if f.TakesValue {
			if !inline {
				if i+1 >= len(args) {
					return fmt.Errorf("%s requires a value", name)
				}
				i++
				value = args[i]
			}
			if value == "" && !f.AllowEmpty {
				return fmt.Errorf("%s requires a nonempty value", name)
			}
		} else if inline {
			return fmt.Errorf("%s is a boolean flag without a value", name)
		}
		if f.TakesValue {
			switch f.ValueType {
			case "session-id":
				if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`).MatchString(value) {
					return fmt.Errorf("%s requires a session id without path separators or whitespace", name)
				}
			case "integer":
				if _, err := strconv.Atoi(value); err != nil {
					return fmt.Errorf("%s requires an integer", name)
				}
			case "number":
				v, err := strconv.ParseFloat(value, 64)
				if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
					return fmt.Errorf("%s requires a finite number", name)
				}
			case "duration":
				v, err := time.ParseDuration(value)
				if err != nil || v < 0 {
					return fmt.Errorf("%s requires a nonnegative duration", name)
				}
			case "lock-duration":
				if v, err := time.ParseDuration(value); err != nil || v < 0 {
					if n, e := strconv.Atoi(value); e != nil || n < 0 {
						return fmt.Errorf("%s requires a duration or nonnegative seconds", name)
					}
				}
			}
		}
		if len(f.Values) > 0 {
			valid := false
			for _, v := range f.Values {
				if value == v {
					valid = true
				}
			}
			if !valid {
				return fmt.Errorf("%s accepts %s", name, strings.Join(f.Values, "|"))
			}
		}
	}
	if pos < d.MinArgs || pos > d.MaxArgs {
		return fmt.Errorf("%s requires %d..%d positional arguments", d.Path, d.MinArgs, d.MaxArgs)
	}
	for _, f := range d.Required {
		if !seen[f] {
			return fmt.Errorf("%s requires %s", d.Path, f)
		}
	}
	for _, rule := range d.Rules {
		if rule.WhenPresent != "" && !seen[rule.WhenPresent] || rule.UnlessPresent != "" && seen[rule.UnlessPresent] {
			continue
		}
		if len(rule.ExactlyOne) > 0 {
			n := 0
			for _, f := range rule.ExactlyOne {
				if seen[f] {
					n++
				}
			}
			if n != 1 {
				return fmt.Errorf("%s requires exactly one of %s", d.Path, strings.Join(rule.ExactlyOne, ", "))
			}
		}
		for _, f := range rule.Requires {
			if !seen[f] {
				return fmt.Errorf("%s requires %s", d.Path, f)
			}
		}
		for _, f := range rule.Forbids {
			if seen[f] {
				return fmt.Errorf("%s is unavailable with %s", f, rule.WhenPresent)
			}
		}
	}
	return nil
}
func capabilityDocument() map[string]any {
	return map[string]any{"schema_version": "dw.capabilities.v2", "version": version, "commands": commandDefinitions(), "actions": browser.ActionDefinitions()}
}
func generatedHelp(path string) map[string]any {
	ds := []CommandDefinition{}
	for _, d := range commandDefinitions() {
		if path == "" || d.Path == path || strings.HasPrefix(d.Path, path+" ") {
			ds = append(ds, d)
		}
	}
	return map[string]any{"version": version, "usage": "dw-browser <command> [typed arguments]", "commands": ds, "discovery": "capabilities"}
}
func rootCommandNames() []string {
	m := map[string]bool{}
	for _, d := range commandDefinitions() {
		m[strings.Fields(d.Path)[0]] = true
	}
	out := []string{}
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func flagValueType(name string) string {
	switch name {
	case "id":
		return "session-id"
	case "top", "budget", "count", "steps", "threshold", "width", "height", "owner-pid", "debug-port", "max-html-bytes", "max-network-bytes":
		return "integer"
	case "x", "y", "end-x", "end-y", "delta-x", "delta-y":
		return "number"
	case "timeout", "duration", "older-than", "min-age", "idle-ttl":
		return "duration"
	case "lock-wait":
		return "lock-duration"
	default:
		return "string"
	}
}

func parseDeclaredOptions(args []string, command string) (map[string]string, error) {
	options, _, err := parseDeclaredArguments(args, command)
	return options, err
}

func parseDeclaredArguments(args []string, command string) (map[string]string, []string, error) {
	d, _, err := lookupCommand(strings.Fields(command))
	if err != nil {
		return nil, nil, err
	}
	if err = validateCommandArgs(d, args); err != nil {
		return nil, nil, err
	}
	values := map[string]string{}
	var positional []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(args[i], "--") {
			positional = append(positional, args[i])
			continue
		}
		name, value, inline := strings.Cut(args[i], "=")
		for _, f := range d.Flags {
			if f.Name == name {
				if f.TakesValue && !inline {
					i++
					value = args[i]
				} else if !f.TakesValue {
					value = "true"
				}
				values[name] = value
				break
			}
		}
	}
	return values, positional, nil
}

func flagValues(name string) []string {
	switch name {
	case "mode":
		return []string{"headless", "headed", "visible"}
	case "kind":
		return []string{"task", "interactive", "service", "debug", "test"}
	case "scenario":
		return []string{"app-test-explore", "app-test-baseline", "webvisit"}
	case "engine":
		return []string{"chrome", "safari"}
	case "format":
		return []string{"json", "compact"}
	case "content":
		return []string{"text", "list", "table"}
	case "scope":
		return []string{"visible", "all"}
	case "count":
		return []string{"1", "2"}
	case "button":
		return []string{"left", "right"}
	case "direction":
		return []string{"up", "down", "left", "right"}
	case "checked":
		return []string{"true", "false"}
	case "unit":
		return []string{"css-px", "percent"}
	case "frame":
		return []string{"viewport", "target"}
	default:
		return nil
	}
}

// projectCommandArguments projects only declared infrastructure fields. A flag
// value is data even when its literal spelling looks like another CLI flag.
func projectCommandArguments(d CommandDefinition, args []string) (domain []string, infrastructure []string, output string) {
	specs := map[string]CLIFlag{}
	for _, f := range d.Flags {
		specs[f.Name] = f
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			domain = append(domain, args[i:]...)
			break
		}
		name, value, inline := strings.Cut(args[i], "=")
		f, known := specs[name]
		tokens := []string{args[i]}
		if known && f.TakesValue && !inline && i+1 < len(args) {
			i++
			value = args[i]
			tokens = append(tokens, args[i])
		}
		if f.Infrastructure != "" {
			infrastructure = append(infrastructure, tokens...)
			if f.Infrastructure == "output" {
				output = value
			}
		} else {
			domain = append(domain, tokens...)
		}
	}
	return
}
func runHelpResult(args []string) (CommandResult, error) {
	path := strings.Join(args, " ")
	doc := generatedHelp(path)
	if path != "" && len(doc["commands"].([]CommandDefinition)) == 0 {
		return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("unknown command path %q", path))
	}
	return commandSuccess(doc), nil
}
