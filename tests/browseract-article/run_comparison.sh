#!/usr/bin/env bash
# Typed authored workflows are release gates; visual advice is informational.
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CASE_DIR="$ROOT/tests/browseract-article"
RUN_ID="${RUN_ID:-$(date +%Y%m%d-%H%M%S)-$$}"
OUT="$CASE_DIR/reports/$RUN_ID"
PORT="${PORT:-18765}"
BASE_URL="http://127.0.0.1:$PORT"
DW="${DW:-$ROOT/bin/dw-browser}"
VSID="browseract-vision-$RUN_ID"
SERVER_PID=""
VISION_OPENED=0
mkdir -p "$OUT"
cleanup() {
  if [ "$VISION_OPENED" -eq 1 ]; then
    "$DW" close --id "$VSID" >"$OUT/vision/close.json" 2>"$OUT/vision/close.stderr"
  fi
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" >/dev/null 2>&1
  fi
}
trap cleanup EXIT
if [ ! -x "$DW" ]; then
  (cd "$ROOT" && GOMAXPROCS=2 go build -o "$DW" ./cmd/dw-browser) || exit 2
fi
python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$CASE_DIR/fixture" >"$OUT/http-server.log" 2>&1 &
SERVER_PID=$!
sleep 1
if ! curl -fsS "$BASE_URL/" >"$OUT/fixture.html" 2>"$OUT/curl.stderr"; then
  exit 2
fi
mkdir -p "$OUT/a11y" "$OUT/authored" "$OUT/vision"
"$DW" journey --file "$CASE_DIR/journeys/a11y-deterministic.yaml" --scenario app-test-baseline --base-url "$BASE_URL" --evidence "$OUT/a11y/evidence" >"$OUT/a11y/result.json" 2>"$OUT/a11y/stderr.log"
A11Y_CODE=$?
"$DW" journey --file "$CASE_DIR/journeys/nl-step.yaml" --scenario app-test-baseline --base-url "$BASE_URL" --evidence "$OUT/authored/evidence" >"$OUT/authored/result.json" 2>"$OUT/authored/stderr.log"
AUTHORED_CODE=$?
"$DW" open "$BASE_URL/" --id "$VSID" --scenario app-test-baseline --mode headless --evidence "$OUT/vision/evidence" >"$OUT/vision/open.json" 2>"$OUT/vision/open.stderr"
OPEN_CODE=$?
VISION_CODE=2
if [ "$OPEN_CODE" -eq 0 ]; then
  VISION_OPENED=1
  "$DW" observe --id "$VSID" --output "$OUT/vision/observe.json" >"$OUT/vision/observe.stdout" 2>"$OUT/vision/observe.stderr"
  OBSERVE_CODE=$?
  if [ "$OBSERVE_CODE" -eq 0 ]; then
    "$DW" check --observation "$OUT/vision/observe.json" --using visual --assert "截图中可见中文页面，顶部有大标题，中部有交互筛选输入框，下方有能力卡片，主要内容没有明显重叠" --output "$OUT/vision/check.json" >"$OUT/vision/check.stdout" 2>"$OUT/vision/check.stderr"
    VISION_CODE=$?
  fi
fi
python3 - "$OUT" "$RUN_ID" "$BASE_URL" "$A11Y_CODE" "$AUTHORED_CODE" "$VISION_CODE" <<'PY'
import json, pathlib, sys
from datetime import datetime, timezone
out, run_id, base_url, *codes = sys.argv[1:]
root = pathlib.Path(out)
summary = {'schema':'browseract-article-comparison/v2','run_id':run_id,'base_url':base_url,
           'created_at':datetime.now(timezone.utc).isoformat(),'routes':[]}
for (key, name, artifact, gate), code in zip([
 ('a11y','Deterministic article workflow','a11y/result.json',True),
 ('authored','Caller-authored typed workflow','authored/result.json',True),
 ('vision','Visual advice','vision/check.json',False)], codes):
    try:
        data=json.loads((root/artifact).read_text())
        status=data.get('status','ERROR' if int(code) else 'UNKNOWN')
    except Exception as exc:
        data={'error':str(exc)}; status='ERROR'
    summary['routes'].append(dict(key=key,name=name,artifact=artifact,required=gate,
                                 exit_code=int(code),status=status,result=data))
(root/'summary.json').write_text(json.dumps(summary,ensure_ascii=False,indent=2)+'\n')
lines=['# BrowserAct article comparison','',f'Run: `{run_id}`',f'Fixture: `{base_url}/`','',
       '| Workflow | Status | Exit | Required | Evidence |','|---|---|---|---|---|']
for r in summary['routes']:
    lines.append(f"| {r['name']} | {r['status']} | {r['exit_code']} | {r['required']} | {r['artifact']} |")
lines.extend(['','The two authored journeys contain typed actions and deterministic assertions.',
              'Visual advice cannot establish required PASS. Its provider/evidence errors remain in the report.',
              'Runtime natural-language planning and legacy plan/do commands are retired; caller planning precedes the typed journey boundary.'])
(root/'summary.md').write_text('\n'.join(lines)+'\n')
PY
printf 'Report: %s/summary.md\n' "$OUT"
if [ "$A11Y_CODE" -ne 0 ] || [ "$AUTHORED_CODE" -ne 0 ]; then
  exit 1
fi
exit 0
