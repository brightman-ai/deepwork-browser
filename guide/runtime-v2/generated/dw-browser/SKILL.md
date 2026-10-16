---
name: dw-browser
description: Observe and operate real browser sessions with the DW-Browser v2 CLI, preserving visible evidence, Human control and deterministic journey assertions. Use for browser tasks through this tool; ordinary software editing does not need it.
---

# DW-Browser 0.13.0-v2 tool protocol

Choose by task: UI experience, screenshots and interaction use `open` → `observe` → `act` (or `once` for one capture); deterministic acceptance uses `journey`; background public DOM acquisition uses `document URL --timeout 30s --max-html-bytes 8388608 --max-network-bytes 33554432`. `document` returns `dw.document.v1` with `content` and `representation=dom-html`, without screenshots or caller JavaScript. XML/RSS/Atom return an `unsupported_content_type` reason instead of a browser viewer masquerading as source. It owns an isolated disposable headless browser, never attaches a session or user profile, and grants no arbitrary eval. Its `page-requests-public-only` policy covers webpage requests through one Chrome-wide DNS-pinned proxy; it is not an OS firewall. Child targets are not paused or reattached. Chrome's own sandbox stays enabled. Windows fails before launch because verified process-tree cleanup is unavailable. The returned `url` is the actual main-frame location; `declared_canonical_url` is only an untrusted DOM hint. DOM-ready is not a promise that site-specific async content is complete. Budgets fail rather than truncate.

Create with `open URL --id NAME --scenario app-test-explore --persona desktop --mode headless --evidence DIR`. Creation environment is inherited by every later process. `open` rejects an existing ID. Use `session viewport --id NAME --size 880x720` to resize the same browser.

Use `observe --id NAME`. Read the returned real PNG before visual actions; the canonical observation path/id is the fact source. Channel states distinguish captured, unavailable and not-requested. An unavailable channel cannot prove zero errors. Empty refs can still be a valid static screenshot. Per-control `allowed_actions` comes from captured control state and the shared action declaration; unavailable state does not prove editability. Reobserve after an action/resize/tab/navigation/intervention before using a ref or screenshot coordinate. Tokens are opaque and bound to one observation, document, target and runtime generation.

Use `act VERB TARGET --id NAME` with typed flags. A target is a returned opaque `@...` token, an explicit `css=...` or `role=...` locator, or coordinates with `--x N --y N --unit css-px --frame viewport --observation-id CAPTURE`; target-relative points use `--unit percent --frame target` and the capture ID, unless their opaque ref already supplies it. Text uses exactly one of `--text` or UTF-8 `--text-file`; file trailing newlines are preserved. `fill` replaces, `type --focused` inserts into physical focus. `fill --sensitive` requires a masked password control and suppresses text facts/traces; unmasked destinations are rejected before focus/deletion, preserving real screenshots. Literal text is never an action string. Strict fill verifies physical selection before deletion. Date/time controls use explicitly synthetic element steps in an app-test-baseline journey; physical fill does not use a setter fallback.

A command error reports phase/dispatch_state/recovery. Unknown or partial dispatch requires observation; do not automatically replay fill/submit. `eval` is gated debugging that may write and revokes observation authority. Witness work uses real visible interactions; debug/synthetic evidence cannot establish Human experience.

`htr takeover --id NAME` really blocks AI input; `htr yield --id NAME` restores authority and requires fresh observation. Status/help are diagnostic and never recover a runtime. Headless and intentionally visible native tabs need no display host; headed mode retains its verified display guard. Close only sessions you created with `close --id NAME`.

For interactive public browsing choose `webvisit`; writes retain origin/allow-host gating. That write policy is not a public-only network sandbox; use `document` when that boundary is required. For deterministic regression choose `app-test-baseline`; internal LLM use is forbidden. Use one typed `journey --file FILE` runner with persistent locators and assertions. A candidate trace is not an accepted baseline. `check` exits 0 only for required PASS, 1 for FAIL, 2 for BLOCKED/runtime/evidence/argument errors. Other exit 0 means complete delivery, not product acceptance. stdout is one JSON; `--output` saves that JSON, `--screenshot` names a PNG and creation `--evidence` names the evidence root.

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
