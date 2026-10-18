package main

import "strings"

// guidanceMap 是全命令的自说话说明层：`help <cmd>` 直接输出 usage/guidance/
// examples，AI 不读外部文档即可正确使用。内容从 generated_protocol.go 的
// 已验证叙述与实测语义提炼——行为变化时与此处同步（契约测试
// TestAllCommandsCarryGuidanceAndExamples 强制全量覆盖，防回退）。
type guidanceDoc struct {
	Usage     string
	Guidance  string
	Examples  []string
}

var guidanceMap = map[string]guidanceDoc{
	"help": {
		Usage:    "help [COMMAND]",
		Guidance: "Returns the same executable command contract: flags with value types and descriptions, required arguments, and per-command usage/guidance/examples. Read `help COMMAND` before first use of any command; it is authoritative for this exact binary version.",
		Examples: []string{"dw-browser help observe", "dw-browser help \"pdf excerpt\""},
	},
	"open": {
		Usage:    "open URL --id NAME --scenario {app-test-explore|app-test-baseline|webvisit} [--persona desktop|mobile] [--mode headless|headed|visible] [--viewport WxH] [--evidence DIR] [--allow-host HOST]",
		Guidance: "Creates the browser session every later --id command operates on; the creation environment (scenario/persona/viewport/evidence root) is inherited by every later process and cannot be changed in place. --scenario is required and sets the interaction contract: app-test-explore for interactive exploration, app-test-baseline for deterministic regression (internal LLM locked), webvisit for real public sites (writes gated except --allow-host). Duplicate IDs are rejected. Use session viewport to resize later. Close only sessions you created.",
		Examples: []string{"dw-browser open http://localhost:8080/portal/chat --id chat1 --scenario app-test-explore", "dw-browser open https://arxiv.org/html/2610.17309v1 --id paper --scenario webvisit"},
	},
	"close": {
		Usage:    "close --id NAME [--force]",
		Guidance: "Closes exactly the named session you created and tears down its runtime. --force is the escape hatch for a wedged close; normal flow never needs it. Other sessions are untouched.",
		Examples: []string{"dw-browser close --id chat1"},
	},
	"once": {
		Usage:    "once URL --scenario NAME [--screenshot PATH] [--content text|list|table] [--health] [--tree] [--all]",
		Guidance: "One canonical observation in an owned short-lived session: creates, captures, closes — for quick look/see checks that do not need follow-up interaction. Same flags as observe plus creation flags. When you need to act after looking, use open + observe instead.",
		Examples: []string{"dw-browser once http://localhost:8080 --scenario app-test-explore --screenshot shot.png"},
	},
	"observe": {
		Usage:    "observe --id NAME [--screenshot PATH] [--content text|list|table] [--health] [--tree] [--top N] [--budget B] [--all] [--annotate] [--hit-audit]",
		Guidance: "The perception verb: returns the thin default {elements@rN, user_state, run_id, step}; additive flags are freely combined. --screenshot writes the actual PNG and returns its path — read the real PNG before visual actions. Channel states distinguish captured/unavailable/not-requested; an unavailable channel cannot prove zero errors. @rN refs share one space with act: `act \"click @r3\"` resolves the refs this observe just returned. --all is a census without operable refs. Reobserve after an action, resize, tab switch, navigation or intervention before using any ref or screenshot coordinate. --annotate draws the browser-chrome occlusion evidence outline (mobile persona debugging), not content annotation — for annotated content crops use excerpt.",
		Examples: []string{"dw-browser observe --id chat1 --screenshot shot.png", "dw-browser observe --id chat1 --health"},
	},
	"check": {
		Usage:    "check --id NAME [--observation ID] [--assert EXPR | --file ASSERT.json] [--using vision-url ...]",
		Guidance: "Judges required assertions against one canonical observation. Exit 0 only for required PASS, 1 for FAIL, 2 for BLOCKED/runtime/evidence/argument errors — a green exit means the assertion held, not that the product is accepted. Vision-backed assertions need explicit provider configuration.",
		Examples: []string{"dw-browser check --id chat1 --assert 'text_contains:Welcome'"},
	},
	"diff": {
		Usage:    "diff OBSERVATION_A OBSERVATION_B",
		Guidance: "Compares two canonical observations (same session) element-by-element; diagnostic only, never recovers a runtime. Use to see what an action actually changed between two observes.",
		Examples: []string{"dw-browser diff obs-1 obs-2"},
	},
	"wait": {
		Usage:    "wait --id NAME (--assert EXPR | --duration 2s) [--timeout 10s]",
		Guidance: "Waits until one assertion holds or a declared duration elapses, then returns. Prefer --assert over fixed durations; --duration is for settle-style pauses only.",
		Examples: []string{"dw-browser wait --id chat1 --assert 'visible:@r3' --timeout 10s", "dw-browser wait --id chat1 --duration 1s"},
	},
	"journey": {
		Usage:    "journey --file STEPS.yaml [--scenario NAME] [--id NAME] [--base-url URL] [--fail-fast]",
		Guidance: "Runs typed deterministic steps through the shared runtime with persistent locators and assertions. Use app-test-baseline for deterministic regression: internal LLM use is forbidden and element steps are synthetic by explicit declaration. A candidate trace is not an accepted baseline — acceptance is a separate human decision. The YAML shape is shown by `protocol`.",
		Examples: []string{"dw-browser journey --file journey.yaml --scenario app-test-baseline --evidence runs/today"},
	},
	"eval": {
		Usage:    "eval --id NAME '<js-expression>'",
		Guidance: "Gated debugging: runs JavaScript in the page and revokes observation authority (it may mutate the page). Requires write permission for the origin (webvisit needs --allow-host). Debug/synthetic evidence cannot establish Human experience — witness work uses real visible interactions (act) instead. Prefer read verbs (observe/excerpt/document) for reading.",
		Examples: []string{"dw-browser eval --id chat1 'location.href'"},
	},
	"cookie-import": {
		Usage:    "cookie-import --id NAME [--browser chrome|firefox] [--domain FILTER]",
		Guidance: "Imports cookies from the local native browser profile (keychain-decrypted) into this session — for reaching login-gated content you already hold. --domain limits the import scope.",
		Examples: []string{"dw-browser cookie-import --id chat1 --browser chrome --domain example.com"},
	},
	"audit": {
		Usage:    "audit --id NAME [--suite NAME] [--check NAME] [--threshold N] [--format json]",
		Guidance: "Deterministic rule audit over the current observation: compatibility, accessibility, layout, performance and browser-chrome rules (e.g. browser-chrome-occlusion on mobile persona). Machine-checkable only — it complements, never replaces, human-facing acceptance.",
		Examples: []string{"dw-browser audit --id chat1 --check browser-chrome-occlusion"},
	},
	"session status": {
		Usage:    "session status --id NAME",
		Guidance: "Inspects the persisted runtime ownership (authority, observation validity) without recovering or touching the runtime. Diagnostic only.",
		Examples: []string{"dw-browser session status --id chat1"},
	},
	"session list": {
		Usage:    "session list",
		Guidance: "Lists persisted sessions with their state. Diagnostic only — never recovers a runtime.",
		Examples: []string{"dw-browser session list"},
	},
	"session viewport": {
		Usage:    "session viewport --id NAME --size WxH",
		Guidance: "Resizes the existing runtime viewport and persists the environment change. Reobserve after resizing before using refs or coordinates. Bare --viewport on open is rejected for mobile sizes — use --persona mobile for real mobile fidelity.",
		Examples: []string{"dw-browser session viewport --id chat1 --size 880x720"},
	},
	"tabs list": {
		Usage:    "tabs list --id NAME",
		Guidance: "Lists the user page targets in the session browser without recovering the runtime. Diagnostic only.",
		Examples: []string{"dw-browser tabs list --id chat1"},
	},
	"tabs select": {
		Usage:    "tabs select --id NAME --target TARGET",
		Guidance: "Activates an existing target under the runtime input lock. Reobserve after switching before using any ref — refs are per-target.",
		Examples: []string{"dw-browser tabs select --id chat1 --target t2"},
	},
	"tabs close": {
		Usage:    "tabs close --id NAME --target TARGET",
		Guidance: "Closes one page target and revokes page authority. Any prior observation of that target is dead.",
		Examples: []string{"dw-browser tabs close --id chat1 --target t2"},
	},
	"tabs new": {
		Usage:    "tabs new --id NAME --url URL",
		Guidance: "Creates a new page target in the existing session browser and focuses it. Reobserve before interacting — the new target has no refs yet.",
		Examples: []string{"dw-browser tabs new --id chat1 --url http://localhost:8080/portal/workspace"},
	},
	"htr status": {
		Usage:    "htr status --id NAME",
		Guidance: "Inspects Human/AI authority state without recovery. Diagnostic only.",
		Examples: []string{"dw-browser htr status --id chat1"},
	},
	"htr takeover": {
		Usage:    "htr takeover --id NAME",
		Guidance: "Really blocks AI input: the human physically owns the page from here. Obsolete observations are revoked.",
		Examples: []string{"dw-browser htr takeover --id chat1"},
	},
	"htr yield": {
		Usage:    "htr yield --id NAME",
		Guidance: "Restores AI authority after a takeover and requires fresh observation before further acts.",
		Examples: []string{"dw-browser htr yield --id chat1"},
	},
	"htr share": {
		Usage:    "htr share --id NAME",
		Guidance: "Shared authority mode; obsolete observations are revoked.",
		Examples: []string{"dw-browser htr share --id chat1"},
	},
	"persona list": {
		Usage:    "persona list",
		Guidance: "Lists available environment personas (device fidelity bundles). Use mobile for mobile-UX acceptance; desktop for PC.",
		Examples: []string{"dw-browser persona list"},
	},
	"profile list": {
		Usage:    "profile list",
		Guidance: "Lists persistent and temporary browser profile directories known to this CLI. Diagnostic.",
		Examples: []string{"dw-browser profile list"},
	},
	"profile import": {
		Usage:    "profile import NAME",
		Guidance: "Copies a profile directory into the CLI profile store for reuse by named profiles.",
		Examples: []string{"dw-browser profile import myprofile"},
	},
	"profile prune": {
		Usage:    "profile prune --ephemeral [--min-age DURATION] [--dry-run] [--reap-orphaned]",
		Guidance: "Explicitly prunes task profile directories. --dry-run first to preview; --reap-orphaned cleans orphans left by dead processes.",
		Examples: []string{"dw-browser profile prune --ephemeral --dry-run"},
	},
	"gc": {
		Usage:    "gc [--older-than DURATION] [--dry-run] [--all]",
		Guidance: "Inspects or reclaims unowned browser resources (runtimes left behind by dead owners). --dry-run first; reclaims only unowned resources.",
		Examples: []string{"dw-browser gc --dry-run"},
	},
	"skills list": {
		Usage:    "skills list",
		Guidance: "Lists installed site recipes (per-site interaction recipes the CLI can use). Diagnostic.",
		Examples: []string{"dw-browser skills list"},
	},
	"skills read": {
		Usage:    "skills read NAME",
		Guidance: "Reads one site recipe. Diagnostic.",
		Examples: []string{"dw-browser skills read example"},
	},
	"skills write": {
		Usage:    "skills write --action PATH",
		Guidance: "Writes a site recipe from declared input.",
		Examples: []string{"dw-browser skills write --action recipe.json"},
	},
	"record start": {
		Usage:    "record start --id NAME",
		Guidance: "Starts a structured action trace on the session (for evidence and replay analysis).",
		Examples: []string{"dw-browser record start --id chat1"},
	},
	"record stop": {
		Usage:    "record stop --id NAME",
		Guidance: "Stops the trace and returns it.",
		Examples: []string{"dw-browser record stop --id chat1"},
	},
	"record export": {
		Usage:    "record export --id NAME",
		Guidance: "Returns the current trace without stopping. Diagnostic.",
		Examples: []string{"dw-browser record export --id chat1"},
	},
	"muxhost serve": {
		Usage:    "muxhost serve",
		Guidance: "Runs the existing headed host infrastructure (shared headed display host for headed sessions).",
		Examples: []string{"dw-browser muxhost serve"},
	},
	"muxhost ensure": {
		Usage:    "muxhost ensure",
		Guidance: "Ensures an explicitly named headed host exists (starts if missing).",
		Examples: []string{"dw-browser muxhost ensure"},
	},
	"muxhost status": {
		Usage:    "muxhost status [--muxhost-id ID | --runtime-id ID | --browser-session-id ID]",
		Guidance: "Inspects an existing headed host. Diagnostic.",
		Examples: []string{"dw-browser muxhost status"},
	},
	"muxhost release": {
		Usage:    "muxhost release [--muxhost-id ID | --runtime-id ID | --browser-session-id ID]",
		Guidance: "Releases a headed host lease.",
		Examples: []string{"dw-browser muxhost release --runtime-id r1"},
	},
	"muxhost shutdown": {
		Usage:    "muxhost shutdown [--muxhost-id ID | --runtime-id ID | --browser-session-id ID]",
		Guidance: "Shuts down the explicitly named headed host.",
		Examples: []string{"dw-browser muxhost shutdown --muxhost-id m1"},
	},
	"version": {
		Usage:    "version",
		Guidance: "Returns protocol and executable version as JSON. Diagnostic.",
		Examples: []string{"dw-browser version"},
	},
	"capabilities": {
		Usage:    "capabilities",
		Guidance: "Returns this executable's full command and action contract (machine-readable). Diagnostic.",
		Examples: []string{"dw-browser capabilities"},
	},
	"export-contract": {
		Usage:    "export-contract --directory DIR",
		Guidance: "Regenerates the same-version contract artifacts (capabilities.json, completions, tool-use skill) into a directory. Run after any command-surface change so the checked-in contract stays green in tests.",
		Examples: []string{"dw-browser export-contract --directory guide/runtime-v2/generated"},
	},
	"completion": {
		Usage:    "completion SHELL",
		Guidance: "Generates shell completion for the current command contract. Diagnostic.",
		Examples: []string{"dw-browser completion zsh"},
	},
	"protocol": {
		Usage:    "protocol",
		Guidance: "Generates the same-version tool-use protocol (the AI-facing usage narrative with a journey example). Diagnostic.",
		Examples: []string{"dw-browser protocol"},
	},
}

