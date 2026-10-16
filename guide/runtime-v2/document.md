# Public document acquisition

`document` is the background DOM acquisition API. Use `open` / `observe` / `act`
for experiencing a UI, and `journey` for deterministic acceptance. Those existing
session APIs keep their interaction and screenshot behavior. `webvisit` and
`--allow-host` control interaction permission; they are not network sandboxes.

```sh
dw-browser document https://example.com/ --timeout 30s \
  --max-html-bytes 8388608 --max-network-bytes 33554432
```

The Go entry point is
`browser.FetchDocument(ctx, browser.DocumentRequest{URL: "https://example.com/"})`.
It is the same kernel used by the CLI. No caller JavaScript, selector, user
profile, session ID, headed mode, cookie import or host permission grant is
accepted. There is no second DOM-export command or arbitrary-eval escape hatch.

## Result contract

Successful stdout is one JSON object with schema `dw.document.v1` and exit 0:

| Field | Meaning |
| --- | --- |
| `schema`, `tool_version` | Document contract and CLI build version; Go consumers provide their own tool version |
| `requested_url` | The caller's URL |
| `url` | Actual CDP main-frame URL, including fragment, checked across the DOM read |
| `declared_canonical_url` | Optional resolved canonical link from the DOM; an untrusted hint that is never fetched or treated as verified navigation |
| `content_type` | Normalized main response Content-Type (including charset); equivalent repeated declarations collapse to one value, conflicting declarations fail; DOM contentType is the fallback when no header is exposed |
| `rendering` | Requested/actual renderer mode and target surface: `headless/headless-target` or `headed/hidden-target`; failed startup can be `not-started` |
| `readiness` | Main document `interactive` or `complete`; `dom_ready: true` |
| `content`, `representation` | Complete `documentElement.outerHTML` at that observation, including site changes already made; representation is `dom-html` |
| `policy` | Applied isolation and budgets, plus settled proxy counters |
| `cleanup` | Browser closed, owned profile removed and network guard closed |

This is DOM acquisition, not article extraction, sanitization, iframe flattening,
raw response capture or a promise of completed application hydration. The tool
uses a fixed program in a CDP isolated world, so page-world prototype overrides
cannot replace its DOM readers. It checks frame and loader identity around that
read and rejects a changing document.

Only HTML/XHTML DOM is currently supported. XML, RSS and Atom return exit 2 with
`partial_result.reason: unsupported_content_type` and the actual Content-Type
and URL, together with settled `policy` and `cleanup`. That reason is emitted
only for a pure unsupported-format outcome after browser, proxy, profile and
renderer-slot cleanup succeed. Joined enforcement or cleanup errors do not grant
permission to reroute. A Chrome XML viewer is never returned as the original feed. Consumers
may route an already confirmed feed type to their existing HTTP subscription
pipeline. Generic XML is not assumed to be a feed, and the tool does not prefetch
every URL through a second HTTP route.

Ordinary acquisition failures use the common CLI error JSON and exit 2, with
`partial_result.schema: dw.document.failure.v1`. This payload is diagnostics:
`usable: false`, requested URL, observed URL when known, `rendering`, `stage`,
`reason`, settled `policy`/`cleanup`, and measured `timing` in milliseconds. It
contains no `content` or `representation` and grants no alternate-transport reroute.
Reasons distinguish `timeout`, `canceled`, `network_policy`, `network_transport`,
`cleanup_failed`, `html_budget_exceeded` and other `acquisition_failed` cases.
A cleanup deadline does not masquerade as a read timeout. Pure unsupported-format
receipts keep their earlier explicit reroute contract. Ordinary HTTP upstream
failures and guard policy denials are classified separately; their synthetic
error pages are never successful documents. Over-budget HTML is a failure;
the tool never returns truncated HTML as success. A cleanup or enforcement error
also prevents success. Successfully denied resource requests increment
`policy.blocked_requests` without killing an otherwise usable document. A denied
main navigation still fails. A DOM containing a login, consent, paywall or challenge is still
that site's DOM: consumers decide whether it qualifies as usable content.

## Runtime and network boundary

Each request owns a fresh Chrome and random disposable profile. Headless remains
the default; headed creates a hidden WebContents through the same owned browser
CDP connection before attaching the DOM reader. It never falls back to an ordinary
target, opens a native window, starts CGVirtualDisplay or calls target activation
on the hidden-headed path. Neither mode borrows a user session. `rendering.surface` identifies the mechanism that actually
started; it is not proof of a normal headed user journey. It never
attaches an existing session or reads a user's browser profile. Audio is muted;
microphone, camera, downloads and popups are denied. Document launches disable
Chrome's delayed updater scheduler for that child only; they do not modify host
update settings or stop pre-existing updater processes. Images and media are blocked
by resource type; JavaScript is available without waiting for all resources or
network idle. No stealth, user-cookie access or browser-security bypass is used.
Site JavaScript may perform normal network activity: a fixed DOM reader is not a
claim that all website scripts or encrypted HTTP requests are side-effect free.

