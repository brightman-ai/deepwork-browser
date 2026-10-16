// dw-browser — BS-09 Browser Runtime CLI 测试引擎（独立二进制）
//
// 零 Deepwork 依赖：不导入 internal/conversation, internal/topic, internal/webui
// 铁律 IR-05 + IR-08 [Ref: BP §B1 Phase 5, CAP-BS09-C5]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
	"github.com/brightman-ai/deepwork-browser/browser/audit"
	"github.com/brightman-ai/deepwork-browser/browser/safari"
	"github.com/brightman-ai/kit/obs"
	cdpTarget "github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

const version = "0.17.0-v2"

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
	evidenceDir       string
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
	// 单操作者/会话锁 (见 acquireSessionOperatorLockOrExit)。真正的取锁发生在
	// main() 的公共入口,这里只是把 flag 从 positional 里吃掉,免得它漏进动作串。
	lockWait string // --lock-wait <dur>: 第二操作者的有界等待时长 (默认 10s)
	force    bool   // --force: 运维逃生口,close 用它跳过会话锁
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
func parseCommonFlagsE(args []string, cmd string) (positional []string, flags commonFlags, parseErr error) {
	flags.profileID = ""
	flags.sessionKind = browser.SessionKindTask

	// Step 1: 应用 env 默认 (优先级 3)
	if envMode := strings.TrimSpace(os.Getenv("DW_BROWSER_DEFAULT_MODE")); envMode != "" && (cmd == "open" || cmd == "once" || (cmd == "journey" && scanSessionIDArg(args) == "")) {
		if mode, ok := parseBrowserMode(envMode); ok {
			flags.mode = mode
			flags.headless = mode == browser.ModeHeadless
			flags.modeExplicit = true
		} else {
			fmt.Fprintf(os.Stderr, "dw-browser: 警告 — DW_BROWSER_DEFAULT_MODE=%q 不识别 (允许: headless/headed/visible), 回退 visible\n",
				os.Getenv("DW_BROWSER_DEFAULT_MODE"))
		}
	}

	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--":
			positional = append(positional, args[i+1:]...)
			i = len(args)
		case arg == "--evidence" && i+1 < len(args):
			flags.evidenceDir = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--evidence="):
			flags.evidenceDir = strings.TrimPrefix(arg, "--evidence=")
			i++
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
		case arg == "--profile" && i+1 < len(args):
			flags.profileID = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--profile="):
			flags.profileID = arg[len("--profile="):]
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
			return positional, flags, fmt.Errorf("%s: invalid arguments", cmd)
		case arg == "--user-agent" && i+1 < len(args):
			flags.userAgent = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--user-agent="):
			flags.userAgent = arg[len("--user-agent="):]
			i++
		case arg == "--mode" && i+1 < len(args):
			// 推荐新 flag (优先级 1): --mode {headless|headed|visible|human}
			mode, ok := parseBrowserMode(args[i+1])
			if !ok {
				fmt.Fprintf(os.Stderr, "dw-browser %s: --mode 值无效 %q (允许: headless/headed/visible)\n", cmd, args[i+1])
				return positional, flags, fmt.Errorf("%s: invalid arguments", cmd)
			}
			flags.mode = mode
			flags.headless = mode == browser.ModeHeadless
			flags.modeExplicit = true
			i += 2
		case strings.HasPrefix(arg, "--mode="):
			val := arg[len("--mode="):]
			mode, ok := parseBrowserMode(val)
			if !ok {
				fmt.Fprintf(os.Stderr, "dw-browser %s: --mode 值无效 %q (允许: headless/headed/visible)\n", cmd, val)
				return positional, flags, fmt.Errorf("%s: invalid arguments", cmd)
			}
			flags.mode = mode
			flags.headless = mode == browser.ModeHeadless
			flags.modeExplicit = true
			i++
		case arg == "--lock-wait" && i+1 < len(args):
			flags.lockWait = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--lock-wait="):
			flags.lockWait = arg[len("--lock-wait="):]
			i++
		case arg == "--force":
			flags.force = true
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
			return positional, flags, fmt.Errorf("%s: invalid arguments", cmd)
		case arg == "--help" || arg == "-h":
			return positional, flags, nil
		default:
			if strings.HasPrefix(arg, "-") {
				return positional, flags, fmt.Errorf("%s: unknown flag %s", cmd, arg)
			}
			positional = append(positional, arg)
			i++
		}
	}

	// 互斥验证: --ephemeral 和 --profile 不可同时使用
	if flags.ephemeral && flags.profileID != "" {
		fmt.Fprintf(os.Stderr, "dw-browser %s: --ephemeral 和 --profile 互斥，不可同时使用\n", cmd)
		return positional, flags, fmt.Errorf("%s: invalid arguments", cmd)
	}

	// 验证 --engine 值
	if flags.engine != "" && flags.engine != "chrome" && flags.engine != "safari" {
		fmt.Fprintf(os.Stderr, "dw-browser %s: --engine 值无效 %q (允许: chrome/safari)\n", cmd, flags.engine)
		return positional, flags, fmt.Errorf("%s: invalid arguments", cmd)
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
		return positional, flags, fmt.Errorf("%s: invalid arguments", cmd)
	}

	// 解析 --persona（优先级高于 --viewport / --user-agent；后者作细粒度 override 逃生口）
	if flags.persona != "" {
		p, err := browser.ResolvePersona(flags.persona)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser: %v\n", err)
			return positional, flags, fmt.Errorf("%s: invalid arguments", cmd)
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
			return positional, flags, fmt.Errorf("%s: invalid arguments", cmd)
		}
		// [REQ-AE-03] 误用硬拦：裸移动尺寸视口 = "假手机"（无 UA/无 touch/无 Safari
		// chrome 仿真），测不出真机问题还给假绿 —— AI-native 最大误用陷阱，error 不 warn。
		if !flags.hasPersona && w > 0 && w < 500 {
			fmt.Fprintf(os.Stderr, "dw-browser %s: 裸 --viewport %dx%d 是\"假手机\"(无 UA/touch/Safari chrome, 测不出真机问题)。\n  测移动端 → --persona mobile\n  确要裸小视口(响应式断点等) → --persona desktop --viewport %dx%d\n", cmd, w, h, w, h)
			return positional, flags, fmt.Errorf("%s: invalid arguments", cmd)
		}
		flags.viewportW = w
		flags.viewportH = h
		flags.hasViewport = true
	}

	// --user-agent 单独指定（覆盖 persona 的 UA）
	if !flags.hasPersona && flags.userAgent != "" {
		flags.hasUA = true
	}

	return positional, flags, nil
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
	case "headed":
		return browser.ModeHeaded, true
	case "visible":
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

func main() { os.Exit(runCLI(os.Args[1:])) }

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
func runPersonaListResult(args []string) (CommandResult, error) {
	entries := []map[string]any{}
	for _, name := range browser.PersonaOrder {
		p := browser.Personas[name]
		if p == nil {
			continue
		}
		w, h, _ := p.EffectiveViewport()
		entries = append(entries, map[string]any{"id": name, "name": p.Name, "viewport": map[string]int{"width": w, "height": h}, "stealth": p.Stealth})
	}
	return commandSuccess(map[string]any{"personas": entries}), nil
}

