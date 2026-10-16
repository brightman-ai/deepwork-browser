# Public HTTP resource fetch

Choose the result your task needs:

| Task | Command | Result |
| --- | --- | --- |
| Download image, file, or unrendered HTML bytes | `fetch URL --save FILE` | Complete HTTP body plus metadata |
| Read HTML after browser JavaScript | `document URL` | Current main-frame DOM |
| Experience, inspect, or interact with a UI | `open` → `observe` → `act` | Session, real screenshots, action receipts |
| Replay accepted product behavior | `journey` | Deterministic acceptance evidence |

`fetch` runs no browser. It never changes representation or tools after an
error. `document` is not an image downloader; downloading an HTML page does not
prove that its UI works.

```sh
dw-browser fetch https://example.com/image.png \
  --save ./image.png --output ./image.meta.json
```

Create both parent directories first. `--save` is required and receives binary
body bytes. stdout is one JSON receipt; optional `--output` receives the same
JSON. The two paths must not alias, including filesystem case/Unicode aliases,
symlinks and hard links.

## Acquisition contract

The operation is an anonymous HTTP GET: no caller headers, credentials,
cookies, session attachment, browser mode, JavaScript or private-host grants.
Redirects are limited to five; every destination is validated again. Only a
complete 2xx response is accepted, excluding partial-content status 206. HTTP
transfer framing and supported content encoding are decoded; bytes are never
converted to text. An empty complete response may truthfully contain zero bytes.

Defaults are `--timeout 20s`, `--max-bytes 8388608` (8 MiB decoded body),
`--max-network-bytes 33554432` (32 MiB guard accounting) and
`--network-route system`. The acquisition deadline includes reading and hashing.
Network shutdown has a separate bounded cleanup period. Over-budget, truncated,
canceled and unclean results fail; no partial body is published.

Routing uses the same public destination guard as `document`. All addresses in
the chosen DNS answer must pass the public-IP policy before any origin dial;
connections use only numeric addresses from that snapshot. A supported static
system HTTP proxy is trusted to honor numeric CONNECT. Explicit `direct`
ignores proxy settings. Neither proxy failure nor DNS failure changes routes.
This guard is a request boundary, not an OS firewall.

## Explicit DNS source

`fetch` and `document` share one `--dns-resolver` option. Its default is
`system`; it never switches automatically or changes the computer's DNS.
When your task explicitly authorizes a different public resolver, use:

```sh
dw-browser fetch https://example.com/image.png --save ./image.png \
  --dns-resolver https://1.1.1.1/dns-query
```

The endpoint must be HTTPS on port 443, have a literal public IP and exact
`/dns-query` path, and contain no credentials, query or fragment. TLS must verify
the endpoint IP certificate. This avoids a separate bootstrap hostname lookup.
DNS-over-HTTPS uses the selected system/direct route and the same job's request,
network-byte, deadline and cleanup boundaries. Each DNS response is separately
bounded to 64 KiB. No DNS endpoint redirects, cookies or authentication are used.

Both A and AAAA queries must complete correctly. A valid NODATA response for one
family permits public addresses from the other; errors, truncation, incomplete
CNAME chains and mixed private/public answers reject the origin connection.
There is no DNS cache: every dial takes a fresh validated snapshot; redirects
are validated and resolved again. The selected source is reported as
`policy.dns_resolver` in success and failure receipts.

Choosing a public resolver sends it destination hostnames, not the original URL
path or response content. This is a caller trust/privacy decision. A timeout is
not authorization to switch resolvers, and unknown/private hostnames must not be
automatically retried through a public resolver.

## Receipts and ownership

A successful exit returns `dw.resource.v1`:

```json
{
  "schema": "dw.resource.v1",
  "requested_url": "https://example.com/image.png",
  "url": "https://example.com/image.png",
  "status_code": 200,
  "content_type": "image/png",
  "representation": "http-body",
  "bytes": 1234,
  "sha256": "<lowercase SHA-256 of the complete decoded body>",
  "body_file": "./image.png",
  "cleanup": {"network_closed": true}
}
```

The receipt also includes `tool_version` and `policy`, with selected routing,
budgets and observed request/byte counters. No browser/profile cleanup is
invented for an operation that creates neither.

The caller owns the published file. A cache consumer should use a fresh private
directory, require a successful exit and expected schema/URL/status/MIME,
verify a regular file, exact size and SHA-256, then decode or otherwise validate
its content. HTTP 200 and a MIME declaration alone cannot distinguish an image
from a challenge page. Delete only files the caller owns.

`FetchResource(ctx, ResourceRequest)` is the shared Go kernel. It returns bounded
`Resource.Body []byte` (`json:"-"`) and metadata; it does not write files.
The CLI adapter alone publishes the file.

## Failure and budget settlement

Acquisition or persistence failures exit 2 and return
`dw.resource.failure.v1`, `usable:false`, stage/reason, known URL/status/MIME,
`body_bytes_read`, policy, timing and cleanup. They return no partial body or
usable `body_file`.

The CLI uses its common error envelope: the typed diagnostic is at
`partial_result`, not at the top level. A Go caller instead receives
`*ResourceAcquisitionError` with `.Diagnostic`. For example:

```json
{
  "success": false,
  "phase": "fetch",
  "dispatch_state": "unknown",
  "error": "context deadline exceeded",
  "recovery": "Inspect the failure diagnostic and retry explicitly; no body file from this attempt is usable",
  "partial_result": {
    "schema": "dw.resource.failure.v1",
    "usable": false,
    "stage": "request",
    "reason": "timeout",
    "body_bytes_read": 0,
    "policy": {
      "dns_resolver": "system",
      "network_bytes": 1536
    },
    "cleanup": {"network_closed": true}
  }
}
```

This abbreviated example omits URL, content type, budgets, routing and timing
fields. Argument/output-preflight failures use the ordinary validation envelope
without a resource diagnostic and have not started acquisition. Treat their
read/network consumption as zero. `dns_resolution` distinguishes rejected DNS
responses from `network_transport`; policy rejection, timeout and cancellation
retain their own reasons.

`body_bytes_read` is the actual decoded body read, including a prefix returned
with an error. A zero-read failure reports explicit `0`; status rejection does
not read the error body. Detecting decoded overflow reads at most the limit plus
one byte. Skipped redirect bodies and rejected HTTP-status bodies are not read
by the resource client and contribute zero to this decoded counter. The network
guard may already have received their bytes; those remain in `network_bytes`.
Consumers must settle reservations from actual counters, never charge
the maximum body reservation for every failed connection.

`policy.network_bytes` has the shared guard's accounting semantics: HTTP body
chunks or bidirectional encrypted CONNECT chunks offered to its budget boundary.
It excludes HTTP headers, can exceed the limit by a rejected chunk, and is not a
wire-byte meter. Decoded body bytes and network accounting are different budgets;
do not sum them as though they measured the same thing. Across multiple jobs the
caller owns its aggregate budget.

Publication is atomic for the body: a private sibling temporary file is renamed
only after complete acquisition and successful network cleanup. Earlier failures
preserve an existing destination. Body publication, optional metadata publication
and stdout delivery are separate commit points, in that order. A late delivery
failure exits 2 and cannot roll back an already published body or metadata.
When stdout is available, delivery diagnostics retain byte counts without
advertising a body file. If stdout itself fails, use the nonzero exit/stderr and
any already committed metadata for accounting, or settle conservatively.
