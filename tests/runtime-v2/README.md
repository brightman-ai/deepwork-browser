# Runtime v2 system acceptance

These harnesses start their own loopback fixtures and browser runtimes, use real
typed input, and close only their own resources. Run them serially. Results are
saved under `reports/` (ignored); each CLI run pins the binary SHA-256.

```sh
GOMAXPROCS=2 go build -p 2 -buildvcs=false -o /tmp/dw-browser-v2 ./cmd/dw-browser
python3 tests/runtime-v2/acceptance.py --binary /tmp/dw-browser-v2 --label smoke
python3 tests/runtime-v2/baseline.py /tmp/dw-browser-v2
python3 tests/runtime-v2/session_edges.py --binary /tmp/dw-browser-v2 --evidence /tmp/dw-session-edges
GOMAXPROCS=2 go run -p 2 tests/runtime-v2/http_acceptance.go /tmp/dw-http-evidence
```

The CLI harness checks actual edit/save/revisit, exact UTF-8 save receipts,
cross-process focus and resize, stale references/coordinates, canvas input,
Human takeover, cancellation, masked sensitive input and canonical artifacts.
The baseline harness requires broken input → BLOCKED, restored input → PASS,
wrong expected bytes → FAIL, restored assertion → PASS, with independent server
persistence. Session-edge checks verify debug intervention revokes authority, wait
conditions and deadlines, and the real public-origin debug gate. The Go/HTTP harness borrows a real core, checks persistence failure
and authority fences, observation/focused-input parity, reordered same-name button refs,
privacy probe negatives, and proves server closure retains the host's core.

Read the generated PNGs to assess actual visual evidence. These scripts are
system checks; they do not replace a cold-context AI Witness using the generated
tool protocol. Platform/engine claims require actual runs on those environments.
