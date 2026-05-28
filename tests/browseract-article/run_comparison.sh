#!/usr/bin/env bash
# BrowserAct 文章 fixture 多路径对比测试。
#
# 输出:
#   tests/browseract-article/reports/<run-id>/summary.md
#   tests/browseract-article/reports/<run-id>/summary.json

set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CASE_DIR="$ROOT/tests/browseract-article"
FIXTURE_DIR="$CASE_DIR/fixture"
REPORT_ROOT="$CASE_DIR/reports"
RUN_ID="${RUN_ID:-$(date +%Y%m%d-%H%M%S)-$$}"
OUT="$REPORT_ROOT/$RUN_ID"
PORT="${PORT:-18765}"
BASE_URL="http://127.0.0.1:$PORT"
DW="${DW:-$ROOT/bin/dw-browser}"
DW_MODE="${DW_MODE:-headless}"

mkdir -p "$OUT"

SERVER_PID=""
cleanup() {
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" >/dev/null 2>&1 || true
  fi
  "$DW" close --session "browseract-plan-$RUN_ID" >/dev/null 2>&1 || true
  "$DW" close --session "browseract-vision-$RUN_ID" >/dev/null 2>&1 || true
}
trap cleanup EXIT

status_of_json() {
  python3 - "$1" <<'PY'
import json, sys
path = sys.argv[1]
try:
    data = json.load(open(path, encoding="utf-8"))
except Exception:
    print("MISSING")
    raise SystemExit(0)
if isinstance(data, dict):
    print(data.get("status") or data.get("Status") or "UNKNOWN")
elif isinstance(data, list):
    statuses = [str(item.get("status", "UNKNOWN")) for item in data if isinstance(item, dict)]
    if "FAIL" in statuses:
        print("FAIL")
    elif "VISUAL_SUSPECT" in statuses:
        print("VISUAL_SUSPECT")
    elif "BLOCKED" in statuses:
        print("BLOCKED")
    elif statuses and all(s == "PASS" for s in statuses):
        print("PASS")
    else:
        print("UNKNOWN")
else:
    print("UNKNOWN")
PY
}

write_summary() {
  python3 - "$OUT" "$RUN_ID" "$BASE_URL" <<'PY'
import json
import os
import sys
from datetime import datetime, timezone

out, run_id, base_url = sys.argv[1:]

routes = [
    ("a11y", "A11y 确定性路径", "a11y/result.json", "稳定结构化控件和正文事实"),
    ("nl_step", "NL step BDD 路径", "nl-step/result.json", "自然语言 step 到 planner/executor"),
    ("plan_do", "plan/do 路径", "plan-do/do.json", "直接 AI-native 计划与执行"),
    ("vision", "Vision LLM 路径", "vision/check.json", "截图级 UI oracle"),
]

summary = {
    "schema": "browseract-article-comparison/v1",
    "run_id": run_id,
    "base_url": base_url,
    "created_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
    "routes": [],
}

def read_json(rel):
    path = os.path.join(out, rel)
    try:
        return json.load(open(path, encoding="utf-8"))
    except Exception as exc:
        return {"status": "MISSING", "reason": str(exc)}

for key, name, rel, purpose in routes:
    data = read_json(rel)
    status = "UNKNOWN"
    reason = ""
    if isinstance(data, dict):
        status = data.get("status", "UNKNOWN")
        reason = data.get("reason") or data.get("error") or ""
        if key == "plan_do" and data.get("errors"):
            status = "FAIL"
            reason = "; ".join(data.get("errors") or [])
        elif key == "plan_do" and "errors" in data:
            status = "PASS"
            reason = "plan/do 执行完成且 errors 为空"
        elif key == "plan_do" and data.get("plan") and data.get("observation"):
            status = "PASS"
            reason = "plan/do 执行完成并返回最终 observation"
    elif isinstance(data, list):
        statuses = [item.get("status", "UNKNOWN") for item in data if isinstance(item, dict)]
        if "FAIL" in statuses:
            status = "FAIL"
        elif "VISUAL_SUSPECT" in statuses:
            status = "VISUAL_SUSPECT"
        elif "BLOCKED" in statuses:
            status = "BLOCKED"
        elif statuses and all(s == "PASS" for s in statuses):
            status = "PASS"
        reason = "; ".join(str(item.get("reason", "")) for item in data if isinstance(item, dict))
    summary["routes"].append({
        "key": key,
        "name": name,
        "purpose": purpose,
        "status": status,
        "artifact": rel,
        "reason": reason[:500],
    })

with open(os.path.join(out, "summary.json"), "w", encoding="utf-8") as f:
    json.dump(summary, f, ensure_ascii=False, indent=2)

lines = []
lines.append(f"# BrowserAct 文章多路径测试对比报告\n")
lines.append(f"- Run ID: `{run_id}`")
lines.append(f"- Fixture: `{base_url}/`")
lines.append(f"- 时间: `{summary['created_at']}`\n")
lines.append("## 结果矩阵\n")
lines.append("| 路径 | 状态 | 用途 | 证据 |")
lines.append("|---|---|---|---|")
for r in summary["routes"]:
    lines.append(f"| {r['name']} | `{r['status']}` | {r['purpose']} | `{r['artifact']}` |")
lines.append("\n## 初步结论\n")
for r in summary["routes"]:
    if r["status"] == "PASS":
        verdict = "可作为当前能力基线。"
    elif r["status"] == "BLOCKED":
        verdict = "外部依赖或 oracle 不可用，不能证明产品失败。"
    elif r["status"] == "VISUAL_SUSPECT":
        verdict = "视觉 oracle 认为存在风险，需要人工或截图复核。"
    else:
        verdict = "需要继续定位并修复。"
    lines.append(f"- {r['name']}: `{r['status']}`。{verdict}")
    if r.get("reason"):
        lines.append(f"  - 备注: {r['reason'][:240]}")
lines.append("\n## 推荐组合\n")
lines.append("- A11y 确定性路径用于发布门禁。")
lines.append("- plan/do 用于 AI-native 可执行性验证，失败优先归因 planner contract。")
lines.append("- NL step 用于 BDD 场景探索，稳定后再转确定性断言。")
lines.append("- Vision LLM 用于布局和视觉语义巡检，BLOCKED 时记录 provider/截图证据。")

with open(os.path.join(out, "summary.md"), "w", encoding="utf-8") as f:
    f.write("\n".join(lines) + "\n")
PY
}

