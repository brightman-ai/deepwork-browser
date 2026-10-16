# DW-Browser 0.16.0-v2 command contract

Generated from executable declarations. Flags on attached sessions inherit creation environment. `--output` is JSON; `--screenshot` is PNG. Absolute and locator-relative points require `--observation-id` from the exact capture; opaque ref-relative points supply capture identity in the token. Gesture endpoints share that identity.

## `help`

Show the same executable command contract. Positional arguments: 0..2. Required flags: .

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `open`

Create one session; duplicate IDs are rejected. Positional arguments: 1..1. Required flags: --scenario, --id.

- `--scenario` VALUE (string); values app-test-explore|app-test-baseline|webvisit
- `--mode` VALUE (string); values headless|headed|visible
- `--persona` VALUE (string)
- `--engine` VALUE (string); values chrome|safari
- `--safari-device` VALUE (string)
- `--viewport` VALUE (string)
- `--profile` VALUE (string)
- `--evidence` VALUE (string)
- `--allow-host` VALUE (string)
- `--browser-chrome` VALUE (string); values auto|on|off
- `--user-agent` VALUE (string)
- `--goal` VALUE (string)
- `--stealth`
- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `close`

Close only the named session. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--force`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `once`

Capture one canonical UI observation in an owned short-lived session. Positional arguments: 1..1. Required flags: --scenario.

- `--scenario` VALUE (string); values app-test-explore|app-test-baseline|webvisit
- `--mode` VALUE (string); values headless|headed|visible
- `--persona` VALUE (string)
- `--engine` VALUE (string); values chrome|safari
- `--safari-device` VALUE (string)
- `--viewport` VALUE (string)
- `--profile` VALUE (string)
- `--evidence` VALUE (string)
- `--allow-host` VALUE (string)
- `--browser-chrome` VALUE (string); values auto|on|off
- `--user-agent` VALUE (string)
- `--goal` VALUE (string)
- `--stealth`
- `--screenshot` VALUE (string)
- `--content` VALUE (string); values text|list|table
- `--health`
- `--tree`
- `--all`
- `--annotate`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `document`

Read public URL DOM in a disposable browser target; no screenshot or session. Positional arguments: 1..1. Required flags: .

Usage: `document URL [--mode headless|headed] [--network-route system|direct] [--timeout DURATION]`

- `URL`: Public HTTP(S) URL; no existing browser session or profile is used.

Default headless, system route. Headed uses a windowless hidden target, not a visible browser and not UI acceptance evidence. No mode fallback or automatic challenge retry. System route honors supported trusted proxy configuration only after validating numeric public destinations; direct deliberately ignores proxy configuration. A captured login or verification page is not article acceptance. Read rendering, policy, cleanup and failure diagnostics.

- `--timeout` VALUE (duration); Whole read budget including queue and browser startup; cleanup has an independent bounded deadline.
- `--max-html-bytes` VALUE (integer)
- `--max-network-bytes` VALUE (integer)
- `--network-route` VALUE (string); values system|direct; Transport configuration: system (default) or explicit direct; public destination validation remains required in both.
- `--mode` VALUE (string); values headless|headed; DOM rendering: headless (default) or headful hidden target without a native window
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `observe`

Capture a canonical observation and actual screenshot. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--screenshot` VALUE (string)
- `--content` VALUE (string); values text|list|table
- `--scope` VALUE (string); values visible|all
- `--top` VALUE (integer)
- `--budget` VALUE (integer)
- `--health`
- `--tree`
- `--all`
- `--hit-audit`
- `--annotate`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `check`

Judge required assertions against one canonical observation. Positional arguments: 0..0. Required flags: .

- `--id` VALUE (session-id)
- `--observation` VALUE (string)
- `--assert` VALUE (string)
- `--file` VALUE (string)
- `--using` VALUE (string)
- `--vision-url` VALUE (string)
- `--vision-provider` VALUE (string)
- `--vision-model` VALUE (string)
- `--vision-api-key` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--id --observation] requires=[] forbids=[].
- Rule: when="" unless="" exactly-one=[--assert --file] requires=[] forbids=[].

## `diff`

Compare two canonical observations. Positional arguments: 2..2. Required flags: .

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `wait`

