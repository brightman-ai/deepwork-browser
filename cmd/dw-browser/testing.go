// testing.go — observe / diff / check / journey / do / get 命令实现
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	llmURL      string // --llm-url (Ollama/vLLM/OpenAI-compatible endpoint)
	llmModel    string // --llm-model (model name, e.g. gemma4:26b-a4b)
	visionURL   string // --vision-url (VLM endpoint, defaults to llm-url)
	visionModel string // --vision-model (VLM model name)
}

// parseLLMFlags 从 args 中提取 LLM/VLM 参数，返回剩余 args。
// CLI flags 优先级高于环境变量；--vision-url 默认 fallback 到 --llm-url。
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
		case arg == "--llm-model" && i+1 < len(args):
			lf.llmModel = args[i+1]
			i++
		case strings.HasPrefix(arg, "--llm-model="):
			lf.llmModel = arg[len("--llm-model="):]
		case arg == "--vision-url" && i+1 < len(args):
			lf.visionURL = args[i+1]
			i++
		case strings.HasPrefix(arg, "--vision-url="):
			lf.visionURL = arg[len("--vision-url="):]
		case arg == "--vision-model" && i+1 < len(args):
			lf.visionModel = args[i+1]
			i++
		case strings.HasPrefix(arg, "--vision-model="):
			lf.visionModel = arg[len("--vision-model="):]
		default:
			remaining = append(remaining, arg)
		}
	}

	// Apply to environment so downstream code (NewPlanner/NewVisionOracle) picks them up.
	if lf.llmURL != "" {
		os.Setenv("DW_BROWSER_LLM_URL", lf.llmURL)
	}
	if lf.llmModel != "" {
		os.Setenv("DW_BROWSER_LLM_MODEL", lf.llmModel)
	}
	if lf.visionURL != "" {
		os.Setenv("DW_BROWSER_VISION_URL", lf.visionURL)
	} else if lf.llmURL != "" {
		// vision defaults to llm endpoint if not specified separately
		os.Setenv("DW_BROWSER_VISION_URL", lf.llmURL)
	}
	if lf.visionModel != "" {
		os.Setenv("DW_BROWSER_VISION_MODEL", lf.visionModel)
	}

	return lf, remaining
}

