# DW-Browser v2 migration

The runtime uses one typed Action kernel, one canonical observation contract and one typed journey runner. Historical evidence/reports are preserved. Installing a built binary is an explicit delivery action; external callers migrate using this contract.

In 0.16.0-v2, `document` keeps headless as its default and adds explicit headed
rendering through a windowless hidden target. The default network route becomes
`system`, using supported trusted proxy configuration after numeric public-IP
pinning; `--network-route direct` is explicit. Success adds `rendering` and routing
facts; default `policy.profile=ephemeral-headless` is preserved. Ordinary failures
add `dw.document.failure.v1` diagnostics with `usable:false`, never partial DOM.
Pure unsupported-format rerouting remains unchanged. See [document](document.md).

In 0.15.0-v2, select requires exactly one explicit selector: `--label` for the
full captured label, or `--value` for the exact DOM value. The old automatic
value/label/prefix matcher is removed; duplicate matches reject before input.
Go/HTTP/journey actions use `option: {label: ...}` or `option: {value: ...}`
instead of select's top-level `value`. Observation provides reusable option
objects. See [native select](native-select.md) for the complete contract.

In 0.14.0-v2, visual native `select` uses verified keyboard input. Only explicit
baseline element steps retain synthetic selection. Action JSON names requested
policy `requested_fidelity`, separately reporting actual execution/evidence;
journey drops its old mixed fidelity scalar. See [native select](native-select.md)
for capability discovery, evidence scope and receipt migration.

| Prior interface | v2 interface |
| --- | --- |
| String `act "fill ..."` / implicit one-shot act | `act fill TARGET --id NAME --text|--text-file VALUE` |
| `session start` / `session close` | `open URL --id NAME --scenario SCENARIO` / `close --id NAME` |
| `personas` | `persona list` |
| HTR attach/acquire/release | HTR status/takeover/yield/share |
| `--headless`, mode human/standard | `--mode headless|headed|visible` only at creation |
| Task `--ephemeral` / `--isolation` | Temporary task profile by default; explicit `--profile` is persistent |
| `--out`, `--json` | `--output` JSON; `--screenshot` PNG; creation `--evidence` directory |
| Independent layout / once view / once action | observe/audit/content, once common observe, typed journey |
| Legacy `test` runner | v2 `journey`; typed action/observe/wait/check/probe steps retain CSS/HTTP/transcript/mutation/recovery ports |
| clickat/dblclickat/rclickat | typed click target/point, count and button |
| tapxy/tapat, hoverat/wheelat, dragat/swipeat | typed tap/hover/scroll/drag/swipe with explicit coordinates and capture observation_id |
| typetext, scrollinto, check/uncheck | type --focused, scrollto, set-checked --checked BOOLEAN |
| Per-command environment overrides | Persisted creation environment; session viewport for resize |
| Reused `@rN` / flat observation JSON | Opaque capture tokens; canonical dw.observe.v2 artifact or explicit dw.observe.ref.v2 reference |

`--id` is an identity, not a path; whitespace/path separators are rejected. Text is literal UTF-8, including an explicitly empty replacement and trailing file newlines. Named arguments that look like flags remain data when used as a text value. Unknown flags and inappropriate scopes fail before dispatch.

Sensitive fill requires an actually masked password control. Unmasked fields reject before focus/deletion; JSON facts and traces redact credentials, and screenshots remain real masked browser output. Negative text assertions require complete document text; redacted or truncated misses are BLOCKED. Date/time inputs retain setter-based functional testing only in explicit element steps of a baseline journey, with synthetic fidelity reported. Strict physical input has no setter rescue.

Go consumers first bind an explicit recognized scenario through `core.(browser.ScenarioInteractionCapable).SetInteractionScenario(browser.ScenarioAppTestExplore)` (or the baseline/webvisit scenario appropriate to their task), then construct `browser.Action` / `Target` / `Point` and call `BrowserCore.ExecuteAction`. An unbound or unknown interaction policy is rejected before transport or dispatch. Visual mode does not choose a scenario; synthetic element behavior still requires the explicit baseline posture and mode. Its exact Outcome carries phase/dispatch/recovery; callers must inspect it, then observe after unknown dispatch. `Snap` explicitly mints capture-scoped capabilities; internal navigation/post-action snapshots do not. `browser/testing` owns BuildObservation/WriteObservation/LoadObservation, AssertionEngine and the shared WaitForAssertion kernel. Empty/unknown schemas and bad references are rejected.

HTTP hosts bind their existing runtime with `dwbrowser.WithRuntimeResolver`; requests use the same Action model. Host persistence stays host-owned through the BoundRuntime persist callback and shared pure session/observation contracts. Read-only sessions are not silently replaced by a private browser. The adapter/server source defines the current binding signature; callers must provide their own authorized core/session lookup.

Optional planning emits typed actions and waits before execution. It is absent from runtime input retries and forbidden in baseline execution. Site recipe fences use `dw-browser-plan` JSON. Legacy string recipes/traces require explicit migration; they are not kept as executable aliases. A trace/candidate does not receive accepted-baseline identity automatically.

The generated skill, capability table and completions share this executable's declarations. Generate into an explicit repository directory with `export-contract --directory DIR`; validation/build checks are reported separately from macOS runtime evidence. Windows/Linux/Safari/headed execution claims require their own actual evidence.

An embedded host binding has this shape (the host supplies its existing authorized core and synchronized state):

```go
srv, err := dwbrowser.NewServer(dwbrowser.WithRuntimeResolver(
    func(ctx context.Context, id string) (dwbrowser.BoundRuntime, error) {
        core, info, err := hostLookup(ctx, id)
        if err != nil { return dwbrowser.BoundRuntime{}, err }
        return dwbrowser.BoundRuntime{
            Core: core, Info: info,
            State: func(ctx context.Context) (*browser.SessionInfo, error) {
                return hostReadState(ctx, id)
            },
            Persist: hostPersistState,
        }, nil
    },
))
```

The helper names above are host functions, not a second provided store. The current HTTP action body is `{"id":"NAME","action":{"kind":"fill","target":{"kind":"locator","locator":"css=#editor"},"text":"literal value"}}`. Do not replace it with an action string. The canonical server source is the authority for endpoint/binding signatures; runtime tests distinguish actual borrowed ownership from compilation alone.
