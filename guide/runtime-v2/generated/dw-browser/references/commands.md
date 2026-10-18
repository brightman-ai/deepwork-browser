# DW-Browser 0.19.0-v2 command contract

Generated from executable declarations. Flags on attached sessions inherit creation environment. `--output` is JSON; `--screenshot` is PNG. Absolute and locator-relative points require `--observation-id` from the exact capture; opaque ref-relative points supply capture identity in the token. Gesture endpoints share that identity.

## `help`

Show the same executable command contract. Positional arguments: 0..2. Required flags: .

Usage: `help [COMMAND]`


Returns the same executable command contract: flags with value types and descriptions, required arguments, and per-command usage/guidance/examples. Read `help COMMAND` before first use of any command; it is authoritative for this exact binary version.

Example: `dw-browser help observe`

Example: `dw-browser help "pdf excerpt"`

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `open`

Create one session; duplicate IDs are rejected. Positional arguments: 1..1. Required flags: --scenario, --id.

Usage: `open URL --id NAME --scenario {app-test-explore|app-test-baseline|webvisit} [--persona desktop|mobile] [--mode headless|headed|visible] [--viewport WxH] [--evidence DIR] [--allow-host HOST]`


Creates the browser session every later --id command operates on; the creation environment (scenario/persona/viewport/evidence root) is inherited by every later process and cannot be changed in place. --scenario is required and sets the interaction contract: app-test-explore for interactive exploration, app-test-baseline for deterministic regression (internal LLM locked), webvisit for real public sites (writes gated except --allow-host). Duplicate IDs are rejected. Use session viewport to resize later. Close only sessions you created.

Example: `dw-browser open http://localhost:8080/portal/chat --id chat1 --scenario app-test-explore`

Example: `dw-browser open https://arxiv.org/html/2610.17309v1 --id paper --scenario webvisit`

- `--scenario` VALUE (string); values app-test-explore|app-test-baseline|webvisit; interaction contract: app-test-explore (interactive exploration), app-test-baseline (deterministic regression), webvisit (real public site; writes gated except --allow-host)
- `--mode` VALUE (string); values headless|headed|visible; headless (default), headed (windowless hidden target) or visible
- `--persona` VALUE (string); environment fidelity bundle: desktop (default) or mobile (iPhone bundle + Safari chrome simulation; required for mobile UX acceptance)
- `--engine` VALUE (string); values chrome|safari
- `--safari-device` VALUE (string)
- `--viewport` VALUE (string); initial viewport WxH (mobile sizes require --persona mobile)
- `--profile` VALUE (string)
- `--evidence` VALUE (string); evidence root directory for observations/screenshots
- `--allow-host` VALUE (string); trusted host whose writes are always allowed (webvisit)
- `--browser-chrome` VALUE (string); values auto|on|off
- `--user-agent` VALUE (string)
- `--goal` VALUE (string)
- `--stealth`
- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `close`

Close only the named session. Positional arguments: 0..0. Required flags: --id.

Usage: `close --id NAME [--force]`


Closes exactly the named session you created and tears down its runtime. --force is the escape hatch for a wedged close; normal flow never needs it. Other sessions are untouched.

Example: `dw-browser close --id chat1`

- `--id` VALUE (session-id)
- `--force`; force-close a wedged session (escape hatch)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `once`

Capture one canonical UI observation in an owned short-lived session. Positional arguments: 1..1. Required flags: --scenario.

Usage: `once URL --scenario NAME [--screenshot PATH] [--content text|list|table] [--health] [--tree] [--all]`


One canonical observation in an owned short-lived session: creates, captures, closes — for quick look/see checks that do not need follow-up interaction. Same flags as observe plus creation flags. When you need to act after looking, use open + observe instead.

Example: `dw-browser once http://localhost:8080 --scenario app-test-explore --screenshot shot.png`

- `--scenario` VALUE (string); values app-test-explore|app-test-baseline|webvisit
- `--mode` VALUE (string); values headless|headed|visible
- `--persona` VALUE (string)
- `--engine` VALUE (string); values chrome|safari
- `--safari-device` VALUE (string)
- `--viewport` VALUE (string)
- `--profile` VALUE (string)
- `--evidence` VALUE (string)
- `--allow-host` VALUE (string)
- `--browser-chrome` VALUE (string); values auto|on|off
- `--user-agent` VALUE (string)
- `--goal` VALUE (string)
- `--stealth`
- `--screenshot` VALUE (string)
- `--content` VALUE (string); values text|list|table
- `--health`
- `--tree`
- `--all`
- `--annotate`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `fetch`

Save a complete public HTTP response body without JavaScript or a browser. Positional arguments: 1..1. Required flags: --save.

Usage: `fetch URL --save FILE [--timeout 20s] [--max-bytes 8388608] [--max-network-bytes 33554432] [--network-route system|direct] [--dns-resolver system|https://PUBLIC_NUMERIC_IP/dns-query]`

- `URL`: Public HTTP(S) URL whose response body is saved without JavaScript, cookies or browser rendering.

Use fetch for unrendered HTML, images or files; document for browser-rendered DOM; open/observe for UI. --save writes the complete body; stdout and --output contain dw.resource.v1 metadata and body_file only. Existing save files are replaced only after successful acquisition and network cleanup. Save and metadata paths must differ. Default system route; direct is explicit, with no automatic route or tool fallback. Delivery commits in order: body rename, optional metadata file, then stdout. Each committed output remains if a later delivery fails. Encoding or metadata-write failures exit 2 and, when stdout works, report diagnostics preserving byte counts without body_file. A stdout failure returns exit 2 and stderr; an already committed metadata file may still contain successful body_file metadata. Inspect available committed metadata or settle budgets conservatively. Consumers verify MIME/signature and meaning: an HTTP 200 challenge page is not an image or accepted article. DNS resolver choice is per call: system by default, or an explicit public numeric-IP HTTPS /dns-query endpoint. Only use that endpoint when the caller explicitly chooses a public DNS strategy; never switch after a timeout. No resolver fallback, environment override or cross-call cache. Resolver DNS traffic shares the acquisition network-byte budget. A third-party resolver learns queried hostnames and the client/proxy source IP, not the original URL path or response content; existing proxy trust remains. TLS must validate its IP SAN.