// actGuidanceCommon 是全部 act 动词共享的语义（协议 act 段提炼）。
const actGuidanceCommon = "Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry."

// actVerbNotes 是动词特有语义（追加在共享段后）。
var actVerbNotes = map[string]string{
	"click":  "Clicks the target center (respects occlusion: a click blocked by browser chrome fails loud).",
	"tap":    "Mobile-persona tap; same targeting rules as click.",
	"hover":  "Hovers the target; use for menus/tooltips that need pointer presence.",
	"fill":   "Replaces the field content (selects all then types). Input targets come from observe refs; `fill @rN` is the working form. --sensitive requires a masked password control and suppresses text facts. Strict fill verifies physical selection before deletion.",
	"type":   "Types text; --focused inserts into physical focus without changing selection.",
	"press":  "Presses a key or chord, e.g. `press Control+b`, `press Enter`; modifiers are real CDP modifier masks, not characters.",
	"focus":  "Focuses the target element.",
	"select": "Native select by exact complete displayed option label (--label) or exact DOM option value (--value) — exactly one selector, including an explicit empty string; no prefix or first-match fallback. First observe the control, read its option labels/values, choose one field, execute, then observe again.",
	"set-checked": "Sets checkbox/radio state; argument is the desired state.",
	"scroll":  "Scrolls (e.g. `scroll down 3`); pure-coordinate forms exist for canvas targets.",
	"scrollto": "Scrolls a locator or coordinates into view.",
	"drag":    "Drags from target/coordinates to an end point (--end-x/--end-y).",
	"swipe":   "Mobile-persona swipe gesture.",
	"navigate": "Navigates the page to a URL; refs die afterwards.",
	"reload":  "Reloads the current page; refs die afterwards.",
	"back":    "Browser back; refs die afterwards.",
	"forward": "Browser forward; refs die afterwards.",
	"zoom":    "Enters/adjusts the visual zoom state (persona chrome stays fixed); `zoom reset` restores.",
	"keyboard": "Opens or dismisses the simulated on-screen keyboard (mobile persona state).",
}

