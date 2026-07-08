// testing.go — observe / diff / check / journey 命令实现
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
	btest "github.com/brightman-ai/deepwork-browser/browser/testing"
	"gopkg.in/yaml.v3"
)

// ============================================================
// § LLM/VLM CLI flags
// ============================================================

// llmFlags 包含 LLM/VLM 相关的 CLI 参数。
type llmFlags struct {
	llmURL         string // --llm-url (Ollama/vLLM/OpenAI-compatible endpoint)
	llmProvider    string // --llm-provider (auto|ollama|openai)
	llmModel       string // --llm-model (model name, e.g. google/gemma-4-26b-a4b-it)
	llmAPIKey      string // --llm-api-key
	visionURL      string // --vision-url / --vlm-url (defaults to llm-url)
	visionProvider string // --vision-provider / --vlm-provider (defaults to llm-provider)
	visionModel    string // --vision-model / --vlm-model (defaults to llm-model)
	visionAPIKey   string // --vision-api-key / --vlm-api-key (defaults to llm-api-key)
}

// parseLLMFlags 从 args 中提取 LLM/VLM 参数，返回剩余 args。
// CLI flags 优先级高于环境变量。VLM flags 接受 vision/vlm 两组别名；
// 未显式配置 VLM 时继承 LLM 的 endpoint/provider/model/key，适配 Gemma 4 这类同模态模型。
func parseLLMFlags(args []string) (llmFlags, []string) {
	var lf llmFlags
	remaining := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case (arg == "--llm-url" || arg == "--llm-endpoint") && i+1 < len(args):
			lf.llmURL = args[i+1]
			i++
		case strings.HasPrefix(arg, "--llm-url="):
			lf.llmURL = arg[len("--llm-url="):]
		case strings.HasPrefix(arg, "--llm-endpoint="):
			lf.llmURL = arg[len("--llm-endpoint="):]
		case arg == "--llm-provider" && i+1 < len(args):
			lf.llmProvider = args[i+1]
			i++
		case strings.HasPrefix(arg, "--llm-provider="):
			lf.llmProvider = arg[len("--llm-provider="):]
		case arg == "--llm-model" && i+1 < len(args):
			lf.llmModel = args[i+1]
			i++
		case strings.HasPrefix(arg, "--llm-model="):
			lf.llmModel = arg[len("--llm-model="):]
		case arg == "--llm-api-key" && i+1 < len(args):
			lf.llmAPIKey = args[i+1]
			i++
		case strings.HasPrefix(arg, "--llm-api-key="):
			lf.llmAPIKey = arg[len("--llm-api-key="):]
		case (arg == "--vision-url" || arg == "--vlm-url") && i+1 < len(args):
			lf.visionURL = args[i+1]
			i++
		case strings.HasPrefix(arg, "--vision-url="):
			lf.visionURL = arg[len("--vision-url="):]
		case strings.HasPrefix(arg, "--vlm-url="):
			lf.visionURL = arg[len("--vlm-url="):]
		case (arg == "--vision-provider" || arg == "--vlm-provider") && i+1 < len(args):
			lf.visionProvider = args[i+1]
			i++
		case strings.HasPrefix(arg, "--vision-provider="):
			lf.visionProvider = arg[len("--vision-provider="):]
		case strings.HasPrefix(arg, "--vlm-provider="):
			lf.visionProvider = arg[len("--vlm-provider="):]
		case (arg == "--vision-model" || arg == "--vlm-model") && i+1 < len(args):
			lf.visionModel = args[i+1]
			i++
		case strings.HasPrefix(arg, "--vision-model="):
			lf.visionModel = arg[len("--vision-model="):]
		case strings.HasPrefix(arg, "--vlm-model="):
			lf.visionModel = arg[len("--vlm-model="):]
		case (arg == "--vision-api-key" || arg == "--vlm-api-key") && i+1 < len(args):
			lf.visionAPIKey = args[i+1]
			i++
		case strings.HasPrefix(arg, "--vision-api-key="):
			lf.visionAPIKey = arg[len("--vision-api-key="):]
		case strings.HasPrefix(arg, "--vlm-api-key="):
			lf.visionAPIKey = arg[len("--vlm-api-key="):]
		default:
			remaining = append(remaining, arg)
		}
	}

	// Apply to environment so downstream code (NewPlanner/NewVisionOracle) picks them up.
	if lf.llmURL != "" {
		os.Setenv("DW_BROWSER_LLM_URL", lf.llmURL)
	}
	if lf.llmProvider != "" {
		os.Setenv("DW_BROWSER_LLM_PROVIDER", lf.llmProvider)
	}
	if lf.llmModel != "" {
		os.Setenv("DW_BROWSER_LLM_MODEL", lf.llmModel)
	}
	if lf.llmAPIKey != "" {
		os.Setenv("DW_BROWSER_LLM_API_KEY", lf.llmAPIKey)
	}
	if lf.visionURL != "" {
		os.Setenv("DW_BROWSER_VISION_URL", lf.visionURL)
	} else if lf.llmURL != "" {
		// vision defaults to llm endpoint if not specified separately
		os.Setenv("DW_BROWSER_VISION_URL", lf.llmURL)
	}
	if lf.visionProvider != "" {
		os.Setenv("DW_BROWSER_VISION_PROVIDER", lf.visionProvider)
	} else if lf.llmProvider != "" {
		os.Setenv("DW_BROWSER_VISION_PROVIDER", lf.llmProvider)
	}
	if lf.visionModel != "" {
		os.Setenv("DW_BROWSER_VISION_MODEL", lf.visionModel)
	} else if lf.llmModel != "" {
		os.Setenv("DW_BROWSER_VISION_MODEL", lf.llmModel)
	}
	if lf.visionAPIKey != "" {
		os.Setenv("DW_BROWSER_VISION_API_KEY", lf.visionAPIKey)
	} else if lf.llmAPIKey != "" {
		os.Setenv("DW_BROWSER_VISION_API_KEY", lf.llmAPIKey)
	}

	return lf, remaining
}

// visionEnabled 判断是否应启用 VisionOracle。
// ROOT-C: DW_BROWSER_VISION_* env 只是端点 CONFIG（在哪调用），不是激活信号。
// 激活只来自显式 opt-in——usingRaw 含 "visual"，env 绝不隐式激活，故一次
// deterministic 运行不会静默调用 LLM oracle。端点由 NewVisionOracle 自行读 env。
func visionEnabled(usingRaw string) bool {
	for _, u := range strings.Split(usingRaw, ",") {
		if strings.TrimSpace(u) == "visual" {
			return true
		}
	}
	return false
}

