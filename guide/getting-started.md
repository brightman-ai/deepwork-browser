# Getting started with DW-Browser v2

Use Go 1.26+ and Chrome/Chromium. Build into a project artifact directory; installing globally is a separate explicit action.

```sh
mkdir -p artifacts
GOMAXPROCS=2 go build -p 2 -o artifacts/dw-browser ./cmd/dw-browser
artifacts/dw-browser help
```

Create with `open URL --id NAME --scenario app-test-explore`. Creation-only persona, engine, rendering, profile and evidence parameters remain in the session. The default task profile is temporary; `--profile NAME` selects persistent storage. Duplicate IDs and missing IDs on observe/act are errors.

`observe --id NAME` captures an actual PNG and a canonical observation. Read its image and visible controls. `--content text|list|table`, `--tree`, `--health` and `--hit-audit` add facts to that collector. A missing telemetry channel does not mean zero errors. Static pages with zero refs can still have a valid image.

Use typed actions, keeping each literal text separate from the request:

```sh
dw-browser act fill "$REF" --id NAME --text-file draft.txt
dw-browser act type --focused --id NAME --text 'append this'
dw-browser act click "$REF" --id NAME --count 2 --button left
dw-browser act navigate --id NAME --url http://127.0.0.1:8080
dw-browser session viewport --id NAME --size 880x720
```

Observe again before a new ref or screenshot-coordinate action. A failed/unknown dispatch is not a reason to repeat fill/submit automatically. `eval --id NAME 'EXPRESSION'` is gated debugging, records intervention and revokes old authority. It cannot establish an independent Witness experience.

`once URL --scenario webvisit` creates, uses the same observation contract, and closes its own short-lived session. Multi-step work belongs in a typed `journey --file FILE`. The [generated journey example](runtime-v2/generated/dw-browser/references/journey.yaml) roundtrips through the strict loader. Its selectors must be adapted to the actual UI; a candidate remains separate from an accepted baseline.

Advanced operations retain their specific capabilities:

```sh
dw-browser tabs new --id NAME --url http://127.0.0.1:8080
dw-browser tabs select --id NAME --target TARGET_ID
dw-browser htr status --id NAME
dw-browser htr takeover --id NAME
dw-browser htr yield --id NAME
dw-browser cookie-import --id NAME --browser chrome --domain example.com
dw-browser audit --id NAME --suite a11y --threshold 85 --format json
dw-browser record start --id NAME
dw-browser record export --id NAME
dw-browser record stop --id NAME
dw-browser skills read chatgpt.com --action send-message
```

Cookie import retains the native database/keychain importer; it is an explicit authentication operation. Audit has compatibility, accessibility, layout, performance and browser chrome rules, with named suites and individual checks. Trace requests/outcomes are typed, and sensitive text is redacted. Site recipes are planning material; they never rescue a failed input through an internal model.

For precise required parameters and conditional groups use `help COMMAND` or [the generated reference](runtime-v2/generated/dw-browser/references/commands.md). [Migration](runtime-v2/migration.md) gives the current Go and HTTP ports.