Example: `dw-browser fetch https://example.com/image.png --save ./image.png --output ./image.meta.json`

Example: `dw-browser fetch https://example.com/article --save ./article.html --network-route direct`

Example: `dw-browser fetch https://example.com/article --save ./article.html --dns-resolver https://1.1.1.1/dns-query`

- `--save` VALUE (string); Required body file; existing parent directory must be writable. Complete bytes are published atomically.
- `--timeout` VALUE (duration); Positive acquisition budget; default 20s.
- `--max-bytes` VALUE (integer); Maximum decoded response-body bytes; default 8388608 (8 MiB); oversize fails without partial output.
- `--max-network-bytes` VALUE (integer); Guard network-accounting budget; default 33554432 (32 MiB), separate from decoded body bytes; includes DNS traffic.
- `--network-route` VALUE (string); values system|direct; system (default) honors supported trusted proxy settings; direct deliberately ignores them. Both validate public destinations.
- `--dns-resolver` VALUE (string); system (default), or HTTPS endpoint at a literal public IP with default/443 port and exact /dns-query path; no userinfo, query or fragment. TLS must validate the resolver IP SAN.
- `--output` VALUE (string); JSON metadata file, never the response body; must not alias --save.
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `document`

Read public URL DOM in a disposable browser target; no screenshot or session. Positional arguments: 1..1. Required flags: .

Usage: `document URL [--mode headless|headed] [--network-route system|direct] [--timeout DURATION] [--dns-resolver system|https://PUBLIC_NUMERIC_IP/dns-query]`

- `URL`: Public HTTP(S) URL; no existing browser session or profile is used.

Default headless, system route. Headed uses a windowless hidden target, not a visible browser and not UI acceptance evidence. No mode fallback or automatic challenge retry. System route honors supported trusted proxy configuration only after validating numeric public destinations; direct deliberately ignores proxy configuration. A captured login or verification page is not article acceptance. Read rendering, policy, cleanup and failure diagnostics. DNS resolver choice is per call: system by default, or an explicit public numeric-IP HTTPS /dns-query endpoint. Only use that endpoint when the caller explicitly chooses a public DNS strategy; never switch after a timeout. No resolver fallback, environment override or cross-call cache. Resolver DNS traffic shares the acquisition network-byte budget. A third-party resolver learns queried hostnames and the client/proxy source IP, not the original URL path or response content; existing proxy trust remains. TLS must validate its IP SAN.

Example: `dw-browser document https://example.com/article --dns-resolver https://1.1.1.1/dns-query`

- `--timeout` VALUE (duration); Whole read budget including queue and browser startup; cleanup has an independent bounded deadline.
- `--max-html-bytes` VALUE (integer)
- `--max-network-bytes` VALUE (integer)
- `--network-route` VALUE (string); values system|direct; Transport configuration: system (default) or explicit direct; public destination validation remains required in both.
- `--dns-resolver` VALUE (string); system (default), or HTTPS endpoint at a literal public IP with default/443 port and exact /dns-query path; no userinfo, query or fragment. TLS must validate the resolver IP SAN.
- `--mode` VALUE (string); values headless|headed; DOM rendering: headless (default) or headful hidden target without a native window
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `observe`

Capture a canonical observation and actual screenshot. Positional arguments: 0..0. Required flags: --id.

Usage: `observe --id NAME [--screenshot PATH] [--content text|list|table] [--health] [--tree] [--top N] [--budget B] [--all] [--annotate] [--hit-audit]`


The perception verb: returns the thin default {elements@rN, user_state, run_id, step}; additive flags are freely combined. --screenshot writes the actual PNG and returns its path — read the real PNG before visual actions. Channel states distinguish captured/unavailable/not-requested; an unavailable channel cannot prove zero errors. @rN refs share one space with act: `act "click @r3"` resolves the refs this observe just returned. --all is a census without operable refs. Reobserve after an action, resize, tab switch, navigation or intervention before using any ref or screenshot coordinate. --annotate draws the browser-chrome occlusion evidence outline (mobile persona debugging), not content annotation — for annotated content crops use excerpt.

Example: `dw-browser observe --id chat1 --screenshot shot.png`

Example: `dw-browser observe --id chat1 --health`

- `--id` VALUE (session-id)
- `--screenshot` VALUE (string); write the actual PNG to this path and return it — read the real image before visual actions
- `--content` VALUE (string); values text|list|table; also capture visible text as text|list|table
- `--scope` VALUE (string); values visible|all
- `--top` VALUE (integer); elements list size (default 20)
- `--budget` VALUE (integer); elements text budget bytes (default 4096)
- `--health`; also capture telemetry (console/network via visible_errors scanning; CLI cannot capture full console/network)
- `--tree`; also return the full a11y text (rarely needed)
- `--all`; census mode: full inventory without operable refs (clears session refs)
- `--hit-audit`
- `--annotate`; draw browser-chrome occlusion outline evidence (mobile persona debugging)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `excerpt`

Crop read-only annotated PNG screenshot excerpts from the open page. Positional arguments: 0..0. Required flags: --id, --spec.

Usage: `excerpt --id SESSION --spec '<json array | @file | ->' [--out DIR] [--settle 500ms]`