// visionEnabled 判断是否应启用 VisionOracle。
// 检测：DW_BROWSER_VISION_URL 或 DW_BROWSER_VISION_PROVIDER 已设置，或 usingRaw 含 "visual"。
// parseLLMFlags 已把 --vision-* CLI flags 写入环境变量，所以这里只需检测 env。
func visionEnabled(usingRaw string) bool {
	if os.Getenv("DW_BROWSER_VISION_URL") != "" {
		return true
	}
	if os.Getenv("DW_BROWSER_VISION_PROVIDER") != "" {
		return true
	}
	for _, u := range strings.Split(usingRaw, ",") {
		if strings.TrimSpace(u) == "visual" {
			return true
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
	help := `dw-browser testing commands:

  observe   Multi-channel page observation (A11y + screenshot + behavior + telemetry)
  diff      Compare two observations (before/after state diff)
  check     Run assertion against page or observation file
  journey   Execute BDD YAML test scenario with evidence collection
  do        Execute natural language goal via LLM planner + Skills KB
  get       Extract typed data from current page state

Usage examples:

  # Observe current page state
  dw-browser observe --id session1 --layers structural,behavior,telemetry --out obs.json

  # Compare before/after
  dw-browser diff before.json after.json --out diff.json

  # Run single assertion
  dw-browser check --id session1 --assert "console_errors_count == 0"

  # Run baseline assertions from file
  dw-browser check --id session1 --file baselines/portal/app-shell.yaml

  # Run assertion against saved observation (no session needed)
  dw-browser check --observation obs.json --assert "exists(role='button')"

  # Execute BDD journey with evidence
  dw-browser journey --file tests/bdd/bs15-sidebar-research.yaml --evidence ./evidence/

  # Execute natural language goal
  dw-browser do --id session1 "Open browser sidebar and search for AI testing"

  # Extract data
  dw-browser get --id session1 "active tab url"

  # Generate baseline from exploration
  dw-browser explore --id session1 --learn-baseline --out candidate-baseline.yaml

LLM/VLM configuration (for do, check --using visual, journey with visual checks):

  --llm-url <url>        LLM endpoint (Ollama/vLLM/OpenAI-compatible)
                         Default: http://127.0.0.1:11434
                         Env: DW_BROWSER_LLM_URL

  --llm-model <name>     LLM model name
                         Default: gemma4:26b-a4b
                         Env: DW_BROWSER_LLM_MODEL

  --vision-url <url>     VLM endpoint (defaults to --llm-url if not set)
                         Env: DW_BROWSER_VISION_URL

  --vision-model <name>  VLM model name (defaults to --llm-model if not set)
                         Env: DW_BROWSER_VISION_MODEL

Assertion DSL primitives:

  exists(role='button', name='Search')    Element exists in A11y tree
  count(role='link') >= 1                 Count matching elements
  visible(testid='sidebar')              Element is visible (= exists in P1)
  gone(text='Loading')                   Element no longer exists
  text_contains('hello')                 Page text contains string
  url_matches('/portal/topic')           URL contains pattern
  tab_count == 2                         Tab count equals value
  active_tab_url_contains('example.com') Active tab URL contains
  console_errors_count == 0              No console errors
  network_failures_count == 0            No network failures
  latency_lt(3000)                       Action latency under threshold (ms)

Exit codes: 0=PASS  1=FAIL  2=RUN_ERROR
`
	fmt.Fprint(os.Stderr, help)
}

// ============================================================
// § observe 命令
// ============================================================

// runObserve 采集当前 session 的多通道 Observation 快照。
// dw-browser observe --id <session-id> [--layers structural,behavior,telemetry] [--out file.json]
func runObserve(args []string) {
	if containsHelp(args) {
		printTestingHelp()
		os.Exit(exitOK)
	}
	var outFile string
	var layersRaw string
	clean := make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--out" && i+1 < len(args):
			outFile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--out="):
			outFile = arg[len("--out="):]
		case arg == "--layers" && i+1 < len(args):
			layersRaw = args[i+1]
			i++
		case strings.HasPrefix(arg, "--layers="):
			layersRaw = arg[len("--layers="):]
		default:
			clean = append(clean, arg)
		}
	}

	_, flags := parseCommonFlags(clean, "observe")
	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser observe: requires --id <session-id>")
		os.Exit(exitRunErr)
	}

	// 解析 --layers（默认全部采集）
	wantStructural, wantBehavior, wantTelemetry, wantLayout := true, true, true, true
	if layersRaw != "" {
		wantStructural, wantBehavior, wantTelemetry, wantLayout = false, false, false, false
		for _, layer := range strings.Split(layersRaw, ",") {
			switch strings.TrimSpace(layer) {
			case "structural":
				wantStructural = true
			case "behavior":
				wantBehavior = true
			case "telemetry":
				wantTelemetry = true
			case "layout", "visual":
				wantLayout = true
			}
		}
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

	// — structural 层 —
	var snap *browser.Snapshot
	if wantStructural {
		snap, err = impl.SnapWithSessionMode(ctx, sessionInfo.SnapEpoch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser observe: snap failed: %v\n", err)
			os.Exit(exitRunErr)
		}
		// FIX-J4: settle-wait — retry until SPA a11y tree is populated (up to ~6s).
		for i := 0; i < 6 && (snap == nil || len(snap.Refs) == 0); i++ {
			time.Sleep(1 * time.Second)
			snap, _ = impl.SnapWithSessionMode(ctx, sessionInfo.SnapEpoch)
		}
	}

	// — visual 层（截图）—
	var screenshotData []byte
	screenshotPath := ""
	if outFile != "" {
		dir := strings.TrimSuffix(outFile, ".json")
		screenshotPath = dir + "-screenshot.png"
	} else {
		screenshotPath = fmt.Sprintf("obs-%d-screenshot.png", time.Now().UnixMilli())
	}
	if screenshotRaw, err2 := impl.Screenshot(ctx, false); err2 == nil {
		screenshotData = screenshotRaw
	}

	// — behavior 层 — 从 SessionInfo 中提取（stateless CLI 模式）
	var behavior *btest.BehaviorState
	if wantBehavior {
		behavior = behaviorFromSessionInfo(sessionInfo)
	}

	// — telemetry 层 — P1: CDP 持久连接不在 stateless CLI 范围，返回空+注记
	var telemetry *btest.TelemetryState
	if wantTelemetry {
		telemetry = &btest.TelemetryState{
			ConsoleErrors:   []btest.ConsoleEntry{},
			NetworkFailures: []btest.NetworkEntry{},
		}
	}

	obs := btest.BuildObservation(flags.sessionID, snap, screenshotData, screenshotPath, behavior, telemetry)

	// — layout 层（data-region regions via JS eval）—
	if wantLayout {
		if regions, err2 := btest.CollectRegionsViaEval(func(expr string, result interface{}) error {
			return impl.EvalJS(ctx, expr, result)
		}); err2 == nil && len(regions) > 0 {
			if obs.Visual == nil {
				obs.Visual = &btest.VisualState{}
			}
			obs.Visual.Regions = regions
		}
	}

	// 补充 telemetry 注记（不修改 browser/testing 包，在输出层添加）
	type observeOutput struct {
		*btest.Observation
		TelemetryNote string `json:"_telemetry_note,omitempty"`
	}
	out := observeOutput{Observation: obs}
	if wantTelemetry {
		out.TelemetryNote = "P1: telemetry requires persistent CDP connection, not available in stateless CLI mode"
	}

	writeJSONOrStdout(out, outFile, "observe")
	os.Exit(exitOK)
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
	if visionEnabled(usingRaw) {
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
		// 单条断言：解析 --using 并传入，支持 visual oracle
		var usingSlice []string
		if usingRaw != "" {
			parts := strings.Split(usingRaw, ",")
			usingSlice = make([]string, 0, len(parts))
			for _, p := range parts {
				usingSlice = append(usingSlice, strings.TrimSpace(p))
			}
		}
		result := engine.EvaluateWithUsing(obs, assertExpr, usingSlice)
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
		}
		defer closeSessionCore(impl)
	} else if spec.Environment.BaseURL != "" || spec.Environment.EntryURL != "" {
		// 新建 one-shot session（从 spec 的 entry URL）
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
		if _, navErr := bc.Navigate(ctx, entryURL); navErr != nil {
			fmt.Fprintf(os.Stderr, "dw-browser journey: navigate to %s: %v\n", entryURL, navErr)
			os.Exit(exitRunErr)
		}

		executor = &oneshotActionExecutor{impl: bc, ctx: ctx, telemetry: journeyTelemetry}
	} else {
		fmt.Fprintln(os.Stderr, "dw-browser journey: requires --id <session-id> or spec.environment.base_url")
		os.Exit(exitRunErr)
	}

	runner, err := btest.NewRunner(executor, evidenceDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser journey: create runner: %v\n", err)
		os.Exit(exitRunErr)
	}
	if visionEnabled("") {
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
}