Wait for one assertion or a declared duration. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--assert` VALUE (string)
- `--timeout` VALUE (duration)
- `--duration` VALUE (duration)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--assert --duration] requires=[] forbids=[].
- Rule: when="--assert" unless="" exactly-one=[] requires=[--timeout] forbids=[].
- Rule: when="--duration" unless="" exactly-one=[] requires=[] forbids=[--timeout].

## `journey`

Run typed deterministic steps through the shared runtime. Positional arguments: 0..0. Required flags: --file.

- `--scenario` VALUE (string); values app-test-explore|app-test-baseline|webvisit
- `--mode` VALUE (string); values headless|headed|visible
- `--persona` VALUE (string)
- `--engine` VALUE (string); values chrome|safari
- `--safari-device` VALUE (string)
- `--viewport` VALUE (string)
- `--profile` VALUE (string)
- `--evidence` VALUE (string)
- `--allow-host` VALUE (string)
- `--browser-chrome` VALUE (string); values auto|on|off
- `--user-agent` VALUE (string)
- `--goal` VALUE (string)
- `--stealth`
- `--id` VALUE (session-id)
- `--file` VALUE (string)
- `--base-url` VALUE (string)
- `--fail-fast`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="--id" unless="" exactly-one=[] requires=[] forbids=[--scenario --mode --persona --engine --safari-device --viewport --profile --evidence --allow-host --browser-chrome --user-agent --goal --stealth].
- Rule: when="" unless="--id" exactly-one=[] requires=[--scenario] forbids=[].

## `eval`

Run gated JavaScript debugging and revoke old observation authority. Positional arguments: 1..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `cookie-import`

Import native browser cookies with keychain decryption and a domain filter. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--browser` VALUE (string)
- `--domain` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `audit`

Audit compatibility, accessibility, layout, performance and browser chrome rules. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--suite` VALUE (string)
- `--check` VALUE (string)
- `--threshold` VALUE (integer)
- `--format` VALUE (string); values json|compact
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `session status`

Inspect persisted runtime ownership without recovery. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `session list`

List persisted sessions. Positional arguments: 0..0. Required flags: .

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `session viewport`

Resize the existing runtime and persist its environment. Positional arguments: 0..0. Required flags: --id, --size.

- `--id` VALUE (session-id)
- `--size` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `tabs list`

List user page targets without recovering the runtime. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `tabs select`

Activate an existing target under the runtime input lock. Positional arguments: 0..0. Required flags: --id, --target.

- `--id` VALUE (session-id)
- `--target` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `tabs close`

Close a target and revoke page authority. Positional arguments: 0..0. Required flags: --id, --target.

- `--id` VALUE (session-id)
- `--target` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `tabs new`

Create a target in the existing browser. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--url` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `htr status`

Inspect Human control and runtime state without recovery. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `htr takeover`

Set Human/AI authority; obsolete observations are revoked. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `htr yield`

Set Human/AI authority; obsolete observations are revoked. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `htr share`

Set Human/AI authority; obsolete observations are revoked. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `persona list`

List environment personas. Positional arguments: 0..0. Required flags: .

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `profile list`

List persistent and temporary profile directories. Positional arguments: 0..0. Required flags: .

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `profile import`

Copy a profile into the CLI profile store. Positional arguments: 1..1. Required flags: .

- `--name` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `profile prune`

Prune task profile directories explicitly. Positional arguments: 0..0. Required flags: --ephemeral.

- `--min-age` VALUE (duration)
- `--ephemeral`
- `--dry-run`
- `--reap-orphaned`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `gc`

Inspect or reclaim unowned browser resources explicitly. Positional arguments: 0..0. Required flags: .

- `--older-than` VALUE (duration)
- `--dry-run`
- `--all`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `skills list`

List installed site recipes. Positional arguments: 0..0. Required flags: .

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `skills read`

Read one site recipe. Positional arguments: 1..1. Required flags: .

- `--action` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `skills write`

Write a site recipe from declared input. Positional arguments: 1..1. Required flags: --action.

- `--action` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `record start`

Start a structured action trace. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `record stop`

Stop and return the trace. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `record export`

Return the current trace. Positional arguments: 0..0. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `muxhost serve`

Run the existing headed host infrastructure. Positional arguments: 0..0. Required flags: .

- `--muxhost-id` VALUE (string)
- `--runtime-id` VALUE (string)
- `--browser-session-id` VALUE (string)
- `--kind` VALUE (string); values task|interactive|service|debug|test
- `--profile-id` VALUE (string)
- `--profile-dir` VALUE (string)
- `--chrome-path` VALUE (string)
- `--debug-port` VALUE (integer)
- `--owner-pid` VALUE (integer)
- `--owner` VALUE (string)
- `--isolation` VALUE (string)
- `--mode` VALUE (string); values headless|headed|visible
- `--goal` VALUE (string)
- `--idle-ttl` VALUE (duration)
- `--persona` VALUE (string); empty string permitted
- `--preset` VALUE (string); empty string permitted
- `--width` VALUE (integer)
- `--height` VALUE (integer)
- `--user-agent` VALUE (string)
- `--service` VALUE (string)
- `--account` VALUE (string)
- `--identity-key` VALUE (string)
- `--touch`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `muxhost ensure`

