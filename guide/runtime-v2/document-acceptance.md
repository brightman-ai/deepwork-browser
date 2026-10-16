# Document acquisition acceptance — 0.13.0-v2

Verified on 2026-10-16, macOS arm64, Chrome 155.0.8059.80, Go 1.26.
The executable is a regular binary at `~/.local/bin/dw-browser`. The original
0.13.0 release SHA-256 is
`f2c241feb27cdbb64ca20aee0fffe78e7f4806382226c6534852bc4689905d16`.
Earlier binaries were preserved before atomic replacement. The current patch
and its installation fingerprint are recorded below.

## User scenario and outcome

The reported command tried to screenshot Readwise Appearance while gathering
reference material. The original screenshot defect and historical evidence limits
are recorded in [screenshot loading](screenshot-loading.md). It was dependent on
initial render state, not a failure of every website. Existing screenshot/UI
acceptance remains available; background acquisition now has a separate typed
`document` command without screenshot or arbitrary eval permission.

The release binary was exercised against actual public URLs:

| URL | Exit | Content-Type | Outcome | Elapsed |
| --- | --- | --- | --- | --- |
| `https://example.com/` | 0 | `text/html; charset=utf-8` | 2,358 bytes `content`, `dom-html` | 4.262s |
| `https://docs.readwise.io/reader/docs/faqs/appearance` | 0 | `text/html; charset=utf-8` | 200,698 bytes `content`, `dom-html` | 5.999s |
| `https://hnrss.org/frontpage` | 2 | `application/xml; charset=utf-8` | Pure `unsupported_content_type`, actual URL/type, settled policy and cleanup | 3.550s |

Every result above reported browser closed, profile removed and network closed.
The XML result contains no substituted Chrome viewer DOM and grants rerouting
only after renderer-slot cleanup also succeeds. A login, paywall or challenge
DOM is not a promise of qualified article content; that belongs to the consumer.

## Reproducible checks

```sh
GOMAXPROCS=2 GOWORK=off go build -p 2 -buildvcs=false -o artifacts/dw-browser-0.13.0 ./cmd/dw-browser
GOMAXPROCS=2 GOWORK=off go test -p 2 . ./cmd/dw-browser ./browser/testing -count=1 -timeout=120s
GOMAXPROCS=2 GOWORK=off go test -race -p 2 ./browser -run 'Test(Document|Unsupported|PublicNetwork|PublicDocumentIPPolicy|ValidatePublicDocumentURL|RunScreenshotWithTimeout|TargetWebSocketURL|ScreenshotTimeout|CaptureDiagnostics|InitialRenderProgressive)' -count=1 -timeout=90s
GOMAXPROCS=2 go test -race -tags integration -p 2 ./browser -run '^TestDocumentLive$' -count=1 -v -timeout=180s
python3 tests/runtime-v2/baseline.py /absolute/path/to/dw-browser
```

The complete real-Chrome integration matrix passed all 17 cases under the race
detector (43.72s test time; 45.467s package time):

| Behavior | Required observation |
| --- | --- |
| Site JS and isolated DOM | Real JS changes captured; page-world prototype overrides cannot forge DOM/canonical read |
| Site POST query | Website JavaScript loads article content through its natural POST request |
| Private redirect, script, WS, worker, frame | Owned private TCP trap receives zero requests; worker/frame public proof endpoint confirms execution |
| WebRTC | Real `RTCPeerConnection` with loopback STUN server sends zero packets to the owned UDP trap |
| Private main DNS | Failure identifies the guard rejection; proxy error HTML is never successful document content |
| DNS rebinding | Second resolution really occurs and rejects changed-to-private DNS without dialing it |
| Worker completion | Ordinary worker `self.close()` does not fail a healthy document |
| Media | Private image/audio resources are unnecessary and never hit their trap |
| HTML/network budgets | Explicit failure, no truncated success; global network failure promptly cancels the CDP operation |
| RSS and generic XML | Actual type reported as unsupported, without assuming generic XML is a feed |
| Deadline and cleanup | One read deadline and independent cleanup; no new owned profile remains after any case |