- `--spec`: JSON array; each entry becomes one cropped PNG screenshot: {name, targets:[...], highlight:[phrases], box_style:{color:"#rrggbb", width:int(css px)}, highlight_color:"#rrggbb", crop_margin:int(css px)}. targets resolve elements on the open page: "id:ELEMID" by DOM id (dots allowed, no CSS escaping needed), "title:ELEMID" for the .ltx_title inside that id (arXiv headings), or a bare value as a CSS selector. highlight phrases are matched inside the target text with whitespace tolerance, skipping MathML annotations and spanning inline links/emphasis; each match is painted with a translucent highlighter and every matched line keeps readable text.

Screenshot verb (read-only observation class): crops page regions as PNG images with a red box around the target union and translucent highlights over chosen phrases — built for quoting web originals (papers, docs) into markdown. It never mutates the page, needs no write grant or allow-host, and does not invalidate observation refs (a 20-image batch needs no re-observe). One full-page capture serves the whole spec. Unresolved targets and unmatched phrases are explicit per-entry business results in missing (exit stays 0); entries whose targets all miss produce no file. warnings reports fixed/sticky elements overlapping the box and images still loading. Elements taller than the viewport are captured whole. Style defaults: red 3px box, #ffe94d highlight, 14px crop margin; override per entry with box_style/highlight_color/crop_margin.

Example: `dw-browser excerpt --id paper --spec '[{"name":"readout","targets":["id:S6.SS2.SSS0.Px1"],"highlight":["Instant readouts have approximately 2–8% higher latency"]}]'`

Example: `dw-browser excerpt --id paper --spec @spec.json --out ./shots`

- `--id` VALUE (session-id)
- `--spec` VALUE (string); JSON array of screenshot entries: [{name, targets:["id:X"|"title:X"|css], highlight:[phrases], box_style?, highlight_color?, crop_margin?}]; or @file / - for stdin
- `--out` VALUE (string); output directory for the PNGs (default: evidence/excerpts/<session>)
- `--settle` VALUE (duration); wait before collecting (fonts/images settle after load), e.g. 500ms
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `pdf preview`

Render PDF or office-document pages as PNG images. Positional arguments: 0..0. Required flags: --file.

Usage: `pdf preview --file <path|url> [--pages 1-3] [--dpi 150] [--out DIR] [--network-route system|direct]`

- `--file`: Local PDF path, or http(s) URL downloaded through the fetch network boundary; office documents (docx/pptx/xlsx/odt…) are converted to PDF via libreoffice automatically.

Page-image verb (no browser session): renders chosen pages of a local or downloaded PDF (office documents convert first, kind reported in source.kind) as PNG files for reading/preview. Needs poppler-utils; office input additionally needs libreoffice (both fail loud with install hints when missing). Pages are 1-based; --pages takes comma-separated ranges like 2,5-7 clamped to the page count. Default dpi 150 (36–600). This is static rendering, not UI observation: it cannot prove interactive behavior.

Example: `dw-browser pdf preview --file paper.pdf --pages 1-3 --out ./pages`

Example: `dw-browser pdf preview --file https://arxiv.org/pdf/2610.17309 --pages 1`

- `--file` VALUE (string); local PDF/office file path or http(s) URL
- `--pages` VALUE (string); 1-based pages: "3" or "1-3" or "2,5-7" (default 1)
- `--dpi` VALUE (integer); render resolution 36–600 (default 150)
- `--out` VALUE (string); output directory (default evidence/pdf-preview)
- `--network-route` VALUE (string); URL download route: system (default, honors validated proxy config) or direct (ignores proxies, explicit choice)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `pdf excerpt`

Crop annotated PNG screenshot excerpts from PDF pages by phrase or box. Positional arguments: 0..0. Required flags: --file, --spec.

Usage: `pdf excerpt --file <path|url> --spec '<json array | @file | ->' [--out DIR] [--network-route system|direct]`

- `--spec`: JSON array; each entry becomes one cropped PNG screenshot from a PDF page: {name, page, phrases:[...], boxes:[[x0,y0,x1,y1],...] in PDF points (72dpi space, origin top-left), box_style:{color:"#rrggbb", width:int(render px)}, highlight_color:"#rrggbb", crop_margin:int(render px at 200 dpi)}. phrases are matched as a contiguous word sequence (NFKC-normalized, typographic dashes/quotes folded to ASCII, punctuation-affix tolerant) via poppler word bounding boxes — multi-line matches highlight each matched word. The red box is drawn around the union of boxes and matched words; crop adds a margin around it.

Screenshot verb (static PDF counterpart of excerpt): crops annotated PNG excerpts from PDF pages — red box around the target union, translucent highlighter over matched phrases — for quoting paper originals into markdown when the HTML version is missing or loses figures. Same style defaults as excerpt (red 3px, #ffe94d, 14px margin). Word-level matching means a phrase must appear verbatim (after normalization); unmatched phrases are explicit per-entry results in missing (exit stays 0), entries with no match and no boxes produce no file. page_out_of_range is reported in warnings. Needs poppler-utils; office input additionally needs libreoffice. Rendering runs at a fixed 200 dpi for text sharpness. Not a UI observation.

Example: `dw-browser pdf excerpt --file paper.pdf --spec '[{"name":"latency-table","page":7,"phrases":["Single-pass decision readouts"],"boxes":[[60,95,540,320]]}]'`

- `--file` VALUE (string); local PDF/office file path or http(s) URL
- `--spec` VALUE (string); JSON array of entries: [{name, page, phrases?, boxes?, box_style?, highlight_color?, crop_margin?}]; or @file / - for stdin
- `--out` VALUE (string); output directory (default evidence/pdf-excerpts)
- `--network-route` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `check`

Judge required assertions against one canonical observation. Positional arguments: 0..0. Required flags: .

Usage: `check --id NAME [--observation ID] [--assert EXPR | --file ASSERT.json] [--using vision-url ...]`


Judges required assertions against one canonical observation. Exit 0 only for required PASS, 1 for FAIL, 2 for BLOCKED/runtime/evidence/argument errors — a green exit means the assertion held, not that the product is accepted. Vision-backed assertions need explicit provider configuration.

Example: `dw-browser check --id chat1 --assert 'text_contains:Welcome'`

- `--id` VALUE (session-id)
- `--observation` VALUE (string)
- `--assert` VALUE (string); required assertion expression
- `--file` VALUE (string); assertions JSON file
- `--using` VALUE (string); oracle selector (e.g. vision provider)
- `--vision-url` VALUE (string)
- `--vision-provider` VALUE (string)
- `--vision-model` VALUE (string)
- `--vision-api-key` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--id --observation] requires=[] forbids=[].
- Rule: when="" unless="" exactly-one=[--assert --file] requires=[] forbids=[].

