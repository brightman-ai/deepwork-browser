package main

import (
	"fmt"
	"github.com/brightman-ai/deepwork-browser/browser"
	btest "github.com/brightman-ai/deepwork-browser/browser/testing"
	"gopkg.in/yaml.v3"
	"strings"
)

func generatedProtocol() string {
	var b strings.Builder
	b.WriteString("---\nname: dw-browser\ndescription: Observe and operate real browser sessions with the DW-Browser v2 CLI, preserving visible evidence, Human control and deterministic journey assertions. Use for browser tasks through this tool; ordinary software editing does not need it.\n---\n\n")
	fmt.Fprintf(&b, "# DW-Browser %s tool protocol\n\n", version)
	b.WriteString("Choose by task: DW-Browser primarily supports real Web experience, exploration and acceptance. Use `open` → `observe` → `act` for UI tasks (`once` for one capture), and typed `journey` steps with assertions for deterministic acceptance. Auxiliary background reads use `document URL` for rendered DOM or `fetch URL --save FILE` for an HTTP response body. Choose the required task and evidence explicitly; a successful read receipt cannot prove UI visibility, successful interaction or an accepted baseline.\n\n")
	b.WriteString("If a required screenshot is unavailable, UI acceptance remains BLOCKED. Restore `observe` and inspect its actual PNG before continuing visual actions or acceptance; successful document/fetch reads cannot turn missing UI evidence into a PASS.\n\n")
	b.WriteString("Create with `open URL --id NAME --scenario app-test-explore --persona desktop --mode headless --evidence DIR`. Creation environment is inherited by every later process. `open` rejects an existing ID. Use `session viewport --id NAME --size 880x720` to resize the same browser.\n\n")
	b.WriteString("Use `observe --id NAME`. Read the returned real PNG before visual actions; the canonical observation path/id is the fact source. Channel states distinguish captured, unavailable and not-requested. An unavailable channel cannot prove zero errors. Empty refs can still be a valid static screenshot. Per-control `allowed_actions` comes from captured control state and the shared action declaration; unavailable state does not prove editability. Native select controls include bounded declared option labels/values and a `select` capability; options_complete=false means the list is incomplete. Copy a selectable choice's `select.options[].option` unchanged into canonical `Action.option`; for CLI map its sole `label` or `value` field directly to `--label` or `--value`, preserving the exact string including empty strings. A displayed choice with selectable=false is not an executable suggestion. Privacy or truncation can withhold choices; a selector explicitly known from the task still requires fresh exact-uniqueness validation. This data does not prove a visible native popup. Reobserve after an action/resize/tab/navigation/intervention before using a ref or screenshot coordinate. Tokens are opaque and bound to one observation, document, target and runtime generation.\n\n")
	b.WriteString("Use `excerpt --id NAME --spec '<json array>' [--out DIR]` to crop annotated PNG screenshot excerpts of page regions: each entry `{name, targets:[\"id:X\"|\"title:X\"|css], highlight:[phrases], box_style?, highlight_color?, crop_margin?}` draws a red box around the target union, translucent highlights over matched phrases (whitespace-tolerant, MathML-skipping, cross-inline), and crops with a margin from one full-page capture. It is read-only observation-class work: no DOM mutation, no write grant, observation stays valid. Unresolved targets and unmatched phrases are reported explicitly per entry in `missing`; `warnings` surfaces fixed/sticky overlap and pending images. Entries with no resolved target produce no file; page-level failures exit non-zero while per-entry misses stay business results in the JSON.\n\n")
	b.WriteString("For PDF and office documents use the session-less `pdf` family (poppler + libreoffice rendering, no browser): `pdf preview --file PATH|URL [--pages 1-3] [--dpi 150] [--out DIR]` renders pages as PNGs; `pdf excerpt --file PATH|URL --spec '<json>'` crops annotated excerpts with the same look as `excerpt`, matching phrases as normalized contiguous word sequences and accepting explicit `boxes` in PDF points. Office inputs (docx/pptx/xlsx…) convert via libreoffice automatically. Static rendering cannot prove interactive UI behavior; the in-browser PDF viewer is not used (plugin DOM is inaccessible to observation).\n\n")
	b.WriteString("Use `act VERB TARGET --id NAME` with typed flags. A target is a returned opaque `@...` token, an explicit `css=...` or `role=...` locator, or coordinates with `--x N --y N --unit css-px --frame viewport --observation-id CAPTURE`; target-relative points use `--unit percent --frame target` and the capture ID, unless their opaque ref already supplies it. Text uses exactly one of `--text` or UTF-8 `--text-file`; file trailing newlines are preserved. `fill` replaces, `type --focused` inserts into physical focus. `fill --sensitive` requires a masked password control and suppresses text facts/traces; unmasked destinations are rejected before focus/deletion, preserving real screenshots. Literal text is never an action string. Strict fill verifies physical selection before deletion. Date/time controls use explicitly synthetic element steps in an app-test-baseline journey; physical fill does not use a setter fallback. `act select TARGET --id SESSION --label LABEL` matches only the exact complete displayed option label; `--value VALUE` matches only the exact DOM option value. Supply exactly one selector, including an explicit empty string if needed. There is no cross-field fallback, prefix syntax or first-match choice for duplicates. TARGET is the control ref from observe or an explicit locator; --id identifies the session. First observe, read the control option projection, choose a label or DOM value without guessing, execute, then observe again. For a control named 来源, `dw-browser act select <CONTROL_REF> --id <SESSION> --label '完整内容'` uses placeholders from the observation; replace angle-bracket placeholders before execution. Selection uses native keyboard selection in visual mode; native failure never falls back to a setter. Native typeahead can emit intermediate changes; interrupted or partial input requires observation. Only explicit element steps in app-test-baseline use synthetic select. Receipts separate requested_fidelity and interaction_mode from actual execution/evidence_scope; keyboard-selection does not prove popup visibility, pointer hit targets or dismissal.\n\n")
	b.WriteString("A command error reports phase/dispatch_state/recovery. Unknown or partial dispatch requires observation; do not automatically replay fill/submit. `eval` is gated debugging that may write and revokes observation authority. Witness work uses real visible interactions; debug/synthetic evidence cannot establish Human experience.\n\n")
	b.WriteString("`htr takeover --id NAME` really blocks AI input; `htr yield --id NAME` restores authority and requires fresh observation. Status/help are diagnostic and never recover a runtime. Headless and intentionally visible native tabs need no display host; headed mode retains its verified display guard. Close only sessions you created with `close --id NAME`.\n\n")
	b.WriteString("For interactive public browsing choose `webvisit`; writes retain origin/allow-host gating. That write policy is not a public-only network sandbox; for background DOM reads requiring the public-only request boundary, use `document`. This auxiliary read does not replace UI observation or interaction. For deterministic regression choose `app-test-baseline`; internal LLM use is forbidden. Use one typed `journey --file FILE` runner with persistent locators and assertions. A candidate trace is not an accepted baseline. `check` exits 0 only for required PASS, 1 for FAIL, 2 for BLOCKED/runtime/evidence/argument errors. Other exit 0 means complete delivery, not product acceptance. stdout is one JSON; `--output` saves that JSON, `--screenshot` names a PNG and creation `--evidence` names the evidence root.\n\n")
	b.WriteString("For auxiliary background public DOM acquisition use `document URL --timeout 30s --max-html-bytes 8388608 --max-network-bytes 33554432`. `document` returns `dw.document.v1` with `content` and `representation=dom-html`, without screenshots or caller JavaScript. XML/RSS/Atom return an `unsupported_content_type` reason instead of a browser viewer masquerading as source. It owns a disposable browser and profile, never attaches an existing session or user profile, and grants no arbitrary eval. Default --mode headless; explicit --mode headed creates a native headful hidden target without a browser window or virtual display. Rendering mode and actual target surface are reported. It never silently switches modes; a challenge page is not usable article evidence, and changing mode is not a guaranteed remedy. Default --network-route system uses supported trusted system proxy configuration only after local public-IP validation and numeric CONNECT pinning; --network-route direct deliberately ignores proxy configuration. Proxy errors never fall back to direct. Ordinary failures report dw.document.failure.v1 diagnostics with usable=false, stage, reason, timing, policy and cleanup; they contain no partial DOM and grant no feed reroute. Its `page-requests-public-only` policy covers webpage requests through one Chrome-wide DNS-pinned guard; a configured system proxy is explicitly trusted to honor numeric CONNECT, and this is not an OS firewall. Child targets are not paused or reattached. Chrome's own sandbox stays enabled. Windows fails before launch because verified process-tree cleanup is unavailable. The returned `url` is the actual main-frame location; `declared_canonical_url` is only an untrusted DOM hint. DOM-ready is not a promise that site-specific async content is complete. Budgets fail rather than truncate.\n\n")
	b.WriteString("For auxiliary HTTP body acquisition use `fetch URL --save FILE`: it saves the complete response body without JavaScript or Chrome (unrendered HTML, images or files). Fetch defaults to a 20s acquisition budget, 8388608 decoded body bytes (8 MiB), a 33554432-byte guard network-accounting budget (32 MiB), separate from decoded body bytes and including DNS traffic, and `--network-route system`; `direct` must be requested explicitly. It never changes tools or routes based on content type or failure. Example: `fetch https://example.com/image.png --save ./image.png --output ./image.meta.json`: --save receives complete body bytes, stdout and --output receive dw.resource.v1 JSON metadata with body_file. These paths must not alias, and parent directories must already exist. Body publication occurs atomically after network cleanup. Acquisition or body-persist failure returns dw.resource.failure.v1 diagnostics with usable=false and body_bytes_read, never a usable partial file. Delivery commits in order: body rename, optional --output metadata file, then stdout; each committed output remains if a later delivery fails. JSON encoding or metadata-write failures exit 2 and, when stdout works, report diagnostics preserving byte counts without body_file. A stdout failure returns exit 2 and stderr; an already committed metadata file may still contain successful body_file metadata. Inspect available committed metadata or settle budgets conservatively. The consumer verifies MIME, signature, status and meaning; a 200 challenge page is not proof of an image or article. Fetch accepts no session, browser mode, cookies, headers, method override, JavaScript or host grants.\n\n")
	b.WriteString(dnsResolverGuidance + " Explicit caller-authorized example: `fetch https://example.com/article --save ./article.html --dns-resolver https://1.1.1.1/dns-query`; the same --dns-resolver option is available on document only. An endpoint must be HTTPS with a literal public IP, default or 443 port, exact /dns-query path and no userinfo, query or fragment.\n\n")
	b.WriteString("For exact required flags, positional counts, enums and conditional rules, read [the generated command reference](references/commands.md), or invoke `help COMMAND` / `capabilities` from this same executable.\n\n")
	b.WriteString("## Deterministic editing journey\n\nAdapt persistent CSS locators to the actual visible controls, then save this v2 YAML and run `journey --file FILE --scenario app-test-baseline --evidence DIR`. The wait/check assert saved text after navigation; changing input into a synthetic setter cannot stand in for visual editing.\n\n```yaml\n")
	example, err := yaml.Marshal(exampleJourneySpec())
	if err != nil {
		panic(err)
	}
	b.Write(example)
	b.WriteString("```\n")

	return b.String()
}