func (e *cliActionExecutor) Execute(ctx context.Context, action string) error {
	// 将 step.Do 直接作为 act 命令执行
	// 支持 dw-browser act 语法: "click @r5", "fill #input 'text'", "navigate http://..."
	// FIX-J3a: only treat as navigation when the target looks like a URL.
	// Natural-language goals like "navigate to X" fall through to ActWithSessionMode.
	trimmed := strings.TrimSpace(action)
	if rest, ok := strings.CutPrefix(strings.ToLower(trimmed), "navigate "); ok {
		target := strings.TrimSpace(trimmed[len(trimmed)-len(rest):]) // preserve original case
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "/") {
			if _, err := e.impl.Navigate(ctx, target); err != nil {
				return fmt.Errorf("navigate %s: %w", target, err)
			}
			return nil
		}
		// NL goal like "navigate to X" — fall through to ActWithSessionMode.
		// TODO(planner): route free-form NL goals through the `do` command's LLM planner (hybrid strategy, deferred)
	}
	_, err := e.impl.ActWithSessionMode(ctx, action, false)
	return err
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
}

func (e *oneshotActionExecutor) Execute(ctx context.Context, action string) error {
	// FIX-J3a: only treat as navigation when the target looks like a URL.
	// Natural-language goals like "navigate to X" fall through to Act.
	trimmed := strings.TrimSpace(action)
	if rest, ok := strings.CutPrefix(strings.ToLower(trimmed), "navigate "); ok {
		target := strings.TrimSpace(trimmed[len(trimmed)-len(rest):]) // preserve original case
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "/") {
			if _, err := e.impl.Navigate(ctx, target); err != nil {
				return fmt.Errorf("navigate %s: %w", target, err)
			}
			return nil
		}
		// NL goal like "navigate to X" — fall through to Act.
		// TODO(planner): route free-form NL goals through the `do` command's LLM planner (hybrid strategy, deferred)
	}
	_, err := e.impl.Act(ctx, action, false)
	return err
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
// § do 命令 — NL Facade: skill-first, planner fallback
// ============================================================

