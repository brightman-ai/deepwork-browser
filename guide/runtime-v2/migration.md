# DW-Browser v2 migration

The runtime uses one typed Action kernel, one canonical observation contract and one typed journey runner. Historical evidence/reports are preserved. Installing a built binary is an explicit delivery action; external callers migrate using this contract.

In 0.20.0-v2, `close` changes meaning and its receipt grows. A profile that
outlives the session (an explicit `--profile`, or dedicated isolation) is no
longer killed: Chrome is asked to exit (CDP `Browser.close`, then SIGTERM after
8s, then SIGKILL after 2s more) so cookies and local state reach disk, and the
receipt reports how it went away as `shutdown` = `graceful` | `terminated` |
`killed` | `gone` plus `shutdown_ms` and, when it was not graceful,
`shutdown_reason`. Only `graceful` promises the profile state is fully on disk.
A temporary profile (deleted with its session) and `--force` still kill at once.
The BrowserMuxHost `/shutdown` call is now synchronous: it answers after Chrome is
gone and the profile is sealed, so callers must allow for that wait.
Every CLI profile now carries `.dw-profile.json`; its `os_crypt` identity
(`mock` or `system`) is fixed by the profile, not by the launch mode, which ends
"logged in under headed, logged out under headless". An existing profile without
a manifest is classified from evidence the first time it is opened: cookies that
the public mock password decrypts make it `mock`, cookies it cannot decrypt keep
it on the system keychain (`system`, never sealed). A persistent `mock` profile
is sealed into one encrypted `profile.vault` when the session closes and
unsealed when it is opened again; while sealed the directory holds exactly two
files. The `open`, `close` and `profile list` receipts carry a `profile` section
(`os_crypt`, `vault` = `none` | `open` | `sealed`, `protection` = `keychain` |
`file`, and `error` / `error_kind` / `note` when something did not go to plan).
A seal that fails is reported, never hidden: the plaintext profile is left intact
and sealing is retried on the next close. Temporary profile directories are now
removed through a list-validate-delete funnel bounded to the CLI profile root.

Process records are never trusted blindly in 0.20.0-v2. Everything that signals a
process it only remembers by pid first checks the command line: the BrowserMuxHost
must be `dw-browser muxhost serve --muxhost-id <its id>`, a recorded Chrome must
carry `--user-data-dir=<its profile>` (`--remote-debugging-port` when the record
has no profile directory). A pid that no longer matches is a stale record: nothing
is signalled and the record is dropped (registry reaper: `record_removed` /
`pid_not_ours: ...`). A recorded process group is killed only when it is read
live off the verified pid and equals the recorded one. `profile prune --ephemeral
--reap-orphaned` follows the same rule and, in addition, no longer kills the
BrowserMuxHost or the owner process named in a profile's marker (the host is
shared by every session; close it with `muxhost shutdown`): it kills only
processes whose command line is that profile's own Chrome, and lists every pid it
refused in the entry's `notes`.

Also in 0.20.0-v2, headed input is fixed. Input actions against an attached
headed session (`act click` was reproduced 3/3; the other input verbs share the
same critical section) used to fail after about 8s with "another operator holds
the input window": the target-activation hook ran inside the
input critical section and asked for the same lock again, so the process waited
for itself. The critical section is now an explicit credential: code that runs
inside the window receives the `*BrowserInputLock` it was handed and checks it
with `Covers(key)` (`ErrInputWindowNotHeld` when it is nil, released or for
another browser instance) and never acquires the lock itself. Library callers
see two signature changes: `AcquireBrowserInputLock(key, op, wait)` takes a
diagnostic `op` (for example `action.dispatch`, `tracker.activate`), and the
activation hooks take the credential (`func(ctx, *BrowserInputLock)`). A lock
wait that runs out now says who holds it: another process is named by pid,
command and operation with how long it has held the window; a holder inside the
same process is reported as "held in this process" (a suspected re-entry, or a
holder that is stuck past the wait limit; Go cannot tell which) instead of being
disguised as another operator.

In 0.19.0-v2, cross-process headed session attach restores target tracking and
no-front activation. Input now requires a live, identity-matched mux-host
containment check and uses target activation without `Page.bringToFront`.
Headed virtual-display window placement accounts for the requested window size;
failed launches publish an early owner marker so their exact Chrome process tree
can be reclaimed. A containment failure remains a fail-closed runtime error.
This hardens the browser boundary; it does not make a site's server challenge
pass or certify the WeChat long-link case.

In 0.17.0-v2, `fetch URL --save FILE` adds a browser-free public HTTP GET kernel
for complete body bytes, with typed metadata and failure accounting. It shares
the public guard with `document`; these remain distinct representations with no
automatic fallback. Both readers add per-call `--dns-resolver system` (default)
or an explicitly trusted numeric HTTPS DNS endpoint. This does not change the
computer's DNS, retry through a different resolver, or relax private-address
rejection. See [fetch](fetch.md).

Go consumers replace `DocumentNetworkRoute` / `DocumentNetworkRouting` with
`NetworkRoute` / `NetworkRouting`, and constants with `NetworkRouteSystem` /
`NetworkRouteDirect`. `ValidatePublicDocumentURL` becomes `ValidatePublicURL`,
and `IsPublicDocumentIP` becomes `IsPublicIP`.
These are shared public-resource concepts; no legacy aliases remain. The
existing `document` CLI and representation retain their meanings.

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