// autoLoadLLMEnv sources ~/.deepwork/testing-llm.env into the process environment.
// It fills only missing keys so explicit environment/CLI values still win, while
// allowing VLM keys to be backfilled even when only DW_BROWSER_LLM_URL is preset.
// Existing env vars are never overridden (explicit beats file).
func autoLoadLLMEnv() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(home, ".deepwork", "testing-llm.env"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if kv, ok := strings.CutPrefix(line, "export "); ok {
			line = kv
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k != "" && os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

// isNLGoal returns true when action is a free-form natural-language goal
// rather than a deterministic browser command. NL goals are routed through
// the LLM Planner; structural commands go directly to ActWithSessionMode.
//
// Structural prefixes: click, fill, press, scroll, select, navigate, wait, type, hover, back, forward.
// Anything else (e.g. "在浏览器中搜索 arxiv") is treated as an NL goal.
func isNLGoal(action string) bool {
	lower := strings.ToLower(strings.TrimSpace(action))
	for _, p := range []string{
		"click ", "fill ", "press ", "scroll ", "select ",
		"navigate ", "wait ", "type ", "hover ",
	} {
		if strings.HasPrefix(lower, p) {
			return false
		}
	}
	// Bare "back" and "forward" are valid structural commands.
	if lower == "back" || lower == "forward" {
		return false
	}
	return len(lower) > 0
}

// specHasNLGoal reports whether any journey/recovery step carries a free-form
// natural-language goal that needs the LLM planner. It reuses isNLGoal — the
// same step-type judgement used at execution time — so the classification is
// single-sourced. Activation is spec-driven (ROOT-C: env is CONFIG, not a
// trigger), so a purely structural spec never spins up the planner.
func specHasNLGoal(spec *btest.JourneySpec) bool {
	if spec == nil {
		return false
	}
	for _, steps := range [][]btest.StepSpec{spec.Journey, spec.Recovery} {
		for _, s := range steps {
			if isNLGoal(s.Do) {
				return true
			}
		}
	}
	return false
}

// specHasVisualUsing reports whether any assertion in the spec opts into the
// visual oracle via using:[visual]. Only explicit per-assertion opt-in counts.
func specHasVisualUsing(spec *btest.JourneySpec) bool {
	if spec == nil {
		return false
	}
	stepsHaveVisual := func(steps []btest.StepSpec) bool {
		for _, s := range steps {
			for _, a := range s.Check {
				if cliUsingContainsVisual(a.Using) {
					return true
				}
			}
		}
		return false
	}
	if stepsHaveVisual(spec.Journey) || stepsHaveVisual(spec.Recovery) {
		return true
	}
	if spec.Baseline != nil {
		for _, a := range spec.Baseline.Invariants {
			if cliUsingContainsVisual(a.Using) {
				return true
			}
		}
	}
	return false
}

// containsHelp returns true if args contains --help or -h.
func containsHelp(args []string) bool {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			return true
		}
	}
	return false
}

// printTestingHelp 打印 testing 子命令的完整帮助信息。
func printTestingHelp() {
	help := `dw-browser 基线回归命令 (场景 B — 确定性, CLI 零 LLM):

  journey   跑 BDD YAML 旅程 (open/act/check 步) + 证据采集, pass/fail
  check     对当前会话或 observation 文件求值断言
  diff      对比两份 observe 快照 (before/after)

用法:

  # BDD 旅程 + 证据 (CI 主入口)
  dw-browser journey --file tests/bdd/portal.yaml --evidence evidence/run-001
  dw-browser journey --file spec.yaml --base-url http://localhost:8080 --fail-fast

  # 单条断言 (当前会话)
  dw-browser check --id s1 --assert "console_errors_count == 0"
  dw-browser check --id s1 --assert "exists(role='button', name='创建')"

  # 批量断言 (baseline 的 invariants 段)
  dw-browser check --id s1 --file baselines/portal/app-shell.yaml

  # 对已落盘 observation 求值 (无需会话)
  dw-browser observe --id s1 --json before.json
  dw-browser check --observation before.json --assert "exists(role='button')"

  # 对比前后
  dw-browser diff before.json after.json --out diff.json

断言 DSL:

  exists(role='button', name='Search')    元素存在于 A11y 树
  count(role='link') >= 1                 匹配元素计数
  visible(testid='sidebar')               元素可见
  gone(text='Loading')                    元素已消失
  text_contains('hello')                  页面文本含字符串
  url_matches('/portal/topic')            URL 含 pattern
  tab_count == 2                          标签数
  active_tab_url_contains('example.com')  活动标签 URL 含
  console_errors_count == 0               无 console 错误
  network_failures_count == 0             无网络失败
  latency_lt(3000)                        动作延迟低于阈值 (ms)

软审查 (soft-review): CLI 只跑确定性 hard 断言。视觉/语义判断 (UX 对不对)
是调用方的活 — agent 自己 Read 截图判, 或 baseline YAML 里标 soft hint 交调用方。
CLI 默认不起 LLM; 仅 --using visual 显式开启 vision oracle (opt-in):

  --llm-url / --llm-provider / --llm-model / --llm-api-key   Planner/VLM 端点 (env: DW_BROWSER_LLM_*)
  --vision-url / --vision-model / --vision-api-key           VLM (默认回退 --llm-*; env: DW_BROWSER_VISION_*)
  dw-browser check --id s1 --assert "looks_like('登录表单')" --using visual

Exit codes: 0=PASS  1=FAIL  2=RUN_ERROR
`
	fmt.Fprint(os.Stderr, help)
}

// ============================================================
// § observe 命令
// ============================================================

// visibleErrorScanJS scans the rendered page for error UI a human notices instantly but
// console/network telemetry misses (CHG-016 R3 — the "errs==0 but red banner on screen"
// silent-failure gap). Four signal classes, deduped, capped, fail-safe: (1) W3C semantic
// markers role=alert / aria-invalid / aria-errormessage; (2) error-styled leaf elements;
// (3) red-colored visible text (computed color); (4) multilingual error keywords. Returns
// [{kind,text,selector}]. Best practice: surface signals for the agent to INSPECT, not a
// hard verdict — so false positives cost a look, never a missed error.
const visibleErrorScanJS = `(() => {
  const out = [], seen = new Set();
  const vis = el => { try { const r = el.getBoundingClientRect(); return el.offsetParent !== null && r.width > 0 && r.height > 0; } catch (e) { return false; } };
  const push = (kind, text, sel) => {
    text = (text || '').replace(/\s+/g, ' ').trim().slice(0, 200);
    if (!text) return;
    const key = kind + '|' + text;
    if (seen.has(key)) return; seen.add(key);
    out.push({ kind: kind, text: text, selector: (sel || '').toString().slice(0, 80) });
  };
  try { document.querySelectorAll('[role=alert],[aria-invalid=true],[aria-errormessage]').forEach(el => {
    if (out.length < 25 && vis(el) && el.textContent.trim()) push('aria', el.textContent, el.getAttribute('role') || 'aria-invalid');
  }); } catch (e) {}
  try { document.querySelectorAll('[class*=error i],[class*=danger i],[class*=invalid i],[class*=failed i],[data-error]').forEach(el => {
    if (out.length < 25 && el.children.length === 0 && vis(el) && el.textContent.trim()) push('styled', el.textContent, el.className || el.tagName);
  }); } catch (e) {}
  try { document.querySelectorAll('body *').forEach(el => {
    if (out.length >= 25 || el.children.length !== 0 || !el.textContent.trim() || !vis(el)) return;
    const c = (getComputedStyle(el).color || ''), m = c.match(/rgba?\((\d+),\s*(\d+),\s*(\d+)/);
    if (m) { const R = +m[1], G = +m[2], B = +m[3]; if (R > 150 && G < 90 && B < 90) push('color', el.textContent, 'red:' + el.tagName); }
  }); } catch (e) {}
  const kw = /(invalid context|failed to|exception|not found|timed? ?out|unavailable|请求失败|加载失败|无法连接|连接失败|出错了|未找到|无效|异常|不可用|报错)/i;
  try { document.querySelectorAll('body *').forEach(el => {
    if (out.length >= 25 || el.children.length !== 0 || !vis(el)) return;
    const t = el.textContent.trim();
    if (t && t.length < 160 && kw.test(t)) push('keyword', t, el.tagName.toLowerCase());
  }); } catch (e) {}
  return out.slice(0, 20);
})()`