func wantsHelp(args []string) bool {
	valueFlags := map[string]bool{}
	for _, d := range commandDefinitions() {
		for _, f := range d.Flags {
			if f.TakesValue {
				valueFlags[f.Name] = true
			}
		}
	}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return false
		}
		name, _, inline := strings.Cut(args[i], "=")
		if args[i] == "--help" || args[i] == "-h" {
			return true
		}
		if valueFlags[name] && !inline {
			i++
		}
	}
	return false
}

func runSessionStatusResult(args []string) (commandOutput CommandResult, resultErr error) {
	_, flags, parseErr := parseCommonFlagsE(args, "session status")
	if parseErr != nil {
		return commandOutput, commandError("validation", exitRunErr, parseErr)
	}
	if flags.sessionID == "" {
		commandOutput.note("%v\n", "dw-browser session status: requires --id <id>")
		return finishCommand(commandOutput, exitRunErr)
	}
	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		commandOutput.note("dw-browser session status: %v\n", err)
		return finishCommand(commandOutput, exitRunErr)
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
	commandOutput.Payload = output
	return finishCommand(commandOutput, exitOK)

}

func runSessionListResult(args []string) (commandOutput CommandResult, resultErr error) {
	_, _, parseErr := parseCommonFlagsE(args, "session list")
	if parseErr != nil {
		return commandOutput, parseErr
	}
	sessions, err := browser.ListSessions()
	if err != nil {
		commandOutput.note("dw-browser session list: %v\n", err)
		return finishCommand(commandOutput, exitRunErr)
	}
	output := map[string]interface{}{"sessions": sessions}
	commandOutput.Payload = output
	return finishCommand(commandOutput, exitOK)
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
func runProfileResult(args []string) (commandOutput CommandResult, resultErr error) {
	if len(args) < 1 {
		commandOutput.note("%v\n", "dw-browser profile: requires subcommand (list|import|prune)")
		return finishCommand(commandOutput, exitRunErr)
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
				commandOutput.note("dw-browser profile list: %v\n", err)
				return finishCommand(commandOutput, exitRunErr)
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
		commandOutput.Payload = output
		return finishCommand(commandOutput, exitOK)

	case "prune":
		return runProfilePruneResult(args[1:])

	case "import":
		if len(args) < 2 {
			commandOutput.note("%v\n", "dw-browser profile import: requires <source-path>")
			return finishCommand(commandOutput, exitRunErr)
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
			commandOutput.note("dw-browser profile import: mkdir %s: %v\n", profilesDir, err)
			return finishCommand(commandOutput, exitRunErr)
		}
		destDir := filepath.Join(profilesDir, importName)
		if _, statErr := os.Stat(destDir); statErr == nil {
			commandOutput.note("dw-browser profile import: profile %q already exists at %s\n", importName, destDir)
			return finishCommand(commandOutput, exitRunErr)
		}

		cmd := exec.Command("cp", "-r", srcPath, destDir)
		if out, cpErr := cmd.CombinedOutput(); cpErr != nil {
			commandOutput.note("dw-browser profile import: cp -r failed: %v\n%s\n", cpErr, out)
			return finishCommand(commandOutput, exitRunErr)
		}

		enc, _ := json.MarshalIndent(map[string]interface{}{
			"imported": true,
			"name":     importName,
			"path":     destDir,
		}, "", "  ")
		commandOutput.Payload = json.RawMessage(enc)
		return finishCommand(commandOutput, exitOK)

	default:
		commandOutput.note("dw-browser profile: unknown subcommand %q (use list|import|prune)\n", args[0])
		return finishCommand(commandOutput, exitRunErr)
	}

}

func runProfilePruneResult(args []string) (commandOutput CommandResult, resultErr error) {
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
				commandOutput.note("dw-browser profile prune: invalid --min-age %q\n", args[i+1])
				return finishCommand(commandOutput, exitRunErr)
			}
			minAge = d
			i++
		case strings.HasPrefix(arg, "--min-age="):
			raw := strings.TrimPrefix(arg, "--min-age=")
			d, err := time.ParseDuration(raw)
			if err != nil {
				commandOutput.note("dw-browser profile prune: invalid --min-age %q\n", raw)
				return finishCommand(commandOutput, exitRunErr)
			}
			minAge = d
		default:
			commandOutput.note("dw-browser profile prune: unknown flag %q\n", arg)
			return finishCommand(commandOutput, exitRunErr)
		}
	}
	if !ephemeralOnly {
		commandOutput.note("%v\n", "dw-browser profile prune: requires --ephemeral")
		return finishCommand(commandOutput, exitRunErr)
	}
	result, err := browser.PruneBrowserCLIEphemeralProfiles(browser.BrowserCLIEphemeralPruneOptions{
		MinAge:       minAge,
		DryRun:       dryRun,
		ReapOrphaned: reapOrphaned,
	})
	if err != nil {
		commandOutput.note("dw-browser profile prune: %v\n", err)
		return finishCommand(commandOutput, exitRunErr)
	}
	enc, _ := json.MarshalIndent(result, "", "  ")
	commandOutput.Payload = json.RawMessage(enc)
	return finishCommand(commandOutput, exitOK)

}

func replayViewportProfileE(core browser.BrowserCore, presetID string, width, height int, touch bool, cmdName string) error {
	if width <= 0 || height <= 0 {
		return fmt.Errorf("viewport metadata unavailable")
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
			return fmt.Errorf("%s: viewport replay failed: %w", cmdName, err)
		}
	}
	return nil
}

