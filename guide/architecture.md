# DW-Browser v2 architecture

The browser runtime owns an instance/run generation; the session owns creation environment, active target and control authority. Observations are immutable captured facts. Actions are typed requests; outcomes express dispatch and recovery independently of assertions or product acceptance.

CLI declarations in `cmd/dw-browser/command_definitions.go` drive routing, allowed arguments, conditional groups, help, capabilities, completion and the generated tool protocol. Every active handler returns `CommandResult,error`; `cli_boundary.go` owns JSON stdout/files and exit classification. It does not interpret arbitrary stdout or use handler process exits as results.

CLI, typed journey, Go and HTTP adapters construct the same `browser.Action` and call `BrowserCore.ExecuteAction`. ActionDefinitions owns kinds/fields/target kinds/mutation classification. Native input validates visibility/focus/editability, holds the browser input critical section and reports the canonical Outcome. There is no string-action parser or model input-failure fallback.

The snapshot engine issues capture identity and opaque refs once. Session binding preserves those identities; source dispatch guards revalidate runtime/document/target/geometry and authority under the same input lock used by target activation and Human raw input. Session viewport persists an actual resize instead of creating another browser. Help/status read metadata without recovery or lock waits.

`browser/testing` owns the canonical observation/file loader, assertion engine, wait kernel and one typed journey runner. A requested channel is captured/unavailable/not-requested. Screenshot failure cannot reuse an old file. SUT-specific CSS/HTTP/transcript/mutation facts are explicit probes; the browser does not infer application URL chips or service routes. Required BLOCKED remains blocked, and VLM advice cannot become hard acceptance.

The root `dwbrowser.Server` binds a host-owned `BoundRuntime` through a resolver. Its persistence callback stays host-owned; it does not create a hidden browser/session store or close a borrowed core. Existing browser-pool, Human takeover and LiveView mechanisms remain the runtime integration boundary.
