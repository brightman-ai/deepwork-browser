// testing.go — observe / diff / check / journey 命令实现
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
	btest "github.com/brightman-ai/deepwork-browser/browser/testing"
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

// ============================================================
// § observe 命令
// ============================================================

// visibleErrorScanJS scans the rendered page for error UI a human notices instantly but
// console/network telemetry misses (CHG-016 R3 — the "errs==0 but red banner on screen"
// silent-failure gap). Four signal classes, deduped, capped, fail-safe: (1) W3C semantic
// markers role=alert / aria-invalid / aria-errormessage; (2) error-styled leaf elements;
// (3) red-colored visible text (computed color); (4) error keywords WITH corroboration.
// Returns [{kind,text,selector}]. Signals are for the agent to INSPECT, not a hard verdict.
//
// 精度约束 [BUG-VISERR-NOISE] — 安全类产品实测 14 命中 9 个是误报(64% 噪声):
//
//	a) 业务图形文字: SVG 图册里的「可疑异常访问」「连接失败告警规则」是**数据**不是报错
//	   → 跳过 <svg>/<canvas> 子树（应用报错 chrome 是 HTML，不画在图里）。
//	b) danger 风格的**动作**按钮(🗑/删除规则/移入回收站)是操作入口不是错误
//	   → styled/color 类跳过 button/a/[role=button] 等可点元素。
//	c) 裸关键词(异常/无效/未找到)在安全产品的正常文案里恒常出现 → keyword 类要求佐证:
//	   必须处于报错语境(role=alert/status 或 error/danger/toast/banner 类容器)或本身是红字。
//
// 取舍(明说): 这会漏掉"纯黑、无 class、无容器"的报错文案。但 64% 噪声会让 agent 直接
// 无视整个字段 → 告警疲劳的召回损失更大。宁可少而可信。
const visibleErrorScanJS = `(() => {
  const out = [], seen = new Set();
  const vis = el => { try { const r = el.getBoundingClientRect(); return el.offsetParent !== null && r.width > 0 && r.height > 0; } catch (e) { return false; } };
  const safeClosest = (el, sel) => { try { return !!(el.closest && el.closest(sel)); } catch (e) { return false; } };
  // 业务图形内容（图册/拓扑图的文字是数据，不是应用报错）
  const inGraphic = el => safeClosest(el, 'svg,canvas');
  // 可点动作入口（danger 样式的删除按钮是"操作"，不是"错误"）
  const isAction = el => safeClosest(el, 'button,a,[role=button],[role=tab],[role=menuitem],[role=link]');
  // 报错语境容器
  const inErrCtx = el => safeClosest(el, '[role=alert],[role=status],[class*=error i],[class*=danger i],[class*=invalid i],[class*=failed i],[class*=toast i],[class*=banner i],[class*=notice i],[data-error]');
  const isRed = el => {
    try {
      const m = (getComputedStyle(el).color || '').match(/rgba?\((\d+),\s*(\d+),\s*(\d+)/);
      return !!m && +m[1] > 150 && +m[2] < 90 && +m[3] < 90;
    } catch (e) { return false; }
  };
  const push = (kind, text, sel) => {
    text = (text || '').replace(/\s+/g, ' ').trim().slice(0, 200);
    if (!text) return;
    const key = kind + '|' + text;
    if (seen.has(key)) return; seen.add(key);
    out.push({ kind: kind, text: text, selector: (sel || '').toString().slice(0, 80) });
  };
  // (1) 语义标记 — 作者显式声明,精度最高,不加限制
  try { document.querySelectorAll('[role=alert],[aria-invalid=true],[aria-errormessage]').forEach(el => {
    if (out.length < 25 && vis(el) && el.textContent.trim()) push('aria', el.textContent, el.getAttribute('role') || 'aria-invalid');
  }); } catch (e) {}
  // (2) 报错样式叶子 — 排除动作按钮
  try { document.querySelectorAll('[class*=error i],[class*=danger i],[class*=invalid i],[class*=failed i],[data-error]').forEach(el => {
    if (out.length < 25 && el.children.length === 0 && vis(el) && el.textContent.trim() && !isAction(el) && !inGraphic(el)) push('styled', el.textContent, el.className || el.tagName);
  }); } catch (e) {}
  // (3) 红字 — 排除图形内容与动作按钮
  try { document.querySelectorAll('body *').forEach(el => {
    if (out.length >= 25 || el.children.length !== 0 || !el.textContent.trim() || !vis(el)) return;
    if (inGraphic(el) || isAction(el)) return;
    if (isRed(el)) push('color', el.textContent, 'red:' + el.tagName);
  }); } catch (e) {}
  // (4) 关键词 — 必须有佐证(报错语境 或 红字), 且排除图形/动作
  const kw = /(invalid context|failed to|exception|timed? ?out|unavailable|请求失败|加载失败|无法连接|连接失败|出错了|报错|加载出错|保存失败|提交失败)/i;
  try { document.querySelectorAll('body *').forEach(el => {
    if (out.length >= 25 || el.children.length !== 0 || !vis(el)) return;
    if (inGraphic(el) || isAction(el)) return;
    const t = el.textContent.trim();
    if (!t || t.length >= 160 || !kw.test(t)) return;
    if (!inErrCtx(el) && !isRed(el)) return; // 无佐证 → 视为业务文案
    push('keyword', t, el.tagName.toLowerCase());
  }); } catch (e) {}
  return out.slice(0, 20);
})()`

// sessionRefsForObservation decides what ref authority an observe leaves behind.
//
// A normal observe mints the visible set it just showed. `--all` is a read-only
// census: it shows no @rN, so it must mint none — but it must not REVOKE the
// handles a previous observe granted either. Wiping them turned a debug census
// into a silent capability reset [BUG-CENSUS-REVOKES-REFS]. The one thing that
// does revoke them is a document boundary: refs from another page are dead.
func sessionRefsForObservation(info *browser.SessionInfo, snap *browser.Snapshot, censusOnly bool) []browser.SessionRef {
	if snap == nil {
		return nil
	}
	if censusOnly {
		if info == nil {
			return nil
		}
		if info.PageURL != "" && snap.URL != "" && snap.URL != info.PageURL {
			return nil
		}
		return info.Refs
	}
	return browser.SessionRefsFromSnapshot(snap, true)
}

