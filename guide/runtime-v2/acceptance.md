# Runtime v2 acceptance

The acceptance task is editing an existing note, saving it, reopening it,
resizing the same session from 1280 to 880 CSS pixels, and continuing to edit.
Machine evidence checks exact UTF-8 bytes and trailing newlines; a cold-context
AI witness reads actual PNGs and performs the task using the generated protocol.

The reproducible system harnesses are in [tests/runtime-v2](../../tests/runtime-v2/README.md).
They use real browser input and independent fixture persistence. Run Chrome
checks serially. Reports and raw images stay local and are ignored by Git.

| Boundary | Required evidence |
| --- | --- |
| CLI | Native edit/save/revisit, cross-process resize/focus, stale targets, Human control, cancellation, masked credentials, canonical artifacts |
| Go/HTTP | Borrowed host runtime, observation/focus parity, reordered same-name targets, private probe unavailability, persistence uncertainty, host lifetime |
| Journey | Broken native input returns BLOCKED, restored input passes, wrong expected bytes fail, restored assertion passes, no internal LLM |
| Observation | Missing canonical/image, wrong schema and swapped identity reject; same-page capture IDs do not fabricate semantic changes |
| Wait/debug | Real condition/duration, unmet deadline blocks, debugging revokes old authority, public-origin debugging is gated |
| Runtime | Owned/attached/pooled cores and Human input share browser identity; ownership stays with the constructor/host |
| Interface | Generated protocol/help/capabilities/completion equal executable declarations; removed interfaces reject before side effects |

Runtime claims are limited to macOS arm64 and real headless Chrome. Linux and
Windows compilation is separate evidence. Safari and headed execution require
their own actual acceptance runs. An unrequested telemetry channel does not
prove zero errors. Visual persistence does not prove invisible trailing whitespace.

## Verified on 2026-10-15

- CLI: 127 real calls passed on the final candidate, including independently
  received exact save bytes, cancellation preservation and masked password PNGs.
- Browser checks: 142 passed in the broad serial run; its two remaining test
  migrations passed in focused reruns with unchanged timing/state assertions.
  Native-select functional behavior uses explicit baseline element mode; the
  checkbox Space test establishes real focus before testing the key event.
  No selected test skipped. This is selected coverage, not a full-suite claim.
- Deterministic baseline: broken input BLOCKED (2), restored input PASS (0),
  wrong expected bytes FAIL (1), restored assertion PASS (0). Healthy runs saved
  the exact UTF-8 text including the trailing newline on the independent server.
- Real Go/HTTP checks passed, including a nonempty LiveView frame after a native
  caret update. A static-start-only attempt delivered no frame within five
  seconds; the accepted check exercises the event-driven stream after subscribing.
- Session-edge checks: 13 real calls passed. Canonical/schema/identity negatives
  and actual URL/semantic diffs also passed on the final CLI.
- Independent Medium witness completed the task and a focused keyboard rerun,
  reading actual PNGs. Its earlier orchestration-confounded attempt is excluded.
- Root/CLI/journey/Safari adapter unit packages, ordinary all-package compilation,
  integration-tag compilation, shell syntax and generated-contract equality passed.
  Standalone compilation and those unit packages also passed with GOWORK=off.
- Linux amd64 and Windows amd64 CLI compilation passed; runtime remains unverified.

The tested macOS arm64 binary is version 0.12.0-v2, built with -buildvcs=false in
the local Go workspace (clean kit v0.22.1 checkout). SHA-256:

```text
302cb352737bd3a4bfba144f0d6fa798923221cb5e49c3f002809ed0fa9ddcd3
```

Earlier failed candidates and exact local command receipts are retained. Missing
or confounded evidence was not counted as a pass. No source change was made to
native action watchdogs, selection verification or product verdict thresholds
to obtain these results.
