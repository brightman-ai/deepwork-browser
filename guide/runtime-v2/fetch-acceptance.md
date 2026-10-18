# Public resource fetch acceptance — 0.17.2-v2

The typed fetch and explicit DNS contracts passed independent source review and
owned transport tests on macOS/arm64. **Hugging Face nine-image coverage remains
open.** A longer research timeout does not establish default production latency.

## Verified boundaries

| Check | Evidence |
| --- | --- |
| Complete binary bytes, size and SHA-256; anonymous GET | Owned HTTP/TLS fixtures |
| Public redirects, five-hop limit, no cookies or automatic tool fallback | Resource tests |
| Status 404/500/206, gzip checksum, decoded overflow, cancellation and truncation | Typed failures, actual decoded counters, no returned partial body |
| Real truncated chunked response | Red before the shared guard stopped swallowing `io.Copy` errors; green after response abort |
| Cleanup failure | Actual independent five-second cleanup timeout rejects success |
| Output path equivalence, existing destination, persistence and delivery errors | CLI filesystem fixtures, including macOS case/Unicode aliases and closed stdout |
| Numeric system-proxy CONNECT for HTTP and TLS | Owned proxy records numeric targets only |
| Numeric DoH endpoint and strict IP SAN certificate | Owned TLS resolver; wrong-IP certificate rejects |
| A and AAAA completeness | NODATA/IPv6-only, CNAME with SOA proof, malformed/truncated/referral/error and mixed-private negative cases |
| DNS budget and ownership | Shared request/network accounting, response cap, deadline/cancel and cleanup fixtures |
| DNS provenance | Source in receipts; forged origin markers removed; unrelated/stale errors cannot poison successful navigation |
| Real Chrome HTTPS DNS failure | Headless owned browser reaches the guard failure; `dns_resolution` and all three cleanup fields verified |
| Existing public DOM behavior | Real headless Document suite and repeated-content-type suite pass |
| Help, generated skill, capabilities and completions | Same-version export comparison and full CLI tests pass |
| Portability | Linux/amd64 and Windows/amd64 compile with CGO disabled; no runtime claim |

Independent race checks passed for the resource/DNS/failure contracts and CLI.
No user browser profiles, physical display settings, audio or clipboard were
used. Fetch creates no Chrome process. The browser integration cases use only
their own headless processes, profiles, servers and proxy fixtures.

## Real public-network observations

The affected image was:

```text
https://cdn-uploads.huggingface.co/production/uploads/640e21ef3c82bd463ee5a76d/aooU0a3DMtYmy_1IWMaIM.png
```

1. System resolution returned unrelated rotating public addresses. Both native
   and Go resolution agreed; changing Go resolver mode did not solve it.
2. Google and Cloudflare HTTPS DNS agreed on the CloudFront CNAME and address
   set. A research numeric CONNECT to one such address, with the original
   hostname's TLS certificate verified, fetched the complete 1,322,097-byte PNG
   in 5.743 seconds. The production solution exposes an explicit resolver source;
   it contains no image-host IP rule, hostname CONNECT or DNS fallback.
3. An intermediate 0.17 candidate with explicit DoH received `200 image/png`
   after about 3.4 seconds, then reached the default 20-second deadline after
   434,176 decoded bytes. It reported 472,538 guard bytes, `reason:timeout`,
   `usable:false`, `network_closed:true`, and published no body file.
4. Contemporaneous controls were also slow: standalone Go without the DW guard
   timed out with partial bodies; Python numeric CONNECT completed in 59.445 and
   83.578 seconds, and ordinary hostname-proxy Python completed in 52.384 seconds.
   This establishes link-wide variability at that time, rather than a DW-only
   body-copy failure. Earlier fast results do not guarantee a 20-second transfer.

The nine-image research run completed all nine GETs, SHA-256 checks and full
image decoding with 10,943,729 body bytes and 11,202,612 guard bytes, within the
unchanged 8 MiB per-body and 32 MiB aggregate network limits. Its explicit
600-second per-job timeout is research-only: the 8,009,386-byte GIF took 513.094
seconds (all 40 frames decoded); the other GIF took 105.121 seconds (18 frames).
The 1,322,097-byte PNG took 38.132 seconds. All nine cleanup receipts reported
network closed, and no child process was sampled under their CLI jobs.
These results establish completeness, not default production latency. Consumers
must retain their production deadlines, truthful placeholders and explicit
bounded retries; no failed or incomplete file becomes a cache hit.

An installed 0.17.0 binary also fetched a 56,465-byte PNG in 6.631 seconds under
the default 20-second budget, with matching hash and successful image decoding.
0.17.1 changes only AI protocol ordering and evidence guidance: the UI task
chooser and interaction workflow come first; auxiliary reads cannot substitute
for required screenshot or interaction evidence. Acquisition behavior is unchanged.

The independent WeChat long-link challenge case also remains open; this release
does not change URLs, read user profiles or bypass site challenges.

## Reproduction

```sh
go test -race ./browser -run '^(TestResource|TestPublicDNS|TestDNSResolver|TestDocument)' -count=1
go test ./cmd/dw-browser -count=1
go test -tags integration ./browser -run '^TestDocumentLive$|^TestDocumentDuplicateContentTypeLive$|^TestDocumentDoHHTTPSFailureLive$' -count=1
```

The last command starts owned headless Chrome. Broader unrelated display tests
are deliberately outside this resource-fetch acceptance scope.
