# deepwork-browser

DW-Browser v2 operates real Chrome/Safari sessions through typed actions, immutable observations and deterministic journeys. It preserves the managed browser pool, Human takeover, LiveView and host-owned HTTP integration.

Build a local candidate with Go 1.26+ and Chrome installed:

```sh
GOMAXPROCS=2 go build -p 2 -o ./artifacts/dw-browser ./cmd/dw-browser
./artifacts/dw-browser capabilities
./artifacts/dw-browser help act fill
```

A session owns its creation environment and evidence directory. Open creates only; every later operation names the session.

```sh
dw-browser open http://127.0.0.1:8080 --id edit --scenario app-test-explore --persona desktop --mode headless --viewport 1280x720 --evidence evidence/edit
dw-browser observe --id edit
# Read the returned PNG, then copy a real opaque token from that observation.
dw-browser act fill "$EDITOR_REF" --id edit --text-file draft.txt
dw-browser observe --id edit
dw-browser act click "$SAVE_REF" --id edit
dw-browser act navigate --id edit --url http://127.0.0.1:8080
dw-browser observe --id edit
dw-browser session viewport --id edit --size 880x720
dw-browser observe --id edit
dw-browser close --id edit
```

`fill` replaces through verified physical selection/input; `type --focused` inserts. Text files retain UTF-8 and trailing newlines. Tokens and screenshot geometry expire across input, target, document and runtime changes. Unknown/partial dispatch requires observation before deciding whether another input is safe.

Native HTML dropdowns use `act select CONTROL_REF --id edit --label '完整显示文字'` (copy the actual control ref from `observe`).
Default visual execution uses real keyboard selection; explicit baseline element
steps retain synthetic functional testing. Observations expose bounded choices
with reusable exact selectors. `--label` matches only the full captured label;
`--value` matches only the DOM value. Duplicate matches reject before input.
Receipts separate requested policy from actual evidence. See [native select](guide/runtime-v2/native-select.md).

stdout is one JSON value. `--output` saves JSON, `--screenshot` names a PNG, and creation `--evidence` selects the evidence root. `check` exits 0 only for required PASS, 1 for FAIL, and 2 for BLOCKED/evidence/runtime/argument errors. Other exit 0 means the command delivered its result.

Use `app-test-baseline` for deterministic typed journeys with no internal LLM; use `webvisit` for public browsing, retaining explicit `--allow-host` write authorization. `htr takeover --id edit` blocks AI input; `htr yield --id edit` restores authority and requires a new observation.

For background public DOM acquisition, use `dw-browser document https://example.com/`.
It returns `dw.document.v1` HTML from a disposable browser (headless by default;
explicit headed uses a windowless hidden target),
without screenshots or arbitrary eval. It restricts webpage requests to validated
public destinations, with supported system proxy routing after numeric-IP validation;
it is not an OS firewall. Failures include typed diagnostics without a usable partial DOM. See the [document contract](guide/runtime-v2/document.md).

The same executable generates [the tool-use skill](guide/runtime-v2/generated/dw-browser/SKILL.md), [command reference](guide/runtime-v2/generated/dw-browser/references/commands.md), capabilities and completion:

```sh
dw-browser export-contract --directory guide/runtime-v2/generated
```

See [getting started](guide/getting-started.md) and [the v2 migration](guide/runtime-v2/migration.md) for Go/HTTP integration and advanced cookie import, audit rules, site recipes and traces. Candidate artifacts do not replace any installed binary or automatically become accepted baselines.

[Acceptance evidence and scope](guide/runtime-v2/acceptance.md) describe the real
browser checks and their reproducible harnesses.