func observeListing(snap *browser.Snapshot, censusOnly bool, topN, budget int, scenarios ...browser.Scenario) (map[string]interface{}, bool) {
	if censusOnly {
		census := snap.Census
		if census == nil {
			census = []browser.CensusEntry{}
		}
		return map[string]interface{}{
			"census":     census,
			"shown":      len(census),
			"total":      snap.DocumentInteractableCount,
			"all":        true,
			"actionable": false,
		}, false
	}
	elements, selectedTotal, truncated, omitted := briefElements(snap.Refs, topN, budget, scenarios...)
	total := selectedTotal
	if snap.SeeToClick {
		total = snap.DocumentInteractableCount
	}
	// 三个计数是三件事,必须同时出现才不会被读成互相矛盾:
	//   shown   本次真的打印了几个 (受 --top/--budget 约束)
	//   visible 当前截图可见集全量 = 已铸 @rN 数 = hit_audit 采样口径
	//   total   全文档可交互数; total - visible = offscreen
	listing := map[string]interface{}{
		"elements": elements,
		"shown":    len(elements),
		"visible":  len(snap.Refs),
		"total":    total,
	}
	if len(omitted) > 0 {
		listing["omitted_by_role"] = omitted
	}
	return listing, truncated
}

// formatRoleCounts 把 role→数量 渲染成稳定次序的可读串 (map 迭代序不可信)。
func formatRoleCounts(counts map[string]int) string {
	roles := make([]string, 0, len(counts))
	for role := range counts {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	parts := make([]string, 0, len(roles))
	for _, role := range roles {
		parts = append(parts, fmt.Sprintf("%s %d", role, counts[role]))
	}
	return strings.Join(parts, ", ")
}

const (
	hitAuditScopeVisible = "visible"
	hitAuditScopeAll     = "all"
)

// stripObserveHitAuditFlag pulls --hit-audit and its --scope out of the arg
// list. scope=all widens the audit to the elements observe dropped for
// occlusion — the ones the visible-set-only audit could never see.
func stripObserveHitAuditFlag(args []string) ([]string, bool, string, error) {
	clean := make([]string, 0, len(args))
	want := false
	scope := hitAuditScopeVisible
	setScope := func(value string) error {
		switch value {
		case hitAuditScopeVisible, "visible_set":
			scope = hitAuditScopeVisible
		case hitAuditScopeAll:
			scope = hitAuditScopeAll
		default:
			return fmt.Errorf("--scope 只接受 visible|all, 收到 %q", value)
		}
		return nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--hit-audit":
			want = true
		case arg == "--scope" && i+1 < len(args):
			if err := setScope(args[i+1]); err != nil {
				return nil, false, "", err
			}
			i++
		case strings.HasPrefix(arg, "--scope="):
			if err := setScope(arg[len("--scope="):]); err != nil {
				return nil, false, "", err
			}
		default:
			clean = append(clean, arg)
		}
	}
	return clean, want, scope, nil
}

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
func runObserveResult(args []string) (CommandResult, error) {
	args, wantHitAudit, hitAuditScope, scopeErr := stripObserveHitAuditFlag(args)
	if scopeErr != nil {
		return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("dw-browser observe: %v\n", scopeErr))
	}
	var outFile, evidenceDir, contentFormat string
	wantHealth, wantTree, wantAnnotate, wantAll := false, false, false, false
	topN, budget := defaultBriefTopN, defaultBriefBudget
	topExplicit, budgetExplicit := false, false
	clean := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--screenshot" && i+1 < len(args):
			outFile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--screenshot="):
			outFile = arg[len("--screenshot="):]
		case arg == "--content" && i+1 < len(args):
			contentFormat = args[i+1]
			i++
		case strings.HasPrefix(arg, "--content="):
			contentFormat = strings.TrimPrefix(arg, "--content=")
		case arg == "--health":
			wantHealth = true
		case arg == "--tree":
			wantTree = true
		case arg == "--annotate":
			wantAnnotate = true
		case arg == "--all":
			wantAll = true
		case arg == "--top" && i+1 < len(args):
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				topN = n
				topExplicit = true
			}
			i++
		case strings.HasPrefix(arg, "--top="):
			if n, err := strconv.Atoi(arg[len("--top="):]); err == nil {
				topN = n
				topExplicit = true
			}
		case arg == "--budget" && i+1 < len(args):
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				budget = n
				budgetExplicit = true
			}
			i++
		case strings.HasPrefix(arg, "--budget="):
			if n, err := strconv.Atoi(arg[len("--budget="):]); err == nil {
				budget = n
				budgetExplicit = true
			}
		default:
			clean = append(clean, arg)
		}
	}

	if contentFormat != "" && contentFormat != "text" && contentFormat != "list" && contentFormat != "table" {
		return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("unsupported content format %q", contentFormat))
	}
	if err := validateReadSessionArgsE(clean, "observe"); err != nil {
		return CommandResult{}, commandError("validation", exitRunErr, err)
	}
	_, flags, parseErr := parseCommonFlagsE(clean, "observe")
	if parseErr != nil {
		return CommandResult{}, commandError("validation", exitRunErr, parseErr)
	}
	if flags.sessionID == "" {
		return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("%s", fmt.Sprint("dw-browser observe: requires --id <session-id>")))
	}

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("dw-browser observe: %v\n", err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	impl, connectErr := connectSessionE(ctx, sessionInfo, "observe", flags)
	if connectErr != nil {
		return CommandResult{}, commandError("attachment", exitRunErr, connectErr)
	}
	defer closeSessionCore(impl)
	if capable, ok := impl.(browser.ScenarioInteractionCapable); ok {
		capable.SetObserveAll(wantAll)
	}
	impl.RestoreRefsFromSession(sessionInfo.Refs)

	// — 感知 a11y (始终) —
	// census 是只读普查: 不铸 ref、不撤 ref, 也不推进 epoch(推进会连带作废
	// human-focus 证明) [BUG-CENSUS-REVOKES-REFS]。
	if !wantAll {
		sessionInfo.SnapEpoch++
	}
	snap, err := impl.SnapWithSessionMode(ctx, sessionInfo.SnapEpoch)
	if err != nil {
		return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("dw-browser observe: snap failed: %v\n", err))
	}
	if snap == nil {
		return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("%s", fmt.Sprint("dw-browser observe: empty snapshot")))
	}

	observationID := snap.ObservationID
	if observationID == "" {
		observationID = btest.NewObservationID()
	}
	if evidenceDir == "" {
		evidenceDir = sessionInfo.EvidenceRoot
	}
	if evidenceDir == "" {
		evidenceDir = filepath.Join("evidence", "observations", flags.sessionID)
	}
	evidenceDir, err = filepath.Abs(evidenceDir)
	if err != nil {
		return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("%s", fmt.Sprint(err)))
	}
	artifactPath := filepath.Join(evidenceDir, observationID+".json")
	if !wantAll {
		if identity, ok := impl.(interface {
			SessionDocumentIdentity(context.Context) (int64, error)
		}); ok && snap.DocumentID == 0 {
			documentID, err := identity.SessionDocumentIdentity(ctx)
			if err != nil {
				return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("dw-browser observe: document identity: %v\n", err))
			}
			snap.DocumentID = documentID
		}
		if err := browser.BindSessionObservation(sessionInfo, snap, observationID, artifactPath); err != nil {
			return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("%s", fmt.Sprint(err)))
		}
	}

	// occluded 与 offscreen 是两种不同的"看不到句柄": offscreen 滚动可达,
	// occluded 是在屏却被别的元素占住中心 —— 后者正是 UX 缺陷候选,
	// 过去被静默丢弃, 现在始终计数 [BUG-OCCLUDED-SILENTLY-DROPPED]。
	occludedCount := 0
	occlusionCapable, hasOcclusionCensus := impl.(browser.OcclusionCensusCapable)
	if hasOcclusionCensus {
		occludedCount = occlusionCapable.OccludedInteractableCount()
	}

	var hitAudit []browser.HitAuditFinding
	if wantHitAudit {
		capable, ok := impl.(browser.HitAuditCapable)
		if !ok {
			return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("%s", fmt.Sprint("dw-browser observe: --hit-audit is unavailable for this browser engine")))
		}
		hitAudit, err = capable.AuditHitCoverage(ctx, snap.Refs)
		if err != nil {
			return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("dw-browser observe: hit audit failed: %v\n", err))
		}
		if hitAuditScope == hitAuditScopeAll {
			if !hasOcclusionCensus {
				return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("%s", fmt.Sprint("dw-browser observe: --hit-audit --scope all is unavailable for this browser engine")))
			}
			occludedFindings, auditErr := occlusionCapable.AuditOccludedInteractables(ctx)
			if auditErr != nil {
				return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("dw-browser observe: occluded hit audit failed: %v\n", auditErr))
			}
			hitAudit = append(hitAudit, occludedFindings...)
		}
	}

	// 显式 --top N 未配 --budget 时，放大字节预算以真的兑现 N 个元素。
	// budget 的职责是保护"默认"上下文开销；调用方明确要 N 个却静默只给 ~37 个 = 骗人。
	if topExplicit && !budgetExplicit {
		if want := topN * perElementBudgetEst; want > budget {
			budget = want
		}
	}

	// — 瘦默认输出可见 elements；--all 改为完全无 ref 的 census —
	listing, truncated := observeListing(snap, wantAll, topN, budget, browser.Scenario(sessionInfo.Scenario))
	output := map[string]interface{}{
		"url":        snap.URL,
		"title":      snap.PageTitle,
		"user_state": buildUserState(snap),
	}
	for key, value := range listing {
		output[key] = value
	}
	if wantHitAudit {
		if hitAudit == nil {
			hitAudit = []browser.HitAuditFinding{}
		}
		output["hit_audit"] = hitAudit
		// 采样口径 = 可见集全量(与 listing.visible 同一个数),不是 shown。
		// 二者不同时曾被读成"审计比可见集还多",故把口径写进输出本身。
		if hitAuditScope == hitAuditScopeAll {
			output["hit_audit_sampled"] = len(snap.Refs) + occludedCount
			output["hit_audit_scope"] = "visible_set+occluded"
			output["hit_audit_note"] = "scope=all: 可见集全量 + observe 时因中心被遮挡而落选的可交互元素;" +
				" 后者 scope=\"occluded\"、无 @ref(点不到的东西不发句柄), occluded_by 是遮挡者 —— " +
				"这批正是\"用户看得见却点不到\"的 UX 缺陷候选"
		} else {
			output["hit_audit_sampled"] = len(snap.Refs)
			output["hit_audit_scope"] = "visible_set"
		}
	}
	if snap.SeeToClick {
		output["occluded"] = occludedCount
		if occludedCount > 0 {
			output["occluded_hint"] = fmt.Sprintf(
				"%d 个可交互元素在屏内却被别的元素占住中心, 因此不在可见集(既非屏外也非未渲染); "+
					"`observe --hit-audit --scope all` 可列出它们与各自的遮挡者", occludedCount)
		}
		output["offscreen"] = snap.OffscreenInteractableCount
		output["viewport"] = map[string]interface{}{
			"width":             snap.Viewport.Width,
			"height":            snap.Viewport.Height,
			"dpr":               snap.Viewport.DevicePixelRatio,
			"scroll_x":          snap.Viewport.ScrollX,
			"scroll_y":          snap.Viewport.ScrollY,
			"coordinate_space":  "css-px",
			"screenshot_pixels": "CSS px × dpr",
		}
	}
	// [BUG-MODAL-FIRST] 有活跃模态 → 明说"只有这层能点"，并说明背后多少元素被挡
	if activeN, blockedN, hasModal := modalNotice(snap.Refs); hasModal {
		output["modal_active"] = true
		output["modal_elements"] = activeN
		output["blocked_elements"] = blockedN
		output["modal_hint"] = fmt.Sprintf(
			"页面被活跃模态层拦截: 只有前 %d 个元素(未标 blocked)可交互; 其余 %d 个标 blocked=true 的元素在浮层背后, 点击会静默失败 — 先处理/关闭浮层",
			activeN, blockedN)
	}
	if truncated {
		output["truncated"] = true
		hint := fmt.Sprintf("可见集 %d 个, 本次只打印 %d 个(--top/--budget 所限); 未打印的元素句柄仍然存在, --top N / --budget BYTES 放宽即可取到",
			len(snap.Refs), listing["shown"])
		if omitted, ok := listing["omitted_by_role"].(map[string]int); ok && len(omitted) > 0 {
			hint += "; 省略明细见 omitted_by_role: " + formatRoleCounts(omitted)
		}
		output["hint"] = hint
	}
	if snap.SeeToClick && snap.OffscreenInteractableCount > 0 {
		output["scroll_hint"] = "还有元素不在当前截图可见集；滚动后重新 observe"
	}
	injectEvidenceID(output, sessionInfo)
	injectSnapshotState(output, snap)

	// — browser chrome 仿真机读块（REQ-BC-04）：像素给 Witness 感知，此块给 AI 校准。
	// simulated:false 也如实报（裸 viewport/桌面会话无 chrome 数据 = 诚实降级，不猜）。
	if bcc, ok := impl.(browser.BrowserChromeCapable); ok {
		if spec, baseH, scale, on := bcc.BrowserChromeState(); on {
			dpr := 1.0
			if p := browser.BuiltinPresets[browser.NormalizePresetID(sessionInfo.PresetID)]; p != nil {
				dpr = p.DeviceScaleFactor
			}
			kb := bcc.KeyboardVisible()
			hint := fmt.Sprintf(
				"视口底部 y>=%d 为 Safari 底栏遮挡带(截图中已画出): 带内元素用户不可见/不可点, act 点击会被拒; 页面 100vh 布局的底部 UI 会沉入此带(与真机同构). 页面感知的 visualViewport/innerHeight/100svh=%d(小视口, 自适应页已真实 reflow); 100dvh/fixed-bottom 是引擎残留假阳通道(act 豁免放行, audit pass+标记)", baseH, baseH)
			if kb {
				hint = fmt.Sprintf(
					"软键盘弹起: 视口底部 y>=%d 为键盘遮挡区(截图中已画出), 带内元素不可见/不可点且无豁免; act \"keyboard hide\" 收起. 页面感知的 visualViewport.height 已收窄(自适应页应上浮 composer; 没上浮=键盘布局 bug)", spec.KeyboardTopY())
			}
			output["browser_chrome"] = map[string]interface{}{
				"simulated": true,
				"style":     spec.Style,
				// zones 坐标系 = CSS px 视口坐标（与 act/元素 rect 同系）；
				// 截图为物理像素，索引像素行须乘 device_pixel_ratio。
				"coordinate_space":   "css-px",
				"device_pixel_ratio": dpr,
				"page_scale":         scale,
				"keyboard": map[string]interface{}{
					"visible":   kb,
					"inset_css": spec.KeyboardInsetH(),
					"top_y":     spec.KeyboardTopY(),
					"how_to":    "act \"keyboard show|hide\"; click/tap/fill 输入框后自动弹起(真机语义)",
				},
				// 视口单位事实（REQ-BC-11）：svh 经 CDP override=小视口; dvh/vh 锁死
				// layout viewport=大视口(Chrome 引擎约束, 真机 bars-expanded 下 dvh=小视口
				// → dvh 页在截图里可能显示为被遮而真机正常 = 残留假阳, 判定层已豁免)。
				"viewport_units":   map[string]interface{}{"svh": spec.SmallViewportH(), "lvh": spec.LargeViewportH(), "vh": spec.LargeViewportH(), "dvh_residual": spec.LargeViewportH()},
				"occlusion_zones":  spec.OcclusionZones(sessionInfo.ViewportW, kb),
				"page_protections": bcc.ProbePageProtections(ctx),
				"hint":             hint,
			}
		} else {
			output["browser_chrome"] = map[string]interface{}{"simulated": false}
		}
	}

	if hint := formatSkillHint(snap.URL); hint != "" {
		output["skill_hint"] = hint
	}
	if ph := formatPersonaHint(sessionInfo.Scenario, sessionInfo.PersonaID); ph != "" {
		output["persona_hint"] = ph
	}

	// — 加法: --tree (全 a11y 文本) —
	if wantTree && !wantAll {
		output["tree"] = snap.Text
	}

	if outFile == "" {
		outFile = filepath.Join(evidenceDir, observationID+".png")
	}
	outFile, err = filepath.Abs(outFile)
	if err != nil {
		return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("%s", fmt.Sprint(err)))
	}
	shot, shotErr := impl.Screenshot(ctx, wantAnnotate)
	obs := btest.BuildObservation(flags.sessionID, snap, shot, filepath.Join(evidenceDir, observationID+".png"), behaviorFromSessionInfo(sessionInfo), nil, shotErr)
	if obs.Visual != nil && outFile != obs.Visual.ScreenshotPath {
		if err := os.MkdirAll(filepath.Dir(outFile), 0755); err != nil {
			shotErr = err
		} else if canonicalPNG, err := os.ReadFile(obs.Visual.ScreenshotPath); err != nil {
			shotErr = err
		} else if err := os.WriteFile(outFile, canonicalPNG, 0644); err != nil {
			shotErr = err
		}
	}
	obs.ID = observationID
	obs.OperationID = observationID
	obs.Scenario = string(sessionInfo.Scenario)
	obs.BindActionHints(snap, browser.Scenario(sessionInfo.Scenario))
	obs.Deterministic = sessionInfo.Policy.Deterministic
	if obs.RuntimeRunID == "" {
		obs.RuntimeRunID = sessionInfo.BrowserRunID
	}
	if obs.TargetID == "" {
		obs.TargetID = sessionInfo.TargetID
	}
	obs.Page.ViewportW = int(snap.Viewport.Width)
	obs.Page.ViewportH = int(snap.Viewport.Height)
	var contentErr error
	obs.Channels["content"] = btest.ChannelState{Status: "not-requested"}
	if contentFormat != "" {
		obs.Content, contentErr = btest.CollectContent(ctx, impl, contentFormat)
		if contentErr != nil {
			obs.Channels["content"] = btest.ChannelState{Status: "unavailable", Reason: contentErr.Error()}
		} else {
			obs.Channels["content"] = btest.ChannelState{Status: "captured"}
		}
	}
	obs.Page.Engine = string(sessionInfo.Engine)
	if obs.Page.Engine == "" {
		obs.Page.Engine = "chrome"
	}
	if wantHealth {
		obs.Channels["console"] = btest.ChannelState{Status: "unavailable", Reason: "stateless session has no persistent console collector"}
		obs.Channels["network"] = btest.ChannelState{Status: "unavailable", Reason: "stateless session has no persistent network collector"}
		var vis []btest.VisibleErrorEntry
		if scanErr := impl.EvalJS(ctx, visibleErrorScanJS, &vis); scanErr == nil {
			obs.Telemetry = &btest.TelemetryState{VisibleErrors: vis}
			obs.Channels["visible_errors"] = btest.ChannelState{Status: "captured"}
		} else {
			obs.Channels["visible_errors"] = btest.ChannelState{Status: "unavailable", Reason: scanErr.Error()}
		}
	}
	if err := btest.WriteObservation(artifactPath, obs); err != nil {
		return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("dw-browser observe: artifact: %v\n", err))
	}

	if obs.Visual != nil && obs.Channels["structural"].Status == "captured" {
		if err := browser.SaveObservedSession(sessionInfo, snap, sessionRefsForObservation(sessionInfo, snap, wantAll)); err != nil {
			return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("dw-browser observe: persist observation authority: %v\n", err))
		}
	} else {
		output["actionable"] = false
	}
	output["schema"] = "dw.observe.ref.v2"
	output["observation_id"] = obs.ID
	output["observation_path"] = artifactPath
	output["channels"] = obs.Channels
	output["observation_state"] = obs.State
	if obs.Content != nil {
		output["content"] = obs.Content
	}
	if obs.Visual != nil {
		output["screenshot"] = obs.Visual.ScreenshotPath
	}
	if shotErr != nil || obs.Visual == nil {
		if shotErr == nil {
			shotErr = fmt.Errorf("%s", obs.Channels["visual"].Reason)
		}
		return commandResult(output, exitRunErr), commandError("screenshot", exitRunErr, fmt.Errorf("screenshot unavailable: %w (partial artifact %s)", shotErr, artifactPath))
	}
	if obs.Channels["structural"].Status != "captured" {
		return commandResult(output, exitRunErr), commandError("capture", exitRunErr, fmt.Errorf("structural observation pending: %s", obs.Channels["structural"].Reason))
	}
	if contentErr != nil {
		return commandResult(output, exitRunErr), commandError("content", exitRunErr, contentErr)
	}
	return commandSuccess(output), nil
}