## `diff`

Compare two canonical observations. Positional arguments: 2..2. Required flags: .

Usage: `diff OBSERVATION_A OBSERVATION_B`


Compares two canonical observations (same session) element-by-element; diagnostic only, never recovers a runtime. Use to see what an action actually changed between two observes.

Example: `dw-browser diff obs-1 obs-2`

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `wait`

Wait for one assertion or a declared duration. Positional arguments: 0..0. Required flags: --id.

Usage: `wait --id NAME (--assert EXPR | --duration 2s) [--timeout 10s]`


Waits until one assertion holds or a declared duration elapses, then returns. Prefer --assert over fixed durations; --duration is for settle-style pauses only.

Example: `dw-browser wait --id chat1 --assert 'visible:@r3' --timeout 10s`

Example: `dw-browser wait --id chat1 --duration 1s`

- `--id` VALUE (session-id)
- `--assert` VALUE (string); assertion expression to wait for (preferred)
- `--timeout` VALUE (duration); maximum wait
- `--duration` VALUE (duration); fixed settle pause (e.g. 1s)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--assert --duration] requires=[] forbids=[].
- Rule: when="--assert" unless="" exactly-one=[] requires=[--timeout] forbids=[].
- Rule: when="--duration" unless="" exactly-one=[] requires=[] forbids=[--timeout].

## `journey`

Run typed deterministic steps through the shared runtime. Positional arguments: 0..0. Required flags: --file.

Usage: `journey --file STEPS.yaml [--scenario NAME] [--id NAME] [--base-url URL] [--fail-fast]`


Runs typed deterministic steps through the shared runtime with persistent locators and assertions. Use app-test-baseline for deterministic regression: internal LLM use is forbidden and element steps are synthetic by explicit declaration. A candidate trace is not an accepted baseline — acceptance is a separate human decision. The YAML shape is shown by `protocol`.

Example: `dw-browser journey --file journey.yaml --scenario app-test-baseline --evidence runs/today`

- `--scenario` VALUE (string); values app-test-explore|app-test-baseline|webvisit
- `--mode` VALUE (string); values headless|headed|visible
- `--persona` VALUE (string)
- `--engine` VALUE (string); values chrome|safari
- `--safari-device` VALUE (string)
- `--viewport` VALUE (string)
- `--profile` VALUE (string)
- `--evidence` VALUE (string)
- `--allow-host` VALUE (string)
- `--browser-chrome` VALUE (string); values auto|on|off
- `--user-agent` VALUE (string)
- `--goal` VALUE (string)
- `--stealth`
- `--id` VALUE (session-id)
- `--file` VALUE (string); typed YAML steps (shape shown by `protocol`)
- `--base-url` VALUE (string); base URL for relative step targets
- `--fail-fast`; stop at first failing step
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="--id" unless="" exactly-one=[] requires=[] forbids=[--scenario --mode --persona --engine --safari-device --viewport --profile --evidence --allow-host --browser-chrome --user-agent --goal --stealth].
- Rule: when="" unless="--id" exactly-one=[] requires=[--scenario] forbids=[].

## `eval`

Run gated JavaScript debugging and revoke old observation authority. Positional arguments: 1..1. Required flags: --id.

Usage: `eval --id NAME '<js-expression>'`


Gated debugging: runs JavaScript in the page and revokes observation authority (it may mutate the page). Requires write permission for the origin (webvisit needs --allow-host). Debug/synthetic evidence cannot establish Human experience — witness work uses real visible interactions (act) instead. Prefer read verbs (observe/excerpt/document) for reading.

Example: `dw-browser eval --id chat1 'location.href'`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `cookie-import`

Import native browser cookies with keychain decryption and a domain filter. Positional arguments: 0..0. Required flags: --id.

Usage: `cookie-import --id NAME [--browser chrome|firefox] [--domain FILTER]`


Imports cookies from the local native browser profile (keychain-decrypted) into this session — for reaching login-gated content you already hold. --domain limits the import scope.

Example: `dw-browser cookie-import --id chat1 --browser chrome --domain example.com`

- `--id` VALUE (session-id)
- `--browser` VALUE (string)
- `--domain` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `audit`

Audit compatibility, accessibility, layout, performance and browser chrome rules. Positional arguments: 0..0. Required flags: --id.

Usage: `audit --id NAME [--suite NAME] [--check NAME] [--threshold N] [--format json]`


Deterministic rule audit over the current observation: compatibility, accessibility, layout, performance and browser-chrome rules (e.g. browser-chrome-occlusion on mobile persona). Machine-checkable only — it complements, never replaces, human-facing acceptance.

Example: `dw-browser audit --id chat1 --check browser-chrome-occlusion`

- `--id` VALUE (session-id)
- `--suite` VALUE (string)
- `--check` VALUE (string)
- `--threshold` VALUE (integer)
- `--format` VALUE (string); values json|compact
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `session status`