The integration fixture uses real Chrome and the production proxy/reader. Only
the public fixture hostname's DNS and final numeric dial are mapped to an owned
loopback HTTP server. Private destinations are not remapped or mocked: an actual
bypass would reach the owned TCP/UDP trap. Independent proxy tests also exercise
real HTTP and CONNECT listeners, mixed DNS answers, numeric IP pinning, candidate
fallback, upload/download byte budgets, error-header integrity and cancellation.

After the final unsupported-result trust change, the RSS/XML cases were rerun
under race with assertions on the real settled policy/cleanup (4.800s package
time). Pure tests also reject rerouting on joined enforcement, browser, network
or renderer-slot cleanup failure. CLI dispatch uses a direct typed error check.

The existing edit/save/revisit baseline produced expected exits `2/0/1/0`.
Healthy cases preserved exact UTF-8 text and its trailing newline in independent
server state. Real screenshot artifacts remained valid and were inspected.
Generated help, capabilities, completions and tool protocol match executable
declarations. Removed/irrelevant document flags reject before browser startup.

## Mechanisms corrected during acceptance

- An unsupported `--force-webrtc-ip-handling-policy` flag was silently ignored
  by this Chrome. The original UDP test failed with two private STUN packets.
  Replacing it with supported fresh-profile `webrtc.ip_handling_policy` fixed
  the same test. It was not waived or hidden behind a narrower claim.
- Pausing/re-attaching workers and out-of-process frames caused unsupported
  Fetch calls or missing execution contexts. This duplicate layer was removed;
  the Chrome-wide forced proxy is the destination-policy source of truth.
- Reading the owned DevTools port file during Chrome's partial write caused
  intermittent false startup failures. Incomplete data now waits within the
  existing deadline; a complete endpoint with wrong identity still fails.
- Successful resource rejection no longer aborts unrelated healthy content.
  Enforcement failure and global budget exhaustion remain fatal.
- Startup failure, cancellation and cleanup use recorded process ownership;
  cleanup failure cannot become successful content or a feed-reroute receipt.

The contract is **page-request public-only**, not an OS firewall. Chrome's own
sandbox remains enabled. The rejected outer macOS Seatbelt experiment and its
Chrome child-sandbox failure did not become a fallback or runtime dependency.
Windows fails before launch; Linux and other Chrome versions require their own
live platform evidence. No user windows, tabs, profile, microphone or clipboard
were used by the document checks.

## Content-Type compatibility patch — 0.13.1-v2

Production integration exposed a general metadata bug: CDP joins repeated
HTTP fields using LF. The WeChat sample returned two identical declarations,
`text/html; charset=UTF-8`, separated by an actual LF. Parsing that entire string
as one MIME value incorrectly produced `unsupported_content_type`.

One normalizer now handles observed response metadata, DOM fallback and HTML
classification. It accepts equivalent media types and parameter names, normalizes
charset case, merges non-conflicting parameters and preserves unknown values.
All case-insensitive occurrences of the response header are collected together;
map iteration order cannot select an arbitrary declaration. Conflicting media
types or parameter values return `DocumentContentTypeError`, preserving the raw
observed value. They cannot become an unsupported-format reroute receipt.

RED→GREEN unit tests cover the original LF case, CRLF/case variations, partial
parameter declarations, unknown parameters, quoted commas and conflicts. Pure
document/network race tests passed (3.062s), and root/CLI/journey packages passed.
Five real Chrome repeated-header cases also passed under race (9.93s test time,
11.432s package time), covering three accepted equivalents and two rejected
media-type/charset conflicts. The actual WeChat URL then returned HTML successfully
with normalized `text/html; charset=utf-8`, rather than a format error. Article
qualification remains the consumer's responsibility.

The installed 0.13.1 binary SHA-256 is
`9dee0bf8bcca0e993a57d88cb21195e339edbb9a8ef76dd4a2ff0033eb7d0af0`.
The 0.13.0 binary is retained in the local backup directory.
Installation smoke returned 3,588,734 bytes from the original WeChat URL in
4.410s with exit 0; the real HN RSS sample retained exit 2 and a settled
unsupported-format receipt in 3.815s. Browser, profile and network cleanup were
all confirmed in both installed-command results.

```sh
GOMAXPROCS=2 go test -race -tags integration -p 2 ./browser -run '^TestDocumentDuplicateContentTypeLive$' -count=1 -v -timeout=90s
```
