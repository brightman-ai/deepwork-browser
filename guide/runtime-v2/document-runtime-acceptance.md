# Document runtime acceptance — 0.16.0-v2

Verified on macOS arm64, installed Chrome 155.0.8059.80. This covers owned
headless and headful hidden DOM targets. It does not certify traditional GUI
headed execution, other platforms, or automatic passage through site challenges.

## Root causes and corrections

1. **Transport mismatch:** the host's normal system proxy reached Project Zero;
   direct requests timed out. The old document guard always dialed directly.
   The new default system route applies the same complete public DNS validation
   before sending only numeric CONNECT destinations to trusted proxy transport.
   A controlled numeric-CONNECT/TLS-SNI request returned HTTP 200; the actual
   document kernel subsequently returned the full article and successful cleanup.
2. **Delayed updater and stderr ownership:** Chrome 155 schedules updater work
   after 19 seconds. On macOS, `WakeAllUpdaters` explicitly starts a new process
   group. The reported 22-second failure was reproduced; owned ancestry captured
   the updater launcher and `GoogleUpdater --wake-all` outside Chrome's group.
   Its inherited stderr could keep `cmd.Wait()` waiting after browser exit.
   Document-only `--disable-updater-scheduler` prevents this unrelated startup
   work without changing machine settings or touching existing updater jobs.
   An owned bounded stderr reader separately reports incomplete drain; it never
   promotes a surviving inherited writer into clean browser shutdown.
3. **Failure provenance:** ordinary context/transport failures were sometimes
   labeled network policy. Enforcement, transport, acquisition deadline, and
   the independent cleanup deadline now remain distinct. Synthetic proxy error
   pages cannot become successful DOM; source-supplied proxy markers are stripped.

Exact Chromium source for the updater chain:
[registration guard](https://chromium.googlesource.com/chromium/src/+/155.0.8059.80/chrome/browser/chrome_browser_main.cc#1109),
[19-second scheduling](https://chromium.googlesource.com/chromium/src/+/155.0.8059.80/chrome/browser/updater/scheduler.cc),
[independent process group](https://chromium.googlesource.com/chromium/src/+/155.0.8059.80/chrome/browser/updater/scheduler_impl.cc#75).

## Runtime evidence

| Boundary | Observed result |
| --- | --- |
| Original Project Zero URL, old direct path, 22s budget | Reproduced read plus cleanup timeout; no usable DOM |
| Final 0.16 direct path, same URL and 22s budget | Exit 2 / navigation timeout in 23.697s including cleanup; all three cleanup facts true, no profile survivors or updater child |
| System route, same public article | Full 19,633-byte HTML with article ending, normal content type and all cleanup facts true |
| Final headful hidden target, WeChat short link | Full article DOM over 3.5MB with `js_content`; all cleanup facts true |
| Same-article WeChat long link, headless and hidden headed | Challenge DOM in both modes; not certified as usable article content |
| Same hidden job: short, retain cookies, then long; observe 5 seconds | Short remained article; long remained challenge and displayed environment-abnormal text; all resources cleaned |
| Real Chrome race integration suite | 24 owned browser launches passed: isolated fixed reader, site JS, redirects, workers, OOPIF, WebSocket/UDP/private-IP traps, rebinding, byte limits, deadline, MIME conflict and both rendering modes |
| Monitor over that integration suite | 24 owned PIDs tracked, no front activation or visible owned window, display geometry unchanged |
| Final explicit headed CLI run | 10 owned PIDs tracked, no front activation, no native-window samples at all, display geometry unchanged, no profile survivors |
| Pure/race checks and independent review | Network pinning/tunnel cancellation, inherited stderr, classification, CLI schema and hidden bootstrap passed; independent Medium source review found no blocker |

Headless Chrome can have internal off-screen window objects. Those are kept
separate from visible-window evidence; the hidden headed runs had no window
samples at all. A preliminary full-batch monitor that tracked zero PIDs was
rejected as unavailable evidence and replaced with ownership-based monitoring.
A temporary bootstrap constructor panic was fixed and locked by a real chromedp
constructor regression before the successful explicit headed runs.

The earlier no-network probe and causal session experiment are research
receipts, not substitutes for the production `document` runs. The production
kernel never receives pre-fetched research HTML or a site-specific script.
WeChat long-link verification remains an open acquisition case. The evidence
rejects “headed always solves it,” “wait five seconds solves it,” and a generic
unsupported-content-type diagnosis. It does not explain the site's undisclosed
risk rule or reproduce the user's established PC profile. No CAPTCHA solving,
cookie import, stealth script, private-DNS relaxation, GUI display manipulation
or automatic mode retry was added.

## Binary and reproducibility

The verified final candidate is `0.16.0-v2`, SHA-256:

```text
7e9474ef1b83f8eb93c0e0712e53b80f563689ec97db544e304bb393550645d4
```

Run the existing integration suites serially:

```sh
go test -race -tags integration ./browser -run '^(TestDocumentLive|TestDocumentDuplicateContentTypeLive|TestDocumentRenderingModesLive)$' -count=1 -timeout=150s
```

The fixture tests use owned loopback servers and private resolver/dial seams;
production target validation, proxy protocol, browser, reader and cleanup run
unchanged. Run hidden-headed acceptance only on a verified noninterfering runtime.
The pure controlled-process tests independently verify group disappearance and
require a cleanup failure while a deliberately escaped stderr writer is alive.
Raw site DOM, tokens, screenshots, failed attempts and native monitor records
remain private local evidence and are not committed.

Independent Medium cold reader verified the final hash and used the default
command to read the Project Zero title and final paragraph, with all cleanup
facts true. The installed final binary then repeated the actual request in
4.389s, returning 19,633-byte article DOM and all cleanup facts true; ownership
inspection found no profile survivors. Standalone compilation and Linux/Windows
amd64 CLI cross-compilation passed; those platforms' runtime remains unverified.