func generatedCompletion(shell string) (string, error) {
	roots := strings.Join(rootCommandNames(), " ")
	switch shell {
	case "zsh":
		var b strings.Builder
		b.WriteString("#compdef dw-browser\n_dw_browser_v2() {\n local -a candidates\n")
		fmt.Fprintf(&b, " if (( CURRENT == 2 )); then candidates=(%s); _describe 'command' candidates; return; fi\n", roots)
		b.WriteString(" case \"${words[2]} ${words[3]}\" in\n")
		for _, d := range commandDefinitions() {
			if !strings.Contains(d.Path, " ") {
				continue
			}
			fmt.Fprintf(&b, " '%s') candidates=(", d.Path)
			for _, f := range d.Flags {
				fmt.Fprintf(&b, "'%s' ", f.Name)
			}
			b.WriteString("); _describe 'flag' candidates; return;;\n")
		}
		b.WriteString(" esac\n case \"${words[2]}\" in\n")
		for _, root := range rootCommandNames() {
			var sub []string
			var fs []CLIFlag
			for _, d := range commandDefinitions() {
				p := strings.Fields(d.Path)
				if p[0] != root {
					continue
				}
				if len(p) > 1 {
					sub = append(sub, p[1])
				} else {
					fs = d.Flags
				}
			}
			fmt.Fprintf(&b, " '%s') candidates=(", root)
			for _, v := range sub {
				fmt.Fprintf(&b, "'%s' ", v)
			}
			for _, f := range fs {
				fmt.Fprintf(&b, "'%s' ", f.Name)
			}
			b.WriteString(");;\n")
		}
		b.WriteString(" esac\n _describe 'argument' candidates\n}\ncompdef _dw_browser_v2 dw-browser\n")
		return b.String(), nil

	case "bash":
		var b strings.Builder
		b.WriteString("_dw_browser_v2() {\n local cur=\"${COMP_WORDS[COMP_CWORD]}\" prev=\"${COMP_WORDS[COMP_CWORD-1]}\" choices=\"\"\n")
		fmt.Fprintf(&b, " if (( COMP_CWORD == 1 )); then choices='%s'; else\n", roots)
		b.WriteString(" case \"${COMP_WORDS[1]} ${COMP_WORDS[2]}\" in\n")
		for _, d := range commandDefinitions() {
			if !strings.Contains(d.Path, " ") {
				continue
			}
			fmt.Fprintf(&b, " '%s') choices='", d.Path)
			for _, f := range d.Flags {
				fmt.Fprintf(&b, "%s ", f.Name)
			}
			b.WriteString("';;\n")
		}
		b.WriteString(" esac\n if [[ -z $choices ]]; then case \"${COMP_WORDS[1]}\" in\n")
		for _, root := range rootCommandNames() {
			fmt.Fprintf(&b, " '%s') choices='", root)
			for _, d := range commandDefinitions() {
				p := strings.Fields(d.Path)
				if p[0] != root {
					continue
				}
				if len(p) > 1 {
					fmt.Fprintf(&b, "%s ", p[1])
				} else {
					for _, f := range d.Flags {
						fmt.Fprintf(&b, "%s ", f.Name)
					}
				}
			}
			b.WriteString("';;\n")
		}
		b.WriteString(" esac; fi\n fi\n COMPREPLY=($(compgen -W \"$choices\" -- \"$cur\"))\n}\ncomplete -F _dw_browser_v2 dw-browser\n")
		return b.String(), nil

	default:
		return "", fmt.Errorf("completion supports zsh|bash")
	}
}
func runGeneratedCompletion(args []string) (CommandResult, error) {
	if len(args) != 1 {
		return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("completion requires zsh|bash"))
	}
	content, err := generatedCompletion(args[0])
	if err != nil {
		return CommandResult{}, commandError("validation", exitRunErr, err)
	}
	return commandSuccess(map[string]any{"shell": args[0], "content": content}), nil
}

