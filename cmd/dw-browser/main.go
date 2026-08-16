// dw-browser — BS-09 Browser Runtime CLI 测试引擎（独立二进制）
//
// 零 Deepwork 依赖：不导入 internal/conversation, internal/topic, internal/webui
// 铁律 IR-05 + IR-08 [Ref: BP §B1 Phase 5, CAP-BS09-C5]
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
	"github.com/brightman-ai/deepwork-browser/browser/audit"
	"github.com/brightman-ai/deepwork-browser/browser/safari"
	"github.com/brightman-ai/kit/obs"
	cdpTarget "github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"gopkg.in/yaml.v3"
)

const version = "0.11.0"

// ActionEngine returns its own explicit error at 15s. The CLI watchdog is a
// slightly wider process-level fence covering every BrowserCore implementation.
const cliActTimeout = 17 * time.Second

// exitCodes [IR-08]
const (
	exitOK     = 0 // 全 PASS
	exitFail   = 1 // 断言失败
	exitRunErr = 2 // 运行错误（Chrome 未找到等）
)

// ============================================================
// § 身份轴:Persona(可组合测试保真环境)
// ============================================================
// 设备/指纹 SSOT 已收敛到 browser 包:身份指纹 = browser.FingerprintPreset
// (browser/fingerprint.go 的 BuiltinPresets),命名组合人格 = browser.Personas
// (browser/persona.go)。旧的 cmd 层 DevicePreset/devicePresets 已删除(消除
// "同一身份轴两份实现"的冗余);CLI 入口 = --persona,见 parseCommonFlags。

// ============================================================
// § CLI Flags 通用结构
// ============================================================

// commonFlags 所有子命令共享的 flags。
type commonFlags struct {
	profileID         string
	sessionID         string // --id <id>
	sessionKind       browser.BrowserSessionKind
	goal              string
	owner             string
	isolation         string
	isolationExplicit bool
	serviceName       string
	accountID         string
	url               string
	ephemeral         bool
	headless          bool // --headless 兼容字段；真实语义以 mode 为准
	mode              browser.BrowserMode
	modeExplicit      bool
	viewport          string // WxH 格式
	persona           string // --persona: 组合测试保真人格名(身份轴主入口,见 browser.Personas)
	userAgent         string
	diag              bool // --diag 启用每步 observation log
	verbose           bool // --verbose 显示内部 DEBUG/INFO/WARN 诊断日志
	stealth           bool // --stealth 只在 headless 下生效：反检测 UA + 额外 flags
	// Safari 引擎
	engine       string // --engine: "chrome" (default) | "safari"
	safariDevice string // --safari-device: Safari Simulator 设备名/UDID
	// Scenario (业务主入口, session-creating 命令必选；见 browser/scenario.go)。
	// Policy/render/kind 由 scenario 导出，不再有裸技术开关。
	scenario string // --scenario app-test-explore | app-test-baseline | webvisit
	// allowHosts 仍保留 (webvisit 的信任放行参数；非绕过旁路)。
	allowHosts []string // --allow-host <hosts> (comma-sep / repeatable): 额外信任主机
	// browserChrome — browser chrome 仿真模式 (auto|on|off, 默认 auto)。
	// auto: 移动预设带 chrome 几何数据即启用(双视口显形 + 截图画 Safari 底栏 +
	// act 遮挡拒绝 + observe/audit 机读遮挡区)。spec: docs/product/browser-chrome/。
	browserChrome string
	// 解析后
	viewportW          int
	viewportH          int
	hasViewport        bool
	hasUA              bool
	hasPersona         bool
	personaTouch       bool   // 解析后:所选 persona 的 Fingerprint facet 是否触摸设备
	personaFingerprint string // 解析后:所选 persona 的身份指纹 ID(流入 PresetID 管道)
}

// scenarioPolicyOrExit enforces the REQUIRED --scenario on every session-creating
// command. It normalizes the flag — erroring with the three legal values when
// missing/unknown (fail-closed) — then derives the SessionPolicy + internal
// session kind. --allow-host is webvisit's safelist ONLY: it is merged into the
// policy for webvisit and rejected for the local-app scenarios (a stray safelist
// on a local scenario is a mis-use, not a silent no-op). It does NOT mutate the
// render mode; callers apply the scenario's default mode themselves (some, like
// journey, have their own mode precedence). Returns scenario, policy, and the
// scenario's default render mode.
func scenarioPolicyOrExit(flags *commonFlags, cmd string) (browser.Scenario, browser.SessionPolicy, browser.BrowserMode) {
	scenario, err := browser.NormalizeScenario(flags.scenario)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: %v\n", cmd, err)
		os.Exit(exitRunErr)
	}
	if len(flags.allowHosts) > 0 && scenario != browser.ScenarioWebVisit {
		fmt.Fprintf(os.Stderr, "dw-browser %s: --allow-host is only valid with --scenario webvisit\n", cmd)
		os.Exit(exitRunErr)
	}
	policy, mode, kind := browser.ScenarioPolicy(scenario)
	if scenario == browser.ScenarioWebVisit {
		policy.AllowHosts = flags.allowHosts
	}
	flags.sessionKind = kind
	flags.scenario = string(scenario)
	return scenario, policy, mode
}

// resolveScenario is scenarioPolicyOrExit plus applying the scenario's default
// render mode onto flags (only when the caller did not pass an explicit --mode).
// Used by open / once / act(one-shot) / test / layout.
func resolveScenario(flags *commonFlags, cmd string) (browser.Scenario, browser.SessionPolicy) {
	scenario, policy, mode := scenarioPolicyOrExit(flags, cmd)
	if !flags.modeExplicit {
		flags.mode = mode
		flags.headless = mode == browser.ModeHeadless
	}
	return scenario, policy
}

// parseCommonFlags 从 args 中提取通用 flags，返回剩余 positional args。
// 错误时打印提示并 exit 2。
//
// Mode 默认值优先级:
//  1. 显式 --mode {headless|headed|visible|human} (最高)
//  2. 显式 --headless (兼容老 flag)
//  3. env DW_BROWSER_DEFAULT_MODE={headless|headed|visible|human}
//  4. fallback "visible" (兼容 CLI open 的持久会话语义)
//
// 测试场景 (Makefile / CI / sub-agent smoke test) 通过 env 一刀切设置,
// 避免每个调用点都得手加 --headless.
func parseCommonFlags(args []string, cmd string) (positional []string, flags commonFlags) {
	flags.profileID = ""
	flags.sessionKind = browser.SessionKindTask

	// Step 1: 应用 env 默认 (优先级 3)
	if envMode := strings.TrimSpace(os.Getenv("DW_BROWSER_DEFAULT_MODE")); envMode != "" {
		if mode, ok := parseBrowserMode(envMode); ok {
			flags.mode = mode
			flags.headless = mode == browser.ModeHeadless
			flags.modeExplicit = true
		} else {
			fmt.Fprintf(os.Stderr, "dw-browser: 警告 — DW_BROWSER_DEFAULT_MODE=%q 不识别 (允许: headless/headed/visible/human), 回退 visible\n",
				os.Getenv("DW_BROWSER_DEFAULT_MODE"))
		}
	}

	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--id" && i+1 < len(args):
			flags.sessionID = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--id="):
			flags.sessionID = arg[len("--id="):]
			i++
		case arg == "--scenario" && i+1 < len(args):
			flags.scenario = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--scenario="):
			flags.scenario = arg[len("--scenario="):]
			i++
		case arg == "--allow-host" && i+1 < len(args):
			flags.allowHosts = append(flags.allowHosts, strings.Split(args[i+1], ",")...)
			i += 2
		case strings.HasPrefix(arg, "--allow-host="):
			flags.allowHosts = append(flags.allowHosts, strings.Split(arg[len("--allow-host="):], ",")...)
			i++
		case arg == "--goal" && i+1 < len(args):
			flags.goal = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--goal="):
			flags.goal = arg[len("--goal="):]
			i++
		case arg == "--url" && i+1 < len(args):
			flags.url = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--url="):
			flags.url = arg[len("--url="):]
			i++
		case arg == "--owner" && i+1 < len(args):
			flags.owner = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--owner="):
			flags.owner = arg[len("--owner="):]
			i++
		case arg == "--isolation" && i+1 < len(args):
			flags.isolation = args[i+1]
			flags.isolationExplicit = true
			i += 2
		case strings.HasPrefix(arg, "--isolation="):
			flags.isolation = arg[len("--isolation="):]
			flags.isolationExplicit = true
			i++
		case arg == "--service" && i+1 < len(args):
			flags.serviceName = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--service="):
			flags.serviceName = arg[len("--service="):]
			i++
		case (arg == "--account" || arg == "--provider-account") && i+1 < len(args):
			flags.accountID = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--account="):
			flags.accountID = arg[len("--account="):]
			i++
		case strings.HasPrefix(arg, "--provider-account="):
			flags.accountID = arg[len("--provider-account="):]
			i++
		case arg == "--profile" && i+1 < len(args):
			flags.profileID = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--profile="):
			flags.profileID = arg[len("--profile="):]
			i++
		case arg == "--ephemeral":
			flags.ephemeral = true
			i++
		case arg == "--viewport" && i+1 < len(args):
			flags.viewport = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--viewport="):
			flags.viewport = arg[len("--viewport="):]
			i++
		case arg == "--persona" && i+1 < len(args):
			flags.persona = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--persona="):
			flags.persona = arg[len("--persona="):]
			i++
		case arg == "--device" || strings.HasPrefix(arg, "--device="):
			// Removed: --device 被 --persona 取代(身份轴收敛到单一 Persona 模型)。
			// 显式 fail-closed,不让陈旧 flag 静默落到 positional。
			fmt.Fprintf(os.Stderr, "dw-browser %s: --device 已移除,请用 --persona <名称>(dw-browser --help 看可用人格)\n", cmd)
			os.Exit(exitRunErr)
		case arg == "--user-agent" && i+1 < len(args):
			flags.userAgent = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--user-agent="):
			flags.userAgent = arg[len("--user-agent="):]
			i++
		case arg == "--headless":
			// 兼容老 flag (优先级 2): 显式 --headless 永远胜过 env
			flags.headless = true
			flags.mode = browser.ModeHeadless
			flags.modeExplicit = true
			i++
		case arg == "--mode" && i+1 < len(args):
			// 推荐新 flag (优先级 1): --mode {headless|headed|visible|human}
			mode, ok := parseBrowserMode(args[i+1])
			if !ok {
				fmt.Fprintf(os.Stderr, "dw-browser %s: --mode 值无效 %q (允许: headless/headed/visible/human)\n", cmd, args[i+1])
				os.Exit(exitRunErr)
			}
			flags.mode = mode
			flags.headless = mode == browser.ModeHeadless
			flags.modeExplicit = true
			i += 2
		case strings.HasPrefix(arg, "--mode="):
			val := arg[len("--mode="):]
			mode, ok := parseBrowserMode(val)
			if !ok {
				fmt.Fprintf(os.Stderr, "dw-browser %s: --mode 值无效 %q (允许: headless/headed/visible/human)\n", cmd, val)
				os.Exit(exitRunErr)
			}
			flags.mode = mode
			flags.headless = mode == browser.ModeHeadless
			flags.modeExplicit = true
			i++
		case arg == "--diag":
			flags.diag = true
			i++
		case arg == "--verbose":
			flags.verbose = true
			i++
		case arg == "--stealth":
			flags.stealth = true
			i++
		case arg == "--engine" && i+1 < len(args):
			flags.engine = strings.ToLower(strings.TrimSpace(args[i+1]))
			i += 2
		case strings.HasPrefix(arg, "--engine="):
			flags.engine = strings.ToLower(strings.TrimSpace(arg[len("--engine="):]))
			i++
		case arg == "--browser-chrome" && i+1 < len(args):
			flags.browserChrome = strings.ToLower(strings.TrimSpace(args[i+1]))
			i += 2
		case strings.HasPrefix(arg, "--browser-chrome="):
			flags.browserChrome = strings.ToLower(strings.TrimSpace(arg[len("--browser-chrome="):]))
			i++
		case arg == "--safari-device" && i+1 < len(args):
			flags.safariDevice = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--safari-device="):
			flags.safariDevice = arg[len("--safari-device="):]
			i++
		case arg == "--remote-writes" || strings.HasPrefix(arg, "--remote-writes=") ||
			arg == "--deterministic" || strings.HasPrefix(arg, "--deterministic=") ||
			arg == "--kind" || strings.HasPrefix(arg, "--kind=") ||
			arg == "--commit" || strings.HasPrefix(arg, "--commit="):
			// Removed in v0.4.0: session behavior is now derived from --scenario.
			// Reject explicitly (fail-closed) rather than let a stale flag fall
			// through to a positional arg and silently change nothing.
			name := arg
			if idx := strings.IndexByte(name, '='); idx >= 0 {
				name = name[:idx]
			}
			fmt.Fprintf(os.Stderr, "dw-browser %s: flag %s removed; session behavior is now set by --scenario (see --help)\n", cmd, name)
			os.Exit(exitRunErr)
		case arg == "--help" || arg == "-h":
			printCommandUsage(cmd)
			os.Exit(exitOK)
		default:
			positional = append(positional, arg)
			i++
		}
	}

	// 互斥验证: --ephemeral 和 --profile 不可同时使用
	if flags.ephemeral && flags.profileID != "" {
		fmt.Fprintf(os.Stderr, "dw-browser %s: --ephemeral 和 --profile 互斥，不可同时使用\n", cmd)
		os.Exit(exitRunErr)
	}

	// 验证 --engine 值
	if flags.engine != "" && flags.engine != "chrome" && flags.engine != "safari" {
		fmt.Fprintf(os.Stderr, "dw-browser %s: --engine 值无效 %q (允许: chrome/safari)\n", cmd, flags.engine)
		os.Exit(exitRunErr)
	}

	defaults := browser.DefaultsForBrowserSessionKind(flags.sessionKind)
	if !flags.modeExplicit {
		flags.mode = defaults.Mode
		flags.headless = flags.mode == browser.ModeHeadless
	}
	if flags.owner == "" {
		flags.owner = defaults.Owner
	}
	if flags.isolation == "" {
		flags.isolation = defaults.Isolation
	}
	if flags.profileID != "" && !flags.isolationExplicit && flags.isolation == browser.SessionIsolationEphemeral {
		flags.isolation = browser.SessionIsolationDedicated
	}
	if flags.ephemeral {
		flags.isolation = browser.SessionIsolationEphemeral
	}
	if flags.isolation != browser.SessionIsolationEphemeral &&
		flags.isolation != browser.SessionIsolationDedicated &&
		flags.isolation != browser.SessionIsolationPool {
		fmt.Fprintf(os.Stderr, "dw-browser %s: --isolation 值无效 %q (允许: ephemeral/dedicated/profile-pool)\n", cmd, flags.isolation)
		os.Exit(exitRunErr)
	}

	// 解析 --persona（优先级高于 --viewport / --user-agent；后者作细粒度 override 逃生口）
	if flags.persona != "" {
		p, err := browser.ResolvePersona(flags.persona)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser: %v\n", err)
			printPersonas()
			os.Exit(exitRunErr)
		}
		w, h, _ := p.EffectiveViewport()
		flags.viewportW = w
		flags.viewportH = h
		flags.userAgent = p.EffectiveUserAgent()
		flags.personaTouch = p.Touch()
		flags.personaFingerprint = p.FingerprintID()
		flags.hasViewport = w > 0 && h > 0
		flags.hasUA = true
		flags.hasPersona = true
		// --viewport / --user-agent 若同时指定则覆盖 persona facet 默认值
	}

	// 解析 --viewport（覆盖 persona 的 viewport）
	if flags.viewport != "" {
		w, h, err := parseViewport(flags.viewport)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser: --viewport 格式错误 %q，应为 WxH（如 1920x1080）\n", flags.viewport)
			os.Exit(exitRunErr)
		}
		// [REQ-AE-03] 误用硬拦：裸移动尺寸视口 = "假手机"（无 UA/无 touch/无 Safari
		// chrome 仿真），测不出真机问题还给假绿 —— AI-native 最大误用陷阱，error 不 warn。
		if !flags.hasPersona && w > 0 && w < 500 {
			fmt.Fprintf(os.Stderr, "dw-browser %s: 裸 --viewport %dx%d 是\"假手机\"(无 UA/touch/Safari chrome, 测不出真机问题)。\n  测移动端 → --persona mobile\n  确要裸小视口(响应式断点等) → --persona desktop --viewport %dx%d\n", cmd, w, h, w, h)
			os.Exit(exitRunErr)
		}
		flags.viewportW = w
		flags.viewportH = h
		flags.hasViewport = true
	}

	// --user-agent 单独指定（覆盖 persona 的 UA）
	if !flags.hasPersona && flags.userAgent != "" {
		flags.hasUA = true
	}

	return positional, flags
}

func parseBrowserSessionKind(raw string) (browser.BrowserSessionKind, bool) {
	switch browser.BrowserSessionKind(strings.ToLower(strings.TrimSpace(raw))) {
	case browser.SessionKindTask:
		return browser.SessionKindTask, true
	case browser.SessionKindInteractive:
		return browser.SessionKindInteractive, true
	case browser.SessionKindService:
		return browser.SessionKindService, true
	case browser.SessionKindDebug:
		return browser.SessionKindDebug, true
	case browser.SessionKindTest:
		return browser.SessionKindTest, true
	default:
		return "", false
	}
}

func parseBrowserMode(raw string) (browser.BrowserMode, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "headless":
		return browser.ModeHeadless, true
	case "headed", "standard":
		return browser.ModeHeaded, true
	case "visible", "human":
		return browser.ModeVisible, true
	default:
		return "", false
	}
}

// parseViewport 解析 "WxH" 格式。
func parseViewport(s string) (w, h int, err error) {
	parts := strings.SplitN(s, "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid viewport format")
	}
	w, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	h, err = strconv.Atoi(parts[1])
	return w, h, err
}

// resolveProfileID 根据 flags 解析 profile ID。
// --ephemeral → 自动生成 ephemeral-{pid}-{nanosec}
// --profile   → 使用指定值
// 默认        → 使用 fallback
func resolveProfileID(flags commonFlags, fallback string) string {
	if flags.ephemeral {
		return browser.NewBrowserCLIEphemeralProfileID()
	}
	if flags.profileID != "" {
		return flags.profileID
	}
	return fallback
}

func defaultProfileID(flags commonFlags) string {
	base := strings.TrimSpace(flags.sessionID)
	if base == "" {
		base = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	switch flags.sessionKind {
	case browser.SessionKindInteractive:
		return "interactive-" + base
	case browser.SessionKindService:
		account := strings.TrimSpace(flags.accountID)
		if account != "" {
			return "service-" + sanitizeProfileToken(account)
		}
		return "service-" + base
	case browser.SessionKindDebug:
		return "debug-" + base
	case browser.SessionKindTest:
		return "test-" + base
	default:
		return "task-" + base
	}
}

func sanitizeProfileToken(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "default"
	}
	return out
}

// browserOptionsFromFlags 从 flags 构建 BrowserOption 列表。
func browserOptionsFromFlags(flags commonFlags) []browser.BrowserOption {
	var opts []browser.BrowserOption
	presetID := resolveSessionPresetID(flags)
	opts = append(opts, browser.WithFingerprintPreset(presetID))
	if flags.hasViewport {
		opts = append(opts, browser.WithViewport(flags.viewportW, flags.viewportH))
	}
	if flags.hasUA || flags.userAgent != "" {
		opts = append(opts, browser.WithUserAgent(flags.userAgent))
	}
	if flags.hasPersona && flags.personaTouch {
		opts = append(opts, browser.WithTouchEmulation(true))
	}
	if flags.hasPersona {
		// 携带完整 persona → 直连路径由 applyPersonaEmulation realize Shell/Network/Env facet。
		if p, err := browser.ResolvePersona(flags.persona); err == nil {
			opts = append(opts, browser.WithPersona(p))
		}
	}
	opts = append(opts, browser.WithMode(browser.NormalizeBrowserMode(flags.mode, browser.ModeVisible)))
	if flags.stealth {
		opts = append(opts, browser.WithStealth(true))
	}
	return opts
}

// resolveSessionPresetID 为 dw-browser session/open 链路挑选一个合理的指纹 preset。
// 优先用 --persona 明确指定的身份指纹;否则据显式 UA 推断;再否则回退平台默认。
func resolveSessionPresetID(flags commonFlags) string {
	if flags.personaFingerprint != "" {
		return flags.personaFingerprint
	}
	ua := flags.userAgent
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		return browser.PresetIPhoneSafariUA
	case strings.Contains(ua, "Version/") && strings.Contains(ua, "Safari/") && strings.Contains(ua, "Macintosh") && !strings.Contains(ua, "Chrome/"):
		return browser.PresetMacOSSafariUA
	case strings.Contains(ua, "Macintosh"):
		return browser.PresetMacOSChrome
	case strings.Contains(ua, "Windows"):
		return browser.PresetWindowsChrome
	case strings.Contains(ua, "Android"):
		return browser.PresetAndroidChrome
	case strings.Contains(ua, "Linux"):
		return browser.PresetLinuxChrome
	default:
		return browser.DefaultPresetID()
	}
}