// runObserve 采集当前 session 的多通道 Observation 快照。
// dw-browser observe --id <session-id> [--layers structural,behavior,telemetry] [--out file.json]
// runObserve — SSOT 感知动词。瘦默认 + 加法 flag。
//
//	dw-browser observe --id X              瘦默认 {elements@rN, user_state, run_id, step} ~3K
//	  --out m.png                          + 存截图 → {screenshot: "<path>"} (AI Read 图判 UX)
//	  --health                             + {telemetry:{console_errors,network_failures,visible_errors}}
//	  --tree                               + {tree: "<全 a11y 文本>"} (罕用)
//	  --top N / --budget B                 调 elements 列表 (默认 20 / 4096 字节)
//	  --json out.json                      把整个 JSON 落盘 (默认 stdout)
//
// flag 自由组合 (加法, 非互斥枚举)。observe 与 act 共享 @rN ref-space:
// observe 后 `act "click @rN"` 解析的是 observe 刚看到的同一批 refs。
func runObserve(args []string) {
	if containsHelp(args) {
		printObserveHelp()
		os.Exit(exitOK)
	}
	var jsonOut, outFile string
	wantHealth, wantTree := false, false
	topN, budget := defaultBriefTopN, defaultBriefBudget
	clean := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--out" && i+1 < len(args):
			outFile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--out="):
			outFile = arg[len("--out="):]
		case arg == "--json" && i+1 < len(args):
			jsonOut = args[i+1]
			i++
		case strings.HasPrefix(arg, "--json="):
			jsonOut = arg[len("--json="):]
		case arg == "--health":
			wantHealth = true
		case arg == "--tree":
			wantTree = true
		case arg == "--top" && i+1 < len(args):
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				topN = n
			}
			i++
		case strings.HasPrefix(arg, "--top="):
			if n, err := strconv.Atoi(arg[len("--top="):]); err == nil {
				topN = n
			}
		case arg == "--budget" && i+1 < len(args):
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				budget = n
			}
			i++
		case strings.HasPrefix(arg, "--budget="):
			if n, err := strconv.Atoi(arg[len("--budget="):]); err == nil {
				budget = n
			}
		default:
			clean = append(clean, arg)
		}
	}

	_, flags := parseCommonFlags(clean, "observe")
	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser observe: requires --id <session-id>")
		os.Exit(exitRunErr)
	}

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser observe: %v\n", err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	impl := connectSession(ctx, sessionInfo, "observe", flags)
	defer closeSessionCore(impl)
	impl.RestoreRefsFromSession(sessionInfo.Refs)

	// — 感知 a11y (始终) —
	sessionInfo.SnapEpoch++
	snap, err := impl.SnapWithSessionMode(ctx, sessionInfo.SnapEpoch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser observe: snap failed: %v\n", err)
		os.Exit(exitRunErr)
	}
	// settle-wait — retry until SPA a11y tree is populated (up to ~6s).
	for i := 0; i < 6 && (snap == nil || len(snap.Refs) == 0); i++ {
		time.Sleep(1 * time.Second)
		snap, _ = impl.SnapWithSessionMode(ctx, sessionInfo.SnapEpoch)
	}
	// SSOT: persist THIS observation's @rN refs so a subsequent `act "click @rN"`
	// resolves the SAME refs the caller just saw (observe shares act's ref-space).
	if snap != nil {
		sessionRefs := make([]browser.SessionRef, 0, len(snap.Refs))
		for _, ref := range snap.Refs {
			sessionRefs = append(sessionRefs, browser.SessionRef{
				Ref:           ref.Ref,
				BackendNodeID: ref.BackendNodeID,
				Role:          ref.Role,
				Name:          ref.NameFull,
				TestID:        ref.TestID,
				Placeholder:   ref.Placeholder,
			})
		}
		sessionInfo.Refs = sessionRefs
		sessionInfo.PageURL = snap.URL
		if err := browser.SaveSession(sessionInfo); err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser observe: save refs: %v\n", err)
		}
	}
	if snap == nil {
		fmt.Fprintln(os.Stderr, "dw-browser observe: empty snapshot")
		os.Exit(exitRunErr)
	}

	// — 瘦默认输出 {elements@rN, user_state, run_id, step} —
	elements, total, truncated := briefElements(snap.Refs, topN, budget)
	output := map[string]interface{}{
		"url":        snap.URL,
		"title":      snap.PageTitle,
		"user_state": buildUserState(snap, sessionInfo.PageURL),
		"elements":   elements,
		"shown":      len(elements),
		"total":      total,
	}
	if truncated {
		output["truncated"] = true
		output["hint"] = "更多元素被省略; --top N / --budget BYTES 放宽, 或 --tree 看全树"
	}
	injectEvidenceID(output, sessionInfo)
	injectSnapshotState(output, snap)
	if hint := formatSkillHint(snap.URL); hint != "" {
		output["skill_hint"] = hint
	}
	if ph := formatPersonaHint(sessionInfo.Scenario, sessionInfo.PersonaID); ph != "" {
		output["persona_hint"] = ph
	}

	// — 加法: --tree (全 a11y 文本) —
	if wantTree {
		output["tree"] = snap.Text
	}

	// — 加法: --out (截图落盘, 返回路径) —
	if outFile != "" {
		if shot, err2 := impl.Screenshot(ctx, false); err2 == nil {
			if werr := os.WriteFile(outFile, shot, 0o644); werr == nil {
				output["screenshot"] = outFile
			} else {
				fmt.Fprintf(os.Stderr, "dw-browser observe: write screenshot: %v\n", werr)
			}
		}
	}

	// — 加法: --health (console/network/visible_errors 诊断/grader lens) —
	if wantHealth {
		health := map[string]interface{}{
			"console_errors":   []btest.ConsoleEntry{},
			"network_failures": []btest.NetworkEntry{},
		}
		// visible-error scan — on-screen error UI a human sees instantly but
		// console/network telemetry MISSES (the "errs==0 but red banner" gap).
		var vis []btest.VisibleErrorEntry
		if err2 := impl.EvalJS(ctx, visibleErrorScanJS, &vis); err2 == nil && len(vis) > 0 {
			health["visible_errors"] = vis
		}
		health["_note"] = "console/network require persistent CDP; stateless CLI returns visible_errors (DOM scan) only"
		output["telemetry"] = health
	}

	writeJSONOrStdout(output, jsonOut, "observe")
	os.Exit(exitOK)
}