func exampleJourneySpec() *btest.JourneySpec {
	required := true
	return &btest.JourneySpec{Version: 2, ID: "edit-save-revisit", Name: "Edit, save, and revisit", Environment: btest.EnvironmentSpec{BaseURL: "http://127.0.0.1:8080", EntryURL: "/", Mode: "headless", Viewport: "1280x720", Client: "chrome"}, Journey: []btest.StepSpec{
		{ID: "look", Kind: "observe"},
		{ID: "edit", Kind: "action", Mode: browser.InteractionModeVisual, Action: &browser.Action{Verb: "fill", Target: browser.Target{Kind: "locator", Locator: "css=[data-testid='editor']"}, Text: "A persisted edit"}},
		{ID: "save", Kind: "action", Mode: browser.InteractionModeVisual, Action: &browser.Action{Verb: "click", Target: browser.Target{Kind: "locator", Locator: "css=[data-testid='save']"}}},
		{ID: "revisit", Kind: "action", Mode: browser.InteractionModeVisual, Action: &browser.Action{Verb: "navigate", URL: "http://127.0.0.1:8080/"}},
		{ID: "await-persisted", Kind: "wait", Wait: &btest.WaitSpec{Assert: "text_contains('A persisted edit')", TimeoutMs: 5000}},
		{ID: "assert-persisted", Kind: "check", Check: []btest.AssertionSpec{{ID: "persisted", Assert: "text_contains('A persisted edit')", Required: &required}}},
	}, Evidence: btest.EvidenceSpec{Save: []string{"screenshot", "observation", "report", "trace"}}}
}
func generatedCommandReference() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# DW-Browser %s command contract\n\nGenerated from executable declarations. Flags on attached sessions inherit creation environment. `--output` is JSON; `--screenshot` is PNG. Absolute and locator-relative points require `--observation-id` from the exact capture; opaque ref-relative points supply capture identity in the token. Gesture endpoints share that identity.\n\n", version)
	for _, d := range commandDefinitions() {
		fmt.Fprintf(&b, "## `%s`\n\n%s. Positional arguments: %d..%d. Required flags: %s.\n\n", d.Path, d.Summary, d.MinArgs, d.MaxArgs, strings.Join(d.Required, ", "))
		if d.Usage != "" {
			fmt.Fprintf(&b, "Usage: `%s`\n\n", d.Usage)
		}
		for _, a := range d.Arguments {
			fmt.Fprintf(&b, "- `%s`: %s\n", a.Name, a.Description)
		}
		if d.Guidance != "" {
			fmt.Fprintf(&b, "\n%s\n\n", d.Guidance)
		}
		for _, example := range d.Examples {
			fmt.Fprintf(&b, "Example: `%s`\n\n", example)
		}
		for _, f := range d.Flags {
			fmt.Fprintf(&b, "- `%s`", f.Name)
			if f.TakesValue {
				fmt.Fprintf(&b, " VALUE (%s)", f.ValueType)
			}
			if len(f.Values) > 0 {
				fmt.Fprintf(&b, "; values %s", strings.Join(f.Values, "|"))
			}
			if f.AllowEmpty {
				b.WriteString("; empty string permitted")
			}
			if f.Description != "" {
				fmt.Fprintf(&b, "; %s", f.Description)
			}
			b.WriteString("\n")
		}
		for _, r := range d.Rules {
			fmt.Fprintf(&b, "- Rule: when=%q unless=%q exactly-one=%v requires=%v forbids=%v.\n", r.WhenPresent, r.UnlessPresent, r.ExactlyOne, r.Requires, r.Forbids)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}