Inspect persisted runtime ownership without recovery. Positional arguments: 0..0. Required flags: --id.

Usage: `session status --id NAME`


Inspects the persisted runtime ownership (authority, observation validity) without recovering or touching the runtime. Diagnostic only.

Example: `dw-browser session status --id chat1`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `session list`

List persisted sessions. Positional arguments: 0..0. Required flags: .

Usage: `session list`


Lists persisted sessions with their state. Diagnostic only — never recovers a runtime.

Example: `dw-browser session list`

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `session viewport`

Resize the existing runtime and persist its environment. Positional arguments: 0..0. Required flags: --id, --size.

Usage: `session viewport --id NAME --size WxH`


Resizes the existing runtime viewport and persists the environment change. Reobserve after resizing before using refs or coordinates. Bare --viewport on open is rejected for mobile sizes — use --persona mobile for real mobile fidelity.

Example: `dw-browser session viewport --id chat1 --size 880x720`

- `--id` VALUE (session-id)
- `--size` VALUE (string); new viewport WxH; reobserve afterwards
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `tabs list`

List user page targets without recovering the runtime. Positional arguments: 0..0. Required flags: --id.

Usage: `tabs list --id NAME`


Lists the user page targets in the session browser without recovering the runtime. Diagnostic only.

Example: `dw-browser tabs list --id chat1`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `tabs select`

Activate an existing target under the runtime input lock. Positional arguments: 0..0. Required flags: --id, --target.

Usage: `tabs select --id NAME --target TARGET`


Activates an existing target under the runtime input lock. Reobserve after switching before using any ref — refs are per-target.

Example: `dw-browser tabs select --id chat1 --target t2`

- `--id` VALUE (session-id)
- `--target` VALUE (string); target id from tabs list
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `tabs close`

Close a target and revoke page authority. Positional arguments: 0..0. Required flags: --id, --target.

Usage: `tabs close --id NAME --target TARGET`


Closes one page target and revokes page authority. Any prior observation of that target is dead.

Example: `dw-browser tabs close --id chat1 --target t2`

- `--id` VALUE (session-id)
- `--target` VALUE (string); target id from tabs list
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `tabs new`

Create a target in the existing browser. Positional arguments: 0..0. Required flags: --id.

Usage: `tabs new --id NAME --url URL`


Creates a new page target in the existing session browser and focuses it. Reobserve before interacting — the new target has no refs yet.

Example: `dw-browser tabs new --id chat1 --url http://localhost:8080/portal/workspace`

- `--id` VALUE (session-id)
- `--url` VALUE (string); URL for the new target; reobserve afterwards
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `htr status`

Inspect Human control and runtime state without recovery. Positional arguments: 0..0. Required flags: --id.

Usage: `htr status --id NAME`


Inspects Human/AI authority state without recovery. Diagnostic only.

Example: `dw-browser htr status --id chat1`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `htr takeover`

Set Human/AI authority; obsolete observations are revoked. Positional arguments: 0..0. Required flags: --id.

Usage: `htr takeover --id NAME`


Really blocks AI input: the human physically owns the page from here. Obsolete observations are revoked.

Example: `dw-browser htr takeover --id chat1`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `htr yield`

Set Human/AI authority; obsolete observations are revoked. Positional arguments: 0..0. Required flags: --id.

Usage: `htr yield --id NAME`


Restores AI authority after a takeover and requires fresh observation before further acts.

Example: `dw-browser htr yield --id chat1`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `htr share`

Set Human/AI authority; obsolete observations are revoked. Positional arguments: 0..0. Required flags: --id.

Usage: `htr share --id NAME`


Shared authority mode; obsolete observations are revoked.

Example: `dw-browser htr share --id chat1`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `persona list`

List environment personas. Positional arguments: 0..0. Required flags: .

Usage: `persona list`


Lists available environment personas (device fidelity bundles). Use mobile for mobile-UX acceptance; desktop for PC.

Example: `dw-browser persona list`

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `profile list`

List persistent and temporary profile directories. Positional arguments: 0..0. Required flags: .

Usage: `profile list`


Lists persistent and temporary browser profile directories known to this CLI. Diagnostic.

Example: `dw-browser profile list`

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `profile import`

Copy a profile into the CLI profile store. Positional arguments: 1..1. Required flags: .

Usage: `profile import NAME`


Copies a profile directory into the CLI profile store for reuse by named profiles.

Example: `dw-browser profile import myprofile`

- `--name` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `profile prune`

Prune task profile directories explicitly. Positional arguments: 0..0. Required flags: --ephemeral.

Usage: `profile prune --ephemeral [--min-age DURATION] [--dry-run] [--reap-orphaned]`


Explicitly prunes task profile directories. --dry-run first to preview; --reap-orphaned cleans orphans left by dead processes.

Example: `dw-browser profile prune --ephemeral --dry-run`

- `--min-age` VALUE (duration)
- `--ephemeral`
- `--dry-run`
- `--reap-orphaned`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `gc`

Inspect or reclaim unowned browser resources explicitly. Positional arguments: 0..0. Required flags: .

Usage: `gc [--older-than DURATION] [--dry-run] [--all]`


Inspects or reclaims unowned browser resources (runtimes left behind by dead owners). --dry-run first; reclaims only unowned resources.

Example: `dw-browser gc --dry-run`

- `--older-than` VALUE (duration)
- `--dry-run`
- `--all`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `skills list`

List installed site recipes. Positional arguments: 0..0. Required flags: .

Usage: `skills list`


Lists installed site recipes (per-site interaction recipes the CLI can use). Diagnostic.

Example: `dw-browser skills list`

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `skills read`

Read one site recipe. Positional arguments: 1..1. Required flags: .