// cleanupEphemeral 清理 ephemeral profile 目录。
func cleanupEphemeral(profileID string) {
	if err := browser.RemoveBrowserCLIEphemeralProfile(profileID); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: cleanup ephemeral profile %q: %v\n", profileID, err)
	}
}

func main() {
	configureCLIInternalLogging(os.Args[1:])
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(exitRunErr)
	}

	cmd := os.Args[1]
	switch cmd {
	case "--version", "version":
		fmt.Printf("dw-browser %s\n", version)
		os.Exit(exitOK)
	case "--help", "-h", "help":
		printUsage()
		os.Exit(exitOK)
	case "session":
		runSession(os.Args[2:])
	case "once":
		runOnce(os.Args[2:])
	case "muxhost":
		runMuxHost(os.Args[2:])
	case "tabs":
		runTabs(os.Args[2:])
	case "htr":
		runHTR(os.Args[2:])
	case "open":
		runOpen(os.Args[2:])
	case "close":
		runClose(os.Args[2:])
	case "act":
		runAct(os.Args[2:])
	case "wait":
		runWait(os.Args[2:])
	case "test":
		runTest(os.Args[2:])
	case "layout":
		runLayout(os.Args[2:])
	case "eval":
		runEval(os.Args[2:])
	case "cookie-import":
		runCookieImport(os.Args[2:])
	case "profile":
		runProfile(os.Args[2:])
	case "skills":
		runSkills(os.Args[2:])
	case "record":
		runRecord(os.Args[2:])
	case "audit":
		runAudit(os.Args[2:])
	case "observe":
		runObserve(os.Args[2:])
	case "diff":
		runDiff(os.Args[2:])
	case "check":
		runCheck(os.Args[2:])
	case "journey":
		runJourney(os.Args[2:])
	case "persona", "personas":
		// AI-native 意图入口：`dw-browser persona [list]` 直达人格表（Witness 实测
		// 会先试这个拼法；报 unknown 再甩全量 help = 误导）。
		printPersonas()
	default:
		fmt.Fprintf(os.Stderr, "dw-browser: unknown command %q\n", cmd)
		printUsage()
		os.Exit(exitRunErr)
	}
}

func verboseRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--verbose" {
			return true
		}
	}
	return false
}

// configureCLIInternalLogging keeps machine-readable command output clean by
// default. Operational failures are still returned through the command's
// explicit error path; --verbose restores the internal diagnostic stream.
func configureCLIInternalLogging(args []string) {
	if verboseRequested(args) {
		obs.Init(obs.DEBUG, os.Stderr)
		log.SetOutput(os.Stderr)
		return
	}
	obs.Init(obs.ERROR, os.Stderr)
	log.SetOutput(io.Discard)
}

// printPersonas 打印可用测试保真人格列表 (--persona)。
func printPersonas() {
	fmt.Println("  ★ 测试怎么选 (AI 首选, 一眼最优):")
	fmt.Println("      测移动端 → --persona mobile     (iPhone 保真全家桶: UA/touch/DPR/视口/Safari底栏仿真)")
	fmt.Println("      测 PC 端 → --persona desktop    (平台默认桌面指纹, 保真姿态)")
	fmt.Println("")
	fmt.Println("  内置测试保真人格 (--persona):")
	fmt.Println("    说明: persona 组合 设备指纹 × in-app 壳 × 网络 × 环境;运行在 Chrome/CDP,")
	fmt.Println("          不替代真机/真微信的绝对确认(QR 真实握手)。")
	fmt.Println("    [保真 fidelity=测试用] 在前, [stealth=反检测用, 非测试保真] 在后:")
	fmt.Printf("    %-16s  %s\n", "人格名", "说明 / 视口")
	fmt.Printf("    %-16s  %s\n", "------", "-----------")
	for _, name := range browser.PersonaOrder {
		p := browser.Personas[name]
		if p == nil {
			continue
		}
		w, h, _ := p.EffectiveViewport()
		dim := ""
		if w > 0 && h > 0 {
			dim = fmt.Sprintf("%dx%d", w, h)
		}
		tag := ""
		if p.Stealth {
			tag = "  [stealth]"
		}
		fmt.Printf("    %-16s  %s  %s%s\n", name, p.Name, dim, tag)
	}
}

func wantsHelp(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" || arg == "help" {
			return true
		}
	}
	return false
}

func runSession(args []string) {
	if len(args) == 1 && wantsHelp(args) {
		printCommandUsage("session")
		os.Exit(exitOK)
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser session: requires subcommand (start|status|list|close)")
		os.Exit(exitRunErr)
	}
	switch args[0] {
	case "start":
		if wantsHelp(args[1:]) {
			printCommandUsage("session start")
			os.Exit(exitOK)
		}
		runOpen(args[1:])
	case "status":
		runSessionStatus(args[1:])
	case "list":
		runSessionList(args[1:])
	case "close":
		runClose(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "dw-browser session: unknown subcommand %q (use start|status|list|close)\n", args[0])
		os.Exit(exitRunErr)
	}
}

// runView — CHG-DWB-AGENT-CLI: 单一感知动词。
//
//	dw-browser view --id S                # 默认 brief: top-N 可操作元素 + user_state,无 a11y 树 (~瘦)
//	dw-browser view --id S --as full      # 全量: a11y 树 + 全 elements (=旧 snap 语义)
//	dw-browser view --id S --as shot      # 看图: 截图落盘,返回 {screenshot, run_id, step}
//	dw-browser view --id S --as reading   # 读正文: 纯文本
//	dw-browser view --id S --as state     # 会话/target 状态
//	dw-browser view --id S --as evidence  # 证据包: 截图 + a11y + (run_id, step)
//	dw-browser view --id S --as compact   # 仅 compact 索引元素 (= 旧 state 命令)
//
// 已废弃子命令 view action|reading|state|evidence 仍可用 (stderr 警告),下个 cycle 删除。
func runSessionStatus(args []string) {
	_, flags := parseCommonFlags(args, "session status")
	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser session status: requires --id <id>")
		os.Exit(exitRunErr)
	}
	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser session status: %v\n", err)
		os.Exit(exitRunErr)
	}
	targets := []map[string]interface{}{}
	if sessionInfo.DebugPort > 0 {
		if fetched, fetchErr := browser.FetchChromeTargets(sessionInfo.DebugPort); fetchErr == nil {
			targets = fetched
		}
	}
	output := map[string]interface{}{
		"session": sessionInfo,
		"targets": targets,
	}
	enc, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(enc))
	os.Exit(exitOK)
}

func runSessionList(args []string) {
	_, _ = parseCommonFlags(args, "session list")
	sessions, err := browser.ListSessions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser session list: %v\n", err)
		os.Exit(exitRunErr)
	}
	output := map[string]interface{}{"sessions": sessions}
	enc, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(enc))
	os.Exit(exitOK)
}

// runProfile handles `dw-browser profile list|import|prune`.
// profile list   → lists profiles under ~/.deepwork/browser-cli/
// profile import <src> [--name <name>] → copies src dir into the profile dir
// profile prune --ephemeral [--dry-run] [--min-age 30m] [--reap-orphaned] → removes stale temp sessions
// --reap-orphaned also kills any still-alive Chrome left over from a task-/
// test-/ephemeral- session whose launching CLI died without cleanup, instead
// of leaving it protected forever (see BrowserCLIEphemeralPruneOptions.ReapOrphaned).
//
// Both use ~/.deepwork/browser-cli/ — the same dir that --profile <name>
// resolves to (see resolveProfileID → browser-cli/{profileID}). Imported
// profiles must live here or --profile would never find them.
func runProfile(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser profile: requires subcommand (list|import|prune)")
		os.Exit(exitRunErr)
	}
	switch args[0] {
	case "list":
		type profileEntry struct {
			Name   string  `json:"name"`
			Path   string  `json:"path"`
			SizeMB float64 `json:"size_mb"`
		}
		profiles := []profileEntry{}
		for _, profilesDir := range browser.BrowserCLIProfileRoots("") {
			entries, err := os.ReadDir(profilesDir)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				fmt.Fprintf(os.Stderr, "dw-browser profile list: %v\n", err)
				os.Exit(exitRunErr)
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				p := filepath.Join(profilesDir, e.Name())
				var sizeBytes int64
				_ = filepath.WalkDir(p, func(_ string, d os.DirEntry, err error) error {
					if err != nil {
						return nil
					}
					if !d.IsDir() {
						if info, infoErr := d.Info(); infoErr == nil {
							sizeBytes += info.Size()
						}
					}
					return nil
				})
				profiles = append(profiles, profileEntry{
					Name:   e.Name(),
					Path:   p,
					SizeMB: float64(sizeBytes) / (1024 * 1024),
				})
			}
		}
		output := map[string]interface{}{"profiles": profiles}
		enc, _ := json.MarshalIndent(output, "", "  ")
		fmt.Println(string(enc))
		os.Exit(exitOK)

	case "prune":
		runProfilePrune(args[1:])

	case "import":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "dw-browser profile import: requires <source-path>")
			os.Exit(exitRunErr)
		}
		srcPath := args[1]
		importName := filepath.Base(srcPath)
		rest := args[2:]
		for i := 0; i < len(rest); i++ {
			switch {
			case rest[i] == "--name" && i+1 < len(rest):
				importName = rest[i+1]
				i++
			case strings.HasPrefix(rest[i], "--name="):
				importName = rest[i][len("--name="):]
			}
		}
		importName = sanitizeProfileToken(importName)
		if importName == "" || importName == "default" {
			// avoid overwriting default accidentally
			importName = "imported-" + fmt.Sprintf("%d", time.Now().Unix())
		}

		profilesDir := browser.BrowserCLIProfileRoot("")
		if err := os.MkdirAll(profilesDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser profile import: mkdir %s: %v\n", profilesDir, err)
			os.Exit(exitRunErr)
		}
		destDir := filepath.Join(profilesDir, importName)
		if _, statErr := os.Stat(destDir); statErr == nil {
			fmt.Fprintf(os.Stderr, "dw-browser profile import: profile %q already exists at %s\n", importName, destDir)
			os.Exit(exitRunErr)
		}

		cmd := exec.Command("cp", "-r", srcPath, destDir)
		if out, cpErr := cmd.CombinedOutput(); cpErr != nil {
			fmt.Fprintf(os.Stderr, "dw-browser profile import: cp -r failed: %v\n%s\n", cpErr, out)
			os.Exit(exitRunErr)
		}

		enc, _ := json.MarshalIndent(map[string]interface{}{
			"imported": true,
			"name":     importName,
			"path":     destDir,
		}, "", "  ")
		fmt.Println(string(enc))
		os.Exit(exitOK)

	default:
		fmt.Fprintf(os.Stderr, "dw-browser profile: unknown subcommand %q (use list|import|prune)\n", args[0])
		os.Exit(exitRunErr)
	}
}

func runProfilePrune(args []string) {
	minAge := time.Duration(0)
	dryRun := false
	ephemeralOnly := false
	reapOrphaned := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--ephemeral":
			ephemeralOnly = true
		case arg == "--dry-run":
			dryRun = true
		case arg == "--reap-orphaned":
			reapOrphaned = true
		case arg == "--min-age" && i+1 < len(args):
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "dw-browser profile prune: invalid --min-age %q\n", args[i+1])
				os.Exit(exitRunErr)
			}
			minAge = d
			i++
		case strings.HasPrefix(arg, "--min-age="):
			raw := strings.TrimPrefix(arg, "--min-age=")
			d, err := time.ParseDuration(raw)
			if err != nil {
				fmt.Fprintf(os.Stderr, "dw-browser profile prune: invalid --min-age %q\n", raw)
				os.Exit(exitRunErr)
			}
			minAge = d
		default:
			fmt.Fprintf(os.Stderr, "dw-browser profile prune: unknown flag %q\n", arg)
			os.Exit(exitRunErr)
		}
	}
	if !ephemeralOnly {
		fmt.Fprintln(os.Stderr, "dw-browser profile prune: requires --ephemeral")
		os.Exit(exitRunErr)
	}
	result, err := browser.PruneBrowserCLIEphemeralProfiles(browser.BrowserCLIEphemeralPruneOptions{
		MinAge:       minAge,
		DryRun:       dryRun,
		ReapOrphaned: reapOrphaned,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser profile prune: %v\n", err)
		os.Exit(exitRunErr)
	}
	enc, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(enc))
	os.Exit(exitOK)
}

func runOnce(args []string) {
	view := "action"
	url := ""
	action := ""
	outFile := "screenshot.png"
	readingFormat := "plain"
	readingWaitMs := 0
	clean := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--view" && i+1 < len(args):
			view = args[i+1]
			i++
		case strings.HasPrefix(arg, "--view="):
			view = arg[len("--view="):]
		case arg == "--url" && i+1 < len(args):
			url = args[i+1]
			i++
		case strings.HasPrefix(arg, "--url="):
			url = arg[len("--url="):]
		case arg == "--action" && i+1 < len(args):
			action = args[i+1]
			i++
		case strings.HasPrefix(arg, "--action="):
			action = arg[len("--action="):]
		case arg == "--out" && i+1 < len(args):
			outFile = args[i+1]
			i++
		case strings.HasPrefix(arg, "--out="):
			outFile = arg[len("--out="):]
		case arg == "--format" && i+1 < len(args):
			readingFormat = strings.ToLower(strings.TrimSpace(args[i+1]))
			i++
		case strings.HasPrefix(arg, "--format="):
			readingFormat = strings.ToLower(strings.TrimSpace(arg[len("--format="):]))
		case arg == "--wait" && i+1 < len(args):
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				readingWaitMs = n
			}
			i++
		case strings.HasPrefix(arg, "--wait="):
			if n, err := strconv.Atoi(arg[len("--wait="):]); err == nil {
				readingWaitMs = n
			}
		default:
			clean = append(clean, arg)
		}
	}

	positional, flags := parseCommonFlags(clean, "once")
	// --scenario 必选: once 也创建会话(即用即弃), Policy/render 由场景导出。
	_, scenarioPolicy := resolveScenario(&flags, "once")
	if url == "" && len(positional) > 0 {
		url = positional[0]
		positional = positional[1:]
	}
	if action == "" && len(positional) > 0 {
		action = positional[0]
	}
	if url == "" {
		fmt.Fprintln(os.Stderr, "dw-browser once: requires --url <url> or positional <url>")
		os.Exit(exitRunErr)
	}

	profileID := resolveProfileID(flags, defaultProfileID(flags))
	bc := newBrowserCore(profileID, browserOptionsFromFlags(flags)...)
	applyInteractionScenario(bc, flags.scenario)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// exit closes bc + cleans up an ephemeral profile before terminating.
	// os.Exit skips defers, so every exit path below must go through this
	// instead of calling os.Exit directly (see cleanupEphemeral leak fix).
	exit := func(code int) {
		bc.Close(ctx)
		if flags.ephemeral || flags.isolation == browser.SessionIsolationEphemeral {
			cleanupEphemeral(profileID)
		}
		os.Exit(code)
	}

	snap, err := navigateWithRetry(ctx, bc, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser once: navigate failed: %v\n", err)
		exit(exitRunErr)
	}
	// 进程内 open→act 路径：policy 由 scenario 导出，origin 分类用 open 后的 URL。
	bc.SetPolicy(scenarioPolicy, snap.URL)

	switch strings.ToLower(strings.TrimSpace(view)) {
	case "action":
		if action != "" {
			snap, err = actWithRetry(ctx, bc, action, true)
			if err != nil {
				fmt.Fprintf(os.Stderr, "dw-browser once: act failed: %v\n", err)
				exit(exitFail)
			}
		}
		output := map[string]interface{}{
			"view":       "action",
			"url":        snap.URL,
			"title":      snap.PageTitle,
			"summary":    snap.Text,
			"refs_count": len(snap.Refs),
			"elements":   buildRefsOutput(snap.Refs),
		}
		injectActionFidelity(output, actionFidelityReport(bc))
		injectSnapshotState(output, snap)
		enc, _ := json.MarshalIndent(output, "", "  ")
		fmt.Println(string(enc))
	case "reading":
		if readingWaitMs > 0 {
			time.Sleep(time.Duration(readingWaitMs) * time.Millisecond)
		}
		switch readingFormat {
		case "plain", "":
			text, err := bc.Text(ctx, nil)
			if err != nil {
				fmt.Fprintf(os.Stderr, "dw-browser once reading: %v\n", err)
				exit(exitRunErr)
			}
			// P2: SPA content retry — if text is very short, wait and retry once
			if len(text) < 200 {
				fmt.Fprintln(os.Stderr, "[dw-browser] reading: sparse content, retrying after 2s (SPA render lag)")
				time.Sleep(2 * time.Second)
				if retried, retryErr := bc.Text(ctx, nil); retryErr == nil && len(retried) > len(text) {
					text = retried
				}
			}
			output := map[string]interface{}{
				"view":  "reading",
				"url":   snap.URL,
				"title": snap.PageTitle,
				"text":  text,
			}
			enc, _ := json.MarshalIndent(output, "", "  ")
			fmt.Println(string(enc))
		case "list":
			var items []string
			jsExpr := `Array.from(document.querySelectorAll('li')).map(li=>li.innerText.trim()).filter(Boolean)`
			if err := bc.EvalJS(ctx, jsExpr, &items); err != nil {
				fmt.Fprintf(os.Stderr, "dw-browser once reading list: %v\n", err)
				exit(exitRunErr)
			}
			output := map[string]interface{}{
				"view":   "reading",
				"format": "list",
				"url":    snap.URL,
				"title":  snap.PageTitle,
				"items":  items,
			}
			enc, _ := json.MarshalIndent(output, "", "  ")
			fmt.Println(string(enc))
		case "table":
			var tables [][][]string
			jsExpr := `Array.from(document.querySelectorAll('table')).map(tbl=>Array.from(tbl.querySelectorAll('tr')).map(tr=>Array.from(tr.querySelectorAll('th,td')).map(td=>td.innerText.trim())))`
			if err := bc.EvalJS(ctx, jsExpr, &tables); err != nil {
				fmt.Fprintf(os.Stderr, "dw-browser once reading table: %v\n", err)
				exit(exitRunErr)
			}
			output := map[string]interface{}{
				"view":   "reading",
				"format": "table",
				"url":    snap.URL,
				"title":  snap.PageTitle,
				"tables": tables,
			}
			enc, _ := json.MarshalIndent(output, "", "  ")
			fmt.Println(string(enc))
		default:
			fmt.Fprintf(os.Stderr, "dw-browser once reading: unknown --format %q (use plain|list|table)\n", readingFormat)
			exit(exitRunErr)
		}
	case "evidence":
		data, err := screenshotWithRetry(ctx, bc, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser once evidence: %v\n", err)
			exit(exitRunErr)
		}
		if err := os.WriteFile(outFile, data, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser once evidence: write screenshot: %v\n", err)
			exit(exitRunErr)
		}
		enc, _ := json.MarshalIndent(map[string]interface{}{
			"view":  "evidence",
			"url":   snap.URL,
			"title": snap.PageTitle,
			"path":  outFile,
			"bytes": len(data),
		}, "", "  ")
		fmt.Println(string(enc))
	default:
		fmt.Fprintf(os.Stderr, "dw-browser once: unknown --view %q (use action|reading|evidence)\n", view)
		exit(exitRunErr)
	}
	exit(exitOK)
}