// runDo 执行 NL 目标：先查 Skills KB，无匹配时调 Planner 分解，逐步执行。
// dw-browser do --id <session-id> "Open browser sidebar"
func runDo(args []string) {
	if containsHelp(args) {
		printTestingHelp()
		os.Exit(exitOK)
	}
	_, args = parseLLMFlags(args)
	positional, flags := parseCommonFlags(args, "do")

	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser do: requires --id <session-id>")
		os.Exit(exitRunErr)
	}
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser do: requires <goal>")
		os.Exit(exitRunErr)
	}
	goal := positional[0]

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser do: %v\n", err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	impl := connectSession(ctx, sessionInfo, "do", flags)
	defer closeSessionCore(impl)
	impl.RestoreRefsFromSession(sessionInfo.Refs)

	// Step 1: snap 获取当前 structural state
	snap, err := impl.SnapWithSessionMode(ctx, sessionInfo.SnapEpoch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser do: snap failed: %v\n", err)
		os.Exit(exitRunErr)
	}

	var structural *btest.StructuralState
	if snap != nil {
		structural = doStructuralFromSnap(snap)
	}

	// Step 2: 检查 Skills KB 是否有匹配
	var plan *btest.PlanResult
	recipeSteps := lookupSkillRecipe(sessionInfo.PageURL, goal)
	if recipeSteps != nil {
		// 有匹配 skill — 直接用 recipe 步骤
		plan = &btest.PlanResult{Goal: goal, Steps: recipeSteps}
	} else {
		// 无 skill — 调用 Planner
		planner := btest.NewPlanner()
		plan, err = planner.Plan(ctx, goal, structural)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser do: planner: %v\n", err)
			os.Exit(exitRunErr)
		}
	}

	// Step 3: 逐步执行
	executor := &cliActionExecutor{
		sessionID:   flags.sessionID,
		sessionInfo: sessionInfo,
		impl:        impl,
		ctx:         ctx,
	}

	stepErrors := make([]string, 0)
	for i, step := range plan.Steps {
		if err := executor.Execute(ctx, step.Action); err != nil {
			stepErrors = append(stepErrors, fmt.Sprintf("step %d (%s): %v", i+1, step.Description, err))
			break // stop on first failure
		}
		if step.Wait != "" {
			if waitErr := executor.Wait(ctx, step.Wait, 15000); waitErr != nil {
				stepErrors = append(stepErrors, fmt.Sprintf("step %d wait (%s): %v", i+1, step.Wait, waitErr))
				break
			}
		}
	}

	// Step 4: observe final state
	finalObs := observeSession(flags, "do")

	type doOutput struct {
		Plan        *btest.PlanResult   `json:"plan"`
		Observation *btest.Observation  `json:"observation"`
		Errors      []string            `json:"errors,omitempty"`
	}
	out := doOutput{Plan: plan, Observation: finalObs}
	if len(stepErrors) > 0 {
		out.Errors = stepErrors
	}

	enc, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(enc))

	if len(stepErrors) > 0 {
		os.Exit(exitFail)
	}
	os.Exit(exitOK)
}

// lookupSkillRecipe 检查当前页面 domain 的 skill 中是否有匹配 goal 的 recipe。
// 返回 []btest.PlannedStep（来自 recipe 行），未匹配返回 nil。
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
func runGetNL(args []string) {
	if containsHelp(args) {
		printTestingHelp()
		os.Exit(exitOK)
	}
	positional, flags := parseCommonFlags(args, "get")

	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser get: requires --id <session-id>")
		os.Exit(exitRunErr)
	}
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser get: requires <query>")
		os.Exit(exitRunErr)
	}
	query := positional[0]

	obs := observeSession(flags, "get")

	g := btest.Getter{}
	result := g.Get(obs, query)

	enc, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(enc))
	os.Exit(exitOK)
}