// printObserveHelp 打印 observe 的瘦默认 + 加法 flag 说明。
func printObserveHelp() {
	fmt.Fprintln(os.Stderr, "dw-browser observe — 感知当前会话 (SSOT 感知动词, 瘦默认 + 加法 flag)")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "用法:")
	fmt.Fprintln(os.Stderr, "  dw-browser observe --id X            瘦默认 {elements@rN, user_state, run_id, step} ~3K")
	fmt.Fprintln(os.Stderr, "  dw-browser observe --id X --out m.png + 存截图 → {screenshot:\"<path>\"} (AI Read 图判 UX)")
	fmt.Fprintln(os.Stderr, "  dw-browser observe --id X --health   + {telemetry:{console_errors,network_failures,visible_errors}}")
	fmt.Fprintln(os.Stderr, "  dw-browser observe --id X --tree     + {tree:\"<全 a11y 文本>\"} (罕用)")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "flag 自由组合 (加法, 非互斥):")
	fmt.Fprintln(os.Stderr, "  --out <path>    截图落盘路径 (返回 screenshot 字段)")
	fmt.Fprintln(os.Stderr, "  --health        附健康通道 (诊断/grader lens)")
	fmt.Fprintln(os.Stderr, "  --tree          附全 a11y 树文本")
	fmt.Fprintln(os.Stderr, "  --top <N>       elements 上限 (默认 20)")
	fmt.Fprintln(os.Stderr, "  --budget <B>    elements 输出字节硬上限 (默认 4096)")
	fmt.Fprintln(os.Stderr, "  --json <path>   整个 JSON 落盘 (默认 stdout)")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "证据关联: 每次输出带 run_id (会话稳定) + step (单调), 把截图↔a11y↔finding 对齐。")
	fmt.Fprintln(os.Stderr, "observe 后 act \"click @rN\" 解析 observe 刚看到的同一批 refs。")
}

// behaviorFromSessionInfo 从 SessionInfo 构建 BehaviorState（无需连接浏览器）。
func behaviorFromSessionInfo(info *browser.SessionInfo) *btest.BehaviorState {
	// SessionInfo 持有上次 snap 时的 PageURL，tabs 信息存在 AuthorityState 解析的地方。
	// stateless CLI 中最可靠的信息来源是 info.PageURL + info.Refs 计数。
	// Tabs 需要通过 FetchChromeTargets 从 CDP 获取实时状态。
	tabs := fetchTabsFromSession(info)

	activeTabID := ""
	for _, t := range tabs {
		if t.Active {
			activeTabID = t.ID
			break
		}
	}

	return &btest.BehaviorState{
		URL:         info.PageURL,
		Title:       "",
		Tabs:        tabs,
		ActiveTabID: activeTabID,
		TabCount:    len(tabs),
		LoadState:   "idle",
	}
}

// fetchTabsFromSession 通过 FetchChromeTargets 获取实时 tab 列表（尽力而为）。
func fetchTabsFromSession(info *browser.SessionInfo) []btest.TabState {
	if info.DebugPort <= 0 {
		// 无 debug port（Safari 等），回退到只有当前页 URL 的单 tab
		if info.PageURL != "" {
			return []btest.TabState{{
				ID:     info.TargetID,
				Index:  0,
				URL:    info.PageURL,
				Title:  "",
				Active: true,
			}}
		}
		return []btest.TabState{}
	}

	targets, err := browser.FetchChromeTargets(info.DebugPort)
	if err != nil {
		return []btest.TabState{}
	}

	tabs := make([]btest.TabState, 0, len(targets))
	for i, t := range targets {
		targetID := browser.ExtractDevToolsTargetID(t)
		url, _ := t["url"].(string)
		title, _ := t["title"].(string)
		active := targetID == info.TargetID
		tabs = append(tabs, btest.TabState{
			ID:     targetID,
			Index:  i,
			URL:    url,
			Title:  title,
			Active: active,
		})
	}
	return tabs
}

// ============================================================
// § diff 命令
// ============================================================

// runDiff 对比两个 Observation JSON 文件。
// dw-browser diff before.json after.json [--out diff.json]
func runDiff(args []string) {
	var outFile string
	positional := make([]string, 0, 2)

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--out" && i+1 < len(args):
			outFile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--out="):
			outFile = arg[len("--out="):]
		case arg == "--help" || arg == "-h":
			fmt.Fprintln(os.Stderr, "usage: dw-browser diff before.json after.json [--out diff.json]")
			os.Exit(exitOK)
		default:
			positional = append(positional, arg)
		}
	}

	if len(positional) < 2 {
		fmt.Fprintln(os.Stderr, "dw-browser diff: requires <before.json> <after.json>")
		os.Exit(exitRunErr)
	}

	before, err := loadObservationFile(positional[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser diff: %v\n", err)
		os.Exit(exitRunErr)
	}
	after, err := loadObservationFile(positional[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser diff: %v\n", err)
		os.Exit(exitRunErr)
	}

	diff := btest.ComputeDiff(before, after)
	writeJSONOrStdout(diff, outFile, "diff")
	os.Exit(exitOK)
}

// ============================================================
// § check 命令
// ============================================================

// runCheck 执行断言检查。
// dw-browser check --id <session-id> --assert "expr" [--out result.json]
// dw-browser check --id <session-id> --file baselines/app.yaml [--out results.json]
// dw-browser check --observation obs.json --assert "expr" [--out result.json]
func runCheck(args []string) {
	if containsHelp(args) {
		printTestingHelp()
		os.Exit(exitOK)
	}
	autoLoadLLMEnv()
	_, args = parseLLMFlags(args)
	var assertExpr string
	var specFile string
	var obsFile string
	var outFile string
	var usingRaw string
	clean := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--assert" && i+1 < len(args):
			assertExpr = args[i+1]
			i++
		case strings.HasPrefix(arg, "--assert="):
			assertExpr = arg[len("--assert="):]
		case arg == "--file" && i+1 < len(args):
			specFile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--file="):
			specFile = arg[len("--file="):]
		case arg == "--observation" && i+1 < len(args):
			obsFile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--observation="):
			obsFile = arg[len("--observation="):]
		case arg == "--out" && i+1 < len(args):
			outFile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--out="):
			outFile = arg[len("--out="):]
		case arg == "--using" && i+1 < len(args):
			usingRaw = args[i+1]
			i++
		case strings.HasPrefix(arg, "--using="):
			usingRaw = arg[len("--using="):]
		default:
			clean = append(clean, arg)
		}
	}

	_, flags := parseCommonFlags(clean, "check")

	// Tokenize --using once for consistent visual detection (substring matching
	// would false-match values like "nonvisual"/"previsual").
	var usingSlice []string
	if usingRaw != "" {
		for _, p := range strings.Split(usingRaw, ",") {
			usingSlice = append(usingSlice, strings.TrimSpace(p))
		}
	}

	// Deterministic hard-lock honors BOTH the CLI flag and the attached session's
	// persisted Policy.Deterministic (a baseline session locks determinism once at
	// open). Under deterministic, --using visual is a HARD reject — the vision
	// oracle is never activated, so a reproducible check can't silently invoke the
	// internal LLM.
	// determinism is a property of the attached session's scenario (app-test-baseline
	// locks it at open); it is no longer a per-command flag.
	deterministic := false
	if flags.sessionID != "" {
		if si, err := browser.LoadSession(flags.sessionID); err == nil && si.Policy.Deterministic {
			deterministic = true
		}
	}
	if deterministic && cliUsingContainsVisual(usingSlice) {
		fmt.Fprintln(os.Stderr, "dw-browser check: deterministic mode: internal LLM disabled — --using visual rejected (open the session with --scenario app-test-explore/webvisit to enable the vision oracle)")
		os.Exit(exitRunErr)
	}

	// 1. 获取 Observation
	var obs *btest.Observation
	if obsFile != "" {
		var err error
		obs, err = loadObservationFile(obsFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser check: %v\n", err)
			os.Exit(exitRunErr)
		}
	} else if flags.sessionID != "" {
		// observe 当前 session 状态
		obs = observeSession(flags, "check")
	} else {
		fmt.Fprintln(os.Stderr, "dw-browser check: requires --id <session-id> or --observation <file>")
		os.Exit(exitRunErr)
	}

	engine := btest.AssertionEngine{}
	// ROOT-C fix: DW_BROWSER_VISION_* env is CONFIG (where the endpoint is), NOT activation.
	// Vision activates only on explicit per-call opt-in (--using visual), never from ambient
	// env, so a deterministic `check` can never silently invoke the LLM oracle. The
	// !deterministic guard is belt-and-suspenders — deterministic + visual already exited above.
	if !deterministic && cliUsingContainsVisual(usingSlice) {
		engine.Vision = btest.NewVisionOracle()
	}
	hasFail := false

	// 2. 执行断言
	if specFile != "" {
		// 批量断言：从 baseline YAML 文件加载
		specs, err := loadAssertionSpecs(specFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser check: load spec: %v\n", err)
			os.Exit(exitRunErr)
		}
		results := engine.EvaluateAll(obs, specs)
		for _, r := range results {
			if r.Status == btest.StatusFail {
				hasFail = true
				break
			}
		}
		writeJSONOrStdout(results, outFile, "check")
	} else if assertExpr != "" {
		// 单条断言：--using 已在上方 tokenize，直接复用
		if !deterministic && engine.Vision == nil && cliUsingContainsVisual(usingSlice) {
			engine.Vision = btest.NewVisionOracle()
		}
		result := engine.EvaluateWithUsing(obs, assertExpr, usingSlice)
		if result.Status == btest.StatusBlocked &&
			strings.HasPrefix(result.Reason, "cannot parse assertion expression") &&
			cliUsingContainsVisual(usingSlice) {
			result = engine.EvaluateVisualOnly(obs, assertExpr, usingSlice)
		}
		if result.Status == btest.StatusFail {
			hasFail = true
		}
		writeJSONOrStdout(result, outFile, "check")
	} else {
		fmt.Fprintln(os.Stderr, "dw-browser check: requires --assert <expr> or --file <spec.yaml>")
		os.Exit(exitRunErr)
	}

	if hasFail {
		os.Exit(exitFail)
	}
	os.Exit(exitOK)
}