// printUsage 打印使用说明。
func printUsage() {
	p := func(s string) { fmt.Println(s) }
	p("dw-browser — AI agent 浏览器运行时。三种业务场景:")
	p("")
	p("  A 探索发现 (Claude agent 调): 瘦感知 + act 循环, 撞体验问题。")
	p("  B 基线回归 (测试脚本/CI 调): 确定性 pass/fail + 证据, 无 LLM。")
	p("")
	p("─── --scenario <必选> · 业务主入口 (open/once/session start 必带) ──")
	p("  Policy(公网写门控)+render(默认视图)+kind 全由场景自动导出, 不再有裸技术开关:")
	p("  app-test-explore    strict-human: 所见即可点+真鼠标/键盘; 本地写放行/公网写拦, 允许内部 LLM")
	p("  app-test-baseline   dual: 视觉默认+单步 mode:element; 硬锁内部 LLM")
	p("  webvisit            utility: 所见即可点, 元素通道允许; 全站写拦, 仅 --allow-host 放行")
	p("")
	p("─── A 探索循环 ─────────────────────────────────────────────")
	p("  open <url> --id X --scenario <场景>  打开/导航 (--scenario 必选; render 由场景定, --mode 可覆盖)")
	p("  observe --id X                      ★感知: 三场景仅列当前截图可见 elements@rN + bbox(CSS px)")
	p("      --out shot.png                  + 存截图, 返回 {screenshot:\"<path>\"} (Read 图判 UX)")
	p("      --all                           调试: 全文档 ref-less census(role/name), 不返回/持久化 @ref")
	p("      --hit-audit                     可见 ref 五点命中普查，列出 hit_coverage 警示")
	p("      --health                        + {telemetry:{console_errors,network_failures,visible_errors}}")
	p("      --tree                          + {tree:\"<全 a11y 文本>\"} (罕用)")
	p("                                      flag 自由组合 (加法, 非互斥)")
	p("  act --id X \"click @rN\"              三场景: ref 须来自上次可见集且当前仍可见; 真鼠标命中")
	p("  act --id X \"click 320,240\"           按截图所见的视口 CSS px 坐标真鼠标点击")
	p("      act --id X \"fill @r3 'hi'\" --snap   --snap 才附 a11y 全树")
	p("  close --id X                        关闭会话")
	p("")
	p("  典型循环: open → observe → (Read 截图/看 elements) → act \"click @rN\" → observe → … → close")
	p("  每条输出带 run_id (会话稳定) + step (单调) — 把截图 ↔ a11y ↔ finding 对齐。")
	p("")
	p("─── B 基线回归 ─────────────────────────────────────────────")
	p("  journey --file b.yaml --scenario app-test-baseline --evidence d/  默认视觉/真鼠标回归")
	p("      步骤可显式写 mode: element       仅该步用旧元素寻址+允许自动滚动(保真等级由剧本持有)")
	p("  check --id X --assert \"<expr>\"        单条硬断言; exit 0=PASS 1=FAIL 2=ERR")
	p("      console_errors_count==0 · exists(role='button') · text_contains('hi') · url_matches('/topic')")
	p("      count(role='link')>=1 · tab_count==2 · network_failures_count==0 · visible(testid='x')")
	p("  check --id X --file baseline.yaml     批量断言 (baseline 的 invariants 段)")
	p("  diff before.json after.json           对比两份 observe 快照 (用 observe --json 落盘)")
	p("")
	p("─── 共享 ───────────────────────────────────────────────────")
	p("  session list                         所有会话总览")
	p("  session status --id X                单会话状态")
	p("  once --url <url> [--action <act>]     无状态一次性 (开+看+关, 不留会话)")
	p("  wait --id X \"<cond>\"                  等条件: 2000(ms) / 'visible #btn' / 'gone #mask' / 'text 成功'")
	p("")
	p("─── 核心参数 ───────────────────────────────────────────────")
	p("  --scenario <场景>    ★必选 (open/once/session start): app-test-explore|app-test-baseline|webvisit")
	p("  --id <id>            会话句柄 (开一次, 后续命令全程复用)")
	p("  --mode <mode>        headless | headed | visible — 覆盖场景默认 render (CI 锁 DW_BROWSER_DEFAULT_MODE=headless)")
	p("  --allow-host <h>     webvisit 信任放行主机 (逗号分隔/可重复), 其 origin 视为本地→写放行")
	p("  --ephemeral          临时 profile, 命令结束即删 (与 --profile 互斥)")
	p("  --profile <id>       固定 profile (登录态/Human 主浏览器)")
	p("  --persona <名称>     ★测试身份入口: mobile=移动端最优 | desktop=PC最优 | 具体机型/壳 见下表")
	p("  --viewport WxH       视口覆盖 (裸移动尺寸无 persona 会被硬拦; 配 --persona desktop 可裸测)   --user-agent <ua>")
	p("  --browser-chrome <m> auto|on|off (默认 auto): iOS 移动 persona 自动仿真 Safari 底栏——截图画出遮挡带、")
	p("                       act 点遮挡区 fail-loud、observe 带 browser_chrome 机读块、audit browser-chrome-occlusion;")
	p("                       act \"zoom <n>|reset\" 进/出缩放态 (chrome 恒定不动, 复刻真机)。off 关闭; on 强制(无数据报错)")
	p("  --goal <text>        本次会话目标 (写入 contract)")
	p("  --verbose            显示内部 DEBUG/INFO/WARN 诊断日志 (默认只输出命令结果/明确错误)")
	p("")
	printPersonas()
	p("")
	p("─── 定位器 (act / check 用) ────────────────────────────────")
	p("  @r7                              上次 observe 的 ref (最稳)")
	p("  #<testid>                        按 data-testid")
	p("  button:'<名称>'                  按 ARIA role + name (contains)    button=\"<名称>\" (exact)")
	p("  role=button[name*=\"创建\"]         Canonical DSL    role=button[name=\"删除\"][nth=3] (nth 消歧)")
	p("  dialog:'创建' >> button='确认'    Scoped selector")
	p("  css=.toolbar .btn                CSS (低稳定性; canvas 容器用 css=#id)")
	p("")
	p("─── act 操作 ───────────────────────────────────────────────")
	p("  click/tap <loc>                  点击 / 触控点击")
	p("  fill <loc> '<text>'              清空后输入      type <loc> '<text>'   不清空输入")
	p("  fillsecret <loc> '<text>'        password/敏感字段的显式安全填充(CDP insertText,穿透Vue/React受控输入;值不回显)")
	p("  press <key> | press <loc> <key>  按键 (Ctrl+A, Enter)")
	p("  select <loc> '<value|label>'     下拉选择(value→label→唯一label前缀)  hover <loc> 悬停")
	p("  scroll down|up [N]               视口中心滚轮      scroll <loc> down|up [N]  对准元素滚轮")
	p("  scrollto <loc>                   显式滚到可见(之后重新 observe); scrollinto <loc> 兼容别名")
	p("  focus/check/uncheck <loc>        聚焦/勾选/取消   back | forward        导航历史")
	p("  zoom <1..5> | zoom reset         页面缩放态 (模拟捏合/聚焦放大; browser chrome 遮挡带恒定不动)")
	p("  keyboard show | keyboard hide    软键盘态 (visualViewport 收窄+resize 事件, 自适应页真实 reflow;")
	p("                                   click/fill 输入框后自动弹起=真机语义, 键盘区点击拒绝)")
	p("  坐标动作 (canvas 图表无子 DOM, 用坐标命中图元):")
	p("    click <x>,<y>                  视口 CSS px 绝对坐标点击(截图像素需先除以 dpr)")
	p("    tapxy <xf> <yf>                视口比例坐标真实点击 (不进 a11y 树的控件最通用解)")
	p("    typetext <text>                向当前焦点插文本 (配合 tapxy 聚焦自定义 input)")
	p("    clickat/dblclickat/rclickat css=#chart <x%> <y%>   元素内相对坐标点击/双击/右键")
	p("    hoverat 414,314                视口 CSS px 直接悬停 (无需 selector)")
	p("    hoverat css=#chart 50% 40%     元素内悬停触发 tooltip/十字线")
	p("    dragat css=#chart 20% 50% 80% 50%   brush 框选 / dataZoom 拖拽")
	p("    wheelat 1200,800 down 6        视口 CSS px 定点滚 6 格 (无需 selector)")
	p("    wheelat css=#chart 50% 50% -240     元素内滚轮缩放 (dy<0 放大)")
	p("    tapat/swipeat <loc> <coords>   移动端真实触控点击/滑动")
	p("")
	p("─── 高级 / 运维 (非核心循环) ───────────────────────────────")
	p("  tabs list|new|select|close --id X    同会话多标签")
	p("  htr attach|takeover|yield|share --id X   Human Trust Runtime 接管")
	p("  muxhost ensure|status|release|shutdown   全局 BrowserMuxHost 运维")
	p("  profile list|import|prune            CLI profile 管理")
	p("  skills list|read|write               站点操作知识库")
	p("  record start|stop|export --id X      录制操作轨迹 → 提炼 skill")
	p("  layout <url> · test <spec.yaml> · eval --id X \"<js>\" · cookie-import · audit")
	p("  各高级命令: dw-browser <cmd> --help")
	p("")
	p("─── 示例 ───────────────────────────────────────────────────")
	p("  dw-browser open http://localhost:8080/ws --id ws1 --scenario app-test-explore")
	p("  dw-browser observe --id ws1 --out /tmp/ws1.png --health")
	p("  dw-browser act --id ws1 \"click @r7\"")
	p("  dw-browser act --id ws1 \"fill #ws-name 'my workspace'\"")
	p("  dw-browser check --id ws1 --assert \"exists(role='button', name='创建')\"")
	p("  dw-browser close --id ws1")
	p("  dw-browser journey --file tests/bdd/portal.yaml --scenario app-test-baseline --evidence evidence/run-001")
}

func printCommandUsage(command string) {
	switch strings.TrimSpace(command) {
	case "", "dw-browser":
		printUsage()
	case "session", "session status", "session list", "session close", "close":
		fmt.Println("dw-browser session — 管理 BrowserSession 生命周期")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  dw-browser session start --id <id> --scenario webvisit --url <url>")
		fmt.Println("  dw-browser session status --id <id>")
		fmt.Println("  dw-browser session list")
		fmt.Println("  dw-browser session close --id <id>")
		fmt.Println()
		fmt.Println("说明:")
		fmt.Println("  session start 必带 --scenario；render/kind/policy 由场景导出 (webvisit → headed)。")
	case "session start", "open":
		fmt.Println("dw-browser session start — 启动或恢复一个浏览器会话")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  dw-browser session start --id <id> --scenario webvisit --profile <profile> --url <url>")
		fmt.Println("  dw-browser session start --id <id> --scenario app-test-explore --url <url>")
		fmt.Println("  dw-browser open <url> --id <id> --scenario <场景> [同等参数]")
		fmt.Println()
		fmt.Println("关键参数:")
		fmt.Println("  --scenario <场景>      ★必选: app-test-explore | app-test-baseline | webvisit")
		fmt.Println("                         Policy(公网写门控)+render+内部 kind 全由场景导出")
		fmt.Println("  --id <id>              本地 BrowserSession 句柄")
		fmt.Println("  --mode <mode>          headless/headed/visible (覆盖场景默认 render)")
		fmt.Println("  --allow-host <h>       webvisit 信任放行主机 (逗号分隔/可重复)")
		fmt.Println("  --profile <id>         dedicated profile；适合 Human 主浏览器和登录态")
		fmt.Println("  --isolation <mode>     ephemeral/dedicated/profile-pool")
		fmt.Println("  --url <url>            初始页面")
		fmt.Println("  --goal <text>          场景目标，会写入 session contract")
		fmt.Println("  --viewport WxH         视口大小")
	case "observe":
		printObserveHelp()
	case "act":
		fmt.Println("dw-browser act — 对当前会话执行语义操作")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  dw-browser act --id <id> \"click @r7\"                     # 默认瘦: {success, user_state}")
		fmt.Println("  dw-browser act --id <id> \"fill searchbox:'搜索' 'deepwork'\" --snap  # --snap 才附 a11y 全树")
		fmt.Println("  dw-browser act --id <id> \"press Enter\" --await --snap   # --await 等页面稳定后再快照")
		fmt.Println()
		fmt.Println("常用操作:")
		fmt.Println("  click @rN | click x,y | fill/type/press/hover/select/back/forward/focus/check/uncheck")
		fmt.Println("  scroll down|up [N] | scroll @rN down|up [N] | scrollto @rN (scroll 后重新 observe)")
		fmt.Println("  hoverat x,y | wheelat x,y down|up [N]              # 视口 CSS px, 无需 selector")
		fmt.Println("  元素内坐标动作(canvas): clickat/dblclickat/rclickat/hoverat/wheelat/dragat/tapat/swipeat")
		fmt.Println("  @rN 取自上次 observe; 详细定位器/动作见 dw-browser --help")
	case "check", "journey", "diff":
		printTestingHelp()
	case "tabs", "tabs list", "tabs new", "tabs select", "tabs close":
		fmt.Println("dw-browser tabs — 管理同一 BrowserSession 内的多标签页")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  dw-browser tabs list --id <id>")
		fmt.Println("  dw-browser tabs new --id <id> --url <url>")
		fmt.Println("  dw-browser tabs select --id <id> --target <index-or-target-id>")
		fmt.Println("  dw-browser tabs close --id <id> --target <index-or-target-id>")
		fmt.Println()
		fmt.Println("说明:")
		fmt.Println("  headed 模式下 select/new 会先做 BrowserMuxHost foreground guard，display 不可信时 fail-closed。")
	case "htr", "htr attach", "htr takeover", "htr yield", "htr share":
		fmt.Println("dw-browser htr — Human Trust Runtime 接管状态")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  dw-browser htr attach --id <id>")
		fmt.Println("  dw-browser htr takeover --id <id>")
		fmt.Println("  dw-browser htr yield --id <id>")
		fmt.Println("  dw-browser htr share --id <id>")
	case "muxhost", "muxhost serve", "muxhost ensure", "muxhost status", "muxhost release", "muxhost shutdown":
		fmt.Println("dw-browser muxhost — 全局 BrowserMuxHost 与 BrowserRuntime 运维")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  dw-browser muxhost ensure --id <session-id> --mode headed --profile <profile>")
		fmt.Println("  dw-browser muxhost status --id <session-id>")
		fmt.Println("  dw-browser muxhost status --runtime-id <runtime-id>")
		fmt.Println("  dw-browser muxhost status --muxhost-id browser-mux-host-global")
		fmt.Println("  dw-browser muxhost release --id <session-id>")
		fmt.Println("  dw-browser muxhost shutdown --id <session-id>")
		fmt.Println()
		fmt.Println("说明:")
		fmt.Println("  BrowserMuxHost 是全局独立控制进程；每个 BrowserRuntime cell 独立持有 profile、Chrome 与显示后端。")
	case "skills":
		fmt.Println("dw-browser skills — 管理 Browser Skill")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  dw-browser skills list")
		fmt.Println("  dw-browser skills read <name> [--action <action>]")
		fmt.Println("  dw-browser skills write <name> --action <action> < action.md")
		fmt.Println()
		fmt.Println("目录:")
		fmt.Println("  ~/.deepwork/browser-skills/<name>/SKILL.md")
	case "profile":
		fmt.Println("dw-browser profile — 管理 CLI browser profile")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  dw-browser profile list")
		fmt.Println("  dw-browser profile import <source-path> [--name <name>]")
		fmt.Println("  dw-browser profile prune --ephemeral [--dry-run] [--min-age 30m]")
	default:
		printUsage()
	}
}

func replayViewportProfile(core browser.BrowserCore, presetID string, width, height int, touch bool, cmdName string) {
	if width <= 0 || height <= 0 {
		return
	}
	presetID = browser.NormalizePresetID(presetID)
	dpr := 1.0
	mobile := touch
	if preset := browser.BuiltinPresets[presetID]; preset != nil {
		dpr = preset.DeviceScaleFactor
		mobile = preset.Mobile
	}
	if syncer, ok := core.(browser.LiveViewportSyncer); ok {
		if err := syncer.SetLiveViewport(width, height, dpr, mobile); err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser %s: viewport replay failed: %v\n", cmdName, err)
		}
	}
}

// connectSession 连接到 session 的浏览器，内置 target 自愈，并重放 session/device viewport 语义。
// 所有 session 命令统一通过此函数连接，而非直接调用 NewBrowserCoreFromSession。
// 返回 SessionCore（扩展了 BrowserCore + SnapWithSessionMode/ActWithSessionMode/RestoreRefsFromSession）。
func connectSession(ctx context.Context, sessionInfo *browser.SessionInfo, cmdName string, flags commonFlags) browser.SessionCore {
	// Safari 引擎分支
	if browser.NormalizeEngine(sessionInfo.Engine) == browser.EngineSafari {
		return connectSafariSession(ctx, sessionInfo, cmdName)
	}

	ensureSessionBrowserMuxHostReady(ctx, sessionInfo, cmdName)

	// target 自愈: 全页面导航/崩溃后 target_id 可能 stale
	targetID, err := browser.ResolveSessionTarget(sessionInfo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: target resolution failed: %v\n", cmdName, err)
		os.Exit(exitRunErr)
	}

	presetID := strings.TrimSpace(sessionInfo.PresetID)
	if flags.hasPersona || flags.hasUA {
		presetID = resolveSessionPresetID(flags)
	}
	personaID := strings.TrimSpace(sessionInfo.PersonaID)
	if flags.hasPersona {
		personaID = flags.persona
	}

	runtimeMode := browser.NormalizeBrowserMode(sessionInfo.Mode, browser.ModeVisible)
	impl, err := browser.NewBrowserCoreFromSession(ctx, sessionInfo.WSURL, targetID, presetID, personaID, runtimeMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: connect to session failed: %v\n", cmdName, err)
		os.Exit(exitRunErr)
	}
	applyInteractionScenario(impl, sessionInfo.Scenario)
	if capable, ok := impl.(browser.ActionFidelityCapable); ok {
		state := browser.HumanFocusState{}
		if sessionInfo.HumanFocusEpoch == sessionInfo.SnapEpoch && sessionInfo.HumanFocusPageURL == sessionInfo.PageURL {
			state.BackendNodeID = sessionInfo.HumanFocusBackendNodeID
		}
		capable.RestoreHumanFocus(state)
	}

	width := sessionInfo.ViewportW
	height := sessionInfo.ViewportH
	if flags.hasViewport || flags.hasPersona {
		width = flags.viewportW
		height = flags.viewportH
	}
	replayViewportProfile(impl, presetID, width, height, sessionInfo.Touch, cmdName)
	// 接线远程写策略：origin 分类的 current URL 用 session 追踪的 PageURL。
	// 若此处漏传 PageURL，lastURL 为空 → 所有 mutating act 的 origin="" 被判 remote
	// → localhost 存量行为被误阻断。故必须传 sessionInfo.PageURL。
	impl.SetPolicy(sessionInfo.Policy, sessionInfo.PageURL)
	// browser chrome 仿真：会话 open 时定死的模式，attach 路径恢复。
	// PageScale 必须经 CDP 重放 —— 上面的 replayViewportProfile(SetDeviceMetricsOverride)
	// 会把 Chrome 内的 page scale 踩回 1，只靠"驻留"缩放态会静默丢（实测抓获）。
	// 缩放态恢复不依赖仿真开关：非 sim 会话的 act "zoom" 同样要跨调用持久（评审 finding-8）。
	if bcc, ok := impl.(browser.BrowserChromeCapable); ok {
		if sessionInfo.BrowserChrome == "on" {
			bcc.EnableBrowserChromeSim()
		}
		if sessionInfo.PageScale > 0 {
			bcc.RestorePageScale(ctx, sessionInfo.PageScale)
		}
		// 视口事实重放（REQ-BC-11/12）：svh override 跨导航复位 + 上面的
		// replayViewportProfile 可能踩状态 → 每次 attach 重放（幂等）；键盘态
		// 从 SessionInfo（意图态 SSOT）推回页面。失败降级警告不阻塞命令。
		if sessionInfo.BrowserChrome == "on" {
			if err := bcc.ApplyViewportFacts(ctx, sessionInfo.Keyboard, false); err != nil {
				fmt.Fprintf(os.Stderr, "dw-browser %s: viewport facts replay degraded: %v\n", cmdName, err)
			}
		}
	}
	return impl
}

// applyInteractionScenario wires the scenario-derived interaction posture into
// CDP cores. Non-CDP engines intentionally keep their existing behavior.
func applyInteractionScenario(core browser.BrowserCore, raw string) {
	scenario, err := browser.NormalizeScenario(raw)
	if err != nil {
		return
	}
	if capable, ok := core.(browser.ScenarioInteractionCapable); ok {
		capable.SetInteractionScenario(scenario)
	}
}

func scenarioUsesSeeToClick(raw string) bool {
	scenario, err := browser.NormalizeScenario(raw)
	return err == nil && browser.ScenarioInteractionPolicy(scenario).SeeToClick
}

func actionFidelityReport(core browser.BrowserCore) browser.ActionFidelityReport {
	if capable, ok := core.(browser.ActionFidelityCapable); ok {
		return capable.LastActionFidelity()
	}
	return browser.ActionFidelityReport{}
}