var actExamples = map[string][]string{
	"click":   {`dw-browser act click @r3 --id chat1`, `dw-browser act click css=#submit --id chat1`},
	"fill":    {`dw-browser act fill @r5 --text "hello" --id chat1`},
	"press":   {`dw-browser act press Control+b --id chat1`},
	"select":  {`dw-browser act select @r7 --label "完整选项文案" --id chat1`, `dw-browser act select <CONTROL_REF> --id <SESSION> --value '<EXACT_DOM_VALUE_FROM_OBSERVE>'`},
	"navigate": {`dw-browser act navigate https://example.com --id chat1`},
	"scroll":  {`dw-browser act scroll down 3 --id chat1`},
}

// applyGuidanceDocs 把说明层注入全部命令（commandDefinitions() 尾部调用，
// 是 help 输出 guidance/examples/usage 的唯一注入点）。
func applyGuidanceDocs(defs []CommandDefinition) {
	for i := range defs {
		d := &defs[i]
		if doc, ok := guidanceMap[d.Path]; ok {
			if doc.Usage != "" {
				d.Usage = doc.Usage
			}
			if doc.Guidance != "" {
				d.Guidance = doc.Guidance
			}
			if len(doc.Examples) > 0 {
				d.Examples = doc.Examples
			}
		}
		if verb, ok := strings.CutPrefix(d.Path, "act "); ok {
			g := actGuidanceCommon
			if note, ok := actVerbNotes[verb]; ok {
				g += " " + note
			}
			d.Guidance = g
			if d.Usage == "" {
				d.Usage = "act " + verb + " [TARGET] --id NAME [verb flags — see flags below]"
			}
			if ex, ok := actExamples[verb]; ok {
				d.Examples = ex
			} else {
				d.Examples = []string{"dw-browser act " + verb + " @r1 --id chat1"}
			}
		}
	}
	applyCoreFlagDescriptions(defs)
}