// observeSession 复用 observe 逻辑，从已有 session 采集 Observation。
func observeSession(flags commonFlags, cmdName string) *btest.Observation {
	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: %v\n", cmdName, err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	impl := connectSession(ctx, sessionInfo, cmdName, flags)
	defer closeSessionCore(impl)
	impl.RestoreRefsFromSession(sessionInfo.Refs)

	snap, err := impl.SnapWithSessionMode(ctx, sessionInfo.SnapEpoch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: snap failed: %v\n", cmdName, err)
		os.Exit(exitRunErr)
	}

	screenshotData, _ := impl.Screenshot(ctx, false)
	behavior := behaviorFromSessionInfo(sessionInfo)
	telemetry := &btest.TelemetryState{
		ConsoleErrors:   []btest.ConsoleEntry{},
		NetworkFailures: []btest.NetworkEntry{},
	}

	return btest.BuildObservation(flags.sessionID, snap, screenshotData, "", behavior, telemetry)
}

// loadAssertionSpecs 从 YAML 文件加载断言规格列表。
// 支持两种格式：
//  1. 顶层 []AssertionSpec 数组
//  2. 包含 invariants: [...] 字段的 BaselineRef 结构
func loadAssertionSpecs(path string) ([]btest.AssertionSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// 先尝试解析为 AssertionSpec 数组
	var specs []btest.AssertionSpec
	if err := yaml.Unmarshal(data, &specs); err == nil && len(specs) > 0 && specs[0].Assert != "" {
		return specs, nil
	}

	// 再尝试解析为 BaselineRef（含 invariants 字段）
	var baseline struct {
		Invariants []btest.AssertionSpec `yaml:"invariants"`
	}
	if err := yaml.Unmarshal(data, &baseline); err == nil && len(baseline.Invariants) > 0 {
		return baseline.Invariants, nil
	}

	return nil, fmt.Errorf("cannot parse assertion specs from %s: expected []AssertionSpec or baseline with invariants", path)
}

// ============================================================
// § journey 命令
// ============================================================