Usage: `skills read NAME`


Reads one site recipe. Diagnostic.

Example: `dw-browser skills read example`

- `--action` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `skills write`

Write a site recipe from declared input. Positional arguments: 1..1. Required flags: --action.

Usage: `skills write --action PATH`


Writes a site recipe from declared input.

Example: `dw-browser skills write --action recipe.json`

- `--action` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `record start`

Start a structured action trace. Positional arguments: 0..0. Required flags: --id.

Usage: `record start --id NAME`


Starts a structured action trace on the session (for evidence and replay analysis).

Example: `dw-browser record start --id chat1`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `record stop`

Stop and return the trace. Positional arguments: 0..0. Required flags: --id.

Usage: `record stop --id NAME`


Stops the trace and returns it.

Example: `dw-browser record stop --id chat1`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `record export`

Return the current trace. Positional arguments: 0..0. Required flags: --id.

Usage: `record export --id NAME`


Returns the current trace without stopping. Diagnostic.

Example: `dw-browser record export --id chat1`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `muxhost serve`

Run the existing headed host infrastructure. Positional arguments: 0..0. Required flags: .

Usage: `muxhost serve`


Runs the existing headed host infrastructure (shared headed display host for headed sessions).

Example: `dw-browser muxhost serve`

- `--muxhost-id` VALUE (string)
- `--runtime-id` VALUE (string)
- `--browser-session-id` VALUE (string)
- `--kind` VALUE (string); values task|interactive|service|debug|test
- `--profile-id` VALUE (string)
- `--profile-dir` VALUE (string)
- `--chrome-path` VALUE (string)
- `--debug-port` VALUE (integer)
- `--owner-pid` VALUE (integer)
- `--owner` VALUE (string)
- `--isolation` VALUE (string)
- `--mode` VALUE (string); values headless|headed|visible
- `--goal` VALUE (string)
- `--idle-ttl` VALUE (duration)
- `--persona` VALUE (string); empty string permitted
- `--preset` VALUE (string); empty string permitted
- `--width` VALUE (integer)
- `--height` VALUE (integer)
- `--user-agent` VALUE (string)
- `--service` VALUE (string)
- `--account` VALUE (string)
- `--identity-key` VALUE (string)
- `--touch`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `muxhost ensure`

Ensure an explicitly named headed host. Positional arguments: 0..0. Required flags: .

Usage: `muxhost ensure`


Ensures an explicitly named headed host exists (starts if missing).

Example: `dw-browser muxhost ensure`

- `--muxhost-id` VALUE (string)
- `--runtime-id` VALUE (string)
- `--browser-session-id` VALUE (string)
- `--kind` VALUE (string); values task|interactive|service|debug|test
- `--profile-id` VALUE (string)
- `--profile-dir` VALUE (string)
- `--chrome-path` VALUE (string)
- `--debug-port` VALUE (integer)
- `--owner-pid` VALUE (integer)
- `--owner` VALUE (string)
- `--isolation` VALUE (string)
- `--mode` VALUE (string); values headless|headed|visible
- `--goal` VALUE (string)
- `--idle-ttl` VALUE (duration)
- `--persona` VALUE (string); empty string permitted
- `--preset` VALUE (string); empty string permitted
- `--width` VALUE (integer)
- `--height` VALUE (integer)
- `--user-agent` VALUE (string)
- `--service` VALUE (string)
- `--account` VALUE (string)
- `--identity-key` VALUE (string)
- `--touch`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `muxhost status`

Inspect an existing headed host. Positional arguments: 0..0. Required flags: .

Usage: `muxhost status [--muxhost-id ID | --runtime-id ID | --browser-session-id ID]`


Inspects an existing headed host. Diagnostic.

Example: `dw-browser muxhost status`

- `--muxhost-id` VALUE (string)
- `--runtime-id` VALUE (string)
- `--browser-session-id` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--muxhost-id --runtime-id --browser-session-id] requires=[] forbids=[].

## `muxhost release`

Release an existing headed host lease. Positional arguments: 0..0. Required flags: .

Usage: `muxhost release [--muxhost-id ID | --runtime-id ID | --browser-session-id ID]`


Releases a headed host lease.

Example: `dw-browser muxhost release --runtime-id r1`

- `--muxhost-id` VALUE (string)
- `--runtime-id` VALUE (string)
- `--browser-session-id` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--muxhost-id --runtime-id --browser-session-id] requires=[] forbids=[].

## `muxhost shutdown`

Shut down the explicitly named headed host. Positional arguments: 0..0. Required flags: .

Usage: `muxhost shutdown [--muxhost-id ID | --runtime-id ID | --browser-session-id ID]`


Shuts down the explicitly named headed host.

Example: `dw-browser muxhost shutdown --muxhost-id m1`

- `--muxhost-id` VALUE (string)
- `--runtime-id` VALUE (string)
- `--browser-session-id` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--muxhost-id --runtime-id --browser-session-id] requires=[] forbids=[].

## `version`

Return the candidate protocol version. Positional arguments: 0..0. Required flags: .

Usage: `version`


Returns protocol and executable version as JSON. Diagnostic.

Example: `dw-browser version`

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `capabilities`

Return this executable command and action contract. Positional arguments: 0..0. Required flags: .

Usage: `capabilities`


Returns this executable's full command and action contract (machine-readable). Diagnostic.

Example: `dw-browser capabilities`

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `export-contract`

Generate same-version capabilities, completions and tool-use skill. Positional arguments: 0..0. Required flags: --directory.

Usage: `export-contract --directory DIR`


Regenerates the same-version contract artifacts (capabilities.json, completions, tool-use skill) into a directory. Run after any command-surface change so the checked-in contract stays green in tests.

Example: `dw-browser export-contract --directory guide/runtime-v2/generated`