echo "== BrowserAct 文章多路径测试 =="
echo "ROOT=$ROOT"
echo "DW=$DW"
echo "BASE_URL=$BASE_URL"
echo "OUT=$OUT"
echo

if [ ! -x "$DW" ]; then
  echo "dw-browser 不存在，开始构建: $DW"
  (cd "$ROOT" && env -u GOROOT go build -o bin/dw-browser ./cmd/dw-browser) || exit 2
fi

python3 -m http.server "$PORT" --bind 127.0.0.1 --directory "$FIXTURE_DIR" >"$OUT/http-server.log" 2>&1 &
SERVER_PID=$!
sleep 1
if ! curl -fsS "$BASE_URL/" >"$OUT/fixture.html" 2>"$OUT/curl.stderr"; then
  echo "fixture server 不可达"
  exit 2
fi

echo "-- A11y 确定性路径 --"
mkdir -p "$OUT/a11y"
"$DW" journey --file "$CASE_DIR/journeys/a11y-deterministic.yaml" --base-url "$BASE_URL" --evidence "$OUT/a11y/evidence" >"$OUT/a11y/result.json" 2>"$OUT/a11y/stderr.log"
A11Y_CODE=$?
echo "A11y exit=$A11Y_CODE status=$(status_of_json "$OUT/a11y/result.json")"

echo "-- NL step BDD 路径 --"
mkdir -p "$OUT/nl-step"
"$DW" journey --file "$CASE_DIR/journeys/nl-step.yaml" --base-url "$BASE_URL" --evidence "$OUT/nl-step/evidence" >"$OUT/nl-step/result.json" 2>"$OUT/nl-step/stderr.log"
NL_CODE=$?
echo "NL step exit=$NL_CODE status=$(status_of_json "$OUT/nl-step/result.json")"

echo "-- plan/do 路径 --"
mkdir -p "$OUT/plan-do"
SID="browseract-plan-$RUN_ID"
"$DW" open "$BASE_URL/" --session "$SID" --ephemeral --mode "$DW_MODE" >"$OUT/plan-do/open.json" 2>"$OUT/plan-do/open.stderr"
GOAL="在关键词筛选框输入 stealth-extract，然后点击生成测试摘要按钮，最后确认页面出现测试摘要"
"$DW" plan --session "$SID" "$GOAL" --out "$OUT/plan-do/plan.json" >"$OUT/plan-do/plan.stdout" 2>"$OUT/plan-do/plan.stderr"
PLAN_CODE=$?
"$DW" do --session "$SID" --plan-file "$OUT/plan-do/plan.json" >"$OUT/plan-do/do.json" 2>"$OUT/plan-do/do.stderr"
DO_CODE=$?
"$DW" observe --session "$SID" --layers structural,behavior,visual --out "$OUT/plan-do/observe-after.json" >"$OUT/plan-do/observe-after.stdout" 2>"$OUT/plan-do/observe-after.stderr"
echo "plan exit=$PLAN_CODE do exit=$DO_CODE"

echo "-- Vision LLM 路径 --"
mkdir -p "$OUT/vision"
VSID="browseract-vision-$RUN_ID"
"$DW" open "$BASE_URL/" --session "$VSID" --ephemeral --mode "$DW_MODE" >"$OUT/vision/open.json" 2>"$OUT/vision/open.stderr"
"$DW" observe --session "$VSID" --layers structural,visual --out "$OUT/vision/observe.json" >"$OUT/vision/observe.stdout" 2>"$OUT/vision/observe.stderr"
"$DW" check --observation "$OUT/vision/observe.json" --using=visual --assert "截图中可见中文页面，顶部有大标题，中部有交互筛选输入框，下方有能力卡片，主要内容没有明显重叠" --out "$OUT/vision/check.json" >"$OUT/vision/check.stdout" 2>"$OUT/vision/check.stderr"
VISION_CODE=$?
echo "Vision exit=$VISION_CODE status=$(status_of_json "$OUT/vision/check.json")"

write_summary

echo
echo "报告: $OUT/summary.md"
sed -n '1,220p' "$OUT/summary.md"

if [ "$A11Y_CODE" -ne 0 ] || [ "$DO_CODE" -ne 0 ]; then
  exit 1
fi
exit 0