// runJourney 执行 BDD 旅程测试。
// dw-browser journey --file tests/bdd/bs15-smoke.yaml --evidence tests/evidence/run-001 [--fail-fast]
func runJourney(args []string) {
	// Auto-source ~/.deepwork/testing-llm.env before flag parsing so vision oracle
	// and NL planner are available without requiring the caller to source the file.
	autoLoadLLMEnv()
	_, args = parseLLMFlags(args)
	var specFile string
	var evidenceDir string
	var baseURLOverride string
	failFast := false
	clean := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--file" && i+1 < len(args):
			specFile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--file="):
			specFile = arg[len("--file="):]
		case arg == "--evidence" && i+1 < len(args):
			evidenceDir = args[i+1]
			i++
		case strings.HasPrefix(arg, "--evidence="):
			evidenceDir = arg[len("--evidence="):]
		case arg == "--base-url" && i+1 < len(args):
			baseURLOverride = args[i+1]
			i++
		case strings.HasPrefix(arg, "--base-url="):
			baseURLOverride = arg[len("--base-url="):]
		case arg == "--fail-fast":
			failFast = true
		case arg == "--help" || arg == "-h":
			fmt.Fprintln(os.Stderr, "usage: dw-browser journey --file <spec.yaml> [--evidence <dir>] [--base-url <url>] [--fail-fast]")
			fmt.Fprintln(os.Stderr, "  --file       BDD YAML spec file")
			fmt.Fprintln(os.Stderr, "  --evidence   directory to write evidence (default: evidence/<spec-id>)")
			fmt.Fprintln(os.Stderr, "  --base-url   override spec environment.base_url (e.g. http://localhost:8080)")
			fmt.Fprintln(os.Stderr, "  --fail-fast  stop on first step failure")
			os.Exit(exitOK)
		default:
			clean = append(clean, arg)
		}
	}

	_ = failFast // reserved for future implementation

	_, flags := parseCommonFlags(clean, "journey")

	if specFile == "" {
		fmt.Fprintln(os.Stderr, "dw-browser journey: requires --file <spec.yaml>")
		os.Exit(exitRunErr)
	}

	spec, err := btest.LoadSpec(specFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser journey: %v\n", err)
		os.Exit(exitRunErr)
	}

	if baseURLOverride != "" {
		spec.Environment.BaseURL = baseURLOverride
	}

	// 从 spec.Environment 覆盖 flags 中未指定的 session mode
	if !flags.modeExplicit && spec.Environment.Mode != "" {
		if mode, ok := parseBrowserMode(spec.Environment.Mode); ok {
			flags.mode = mode
			flags.headless = mode == browser.ModeHeadless
		}
	}

	if evidenceDir == "" {
		evidenceDir = fmt.Sprintf("evidence/%s-%d", spec.ID, time.Now().UnixMilli())
	}

	// 决定 session：spec 需要 session（通过 --id 提供）或新建 ephemeral session
	//
	// 内部 LLM 激活由 *spec 内容* + 非 deterministic 决定，而非 ambient env
	// （ROOT-C: env 是端点 CONFIG，不是激活信号）。determinism 需要 LLM 的
	// spec 硬拒并显式报错，而非静默跳过。
	//
	// determinism 由 *附着 session 的持久 Policy.Deterministic* 决定（不再有 CLI flag）：
	// 一次 app-test-baseline session 在 open 时锁定确定性，journey 复用它时硬锁必须仍然生效。
	deterministic := false
	if flags.sessionID != "" {
		if si, err := browser.LoadSession(flags.sessionID); err == nil && si.Policy.Deterministic {
			deterministic = true
		}
	}
	specNeedsPlanner := specHasNLGoal(spec)
	specNeedsVision := specHasVisualUsing(spec)
	if deterministic && (specNeedsPlanner || specNeedsVision) {
		fmt.Fprintln(os.Stderr, "dw-browser journey: deterministic mode: internal LLM disabled but spec requires NL planner / visual oracle")
		os.Exit(exitRunErr)
	}
	// Build planner once — shared across executor calls (both executors reject NL
	// goals when this is nil). nil unless the spec has NL goal steps and
	// determinism is off — so under deterministic the executors' planner is nil too.
	var journeyPlanner *btest.Planner
	if !deterministic && specNeedsPlanner {
		journeyPlanner = btest.NewPlanner()
	}

	var executor btest.ActionExecutor
	if flags.sessionID != "" {
		// 连接已有 session
		sessionInfo, loadErr := browser.LoadSession(flags.sessionID)
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "dw-browser journey: %v\n", loadErr)
			os.Exit(exitRunErr)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		impl := connectSession(ctx, sessionInfo, "journey", flags)
		impl.RestoreRefsFromSession(sessionInfo.Refs)

		executor = &cliActionExecutor{
			sessionID:   flags.sessionID,
			sessionInfo: sessionInfo,
			impl:        impl,
			ctx:         ctx,
			planner:     journeyPlanner,
		}
		defer closeSessionCore(impl)
	} else if spec.Environment.BaseURL != "" || spec.Environment.EntryURL != "" {
		// 新建 one-shot session（从 spec 的 entry URL）→ 无 --id 继承, 故 --scenario 必选。
		// 用不改 mode 的 scenarioPolicyOrExit: journey 的 render 仍走 spec.Environment.Mode
		// / --mode 优先级；仅当二者都未给时, 才落到场景默认 render。
		_, scenarioPolicy, scenMode := scenarioPolicyOrExit(&flags, "journey")
		if !flags.modeExplicit && spec.Environment.Mode == "" {
			flags.mode = scenMode
			flags.headless = scenMode == browser.ModeHeadless
		}
		entryURL := joinURL(spec.Environment.BaseURL, spec.Environment.EntryURL)
		profileID := fmt.Sprintf("journey-%s-%d", spec.ID, time.Now().UnixMilli())
		browserOpts := browserOptionsFromFlags(flags)
		bc := newBrowserCore(profileID, browserOpts...)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		// TASK C: Chrome-leak fix — wrap ephemeral cleanup in a panic-recovering defer.
		// Without the recover, a panic inside the journey runner would unwind past the
		// deferred cleanup without executing it, leaving a detached Chrome process behind
		// and eventually exhausting available CDP ports.
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "dw-browser journey: recovered panic, cleaning up Chrome: %v\n", r)
			}
			bc.Close(ctx)
			cleanupEphemeral(profileID)
		}()

		// FIX-J1: wire telemetry collector BEFORE Navigate so load-time events are captured.
		// Strategy: prefer the browser-level CDP context (browserCtx) which is the primary
		// target context and receives events for the initial navigation. The active-target
		// context from TargetTracker is the same object before first navigation, so either
		// works — but browserCtx is always non-nil and stable across target switches.
		var journeyTelemetry *browser.TelemetryCollector
		if prov, ok := bc.(browser.CDPContextProvider); ok {
			if cdpCtx := prov.CDPContext(); cdpCtx != nil {
				journeyTelemetry = browser.NewTelemetryCollector(cdpCtx)
			}
		}

		// 导航到入口 URL
		navSnap, navErr := bc.Navigate(ctx, entryURL)
		if navErr != nil {
			fmt.Fprintf(os.Stderr, "dw-browser journey: navigate to %s: %v\n", entryURL, navErr)
			os.Exit(exitRunErr)
		}
		// 进程内 open→act 路径：policy 由 scenario 导出（无持久 session）。
		navURL := entryURL
		if navSnap != nil && navSnap.URL != "" {
			navURL = navSnap.URL
		}
		bc.SetPolicy(scenarioPolicy, navURL)

		executor = &oneshotActionExecutor{impl: bc, ctx: ctx, telemetry: journeyTelemetry, planner: journeyPlanner}
	} else {
		fmt.Fprintln(os.Stderr, "dw-browser journey: requires --id <session-id> or spec.environment.base_url")
		os.Exit(exitRunErr)
	}

	runner, err := btest.NewRunner(executor, evidenceDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser journey: create runner: %v\n", err)
		os.Exit(exitRunErr)
	}
	// Vision oracle activates only when the spec explicitly opts in (using:[visual])
	// and determinism is off (CLI flag OR persisted session policy). Deterministic +
	// visual specs already errored above.
	if !deterministic && specNeedsVision {
		runner.SetVision(btest.NewVisionOracle())
	}

	// Use a generous timeout (70 min) to accommodate long AI research turns
	// that can each take 10+ minutes. Individual step timeouts are governed by
	// spec wait.timeout_ms; this outer deadline is a last-resort safety net.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 70*time.Minute)
	defer cancel2()

	result, err := runner.Run(ctx2, spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser journey: run error: %v\n", err)
		os.Exit(exitRunErr)
	}

	enc, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(enc))

	switch result.Status {
	case btest.StatusPass:
		os.Exit(exitOK)
	case btest.StatusFail:
		os.Exit(exitFail)
	default:
		os.Exit(exitRunErr)
	}
}

// ============================================================
// § ActionExecutor 实现 — session 模式
// ============================================================

// cliActionExecutor 连接已有 session 的 ActionExecutor 实现。
type cliActionExecutor struct {
	sessionID   string
	sessionInfo *browser.SessionInfo
	impl        browser.SessionCore
	ctx         context.Context
	planner     *btest.Planner // nil when LLM not configured; NL goals fail gracefully
}

func (e *cliActionExecutor) Execute(ctx context.Context, action string) error {
	trimmed := strings.TrimSpace(action)

	// Direct URL navigation — deterministic fast path.
	if rest, ok := strings.CutPrefix(strings.ToLower(trimmed), "navigate "); ok {
		target := strings.TrimSpace(trimmed[len(trimmed)-len(rest):]) // preserve original case
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "/") {
			if _, err := e.impl.Navigate(ctx, target); err != nil {
				return fmt.Errorf("navigate %s: %w", target, err)
			}
			return nil
		}
	}
	switch strings.ToLower(trimmed) {
	case "wait", "noop", "none":
		return nil
	}

	// NL goal — route through LLM planner when configured. In deterministic /
	// llm-off mode the planner is nil; reject explicitly rather than silently
	// feeding a natural-language goal to the structural action engine.
	if isNLGoal(trimmed) {
		if e.planner == nil {
			return fmt.Errorf("deterministic/llm-off: NL goal %q rejected, use structural action", trimmed)
		}
		return e.executeNLGoal(ctx, trimmed)
	}

	_, err := e.impl.ActWithSessionMode(ctx, action, false)
	return err
}