The receipt names the boundary **`page-requests-public-only`**. Native Chrome
retains its own sandbox. Its forced HTTP proxy applies to webpage HTTP(S) and
WebSocket requests, including worker and iframe traffic. Loopback proxy bypass
is disabled; direct DNS resolution, QUIC and non-proxied WebRTC UDP are disabled.
CDP guards apply additional resource restrictions to the main target; they do not
pause or reattach child targets. Worker and iframe requests use the same forced
proxy, keeping destination policy in one place without blocking script execution.
The proxy resolves every new destination, rejects the entire DNS answer set if
any address is non-public, and dials only numeric addresses already validated.
Redirects and subresources use the same boundary. Ports are limited to 80/443;
credentials, private IP literals, special-use and unallocated destinations are
rejected. System DNS resolution is a separate control plane and may use the
machine's configured private resolver. This is a webpage request boundary, not
an OS firewall or an assertion about every operating-system/broker connection.
The tested runtime is macOS arm64 Chrome; Linux needs its own live evidence.
Windows fails before launch because verified process-tree cleanup is unavailable.

An outer macOS `sandbox-exec` backend was rejected after real Chrome failed its
own child sandbox initialization. No `--no-sandbox` workaround is used and Docker
is not a normal dependency of document acquisition.

`--network-route system` is the default. Each job takes a read-only snapshot of
supported static HTTP/HTTPS proxy configuration. Explicit HTTP_PROXY/HTTPS_PROXY/
ALL_PROXY environment settings take precedence over the OS snapshot; conflicting
upper/lowercase settings, credentials, unsupported schemes, environment NO_PROXY
exclusions, PAC/WPAD and SOCKS-only configurations fail before launch. HTTP and
HTTPS can use distinct static HTTP proxies; a simultaneous SOCKS setting does
not disable valid HTTP/HTTPS settings. No PAC program is executed. An explicit
`--network-route direct` ignores these proxy settings for this job. No proxy
failure silently falls back to direct.

The guard still resolves and validates **the entire destination answer set**
locally before any transport connection. A selected system proxy receives only
`CONNECT <validated-numeric-IP>:<port>`, including for HTTP destinations. It never
receives an authorized destination hostname to resolve. Chrome retains TLS/SNI
and certificate validation for the original HTTPS host. The proxy endpoint is
trusted transport infrastructure from the host configuration and can be private;
this delegates trust that the proxy honors numeric CONNECT. It cannot prove
which TCP destination the proxy itself ultimately uses. The receipt explicitly
reports this trust instead of claiming an OS firewall.

`policy.requested_network_route` is `system` or `direct`.
`effective_transport` summarizes the frozen HTTP/HTTPS route configuration as
`direct`, `system-http-proxy-numeric-CONNECT` or `mixed`, not a count of routes
actually used. `network_route_source` names the configuration source;
`numeric_target_pinning` describes the enforced rule, and
`trusted_proxy_delegation` identifies proxy transport trust. No proxy credentials
are returned. `DW_BROWSER_PROXY` does not override the job's forced guard.
The boundary applies only to `FetchDocument`, not arbitrary browser sessions.

## Budgets and ownership

Defaults are 30 seconds for the whole read, 8 MiB UTF-8 HTML and 32 MiB proxy
payload. The deadline includes waiting for one of two cross-process renderer
slots, startup, navigation and DOM reading. Cleanup has an independent five-second
budget. CLI budget flags must be positive; Go zero values select defaults.

The proxy also limits each job to 128 plaintext HTTP requests plus CONNECT
tunnels. HTTPS stays encrypted end to end: a CONNECT counts once, rather than
pretending to count every resource inside it. Network bytes count plaintext HTTP
request/response bodies and both directions of CONNECT payload, excluding HTTP headers.
The counter charges chunks before forwarding and can include the rejected final
chunk; it is not an exact count of all network-stack bytes or decompressed data.
The HTML budget independently bounds the returned DOM. Renderer concurrency and
deadlines bound work, not an OS memory or CPU quota.

The consumer owns extraction, per-domain schemas, content qualification,
provenance, caching and the choice of a separately verified HTTP fast path.
The browser tool owns navigation, fixed DOM access, network policy, time and
resource budgets, and cleanup of only its own processes and profile.

## Acceptance

Release evidence is recorded in [document acceptance](document-acceptance.md).
The original screenshot failure, its limited historical evidence and the installed
0.12.1 repair are documented in [screenshot loading](screenshot-loading.md).

## Choosing a renderer and interpreting site challenges

```sh
dw-browser document https://example.com/ --mode headed --network-route system
```

The kernel does not retry or upgrade modes automatically. Both modes keep the
same sandbox, public-destination rule, budget and disposable-profile ownership.
The profile receipt remains `ephemeral-headless` for the default and is
`ephemeral-headed-hidden` for explicit headed. Go uses `DocumentRequest.Mode`
and `DocumentRequest.NetworkRoute` through the same kernel.

A captured verification/login/paywall page is still that site's response. A
successful DOM receipt does not certify article availability. The verified
WeChat same-article short link returned article DOM, while the parameterized long
link returned a challenge in both modes; retaining the same fresh job's cookies
and observing for five seconds did not change that result. Do not spend a second
headed budget assuming it bypasses verification. Content qualification and any
Human verification flow remain explicit consumer decisions, not site-specific
browser scripts or a false unsupported-content-type claim.

See [0.16.0 runtime diagnosis and acceptance](document-runtime-acceptance.md).