Ensure an explicitly named headed host. Positional arguments: 0..0. Required flags: .

- `--muxhost-id` VALUE (string)
- `--runtime-id` VALUE (string)
- `--browser-session-id` VALUE (string)
- `--kind` VALUE (string); values task|interactive|service|debug|test
- `--profile-id` VALUE (string)
- `--profile-dir` VALUE (string)
- `--chrome-path` VALUE (string)
- `--debug-port` VALUE (integer)
- `--owner-pid` VALUE (integer)
- `--owner` VALUE (string)
- `--isolation` VALUE (string)
- `--mode` VALUE (string); values headless|headed|visible
- `--goal` VALUE (string)
- `--idle-ttl` VALUE (duration)
- `--persona` VALUE (string); empty string permitted
- `--preset` VALUE (string); empty string permitted
- `--width` VALUE (integer)
- `--height` VALUE (integer)
- `--user-agent` VALUE (string)
- `--service` VALUE (string)
- `--account` VALUE (string)
- `--identity-key` VALUE (string)
- `--touch`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `muxhost status`

Inspect an existing headed host. Positional arguments: 0..0. Required flags: .

- `--muxhost-id` VALUE (string)
- `--runtime-id` VALUE (string)
- `--browser-session-id` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--muxhost-id --runtime-id --browser-session-id] requires=[] forbids=[].

## `muxhost release`

Release an existing headed host lease. Positional arguments: 0..0. Required flags: .

- `--muxhost-id` VALUE (string)
- `--runtime-id` VALUE (string)
- `--browser-session-id` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--muxhost-id --runtime-id --browser-session-id] requires=[] forbids=[].

## `muxhost shutdown`

Shut down the explicitly named headed host. Positional arguments: 0..0. Required flags: .

- `--muxhost-id` VALUE (string)
- `--runtime-id` VALUE (string)
- `--browser-session-id` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--muxhost-id --runtime-id --browser-session-id] requires=[] forbids=[].

## `version`

Return the candidate protocol version. Positional arguments: 0..0. Required flags: .

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `capabilities`

Return this executable command and action contract. Positional arguments: 0..0. Required flags: .

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `export-contract`

Generate same-version capabilities, completions and tool-use skill. Positional arguments: 0..0. Required flags: --directory.

- `--directory` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `completion`

Generate shell completion from this command contract. Positional arguments: 1..1. Required flags: .

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `protocol`

Generate the same-version tool-use protocol. Positional arguments: 0..0. Required flags: .

- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act click`

Click a visible target; count 2 or button right selects its mouse variant. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--x` VALUE (number)
- `--y` VALUE (number)
- `--unit` VALUE (string); values css-px|percent
- `--frame` VALUE (string); values viewport|target
- `--observation-id` VALUE (string)
- `--button` VALUE (string); values left|right
- `--count` VALUE (integer); values 1|2
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="--x" unless="" exactly-one=[] requires=[--y] forbids=[].
- Rule: when="--y" unless="" exactly-one=[] requires=[--x] forbids=[].
- Rule: when="--unit" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--frame" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--observation-id" unless="" exactly-one=[] requires=[--x --y] forbids=[].

## `act tap`

Touch a visible target. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--x` VALUE (number)
- `--y` VALUE (number)
- `--unit` VALUE (string); values css-px|percent
- `--frame` VALUE (string); values viewport|target
- `--observation-id` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="--x" unless="" exactly-one=[] requires=[--y] forbids=[].
- Rule: when="--y" unless="" exactly-one=[] requires=[--x] forbids=[].
- Rule: when="--unit" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--frame" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--observation-id" unless="" exactly-one=[] requires=[--x --y] forbids=[].

## `act hover`

Move the pointer to a visible target. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--x` VALUE (number)
- `--y` VALUE (number)
- `--unit` VALUE (string); values css-px|percent
- `--frame` VALUE (string); values viewport|target
- `--observation-id` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="--x" unless="" exactly-one=[] requires=[--y] forbids=[].
- Rule: when="--y" unless="" exactly-one=[] requires=[--x] forbids=[].
- Rule: when="--unit" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--frame" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--observation-id" unless="" exactly-one=[] requires=[--x --y] forbids=[].

## `act fill`

Replace editable text using verified physical selection and input. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--text` VALUE (string); empty string permitted
- `--text-file` VALUE (string)
- `--sensitive`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--text --text-file] requires=[] forbids=[].
- Rule: when="--text" unless="" exactly-one=[] requires=[] forbids=[--text-file].

## `act type`

Insert text into a target or the physically focused editor. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--text` VALUE (string); empty string permitted
- `--text-file` VALUE (string)
- `--focused`
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--text --text-file] requires=[] forbids=[].
- Rule: when="--text" unless="" exactly-one=[] requires=[] forbids=[--text-file].

