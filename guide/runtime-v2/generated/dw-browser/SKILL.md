---
name: dw-browser
description: Observe and operate real browser sessions with the DW-Browser v2 CLI, preserving visible evidence, Human control and deterministic journey assertions. Use for browser tasks through this tool; ordinary software editing does not need it.
---

# DW-Browser 0.17.1-v2 tool protocol

Choose by task: DW-Browser primarily supports real Web experience, exploration and acceptance. Use `open` → `observe` → `act` for UI tasks (`once` for one capture), and typed `journey` steps with assertions for deterministic acceptance. Auxiliary background reads use `document URL` for rendered DOM or `fetch URL --save FILE` for an HTTP response body. Choose the required task and evidence explicitly; a successful read receipt cannot prove UI visibility, successful interaction or an accepted baseline.

If a required screenshot is unavailable, UI acceptance remains BLOCKED. Restore `observe` and inspect its actual PNG before continuing visual actions or acceptance; successful document/fetch reads cannot turn missing UI evidence into a PASS.

Create with `open URL --id NAME --scenario app-test-explore --persona desktop --mode headless --evidence DIR`. Creation environment is inherited by every later process. `open` rejects an existing ID. Use `session viewport --id NAME --size 880x720` to resize the same browser.

Use `observe --id NAME`. Read the returned real PNG before visual actions; the canonical observation path/id is the fact source. Channel states distinguish captured, unavailable and not-requested. An unavailable channel cannot prove zero errors. Empty refs can still be a valid static screenshot. Per-control `allowed_actions` comes from captured control state and the shared action declaration; unavailable state does not prove editability. Native select controls include bounded declared option labels/values and a `select` capability; options_complete=false means the list is incomplete. Copy a selectable choice's `select.options[].option` unchanged into canonical `Action.option`; for CLI map its sole `label` or `value` field directly to `--label` or `--value`, preserving the exact string including empty strings. A displayed choice with selectable=false is not an executable suggestion. Privacy or truncation can withhold choices; a selector explicitly known from the task still requires fresh exact-uniqueness validation. This data does not prove a visible native popup. Reobserve after an action/resize/tab/navigation/intervention before using a ref or screenshot coordinate. Tokens are opaque and bound to one observation, document, target and runtime generation.

Use `act VERB TARGET --id NAME` with typed flags. A target is a returned opaque `@...` token, an explicit `css=...` or `role=...` locator, or coordinates with `--x N --y N --unit css-px --frame viewport --observation-id CAPTURE`; target-relative points use `--unit percent --frame target` and the capture ID, unless their opaque ref already supplies it. Text uses exactly one of `--text` or UTF-8 `--text-file`; file trailing newlines are preserved. `fill` replaces, `type --focused` inserts into physical focus. `fill --sensitive` requires a masked password control and suppresses text facts/traces; unmasked destinations are rejected before focus/deletion, preserving real screenshots. Literal text is never an action string. Strict fill verifies physical selection before deletion. Date/time controls use explicitly synthetic element steps in an app-test-baseline journey; physical fill does not use a setter fallback. `act select TARGET --id SESSION --label LABEL` matches only the exact complete displayed option label; `--value VALUE` matches only the exact DOM option value. Supply exactly one selector, including an explicit empty string if needed. There is no cross-field fallback, prefix syntax or first-match choice for duplicates. TARGET is the control ref from observe or an explicit locator; --id identifies the session. First observe, read the control option projection, choose a label or DOM value without guessing, execute, then observe again. For a control named 来源, `dw-browser act select <CONTROL_REF> --id <SESSION> --label '完整内容'` uses placeholders from the observation; replace angle-bracket placeholders before execution. Selection uses native keyboard selection in visual mode; native failure never falls back to a setter. Native typeahead can emit intermediate changes; interrupted or partial input requires observation. Only explicit element steps in app-test-baseline use synthetic select. Receipts separate requested_fidelity and interaction_mode from actual execution/evidence_scope; keyboard-selection does not prove popup visibility, pointer hit targets or dismissal.

A command error reports phase/dispatch_state/recovery. Unknown or partial dispatch requires observation; do not automatically replay fill/submit. `eval` is gated debugging that may write and revokes observation authority. Witness work uses real visible interactions; debug/synthetic evidence cannot establish Human experience.

`htr takeover --id NAME` really blocks AI input; `htr yield --id NAME` restores authority and requires fresh observation. Status/help are diagnostic and never recover a runtime. Headless and intentionally visible native tabs need no display host; headed mode retains its verified display guard. Close only sessions you created with `close --id NAME`.

For interactive public browsing choose `webvisit`; writes retain origin/allow-host gating. That write policy is not a public-only network sandbox; for background DOM reads requiring the public-only request boundary, use `document`. This auxiliary read does not replace UI observation or interaction. For deterministic regression choose `app-test-baseline`; internal LLM use is forbidden. Use one typed `journey --file FILE` runner with persistent locators and assertions. A candidate trace is not an accepted baseline. `check` exits 0 only for required PASS, 1 for FAIL, 2 for BLOCKED/runtime/evidence/argument errors. Other exit 0 means complete delivery, not product acceptance. stdout is one JSON; `--output` saves that JSON, `--screenshot` names a PNG and creation `--evidence` names the evidence root.

