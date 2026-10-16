# Native select and evidence

Select means “choose one option in this dropdown.” The control target comes
from `observe`; the session ID identifies the browser session. The tool supports
any native HTML dropdown, not a particular application's choice names.

1. Run `dw-browser observe --id review` and read the real screenshot.
2. Find the desired dropdown in `elements`; copy its actual `ref` into
   `CONTROL_REF`. This shell variable is the observed control identity, not its
   name, a made-up token or an option's index.
3. Read `select.options`. Each selectable choice includes an `option` object
   with **exactly one** field: `label` or `value`.
4. Use the corresponding flag and then observe the application result:

```sh
# For an observed option object {"label":"完整内容"}:
dw-browser act select "$CONTROL_REF" --id review --label '完整内容'
# For an observed option object {"value":"full"}, use --value 'full' instead.
dw-browser observe --id review
```

The example label is page content, not a DW-Browser keyword. `--label` matches
only the full captured display label; `--value` matches only the exact DOM value.
They are mutually exclusive. There is no prefix, substring, value-to-label
fallback or automatic first match. Zero or multiple matches reject before input.
Comparison is case-sensitive and does not trim the query. Uniqueness includes
all options, even hidden/disabled choices and choices outside the output budget.

An empty string must be explicit: `--value ''` selects a unique empty DOM value;
omitting both flags does not mean empty. Completely indistinguishable duplicates
are unaddressable, even if already selected; the tool does not invent positional
identity. Labels repeated with distinct values remain addressable by value, and
values repeated with distinct labels remain addressable by label.

The same canonical request is shared by Go, HTTP and journeys:

```json
{"kind":"select","target":{"kind":"locator","locator":"css=select[name=source]"},"option":{"label":"完整内容"}}
```

Use the actual control locator for your application. `option` requires exactly
one string field; null, unknown fields, duplicate keys, an empty object or both
fields are rejected. Old select top-level `value` is removed. Go callers use
`browser.SelectByLabel(text)` or `browser.SelectByValue(value)` as `Action.Option`.
Custom JavaScript dropdowns continue to use their actual visible controls;
`select` targets native HTML `<select>` elements.

## Choose by task

| Task | Existing entry | Execution | Evidence |
| --- | --- | --- | --- |
| Experience a normal single-select | Default visual `act select` or visual journey step | Real pointer focus and browser-native typeahead keys | Verified keyboard selection |
| Deterministic functional state setup | `app-test-baseline` journey, explicit `mode: element` | DOM assignment and synthetic events | Functional state |
| Requested option already selected | Either supported path | No selection input or manufactured events | State only |
| Inspect pointer popup, option hit targets or dismissal | Actual visible interaction and a backend that can observe that surface | Requires its own runtime evidence | Not established by keyboard selection |

Native failure never switches to DOM assignment. No `native-select`/`dom-select`
commands or fallback flags exist. The baseline element mode is selected by an
explicit journey step, not by a hidden retry after native failure:

```yaml
- id: choose-source
  kind: action
  mode: element
  action:
    kind: select
    target: {kind: locator, locator: 'css=select[name=source]'}
    option: {value: full}
```

## Discover before acting

Native select references in an observation include a `select` capability with
`available`, `execution`, `evidence_scope`, `reason` when unavailable, and declared
option labels/values plus disabled/hidden/selected state and per-option
`selectable`/`reason` and a directly reusable `option` selector from the shared
planner. A unique label is preferred; otherwise a unique value is provided.
Unaddressable duplicate choices have no selector and are not selectable. This is structural
control data, not proof that a popup appeared in the screenshot. No popup-option
ref is manufactured.

Display is bounded to 20 options and 2 KiB of option JSON. `option_count` gives
the actual count; `options_complete: false` means the displayed choices are
incomplete or privacy-withheld. It cannot prove a choice is absent. A known label
or value from the user's task can still be requested; execution re-probes the
control rather than trusting the display subset. Complete internal inspection
is bounded to 4,096 options; unknown facts or unsupported backends do not grant
native selection capability.

Privacy keeps control capability separate from disclosure. Sensitive controls
and sessions redacted after password input withhold choices and public result
values without disabling an otherwise usable dropdown. Redacted selection
states have `value_redacted: true`; missing values are not successful empty
values. A genuine empty option value remains explicitly `"value": ""`.

## Interpret the receipt

- `requested_fidelity` is the scenario policy; `interaction_mode` is the requested
  visual/element mode. Neither proves the mechanism that ran.
- `execution: native-keys` identifies actual native-key input. A successful
  `evidence_scope: keyboard-selection` and `selection.verified: true` establish
  the requested index/value result through that path.
- `execution: synthetic` with `evidence_scope: functional-state` identifies the
  explicit baseline setter. `synthetic: true` remains explicit.
- `execution: no-op` with `evidence_scope: state-only` means the target state was
  already satisfied. It does not claim selection input/change events occurred.
- A failure retains actual input-path and selection evidence. Missing
  `evidence_scope`, `selection.verified: false` or an uncertain dispatch cannot
  be promoted into successful selection. Observe before further input; do not
  replay the action automatically.

Typeahead can pass through intermediate choices and native change handlers.
Selection is a Human interaction, not an atomic server transaction. The tool
checks the same control, options and focus after each key; loss of continuity,
unavailable navigation or failure to reach the target stops the operation.
The receipt's `selection.key_steps` counts typeahead dispatch attempts, not a
claim that a particular number of DOM events was observed in production.

The verified initial runtime is macOS arm64 Chrome 155 headless. Its native
typeahead retains a one-second search buffer; the implementation uses a
cancellable 1,100ms buffer boundary inside the ordinary action deadline before
bounded key traversal. Input and result checks remain real. Multiple/listbox
controls, disabled/hidden requested options and unsupported keyboard characters
are unavailable to this native method; explicit baseline functionality retains
its earlier supported assignment cases. An already-satisfied enabled choice can
still provide state-only evidence without needing a typeahead character.

The tests cover Chinese choices, shared prefixes, duplicate labels addressed by
value, default disabled/hidden placeholders, no initial selection, no-ops,
subsequent click/type, interrupted controls, privacy, cancellation and the
explicit synthetic baseline. They do not certify native popup pointer selection,
touch pickers, Safari or other platform/Chrome combinations.

## Interface migration in 0.14.0-v2

The misleading action JSON `fidelity` key becomes `requested_fidelity`. Actual
execution/evidence is separate and is never inferred from `synthetic: false`.
Journey removes its mixed `fidelity: visual/synthetic-element/synthetic` scalar;
its `action_fidelity` record carries the core facts directly in JSON and the
Markdown report. HTTP action results carry the same `action_fidelity` on success
and on failures after execution. Preflight failure cannot reuse an older action's
receipt. Go's existing `ActionFidelityReport.Fidelity` member remains the policy
value and serializes under the clearer requested name.

See [0.14.0-v2 acceptance and binary identity](native-select-acceptance.md).

## Interface migration in 0.15.0-v2

A 0.14 command that supplied a display label via `--value` must use `--label`.
Actual DOM values continue to use `--value`, now strictly within that field.
JSON/YAML actions replace top-level `value: full` with `option: {value: full}`
or `option: {label: '完整内容'}`. All select callers use this one model;
there is no legacy matcher or compatibility alias. A missing or ambiguous choice
requires fresh observation and an explicit unique selector, not automatic retry.

See [exact-selector acceptance](select-exact-acceptance.md).