## `act press`

Dispatch a platform key or shortcut. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--key` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act focus`

Physically focus an editable target. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act select`

Select a native option using verified keyboard interaction; explicit baseline element steps provide synthetic functional evidence. Positional arguments: 1..1. Required flags: --id.

Usage: `act select TARGET --id SESSION (--label LABEL | --value VALUE)`

- `TARGET`: Control ref returned by observe, or an explicit css= or role= locator for the select control.

Placeholders in angle brackets must be replaced. First observe the session, read the control's option labels and DOM values, choose exactly one matching field, execute, then observe again. Do not guess values from labels; duplicate matches fail. Copy a selectable choice's select.options[].option unchanged into Action.option; for CLI map its sole label or value field directly to --label or --value, preserving exact strings including empty strings.

Example: `dw-browser act select <CONTROL_REF_FOR_来源> --id <SESSION> --label '完整内容'`

Example: `dw-browser act select <CONTROL_REF_FOR_来源> --id <SESSION> --value '<EXACT_DOM_VALUE_FROM_OBSERVE>'`

- `--id` VALUE (session-id); Session ID used by observe; this is not the control target.
- `--label` VALUE (string); empty string permitted; Exact complete displayed option label only; no DOM-value fallback. Supply exactly one of --label or --value.
- `--value` VALUE (string); empty string permitted; Exact DOM option value only; no displayed-label fallback. Supply exactly one of --label or --value.
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="" unless="" exactly-one=[--label --value] requires=[] forbids=[].

## `act set-checked`

Set a checkbox to an explicit boolean. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--checked` VALUE (string); values true|false
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act scroll`

Scroll the viewport or target at a declared point. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--x` VALUE (number)
- `--y` VALUE (number)
- `--unit` VALUE (string); values css-px|percent
- `--frame` VALUE (string); values viewport|target
- `--observation-id` VALUE (string)
- `--direction` VALUE (string); values up|down|left|right
- `--steps` VALUE (integer)
- `--delta-x` VALUE (number)
- `--delta-y` VALUE (number)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="--x" unless="" exactly-one=[] requires=[--y] forbids=[].
- Rule: when="--y" unless="" exactly-one=[] requires=[--x] forbids=[].
- Rule: when="--unit" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--frame" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--observation-id" unless="" exactly-one=[] requires=[--x --y] forbids=[].

## `act scrollto`

Scroll a persistent target into view. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act drag`

Drag between two points in one coordinate frame. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--x` VALUE (number)
- `--y` VALUE (number)
- `--unit` VALUE (string); values css-px|percent
- `--frame` VALUE (string); values viewport|target
- `--observation-id` VALUE (string)
- `--end-x` VALUE (number)
- `--end-y` VALUE (number)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="--x" unless="" exactly-one=[] requires=[--y] forbids=[].
- Rule: when="--y" unless="" exactly-one=[] requires=[--x] forbids=[].
- Rule: when="--unit" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--frame" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--observation-id" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--end-x" unless="" exactly-one=[] requires=[--end-y --x --y] forbids=[].
- Rule: when="--end-y" unless="" exactly-one=[] requires=[--end-x --x --y] forbids=[].

## `act swipe`

Swipe between two touch points. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--x` VALUE (number)
- `--y` VALUE (number)
- `--unit` VALUE (string); values css-px|percent
- `--frame` VALUE (string); values viewport|target
- `--observation-id` VALUE (string)
- `--end-x` VALUE (number)
- `--end-y` VALUE (number)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
- Rule: when="--x" unless="" exactly-one=[] requires=[--y] forbids=[].
- Rule: when="--y" unless="" exactly-one=[] requires=[--x] forbids=[].
- Rule: when="--unit" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--frame" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--observation-id" unless="" exactly-one=[] requires=[--x --y] forbids=[].
- Rule: when="--end-x" unless="" exactly-one=[] requires=[--end-y --x --y] forbids=[].
- Rule: when="--end-y" unless="" exactly-one=[] requires=[--end-x --x --y] forbids=[].

## `act navigate`

Navigate the existing session to a URL. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--url` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act reload`

Reload the existing document. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act back`

Traverse browser history backwards. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act forward`

Traverse browser history forwards. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act zoom`

Set page scale. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--value` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`

## `act keyboard`

Show or hide the emulated mobile keyboard. Positional arguments: 0..1. Required flags: --id.

- `--id` VALUE (session-id)
- `--value` VALUE (string)
- `--output` VALUE (string)
- `--lock-wait` VALUE (lock-duration)
- `--verbose`
