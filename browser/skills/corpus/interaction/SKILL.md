---
name: interaction
description: Use DW-Browser v2 physical input for Shadow DOM, iframes, scrolling and consent controls; keep debug interventions separate from Witness experience.
actions: [shadow-dom, iframe, cross-origin-iframe, virtual-scroll, cookie-consent, anti-bot]
---

# Browser interaction techniques

## shadow-dom
intent: Operate the visible Shadow DOM control through physical input

Observe and read the actual PNG. Prefer a granted visible ref; otherwise use explicit screenshot coordinates. The browser dispatches physical pointer/keyboard events, with target/focus/editability verified by the common kernel. A DOM focus/value setter or synthetic click is not an input-failure fallback.

## iframe
intent: Operate a visible same-origin iframe control

Use its actual visible target or screenshot geometry. Do not replace interaction with a contentDocument setter/click. Debug `eval --id NAME EXPRESSION` is a separately gated intervention, and revokes prior authority.

## cross-origin-iframe
intent: Operate the visible control in a cross-origin frame

Read the PNG and use an explicitly bound coordinate request. The current effective-origin gate remains authoritative; an opaque origin is not silently trusted. No DOM access or synthetic interaction is substituted when physical input is blocked.

## virtual-scroll
intent: Expose a virtual-list item through real scrolling

```sh
dw-browser act scroll --id NAME --direction down
dw-browser observe --id NAME
```

Only rendered items exist in a virtual list. Use `scrollto` for an existing persistent target; it cannot create a virtualized item. Explicit SUT CSS probes plus a bounded assertion wait can validate readiness. A duration by itself does not establish ready.

## cookie-consent
intent: Operate the user's authorized cookie-consent choice

Observe the actual current wording (Accept all/Accept cookies/I agree etc.), then use a granted token or explicit persistent locator. Account/site choices remain the user's task; publishing/sending is a separate authorized operation. Reobserve after scrolling, overlays and modal changes.

## anti-bot
intent: Keep interaction faithful to the actual browser

Use typed physical input and the creation persona. `app-test-explore` preserves fidelity; `app-test-baseline` is deterministic and forbids internal LLM. `webvisit` retains origin write gating and explicit allow-host authorization. Image and delivered postconditions establish what happened; DOM preparation and hidden retries cannot stand in for that experience.