// applyCoreFlagDescriptions 补高频 flag 的一句话描述（自说话的参数层）。
func applyCoreFlagDescriptions(defs []CommandDefinition) {
	perCommand := map[string]map[string]string{
		"open": {
			"--scenario": "interaction contract: app-test-explore (interactive exploration), app-test-baseline (deterministic regression), webvisit (real public site; writes gated except --allow-host)",
			"--persona":  "environment fidelity bundle: desktop (default) or mobile (iPhone bundle + Safari chrome simulation; required for mobile UX acceptance)",
			"--mode":     "headless (default), headed (windowless hidden target) or visible",
			"--viewport": "initial viewport WxH (mobile sizes require --persona mobile)",
			"--evidence": "evidence root directory for observations/screenshots",
			"--allow-host": "trusted host whose writes are always allowed (webvisit)",
		},
		"observe": {
			"--screenshot": "write the actual PNG to this path and return it — read the real image before visual actions",
			"--content":    "also capture visible text as text|list|table",
			"--health":     "also capture telemetry (console/network via visible_errors scanning; CLI cannot capture full console/network)",
			"--tree":       "also return the full a11y text (rarely needed)",
			"--top":        "elements list size (default 20)",
			"--budget":     "elements text budget bytes (default 4096)",
			"--all":        "census mode: full inventory without operable refs (clears session refs)",
			"--annotate":   "draw browser-chrome occlusion outline evidence (mobile persona debugging)",
		},
		"wait": {
			"--assert":   "assertion expression to wait for (preferred)",
			"--duration": "fixed settle pause (e.g. 1s)",
			"--timeout":  "maximum wait",
		},
		"journey": {
			"--file":     "typed YAML steps (shape shown by `protocol`)",
			"--base-url": "base URL for relative step targets",
			"--fail-fast": "stop at first failing step",
		},
		"check": {
			"--assert": "required assertion expression",
			"--file":   "assertions JSON file",
			"--using":  "oracle selector (e.g. vision provider)",
		},
		"close": {"--force": "force-close a wedged session (escape hatch)"},
		"session viewport": {"--size": "new viewport WxH; reobserve afterwards"},
		"tabs select":      {"--target": "target id from tabs list"},
		"tabs close":       {"--target": "target id from tabs list"},
		"tabs new":         {"--url": "URL for the new target; reobserve afterwards"},
	}
	for i := range defs {
		descs, ok := perCommand[defs[i].Path]
		if !ok {
			continue
		}
		for fi := range defs[i].Flags {
			if d, ok := descs[defs[i].Flags[fi].Name]; ok {
				defs[i].Flags[fi].Description = d
			}
		}
	}
}