func injectActionFidelity(output map[string]interface{}, report browser.ActionFidelityReport) {
	if output == nil {
		return
	}
	if report.Fidelity != "" {
		output["fidelity"] = report.Fidelity
	}
	if report.Synthetic {
		output["synthetic"] = true
		output["synthetic_note"] = report.SyntheticNote
	}
	if len(report.HumanPath) > 0 {
		output["human_path"] = report.HumanPath
	}
	if report.AimSource != "" {
		output["aim_source"] = report.AimSource
	}
	if report.HitCoverage != "" {
		output["hit_coverage"] = report.HitCoverage
	}
}

func reconcileSessionHumanFocus(info *browser.SessionInfo, report browser.ActionFidelityReport, navigated bool) {
	if info == nil {
		return
	}
	if navigated {
		info.HumanFocusBackendNodeID = 0
		info.HumanFocusPageURL = ""
		info.HumanFocusEpoch = 0
		return
	}
	if report.FocusUpdated {
		info.HumanFocusBackendNodeID = report.FocusedBackend
		if report.FocusedBackend > 0 {
			info.HumanFocusPageURL = info.PageURL
			info.HumanFocusEpoch = info.SnapEpoch
		} else {
			info.HumanFocusPageURL = ""
			info.HumanFocusEpoch = 0
		}
	}
}

// connectSafariSession 重建 Safari SessionCore（通过 UDID 重连已启动的 Simulator）。
func connectSafariSession(ctx context.Context, sessionInfo *browser.SessionInfo, cmdName string) browser.SessionCore {
	opts := safari.SafariOptions{
		UDID:        sessionInfo.DeviceUDID,
		DeviceQuery: sessionInfo.DeviceName,
	}
	core, err := safari.NewSafariBrowserCore(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: connect safari session failed: %v\n", cmdName, err)
		os.Exit(exitRunErr)
	}
	if sessionInfo.PageURL != "" {
		if _, err := core.Navigate(ctx, sessionInfo.PageURL); err != nil {
			_ = core.Close(ctx)
			fmt.Fprintf(os.Stderr, "dw-browser %s: restore safari page failed: %v\n", cmdName, err)
			os.Exit(exitRunErr)
		}
	}
	core.RestoreRefsFromSession(sessionInfo.Refs)
	// 接线远程写策略（同 chrome 分支）：origin 分类 URL 用 session 的 PageURL。
	core.SetPolicy(sessionInfo.Policy, sessionInfo.PageURL)
	return core
}

func closeSessionCore(impl browser.SessionCore) {
	if impl == nil {
		return
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer closeCancel()
	_ = impl.Close(closeCtx)
}

func exitSessionCore(impl browser.SessionCore, code int) {
	closeSessionCore(impl)
	os.Exit(code)
}

func ensureSessionBrowserMuxHostReady(ctx context.Context, sessionInfo *browser.SessionInfo, cmdName string) {
	if sessionInfo == nil || browser.NormalizeBrowserMode(sessionInfo.Mode, browser.ModeHeadless) != browser.ModeHeaded {
		return
	}
	hostCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sessionInfo.BrowserMuxHostID = browser.BrowserMuxHostIDFromBrowserSessionID(sessionInfo.BrowserSessionID)
	if sessionInfo.RuntimeID == "" {
		sessionInfo.RuntimeID = browser.BrowserRuntimeIDFromBrowserSessionID(sessionInfo.BrowserSessionID)
	}
	if state, err := browser.LoadBrowserRuntimeState(sessionInfo.RuntimeID); err == nil {
		if live, healthErr := browser.BrowserMuxHostHealth(hostCtx, state); healthErr == nil {
			if touched, touchErr := browser.TouchBrowserMuxHost(hostCtx, live, os.Getpid()); touchErr == nil {
				applyBrowserMuxHostStateToSession(sessionInfo, touched)
				_ = browser.SaveSession(sessionInfo)
				return
			}
		}
	}

	state, err := browser.EnsureBrowserMuxHost(hostCtx, browser.BrowserMuxHostRequest{
		BrowserSessionID: sessionInfo.BrowserSessionID,
		SessionKind:      sessionInfo.SessionKind,
		MuxHostID:        sessionInfo.BrowserMuxHostID,
		RuntimeID:        sessionInfo.RuntimeID,
		OwnerPID:         os.Getpid(),
		Goal:             sessionInfo.Goal,
		Owner:            sessionInfo.Owner,
		Isolation:        sessionInfo.Isolation,
		ServiceName:      sessionInfo.ServiceName,
		AccountID:        sessionInfo.AccountID,
		ProfileID:        sessionInfo.ProfileID,
		ProfileDir:       sessionInfo.ProfileDir,
		DebugPort:        sessionInfo.DebugPort,
		Mode:             sessionInfo.Mode,
		PresetID:         sessionInfo.PresetID,
		PersonaID:        sessionInfo.PersonaID,
		Width:            sessionInfo.ViewportW,
		Height:           sessionInfo.ViewportH,
		UserAgent:        sessionInfo.UserAgent,
		Touch:            sessionInfo.Touch,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: BrowserMuxHost recovery failed: %v\n", cmdName, err)
		os.Exit(exitRunErr)
	}
	applyBrowserMuxHostStateToSession(sessionInfo, state)
	sessionInfo.Refs = nil

	if targetID, terr := ensurePageTargetReady(state.WSURL, state.DebugPort); terr == nil {
		sessionInfo.TargetID = targetID
		if strings.TrimSpace(sessionInfo.PageURL) != "" {
			if impl, cerr := browser.NewBrowserCoreFromSession(hostCtx, state.WSURL, targetID, sessionInfo.PresetID, sessionInfo.PersonaID, sessionInfo.Mode); cerr == nil {
				applyInteractionScenario(impl, sessionInfo.Scenario)
				if snap, nerr := impl.Navigate(hostCtx, sessionInfo.PageURL); nerr == nil && snap != nil {
					sessionInfo.PageURL = snap.URL
					sessionInfo.Refs = browser.SessionRefsFromSnapshot(snap, false)
				}
				impl.Close(hostCtx)
			}
		}
	}
	_ = browser.SaveSession(sessionInfo)
}

func applyBrowserMuxHostStateToSession(sessionInfo *browser.SessionInfo, state *browser.BrowserMuxHostState) {
	if sessionInfo == nil || state == nil {
		return
	}
	sessionInfo.BrowserMuxHostID = state.MuxHostID
	sessionInfo.BrowserMuxHostPID = state.MuxHostPID
	sessionInfo.RuntimeID = state.RuntimeID
	sessionInfo.ChromePID = state.ChromePID
	sessionInfo.WSURL = state.WSURL
	sessionInfo.DebugPort = state.DebugPort
	sessionInfo.BrowserRunID = state.BrowserRunID
	sessionInfo.DisplayBackend = state.DisplayBackend
	sessionInfo.DisplayID = state.DisplayID
	sessionInfo.DisplayVerified = state.DisplayVerified
	sessionInfo.ChromeWindowContained = state.ChromeWindowContained
	if state.ProfileDir != "" {
		sessionInfo.ProfileDir = state.ProfileDir
	}
	if state.ProfileID != "" {
		sessionInfo.ProfileID = state.ProfileID
	}
}

func ensurePageTargetReady(wsURL string, port int) (string, error) {
	targets, err := browser.FetchChromeTargets(port)
	if err == nil {
		for _, t := range targets {
			if !browser.IsDevToolsPageTarget(t) {
				continue
			}
			if targetID := browser.ExtractDevToolsTargetID(t); targetID != "" {
				return targetID, nil
			}
		}
	}

	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), wsURL)
	defer allocCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()

	var tid cdpTarget.ID
	if err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		var createErr error
		tid, createErr = cdpTarget.CreateTarget(browser.ChromeInitialPageURL).Do(ctx)
		return createErr
	})); err != nil {
		return "", fmt.Errorf("create initial page target: %w", err)
	}
	if tid == "" {
		return "", fmt.Errorf("create initial page target: empty target id")
	}
	return string(tid), nil
}

func selectSessionTargetID(targets []map[string]interface{}, currentURL, fallbackID string) string {
	return browser.SelectAttachablePageTarget(targets, currentURL, fallbackID).ID
}

func appBundlePathForChromeExecutable(chromePath string) string {
	const marker = ".app/Contents/"
	if idx := strings.Index(chromePath, marker); idx >= 0 {
		return chromePath[:idx+4]
	}
	return ""
}

func findChromePIDForDebugPort(port int) int {
	if runtime.GOOS != "darwin" {
		return 0
	}
	out, err := exec.Command("lsof", "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		return 0
	}
	pidStr := strings.TrimSpace(string(out))
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return 0
	}
	return pid
}

// startDetachedChrome forks Chrome without Workspace ownership.
//
// CLI uses this for headless and Linux headed. visible Chrome goes through
// Workspace.LaunchChromeInSpace. macOS headed persistent sessions are rejected:
// CGVirtualDisplay must be owned by a long-lived process, otherwise the display
// disappears when dw-browser open exits and Chrome can migrate to the Human's
// main Space.
//
// [BRR-MODE-1]: visible/headed/headless 路径分离, 各自不互相污染.
func startDetachedChrome(chromePath string, chromeArgs []string) (int, error) {
	cmd := exec.Command(chromePath, chromeArgs...)
	browser.ApplyDetachedProcAttr(cmd)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start Chrome: %w", err)
	}
	return cmd.Process.Pid, nil
}

func xvfbPIDFromDisplayManager(dm *browser.DisplayManager) int {
	if dm == nil {
		return 0
	}
	return dm.XvfbPID()
}

// requireChrome 检查 Chrome 是否可用，否则退出码 2 [TC-09-U-25]。
func requireChrome() *browser.Profile {
	launcher := browser.NewChromeLauncher()
	if _, err := launcher.FindChrome(); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: %v\n", err)
		os.Exit(exitRunErr)
	}
	return nil
}

// newBrowserCore 创建 BrowserCore，失败则退出码 2。
func newBrowserCore(profileID string, opts ...browser.BrowserOption) browser.BrowserCore {
	requireChrome()

	ctx := context.Background()

	bc, err := browser.NewBrowserCore(ctx, profileID, opts...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: failed to start browser: %v\n", err)
		os.Exit(exitRunErr)
	}
	return bc
}

func isTransientBrowserStepError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context canceled") ||
		strings.Contains(msg, "target closed") ||
		strings.Contains(msg, "session closed") ||
		strings.Contains(msg, "not attached to an active page target")
}

func navigateWithRetry(ctx context.Context, bc browser.BrowserCore, url string) (*browser.Snapshot, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		snap, err := bc.Navigate(ctx, url)
		if err == nil {
			return snap, nil
		}
		lastErr = err
		if !isTransientBrowserStepError(err) || attempt == 2 {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 700 * time.Millisecond)
	}
	return nil, lastErr
}

func screenshotWithRetry(ctx context.Context, bc browser.BrowserCore, annotate bool) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		data, err := bc.Screenshot(ctx, annotate)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if !isTransientBrowserStepError(err) || attempt == 2 {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return nil, lastErr
}

func needsRefRefresh(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "backend node not found") ||
		strings.Contains(msg, "node not found for ref") ||
		strings.Contains(msg, "ref is stale") ||
		strings.Contains(msg, "selector not found") ||
		isTransientBrowserStepError(err)
}

type cliActTimeoutError struct {
	Action  string
	Timeout time.Duration
}

func (e *cliActTimeoutError) Error() string {
	return fmt.Sprintf("action %q timed out after %s (CLI watchdog)", e.Action, e.Timeout)
}

func (e *cliActTimeoutError) Unwrap() error { return context.DeadlineExceeded }

func isCLIActTimeout(err error) bool {
	var timeoutErr *cliActTimeoutError
	return errors.As(err, &timeoutErr)
}

type cliActionResult struct {
	snap *browser.Snapshot
	err  error
}

// runCLIActionWithTimeout is independent of the ActionEngine's 15s guard and
// covers alternate BrowserCore implementations with the same bounded contract.
func runCLIActionWithTimeout(
	parent context.Context,
	timeout time.Duration,
	action string,
	run func(context.Context) (*browser.Snapshot, error),
) (*browser.Snapshot, error) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		timeout = cliActTimeout
	}
	if deadline, ok := parent.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, &cliActTimeoutError{Action: action, Timeout: 0}
		}
		if remaining < timeout {
			timeout = remaining
		}
	}

	actionCtx, cancelAction := context.WithCancel(parent)
	done := make(chan cliActionResult, 1)
	go func() {
		snap, err := run(actionCtx)
		done <- cliActionResult{snap: snap, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	defer cancelAction()
	select {
	case result := <-done:
		return result.snap, result.err
	case <-parent.Done():
		if errors.Is(parent.Err(), context.DeadlineExceeded) {
			return nil, &cliActTimeoutError{Action: action, Timeout: timeout}
		}
		return nil, parent.Err()
	case <-timer.C:
		return nil, &cliActTimeoutError{Action: action, Timeout: timeout}
	}
}

func actWithRetry(ctx context.Context, bc browser.BrowserCore, action string, observe bool) (*browser.Snapshot, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		snap, err := runCLIActionWithTimeout(ctx, cliActTimeout, action, func(actionCtx context.Context) (*browser.Snapshot, error) {
			return bc.Act(actionCtx, action, observe)
		})
		if err == nil {
			return snap, nil
		}
		lastErr = err
		if !needsRefRefresh(err) || attempt == 2 {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 400 * time.Millisecond)
		_, _ = bc.Snap(ctx)
	}
	return nil, lastErr
}

func selectorCountWithRetry(ctx context.Context, bc browser.BrowserCore, selector string) (int64, error) {
	var (
		count   int64
		lastErr error
	)
	for attempt := 0; attempt < 3; attempt++ {
		count = 0
		err := bc.EvalJS(ctx, fmt.Sprintf(`document.querySelectorAll(%q).length`, selector), &count)
		if err == nil {
			return count, nil
		}
		lastErr = err
		if !isTransientBrowserStepError(err) || attempt == 2 {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 300 * time.Millisecond)
	}
	return 0, lastErr
}

// buildRefsOutput 构建 refs JSON 输出（含新字段）。
func buildRefsOutput(refs []browser.ElementRef) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(refs))
	for _, ref := range refs {
		r := map[string]interface{}{
			"ref":                 ref.Ref,
			"role":                ref.Role,
			"name_full":           ref.NameFull,
			"name_short":          ref.NameShort,
			"recommended_locator": ref.RecommendedLocator,
			"match_count":         ref.MatchCount,
		}
		if ref.Name != "" && ref.NameFull == "" {
			r["name_full"] = ref.Name
			r["name_short"] = ref.Name
		}
		if ref.TestID != "" {
			r["testid"] = ref.TestID
		}
		if ref.Placeholder != "" {
			r["placeholder"] = ref.Placeholder
		}
		// Alternatives: canonical DSL
		var alts []string
		if ref.TestID == "" && ref.Role != "" && ref.Name != "" {
			alts = append(alts, fmt.Sprintf("role=%s[name=\"%s\"]", ref.Role, ref.Name))
		}
		if len(alts) > 0 {
			r["alternatives"] = alts
		}
		out = append(out, r)
	}
	return out
}

// ============================================================
// § Agent-First 瘦输出 (CHG-DWB-AGENT-CLI)
// 设计: deepwork-pro/docs/changes/CHG-DWB-AGENT-CLI/DESIGN.md
//
// 实测 (■): SPA-scale `snap` 全量=89KB,其中 elements 数组(223 元素 ×
// 6-8 字段 pretty)=65KB(73%),a11y 树仅 8KB(9%)。瘦身正解 =
// top-N 元素 + 每元素精简字段 + 默认丢树,而非单纯丢树。
// ============================================================

const (
	defaultBriefTopN   = 20   // view --as brief 默认返回的可操作元素数
	defaultBriefBudget = 4096 // 瘦输出字节硬上限,超出截断
	// perElementBudgetEst 单元素 JSON 的字节量级估算(实测 ~90-140B)。
	// 用于显式 --top N 时把 budget 放大到真的装得下 N 个,避免"要 320 静默给 37"。
	perElementBudgetEst = 256
)

// isStructuralRole 判断是否为非操作性的结构容器 role (brief 模式跳过)。
func isStructuralRole(role string) bool {
	switch role {
	case "RootWebArea", "generic", "none", "presentation", "":
		return true
	default:
		return false
	}
}

// leanElement 把 ElementRef 序列化为最小可操作 JSON:
// 只留 ref/role/name/locator;name_full 仅当被截断时带,match 仅当 >1 时带。
// 去掉 buildRefsOutput 的 name_short==name_full 重复 + alternatives 复制 locator。
func leanElement(ref browser.ElementRef) map[string]interface{} {
	name := ref.NameShort
	if name == "" {
		name = ref.Name
	}
	locator := ref.RecommendedLocator
	if locator == "" && ref.TestID != "" {
		locator = "testid:" + ref.TestID
	}
	el := map[string]interface{}{
		"ref":     ref.Ref,
		"role":    ref.Role,
		"name":    name,
		"locator": locator,
	}
	if ref.NameFull != "" && ref.NameFull != name {
		el["name_full"] = ref.NameFull
	}
	if ref.MatchCount > 1 {
		el["match"] = ref.MatchCount
	}
	// [BUG-MODAL-FIRST] 被活跃模态遮挡 → 点了会静默失败，必须明说
	if ref.BlockedByModal {
		el["blocked"] = true
	}
	if ref.VisibilityKnown {
		el["bbox"] = map[string]float64{
			"x": ref.BBox.X,
			"y": ref.BBox.Y,
			"w": ref.BBox.Width,
			"h": ref.BBox.Height,
		}
		el["visible"] = ref.VisibleInViewport
	}
	return el
}

// modalNotice 当页面存在活跃模态层时，返回给 agent 的告警信息 [BUG-MODAL-FIRST]。
// 返回 (可交互层元素数, 被遮挡元素数, 是否有活跃模态)。
func modalNotice(refs []browser.ElementRef) (int, int, bool) {
	active, blocked := 0, 0
	for _, r := range refs {
		if r.BlockedByModal {
			blocked++
		} else if r.ModalRank > 0 {
			active++
		}
	}
	return active, blocked, active > 0 || blocked > 0
}

// briefElements 返回 top-N 可操作元素(优先 Interactable),并在 budget 内截断。
// 返回 (元素切片, 总可操作数, 是否截断)。
func briefElements(refs []browser.ElementRef, topN, budget int) ([]map[string]interface{}, int, bool) {
	if topN <= 0 {
		topN = defaultBriefTopN
	}
	if budget <= 0 {
		budget = defaultBriefBudget
	}
	interactable := make([]browser.ElementRef, 0, len(refs))
	for _, r := range refs {
		if r.Interactable && !isStructuralRole(r.Role) {
			interactable = append(interactable, r)
		}
	}
	pool := interactable
	if len(pool) == 0 {
		for _, r := range refs {
			if !isStructuralRole(r.Role) {
				pool = append(pool, r)
			}
		}
		if len(pool) == 0 {
			pool = refs
		}
	}
	total := len(pool)
	out := make([]map[string]interface{}, 0, topN)
	running := 0
	truncated := false
	for i, r := range pool {
		if i >= topN {
			truncated = true
			break
		}
		el := leanElement(r)
		b, _ := json.Marshal(el)
		if running+len(b) > budget && len(out) > 0 {
			truncated = true
			break
		}
		running += len(b)
		out = append(out, el)
	}
	if len(out) < total {
		truncated = true
	}
	return out, total, truncated
}

// sessionRunID 从 session 派生稳定的证据 run_id (CLI 表现层概念,不落引擎 schema)。
func sessionRunID(s *browser.SessionInfo) string {
	if s.BrowserSessionID != "" {
		return s.BrowserSessionID
	}
	return "run-" + s.SessionID
}

// injectEvidenceID 给输出戳上 (run_id, step) 证据关联原语。step = SnapEpoch。
func injectEvidenceID(output map[string]interface{}, s *browser.SessionInfo) {
	output["run_id"] = sessionRunID(s)
	output["step"] = s.SnapEpoch
}

// evidenceOutPath 在未显式 --out 时,把证据落盘到 runs/<run_id>/<step>-<kind>.png。
func evidenceOutPath(s *browser.SessionInfo, kind string) string {
	dir := filepath.Join(deepworkHome(), "runs", sessionRunID(s))
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, fmt.Sprintf("%04d-%s.png", s.SnapEpoch, kind))
}

// deepworkHome 返回 ~/.deepwork (证据落盘根)。
func deepworkHome() string {
	if h := os.Getenv("DEEPWORK_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "deepwork")
	}
	return filepath.Join(home, ".deepwork")
}