// executeNLGoal decomposes a natural-language goal into browser steps via the
// LLM Planner (skill-first, planner fallback — same strategy as `dw-browser do`).
func (e *cliActionExecutor) executeNLGoal(ctx context.Context, goal string) error {
	snap, _ := e.impl.SnapWithSessionMode(ctx, e.sessionInfo.SnapEpoch)
	var structural *btest.StructuralState
	pageURL := e.sessionInfo.PageURL
	if snap != nil {
		structural = doStructuralFromSnap(snap)
		if strings.TrimSpace(snap.URL) != "" {
			pageURL = snap.URL
		}
	}

	var plan *btest.PlanResult
	if steps := lookupSkillRecipe(pageURL, goal); steps != nil {
		plan = &btest.PlanResult{Goal: goal, Steps: steps}
	} else {
		var err error
		plan, err = e.planner.Plan(ctx, goal, structural)
		if err != nil {
			return fmt.Errorf("nl-plan %q: %w", goal, err)
		}
	}

	for _, step := range plan.Steps {
		switch strings.ToLower(strings.TrimSpace(step.Action)) {
		case "wait", "noop", "none":
			// The real synchronization is represented by step.Wait.
		default:
			if _, err := e.impl.ActWithSessionMode(ctx, step.Action, false); err != nil {
				return fmt.Errorf("nl-exec %q → step %q: %w", goal, step.Description, err)
			}
		}
		if step.Wait != "" {
			if err := e.Wait(ctx, step.Wait, 10000); err != nil {
				return fmt.Errorf("nl-exec %q → wait %q: %w", goal, step.Wait, err)
			}
		}
	}
	return nil
}

func (e *cliActionExecutor) Wait(ctx context.Context, condition string, timeoutMs int) error {
	if timeoutMs <= 0 {
		timeoutMs = 15000
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return runWaitCondition(waitCtx, e.impl, condition)
}

func (e *cliActionExecutor) Snapshot(ctx context.Context) (*browser.Snapshot, error) {
	return e.impl.SnapWithSessionMode(ctx, e.sessionInfo.SnapEpoch)
}

func (e *cliActionExecutor) Screenshot(ctx context.Context) ([]byte, error) {
	return e.impl.Screenshot(ctx, false)
}

func (e *cliActionExecutor) GetSessionState(_ context.Context) (*btest.BehaviorState, error) {
	return behaviorFromSessionInfo(e.sessionInfo), nil
}

func (e *cliActionExecutor) GetTelemetry(_ context.Context) (*btest.TelemetryState, error) {
	// TODO: session-mode telemetry — requires a persistent CDP connection across stateless CLI calls.
	// P1: telemetry requires persistent CDP connection, not available in stateless CLI mode.
	return &btest.TelemetryState{
		ConsoleErrors:   []btest.ConsoleEntry{},
		NetworkFailures: []btest.NetworkEntry{},
	}, nil
}

func (e *cliActionExecutor) CollectRegions(ctx context.Context) ([]btest.RegionSnap, error) {
	return btest.CollectRegionsViaEval(func(expr string, result interface{}) error {
		return e.impl.EvalJS(ctx, expr, result)
	})
}

// ============================================================
// § ActionExecutor 实现 — one-shot 模式
// ============================================================

// oneshotActionExecutor 包装 BrowserCore 的 ActionExecutor 实现（无 session 文件）。
type oneshotActionExecutor struct {
	impl      browser.BrowserCore
	ctx       context.Context
	telemetry *browser.TelemetryCollector
	planner   *btest.Planner // nil when LLM not configured
}

func (e *oneshotActionExecutor) Execute(ctx context.Context, action string) error {
	trimmed := strings.TrimSpace(action)

	// Direct URL navigation — deterministic fast path.
	if rest, ok := strings.CutPrefix(strings.ToLower(trimmed), "navigate "); ok {
		target := strings.TrimSpace(trimmed[len(trimmed)-len(rest):]) // preserve original case
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "/") {
			if _, err := e.impl.Navigate(ctx, target); err != nil {
				return fmt.Errorf("navigate %s: %w", target, err)
			}
			return nil
		}
	}
	switch strings.ToLower(trimmed) {
	case "wait", "noop", "none":
		return nil
	}

	// NL goal — route through LLM planner when configured. In deterministic /
	// llm-off mode the planner is nil; reject explicitly rather than silently
	// feeding a natural-language goal to the structural action engine.
	if isNLGoal(trimmed) {
		if e.planner == nil {
			return fmt.Errorf("deterministic/llm-off: NL goal %q rejected, use structural action", trimmed)
		}
		return e.executeNLGoal(ctx, trimmed)
	}

	_, err := e.impl.Act(ctx, action, false)
	return err
}

// executeNLGoal decomposes a natural-language goal into browser steps via the Planner.
func (e *oneshotActionExecutor) executeNLGoal(ctx context.Context, goal string) error {
	snap, _ := e.impl.Snap(ctx)
	var structural *btest.StructuralState
	if snap != nil {
		structural = doStructuralFromSnap(snap)
	}

	plan, err := e.planner.Plan(ctx, goal, structural)
	if err != nil {
		return fmt.Errorf("nl-plan %q: %w", goal, err)
	}

	for _, step := range plan.Steps {
		switch strings.ToLower(strings.TrimSpace(step.Action)) {
		case "wait", "noop", "none":
			// The real synchronization is represented by step.Wait.
		default:
			if _, err := e.impl.Act(ctx, step.Action, false); err != nil {
				return fmt.Errorf("nl-exec %q → step %q: %w", goal, step.Description, err)
			}
		}
		if step.Wait != "" {
			if err := e.Wait(ctx, step.Wait, 10000); err != nil {
				return fmt.Errorf("nl-exec %q → wait %q: %w", goal, step.Wait, err)
			}
		}
	}
	return nil
}