For auxiliary background public DOM acquisition use `document URL --timeout 30s --max-html-bytes 8388608 --max-network-bytes 33554432`. `document` returns `dw.document.v1` with `content` and `representation=dom-html`, without screenshots or caller JavaScript. XML/RSS/Atom return an `unsupported_content_type` reason instead of a browser viewer masquerading as source. It owns a disposable browser and profile, never attaches an existing session or user profile, and grants no arbitrary eval. Default --mode headless; explicit --mode headed creates a native headful hidden target without a browser window or virtual display. Rendering mode and actual target surface are reported. It never silently switches modes; a challenge page is not usable article evidence, and changing mode is not a guaranteed remedy. Default --network-route system uses supported trusted system proxy configuration only after local public-IP validation and numeric CONNECT pinning; --network-route direct deliberately ignores proxy configuration. Proxy errors never fall back to direct. Ordinary failures report dw.document.failure.v1 diagnostics with usable=false, stage, reason, timing, policy and cleanup; they contain no partial DOM and grant no feed reroute. Its `page-requests-public-only` policy covers webpage requests through one Chrome-wide DNS-pinned guard; a configured system proxy is explicitly trusted to honor numeric CONNECT, and this is not an OS firewall. Child targets are not paused or reattached. Chrome's own sandbox stays enabled. Windows fails before launch because verified process-tree cleanup is unavailable. The returned `url` is the actual main-frame location; `declared_canonical_url` is only an untrusted DOM hint. DOM-ready is not a promise that site-specific async content is complete. Budgets fail rather than truncate.

For auxiliary HTTP body acquisition use `fetch URL --save FILE`: it saves the complete response body without JavaScript or Chrome (unrendered HTML, images or files). Fetch defaults to a 20s acquisition budget, 8388608 decoded body bytes (8 MiB), a 33554432-byte guard network-accounting budget (32 MiB), separate from decoded body bytes and including DNS traffic, and `--network-route system`; `direct` must be requested explicitly. It never changes tools or routes based on content type or failure. Example: `fetch https://example.com/image.png --save ./image.png --output ./image.meta.json`: --save receives complete body bytes, stdout and --output receive dw.resource.v1 JSON metadata with body_file. These paths must not alias, and parent directories must already exist. Body publication occurs atomically after network cleanup. Acquisition or body-persist failure returns dw.resource.failure.v1 diagnostics with usable=false and body_bytes_read, never a usable partial file. Delivery commits in order: body rename, optional --output metadata file, then stdout; each committed output remains if a later delivery fails. JSON encoding or metadata-write failures exit 2 and, when stdout works, report diagnostics preserving byte counts without body_file. A stdout failure returns exit 2 and stderr; an already committed metadata file may still contain successful body_file metadata. Inspect available committed metadata or settle budgets conservatively. The consumer verifies MIME, signature, status and meaning; a 200 challenge page is not proof of an image or article. Fetch accepts no session, browser mode, cookies, headers, method override, JavaScript or host grants.

 DNS resolver choice is per call: system by default, or an explicit public numeric-IP HTTPS /dns-query endpoint. Only use that endpoint when the caller explicitly chooses a public DNS strategy; never switch after a timeout. No resolver fallback, environment override or cross-call cache. Resolver DNS traffic shares the acquisition network-byte budget. A third-party resolver learns queried hostnames and the client/proxy source IP, not the original URL path or response content; existing proxy trust remains. TLS must validate its IP SAN. Explicit caller-authorized example: `fetch https://example.com/article --save ./article.html --dns-resolver https://1.1.1.1/dns-query`; the same --dns-resolver option is available on document only. An endpoint must be HTTPS with a literal public IP, default or 443 port, exact /dns-query path and no userinfo, query or fragment.

For exact required flags, positional counts, enums and conditional rules, read [the generated command reference](references/commands.md), or invoke `help COMMAND` / `capabilities` from this same executable.

## Deterministic editing journey

Adapt persistent CSS locators to the actual visible controls, then save this v2 YAML and run `journey --file FILE --scenario app-test-baseline --evidence DIR`. The wait/check assert saved text after navigation; changing input into a synthetic setter cannot stand in for visual editing.

```yaml
version: 2
id: edit-save-revisit
name: Edit, save, and revisit
environment:
    base_url: http://127.0.0.1:8080
    entry_url: /
    mode: headless
    viewport: 1280x720
    client: chrome
journey:
    - id: look
      kind: observe
    - id: edit
      kind: action
      action:
        kind: fill
        target:
            kind: locator
            locator: css=[data-testid='editor']
        text: A persisted edit
      mode: visual
    - id: save
      kind: action
      action:
        kind: click
        target:
            kind: locator
            locator: css=[data-testid='save']
      mode: visual
    - id: revisit
      kind: action
      action:
        kind: navigate
        url: http://127.0.0.1:8080/
      mode: visual
    - id: await-persisted
      kind: wait
      wait:
        assert: text_contains('A persisted edit')
        timeout_ms: 5000
    - id: assert-persisted
      kind: check
      check:
        - id: persisted
          assert: text_contains('A persisted edit')
          required: true
evidence:
    save:
        - screenshot
        - observation
        - report
        - trace
```