func injectSnapshotState(output map[string]interface{}, snap *browser.Snapshot) {
	if snap == nil {
		return
	}
	if snap.LoadState != "" {
		output["load_state"] = snap.LoadState
	}
	if snap.ReadyState != "" {
		output["ready_state"] = snap.ReadyState
	}
	if snap.Progressive {
		output["progressive"] = true
		output["retry_after_ms"] = snap.RetryAfterMillis
		if snap.ProgressReason != "" {
			output["progress_reason"] = snap.ProgressReason
		}
		if len(snap.Diagnostics) > 0 {
			output["diagnostics"] = snap.Diagnostics
		}
	}
}

// buildUserState — CHG-DWB-AGENT-CLI: 结构化 UX 状态 (SSOT)。
// 让 Agent 判断"用户能否继续交互"而非只看日志事件。act/view 共用。
func buildUserState(snap *browser.Snapshot, activeTabURL string) map[string]interface{} {
	userState := map[string]interface{}{
		"url":   snap.URL,
		"title": snap.PageTitle,
	}
	roleCounts := map[string]int{}
	inputReady := false
	clickableCount := 0
	for _, ref := range snap.Refs {
		roleCounts[ref.Role]++
		switch ref.Role {
		case "textbox", "searchbox", "combobox", "textarea":
			inputReady = true
		case "button", "link", "menuitem", "tab", "checkbox", "radio", "switch":
			clickableCount++
		}
		if ref.Role == "clickable" {
			clickableCount++
		}
	}
	userState["interactable_count"] = len(snap.Refs)
	userState["input_ready"] = inputReady
	userState["page_interactive"] = len(snap.Refs) >= 3
	if snap.SeeToClick {
		userState["page_interactive"] = len(snap.Refs) > 0
	}
	if snap.Progressive {
		userState["page_interactive"] = false
		userState["input_ready"] = false
		userState["progressive"] = true
		userState["load_state"] = snap.LoadState
		userState["retry_after_ms"] = snap.RetryAfterMillis
		if snap.ReadyState != "" {
			userState["ready_state"] = snap.ReadyState
		}
		if snap.ProgressReason != "" {
			userState["progress_reason"] = snap.ProgressReason
		}
	}
	userState["clickable_count"] = clickableCount
	var parts []string
	for role, count := range roleCounts {
		parts = append(parts, fmt.Sprintf("%d %s", count, role))
	}
	sort.Strings(parts)
	userState["interactable_summary"] = strings.Join(parts, ", ")
	userState["active_tab_url"] = activeTabURL
	return userState
}

// runSnap 执行 snap 子命令: 导航到 URL 并输出 A11y 快照。
// Session 模式: dw-browser snap --id <id>
// One-shot 模式: dw-browser snap <url> [flags]
func runAct(args []string) {
	positional, flags := parseCommonFlags(args, "act")

	// Session 模式
	if flags.sessionID != "" {
		// 解析 act 专用 flags 并从 positional 中剔除
		awaitStable := false
		snapAfterAct := false
		skillFlag := ""
		var actPositional []string
		for i := 0; i < len(positional); i++ {
			a := positional[i]
			switch {
			case a == "--await":
				awaitStable = true
			case a == "--snap":
				snapAfterAct = true
			case strings.HasPrefix(a, "--skill="):
				skillFlag = a[len("--skill="):]
			case a == "--skill" && i+1 < len(positional):
				skillFlag = positional[i+1]
				i++ // skip value
			default:
				actPositional = append(actPositional, a)
			}
		}
		if len(actPositional) < 1 {
			fmt.Fprintln(os.Stderr, "dw-browser act: requires <action> with --id")
			os.Exit(exitRunErr)
		}
		runActSession(flags, actPositional[0], awaitStable, snapAfterAct, skillFlag)
		return
	}

	// One-shot 模式（向后兼容）: 无 --id → 新建一次性会话, 故 --scenario 必选。
	if len(positional) < 2 {
		fmt.Fprintln(os.Stderr, "dw-browser act: requires <url> <action>")
		os.Exit(exitRunErr)
	}
	_, scenarioPolicy := resolveScenario(&flags, "act")
	url := positional[0]
	action := positional[1]
	profileID := resolveProfileID(flags, fmt.Sprintf("act-%d", time.Now().UnixNano()))
	browserOpts := browserOptionsFromFlags(flags)

	bc := newBrowserCore(profileID, browserOpts...)
	applyInteractionScenario(bc, flags.scenario)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// exit closes bc + cleans up an ephemeral profile before terminating.
	// os.Exit skips defers, so every exit path below must go through this
	// instead of calling os.Exit directly (see cleanupEphemeral leak fix).
	exit := func(code int) {
		bc.Close(ctx)
		if flags.ephemeral {
			cleanupEphemeral(profileID)
		}
		os.Exit(code)
	}

	navSnap, err := navigateWithRetry(ctx, bc, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: navigate failed: %v\n", err)
		exit(exitRunErr)
	}
	// 进程内 navigate→act 路径：policy 由 scenario 导出，origin 分类用导航后的 URL。
	navURL := url
	if navSnap != nil && navSnap.URL != "" {
		navURL = navSnap.URL
	}
	bc.SetPolicy(scenarioPolicy, navURL)

	observe := needsPostActionSnapshot(action)
	snap, err := actWithRetry(ctx, bc, action, observe)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: act failed: %v\n", err)
		exit(exitFail)
	}

	output := map[string]interface{}{
		"success": true,
	}
	injectActionFidelity(output, actionFidelityReport(bc))
	if snap != nil {
		output["snap"] = snap.Text
	}
	enc, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(enc))
	exit(exitOK)
}

// needsPostActionSnapshot 判断操作是否需要后置 A11y 快照。
// 仅 scroll/hover/focus/scrollinto 等纯视觉操作不需要后置快照。
// clickat/tapat 是坐标式 Human pointer 输入，常用于 LiveView/takeover。
// 其完成证据应来自 input-ack、截图和业务可见结果；默认 A11y observe
// 会让流式宿主页把一次点击放大成慢操作。
// back/forward/press 已移出排除列表：
//   - back/forward 会导致页面跳转，快照是唯一的恢复状态证据。
//   - press Enter/Tab 可能触发表单提交或页面导航，需要后置快照。
//
// [P0-α Always-Observe: 确保导航类操作产生快照，让 Agent 看到恢复状态]
func needsPostActionSnapshot(action string) bool {
	fields := strings.Fields(action)
	if len(fields) == 0 {
		return false
	}
	op := strings.ToLower(fields[0])
	switch op {
	// 纯 canvas/视觉坐标动作的完成证据来自截图，A11y 快照对 canvas 无信息量；
	// 而 rclickat（右键上下文菜单）会产生真实 DOM 弹层，仍需后置快照。
	case "scroll", "hover", "hoverat", "focus", "scrollinto", "scrollto", "clickat", "dblclickat", "wheelat", "tapat", "swipeat":
		return false
	default:
		return true
	}
}

// isPointerAction reports whether an action is a pointer/touch tap that may open a new tab.
func isPointerAction(action string) bool {
	fields := strings.Fields(action)
	if len(fields) == 0 {
		return false
	}
	switch strings.ToLower(fields[0]) {
	case "click", "clickat", "dblclickat", "tap", "tapat", "tapxy":
		return true
	default:
		return false
	}
}

// applyPostActionSnapshot persists page identity without replacing the
// capability set granted by the most recent explicit observe. A URL change is
// a navigation boundary, so refs from the previous document are revoked.
func applyPostActionSnapshot(sessionInfo *browser.SessionInfo, snap *browser.Snapshot) bool {
	if sessionInfo == nil || snap == nil {
		return false
	}
	sessionInfo.SnapEpoch++
	navigated := snap.URL != sessionInfo.PageURL
	if navigated {
		sessionInfo.Refs = nil
	}
	sessionInfo.PageURL = snap.URL
	return navigated
}

// runActSession 执行 session 模式 act（连接已有 Chrome，支持 @rN ref）。
// awaitStable: 若为 true，在操作完成后等待 500ms 再重新快照，获取导航/异步操作后的稳定状态。
// snapAfterAct: 若为 true，act 成功后强制获取快照并在输出中合并（r2 SC-19 --snap flag）。
func runActSession(flags commonFlags, action string, awaitStable bool, snapAfterAct bool, skillFlag string) {
	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser act: %v\n", err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// [BUG-FIX CAP-BS09-SESS] Snapshot known targets BEFORE action to detect new-tab opens.
	// pointer/touch 激活 link (target=_blank) → Chrome creates a new page target.
	// We record existing targets here and diff after the action to find newly opened tabs.
	knownTargetIDs := map[string]bool{}
	if isPointerAction(action) && sessionInfo.DebugPort > 0 {
		if existingTargets, err2 := browser.FetchChromeTargets(sessionInfo.DebugPort); err2 == nil {
			for _, t := range existingTargets {
				if targetID := browser.ExtractDevToolsTargetID(t); targetID != "" {
					knownTargetIDs[targetID] = true
				}
			}
		}
	}

	impl := connectSession(ctx, sessionInfo, "act", flags)

	// Restore ref table from session
	impl.RestoreRefsFromSession(sessionInfo.Refs)
	// Persist an uncertainty fence before dispatch. A process crash or watchdog
	// timeout after the browser receives input must never leave pre-action refs
	// looking current to the next CLI process. Keep the in-memory refs only for
	// this already-authorized action.
	actionFence := *sessionInfo
	actionFence.LastActionOutcome = "in_progress"
	actionFence.Refs = nil
	if err := browser.SaveSession(&actionFence); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: persist action fence: %v\n", err)
		exitSessionCore(impl, exitRunErr)
	}

	// snapAfterAct (--snap): 强制 observe=true，确保 action 成功后获取快照 [SC-19, TC-C5-11]
	observe := needsPostActionSnapshot(action) || snapAfterAct
	snap, err := runCLIActionWithTimeout(ctx, cliActTimeout, action, func(actionCtx context.Context) (*browser.Snapshot, error) {
		return impl.ActWithSessionMode(actionCtx, action, observe)
	})
	if err != nil {
		// [SC-19, TC-C5-12] act --snap: action 失败 → 不执行 snap，直接返回错误
		fmt.Fprintf(os.Stderr, "dw-browser: act failed: %v\n", err)
		sessionInfo.Refs = nil
		sessionInfo.LastActionOutcome = "unknown"
		if saveErr := browser.SaveSession(sessionInfo); saveErr != nil {
			fmt.Fprintf(os.Stderr, "dw-browser: persist unknown action outcome: %v\n", saveErr)
		}
		exitSessionCore(impl, exitFail)
	}
	// The returned snapshot is the first terminal page boundary. Commit it with
	// the action outcome before any optional stabilization sleep or auxiliary
	// recording, so another CLI process cannot observe the in-progress fence as
	// though the action never completed.
	report := actionFidelityReport(impl)
	navigated := false
	if snap != nil {
		navigated = applyPostActionSnapshot(sessionInfo, snap)
	}
	reconcileSessionHumanFocus(sessionInfo, report, navigated)
	committedSnap := snap
	sessionInfo.LastActionOutcome = "confirmed"
	if err := browser.SaveSession(sessionInfo); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: persist confirmed action outcome: %v\n", err)
		exitSessionCore(impl, exitRunErr)
	}

	// [Record tap — BS-09 Phase 2] 若当前 session 有活跃录制，追加此 act 动作为一个 step。
	// 零副作用：无录制状态文件 → 静默跳过；写入错误 → 静默忽略，不影响 act 主路径。
	appendRecordStep(flags.sessionID, action, sessionInfo.PageURL)

	// [browser-chrome] zoom 后把页面缩放镜像回 session 文件：Chrome 内的
	// Emulation 覆写跨 CLI 调用驻留，镜像不落盘则下次 observe/守卫按 1.0 折算错。
	zoomAction := strings.HasPrefix(strings.ToLower(strings.TrimSpace(action)), "zoom ")
	keyboardChanged := false
	if bcc, ok := impl.(browser.BrowserChromeCapable); ok {
		dirty := false
		if zoomAction {
			sessionInfo.PageScale = bcc.PageScale()
			dirty = true
		}
		// 键盘态可因显式 op 或焦点自动同步改变（REQ-BC-12）——每次 act 后对账。
		if kb := bcc.KeyboardVisible(); kb != sessionInfo.Keyboard {
			sessionInfo.Keyboard = kb
			keyboardChanged = true
			dirty = true
		}
		if dirty {
			_ = browser.SaveSession(sessionInfo)
		}
	}

	// [P1-α await-stable] --await: 操作完成后等待页面稳定，再重新快照。
	// 解决导航操作（back/forward/click-link）Handler 先返回 HTTP 200、页面尚未完成跳转的竞态。
	//
	// 分两种策略:
	//   - 普通 await (default): sleep 500ms → re-snap（匹配 handleBrowserGoBack 异步延迟）
	//   - press Enter await: 触发前端 fetch /api/browser/navigate，该调用同步等待 Chrome 加载页面（5-30s）。
	//     等待 10s 确保 navigate POST 有足够时间完成，再取稳定快照。
	if awaitStable {
		isPressEnter := strings.Contains(strings.ToLower(action), "press") &&
			strings.Contains(strings.ToLower(action), "enter")
		if isPressEnter {
			// press Enter 常用于触发表单提交/URL 导航，前端发起 /api/browser/navigate fetch
			// 该 fetch 同步等待 Chrome 页面加载（通常 5-8s），最长 60s 超时。
			// 等 10s 足以覆盖大多数外部 URL 首次加载，再取稳定快照。
			time.Sleep(10 * time.Second)
			if snap2, err2 := impl.SnapWithSessionMode(ctx, sessionInfo.SnapEpoch+1); err2 == nil {
				snap = snap2
			}
		} else {
			time.Sleep(500 * time.Millisecond)
			if snap2, err2 := impl.SnapWithSessionMode(ctx, sessionInfo.SnapEpoch+1); err2 == nil {
				snap = snap2
			}
		}
	}

	// A successful same-page act preserves the last explicit observation: typing
	// does not make a button the human just saw become unseen. The initial
	// post-action snapshot was committed with the terminal outcome above. If
	// --await produced a newer snapshot, reconcile that additional boundary now.
	if awaitStable && snap != nil && snap != committedSnap {
		applyPostActionSnapshot(sessionInfo, snap)
		_ = browser.SaveSession(sessionInfo)
	}
	// Scrolling changes the witness viewport without returning a fresh elements
	// list. In every human-interaction scenario, invalidate observed authority so
	// the agent must look again before clicking a ref in the new viewport.
	if scenarioUsesSeeToClick(sessionInfo.Scenario) && actionMovesWitnessViewport(action) {
		for i := range sessionInfo.Refs {
			sessionInfo.Refs[i].Observed = false
		}
		_ = browser.SaveSession(sessionInfo)
	}

	output := map[string]interface{}{
		"session_id":         sessionInfo.SessionID,
		"browser_session_id": sessionInfo.BrowserSessionID,
		"session_kind":       sessionInfo.SessionKind,
		"success":            true,
	}
	injectActionFidelity(output, report)
	if zoomAction {
		if bcc, ok := impl.(browser.BrowserChromeCapable); ok {
			output["page_scale"] = bcc.PageScale()
		}
	}
	// 键盘态变化时输出（显式 keyboard op 或焦点自动同步弹起/收起）——
	// agent 须知道视口刚变了（截图/遮挡区随之变）。
	if keyboardChanged || strings.HasPrefix(strings.ToLower(strings.TrimSpace(action)), "keyboard ") {
		if bcc, ok := impl.(browser.BrowserChromeCapable); ok {
			output["keyboard"] = bcc.KeyboardVisible()
			if keyboardChanged {
				if bcc.KeyboardVisible() {
					output["keyboard_hint"] = "软键盘已弹起(焦点自动同步/显式): visualViewport 已收窄, 底部被键盘遮挡; act \"keyboard hide\" 收起"
				} else {
					output["keyboard_hint"] = "软键盘已收起(焦点离开输入框, 真机语义): 要再截键盘态先 act \"keyboard show\""
				}
			}
		}
	}
	// [Browser Skills] --skill flag: resolve and inject skill execution context
	sc := resolveSkillContext(skillFlag, sessionInfo.PageURL, flags.sessionID)
	injectSkillFields(output, sc)

	// [SC-19, TC-C5-11] act --snap: 输出 action 结果 + snap 结果合并。
	if snapAfterAct {
		output["snap_requested"] = true
	}
	if snap != nil {
		// CHG-DWB-AGENT-CLI: act 默认瘦 — 只返 user_state。
		// a11y 全树仅当 --snap (snapAfterAct) 时附带,避免每次 act 烧 token。
		if snapAfterAct {
			output["snap"] = snap.Text
		}
		injectSnapshotState(output, snap)
		output["user_state"] = buildUserState(snap, sessionInfo.PageURL)
		injectEvidenceID(output, sessionInfo)
	}

	// [BUG-FIX CAP-BS09-SESS] Detect newly opened tab after a pointer action.
	// If a new page target appeared (not present before the action), the click opened a
	// target=_blank link. Update session TargetID so the next snap/act connects to the
	// new tab rather than the stale original tab.
	if isPointerAction(action) && sessionInfo.DebugPort > 0 {
		time.Sleep(browser.TargetPostActionDiscoveryDelay)
		if newTargets, err2 := browser.FetchChromeTargets(sessionInfo.DebugPort); err2 == nil {
			// Iterate in reverse: Chrome lists newer targets last
			for i := len(newTargets) - 1; i >= 0; i-- {
				t := newTargets[i]
				if !browser.IsDevToolsPageTarget(t) {
					continue
				}
				turl := browser.ExtractDevToolsTargetURL(t)
				if !browser.IsUserPageTargetURL(turl) {
					continue
				}
				tid := browser.ExtractDevToolsTargetID(t)
				if tid == "" {
					continue
				}
				if !knownTargetIDs[tid] {
					// New tab opened by this click → switch session to it
					sessionInfo.TargetID = tid
					sessionInfo.PageURL = turl
					sessionInfo.Refs = nil // invalidate: new page needs fresh snap
					reconcileSessionHumanFocus(sessionInfo, browser.ActionFidelityReport{}, true)
					_ = browser.SaveSession(sessionInfo)
					output["new_tab"] = map[string]interface{}{
						"target_id": tid,
						"url":       turl,
					}
					break
				}
			}
		}
	}

	enc, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(enc))
	exitSessionCore(impl, exitOK)
}

func actionMovesWitnessViewport(action string) bool {
	parsed, err := browser.ParseAction(action)
	if err != nil {
		return false
	}
	switch parsed.Op {
	case "scroll", "scrollto", "scrollinto":
		return true
	case "wheelat":
		return parsed.Ref == ""
	default:
		return false
	}
}

// ============================================================
// § runEval — P2: eval 命令 [SC-23, TC-C5-17, TC-C5-18]
// [Ref: CAP-BS09-C5 §3.2b, r2 Delta-REQ TH-0418-c9x]
// ============================================================

// runEval 执行 JavaScript 表达式并输出结果。
// 用法: dw-browser eval --id <id> "<js-expression>"
// 仅支持 session 模式（需要已有 Chrome 连接）。
func runEval(args []string) {
	positional, flags := parseCommonFlags(args, "eval")

	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser eval: requires --id <id>")
		os.Exit(exitRunErr)
	}
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser eval: requires <js-expression>")
		os.Exit(exitRunErr)
	}
	expr := positional[0]

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser eval: %v\n", err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	impl := connectSession(ctx, sessionInfo, "eval", flags)

	// EvalJS 已在 BrowserCore 接口定义，直接调用
	var result interface{}
	if err := impl.EvalJS(ctx, expr, &result); err != nil {
		fmt.Fprintf(os.Stderr, "E_EVAL_FAILED: %v\n", err)
		exitSessionCore(impl, exitFail)
	}

	// 序列化结果为 JSON
	enc, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser eval: marshal result: %v\n", err)
		exitSessionCore(impl, exitRunErr)
	}
	fmt.Println(string(enc))
	exitSessionCore(impl, exitOK)
}

// ============================================================
// § runCookieImport — P1: cookie-import 命令 [SC-22, TC-C4-07~11]
// [Ref: CAP-BS09-C4 §3.2b, r2 Delta-REQ TH-0418-c9x]
// ============================================================