func (e *oneshotActionExecutor) Wait(ctx context.Context, condition string, timeoutMs int) error {
	if timeoutMs <= 0 {
		timeoutMs = 15000
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return runWaitCondition(waitCtx, e.impl, condition)
}

// ResizeViewport implements btest.MutationExecutor so the journey runner uses
// a clean viewport resize path instead of falling back to the JS-based path
// (which fails because ParseAction lowercases the entire action string,
// corrupting "javascript:window.resizeTo(…)" → unknown operation). [FIX-J3b]
func (e *oneshotActionExecutor) ResizeViewport(ctx context.Context, width, height int) error {
	// Prefer SetLiveViewport when the impl supports it (concrete browserCoreImpl does).
	if lvs, ok := e.impl.(browser.LiveViewportSyncer); ok {
		return lvs.SetLiveViewport(width, height, 1.0, false)
	}
	// Fallback: use EvalJS with the JS snippet case preserved.
	return e.impl.EvalJS(ctx, fmt.Sprintf("window.resizeTo(%d,%d)", width, height), nil)
}

func (e *oneshotActionExecutor) Snapshot(ctx context.Context) (*browser.Snapshot, error) {
	return e.impl.Snap(ctx)
}

func (e *oneshotActionExecutor) Screenshot(ctx context.Context) ([]byte, error) {
	return e.impl.Screenshot(ctx, false)
}

func (e *oneshotActionExecutor) GetSessionState(ctx context.Context) (*btest.BehaviorState, error) {
	// FIX-J2: derive behavior from live snapshot.
	snap, _ := e.impl.Snap(ctx)
	url, title := "", ""
	if snap != nil {
		url = snap.URL
		title = snap.PageTitle
	}
	tab := btest.TabState{
		ID:     "oneshot-0",
		Index:  0,
		URL:    url,
		Title:  title,
		Active: true,
	}
	return &btest.BehaviorState{
		URL:         url,
		Title:       title,
		Tabs:        []btest.TabState{tab},
		ActiveTabID: tab.ID,
		TabCount:    1,
		LoadState:   "idle",
	}, nil
}

func (e *oneshotActionExecutor) CollectRegions(ctx context.Context) ([]btest.RegionSnap, error) {
	return btest.CollectRegionsViaEval(func(expr string, result interface{}) error {
		return e.impl.EvalJS(ctx, expr, result)
	})
}

func (e *oneshotActionExecutor) GetTelemetry(_ context.Context) (*btest.TelemetryState, error) {
	// FIX-J1: return real telemetry from the CDP collector when available.
	return convertTelemetry(
		func() []browser.ConsoleError {
			if e.telemetry != nil {
				return e.telemetry.GetConsoleErrors()
			}
			return nil
		}(),
		func() []browser.NetworkFailure {
			if e.telemetry != nil {
				return e.telemetry.GetNetworkFailures()
			}
			return nil
		}(),
	), nil
}

// convertTelemetry maps browser-layer telemetry to btest.TelemetryState.
// Always returns non-nil slices.
func convertTelemetry(ce []browser.ConsoleError, nf []browser.NetworkFailure) *btest.TelemetryState {
	consoleEntries := make([]btest.ConsoleEntry, 0, len(ce))
	for _, c := range ce {
		consoleEntries = append(consoleEntries, btest.ConsoleEntry{
			Level:  c.Level,
			Text:   c.Text,
			Source: c.Source,
			URL:    c.URL,
			Line:   c.Line,
		})
	}
	networkEntries := make([]btest.NetworkEntry, 0, len(nf))
	for _, n := range nf {
		networkEntries = append(networkEntries, btest.NetworkEntry{
			URL:        n.URL,
			Method:     n.Method,
			StatusCode: n.StatusCode,
			Error:      n.Error,
		})
	}
	return &btest.TelemetryState{
		ConsoleErrors:   consoleEntries,
		NetworkFailures: networkEntries,
	}
}

// ============================================================
// § 工具函数
// ============================================================

// loadObservationFile 从 JSON 文件加载 Observation。
func loadObservationFile(path string) (*btest.Observation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read observation %s: %w", path, err)
	}
	var obs btest.Observation
	if err := json.Unmarshal(data, &obs); err != nil {
		return nil, fmt.Errorf("parse observation %s: %w", path, err)
	}
	return &obs, nil
}

// writeJSONOrStdout 将 v 序列化为 JSON，输出到 outFile 或 stdout。
func writeJSONOrStdout(v any, outFile, cmdName string) {
	enc, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: marshal output: %v\n", cmdName, err)
		os.Exit(exitRunErr)
	}
	if outFile != "" {
		if err := os.MkdirAll(dirOf(outFile), 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser %s: mkdir for output: %v\n", cmdName, err)
			os.Exit(exitRunErr)
		}
		if err := os.WriteFile(outFile, enc, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser %s: write output: %v\n", cmdName, err)
			os.Exit(exitRunErr)
		}
		return
	}
	fmt.Println(string(enc))
}

// dirOf 返回路径的父目录（如 "a/b/c.json" → "a/b"，"file.json" → "."）。
func dirOf(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return "."
	}
	return path[:idx]
}

// joinURL joins a base URL and an entry path. If entry is empty, returns base.
// If entry is already absolute (has scheme), returns entry. Otherwise joins
// base + "/" + entry with single-slash normalization.
func joinURL(base, entry string) string {
	if entry == "" {
		return base
	}
	if strings.HasPrefix(entry, "http://") || strings.HasPrefix(entry, "https://") {
		return entry
	}
	if base == "" {
		return entry
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(entry, "/")
}

// ============================================================
// § plan 命令 — NL planning boundary, no browser actions
// ============================================================

type nlPlanOutput struct {
	SessionID string            `json:"session_id"`
	PageURL   string            `json:"page_url,omitempty"`
	Source    string            `json:"source"`
	Plan      *btest.PlanResult `json:"plan"`
}

type nlPlanBuildResult struct {
	Plan    *btest.PlanResult
	Source  string
	PageURL string
}

// runPlan 只生成计划，不执行浏览器动作。
// dw-browser plan --id <session-id> "Open browser sidebar"
func cliUsingContainsVisual(using []string) bool {
	for _, u := range using {
		if strings.EqualFold(strings.TrimSpace(u), "visual") {
			return true
		}
	}
	return false
}

func lookupSkillRecipe(pageURL, goal string) []btest.PlannedStep {
	if pageURL == "" {
		return nil
	}
	domain := extractDomain(pageURL)
	if domain == "" {
		return nil
	}
	import_filepath := skillsBaseDir()
	if import_filepath == "" {
		return nil
	}

	skillFile := import_filepath + "/" + domain + "/SKILL.md"
	data, err := os.ReadFile(skillFile)
	if err != nil {
		return nil
	}

	_, body := parseSkillFrontmatter(string(data))

	// Try to find an action section that matches the goal (case-insensitive substring)
	goalLower := strings.ToLower(goal)
	meta, _ := parseSkillFrontmatter(string(data))
	for _, action := range meta.Actions {
		if strings.Contains(strings.ToLower(action), goalLower) ||
			strings.Contains(goalLower, strings.ToLower(action)) {
			section, found := extractSection(body, action)
			if !found {
				continue
			}
			steps := recipeToPlannedSteps(action, section)
			if len(steps) > 0 {
				return steps
			}
		}
	}
	return nil
}

// recipeToPlannedSteps parses the ```dw-browser fenced block from a skill section
// and returns each non-empty line as a PlannedStep.
func recipeToPlannedSteps(actionName, section string) []btest.PlannedStep {
	var steps []btest.PlannedStep
	inBlock := false
	stepNum := 0
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```dw-browser") {
			inBlock = true
			continue
		}
		if inBlock && strings.HasPrefix(trimmed, "```") {
			break
		}
		if inBlock && trimmed != "" {
			stepNum++
			steps = append(steps, btest.PlannedStep{
				Description: fmt.Sprintf("%s step %d", actionName, stepNum),
				Action:      trimmed,
			})
		}
	}
	return steps
}

// doStructuralFromSnap converts a browser.Snapshot to btest.StructuralState (minimal, for do command).
func doStructuralFromSnap(snap *browser.Snapshot) *btest.StructuralState {
	return &btest.StructuralState{
		SnapshotType: snap.SnapshotType,
		RefsCount:    len(snap.Refs),
		Text:         snap.Text,
		LoadState:    snap.LoadState,
		ReadyState:   snap.ReadyState,
	}
}

// ============================================================
// § get (NL) 命令 — Getter facade over observe
// ============================================================

// runGetNL 执行 NL get 查询：observe 当前 session 状态，Getter 提取字段。
// dw-browser get --id <session-id> "active tab url"