// behaviorFromSessionInfo 从 SessionInfo 构建 BehaviorState（无需连接浏览器）。
func behaviorFromSessionInfo(info *browser.SessionInfo) *btest.BehaviorState {
	// SessionInfo 持有上次 snap 时的 PageURL，tabs 信息存在 AuthorityState 解析的地方。
	// stateless CLI 中最可靠的信息来源是 info.PageURL + info.Refs 计数。
	// Tabs 需要通过 FetchChromeTargets 从 CDP 获取实时状态。
	tabs := fetchTabsFromSession(info)
	if tabs == nil {
		return nil
	}

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
		return nil
	}

	tabs := make([]btest.TabState, 0, len(targets))
	for _, t := range targets {
		if !browser.IsDevToolsPageTarget(t) {
			continue
		}
		targetURL, _ := t["url"].(string)
		if !browser.IsUserPageTargetURL(targetURL) {
			continue
		}
		targetID := browser.ExtractDevToolsTargetID(t)
		url, _ := t["url"].(string)
		title, _ := t["title"].(string)
		active := targetID == info.TargetID
		tabs = append(tabs, btest.TabState{
			ID:     targetID,
			Index:  len(tabs),
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
func runDiffResult(args []string) (CommandResult, error) {
	if len(args) != 2 {
		return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("diff requires exactly two observation paths"))
	}
	before, err := btest.LoadObservation(args[0])
	if err != nil {
		return CommandResult{}, commandError("observation-load", exitRunErr, err)
	}
	after, err := btest.LoadObservation(args[1])
	if err != nil {
		return CommandResult{}, commandError("observation-load", exitRunErr, err)
	}
	return commandSuccess(btest.ComputeDiff(before, after)), nil
}

// ============================================================
// § check 命令
// ============================================================

// runCheck 执行断言检查。
// dw-browser check --id <session-id> --assert "expr" [--output result.json]
// dw-browser check --id <session-id> --file baselines/app.yaml [--output results.json]
// dw-browser check --observation obs.json --assert "expr" [--output result.json]
func runCheckResult(args []string) (CommandResult, error) {
	autoLoadLLMEnv()
	_, args = parseLLMFlags(args)
	var id, expression, file, observation, using string
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		switch name {
		case "--id", "--assert", "--file", "--observation", "--using":
		default:
			return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("unsupported argument %q", args[i]))
		}
		if !inline {
			if i+1 >= len(args) {
				return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("%s requires value", name))
			}
			i++
			value = args[i]
		}
		switch name {
		case "--id":
			id = value
		case "--assert":
			expression = value
		case "--file":
			file = value
		case "--observation":
			observation = value
		case "--using":
			using = value
		}
	}
	if (id == "") == (observation == "") || (expression == "") == (file == "") {
		return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("check requires exactly one id/observation and one assert/file"))
	}
	var usingSlice []string
	if using != "" {
		for _, part := range strings.Split(using, ",") {
			usingSlice = append(usingSlice, strings.TrimSpace(part))
		}
	}
	var specs []btest.AssertionSpec
	var err error
	if file != "" {
		specs, err = loadAssertionSpecs(file)
		if err != nil {
			return CommandResult{}, commandError("validation", exitRunErr, err)
		}
	} else if err := btest.ValidateAssertion(expression); err != nil && !cliUsingContainsVisual(usingSlice) {
		return CommandResult{}, commandError("validation", exitRunErr, err)
	}
	var obs *btest.Observation
	if observation != "" {
		obs, err = btest.LoadObservation(observation)
	} else {
		observed, observeErr := runObserveResult([]string{"--id", id})
		if observeErr != nil {
			return observed, observeErr
		}
		projection, ok := observed.Payload.(map[string]interface{})
		if !ok {
			return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("observation projection unavailable"))
		}
		path, ok := projection["observation_path"].(string)
		if !ok {
			return CommandResult{}, commandError("observation", exitRunErr, fmt.Errorf("canonical path unavailable"))
		}
		obs, err = btest.LoadObservation(path)
	}
	if err != nil {
		return CommandResult{}, commandError("observation-load", exitRunErr, err)
	}
	wantsVision := cliUsingContainsVisual(usingSlice)
	for _, check := range specs {
		wantsVision = wantsVision || cliUsingContainsVisual(check.Using)
	}
	if obs.Deterministic && wantsVision {
		return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("deterministic observation forbids internal LLM"))
	}
	engine := btest.AssertionEngine{}
	if wantsVision {
		engine.Vision = btest.NewVisionOracle()
	}
	if file != "" {
		results := engine.EvaluateAll(obs, specs)
		var requiredResults []btest.AssertionResult
		for i, result := range results {
			if specs[i].Required == nil || *specs[i].Required {
				requiredResults = append(requiredResults, result)
			}
		}
		return commandResult(results, btest.CheckExitCode(requiredResults)), nil
	}
	result := engine.EvaluateWithUsing(obs, expression, usingSlice)
	return commandResult(result, btest.CheckExitCode([]btest.AssertionResult{*result})), nil
}