// runCookieImport 从本机浏览器导入 Cookie 到当前 session。
// 用法: dw-browser cookie-import --id <id> [--browser chrome|firefox] [--domain <filter>]
func runCookieImport(args []string) {
	// Parse cookie-import specific flags
	sourceBrowser := ""
	domainFilter := ""
	var cleanArgs []string
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--browser" && i+1 < len(args):
			sourceBrowser = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--browser="):
			sourceBrowser = arg[len("--browser="):]
			i++
		case arg == "--domain" && i+1 < len(args):
			domainFilter = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--domain="):
			domainFilter = arg[len("--domain="):]
			i++
		default:
			cleanArgs = append(cleanArgs, arg)
			i++
		}
	}

	_, flags := parseCommonFlags(cleanArgs, "cookie-import")

	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser cookie-import: requires --id <id>")
		os.Exit(exitRunErr)
	}

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser cookie-import: %v\n", err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	impl := connectSession(ctx, sessionInfo, "cookie-import", flags)

	importer := browser.NewCookieImporter(impl)
	result, err := importer.Import(ctx, sourceBrowser, domainFilter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser cookie-import: %v\n", err)
		if err == browser.ErrCookieDecryptFailed {
			fmt.Fprintln(os.Stderr, "提示: 请在系统设置中授权 Terminal 访问 Keychain")
		} else if err == browser.ErrCookieDBLocked {
			fmt.Fprintln(os.Stderr, "提示: Cookie 数据库锁定，请关闭源浏览器后重试")
		}
		exitSessionCore(impl, exitRunErr)
	}

	output := map[string]interface{}{
		"total_imported": result.TotalImported,
		"source_browser": result.SourceBrowser,
		"by_domain":      result.ByDomain,
	}
	enc, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(enc))
	fmt.Printf("已导入 %d 个 Cookie（来源: %s）\n", result.TotalImported, result.SourceBrowser)
	exitSessionCore(impl, exitOK)
}

// runOpenSafari 使用 Safari 引擎打开 URL，持久化 session。
// 由 runOpen 在 --engine safari 时调用。
func runOpenSafari(url string, flags commonFlags, scenarioPolicy browser.SessionPolicy) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	deviceQuery := flags.safariDevice
	opts := safari.SafariOptions{
		DeviceQuery: deviceQuery,
	}
	core, err := safari.NewSafariBrowserCore(ctx, opts)
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "session not created") || strings.Contains(errMsg, "not trusted") {
			fmt.Fprintln(os.Stderr, "Safari WebDriver not enabled. Run: sudo safaridriver --enable")
		}
		fmt.Fprintf(os.Stderr, "dw-browser open (safari): init failed: %v\n", err)
		os.Exit(exitRunErr)
	}
	closeSafari := func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer closeCancel()
		_ = core.Close(closeCtx)
	}

	openSnap, err := core.Navigate(ctx, url)
	if err != nil {
		closeSafari()
		fmt.Fprintf(os.Stderr, "dw-browser open (safari): navigate failed: %v\n", err)
		os.Exit(exitRunErr)
	}

	currentURL := url
	if openSnap != nil && openSnap.URL != "" {
		currentURL = openSnap.URL
	}

	openRefs := browser.SessionRefsFromSnapshot(openSnap, false)

	sessionInfo := &browser.SessionInfo{
		SessionID:   flags.sessionID,
		SessionKind: flags.sessionKind,
		Goal:        flags.goal,
		Owner:       flags.owner,
		Isolation:   flags.isolation,
		ServiceName: flags.serviceName,
		AccountID:   flags.accountID,
		PageURL:     currentURL,
		CreatedAt:   time.Now().Format(time.RFC3339),
		SnapEpoch:   0,
		Refs:        openRefs,
		Ephemeral:   flags.ephemeral || flags.isolation == browser.SessionIsolationEphemeral,
		Engine:      browser.EngineSafari,
		DeviceUDID:  core.DeviceUDID(),
		DeviceName:  core.DeviceName(),
		Scenario:    flags.scenario,
		Policy:      scenarioPolicy,
	}
	if err := browser.SaveSession(sessionInfo); err != nil {
		closeSafari()
		fmt.Fprintf(os.Stderr, "dw-browser open (safari): save session: %v\n", err)
		os.Exit(exitRunErr)
	}

	output := map[string]interface{}{
		"session_id":   flags.sessionID,
		"engine":       "safari",
		"device_udid":  core.DeviceUDID(),
		"device_name":  core.DeviceName(),
		"url":          currentURL,
		"session_kind": flags.sessionKind,
	}
	if openSnap != nil {
		interactableCount := 0
		clickableCount := 0
		inputCount := 0
		for _, ref := range openSnap.Refs {
			if ref.Interactable {
				interactableCount++
				switch ref.Role {
				case "button", "link", "menuitem", "tab", "checkbox", "radio":
					clickableCount++
				case "textbox", "searchbox", "combobox":
					inputCount++
				}
			}
		}
		userState := map[string]interface{}{
			"url":                openSnap.URL,
			"title":              openSnap.PageTitle,
			"page_interactive":   interactableCount > 0,
			"interactable_count": interactableCount,
			"clickable_count":    clickableCount,
			"input_count":        inputCount,
			"refs_count":         len(openSnap.Refs),
			"load_state":         openSnap.LoadState,
			"snapshot_type":      openSnap.SnapshotType,
		}
		output["user_state"] = userState
	}
	closeSafari()
	enc, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(enc))
	os.Exit(exitOK)
}

// runOpen 启动浏览器，导航到 URL，持久化 session。
// 支持 Chrome (默认) 和 Safari (--engine safari) 引擎。
func runOpen(args []string) {
	positional, flags := parseCommonFlags(args, "open")
	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser open: requires --id <id>")
		os.Exit(exitRunErr)
	}
	// --scenario 是 session-creating 命令的必选业务主入口: Policy/render/kind 全由它导出。
	_, scenarioPolicy := resolveScenario(&flags, "open")
	if flags.url != "" {
		positional = append([]string{flags.url}, positional...)
	}
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser open: requires <url>")
		os.Exit(exitRunErr)
	}
	url := positional[0]

	// Safari 引擎分支：完全绕开 Chrome/CDP 路径
	if flags.engine == "safari" {
		if flags.browserChrome == browser.BrowserChromeOn {
			fmt.Fprintln(os.Stderr, "dw-browser open: --browser-chrome=on 不适用于 --engine safari（真机 Simulator 自带真实 Safari chrome）")
			os.Exit(exitRunErr)
		}
		runOpenSafari(url, flags, scenarioPolicy)
		return
	}

	sessionPresetID := resolveSessionPresetID(flags)
	sessionPersonaID := flags.persona // 与 preset 同源:随会话持久化 + 传 attach 路径
	browserSessionID := browser.BrowserSessionIDFromSessionID(flags.sessionID)

	// browser chrome 仿真判定（spec: docs/product/browser-chrome/）。
	// auto 默认：移动预设带 chrome 几何 → 启用；启用即"双视口显形"——会话视口
	// 高开到大视口 lvh，底部 [svh,lvh) 为 Safari 底栏遮挡带，100vh 类布局 bug
	// 与真机同构显形。会话级模式，open 时定死，中途不漂。
	browserChromeOn := false
	{
		chromePreset := browser.BuiltinPresets[browser.NormalizePresetID(sessionPresetID)]
		on, bcErr := browser.ResolveBrowserChromeMode(flags.browserChrome, chromePreset, flags.viewport != "")
		if bcErr != nil {
			fmt.Fprintf(os.Stderr, "dw-browser open: %v\n", bcErr)
			os.Exit(exitRunErr)
		}
		if on {
			browserChromeOn = true
			flags.viewportH = chromePreset.BrowserChrome.LargeViewportH()
			flags.hasViewport = true
		} else if chromePreset != nil && chromePreset.BrowserChrome != nil && flags.viewport != "" && flags.browserChrome != browser.BrowserChromeOff {
			// [评审 finding-7] mobile 类 persona 叠显式 --viewport 会静默失去 chrome
			// 仿真承诺 —— 如实提示，不静默。
			fmt.Fprintln(os.Stderr, "dw-browser open: 提示: 显式 --viewport 已关闭该 persona 的 Safari chrome 仿真(几何归设备); 需要仿真请去掉 --viewport")
		}
	}

	// Find Chrome binary
	launcher := browser.NewChromeLauncher()
	chromePath, err := launcher.FindChrome()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser open: %v\n", err)
		os.Exit(exitRunErr)
	}

	// Find free port
	port, err := browser.FindFreePort()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser open: %v\n", err)
		os.Exit(exitRunErr)
	}

	// Resolve profile dir. Snap Chromium is routed through its writable common dir.
	profileID := resolveProfileID(flags, defaultProfileID(flags))
	if pruneResult, pruneErr := browser.PruneBrowserCLIEphemeralProfiles(browser.BrowserCLIEphemeralPruneOptions{
		ChromePath:   chromePath,
		MinAge:       browser.DefaultCLIEphemeralPruneMinAge,
		ReapOrphaned: true,
	}); pruneErr != nil {
		fmt.Fprintf(os.Stderr, "dw-browser open: ephemeral profile prune skipped: %v\n", pruneErr)
	} else if pruneResult.Removed > 0 {
		fmt.Fprintf(os.Stderr, "dw-browser open: pruned %d stale ephemeral profile(s), freed %.1f MB\n",
			pruneResult.Removed, float64(pruneResult.Bytes)/(1024*1024))
	}
	profileDir := browser.BrowserCLIProfileDir(chromePath, profileID)
	mode := browser.NormalizeBrowserMode(flags.mode, browser.ModeVisible)

	// cleanup tears down whatever Chrome/Xvfb/BrowserMuxHost open has actually
	// started so far; it grows as each resource comes up and is disarmed once
	// SaveSession succeeds (open's contract is "launch, hand off, exit — the
	// browser outlives the CLI", so success must NOT tear anything down).
	// exit routes every remaining exit through it — os.Exit skips defers, so
	// without this, any failure between starting Chrome and SaveSession
	// leaked the whole stack with no session file to ever find it by.
	cleanup := func() {}
	exit := func(code int) {
		cleanup()
		os.Exit(code)
	}
	// removeEphemeralProfileDirIfNeeded reclaims profileDir immediately
	// instead of leaving it for the next scheduled prune pass (up to
	// DefaultCLIEphemeralPruneMinAge later) — we already know for certain
	// it's ephemeral, so there's no reason to wait. Every mode's cleanup
	// closure calls this last, after the process it owns is confirmed dead
	// (via browser.KillChromeProcessGroup, not a single-PID kill — a plain
	// proc.Kill() only kills the leader and leaves zygote/renderer/gpu-
	// process/crashpad_handler alive, which keep writing into profileDir and
	// silently resurrect it seconds after RemoveAll reports success).
	removeEphemeralProfileDirIfNeeded := func() {
		if flags.ephemeral || flags.isolation == browser.SessionIsolationEphemeral {
			_ = os.RemoveAll(profileDir)
		}
	}

	// Phase_v2_4 startup recovery (CAP-BS09-C4 §3.5) — 与 BrowserPool 共享 4 步协议:
	//   1. SingletonLock 残留检测 (orphan PID 强杀, 避免上次 CLI 崩溃留下的 chrome 抢锁)
	//   2. profile health check (Cookies SQLite header 16 字节校验)
	//   3. 损坏 → {profileDir}.broken/{UTC ts}/ 隔离 (Chrome 自动重建空 profile)
	// 注: 跨进程语义 (SaveSession/LoadSession) 仍归 session_manager; recovery 仅是低层共享.
	ownerKey := browser.IdentityKey("dw-cli-" + browserSessionID)
	if mode != browser.ModeHeaded {
		if err := browser.RunStartupRecovery(profileDir, ownerKey); err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser open: startup recovery: %v\n", err)
			exit(exitRunErr)
		}
	}

	if err := os.MkdirAll(profileDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser open: mkdir profile: %v\n", err)
		exit(exitRunErr)
	}

	// Viewport
	width, height := 1920, 1080
	if flags.hasViewport {
		width, height = flags.viewportW, flags.viewportH
	} else if flags.hasPersona {
		width, height = flags.viewportW, flags.viewportH
	}

	var dm *browser.DisplayManager
	displayBackend := "none"
	switch mode {
	case browser.ModeHeadless:
	case browser.ModeHeaded:
		displayBackend = "browser-mux-host"
	case browser.ModeVisible:
		displayBackend = "native"
		if runtime.GOOS == "linux" {
			dm = &browser.DisplayManager{}
			if !dm.EnsureDisplay() {
				fmt.Fprintln(os.Stderr, "dw-browser open: visible mode unavailable on linux: Xvfb display setup failed")
				exit(exitRunErr)
			}
			displayBackend = "xvfb"
			cleanup = func() {
				dm.Close()
				removeEphemeralProfileDirIfNeeded()
			}
		}
	default:
		fmt.Fprintf(os.Stderr, "dw-browser open: unsupported browser mode %q\n", mode)
		exit(exitRunErr)
	}

	chromeArgs := browser.BuildDetachedChromeArgs(browser.DetachedChromeLaunchOptions{
		DebugPort:  port,
		ProfileDir: profileDir,
		Width:      width,
		Height:     height,
		PresetID:   sessionPresetID,
		UserAgent:  flags.userAgent,
		Touch:      flags.personaTouch,
		Mode:       mode,
		Stealth:    flags.stealth,
	})

	// Mode-conditional Chrome launch (DDC-I-21, BRR-12, BRR-MODE-1):
	//   - visible → Workspace.LaunchChromeInSpace (D1 序列保证窗口绑定到隔离 Space)
	//   - headed  → BrowserMuxHost 成对持有 DisplayHost + Chrome，CLI/Deepwork 只 attach
	//   - headless → 直接 exec (无窗口 → 无需 Space 绑定 → 不触碰 Workspace)
	// Workspace 是 visible Chrome 启动 SSOT, headed/headless 完全不经过 Workspace.
	var (
		chromePID int
		wsURL     string
		hostState *browser.BrowserMuxHostState
	)
	if mode == browser.ModeHeaded {
		hostReq := browser.BrowserMuxHostRequest{
			BrowserSessionID: browserSessionID,
			SessionKind:      flags.sessionKind,
			OwnerPID:         os.Getpid(),
			Goal:             flags.goal,
			Owner:            flags.owner,
			Isolation:        flags.isolation,
			ServiceName:      flags.serviceName,
			AccountID:        flags.accountID,
			ChromePath:       chromePath,
			ProfileID:        profileID,
			ProfileDir:       profileDir,
			DebugPort:        port,
			Mode:             mode,
			PresetID:         sessionPresetID,
			PersonaID:        sessionPersonaID,
			Width:            width,
			Height:           height,
			UserAgent:        flags.userAgent,
			Touch:            flags.personaTouch,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		var err error
		hostState, err = browser.EnsureBrowserMuxHost(ctx, hostReq)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser open: ensure BrowserMuxHost: %v\n", err)
			exit(exitRunErr)
		}
		chromePID = hostState.ChromePID
		wsURL = hostState.WSURL
		port = hostState.DebugPort
		displayBackend = hostState.DisplayBackend
		// Runtime-scoped: ShutdownBrowserMuxHost always sends this state's
		// RuntimeID, so the shared muxhost daemon (GlobalBrowserMuxHostID —
		// one process multiplexes every headed session on the machine) only
		// closes *this* runtime, not the whole daemon or any other session
		// attached to it.
		cleanup = func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 8*time.Second)
			_, _ = browser.ShutdownBrowserMuxHost(shutdownCtx, hostState)
			shutdownCancel()
			removeEphemeralProfileDirIfNeeded()
		}
	} else if mode == browser.ModeVisible {
		ws := browser.NewWorkspace()
		cleanup = func() { ws.Close() }
		if spaceID, wsErr := ws.EnsureSpace(); wsErr != nil {
			fmt.Fprintf(os.Stderr, "[workspace] EnsureSpace: %v (Chrome will appear on current Space)\n", wsErr)
		} else if spaceID > 0 {
			fmt.Fprintf(os.Stderr, "[workspace] using isolation Space %d\n", spaceID)
		}
		handle, err := ws.LaunchChromeInSpace(browser.ChromeLaunchSpec{
			ChromePath:   chromePath,
			Args:         chromeArgs,
			DebugPort:    port,
			ReadyTimeout: 120 * time.Second,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser open: workspace launch chrome: %v\n", err)
			exit(exitRunErr)
		}
		chromePID = handle.PID()
		wsURL = handle.WSURL()
		// Note: 不 Kill handle — dw-browser open 语义为"启动后退出, Chrome 继续运行"
		// on the SUCCESS path (cleanup is disarmed once SaveSession succeeds,
		// see below). Setpgid 已切断 process group, CLI 退出后 Chrome 不受影响.
		prevCleanup := cleanup
		cleanup = func() {
			prevCleanup()
			browser.KillChromeProcessGroup(chromePID)
			removeEphemeralProfileDirIfNeeded()
		}
	} else {
		var err error
		chromePID, err = startDetachedChrome(chromePath, chromeArgs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser open: %v\n", err)
			exit(exitRunErr)
		}
		cleanup = func() {
			browser.KillChromeProcessGroup(chromePID)
			removeEphemeralProfileDirIfNeeded()
		}
		wsURL, err = browser.WaitForChromeReady(port, 120*time.Second)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser open: %v\n", err)
			exit(exitRunErr)
		}
		if chromePID == 0 {
			chromePID = findChromePIDForDebugPort(port)
		}
	}
	browserRunID := browser.NewBrowserRunID(browserSessionID, chromePID)
	if hostState != nil {
		browserRunID = hostState.BrowserRunID
	} else {
		if err := browser.WriteProfileOwnerMarkerWithMetadata(profileDir, ownerKey, chromePID, port, browser.ProfileOwnerMetadata{
			BrowserSessionID:      browserSessionID,
			SessionKind:           flags.sessionKind,
			BrowserRunID:          browserRunID,
			ProfileID:             profileID,
			DisplayBackend:        displayBackend,
			DisplayVerified:       displayBackend == "none" || displayBackend == "xvfb" || displayBackend == "native",
			ChromeWindowContained: displayBackend == "none" || displayBackend == "xvfb" || displayBackend == "native",
		}); err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser open: write profile owner marker: %v\n", err)
			exit(exitRunErr)
		}
	}

	// Connect via chromedp and navigate
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	initialTargetID, err := ensurePageTargetReady(wsURL, port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser open: ensure page target: %v\n", err)
		exit(exitRunErr)
	}

	impl, err := browser.NewBrowserCoreFromSession(ctx, wsURL, initialTargetID, sessionPresetID, sessionPersonaID, mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser open: connect: %v\n", err)
		exit(exitRunErr)
	}
	applyInteractionScenario(impl, flags.scenario)
	defer impl.Close(ctx)
	replayViewportProfile(impl, sessionPresetID, width, height, flags.personaTouch, "open")
	if browserChromeOn {
		if bcc, ok := impl.(browser.BrowserChromeCapable); ok {
			if !bcc.EnableBrowserChromeSim() {
				fmt.Fprintf(os.Stderr, "dw-browser open: enable browser-chrome sim failed for preset %q\n", sessionPresetID)
				exit(exitRunErr)
			}
			// 视口事实（REQ-BC-11）：shim 须在首次导航前注册（install=true 仅此
			// 一处），页面脚本加载即读到真机同构的 vv/innerHeight；svh override
			// 在导航提交时复位，由 frameNavigated 监听 + Navigate 钩子重放——
			// 首屏存在毫秒级窗口（页面 document-start 一次性读 100svh 且缓存的
			// 场景可能取到 lvh 旧值；CSS 用法随重放自动重算）。建立失败 =
			// 会话保真度谎言源头，fail-loud。
			if err := bcc.ApplyViewportFacts(ctx, false, true); err != nil {
				fmt.Fprintf(os.Stderr, "dw-browser open: establish viewport facts: %v\n", err)
				exit(exitRunErr)
			}
		}
	}

	openSnap, err := impl.Navigate(ctx, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser open: navigate: %v\n", err)
		exit(exitRunErr)
	}

	// Get current page URL after navigation
	var currentURL string
	if openSnap != nil && openSnap.URL != "" {
		currentURL = openSnap.URL
	} else {
		impl.EvalJS(ctx, "window.location.href", &currentURL)
	}
	if currentURL == "" {
		currentURL = url
	}

	// Get target ID from Chrome targets.
	// Root rule: prefer the target that actually navigated to currentURL.
	// Fallback to the initial page target used for this session instead of
	// blindly taking the first page target, otherwise multi-tab/human-mode
	// Chrome may persist ChromeInitialPageURL as the session target.
	targets, err := browser.FetchChromeTargets(port)
	targetID := initialTargetID
	if err == nil && len(targets) > 0 {
		targetID = selectSessionTargetID(targets, currentURL, initialTargetID)
	}

	browserChromeState := ""
	if browserChromeOn {
		browserChromeState = "on"
	}

	// 保存 open 的可见定位事实用于诊断/element-mode；Observed 保持 false。
	// 三个 agent 场景的默认 click 仍必须先显式 observe，不能把 open 的内部
	// snapshot 当成 agent 已经看见的证据。
	openRefs := browser.SessionRefsFromSnapshot(openSnap, false)

	sessionInfo := &browser.SessionInfo{
		SessionID:             flags.sessionID,
		BrowserSessionID:      browserSessionID,
		SessionKind:           flags.sessionKind,
		Goal:                  flags.goal,
		Owner:                 flags.owner,
		AuthorityState:        browser.DefaultsForBrowserSessionKind(flags.sessionKind).AuthorityState,
		ProfileID:             profileID,
		Isolation:             flags.isolation,
		ServiceName:           flags.serviceName,
		AccountID:             flags.accountID,
		ChromePID:             chromePID,
		WSURL:                 wsURL,
		DebugPort:             port,
		TargetID:              targetID,
		Mode:                  mode,
		PresetID:              sessionPresetID,
		PersonaID:             sessionPersonaID,
		ProfileDir:            profileDir,
		PageURL:               currentURL,
		Scenario:              flags.scenario,
		Policy:                scenarioPolicy,
		CreatedAt:             time.Now().Format(time.RFC3339),
		ViewportW:             width,
		ViewportH:             height,
		UserAgent:             flags.userAgent,
		Touch:                 flags.personaTouch,
		SnapEpoch:             0,
		Refs:                  openRefs,
		Ephemeral:             flags.ephemeral || flags.isolation == browser.SessionIsolationEphemeral,
		XvfbPID:               xvfbPIDFromDisplayManager(dm),
		BrowserChrome:         browserChromeState,
		PageScale:             1,
		BrowserRunID:          browserRunID,
		DisplayBackend:        displayBackend,
		DisplayVerified:       displayBackend == "none" || displayBackend == "xvfb" || displayBackend == "native",
		ChromeWindowContained: displayBackend == "none" || displayBackend == "xvfb" || displayBackend == "native",
	}
	if hostState != nil {
		sessionInfo.BrowserMuxHostID = hostState.MuxHostID
		sessionInfo.BrowserMuxHostPID = hostState.MuxHostPID
		sessionInfo.RuntimeID = hostState.RuntimeID
		sessionInfo.DisplayBackend = hostState.DisplayBackend
		sessionInfo.DisplayID = hostState.DisplayID
		sessionInfo.DisplayVerified = hostState.DisplayVerified
		sessionInfo.ChromeWindowContained = hostState.ChromeWindowContained
	}
	if err := browser.SaveSession(sessionInfo); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser open: save session: %v\n", err)
		exit(exitRunErr)
	}
	// Handoff complete: the session file is now the source of truth for
	// closing this Chrome/display/muxhost (dw-browser close reads it back).
	// Disarm cleanup so nothing below this point can ever tear down what we
	// just successfully handed off.
	cleanup = func() {}

	output := map[string]interface{}{
		"session_id":         flags.sessionID,
		"browser_session_id": browserSessionID,
		"session_kind":       flags.sessionKind,
		"profile_id":         profileID,
		"mode":               mode,
		"isolation":          flags.isolation,
		"url":                currentURL,
		"pid":                chromePID,
		"port":               port,
		"ws_url":             wsURL,
	}
	if browserChromeOn {
		output["browser_chrome"] = "on"
		output["browser_chrome_hint"] = fmt.Sprintf("Safari chrome 仿真已启用: 视口 %dx%d(大视口), 底部 y>=%d 为底栏遮挡带(截图可见/act 点击拒绝); 页面感知的 visualViewport/innerHeight/100svh=%d(真机同构, 自适应页真实 reflow); act \"zoom <n>\" 缩放态 / act \"keyboard show\" 软键盘态(点输入框自动弹起)", width, height, browser.BuiltinPresets[browser.NormalizePresetID(sessionPresetID)].BrowserChrome.SmallViewportH(), browser.BuiltinPresets[browser.NormalizePresetID(sessionPresetID)].BrowserChrome.SmallViewportH())
	}
	if hostState != nil {
		output["browser_mux_host_id"] = hostState.MuxHostID
		output["browser_mux_host_pid"] = hostState.MuxHostPID
		output["display_backend"] = hostState.DisplayBackend
		output["display_verified"] = hostState.DisplayVerified
	}
	// [P0-α user_state] open 命令也提供结构化 UX 状态，便于 Agent 验证初始页面可交互性。
	if openSnap != nil {
		userState := map[string]interface{}{
			"url":   openSnap.URL,
			"title": openSnap.PageTitle,
		}
		roleCounts := map[string]int{}
		inputReady := false
		clickableCount := 0
		for _, ref := range openSnap.Refs {
			roleCounts[ref.Role]++
			switch ref.Role {
			case "textbox", "searchbox", "combobox", "textarea":
				inputReady = true
			case "button", "link", "menuitem", "tab", "checkbox", "radio", "switch":
				clickableCount++
			}
			if ref.Role == "clickable" {
				clickableCount++
			}
		}
		userState["interactable_count"] = len(openSnap.Refs)
		userState["input_ready"] = inputReady
		userState["page_interactive"] = len(openSnap.Refs) >= 3
		userState["clickable_count"] = clickableCount
		var parts []string
		for role, count := range roleCounts {
			parts = append(parts, fmt.Sprintf("%d %s", count, role))
		}
		sort.Strings(parts)
		userState["interactable_summary"] = strings.Join(parts, ", ")
		output["user_state"] = userState
	}
	enc, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(enc))
	exit(exitOK)
}