// connectSession 连接到 session 的浏览器，内置 target 自愈，并重放 session/device viewport 语义。
// 所有 session 命令统一通过此函数连接，而非直接调用 NewBrowserCoreFromSession。
// 返回 SessionCore（扩展了 BrowserCore + SnapWithSessionMode/ExecuteAction/RestoreRefsFromSession）。
func connectSessionE(ctx context.Context, sessionInfo *browser.SessionInfo, cmdName string, flags commonFlags) (browser.SessionCore, error) {
	if flags.hasPersona || flags.hasUA || flags.hasViewport || flags.modeExplicit || flags.engine != "" || flags.safariDevice != "" || flags.profileID != "" || flags.scenario != "" || flags.evidenceDir != "" || len(flags.allowHosts) > 0 {
		fmt.Fprintf(os.Stderr, "dw-browser %s: environment flags are creation-only; use session viewport for persistent resize\n", cmdName)
		return nil, fmt.Errorf("%s: session connection or environment validation failed", cmdName)
	}

	// Safari 引擎分支
	if browser.NormalizeEngine(sessionInfo.Engine) == browser.EngineSafari {
		return connectSafariSessionE(ctx, sessionInfo, cmdName)
	}

	if err := ensureSessionBrowserMuxHostReadyE(ctx, sessionInfo, cmdName); err != nil {
		return nil, err
	}

	// target 自愈: 全页面导航/崩溃后 target_id 可能 stale
	targetID, err := browser.ResolveSessionTarget(sessionInfo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: target resolution failed: %v\n", cmdName, err)
		return nil, fmt.Errorf("%s: session connection or environment validation failed", cmdName)
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
		return nil, fmt.Errorf("%s: session connection or environment validation failed", cmdName)
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
	if err := replayViewportProfileE(impl, presetID, width, height, sessionInfo.Touch, cmdName); err != nil {
		closeSessionCore(impl)
		return nil, err
	}
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
	if binder, ok := impl.(interface{ SetSessionAuthoritySource(string) }); ok {
		binder.SetSessionAuthoritySource(sessionInfo.SessionID)
	}
	if err := browser.RestoreSessionAuthority(ctx, impl, sessionInfo); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: restore authority: %v\n", err)
		closeSessionCore(impl)
		return nil, err
	}
	return impl, nil
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

// actionFailedBeforeDispatch reports whether the engine proved the failed
// action died in its validation phase, i.e. no input ever reached the page.
// Engines that cannot prove it are treated as "may have dispatched" — the safe
// side, since the cost of being wrong there is only one extra observe.
func actionFailedBeforeDispatch(core browser.BrowserCore) bool {
	capable, ok := core.(browser.ActionFidelityCapable)
	if !ok {
		return false
	}
	return !capable.LastActionFidelity().Dispatched
}

// applyFailedActionOutcome records what a failed act did to ref authority.
//
// 校验期失败(解析错 / ref 不存在 / 定位器无匹配 / 守卫拒绝) 没有派发任何输入,
// 页面不可能变 —— 上一份 observation 的 @rN 依然成立, 一次打错的定位器不该
// 报废整份可见集 [BUG-ACT-FAILURE-REVOKES-OBSERVATION]。
// 派发之后失败则相反: 页面可能已被改动, 句柄必须作废。
func applyFailedActionOutcome(info *browser.SessionInfo, failedBeforeDispatch bool) {
	if info == nil {
		return
	}
	if failedBeforeDispatch {
		info.LastActionOutcome = browser.SessionActionOutcomeReconciled
		return
	}
	info.Refs = nil
	info.LastActionOutcome = browser.SessionActionOutcomeUnknown
}

func injectActionFidelity(output map[string]interface{}, report browser.ActionFidelityReport) {
	if output == nil {
		return
	}
	if report.Fidelity != "" {
		output["requested_fidelity"] = report.Fidelity
	}
	if report.InteractionMode != "" {
		output["interaction_mode"] = report.InteractionMode
	}
	if report.Execution != "" {
		output["execution"] = report.Execution
	}
	if report.EvidenceScope != "" {
		output["evidence_scope"] = report.EvidenceScope
	}
	if report.Selection != nil {
		output["selection"] = report.Selection
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
	// 前台切换回报: 这次输入之前, 目标 target 不在浏览器前台, 我们把它提了上来。
	// 后台 target 会静默吞掉指针输入, 所以"发生过切换"是判断这次派发健康与否的
	// 一手证据 —— 反复出现意味着有人在和你抢同一个浏览器的前台。
	if report.BroughtToFront {
		output["brought_to_front"] = true
	}
	if report.InputContention != "" {
		output["input_contention"] = report.InputContention
	}
	// 坐标动作的命中回报: 谁真的收到了这次输入 [BUG-COORD-SILENT-FALSE-SUCCESS]。
	// 只报告不拦截 —— 人类同样可能点在遮罩上; 但 agent 必须能据此断言。
	if report.Hit != nil {
		output["hit"] = report.Hit
		output["hit_note"] = "坐标动作命中回报: 上面是派发点上浏览器命中测试解析出的元素。" +
			"success:true 只证明输入已派发, 不证明命中了你想点的东西 —— 请核对 hit 或追加独立断言"
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
func connectSafariSessionE(ctx context.Context, sessionInfo *browser.SessionInfo, cmdName string) (browser.SessionCore, error) {
	opts := safari.SafariOptions{
		UDID:        sessionInfo.DeviceUDID,
		DeviceQuery: sessionInfo.DeviceName,
	}
	core, err := safari.NewSafariBrowserCore(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: connect safari session failed: %v\n", cmdName, err)
		return nil, err
	}
	if sessionInfo.PageURL != "" {
		if _, err := core.Navigate(ctx, sessionInfo.PageURL); err != nil {
			_ = core.Close(ctx)
			fmt.Fprintf(os.Stderr, "dw-browser %s: restore safari page failed: %v\n", cmdName, err)
			return nil, err
		}
	}
	core.RestoreRefsFromSession(sessionInfo.Refs)
	// 接线远程写策略（同 chrome 分支）：origin 分类 URL 用 session 的 PageURL。
	core.SetPolicy(sessionInfo.Policy, sessionInfo.PageURL)
	return core, nil
}

func closeSessionCore(impl browser.SessionCore) {
	if impl == nil {
		return
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer closeCancel()
	_ = impl.Close(closeCtx)
}

func ensureSessionBrowserMuxHostReadyE(ctx context.Context, sessionInfo *browser.SessionInfo, cmdName string) error {
	if sessionInfo == nil || browser.NormalizeBrowserMode(sessionInfo.Mode, browser.ModeHeadless) != browser.ModeHeaded {
		return nil
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
				return nil
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
		return err
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
	return browser.SaveSession(sessionInfo)
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
	// Settle any Chrome whose owner died before we add another one. This is the
	// SIGKILL fallback in action: the note on disk outlives the process that
	// wrote it, and the next dw-browser invocation is the first thing able to
	// act on it.
	browser.EnsureChromeRegistryReaped()
	cmd := exec.Command(chromePath, chromeArgs...)
	browser.ApplyDetachedProcAttr(cmd)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start Chrome: %w", err)
	}
	// Detached by design (`dw-browser open` exits, Chrome stays), so this note
	// is not an owner-death trigger — it is what lets `dw-browser gc` find the
	// process group later, once no live session references it any more.
	_, _ = browser.RegisterChromeProcess(browser.ChromeDetachedSession, cmd.Process.Pid,
		browser.ChromeProfileDirFromArgs(chromeArgs), false)
	return cmd.Process.Pid, nil
}

func xvfbPIDFromDisplayManager(dm *browser.DisplayManager) int {
	if dm == nil {
		return 0
	}
	return dm.XvfbPID()
}

// requireChrome 检查 Chrome 是否可用，否则退出码 2 [TC-09-U-25]。
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
	snap    *browser.Snapshot
	outcome browser.Outcome
	err     error
}

// runCLIActionWithTimeout is independent of the ActionEngine's 15s guard and
// covers alternate BrowserCore implementations with the same bounded contract.
func runTypedCLIActionWithTimeout(
	parent context.Context,
	timeout time.Duration,
	action string,
	run func(context.Context) (*browser.Snapshot, browser.Outcome, error),
) (*browser.Snapshot, browser.Outcome, error) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		timeout = cliActTimeout
	}
	if deadline, ok := parent.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, browser.Outcome{Phase: "dispatch", DispatchState: "unknown", Recovery: "Observe before deciding whether another action is safe"}, &cliActTimeoutError{Action: action, Timeout: 0}
		}
		if remaining < timeout {
			timeout = remaining
		}
	}

	actionCtx, cancelAction := context.WithCancel(parent)
	done := make(chan cliActionResult, 1)
	go func() {
		snap, outcome, err := run(actionCtx)
		done <- cliActionResult{snap: snap, outcome: outcome, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	defer cancelAction()
	select {
	case result := <-done:
		return result.snap, result.outcome, result.err
	case <-parent.Done():
		if errors.Is(parent.Err(), context.DeadlineExceeded) {
			return nil, browser.Outcome{Phase: "dispatch", DispatchState: "unknown", Recovery: "Observe before deciding whether another action is safe"}, &cliActTimeoutError{Action: action, Timeout: timeout}
		}
		return nil, browser.Outcome{Phase: "dispatch", DispatchState: "unknown", Recovery: "Observe before deciding whether another action is safe"}, parent.Err()
	case <-timer.C:
		return nil, browser.Outcome{Phase: "dispatch", DispatchState: "unknown", Recovery: "Observe before deciding whether another action is safe"}, &cliActTimeoutError{Action: action, Timeout: timeout}
	}
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
	// nameFullDisplayRunes 单元素 name_full 的展示上限。name_full 是消歧提示而非
	// 正文抽取通道:整卡片文案(实测 401 runes / 581B)会独吞默认 4096B 预算的
	// 1/7,把同屏其他元素挤出清单。截断只削文案,ref/locator/bbox 全保留。
	nameFullDisplayRunes = 120
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
func leanElement(ref browser.ElementRef, scenarios ...browser.Scenario) map[string]interface{} {
	name := ref.NameShort
	if name == "" {
		name = ref.Name
	}
	locator := ref.RecommendedLocator
	if locator == "" {
		locator = browser.RecommendedLocatorForElement(ref)
	}
	scenario := browser.Scenario("")
	if len(scenarios) > 0 {
		scenario = scenarios[0]
	}
	allowed := browser.AllowedActionsForElement(ref, scenario)
	if allowed == nil {
		allowed = []string{}
	}
	el := map[string]interface{}{
		"allowed_actions": allowed,
		"ref":             ref.Ref,
		"role":            ref.Role,
		"name":            name,
		"locator":         locator,
	}
	if capability := browser.SelectCapabilityForElement(ref, scenario); capability != nil {
		el["select"] = capability
	}
	if ref.NameFull != "" && ref.NameFull != name {
		el["name_full"] = clampRunes(ref.NameFull, nameFullDisplayRunes)
	}
	if ref.MatchCount > 1 {
		el["match"] = ref.MatchCount
	}
	if ref.Value != "" && !ref.Sensitive {
		el["value"] = clampRunes(ref.Value, nameFullDisplayRunes)
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

// clampRunes 按 rune 截断并加省略号,不切坏 UTF-8。
func clampRunes(s string, limit int) string {
	runes := []rune(s)
	if limit <= 0 || len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// briefElements 返回 top-N 可操作元素(优先 Interactable),并在 budget 内截断。
//
// 选择是 **角色公平** 的 [BUG-VISIBLE-SET-COLLAPSE]:先按 role 轮转(每轮各 role
// 取下一个),装得下就收,装不下 **跳过继续试下一个**,最后按原 DFS 次序输出。
// 旧实现是"DFS 前缀 + 遇到装不下就整体停",于是同屏一两个把整卡片文案当
// accessible name 的大 button(实测单元素 1416B = 默认 4096B 预算的 35%)就能
// 把它后面的 **全部** 原生 select/checkbox 挤出清单 —— 句柄明明已经铸好
// (user_state 里还在计数),调用方却一个 @rN 都拿不到。
//
// 返回 (元素切片, 总可操作数, 是否截断, 被省略元素的 role→数量)。
func briefElements(refs []browser.ElementRef, topN, budget int, scenarios ...browser.Scenario) ([]map[string]interface{}, int, bool, map[string]int) {
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

	encoded := make([]map[string]interface{}, total)
	sizes := make([]int, total)
	for i, r := range pool {
		el := leanElement(r, scenarios...)
		b, _ := json.Marshal(el)
		encoded[i] = el
		sizes[i] = len(b)
	}

	picked := make([]bool, total)
	running, taken := 0, 0
	for _, i := range roleFairOrder(pool) {
		if taken >= topN {
			break
		}
		if taken > 0 && running+sizes[i] > budget {
			continue // 大文案元素让位,不再把它后面的整类元素一起腰斩
		}
		picked[i] = true
		running += sizes[i]
		taken++
	}

	out := make([]map[string]interface{}, 0, taken)
	omitted := make(map[string]int)
	for i := range pool {
		if picked[i] {
			out = append(out, encoded[i])
			continue
		}
		role := pool[i].Role
		if role == "" {
			role = "unknown"
		}
		omitted[role]++
	}
	if len(omitted) == 0 {
		omitted = nil
	}
	return out, total, len(out) < total, omitted
}

// roleFairOrder 返回 pool 下标的"按 role 轮转"取用次序:role 首次出现的先后决定
// 轮转顺序,每轮各 role 取自己队列里的下一个。保证任何单一 role 都无法凭体积
// 独占预算而把别的 role 整类饿死。
func roleFairOrder(pool []browser.ElementRef) []int {
	queues := make(map[string][]int, 8)
	order := make([]string, 0, 8)
	for i := range pool {
		role := pool[i].Role
		if _, seen := queues[role]; !seen {
			order = append(order, role)
		}
		queues[role] = append(queues[role], i)
	}
	out := make([]int, 0, len(pool))
	for len(out) < len(pool) {
		progressed := false
		for _, role := range order {
			q := queues[role]
			if len(q) == 0 {
				continue
			}
			out = append(out, q[0])
			queues[role] = q[1:]
			progressed = true
		}
		if !progressed {
			break
		}
	}
	return out
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

// evidenceOutPath 在未指定证据文件时,把证据落盘到 runs/<run_id>/<step>-<kind>.png。
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
func buildUserState(snap *browser.Snapshot) map[string]interface{} {
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
	userState["active_tab_url"] = snap.URL
	return userState
}

// runSnap 执行 snap 子命令: 导航到 URL 并输出 A11y 快照。
// Session 模式: dw-browser snap --id <id>
// One-shot 模式: dw-browser snap <url> [flags]
func runActResult(args []string) (commandOutput CommandResult, resultErr error) {
	action, rest, err := parseTypedActionArgs(args)
	if err != nil {
		commandOutput.Payload = actionErrorPayload(browser.Outcome{Phase: "validation", DispatchState: "not-dispatched"}, err)
		return finishCommand(commandOutput, exitRunErr)
	}
	positional, flags, parseErr := parseCommonFlagsE(rest, "act")
	if parseErr != nil {
		return commandOutput, commandError("validation", exitRunErr, parseErr)
	}
	if flags.sessionID == "" || len(positional) != 0 {
		commandOutput.Payload = actionErrorPayload(browser.Outcome{Phase: "validation", DispatchState: "not-dispatched"}, fmt.Errorf("act requires --id and rejects extra arguments"))
		return finishCommand(commandOutput, exitRunErr)
	}
	return runTypedActSessionResult(flags, action)

}

// actionMayOpenTab identifies typed pointer activations that can open a link.
func actionMayOpenTab(action browser.Action) bool {
	switch action.Verb {
	case "click", "tap":
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
// Public input is always a typed request. Waiting and observation are separate commands.

func runTypedActSessionResult(flags commonFlags, request browser.Action) (commandOutput CommandResult, resultErr error) {
	return runActSessionRequestResult(flags, request)

}

func runActSessionRequestResult(flags commonFlags, typedRequest browser.Action) (commandOutput CommandResult, resultErr error) {
	action := typedRequest.Verb
	request := &typedRequest
	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		commandOutput.note("dw-browser act: %v\n", err)
		return finishCommand(commandOutput, exitRunErr)
	}

	if err := browser.SessionAllowsAIInput(sessionInfo); err != nil {
		commandOutput.Payload = actionErrorPayload(browser.Outcome{Phase: "validation", DispatchState: "not-dispatched", Recovery: "Human must yield control before AI input"}, err)
		return finishCommand(commandOutput, exitRunErr)
	}
	if sessionInfo.LastActionOutcome == browser.SessionActionOutcomeUnknown || sessionInfo.LastActionOutcome == browser.SessionActionOutcomeInProgress {
		commandOutput.Payload = actionErrorPayload(browser.Outcome{Phase: "validation", DispatchState: "not-dispatched", Recovery: "Run observe before another action; prior dispatch is unknown"}, fmt.Errorf("prior action outcome unknown"))
		return finishCommand(commandOutput, exitRunErr)
	}

	if request != nil && (request.Target.Kind == "ref" || request.Target.Point != nil) {
		if err := browser.ValidateSessionObservation(sessionInfo); err != nil {
			commandOutput.Payload = actionErrorPayload(browser.Outcome{Phase: "validation", DispatchState: "not-dispatched"}, err)
			return finishCommand(commandOutput, exitRunErr)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// [BUG-FIX CAP-BS09-SESS] Snapshot known targets BEFORE action to detect new-tab opens.
	// pointer/touch 激活 link (target=_blank) → Chrome creates a new page target.
	// We record existing targets here and diff after the action to find newly opened tabs.
	knownTargetIDs := map[string]bool{}
	if actionMayOpenTab(typedRequest) && sessionInfo.DebugPort > 0 {
		if existingTargets, err2 := browser.FetchChromeTargets(sessionInfo.DebugPort); err2 == nil {
			for _, t := range existingTargets {
				if targetID := browser.ExtractDevToolsTargetID(t); targetID != "" {
					knownTargetIDs[targetID] = true
				}
			}
		}
	}

	impl, connectErr := connectSessionE(ctx, sessionInfo, "act", flags)

	if connectErr != nil {
		return commandOutput, commandError("runtime", exitRunErr, connectErr)
	}

	operationID := "operation-" + browser.NewObservationID()
	sessionInfo.LastActionOperationID = operationID
	if setter, ok := impl.(interface{ SetSessionOperationID(string) }); ok {
		setter.SetSessionOperationID(operationID)
	}
	// Restore ref table from session
	impl.RestoreRefsFromSession(sessionInfo.Refs)
	// Persist an uncertainty fence before dispatch. A process crash or watchdog
	// timeout after the browser receives input must never leave pre-action refs
	// looking current to the next CLI process. Keep the in-memory refs only for
	// this already-authorized action.
	// Conservatively retain the privacy posture even when sensitive dispatch fails.
	// The fence persists this before input; no field identity or plaintext is stored.
	if typedRequest.Sensitive {
		sessionInfo.RedactFormValues = true
	}
	actionFence := *sessionInfo
	actionFence.LastActionOutcome = "in_progress"
	actionFence.Refs = nil
	if err := browser.SaveSession(&actionFence); err != nil {
		commandOutput.note("dw-browser: persist action fence: %v\n", err)
		closeSessionCore(impl)
		return finishCommand(commandOutput, exitRunErr)
	}

	snap, canonicalOutcome, err := runTypedCLIActionWithTimeout(ctx, cliActTimeout, action, func(actionCtx context.Context) (*browser.Snapshot, browser.Outcome, error) {
		return impl.ExecuteAction(actionCtx, typedRequest, typedRequest.Verb == "navigate" || typedRequest.Verb == "reload" || typedRequest.Verb == "back" || typedRequest.Verb == "forward", true, browser.InteractionModeVisual)
	})
	if err != nil {
		failedReport := actionFidelityReport(impl)
		// An input failure returns the canonical stage/outcome without a hidden retry.
		commandOutput.note("dw-browser: act failed: %v\n", err)
		// 失败分两种，后果完全不同 [BUG-ACT-FAILURE-REVOKES-OBSERVATION]:
		//   校验期失败(解析错 / ref 不存在 / 定位器无匹配 / 守卫拒绝) —— 没有任何
		//     输入到达页面, 页面不可能变, 上一份 observation 的 @rN 依然成立;
		//     一次打错的定位器不该报废整份可见集。
		//   派发后失败 —— 页面可能已被改动, 句柄不可再信, 必须重新 observe。
		preDispatch := actionFailedBeforeDispatch(impl)
		keptRefs := preDispatch && len(sessionInfo.Refs) > 0
		applyFailedActionOutcome(sessionInfo, preDispatch)
		if !preDispatch {
			browser.InvalidateSessionObservation(sessionInfo)
		}
		if keptRefs {
			commandOutput.note(
				"dw-browser: (校验期失败, 未派发任何输入; 既有 %d 个 @rN 仍然有效, 无需重新 observe)\n",
				len(sessionInfo.Refs))
		}
		if saveErr := browser.SaveSession(sessionInfo); saveErr != nil {
			commandOutput.note("dw-browser: persist action outcome: %v\n", saveErr)
		}
		closeSessionCore(impl)
		outcome := canonicalOutcome
		if preDispatch {
			outcome.Phase = "validation"
			outcome.DispatchState = "not-dispatched"
		}
		var stage *browser.ActionStageError
		if errors.As(err, &stage) {
			outcome.Phase = stage.Phase
			outcome.DispatchState = stage.State
		}
		failedPayload := actionErrorPayload(outcome, err)
		injectActionFidelity(failedPayload, failedReport)
		commandOutput.Payload = failedPayload
		return finishCommand(commandOutput, exitRunErr)
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
	if request != nil {
		if navigated {
			browser.InvalidateSessionObservation(sessionInfo)
		} else {
			browser.RevokeSessionObservationGeometry(sessionInfo)
		}
	}
	sessionInfo.LastActionOutcome = "confirmed"
	if err := browser.SaveSession(sessionInfo); err != nil {
		commandOutput.note("dw-browser: persist confirmed action outcome: %v\n", err)
		closeSessionCore(impl)
		return finishCommand(commandOutput, exitRunErr)
	}

	// [Record tap — BS-09 Phase 2] 若当前 session 有活跃录制，追加此 act 动作为一个 step。
	// 零副作用：无录制状态文件 → 静默跳过；写入错误 → 静默忽略，不影响 act 主路径。
	if err := appendRecordAction(sessionInfo, typedRequest, canonicalOutcome); err != nil {
		closeSessionCore(impl)
		return commandOutput, deliveryCommandError(err)
	}

	// [browser-chrome] zoom 后把页面缩放镜像回 session 文件：Chrome 内的
	// Emulation 覆写跨 CLI 调用驻留，镜像不落盘则下次 observe/守卫按 1.0 折算错。
	zoomAction := typedRequest.Verb == "zoom"
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
			if err := browser.SaveSession(sessionInfo); err != nil {
				closeSessionCore(impl)
				return commandOutput, deliveryCommandError(err)
			}
		}
	}

	// Scrolling changes the witness viewport without returning a fresh elements
	// list. In every human-interaction scenario, invalidate observed authority so
	// the agent must look again before clicking a ref in the new viewport.
	if scenarioUsesSeeToClick(sessionInfo.Scenario) && actionMovesWitnessViewport(typedRequest) {
		browser.InvalidateSessionObservation(sessionInfo)
		for i := range sessionInfo.Refs {
			sessionInfo.Refs[i].Observed = false
		}
		if err := browser.SaveSession(sessionInfo); err != nil {
			closeSessionCore(impl)
			return commandOutput, deliveryCommandError(err)
		}
	}

	output := map[string]interface{}{
		"session_id":         sessionInfo.SessionID,
		"browser_session_id": sessionInfo.BrowserSessionID,
		"session_kind":       sessionInfo.SessionKind,
		"success":            true,
		"operation_id":       operationID,
		"runtime_run_id":     sessionInfo.BrowserRunID,
	}
	if request != nil {
		output["outcome"] = canonicalOutcome
	}
	injectActionFidelity(output, report)
	if zoomAction {
		if bcc, ok := impl.(browser.BrowserChromeCapable); ok {
			output["page_scale"] = bcc.PageScale()
		}
	}
	// 键盘态变化时输出（显式 keyboard op 或焦点自动同步弹起/收起）——
	// agent 须知道视口刚变了（截图/遮挡区随之变）。
	if keyboardChanged || typedRequest.Verb == "keyboard" {
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
	if snap != nil {
		// CHG-DWB-AGENT-CLI: act 默认瘦 — 只返 user_state。
		// Full structural/visual evidence belongs to the explicit observer.

		injectSnapshotState(output, snap)
		output["user_state"] = buildUserState(snap)
		injectEvidenceID(output, sessionInfo)
	}

	// [BUG-FIX CAP-BS09-SESS] Detect newly opened tab after a pointer action.
	// If a new page target appeared (not present before the action), the click opened a
	// target=_blank link. Update session TargetID so the next snap/act connects to the
	// new tab rather than the stale original tab.
	if actionMayOpenTab(typedRequest) && sessionInfo.DebugPort > 0 {
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
					browser.InvalidateSessionObservation(sessionInfo) // new page requires explicit observe
					reconcileSessionHumanFocus(sessionInfo, browser.ActionFidelityReport{}, true)
					if err := browser.SaveSession(sessionInfo); err != nil {
						closeSessionCore(impl)
						return commandOutput, deliveryCommandError(err)
					}
					output["new_tab"] = map[string]interface{}{
						"target_id": tid,
						"url":       turl,
					}
					break
				}
			}
		}
	}

	commandOutput.Payload = output
	closeSessionCore(impl)
	return finishCommand(commandOutput, exitOK)

}

func actionMovesWitnessViewport(action browser.Action) bool {
	if action.Verb == "scrollto" {
		return true
	}
	if action.Verb != "scroll" {
		return false
	}
	return action.Target.Point == nil || action.Target.Kind == "point"
}

func runEvalResult(args []string) (commandOutput CommandResult, resultErr error) {
	positional, flags, parseErr := parseCommonFlagsE(args, "eval")
	if parseErr != nil {
		return commandOutput, commandError("validation", exitRunErr, parseErr)
	}

	if flags.sessionID == "" {
		commandOutput.note("%v\n", "dw-browser eval: requires --id <id>")
		return finishCommand(commandOutput, exitRunErr)
	}
	if len(positional) < 1 {
		commandOutput.note("%v\n", "dw-browser eval: requires <js-expression>")
		return finishCommand(commandOutput, exitRunErr)
	}
	expr := positional[0]

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		commandOutput.note("dw-browser eval: %v\n", err)
		return finishCommand(commandOutput, exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	impl, connectErr := connectSessionE(ctx, sessionInfo, "eval", flags)

	if connectErr != nil {
		return commandOutput, commandError("runtime", exitRunErr, connectErr)
	}

	lock, lockErr := acquireSessionRuntimeInputLock(sessionInfo)
	if lockErr != nil {
		closeSessionCore(impl)
		return commandOutput, commandError("validation", exitRunErr, lockErr)
	}
	defer lock.Release()
	if err := browser.SessionAllowsAIInput(sessionInfo); err != nil {
		closeSessionCore(impl)
		return commandOutput, commandError("validation", exitRunErr, err)
	}
	var liveURL string
	if err := impl.EvalJS(ctx, "window.location.href", &liveURL); err != nil {
		closeSessionCore(impl)
		return commandOutput, commandError("validation", exitRunErr, err)
	}
	decision := sessionInfo.Policy.EvaluateAct("eval", liveURL)
	if !decision.Allowed {
		closeSessionCore(impl)
		return commandOutput, commandError("validation", exitRunErr, fmt.Errorf("%s", decision.Reason))
	}
	if err := browser.ApplySessionIntervention(sessionInfo, "operation-"+browser.NewObservationID(), "eval"); err != nil {
		closeSessionCore(impl)
		return commandOutput, commandError("validation", exitRunErr, err)
	}
	if err := browser.SaveSession(sessionInfo); err != nil {
		closeSessionCore(impl)
		return commandOutput, deliveryCommandError(err)
	}
	var result interface{}
	if err := impl.EvalJS(ctx, expr, &result); err != nil {
		closeSessionCore(impl)
		return commandOutput, dispatchCommandError(err)
	}
	sessionInfo.LastActionOutcome = browser.SessionActionOutcomeConfirmed
	if err := browser.SaveSession(sessionInfo); err != nil {
		closeSessionCore(impl)
		return commandOutput, deliveryCommandError(err)
	}
	result = map[string]any{"result": result, "intervention": true, "observation_invalidated": true, "session_id": sessionInfo.SessionID, "runtime_run_id": sessionInfo.BrowserRunID}

	// 序列化结果为 JSON
	enc, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		commandOutput.note("dw-browser eval: marshal result: %v\n", err)
		closeSessionCore(impl)
		return finishCommand(commandOutput, exitRunErr)
	}
	commandOutput.Payload = json.RawMessage(enc)
	closeSessionCore(impl)
	return finishCommand(commandOutput, exitOK)

}

// ============================================================
// § runCookieImport — P1: cookie-import 命令 [SC-22, TC-C4-07~11]
// [Ref: CAP-BS09-C4 §3.2b, r2 Delta-REQ TH-0418-c9x]
// ============================================================

// runCookieImport 从本机浏览器导入 Cookie 到当前 session。
// 用法: dw-browser cookie-import --id <id> [--browser chrome|firefox] [--domain <filter>]
func runCookieImportResult(args []string) (commandOutput CommandResult, resultErr error) {
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

	_, flags, parseErr := parseCommonFlagsE(cleanArgs, "cookie-import")

	if parseErr != nil {
		return commandOutput, commandError("validation", exitRunErr, parseErr)
	}

	if flags.sessionID == "" {
		commandOutput.note("%v\n", "dw-browser cookie-import: requires --id <id>")
		return finishCommand(commandOutput, exitRunErr)
	}

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		commandOutput.note("dw-browser cookie-import: %v\n", err)
		return finishCommand(commandOutput, exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	impl, connectErr := connectSessionE(ctx, sessionInfo, "cookie-import", flags)

	if connectErr != nil {
		return commandOutput, commandError("runtime", exitRunErr, connectErr)
	}

	importer := browser.NewCookieImporter(impl)
	result, err := importer.Import(ctx, sourceBrowser, domainFilter)
	if err != nil {
		commandOutput.note("dw-browser cookie-import: %v\n", err)
		if err == browser.ErrCookieDecryptFailed {
			commandOutput.note("%v\n", "提示: 请在系统设置中授权 Terminal 访问 Keychain")
		} else if err == browser.ErrCookieDBLocked {
			commandOutput.note("%v\n", "提示: Cookie 数据库锁定，请关闭源浏览器后重试")
		}
		closeSessionCore(impl)
		return finishCommand(commandOutput, exitRunErr)
	}

	output := map[string]interface{}{
		"total_imported": result.TotalImported,
		"source_browser": result.SourceBrowser,
		"by_domain":      result.ByDomain,
	}
	commandOutput.Payload = output
	commandOutput.note("已导入 %d 个 Cookie（来源: %s）\n", result.TotalImported, result.SourceBrowser)
	closeSessionCore(impl)
	return finishCommand(commandOutput, exitOK)

}

// runOpenSafari 使用 Safari 引擎打开 URL，持久化 session。
// 由 runOpen 在 --engine safari 时调用。
func runOpenSafariResult(url string, flags commonFlags, scenarioPolicy browser.SessionPolicy) (commandOutput CommandResult, resultErr error) {
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
			commandOutput.note("%v\n", "Safari WebDriver not enabled. Run: sudo safaridriver --enable")
		}
		commandOutput.note("dw-browser open (safari): init failed: %v\n", err)
		return finishCommand(commandOutput, exitRunErr)
	}
	closeSafari := func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer closeCancel()
		_ = core.Close(closeCtx)
	}

	openSnap, err := core.Navigate(ctx, url)
	if err != nil {
		closeSafari()
		commandOutput.note("dw-browser open (safari): navigate failed: %v\n", err)
		return finishCommand(commandOutput, exitRunErr)
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
		commandOutput.note("dw-browser open (safari): save session: %v\n", err)
		return finishCommand(commandOutput, exitRunErr)
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
	commandOutput.Payload = output
	return finishCommand(commandOutput, exitOK)

}

// runOpen 启动浏览器，导航到 URL，持久化 session。
// 支持 Chrome (默认) 和 Safari (--engine safari) 引擎。
func runOpenResult(args []string) (commandOutput CommandResult, resultErr error) {
	positional, flags, parseErr := parseCommonFlagsE(args, "open")
	if parseErr != nil {
		return commandOutput, commandError("validation", exitRunErr, parseErr)
	}
	if flags.sessionID == "" {
		commandOutput.note("%v\n", "dw-browser open: requires --id <id>")
		return finishCommand(commandOutput, exitRunErr)
	}
	if _, err := browser.LoadSession(flags.sessionID); err == nil || !errors.Is(err, browser.ErrSessionNotFound) {
		return commandOutput, commandError("validation", exitRunErr, fmt.Errorf("session id %q already exists or cannot be read; creation refused", flags.sessionID))
	}
	// --scenario 是 session-creating 命令的必选业务主入口: Policy/render/kind 全由它导出。
	_, scenarioPolicy, scenarioErr := resolveScenarioE(&flags, "open")
	if scenarioErr != nil {
		return commandOutput, commandError("validation", exitRunErr, scenarioErr)
	}
	if flags.url != "" {
		positional = append([]string{flags.url}, positional...)
	}
	if len(positional) < 1 {
		commandOutput.note("%v\n", "dw-browser open: requires <url>")
		return finishCommand(commandOutput, exitRunErr)
	}
	url := positional[0]
	if err := browser.ValidateNavigationURL(url); err != nil {
		return commandOutput, commandError("validation", exitRunErr, err)
	}

	// Safari 引擎分支：完全绕开 Chrome/CDP 路径
	if flags.engine == "safari" {
		if flags.browserChrome == browser.BrowserChromeOn {
			commandOutput.note("%v\n", "dw-browser open: --browser-chrome=on 不适用于 --engine safari（真机 Simulator 自带真实 Safari chrome）")
			return finishCommand(commandOutput, exitRunErr)
		}
		return runOpenSafariResult(url, flags, scenarioPolicy)
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
			commandOutput.note("dw-browser open: %v\n", bcErr)
			return finishCommand(commandOutput, exitRunErr)
		}
		if on {
			browserChromeOn = true
			flags.viewportH = chromePreset.BrowserChrome.LargeViewportH()
			flags.hasViewport = true
		} else if chromePreset != nil && chromePreset.BrowserChrome != nil && flags.viewport != "" && flags.browserChrome != browser.BrowserChromeOff {
			// [评审 finding-7] mobile 类 persona 叠显式 --viewport 会静默失去 chrome
			// 仿真承诺 —— 如实提示，不静默。
			commandOutput.note("%v\n", "dw-browser open: 提示: 显式 --viewport 已关闭该 persona 的 Safari chrome 仿真(几何归设备); 需要仿真请去掉 --viewport")
		}
	}

	// Find Chrome binary
	launcher := browser.NewChromeLauncher()
	chromePath, err := launcher.FindChrome()
	if err != nil {
		commandOutput.note("dw-browser open: %v\n", err)
		return finishCommand(commandOutput, exitRunErr)
	}

	// Find free port
	port, err := browser.FindFreePort()
	if err != nil {
		commandOutput.note("dw-browser open: %v\n", err)
		return finishCommand(commandOutput, exitRunErr)
	}

	// Resolve profile dir. Snap Chromium is routed through its writable common dir.
	profileID := resolveProfileID(flags, defaultProfileID(flags))
	profileDir := browser.BrowserCLIProfileDir(chromePath, profileID)
	mode := browser.NormalizeBrowserMode(flags.mode, browser.ModeVisible)

	// cleanup tears down whatever Chrome/Xvfb/BrowserMuxHost open has actually
	// started so far; it grows as each resource comes up and is disarmed once
	// SaveSession succeeds (open's contract is "launch, hand off, exit — the
	// browser outlives the CLI", so success must NOT tear anything down).
	// Typed returns and panic unwinding run the deferred cleanup until the
	// persisted session takes ownership of the launched resources.
	cleanup := func() {}
	defer func() { cleanup() }()
	exit := func(code int) (CommandResult, error) {
		return finishCommand(commandOutput, code)
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
			commandOutput.note("dw-browser open: startup recovery: %v\n", err)
			return exit(exitRunErr)
		}
	}

	if err := os.MkdirAll(profileDir, 0755); err != nil {
		commandOutput.note("dw-browser open: mkdir profile: %v\n", err)
		return exit(exitRunErr)
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
				commandOutput.note("%v\n", "dw-browser open: visible mode unavailable on linux: Xvfb display setup failed")
				return exit(exitRunErr)
			}
			displayBackend = "xvfb"
			cleanup = func() {
				dm.Close()
				removeEphemeralProfileDirIfNeeded()
			}
		}
	default:
		commandOutput.note("dw-browser open: unsupported browser mode %q\n", mode)
		return exit(exitRunErr)
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
			commandOutput.note("dw-browser open: ensure BrowserMuxHost: %v\n", err)
			return exit(exitRunErr)
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
			commandOutput.note("[workspace] EnsureSpace: %v (Chrome will appear on current Space)\n", wsErr)
		} else if spaceID > 0 {
			commandOutput.note("[workspace] using isolation Space %d\n", spaceID)
		}
		handle, err := ws.LaunchChromeInSpace(browser.ChromeLaunchSpec{
			ChromePath:   chromePath,
			Args:         chromeArgs,
			DebugPort:    port,
			ReadyTimeout: 120 * time.Second,
		})
		if err != nil {
			commandOutput.note("dw-browser open: workspace launch chrome: %v\n", err)
			return exit(exitRunErr)
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
			commandOutput.note("dw-browser open: %v\n", err)
			return exit(exitRunErr)
		}
		cleanup = func() {
			browser.KillChromeProcessGroup(chromePID)
			removeEphemeralProfileDirIfNeeded()
		}
		wsURL, err = browser.WaitForChromeReady(port, 120*time.Second)
		if err != nil {
			commandOutput.note("dw-browser open: %v\n", err)
			return exit(exitRunErr)
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
			commandOutput.note("dw-browser open: write profile owner marker: %v\n", err)
			return exit(exitRunErr)
		}
	}

	// Connect via chromedp and navigate
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	initialTargetID, err := ensurePageTargetReady(wsURL, port)
	if err != nil {
		commandOutput.note("dw-browser open: ensure page target: %v\n", err)
		return exit(exitRunErr)
	}

	impl, err := browser.NewBrowserCoreFromSession(ctx, wsURL, initialTargetID, sessionPresetID, sessionPersonaID, mode)
	if err != nil {
		commandOutput.note("dw-browser open: connect: %v\n", err)
		return exit(exitRunErr)
	}
	applyInteractionScenario(impl, flags.scenario)
	defer impl.Close(ctx)
	if err := replayViewportProfileE(impl, sessionPresetID, width, height, flags.personaTouch, "open"); err != nil {
		commandOutput.note("%v\n", err)
		return exit(exitRunErr)
	}
	if browserChromeOn {
		if bcc, ok := impl.(browser.BrowserChromeCapable); ok {
			if !bcc.EnableBrowserChromeSim() {
				commandOutput.note("dw-browser open: enable browser-chrome sim failed for preset %q\n", sessionPresetID)
				return exit(exitRunErr)
			}
			// 视口事实（REQ-BC-11）：shim 须在首次导航前注册（install=true 仅此
			// 一处），页面脚本加载即读到真机同构的 vv/innerHeight；svh override
			// 在导航提交时复位，由 frameNavigated 监听 + Navigate 钩子重放——
			// 首屏存在毫秒级窗口（页面 document-start 一次性读 100svh 且缓存的
			// 场景可能取到 lvh 旧值；CSS 用法随重放自动重算）。建立失败 =
			// 会话保真度谎言源头，fail-loud。
			if err := bcc.ApplyViewportFacts(ctx, false, true); err != nil {
				commandOutput.note("dw-browser open: establish viewport facts: %v\n", err)
				return exit(exitRunErr)
			}
		}
	}

	openSnap, err := impl.Navigate(ctx, url)
	if err != nil {
		commandOutput.note("dw-browser open: navigate: %v\n", err)
		return exit(exitRunErr)
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
		EvidenceRoot:          flags.evidenceDir,
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
		commandOutput.note("dw-browser open: save session: %v\n", err)
		return exit(exitRunErr)
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
	commandOutput.Payload = output
	return exit(exitOK)

}

// runClose 关闭 session（杀 Chrome 进程，删 session 文件）。
func runCloseResult(args []string) (commandOutput CommandResult, resultErr error) {
	_, flags, parseErr := parseCommonFlagsE(args, "close")
	if parseErr != nil {
		return commandOutput, commandError("validation", exitRunErr, parseErr)
	}
	if flags.sessionID == "" {
		commandOutput.note("%v\n", "dw-browser close: requires --id <id>")
		return finishCommand(commandOutput, exitRunErr)
	}

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		commandOutput.note("dw-browser close: %v\n", err)
		return finishCommand(commandOutput, exitRunErr)
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
				commandOutput.note("dw-browser close: BrowserMuxHost shutdown fallback: %v\n", shutdownErr)
			}
			cancel()
		} else if sessionInfo.BrowserMuxHostID != "" {
			commandOutput.note("dw-browser close: BrowserMuxHost state not found: %v\n", loadErr)
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
		commandOutput.note("dw-browser close: delete session: %v\n", err)
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
	commandOutput.Payload = json.RawMessage(enc)
	return finishCommand(commandOutput, exitOK)

}

// runGet 执行 get 子命令（text/url/title）。
func runAuditResult(args []string) (commandOutput CommandResult, resultErr error) {
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
				commandOutput.note("dw-browser audit: --threshold must be an integer\n")
				return finishCommand(commandOutput, exitRunErr)
			}
			threshold = v
			i += 2
		case strings.HasPrefix(arg, "--threshold="):
			v, err := strconv.Atoi(arg[len("--threshold="):])
			if err != nil {
				commandOutput.note("dw-browser audit: --threshold must be an integer\n")
				return finishCommand(commandOutput, exitRunErr)
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

	_, flags, parseErr := parseCommonFlagsE(cleanArgs, "audit")

	if parseErr != nil {
		return commandOutput, commandError("validation", exitRunErr, parseErr)
	}

	if flags.sessionID == "" {
		commandOutput.note("%v\n", "dw-browser audit: requires --id <id>")
		return finishCommand(commandOutput, exitRunErr)
	}

	registry := audit.NewRegistry()
	audit.RegisterCompat(registry)
	audit.RegisterA11y(registry)
	audit.RegisterLayout(registry)
	audit.RegisterPerf(registry)
	audit.RegisterChrome(registry)

	if suite != "" {
		if _, ok := audit.Suites[suite]; !ok {
			return commandOutput, commandError("validation", exitRunErr, fmt.Errorf("unknown audit suite %q", suite))
		}
	}
	if check != "" {
		rule := registry.ByID(check)
		if rule == nil {
			return commandOutput, commandError("validation", exitRunErr, fmt.Errorf("unknown audit check %q", check))
		}
		if suite != "" && !audit.Suites[suite].Filter(*rule) {
			return commandOutput, commandError("validation", exitRunErr, fmt.Errorf("check %q is not in suite %q", check, suite))
		}
	}

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		commandOutput.note("dw-browser audit: %v\n", err)
		return finishCommand(commandOutput, exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	impl, connectErr := connectSessionE(ctx, sessionInfo, "audit", flags)

	if connectErr != nil {
		return commandOutput, commandError("runtime", exitRunErr, connectErr)
	}

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
		commandOutput.note("dw-browser audit: %v\n", runErr)
		closeSessionCore(impl)
		return finishCommand(commandOutput, exitRunErr)
	}

	switch format {
	case "compact":
		commandOutput.Payload = map[string]any{"content": report.FormatCompact()}
	default:
		out, err := report.FormatJSON()
		if err != nil {
			commandOutput.note("dw-browser audit: marshal report: %v\n", err)
			closeSessionCore(impl)
			return finishCommand(commandOutput, exitRunErr)
		}
		commandOutput.Payload = json.RawMessage(out)
	}

	if runErr != nil {
		commandOutput.note("AUDIT FAILED: score %d < threshold %d\n", report.Score, threshold)
		closeSessionCore(impl)
		return finishCommand(commandOutput, exitFail)
	}
	closeSessionCore(impl)
	return finishCommand(commandOutput, exitOK)

}
