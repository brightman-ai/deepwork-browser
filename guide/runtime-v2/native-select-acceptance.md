# Native select acceptance — 0.14.0-v2

Verified on 2026-10-16 using macOS arm64 and Chrome 155.0.8059.80 headless.
The claim is native keyboard selection, verified selection state and application
feedback. Native popup visibility, pointer option hit targets, dismissal, Safari,
headed execution and other operating systems require separate runtime evidence.

## Verified boundaries

| Check | Result |
| --- | --- |
| Real Chrome native-select race suite | PASS; 46.32 seconds for the live scenario, 48.519 seconds for the package |
| Planner, capability, empty/redacted values and receipt race checks | PASS, including final per-option capability projection |
| Root, CLI, journey/observation and Safari adapter unit packages | PASS; Safari native runtime is not claimed |
| All-package standalone compile (`GOWORK=off`) | PASS; compile-only, not a full test run |
| Existing deterministic edit/save baseline | Correct exits 2 / 0 / 1 / 0; healthy runs persisted exact UTF-8 bytes including trailing newline |
| Independent Medium cold witness | Completed the original Voicemore source-switch journey, highlight and subsequent content navigation |
| Installed final binary | Completed full → realtime → full on Voicemore with visible document changes; explicit empty-value selection changed fixture feedback; missing value rejected before dispatch |
| Resource ownership | Dedicated headless sessions closed; fixture server stopped; no user browser/profile used |

The live suite checks Chinese labels, shared prefixes, duplicate labels addressed
by value, disabled/hidden targets and placeholders, no initial selection, empty
values, state-only no-ops, occlusion, subsequent click/type, prevented keys,
replacement/lost focus/changed options, cancellation, privacy after password input,
and explicit synthetic baseline behavior. No native failure uses a setter rescue.
The HTTP tests cover receipt propagation through action, persistence and hook
failures, as well as preflight isolation from a previous action's receipt.

## Binary identity and evidence limits

The final candidate and installed `~/.local/bin/dw-browser` have matching SHA-256:

```text
41744b67fa75bea03debd01abe6793e7e3fb9d88a6928d49c95144bb45e7feb2
```

The independent cold witness used an earlier candidate with SHA-256
`33330c0676200cac86fc5a16f675fc27cb466d4829dfe28da46d2c00a381b2c5`.
Later changes preserved empty values in JSON, required explicit CLI `--value`,
and added per-option capability details. The final installed smoke separately
verified those changes and repeated the source switches on the original UI.
The cold witness is not represented as a run of the final binary.

The initial installation smoke harness omitted its desired reference because
ordinary observe output defaults to ten targets. The corrected harness used
`--top 50`; it passed without changing runtime behavior or its assertions.
Raw local JSON, screenshots, private fixture data and failed attempts are retained
outside the public repository; they are not portable public test artifacts.
The durable automated checks are in `browser/native_select_test.go`, the select
capability/CLI tests and the HTTP/journey receipt tests. This is focused acceptance,
not an all-package or all-platform runtime certification.

See [the interaction and migration contract](native-select.md).