// runClose 关闭 session（杀 Chrome 进程，删 session 文件）。
func runClose(args []string) {
	_, flags := parseCommonFlags(args, "close")
	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser close: requires --id <id>")
		os.Exit(exitRunErr)
	}

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser close: %v\n", err)
		os.Exit(exitRunErr)
	}

	hostShutdown := false
	runtimeID := strings.TrimSpace(sessionInfo.RuntimeID)
	if runtimeID == "" && strings.TrimSpace(sessionInfo.BrowserSessionID) != "" {
		runtimeID = browser.BrowserRuntimeIDFromBrowserSessionID(sessionInfo.BrowserSessionID)
	}
	if runtimeID != "" {
		if state, loadErr := browser.LoadBrowserRuntimeState(runtimeID); loadErr == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			if _, shutdownErr := browser.ShutdownBrowserMuxHost(ctx, state); shutdownErr == nil {
				hostShutdown = true
			} else {
				fmt.Fprintf(os.Stderr, "dw-browser close: BrowserMuxHost shutdown fallback: %v\n", shutdownErr)
			}
			cancel()
		} else if sessionInfo.BrowserMuxHostID != "" {
			fmt.Fprintf(os.Stderr, "dw-browser close: BrowserMuxHost state not found: %v\n", loadErr)
		}
	}

	// Kill Chrome process when the session is not BrowserMuxHost-owned, or when
	// BrowserMuxHost shutdown was unavailable and we must fail closed.
	// Whole-process-group kill (see KillChromeProcessGroup doc): a plain
	// single-PID kill leaves zygote/renderer/gpu-process/crashpad_handler
	// alive, which keep writing into the profile dir and undo the cleanup
	// below moments later.
	if !hostShutdown && sessionInfo.ChromePID > 0 {
		browser.KillChromeProcessGroup(sessionInfo.ChromePID)
	}
	if !hostShutdown && sessionInfo.ProfileDir != "" {
		browser.RemoveProfileOwnerMarker(sessionInfo.ProfileDir, browser.IdentityKey("dw-cli-"+sessionInfo.BrowserSessionID))
	}

	// Kill Xvfb process (human mode cleanup)
	if sessionInfo.XvfbPID > 0 {
		proc, err := os.FindProcess(sessionInfo.XvfbPID)
		if err == nil {
			_ = proc.Kill()
		}
	}

	// Delete session file
	if err := browser.DeleteSession(flags.sessionID); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser close: delete session: %v\n", err)
	}

	// Clean up profile dir if ephemeral (from session info or close flag).
	// Driven by SessionInfo.Ephemeral, not by directory-name prefix: the
	// default "task" session kind is policy-classified ephemeral
	// (session_contract.go) but its profile dir is named "task-<id>", so a
	// prefix check here would silently never delete it.
	if sessionInfo.Ephemeral || flags.ephemeral {
		if sessionInfo.ProfileDir != "" {
			_ = os.RemoveAll(sessionInfo.ProfileDir)
		}
		if sessionInfo.ProfileID != "" {
			_ = browser.RemoveBrowserCLIProfileByID(sessionInfo.ProfileID)
		}
	}

	enc, _ := json.MarshalIndent(map[string]interface{}{
		"session_id":         flags.sessionID,
		"browser_session_id": sessionInfo.BrowserSessionID,
		"closed":             true,
	}, "", "  ")
	fmt.Println(string(enc))
	os.Exit(exitOK)
}

// runGet 执行 get 子命令（text/url/title）。
func runWait(args []string) {
	positional, flags := parseCommonFlags(args, "wait")
	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser wait: requires --id <id>")
		os.Exit(exitRunErr)
	}
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser wait: requires <condition>")
		os.Exit(exitRunErr)
	}
	condition := positional[0]

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser wait: %v\n", err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	impl := connectSession(ctx, sessionInfo, "wait", flags)

	if err := runWaitCondition(ctx, impl, condition); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser wait: %v\n", err)
		exitSessionCore(impl, exitFail)
	}

	enc, _ := json.MarshalIndent(map[string]interface{}{
		"session_id":         flags.sessionID,
		"browser_session_id": sessionInfo.BrowserSessionID,
		"condition":          condition,
		"done":               true,
	}, "", "  ")
	fmt.Println(string(enc))
	exitSessionCore(impl, exitOK)
}

// runWaitCondition 执行 wait 条件。
func runWaitCondition(ctx context.Context, impl browser.BrowserCore, condition string) error {
	// Pure integer → sleep
	if ms, err := strconv.Atoi(strings.TrimSpace(condition)); err == nil {
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return nil
	}

	// "visible <selector>" → wait for element to be visible
	if strings.HasPrefix(condition, "visible ") {
		sel := strings.TrimPrefix(condition, "visible ")
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var count int64
			if err := impl.EvalJS(ctx, waitSelectorCountJS(sel), &count); err == nil && count > 0 {
				return nil
			}
			time.Sleep(200 * time.Millisecond)
		}
		return fmt.Errorf("timeout waiting for visible %q", sel)
	}

	// "gone <selector>" → wait for element to disappear
	if strings.HasPrefix(condition, "gone ") {
		sel := strings.TrimPrefix(condition, "gone ")
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var count int64
			if err := impl.EvalJS(ctx, waitSelectorCountJS(sel), &count); err == nil && count == 0 {
				return nil
			}
			time.Sleep(200 * time.Millisecond)
		}
		return fmt.Errorf("timeout waiting for gone %q", sel)
	}

	// "text <string>" → wait for text to appear in page
	if strings.HasPrefix(condition, "text ") {
		text := strings.TrimPrefix(condition, "text ")
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var bodyText string
			if err := impl.EvalJS(ctx, `document.body.textContent`, &bodyText); err == nil && strings.Contains(bodyText, text) {
				return nil
			}
			time.Sleep(200 * time.Millisecond)
		}
		return fmt.Errorf("timeout waiting for text %q", text)
	}

	// "url <pattern>" → wait for URL to match
	if strings.HasPrefix(condition, "url ") {
		pattern := strings.TrimPrefix(condition, "url ")
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var urlSurfaces string
			if err := impl.EvalJS(ctx, waitURLSurfacesJS(), &urlSurfaces); err == nil {
				if urlSurfacesMatch(urlSurfaces, pattern) {
					return nil
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		return fmt.Errorf("timeout waiting for url %q", pattern)
	}

	return fmt.Errorf("unknown wait condition: %q (use: 2000, 'visible #sel', 'gone #sel', 'text ...')", condition)
}

func waitURLSurfacesJS() string {
	return `(() => {
		const values = [];
		const push = (value) => {
			const text = String(value || '').trim();
			if (text) values.push(text);
		};
		push(window.location.href);
		push(document.querySelector('[data-testid="rcb-chip-url"]')?.textContent);
		push(document.querySelector('.browser-tab--active[data-testid^="browser-tab-"]')?.textContent);
		push(document.querySelector('[role="tab"][aria-selected="true"]')?.textContent);
		return values.join('\n');
	})()`
}

func normalizeURLWaitText(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.Trim(value, `"'`)
	value = strings.ReplaceAll(value, "**", "")
	value = strings.TrimPrefix(value, "当前页面:")
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "http://")
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimPrefix(value, "www.")
	value = strings.TrimRight(value, "/")
	return value
}

func urlSurfacesMatch(surfaces string, pattern string) bool {
	rawPattern := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(pattern, "**", "")))
	normalizedPattern := normalizeURLWaitText(pattern)
	if rawPattern == "" && normalizedPattern == "" {
		return true
	}
	for _, line := range strings.Split(surfaces, "\n") {
		rawLine := strings.ToLower(strings.TrimSpace(line))
		normalizedLine := normalizeURLWaitText(line)
		if rawPattern != "" && strings.Contains(rawLine, rawPattern) {
			return true
		}
		if normalizedPattern != "" && strings.Contains(normalizedLine, normalizedPattern) {
			return true
		}
	}
	return false
}

func waitSelectorCountJS(selector string) string {
	return fmt.Sprintf(`(() => {
		const selector = %q;
		const seen = new Set();
		const addAll = (nodes) => Array.from(nodes || []).forEach(node => seen.add(node));
		try { addAll(document.querySelectorAll(selector)); } catch (_) {}
		if (/^#[A-Za-z0-9_-]+$/.test(selector)) {
			const testid = selector.slice(1);
			try { addAll(document.querySelectorAll('[data-testid="' + CSS.escape(testid) + '"]')); } catch (_) {}
		}
		return seen.size;
	})()`, selector)
}

// runScreenshot 执行 screenshot 子命令。
// Session 模式: dw-browser screenshot --id <id> [out.png]
// One-shot 模式: dw-browser screenshot <url> [out.png]
func runLayout(args []string) {
	positional, flags := parseCommonFlags(args, "layout")
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser layout: requires <url>")
		os.Exit(exitRunErr)
	}
	// layout 新建一次性会话（无 --id 继承）→ --scenario 必选。
	_, scenarioPolicy := resolveScenario(&flags, "layout")
	url := positional[0]
	profileID := resolveProfileID(flags, "default")
	browserOpts := browserOptionsFromFlags(flags)

	bc := newBrowserCore(profileID, browserOpts...)
	applyInteractionScenario(bc, flags.scenario)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// exit closes bc + cleans up an ephemeral profile before terminating.
	// os.Exit skips defers, so every exit path below must go through this
	// instead of calling os.Exit directly (see cleanupEphemeral leak fix).
	exit := func(code int) {
		bc.Close(ctx)
		if flags.ephemeral {
			cleanupEphemeral(profileID)
		}
		os.Exit(code)
	}

	snap, err := navigateWithRetry(ctx, bc, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: navigate failed: %v\n", err)
		exit(exitRunErr)
	}
	bc.SetPolicy(scenarioPolicy, snap.URL)

	// L2 布局断言: 验证页面有可交互元素（基本布局完整性）
	if len(snap.Refs) == 0 && snap.SnapshotType == "screenshot_fallback" {
		fmt.Fprintf(os.Stderr, "[L2 FAIL] page has no interactive elements: %s\n", url)
		exit(exitFail)
	}

	fmt.Printf("[L2 PASS] layout OK: %s (refs=%d, type=%s)\n", url, len(snap.Refs), snap.SnapshotType)
	exit(exitOK)
}

// ============================================================
// § YAML 测试引擎（test 子命令）[Ref: CAP-BS09-C5, TC-09-I-21]
// ============================================================

// ============================================================
// § Failure Report v0 [Ref: T5-TS-TEST §4 P0-4]
// ============================================================

// FailureReport 步骤失败时输出到 stderr 的结构化诊断证据包。
type FailureReport struct {
	Version    string       `json:"version"`
	Timestamp  string       `json:"timestamp"`
	TestName   string       `json:"test_name"`
	FailedStep FailedStep   `json:"failed_step"`
	Browser    BrowserState `json:"browser_state"`
	Server     ServerState  `json:"server_state"`
	Screenshot string       `json:"screenshot_path"`
}

