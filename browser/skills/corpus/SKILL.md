---
name: dw-browser-skills
description: Read and author website-specific typed browser recipes when using DW-Browser v2; recipes provide planning material without runtime input fallback.
---

# DW-Browser site recipes

Read `skills list` and `skills read DOMAIN --action NAME` before using relevant site knowledge. Observe the actual page and revalidate old selectors/site evidence; protocol migration does not imply the website was reverified.

Site sections keep intent, preconditions, meaningful gotchas and a `dw-browser-plan` JSON fence. The JSON contains typed `action` and/or `wait` steps. For example:

```dw-browser-plan
{"steps":[{"action":{"kind":"fill","target":{"kind":"locator","locator":"css=#editor"},"text":"{message}"}},{"action":{"kind":"press","key":"Enter"}}]}
```

Substitute parameters in typed fields without tokenizing text, then place the accepted path into a v2 journey with persistent locators and explicit checks. CSS/HTTP/transcript probes are explicit SUT declarations, not default URL-chip inference. Unknown steps/facts cannot be silently skipped. Baseline execution has no internal LLM; plans never execute as an input-failure rescue.

`skills write DOMAIN --action NAME` reads intent/recipe/gotcha input from stdin and updates that site recipe. Write only requested or authorized discoveries. Keep the successful path and evidence-based constraints; ordinary failed attempts do not establish a universal workaround.

`record start|export|stop --id NAME` retains typed requests/outcomes and observation/runtime identities. Sensitive text is redacted. A trace/recipe candidate is not an accepted baseline. Publishing or sending must be authorized by the user task; a site recipe alone does not grant permission.

The `interaction` section covers physical input through Shadow DOM/iframes, virtual scrolling, consent and fidelity choices. Debug evaluation may write, is origin-gated and records intervention; it cannot count as an independent Witness's UI experience.
