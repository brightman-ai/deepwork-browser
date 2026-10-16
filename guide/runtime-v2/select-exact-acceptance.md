# Exact select acceptance — 0.15.0-v2

Scope: AI observes a native dropdown, expresses an exact choice, performs real
browser input and checks application feedback. The canonical `option` object is
shared by CLI, Go, HTTP and journeys; label and value are distinct selectors.
The prior native keyboard mechanism and explicit baseline policy are retained.

## Contract and regressions

- Explicit label/value field collision chooses the intended option in that field.
- Prefix, cross-field fallback and first-match duplicates are removed. Duplicate
  labels or values reject; an alternative unique field remains usable.
- Completely identical choices reject, including the currently selected choice.
  Hidden, disabled and projected-out duplicates still count for ambiguity.
- Missing/both selectors, duplicate flags, null/unknown/non-string fields,
  duplicate JSON keys and old select top-level `value` reject before input.
  Explicit empty strings survive CLI, JSON and YAML and remain selectable.
- Observation's bounded option object is directly consumable as `Action.option`.
  The emitted selector goes through the same exact matcher and execution planner.
- Native trusted events, application state, interrupted/cancelled input,
  subsequent click/type and explicit synthetic baseline remain independently checked.
- Cross-process actions inherit persisted session privacy before selection or
  result evidence. Redaction after password input withholds choices and result
  values while preserving normal dropdown operation.

The privacy check uncovered an existing propagation defect: observation refreshed
privacy from the session provider, while a newly attached action core could
produce selection evidence before snapshot refreshed it. The shared provider
refresh at the locked input boundary now applies the same sticky, fail-closed
privacy rule. This fixes execution and failure receipts without a CLI-only flag
or a synthetic fallback.

## Evidence status

Verified on 2026-10-16:

| Boundary | Result |
| --- | --- |
| Full real native race suite | PASS, live scenario 46.44s / package 48.006s; includes app-driven post-observation duplicate insertion and all prior native failure/privacy/baseline assertions |
| Exact selector, full-facts projection and wire race tests | PASS; collision, null/duplicate keys, empty values and 4,096-option facts |
| Shared session privacy race tests | PASS; sticky false→true→false, provider failure/nil and Human authority refusal |
| Root/CLI/journey/Safari adapter race packages | PASS; adapter contracts do not claim Safari runtime support |
| All-package standalone compilation | PASS; compilation is separate from runtime acceptance |
| Final installed serial CLI regression | PASS, 40 calls including owned-session cleanup; independent trusted input/change and visible application feedback |
| Independent Medium cold actor | PASS on original Voicemore full→realtime→full, highlight and subsequent content navigation; no implementation/spec read |
| Final installed binary | PASS, original source switches by exact label, explicit empty DOM value, missing-selector no-dispatch and owned cleanup |
| Final installed deterministic baseline | PASS: broken input BLOCKED (2), restored input PASS (0), wrong assertion FAIL (1), restored assertion PASS (0); healthy cases persisted exact UTF-8 bytes including trailing newline |

The full native race run preceded the shared privacy-provider correction. The
corrected runtime then passed the isolated cross-process privacy scenario and
complete CLI checks. The last binary adjustment only clarified help/protocol
mapping from the observed option object; native behavior was unchanged.

Final candidate and installed `~/.local/bin/dw-browser` SHA-256:

```text
4709226821c2e2ec3e7a972562919619c0b8bf813ebed4d3e8a95643814c5c04
```

The cold actor's full journey used SHA-256
`1181189057b5613a348b3359fe11cd4ae04c482ca8231ac78ee5c397829a1084`.
It separately read the final binary's help/protocol and verified its final hash;
this is not represented as a repeat journey. The parent repeated the actual
source-switch task on the final installed binary. The actor found the exact
label/value distinction understandable without author guidance.

Raw local receipts, screenshots and failed harness attempts remain private and
are not portable public artifacts. An initial dynamic-fixture test accidentally
revoked its ref through the public debug API; it was corrected to use the
application's own update signal, with the original strict rejection assertion
retained. Parent harness fixes accounted for nested outcomes and normalized
text whitespace; no runtime guard or expected selection was relaxed.

Runtime claims are limited to macOS arm64 / Chrome 155 headless keyboard
selection. Native popup pointer operations, Safari, headed execution and other
platforms are not certified by these checks. Unit adapter tests and compilation
are separate from actual browser evidence.

See [the current interaction contract](native-select.md). The
[0.14.0 native acceptance](native-select-acceptance.md) remains historical evidence
for the initial keyboard mechanism, not the new exact selector contract.

Reproduce the durable cross-process select regression with
`python3 tests/runtime-v2/select_exact.py /absolute/path/to/dw-browser`.
It starts a loopback fixture, pins the binary hash before/after the run, uses
actual CLI processes and trusted application events, and verifies owned cleanup
before writing a passing verdict. Pure/wire regression coverage lives in
`browser/select_option_selector_test.go`, `browser/session_privacy_test.go`,
the CLI select tests and `server_runtime_test.go`.