// FailedStep 失败步骤的元数据。
type FailedStep struct {
	Index       int    `json:"index"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Error       string `json:"error"`
}

// BrowserState 失败瞬间的浏览器状态。
type BrowserState struct {
	URL         string `json:"url"`
	A11ySummary string `json:"a11y_summary"`
}

// ServerState 失败瞬间的服务端状态（graceful: 不可达时返回 error 字段）。
type ServerState struct {
	RecentErrors json.RawMessage `json:"recent_errors"`
	Health       json.RawMessage `json:"health"`
}

// extractBaseURL 从 spec 的 navigate 步骤中推断服务器基础 URL。
// 取第一个 navigate 步骤的 scheme+host+port；找不到则返回默认值。
func extractBaseURL(steps []TestStep) string {
	for _, s := range steps {
		if s.Type == "navigate" && s.URL != "" {
			// 只保留 scheme://host[:port]
			u := s.URL
			// strip path: find third slash after scheme://
			if idx := strings.Index(u, "://"); idx >= 0 {
				rest := u[idx+3:]
				if slash := strings.Index(rest, "/"); slash >= 0 {
					return u[:idx+3+slash]
				}
				return u
			}
		}
	}
	return "http://localhost:8080"
}

// fetchJSONWithTimeout 向 url 发送 GET，超时 timeout，返回 json.RawMessage。
// 失败时返回 {"error": "<reason>"} 而不是真正的 error，保证 graceful。
func fetchJSONWithTimeout(url string, timeout time.Duration) json.RawMessage {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url) //nolint:noctx
	if err != nil {
		msg, _ := json.Marshal(map[string]string{"error": "server unreachable: " + err.Error()})
		return json.RawMessage(msg)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		msg, _ := json.Marshal(map[string]string{"error": "read body: " + err.Error()})
		return json.RawMessage(msg)
	}
	if !json.Valid(body) {
		msg, _ := json.Marshal(map[string]string{"raw": string(body)})
		return json.RawMessage(msg)
	}
	return json.RawMessage(body)
}

// collectFailureReport 采集结构化诊断证据并将 JSON block 输出到 stderr。
// 任何单项采集失败都不影响其他项，也不影响 test runner 的正常流程。
func collectFailureReport(ctx context.Context, bc browser.BrowserCore, testName string, stepIndex int, step TestStep, stepErr error, baseURL string) {
	ts := time.Now().UTC().Format(time.RFC3339)
	stepDesc := step.Description
	if stepDesc == "" {
		stepDesc = fmt.Sprintf("step %d (%s)", stepIndex+1, step.Type)
	}

	report := FailureReport{
		Version:   "v0",
		Timestamp: ts,
		TestName:  testName,
		FailedStep: FailedStep{
			Index:       stepIndex,
			Type:        step.Type,
			Description: stepDesc,
			Error:       stepErr.Error(),
		},
	}

	// --- 浏览器状态 ---
	func() {
		snapCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		var currentURL string
		// graceful: ignore EvalJS error
		_ = bc.EvalJS(snapCtx, "window.location.href", &currentURL)
		report.Browser.URL = currentURL

		snap, err := bc.Snap(snapCtx)
		if err == nil && snap != nil {
			a11y := snap.Text
			const maxA11y = 5 * 1024 // 5KB
			if len(a11y) > maxA11y {
				totalKB := len(a11y) / 1024
				a11y = a11y[:maxA11y] + fmt.Sprintf("\n... (truncated, total: %dKB)", totalKB)
			}
			report.Browser.A11ySummary = a11y
			if currentURL == "" {
				report.Browser.URL = snap.URL
			}
		}
	}()

	// --- 截图 ---
	func() {
		ssCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		data, err := bc.Screenshot(ssCtx, false)
		if err != nil {
			return
		}
		tsFile := time.Now().UTC().Format("20060102-150405")
		outPath := fmt.Sprintf("/tmp/failure-report-%s.png", tsFile)
		if writeErr := os.WriteFile(outPath, data, 0644); writeErr == nil {
			report.Screenshot = outPath
		}
	}()

	// --- 服务端状态 ---
	const serverTimeout = 2 * time.Second
	report.Server.RecentErrors = fetchJSONWithTimeout(
		baseURL+"/api/debug/obs/recent?limit=10", serverTimeout)
	report.Server.Health = fetchJSONWithTimeout(
		baseURL+"/api/health", serverTimeout)

	// --- 输出到 stderr ---
	out := map[string]interface{}{"failure_report": report}
	enc, _ := json.MarshalIndent(out, "", "  ")
	fmt.Fprintln(os.Stderr, string(enc))
}

// TestSpec 测试规格 (YAML/JSON)。
type TestSpec struct {
	Name  string     `yaml:"name" json:"name"`
	Steps []TestStep `yaml:"steps" json:"steps"`
}

// TestStep 单个测试步骤。
type TestStep struct {
	// 标识
	ID          string `yaml:"id,omitempty" json:"id,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// 步骤类型: navigate|snap|act|l1_assert|wait|screenshot|text|css_assert|css_text_assert|assert_text|assert_exists
	Type string `yaml:"type" json:"type"`
	// 导航
	URL     string `yaml:"url,omitempty" json:"url,omitempty"`
	WaitFor string `yaml:"wait_for,omitempty" json:"wait_for,omitempty"`
	// 操作
	Action string `yaml:"action,omitempty" json:"action,omitempty"`
	// 断言
	Role         string `yaml:"role,omitempty" json:"role,omitempty"`
	NameContains string `yaml:"name_contains,omitempty" json:"name_contains,omitempty"`
	Selector     string `yaml:"selector,omitempty" json:"selector,omitempty"`
	Contains     string `yaml:"contains,omitempty" json:"contains,omitempty"`
	// 输出
	Output string `yaml:"output,omitempty" json:"output,omitempty"`
	// 时间
	DurationMs int `yaml:"duration_ms,omitempty" json:"duration_ms,omitempty"`
	TimeoutMs  int `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// AAAAA 断言 (P2)
	Anticipate         string `yaml:"anticipate,omitempty" json:"anticipate,omitempty"`                     // 前置条件描述(文档用)
	ExpectRecentErrors *int   `yaml:"expect_recent_errors,omitempty" json:"expect_recent_errors,omitempty"` // 期望 recent errors 数 (nil=不检查, 0=无错误)
	ExpectURL          string `yaml:"expect_url,omitempty" json:"expect_url,omitempty"`                     // 期望当前 URL 包含此字符串
	ExpectSchema       string `yaml:"expect_schema,omitempty" json:"expect_schema,omitempty"`               // 期望 JSON 响应包含此 key
}

// ObservationRecord 单步观测记录 (--diag 模式)。
type ObservationRecord struct {
	Step         int             `json:"step"`
	Type         string          `json:"type"`
	Description  string          `json:"description,omitempty"`
	URL          string          `json:"url,omitempty"`
	StartedAt    string          `json:"started_at"`
	EndedAt      string          `json:"ended_at"`
	DurationMs   int64           `json:"duration_ms"`
	Result       string          `json:"result"` // "pass" | "fail" | "skip"
	Error        string          `json:"error,omitempty"`
	BrowserURL   string          `json:"browser_url,omitempty"`
	ServerEvents json.RawMessage `json:"server_events,omitempty"` // from /api/debug/obs/recent
}

// runTest 执行 test 子命令（YAML 多步骤测试）[TC-09-I-21, TC-09-J-06]。
func runTest(args []string) {
	positional, flags := parseCommonFlags(args, "test")
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser test: requires <spec.yaml>")
		os.Exit(exitRunErr)
	}
	// test 新建一次性会话（无 --id 继承）→ --scenario 必选。
	_, scenarioPolicy := resolveScenario(&flags, "test")
	specFile := positional[0]

	data, err := os.ReadFile(specFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: read spec file: %v\n", err)
		os.Exit(exitRunErr)
	}

	// 支持真 YAML 解析，兼容 JSON fallback
	var spec TestSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		// YAML 解析失败，尝试 JSON fallback（兼容现有 JSON spec）
		if jsonErr := json.Unmarshal(data, &spec); jsonErr != nil {
			fmt.Fprintf(os.Stderr, "dw-browser: parse spec (yaml: %v, json: %v)\n", err, jsonErr)
			os.Exit(exitRunErr)
		}
	}

	requireChrome()
	profileID := resolveProfileID(flags, fmt.Sprintf("dw-test-%d", time.Now().UnixNano()))
	browserOpts := browserOptionsFromFlags(flags)

	bc := newBrowserCore(profileID, browserOpts...)
	applyInteractionScenario(bc, flags.scenario)
	// scenario 导出的 policy 贯穿本次 test 会话（per-act origin 现场分类，故初始 origin 留空）。
	bc.SetPolicy(scenarioPolicy, "")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	// exit closes bc + cleans up an ephemeral profile before terminating.
	// os.Exit skips defers, so every exit path below must go through this
	// instead of calling os.Exit directly (see cleanupEphemeral leak fix).
	exit := func(code int) {
		bc.Close(ctx)
		if flags.ephemeral {
			cleanupEphemeral(profileID)
		}
		os.Exit(code)
	}

	passed := 0
	failed := 0
	var lastSnap *browser.Snapshot

	// 从 navigate 步骤推断服务基础 URL（用于 failure report server_state 采集）
	baseURL := extractBaseURL(spec.Steps)

	// --diag: 创建 observation log 文件
	diagMode := flags.diag
	var obsFile *os.File
	var obsFileName string
	if diagMode {
		ts := time.Now().Format("20060102-150405")
		specBase := specFile
		// strip path and extension
		if idx := strings.LastIndex(specBase, "/"); idx >= 0 {
			specBase = specBase[idx+1:]
		}
		if idx := strings.LastIndex(specBase, "."); idx >= 0 {
			specBase = specBase[:idx]
		}
		obsFileName = fmt.Sprintf("%s-obs-%s.jsonl", specBase, ts)
		f, err := os.Create(obsFileName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser test: --diag: create obs file: %v\n", err)
			exit(exitRunErr)
		}
		obsFile = f
		defer obsFile.Close()
		fmt.Printf("[DIAG] observation log: %s\n", obsFileName)
	}

	// reportWG 追踪 failure report goroutine，确保在 os.Exit 前全部完成。
	var reportWG sync.WaitGroup

	// spawnReport 启动一个 failure report 采集 goroutine。
	spawnReport := func(i int, step TestStep, err error) {
		reportWG.Add(1)
		go func() {
			defer reportWG.Done()
			collectFailureReport(ctx, bc, spec.Name, i, step, err, baseURL)
		}()
	}

	for i, step := range spec.Steps {
		stepDesc := step.Description
		if stepDesc == "" {
			stepDesc = fmt.Sprintf("step %d (%s)", i+1, step.Type)
		}

		// PRE: anticipate 只是文档标注，打印即可
		if step.Anticipate != "" {
			fmt.Printf("[ANTICIPATE] %s: %s\n", stepDesc, step.Anticipate)
		}

		// OBS: 记录开始时间 (--diag)
		var stepStart time.Time
		if diagMode {
			stepStart = time.Now()
		}

		// EXEC: 原有 switch...case
		// stepHandled=true 表示 pass/fail 已在 case 内部记录（特殊路径）
		var stepExecErr error
		stepHandled := false

		switch step.Type {
		case "navigate":
			snap, err := navigateWithRetry(ctx, bc, step.URL)
			if err != nil {
				stepExecErr = err
			} else {
				lastSnap = snap
				time.Sleep(500 * time.Millisecond)
				if refreshed, refreshErr := bc.Snap(ctx); refreshErr == nil && refreshed != nil {
					lastSnap = refreshed
				}
				fmt.Printf("[PASS] %s: navigated to %s\n", stepDesc, snap.URL)
			}

		case "snap":
			snap, err := bc.Snap(ctx)
			if err != nil {
				stepExecErr = err
			} else {
				lastSnap = snap
				fmt.Printf("[PASS] %s: snap OK (refs=%d)\n", stepDesc, len(snap.Refs))
			}

		case "act":
			// 支持语义选择器 (TH-0405-p7c 修改 3)
			snap, err := actWithRetry(ctx, bc, step.Action, true)
			if err != nil {
				stepExecErr = err
			} else {
				lastSnap = snap
				fmt.Printf("[PASS] %s: act %q OK\n", stepDesc, step.Action)
			}

		case "l1_assert":
			// L1 断言: 零 JS 注入 [IR-06]
			if lastSnap == nil {
				l1Err := fmt.Errorf("no snapshot available")
				fmt.Fprintf(os.Stderr, "[L1 FAIL] %s: %v\n", stepDesc, l1Err)
				spawnReport(i, step, l1Err)
				failed++
				if diagMode {
					writeObservation(obsFile, i, step, stepStart, l1Err, bc, baseURL)
				}
				continue
			}
			found := false
			for _, ref := range lastSnap.Refs {
				if ref.Role == step.Role && strings.Contains(ref.Name, step.NameContains) {
					found = true
					break
				}
			}
			if found {
				fmt.Printf("[L1 PASS] %s: %s '%s' found\n", stepDesc, step.Role, step.NameContains)
				passed++
			} else {
				l1Err := fmt.Errorf("%s '%s' not found in snapshot", step.Role, step.NameContains)
				fmt.Fprintf(os.Stderr, "[L1 FAIL] %s: %v\n", stepDesc, l1Err)
				spawnReport(i, step, l1Err)
				failed++
			}
			if diagMode {
				writeObservation(obsFile, i, step, stepStart, stepExecErr, bc, baseURL)
			}
			continue

		case "wait":
			// 等待指定时间（默认 1000ms）
			dur := step.DurationMs
			if dur <= 0 {
				dur = 1000
			}
			time.Sleep(time.Duration(dur) * time.Millisecond)
			fmt.Printf("[PASS] %s: waited %dms\n", stepDesc, dur)

		case "screenshot":
			// 截图保存到文件
			data, err := screenshotWithRetry(ctx, bc, false)
			if err != nil {
				stepExecErr = err
			} else {
				outFile := step.Output
				if outFile == "" {
					outFile = fmt.Sprintf("/tmp/dw-test-screenshot-%d.png", i+1)
				}
				os.WriteFile(outFile, data, 0644)
				fmt.Printf("[PASS] %s: screenshot → %s (%d bytes)\n", stepDesc, outFile, len(data))
			}

		case "text":
			// 提取页面文本
			text, err := bc.Text(ctx, nil)
			if err != nil {
				stepExecErr = err
			} else {
				// 截断显示
				display := text
				if len(display) > 500 {
					display = display[:500] + "..."
				}
				fmt.Printf("[PASS] %s: text(%d chars): %s\n", stepDesc, len(text), display)
			}

		case "css_assert":
			// CSS 选择器存在性断言
			if step.Selector == "" {
				selErr := fmt.Errorf("selector required")
				fmt.Fprintf(os.Stderr, "[FAIL] %s: %v\n", stepDesc, selErr)
				spawnReport(i, step, selErr)
				failed++
				if diagMode {
					writeObservation(obsFile, i, step, stepStart, selErr, bc, baseURL)
				}
				continue
			}
			count, err := selectorCountWithRetry(ctx, bc, step.Selector)
			if err != nil || count == 0 {
				cssErr := err
				if cssErr == nil {
					cssErr = fmt.Errorf("selector %q not found", step.Selector)
				}
				fmt.Fprintf(os.Stderr, "[FAIL] %s: selector %q not found\n", stepDesc, step.Selector)
				spawnReport(i, step, cssErr)
				failed++
				if diagMode {
					writeObservation(obsFile, i, step, stepStart, cssErr, bc, baseURL)
				}
				continue
			}
			fmt.Printf("[PASS] %s: selector %q found (%d matches)\n", stepDesc, step.Selector, count)

		case "css_text_assert":
			// CSS 选择器 + 文本内容断言
			if step.Selector == "" {
				selErr := fmt.Errorf("selector required")
				fmt.Fprintf(os.Stderr, "[FAIL] %s: %v\n", stepDesc, selErr)
				spawnReport(i, step, selErr)
				failed++
				if diagMode {
					writeObservation(obsFile, i, step, stepStart, selErr, bc, baseURL)
				}
				continue
			}
			var textContent string
			err := bc.EvalJS(ctx, fmt.Sprintf(`(document.querySelector(%q) || {}).textContent || ""`, step.Selector), &textContent)
			if err != nil {
				stepExecErr = err
			} else if step.NameContains != "" && !strings.Contains(textContent, step.NameContains) {
				stepExecErr = fmt.Errorf("%q text=%q not contains %q", step.Selector, textContent, step.NameContains)
			} else {
				fmt.Printf("[PASS] %s: %q text=%q\n", stepDesc, step.Selector, textContent)
			}

		case "l4_probe_start":
			// L4 probe 注入（Lazy Injection） [IR-06]
			fmt.Printf("[L4] %s: probe started (lazy injection)\n", stepDesc)

		case "l4_probe_collect":
			// L4 probe 清理 [IR-06]
			fmt.Printf("[L4] %s: probe collected + cleanup\n", stepDesc)

		default:
			fmt.Fprintf(os.Stderr, "[SKIP] %s: unknown step type %q\n", stepDesc, step.Type)
			stepHandled = true // skip 不计入 pass/fail
		}

		// POST: AAAAA 后置断言 (P2)
		if stepExecErr == nil && !stepHandled {
			if assertErr := runPostAssertions(ctx, bc, step, baseURL); assertErr != nil {
				fmt.Fprintf(os.Stderr, "[ASSERT FAIL] %s: %v\n", stepDesc, assertErr)
				failed++
				spawnReport(i, step, assertErr)
				if diagMode {
					writeObservation(obsFile, i, step, stepStart, assertErr, bc, baseURL)
				}
				continue
			}
		}

		// 记录 pass/fail（跳过 skip 类型）
		if !stepHandled {
			if stepExecErr != nil {
				fmt.Fprintf(os.Stderr, "[FAIL] %s: %v\n", stepDesc, stepExecErr)
				failed++
				spawnReport(i, step, stepExecErr)
			} else {
				passed++
			}
		}

		// OBS: 写 observation record (--diag)
		if diagMode {
			writeObservation(obsFile, i, step, stepStart, stepExecErr, bc, baseURL)
		}
	}

	fmt.Printf("\n== Test Summary: %d PASS, %d FAIL ==\n", passed, failed)

	// 诊断摘要 (--diag)
	if diagMode {
		fmt.Printf("\n== Diagnostic Summary ==\n")
		fmt.Printf("Observation log: %s\n", obsFileName)
		fmt.Printf("Total steps: %d (pass=%d, fail=%d)\n", passed+failed, passed, failed)
	}

	if failed > 0 {
		// 等待所有 failure report goroutine 完成后再退出（最长不超过 server timeout × 2）
		reportWG.Wait()
		exit(exitFail)
	}
	exit(exitOK)
}

// runPostAssertions 执行 AAAAA 后置断言 (P2)。
func runPostAssertions(ctx context.Context, bc browser.BrowserCore, step TestStep, baseURL string) error {
	// expect_url: 检查当前 URL
	if step.ExpectURL != "" {
		var currentURL string
		_ = bc.EvalJS(ctx, "window.location.href", &currentURL)
		if !strings.Contains(currentURL, step.ExpectURL) {
			return fmt.Errorf("expect_url: current %q not contains %q", currentURL, step.ExpectURL)
		}
	}

	// expect_recent_errors: 查询 /api/debug/obs/recent
	if step.ExpectRecentErrors != nil {
		expected := *step.ExpectRecentErrors
		raw := fetchJSONWithTimeout(baseURL+"/api/debug/obs/recent?limit=50&since=5s", 2*time.Second)
		// 检查是否是错误响应
		var errCheck struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &errCheck) == nil && errCheck.Error != "" {
			return fmt.Errorf("expect_recent_errors: server unreachable: %s", errCheck.Error)
		}
		// 解析 entries 数组长度
		var result struct {
			Entries []json.RawMessage `json:"entries"`
		}
		json.Unmarshal(raw, &result)
		actual := len(result.Entries)
		if actual != expected {
			return fmt.Errorf("expect_recent_errors: expected %d, got %d", expected, actual)
		}
	}

	// expect_schema: 检查页面文本包含 JSON key
	if step.ExpectSchema != "" {
		text, err := bc.Text(ctx, nil)
		if err != nil {
			return fmt.Errorf("expect_schema: cannot get text: %v", err)
		}
		if !strings.Contains(text, step.ExpectSchema) {
			return fmt.Errorf("expect_schema: key %q not found in page text", step.ExpectSchema)
		}
	}

	return nil
}

// writeObservation 写单步观测记录到 JSONL 文件 (--diag 模式)。
func writeObservation(f *os.File, index int, step TestStep, start time.Time, stepErr error, bc browser.BrowserCore, baseURL string) {
	if f == nil {
		return
	}
	end := time.Now()
	rec := ObservationRecord{
		Step:        index,
		Type:        step.Type,
		Description: step.Description,
		URL:         step.URL,
		StartedAt:   start.Format(time.RFC3339Nano),
		EndedAt:     end.Format(time.RFC3339Nano),
		DurationMs:  end.Sub(start).Milliseconds(),
		Result:      "pass",
	}
	if stepErr != nil {
		rec.Result = "fail"
		rec.Error = stepErr.Error()
	}

	// 采集浏览器当前 URL
	var currentURL string
	evalCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = bc.EvalJS(evalCtx, "window.location.href", &currentURL)
	rec.BrowserURL = currentURL

	// 采集时间窗口内服务端事件
	sinceMs := end.Sub(start).Milliseconds() + 1000 // 多取 1s buffer
	serverResp := fetchJSONWithTimeout(
		fmt.Sprintf("%s/api/debug/obs/recent?limit=20&since=%dms", baseURL, sinceMs),
		2*time.Second,
	)
	// 只有非错误响应才记录
	var errCheck struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(serverResp, &errCheck) != nil || errCheck.Error == "" {
		rec.ServerEvents = serverResp
	}

	line, _ := json.Marshal(rec)
	fmt.Fprintln(f, string(line))
}

// encodeScreenshot 将截图 bytes 编码为 base64（供 JSON 输出）。
func encodeScreenshot(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// ============================================================
// § runAudit — audit 命令 [browser/audit]
// ============================================================

func runAudit(args []string) {
	suite := "full"
	check := ""
	threshold := 0
	format := "json"

	var cleanArgs []string
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--suite" && i+1 < len(args):
			suite = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--suite="):
			suite = arg[len("--suite="):]
			i++
		case arg == "--check" && i+1 < len(args):
			check = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--check="):
			check = arg[len("--check="):]
			i++
		case arg == "--threshold" && i+1 < len(args):
			v, err := strconv.Atoi(args[i+1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "dw-browser audit: --threshold must be an integer\n")
				os.Exit(exitRunErr)
			}
			threshold = v
			i += 2
		case strings.HasPrefix(arg, "--threshold="):
			v, err := strconv.Atoi(arg[len("--threshold="):])
			if err != nil {
				fmt.Fprintf(os.Stderr, "dw-browser audit: --threshold must be an integer\n")
				os.Exit(exitRunErr)
			}
			threshold = v
			i++
		case arg == "--format" && i+1 < len(args):
			format = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--format="):
			format = arg[len("--format="):]
			i++
		default:
			cleanArgs = append(cleanArgs, arg)
			i++
		}
	}

	_, flags := parseCommonFlags(cleanArgs, "audit")

	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser audit: requires --id <id>")
		os.Exit(exitRunErr)
	}

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser audit: %v\n", err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	impl := connectSession(ctx, sessionInfo, "audit", flags)

	registry := audit.NewRegistry()
	audit.RegisterCompat(registry)
	audit.RegisterA11y(registry)
	audit.RegisterLayout(registry)
	audit.RegisterPerf(registry)
	audit.RegisterChrome(registry)

	// browser chrome 仿真会话：把遮挡区几何（SSOT=设备预设）注入 occlusion 检查。
	// 未启用时 zones 留空 → 脚本自判"不适用"返回 pass，不造假阳。
	if bcc, ok := impl.(browser.BrowserChromeCapable); ok {
		if spec, _, _, on := bcc.BrowserChromeState(); on {
			if c := registry.ByID("browser-chrome-occlusion"); c != nil {
				c.Params["zones"] = spec.OcclusionZones(sessionInfo.ViewportW, sessionInfo.Keyboard)
			}
		}
	}

	runner := audit.NewRunner(registry)

	opts := audit.RunOpts{
		Suite:     suite,
		Threshold: threshold,
		URL:       sessionInfo.PageURL,
	}
	if check != "" {
		opts.Checks = []string{check}
	}

	report, runErr := runner.Run(ctx, impl, opts)
	if report == nil {
		fmt.Fprintf(os.Stderr, "dw-browser audit: %v\n", runErr)
		exitSessionCore(impl, exitRunErr)
	}

	switch format {
	case "compact":
		fmt.Println(report.FormatCompact())
	default:
		out, err := report.FormatJSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser audit: marshal report: %v\n", err)
			exitSessionCore(impl, exitRunErr)
		}
		fmt.Println(string(out))
	}

	if runErr != nil {
		fmt.Fprintf(os.Stderr, "AUDIT FAILED: score %d < threshold %d\n", report.Score, threshold)
		exitSessionCore(impl, exitFail)
	}
	exitSessionCore(impl, exitOK)
}