// loadAssertionSpecs 从 YAML 文件加载断言规格列表。
// 支持两种格式：
//  1. 顶层 []AssertionSpec 数组
//  2. 包含 invariants: [...] 字段的 BaselineRef 结构
func loadAssertionSpecs(path string) ([]btest.AssertionSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var checks []btest.AssertionSpec
	if err := btest.DecodeYAML(data, &checks); err != nil {
		var baseline btest.BaselineRef
		if err := btest.DecodeYAML(data, &baseline); err != nil {
			return nil, err
		}
		checks = baseline.Invariants
	}
	if len(checks) == 0 {
		return nil, fmt.Errorf("assertion file requires nonempty checks")
	}
	for _, check := range checks {
		if err := btest.ValidateAssertion(check.Assert); err != nil {
			return nil, err
		}
	}
	return checks, nil
}

// ============================================================
// § journey 命令
// ============================================================

// runJourney 执行 BDD 旅程测试。
// dw-browser journey --file tests/bdd/bs15-smoke.yaml --evidence tests/evidence/run-001 [--fail-fast]
func runJourneyResult(args []string) (output CommandResult, resultErr error) {
	var file, evidenceRoot, baseURL string
	failFast := false
	clean := []string{}
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		switch name {
		case "--file", "--evidence", "--base-url":
			if !inline {
				if i+1 >= len(args) {
					return output, commandError("validation", exitRunErr, fmt.Errorf("%s requires value", name))
				}
				i++
				value = args[i]
			}
			switch name {
			case "--file":
				file = value
			case "--evidence":
				evidenceRoot = value
			case "--base-url":
				baseURL = value
			}
		case "--fail-fast":
			if inline {
				valueBool, err := strconv.ParseBool(value)
				if err != nil {
					return output, commandError("validation", exitRunErr, err)
				}
				failFast = valueBool
			} else {
				failFast = true
			}
		default:
			clean = append(clean, args[i])
		}
	}
	if file == "" {
		return output, commandError("validation", exitRunErr, fmt.Errorf("journey requires file"))
	}
	spec, err := btest.LoadSpec(file)
	if err != nil {
		return output, commandError("validation", exitRunErr, err)
	}
	if baseURL != "" {
		spec.Environment.BaseURL = baseURL
	}
	positional, flags, err := parseCommonFlagsE(clean, "journey")
	if err != nil || len(positional) != 0 {
		return output, commandError("validation", exitRunErr, fmt.Errorf("journey arguments: %v %v", positional, err))
	}
	if evidenceRoot == "" {
		evidenceRoot = filepath.Join("evidence", "journeys", spec.ID)
	}
	if flags.sessionID != "" && spec.Environment.CleanSession {
		return output, commandError("validation", exitRunErr, fmt.Errorf("clean_session requires owned fresh creation, not attached session"))
	}
	if spec.Environment.CleanSession && flags.profileID != "" {
		return output, commandError("validation", exitRunErr, fmt.Errorf("clean_session cannot reuse persistent profile"))
	}
	ownedID := ""
	if flags.sessionID == "" {
		entry := joinURL(spec.Environment.BaseURL, spec.Environment.EntryURL)
		if entry == "" {
			return output, commandError("validation", exitRunErr, fmt.Errorf("creation requires environment base_url/entry_url"))
		}
		if _, policy, err := resolveScenarioE(&flags, "journey"); err != nil {
			return output, commandError("validation", exitRunErr, err)
		} else if policy.Deterministic && specHasVisualUsing(spec) {
			return output, commandError("validation", exitRunErr, fmt.Errorf("baseline forbids internal LLM"))
		}
		ownedID = "journey-" + btest.NewObservationID()
		creation := append([]string{entry, "--id", ownedID, "--evidence", evidenceRoot}, clean...)
		if spec.Environment.Mode != "" && !flags.modeExplicit {
			creation = append(creation, "--mode", spec.Environment.Mode)
		}
		if spec.Environment.Client != "" && flags.engine == "" {
			creation = append(creation, "--engine", spec.Environment.Client)
		}
		if spec.Environment.Viewport != "" && !flags.hasViewport {
			creation = append(creation, "--viewport", spec.Environment.Viewport)
		}
		created, err := runOpenResult(creation)
		if err != nil || created.Code != exitOK {
			return created, err
		}
		flags.sessionID = ownedID
		defer func() {
			closed, err := runCloseResult([]string{"--id", ownedID})
			if err != nil || closed.Code != exitOK {
				output.Code = exitRunErr
				resultErr = &CommandError{Phase: "cleanup", DispatchState: "unknown", Recovery: "inspect only owned session " + ownedID, Code: exitRunErr, Cause: fmt.Errorf("owned journey close failed: %v", err)}
			}
		}()
	}
	info, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		return output, commandError("session-load", exitRunErr, err)
	}
	if ownedID == "" {
		if spec.Environment.Mode != "" && spec.Environment.Mode != string(info.Mode) {
			return output, commandError("environment", exitRunErr, fmt.Errorf("attached mode does not match declared environment"))
		}
		if spec.Environment.Client != "" && spec.Environment.Client != string(browser.NormalizeEngine(info.Engine)) {
			return output, commandError("environment", exitRunErr, fmt.Errorf("attached engine does not match declared environment"))
		}
		if spec.Environment.Viewport != "" && spec.Environment.Viewport != fmt.Sprintf("%dx%d", info.ViewportW, info.ViewportH) {
			return output, commandError("environment", exitRunErr, fmt.Errorf("attached viewport does not match declared environment"))
		}
	}
	wantsVision := specHasVisualUsing(spec)
	if info.Policy.Deterministic && wantsVision {
		return output, commandError("validation", exitRunErr, fmt.Errorf("baseline forbids internal LLM"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Minute)
	defer cancel()
	impl, err := connectSessionE(ctx, info, "journey", commonFlags{sessionID: info.SessionID})
	if err != nil {
		return output, commandError("attachment", exitRunErr, err)
	}
	defer closeSessionCore(impl)
	executor := &cliActionExecutor{sessionID: info.SessionID, sessionInfo: info, impl: impl, ctx: ctx}
	runner, err := btest.NewRunner(executor, evidenceRoot)
	if err != nil {
		return output, commandError("evidence", exitRunErr, err)
	}
	runner.SetFailFast(failFast)
	if wantsVision {
		autoLoadLLMEnv()
		runner.SetVision(btest.NewVisionOracle())
	}
	result, err := runner.Run(ctx, spec)
	if err != nil {
		return commandResult(result, exitRunErr), &CommandError{Phase: "journey", DispatchState: "unknown", Recovery: "inspect canonical evidence; observe before further input; do not replay mutating actions", Code: exitRunErr, Cause: err}
	}
	code := exitRunErr
	switch result.Status {
	case btest.StatusPass, btest.StatusDelivered:
		code = exitOK
	case btest.StatusFail:
		code = exitFail
	}
	return commandResult(result, code), nil
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
}

func (e *cliActionExecutor) Execute(ctx context.Context, request browser.Action) (browser.Outcome, error) {
	return e.ExecuteWithMode(ctx, request, browser.InteractionModeVisual)
}
func (e *cliActionExecutor) ExecuteWithMode(ctx context.Context, request browser.Action, mode browser.InteractionMode) (browser.Outcome, error) {
	capability, ok := e.impl.(browser.TypedActionCapable)
	if !ok {
		return browser.Outcome{Phase: "validation", DispatchState: "not-dispatched"}, fmt.Errorf("typed action kernel unavailable")
	}
	return executePersistedSessionAction(e.sessionInfo, request, func(operationID string) {
		if setter, ok := e.impl.(interface{ SetSessionOperationID(string) }); ok {
			setter.SetSessionOperationID(operationID)
		}
	}, func() (*browser.Snapshot, browser.Outcome, browser.ActionFidelityReport, error) {
		snap, outcome, err := capability.ExecuteAction(ctx, request, false, true, mode)
		return snap, outcome, actionFidelityReport(e.impl), err
	})
}
func (e *cliActionExecutor) Snapshot(ctx context.Context) (*browser.Snapshot, error) {
	e.sessionInfo.SnapEpoch++
	return e.impl.SnapWithSessionMode(ctx, e.sessionInfo.SnapEpoch)
}
func (e *cliActionExecutor) Screenshot(ctx context.Context) ([]byte, error) {
	return e.impl.Screenshot(ctx, false)
}
func (e *cliActionExecutor) GetSessionState(context.Context) (*btest.BehaviorState, error) {
	state := behaviorFromSessionInfo(e.sessionInfo)
	if state == nil {
		return nil, fmt.Errorf("session behavior unavailable")
	}
	return state, nil
}
func (e *cliActionExecutor) GetTelemetry(context.Context) (*btest.TelemetryState, error) {
	return nil, fmt.Errorf("stateless session telemetry unavailable")
}
func (e *cliActionExecutor) CollectRegions(ctx context.Context) ([]btest.RegionSnap, error) {
	return btest.CollectRegionsViaEval(func(expr string, result any) error { return e.impl.EvalJS(ctx, expr, result) })
}
func (e *cliActionExecutor) PrepareObservation(ctx context.Context, snap *browser.Snapshot, id, path string) error {
	if identity, ok := e.impl.(interface {
		SessionDocumentIdentity(context.Context) (int64, error)
	}); ok && snap.DocumentID == 0 {
		documentID, err := identity.SessionDocumentIdentity(ctx)
		if err != nil {
			return err
		}
		snap.DocumentID = documentID
	}
	return browser.BindSessionObservation(e.sessionInfo, snap, id, path)
}
func (e *cliActionExecutor) CommitObservation(ctx context.Context, snap *browser.Snapshot) error {
	return browser.SaveObservedSession(e.sessionInfo, snap, browser.SessionRefsFromSnapshot(snap, true))
}
func (e *cliActionExecutor) ResizeViewport(ctx context.Context, w, h int) error {
	if syncer, ok := e.impl.(browser.LiveViewportSyncer); ok {
		if err := syncer.SetLiveViewport(w, h, 1, false); err != nil {
			return err
		}
		e.sessionInfo.ViewportW = w
		e.sessionInfo.ViewportH = h
		browser.InvalidateSessionObservation(e.sessionInfo)
		return browser.SaveSession(e.sessionInfo)
	}
	return fmt.Errorf("viewport mutation unavailable")
}
func (e *cliActionExecutor) CollectProbe(ctx context.Context, p btest.ProbeSpec) (btest.ProbeFact, error) {
	return btest.CollectProbe(ctx, p, func(script string, result any) error { return e.impl.EvalJS(ctx, script, result) }, func() error {
		info, err := browser.LoadSession(e.sessionID)
		if err != nil {
			return err
		}
		if err := browser.SessionAllowsAIInput(info); err != nil {
			return err
		}
		var url string
		if err := e.impl.EvalJS(ctx, "window.location.href", &url); err != nil {
			return err
		}
		decision := info.Policy.EvaluateAct("eval", url)
		if !decision.Allowed {
			return fmt.Errorf("%s", decision.Reason)
		}
		if err := browser.ApplySessionIntervention(e.sessionInfo, fmt.Sprintf("probe-%s-%d", p.ID, time.Now().UnixNano()), p.Kind); err != nil {
			return err
		}
		return browser.SaveSession(e.sessionInfo)
	}, func() (bool, error) {
		info, err := browser.LoadSession(e.sessionID)
		if err != nil {
			return true, err
		}
		return info.RedactFormValues, nil
	})
}

// ============================================================
// § 工具函数
// ============================================================

// loadObservationFile 从 JSON 文件加载 Observation。
func loadObservationFile(path string) (*btest.Observation, error) { return btest.LoadObservation(path) }

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
	start := strings.Index(section, "```dw-browser-plan")
	if start < 0 {
		return nil
	}
	body := section[start+len("```dw-browser-plan"):]
	end := strings.Index(body, "```")
	if end < 0 {
		return nil
	}
	plan, err := btest.ParsePlan([]byte(strings.TrimSpace(body[:end])))
	if err != nil {
		return nil
	}
	return plan.Steps
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

// runWaitV2 routes duration and assertion waits through the one condition kernel.
func runWaitResult(args []string) (CommandResult, error) {
	var id, assert, timeoutRaw, durationRaw string
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		switch name {
		case "--id", "--assert", "--timeout", "--duration":
		default:
			return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("unsupported argument %q", args[i]))
		}
		if !inline {
			if i+1 >= len(args) {
				return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("%s requires value", name))
			}
			i++
			value = args[i]
		}
		switch name {
		case "--id":
			id = value
		case "--assert":
			assert = value
		case "--timeout":
			timeoutRaw = value
		case "--duration":
			durationRaw = value
		}
	}
	parseDuration := func(raw string) (int, error) {
		if raw == "" {
			return 0, nil
		}
		value, err := time.ParseDuration(raw)
		if err != nil || value <= 0 || value.Milliseconds() <= 0 {
			return 0, fmt.Errorf("invalid positive duration %q", raw)
		}
		return int(value.Milliseconds()), nil
	}
	timeout, err := parseDuration(timeoutRaw)
	if err != nil {
		return CommandResult{}, commandError("validation", exitRunErr, err)
	}
	duration, err := parseDuration(durationRaw)
	if err != nil {
		return CommandResult{}, commandError("validation", exitRunErr, err)
	}
	spec := btest.WaitSpec{Assert: assert, TimeoutMs: timeout, DurationMs: duration}
	if id == "" {
		return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("wait requires id"))
	}
	if err := spec.Validate(); err != nil {
		return CommandResult{}, commandError("validation", exitRunErr, err)
	}
	info, err := browser.LoadSession(id)
	if err != nil {
		return CommandResult{}, commandError("session-load", exitRunErr, err)
	}
	start := time.Now()
	ctx := context.Background()
	var read btest.ObservationReader
	if spec.Assert != "" {
		waitCtx, cancel := context.WithTimeout(ctx, time.Duration(spec.TimeoutMs)*time.Millisecond)
		defer cancel()
		ctx = waitCtx
		impl, err := connectSessionE(ctx, info, "wait", commonFlags{sessionID: id})
		if err != nil {
			return CommandResult{}, commandError("attachment", exitRunErr, err)
		}
		defer closeSessionCore(impl)
		read = func(readCtx context.Context) (*btest.Observation, error) {
			snap, err := impl.SnapWithSessionMode(readCtx, info.SnapEpoch)
			if err != nil {
				return nil, err
			}
			return btest.BuildObservation(id, snap, nil, "", behaviorFromSessionInfo(info), nil), nil
		}
	}
	err = btest.WaitForAssertion(ctx, spec, read)
	status := btest.StatusDelivered
	reason := "declared duration or condition delivered"
	code := exitOK
	if err != nil {
		status = btest.StatusBlocked
		reason = err.Error()
		code = exitRunErr
	}
	return commandResult(map[string]any{"schema": "dw.wait.v2", "status": status, "reason": reason, "elapsed_ms": time.Since(start).Milliseconds()}, code), nil
}
func (e *cliActionExecutor) LastActionFidelity() browser.ActionFidelityReport {
	return actionFidelityReport(e.impl)
}

func validateReadSessionArgsE(args []string, cmd string) error {
	for i := 0; i < len(args); i++ {
		if args[i] == "--id" && i+1 < len(args) && args[i+1] != "" {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "--id=") && len(args[i]) > 5 {
			continue
		}
		return fmt.Errorf("%s: unsupported or incomplete argument %q", cmd, args[i])
	}
	return nil
}

func (e *cliActionExecutor) ObservationContext() btest.ObservationContext {
	return btest.ObservationContext{SessionID: e.sessionID, RuntimeRunID: e.sessionInfo.BrowserRunID, TargetID: e.sessionInfo.TargetID, Scenario: string(e.sessionInfo.Scenario), Deterministic: e.sessionInfo.Policy.Deterministic}
}