- `--directory` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `completion`

Generate shell completion from this command contract. Positional arguments: 1..1. Required flags: .

Usage: `completion SHELL`


Generates shell completion for the current command contract. Diagnostic.

Example: `dw-browser completion zsh`

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `protocol`

Generate the same-version tool-use protocol. Positional arguments: 0..0. Required flags: .

Usage: `protocol`


Generates the same-version tool-use protocol (the AI-facing usage narrative with a journey example). Diagnostic.

Example: `dw-browser protocol`

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act click`

Click a visible target; count 2 or button right selects its mouse variant. Positional arguments: 0..1. Required flags: --id.

Usage: `act click [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Clicks the target center (respects occlusion: a click blocked by browser chrome fails loud).

Example: `dw-browser act click @r3 --id chat1`

Example: `dw-browser act click css=#submit --id chat1`

- `--id` VALUE (session-id)
- `--x` VALUE (number)
- `--y` VALUE (number)
- `--unit` VALUE (string); values css-px|percent
- `--frame` VALUE (string); values viewport|target
- `--observation-id` VALUE (string)
- `--button` VALUE (string); values left|right
- `--count` VALUE (integer); values 1|2
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="--x" unless="" exactly-one=[] requires=[--y] forbids=[].
- Rule: when="--y" unless="" exactly-one=[] requires=[--x] forbids=[].
- Rule: when="--unit" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--frame" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--observation-id" unless="" exactly-one=[] requires=[--x --y] forbids=[].

## `act tap`

Touch a visible target. Positional arguments: 0..1. Required flags: --id.

Usage: `act tap [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Mobile-persona tap; same targeting rules as click.

Example: `dw-browser act tap @r1 --id chat1`

- `--id` VALUE (session-id)
- `--x` VALUE (number)
- `--y` VALUE (number)
- `--unit` VALUE (string); values css-px|percent
- `--frame` VALUE (string); values viewport|target
- `--observation-id` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="--x" unless="" exactly-one=[] requires=[--y] forbids=[].
- Rule: when="--y" unless="" exactly-one=[] requires=[--x] forbids=[].
- Rule: when="--unit" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--frame" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--observation-id" unless="" exactly-one=[] requires=[--x --y] forbids=[].

## `act hover`

Move the pointer to a visible target. Positional arguments: 0..1. Required flags: --id.

Usage: `act hover [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Hovers the target; use for menus/tooltips that need pointer presence.

Example: `dw-browser act hover @r1 --id chat1`

- `--id` VALUE (session-id)
- `--x` VALUE (number)
- `--y` VALUE (number)
- `--unit` VALUE (string); values css-px|percent
- `--frame` VALUE (string); values viewport|target
- `--observation-id` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="--x" unless="" exactly-one=[] requires=[--y] forbids=[].
- Rule: when="--y" unless="" exactly-one=[] requires=[--x] forbids=[].
- Rule: when="--unit" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--frame" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--observation-id" unless="" exactly-one=[] requires=[--x --y] forbids=[].

## `act fill`

Replace editable text using verified physical selection and input. Positional arguments: 0..1. Required flags: --id.

Usage: `act fill [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Replaces the field content (selects all then types). Input targets come from observe refs; `fill @rN` is the working form. --sensitive requires a masked password control and suppresses text facts. Strict fill verifies physical selection before deletion.

Example: `dw-browser act fill @r5 --text "hello" --id chat1`

- `--id` VALUE (session-id)
- `--text` VALUE (string); empty string permitted
- `--text-file` VALUE (string)
- `--sensitive`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--text --text-file] requires=[] forbids=[].
- Rule: when="--text" unless="" exactly-one=[] requires=[] forbids=[--text-file].

## `act type`

Insert text into a target or the physically focused editor. Positional arguments: 0..1. Required flags: --id.

Usage: `act type [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Types text; --focused inserts into physical focus without changing selection.

Example: `dw-browser act type @r1 --id chat1`

- `--id` VALUE (session-id)
- `--text` VALUE (string); empty string permitted
- `--text-file` VALUE (string)
- `--focused`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--text --text-file] requires=[] forbids=[].
- Rule: when="--text" unless="" exactly-one=[] requires=[] forbids=[--text-file].

## `act press`

Dispatch a platform key or shortcut. Positional arguments: 0..1. Required flags: --id.

Usage: `act press [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Presses a key or chord, e.g. `press Control+b`, `press Enter`; modifiers are real CDP modifier masks, not characters.

Example: `dw-browser act press Control+b --id chat1`

- `--id` VALUE (session-id)
- `--key` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act focus`

Physically focus an editable target. Positional arguments: 0..1. Required flags: --id.

Usage: `act focus [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Focuses the target element.

Example: `dw-browser act focus @r1 --id chat1`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act select`

Select a native option using verified keyboard interaction; explicit baseline element steps provide synthetic functional evidence. Positional arguments: 1..1. Required flags: --id.

Usage: `act select TARGET --id SESSION (--label LABEL | --value VALUE)`

- `TARGET`: Control ref returned by observe, or an explicit css= or role= locator for the select control.

Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Native select by exact complete displayed option label (--label) or exact DOM option value (--value) — exactly one selector, including an explicit empty string; no prefix or first-match fallback. First observe the control, read its option labels/values, choose one field, execute, then observe again.

Example: `dw-browser act select @r7 --label "完整选项文案" --id chat1`

Example: `dw-browser act select <CONTROL_REF> --id <SESSION> --value '<EXACT_DOM_VALUE_FROM_OBSERVE>'`

- `--id` VALUE (session-id); Session ID used by observe; this is not the control target.
- `--label` VALUE (string); empty string permitted; Exact complete displayed option label only; no DOM-value fallback. Supply exactly one of --label or --value.
- `--value` VALUE (string); empty string permitted; Exact DOM option value only; no displayed-label fallback. Supply exactly one of --label or --value.
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--label --value] requires=[] forbids=[].

## `act set-checked`

Set a checkbox to an explicit boolean. Positional arguments: 0..1. Required flags: --id.

Usage: `act set-checked [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Sets checkbox/radio state; argument is the desired state.

Example: `dw-browser act set-checked @r1 --id chat1`

- `--id` VALUE (session-id)
- `--checked` VALUE (string); values true|false
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act scroll`

Scroll the viewport or target at a declared point. Positional arguments: 0..1. Required flags: --id.

Usage: `act scroll [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Scrolls (e.g. `scroll down 3`); pure-coordinate forms exist for canvas targets.

Example: `dw-browser act scroll down 3 --id chat1`

- `--id` VALUE (session-id)
- `--x` VALUE (number)
- `--y` VALUE (number)
- `--unit` VALUE (string); values css-px|percent
- `--frame` VALUE (string); values viewport|target
- `--observation-id` VALUE (string)
- `--direction` VALUE (string); values up|down|left|right
- `--steps` VALUE (integer)
- `--delta-x` VALUE (number)
- `--delta-y` VALUE (number)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="--x" unless="" exactly-one=[] requires=[--y] forbids=[].
- Rule: when="--y" unless="" exactly-one=[] requires=[--x] forbids=[].
- Rule: when="--unit" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--frame" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--observation-id" unless="" exactly-one=[] requires=[--x --y] forbids=[].

## `act scrollto`

Scroll a persistent target into view. Positional arguments: 0..1. Required flags: --id.

Usage: `act scrollto [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Scrolls a locator or coordinates into view.

Example: `dw-browser act scrollto @r1 --id chat1`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act drag`

Drag between two points in one coordinate frame. Positional arguments: 0..1. Required flags: --id.

Usage: `act drag [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Drags from target/coordinates to an end point (--end-x/--end-y).

Example: `dw-browser act drag @r1 --id chat1`

- `--id` VALUE (session-id)
- `--x` VALUE (number)
- `--y` VALUE (number)
- `--unit` VALUE (string); values css-px|percent
- `--frame` VALUE (string); values viewport|target
- `--observation-id` VALUE (string)
- `--end-x` VALUE (number)
- `--end-y` VALUE (number)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="--x" unless="" exactly-one=[] requires=[--y] forbids=[].
- Rule: when="--y" unless="" exactly-one=[] requires=[--x] forbids=[].
- Rule: when="--unit" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--frame" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--observation-id" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--end-x" unless="" exactly-one=[] requires=[--end-y --x --y] forbids=[].
- Rule: when="--end-y" unless="" exactly-one=[] requires=[--end-x --x --y] forbids=[].

## `act swipe`

Swipe between two touch points. Positional arguments: 0..1. Required flags: --id.

Usage: `act swipe [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Mobile-persona swipe gesture.

Example: `dw-browser act swipe @r1 --id chat1`

- `--id` VALUE (session-id)
- `--x` VALUE (number)
- `--y` VALUE (number)
- `--unit` VALUE (string); values css-px|percent
- `--frame` VALUE (string); values viewport|target
- `--observation-id` VALUE (string)
- `--end-x` VALUE (number)
- `--end-y` VALUE (number)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="--x" unless="" exactly-one=[] requires=[--y] forbids=[].
- Rule: when="--y" unless="" exactly-one=[] requires=[--x] forbids=[].
- Rule: when="--unit" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--frame" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--observation-id" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--end-x" unless="" exactly-one=[] requires=[--end-y --x --y] forbids=[].
- Rule: when="--end-y" unless="" exactly-one=[] requires=[--end-x --x --y] forbids=[].

## `act navigate`

Navigate the existing session to a URL. Positional arguments: 0..1. Required flags: --id.

Usage: `act navigate [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Navigates the page to a URL; refs die afterwards.

Example: `dw-browser act navigate https://example.com --id chat1`

- `--id` VALUE (session-id)
- `--url` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act reload`

Reload the existing document. Positional arguments: 0..1. Required flags: --id.

Usage: `act reload [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Reloads the current page; refs die afterwards.

Example: `dw-browser act reload @r1 --id chat1`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act back`

Traverse browser history backwards. Positional arguments: 0..1. Required flags: --id.

Usage: `act back [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Browser back; refs die afterwards.

Example: `dw-browser act back @r1 --id chat1`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act forward`

Traverse browser history forwards. Positional arguments: 0..1. Required flags: --id.

Usage: `act forward [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Browser forward; refs die afterwards.

Example: `dw-browser act forward @r1 --id chat1`

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act zoom`

Set page scale. Positional arguments: 0..1. Required flags: --id.

Usage: `act zoom [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Enters/adjusts the visual zoom state (persona chrome stays fixed); `zoom reset` restores.

Example: `dw-browser act zoom @r1 --id chat1`

- `--id` VALUE (session-id)
- `--value` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act keyboard`

Show or hide the emulated mobile keyboard. Positional arguments: 0..1. Required flags: --id.

Usage: `act keyboard [TARGET] --id NAME [verb flags — see flags below]`


Typed interaction verb through the shared action contract. A TARGET is a returned opaque `@...` ref from the latest observe, an explicit `css=...`/`role=...` locator, or coordinates with --x/--y (plus --unit css-px --frame viewport --observation-id CAPTURE for coordinates). Observe first, act, then reobserve: refs are bound to one observation and die on navigation/tab switch/resize. Literal text is never an action string. Unknown or partially dispatched acts require observation before retry. Opens or dismisses the simulated on-screen keyboard (mobile persona state).

Example: `dw-browser act keyboard @r1 --id chat1`

- `--id` VALUE (session-id)
- `--value` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
