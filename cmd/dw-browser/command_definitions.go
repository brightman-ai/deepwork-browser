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
	ValidateOutputs func([]string, string) error          `json:"-"`
	Arguments       []CLIArgument                         `json:"arguments,omitempty"`
	Usage           string                                `json:"usage,omitempty"`
	Examples        []string                              `json:"examples,omitempty"`
	Guidance        string                                `json:"guidance,omitempty"`
	Path            string                                `json:"command"`
	Summary         string                                `json:"summary"`
	Flags           []CLIFlag                             `json:"flags"`
	Required        []string                              `json:"required,omitempty"`
	MinArgs         int                                   `json:"min_args"`
	MaxArgs         int                                   `json:"max_args"`
	Diagnostic      bool                                  `json:"diagnostic"`
	Handler         func([]string) (CommandResult, error) `json:"-"`
	Rules           []FlagRule                            `json:"rules,omitempty"`
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

const dnsResolverDescription = "system (default), or HTTPS endpoint at a literal public IP with default/443 port and exact /dns-query path; no userinfo, query or fragment. TLS must validate the resolver IP SAN."
const dnsResolverGuidance = " DNS resolver choice is per call: system by default, or an explicit public numeric-IP HTTPS /dns-query endpoint. Only use that endpoint when the caller explicitly chooses a public DNS strategy; never switch after a timeout. No resolver fallback, environment override or cross-call cache. Resolver DNS traffic shares the acquisition network-byte budget. A third-party resolver learns queried hostnames and the client/proxy source IP, not the original URL path or response content; existing proxy trust remains. TLS must validate its IP SAN."

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
	documentFlags := flags("timeout max-html-bytes max-network-bytes network-route dns-resolver", "")
	documentFlags = append(documentFlags, CLIFlag{Name: "--mode", TakesValue: true, ValueType: "string", Values: []string{"headless", "headed"}, Description: "DOM rendering: headless (default) or headful hidden target without a native window"})
	add("fetch", "Save a complete public HTTP response body without JavaScript or a browser", runFetchResult, flags("save timeout max-bytes max-network-bytes network-route dns-resolver", ""), []string{"--save"}, 1, 1, false)
	add("document", "Read public URL DOM in a disposable browser target; no screenshot or session", runDocumentResult, documentFlags, nil, 1, 1, false)
	add("observe", "Capture a canonical observation and actual screenshot", runObserveResult, id(flags("screenshot content scope top budget", "health tree all hit-audit annotate")), []string{"--id"}, 0, 0, false)
	add("excerpt", "Crop read-only annotated PNG screenshot excerpts from the open page", runExcerptResult, id(flags("spec out settle", "")), []string{"--id", "--spec"}, 0, 0, false)
	add("pdf preview", "Render PDF or office-document pages as PNG images", runPdfPreviewResult, flags("file pages dpi out network-route", ""), []string{"--file"}, 0, 0, false)
	add("pdf excerpt", "Crop annotated PNG screenshot excerpts from PDF pages by phrase or box", runPdfExcerptResult, flags("file spec out network-route", ""), []string{"--file", "--spec"}, 0, 0, false)
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
		case "excerpt":
			d.Usage = "excerpt --id SESSION --spec '<json array | @file | ->' [--out DIR] [--settle 500ms]"
			d.Arguments = []CLIArgument{{Name: "--spec", Description: "JSON array; each entry becomes one cropped PNG screenshot: {name, targets:[...], highlight:[phrases], box_style:{color:\"#rrggbb\", width:int(css px)}, highlight_color:\"#rrggbb\", crop_margin:int(css px)}. targets resolve elements on the open page: \"id:ELEMID\" by DOM id (dots allowed, no CSS escaping needed), \"title:ELEMID\" for the .ltx_title inside that id (arXiv headings), or a bare value as a CSS selector. highlight phrases are matched inside the target text with whitespace tolerance, skipping MathML annotations and spanning inline links/emphasis; each match is painted with a translucent highlighter and every matched line keeps readable text."}}
			d.Guidance = "Screenshot verb (read-only observation class): crops page regions as PNG images with a red box around the target union and translucent highlights over chosen phrases — built for quoting web originals (papers, docs) into markdown. It never mutates the page, needs no write grant or allow-host, and does not invalidate observation refs (a 20-image batch needs no re-observe). One full-page capture serves the whole spec. Unresolved targets and unmatched phrases are explicit per-entry business results in missing (exit stays 0); entries whose targets all miss produce no file. warnings reports fixed/sticky elements overlapping the box and images still loading. Elements taller than the viewport are captured whole. Style defaults: red 3px box, #ffe94d highlight, 14px crop margin; override per entry with box_style/highlight_color/crop_margin."
			d.Examples = []string{
				`dw-browser excerpt --id paper --spec '[{"name":"readout","targets":["id:S6.SS2.SSS0.Px1"],"highlight":["Instant readouts have approximately 2–8% higher latency"]}]'`,
				`dw-browser excerpt --id paper --spec @spec.json --out ./shots`,
			}
			for fi := range d.Flags {
				switch d.Flags[fi].Name {
				case "--spec":
					d.Flags[fi].Description = "JSON array of screenshot entries: [{name, targets:[\"id:X\"|\"title:X\"|css], highlight:[phrases], box_style?, highlight_color?, crop_margin?}]; or @file / - for stdin"
				case "--out":
					d.Flags[fi].Description = "output directory for the PNGs (default: evidence/excerpts/<session>)"
				case "--settle":
					d.Flags[fi].Description = "wait before collecting (fonts/images settle after load), e.g. 500ms"
				}
			}
		case "pdf preview":
			d.Usage = "pdf preview --file <path|url> [--pages 1-3] [--dpi 150] [--out DIR] [--network-route system|direct]"
			d.Arguments = []CLIArgument{{Name: "--file", Description: "Local PDF path, or http(s) URL downloaded through the fetch network boundary; office documents (docx/pptx/xlsx/odt…) are converted to PDF via libreoffice automatically."}}
			d.Guidance = "Page-image verb (no browser session): renders chosen pages of a local or downloaded PDF (office documents convert first, kind reported in source.kind) as PNG files for reading/preview. Needs poppler-utils; office input additionally needs libreoffice (both fail loud with install hints when missing). Pages are 1-based; --pages takes comma-separated ranges like 2,5-7 clamped to the page count. Default dpi 150 (36–600). This is static rendering, not UI observation: it cannot prove interactive behavior."
			d.Examples = []string{"dw-browser pdf preview --file paper.pdf --pages 1-3 --out ./pages", "dw-browser pdf preview --file https://arxiv.org/pdf/2610.17309 --pages 1"}
			for fi := range d.Flags {
				switch d.Flags[fi].Name {
				case "--file":
					d.Flags[fi].Description = "local PDF/office file path or http(s) URL"
				case "--pages":
					d.Flags[fi].Description = "1-based pages: \"3\" or \"1-3\" or \"2,5-7\" (default 1)"
				case "--dpi":
					d.Flags[fi].Description = "render resolution 36–600 (default 150)"
				case "--out":
					d.Flags[fi].Description = "output directory (default evidence/pdf-preview)"
				case "--network-route":
					d.Flags[fi].Description = "URL download route: system (default, honors validated proxy config) or direct (ignores proxies, explicit choice)"
				}
			}
		case "pdf excerpt":
			d.Usage = "pdf excerpt --file <path|url> --spec '<json array | @file | ->' [--out DIR] [--network-route system|direct]"
			d.Arguments = []CLIArgument{{Name: "--spec", Description: "JSON array; each entry becomes one cropped PNG screenshot from a PDF page: {name, page, phrases:[...], boxes:[[x0,y0,x1,y1],...] in PDF points (72dpi space, origin top-left), box_style:{color:\"#rrggbb\", width:int(render px)}, highlight_color:\"#rrggbb\", crop_margin:int(render px at 200 dpi)}. phrases are matched as a contiguous word sequence (NFKC-normalized, typographic dashes/quotes folded to ASCII, punctuation-affix tolerant) via poppler word bounding boxes — multi-line matches highlight each matched word. The red box is drawn around the union of boxes and matched words; crop adds a margin around it."}}
			d.Guidance = "Screenshot verb (static PDF counterpart of excerpt): crops annotated PNG excerpts from PDF pages — red box around the target union, translucent highlighter over matched phrases — for quoting paper originals into markdown when the HTML version is missing or loses figures. Same style defaults as excerpt (red 3px, #ffe94d, 14px margin). Word-level matching means a phrase must appear verbatim (after normalization); unmatched phrases are explicit per-entry results in missing (exit stays 0), entries with no match and no boxes produce no file. page_out_of_range is reported in warnings. Needs poppler-utils; office input additionally needs libreoffice. Rendering runs at a fixed 200 dpi for text sharpness. Not a UI observation."
			d.Examples = []string{
				`dw-browser pdf excerpt --file paper.pdf --spec '[{"name":"latency-table","page":7,"phrases":["Single-pass decision readouts"],"boxes":[[60,95,540,320]]}]'`,
			}
			for fi := range d.Flags {
				switch d.Flags[fi].Name {
				case "--file":
					d.Flags[fi].Description = "local PDF/office file path or http(s) URL"
				case "--spec":
					d.Flags[fi].Description = "JSON array of entries: [{name, page, phrases?, boxes?, box_style?, highlight_color?, crop_margin?}]; or @file / - for stdin"
				case "--out":
					d.Flags[fi].Description = "output directory (default evidence/pdf-excerpts)"
				}
			}
		case "fetch":
			d.ValidateOutputs = validateFetchOutputs
			d.Arguments = []CLIArgument{{Name: "URL", Description: "Public HTTP(S) URL whose response body is saved without JavaScript, cookies or browser rendering."}}
			d.Usage = "fetch URL --save FILE [--timeout 20s] [--max-bytes 8388608] [--max-network-bytes 33554432] [--network-route system|direct] [--dns-resolver system|https://PUBLIC_NUMERIC_IP/dns-query]"
			d.Examples = []string{"dw-browser fetch https://example.com/image.png --save ./image.png --output ./image.meta.json", "dw-browser fetch https://example.com/article --save ./article.html --network-route direct", "dw-browser fetch https://example.com/article --save ./article.html --dns-resolver https://1.1.1.1/dns-query"}
			d.Guidance = "Use fetch for unrendered HTML, images or files; document for browser-rendered DOM; open/observe for UI. --save writes the complete body; stdout and --output contain dw.resource.v1 metadata and body_file only. Existing save files are replaced only after successful acquisition and network cleanup. Save and metadata paths must differ. Default system route; direct is explicit, with no automatic route or tool fallback. Delivery commits in order: body rename, optional metadata file, then stdout. Each committed output remains if a later delivery fails. Encoding or metadata-write failures exit 2 and, when stdout works, report diagnostics preserving byte counts without body_file. A stdout failure returns exit 2 and stderr; an already committed metadata file may still contain successful body_file metadata. Inspect available committed metadata or settle budgets conservatively. Consumers verify MIME/signature and meaning: an HTTP 200 challenge page is not an image or accepted article."
			d.Guidance += dnsResolverGuidance
			for fi := range d.Flags {
				switch d.Flags[fi].Name {
				case "--dns-resolver":
					d.Flags[fi].Description = dnsResolverDescription
				case "--save":
					d.Flags[fi].Description = "Required body file; existing parent directory must be writable. Complete bytes are published atomically."
				case "--output":
					d.Flags[fi].Description = "JSON metadata file, never the response body; must not alias --save."
				case "--network-route":
					d.Flags[fi].Values = []string{"system", "direct"}
					d.Flags[fi].Description = "system (default) honors supported trusted proxy settings; direct deliberately ignores them. Both validate public destinations."
				case "--timeout":
					d.Flags[fi].Description = "Positive acquisition budget; default 20s."
				case "--max-bytes":
					d.Flags[fi].Description = "Maximum decoded response-body bytes; default 8388608 (8 MiB); oversize fails without partial output."
				case "--max-network-bytes":
					d.Flags[fi].Description = "Guard network-accounting budget; default 33554432 (32 MiB), separate from decoded body bytes; includes DNS traffic."
				}
			}
		case "document":
			d.Arguments = []CLIArgument{{Name: "URL", Description: "Public HTTP(S) URL; no existing browser session or profile is used."}}
			d.Usage = "document URL [--mode headless|headed] [--network-route system|direct] [--timeout DURATION] [--dns-resolver system|https://PUBLIC_NUMERIC_IP/dns-query]"
			d.Guidance = "Default headless, system route. Headed uses a windowless hidden target, not a visible browser and not UI acceptance evidence. No mode fallback or automatic challenge retry. System route honors supported trusted proxy configuration only after validating numeric public destinations; direct deliberately ignores proxy configuration. A captured login or verification page is not article acceptance. Read rendering, policy, cleanup and failure diagnostics."
			d.Guidance += dnsResolverGuidance
			d.Examples = []string{"dw-browser document https://example.com/article --dns-resolver https://1.1.1.1/dns-query"}
			for fi := range d.Flags {
				switch d.Flags[fi].Name {
				case "--dns-resolver":
					d.Flags[fi].Description = dnsResolverDescription
				case "--network-route":
					d.Flags[fi].Values = []string{"system", "direct"}
					d.Flags[fi].Description = "Transport configuration: system (default) or explicit direct; public destination validation remains required in both."
				case "--timeout":
					d.Flags[fi].Description = "Whole read budget including queue and browser startup; cleanup has an independent bounded deadline."
				}
			}
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
	applyGuidanceDocs(defs)
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
	case "top", "budget", "count", "steps", "threshold", "width", "height", "owner-pid", "debug-port", "dpi", "max-html-bytes", "max-bytes", "max-network-bytes":
		return "integer"
	case "x", "y", "end-x", "end-y", "delta-x", "delta-y":
		return "number"
	case "timeout", "duration", "settle", "older-than", "min-age", "idle-ttl":
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
