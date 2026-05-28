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
	"fmt"
	"io"
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
	btest "github.com/brightman-ai/deepwork-browser/browser/testing"
	cdpTarget "github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"gopkg.in/yaml.v3"
)

const version = "0.1.0"

// exitCodes [IR-08]
const (
	exitOK     = 0 // 全 PASS
	exitFail   = 1 // 断言失败
	exitRunErr = 2 // 运行错误（Chrome 未找到等）
)

// ============================================================
// § 设备预设表（Chrome/CDP 表观模拟）[TH-0405-p7c 修改 1]
// ============================================================

// DevicePreset 设备预设参数。
// 注意: 这些 preset 只驱动 Chrome/CDP 的 viewport、UA 和 touch 能力，
// 不等同于真实 iOS Safari / WebKit 渲染环境。
type DevicePreset struct {
	Width  int
	Height int
	UA     string
	Touch  bool
}

// devicePresets 内置设备预设表。
var devicePresets = map[string]DevicePreset{
	"iphone-14": {
		Width: 390, Height: 844,
		UA:    "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
		Touch: true,
	},
	"iphone-15-pro": {
		Width: 393, Height: 852,
		UA:    "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
		Touch: true,
	},
	"ipad-air": {
		Width: 820, Height: 1180,
		UA:    "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
		Touch: true,
	},
	"pixel-7": {
		Width: 412, Height: 915,
		UA:    "Mozilla/5.0 (Linux; Android 14; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
		Touch: true,
	},
	"galaxy-s23": {
		Width: 360, Height: 780,
		UA:    "Mozilla/5.0 (Linux; Android 14; SM-S911B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
		Touch: true,
	},
}

// ============================================================
// § CLI Flags 通用结构
// ============================================================

// commonFlags 所有子命令共享的 flags。
type commonFlags struct {
	profileID         string
	sessionID         string // --id/--session <id>
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
	device            string // 设备预设名
	userAgent         string
	diag              bool // --diag 启用每步 observation log
	stealth           bool // --stealth 只在 headless 下生效：反检测 UA + 额外 flags
	// Safari 引擎
	engine       string // --engine: "chrome" (default) | "safari"
	safariDevice string // --safari-device: Safari Simulator 设备名/UDID
	// 解析后
	viewportW   int
	viewportH   int
	hasViewport bool
	hasUA       bool
	hasDevice   bool
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
		case arg == "--session" && i+1 < len(args):
			flags.sessionID = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--session="):
			flags.sessionID = arg[len("--session="):]
			i++
		case arg == "--kind" && i+1 < len(args):
			kind, ok := parseBrowserSessionKind(args[i+1])
			if !ok {
				fmt.Fprintf(os.Stderr, "dw-browser %s: --kind 值无效 %q (允许: task/interactive/service/debug/test)\n", cmd, args[i+1])
				os.Exit(exitRunErr)
			}
			flags.sessionKind = kind
			i += 2
		case strings.HasPrefix(arg, "--kind="):
			val := arg[len("--kind="):]
			kind, ok := parseBrowserSessionKind(val)
			if !ok {
				fmt.Fprintf(os.Stderr, "dw-browser %s: --kind 值无效 %q (允许: task/interactive/service/debug/test)\n", cmd, val)
				os.Exit(exitRunErr)
			}
			flags.sessionKind = kind
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
		case arg == "--device" && i+1 < len(args):
			flags.device = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--device="):
			flags.device = arg[len("--device="):]
			i++
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
		case arg == "--stealth":
			flags.stealth = true
			i++
		case arg == "--engine" && i+1 < len(args):
			flags.engine = strings.ToLower(strings.TrimSpace(args[i+1]))
			i += 2
		case strings.HasPrefix(arg, "--engine="):
			flags.engine = strings.ToLower(strings.TrimSpace(arg[len("--engine="):]))
			i++
		case arg == "--safari-device" && i+1 < len(args):
			flags.safariDevice = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--safari-device="):
			flags.safariDevice = arg[len("--safari-device="):]
			i++
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

	// 解析 --device（优先级高于 --viewport / --user-agent）
	if flags.device != "" {
		preset, ok := devicePresets[flags.device]
		if !ok {
			fmt.Fprintf(os.Stderr, "dw-browser: 未知设备预设 %q。可用预设:\n", flags.device)
			printDevicePresets()
			os.Exit(exitRunErr)
		}
		flags.viewportW = preset.Width
		flags.viewportH = preset.Height
		flags.userAgent = preset.UA
		flags.hasViewport = true
		flags.hasUA = true
		flags.hasDevice = true
		// --viewport / --user-agent 若同时指定则覆盖设备预设
	}

	// 解析 --viewport（覆盖设备预设的 viewport）
	if flags.viewport != "" {
		w, h, err := parseViewport(flags.viewport)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser: --viewport 格式错误 %q，应为 WxH（如 1920x1080）\n", flags.viewport)
			os.Exit(exitRunErr)
		}
		flags.viewportW = w
		flags.viewportH = h
		flags.hasViewport = true
	}

	// --user-agent 单独指定（覆盖设备预设的 UA）
	if !flags.hasDevice && flags.userAgent != "" {
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
		return fmt.Sprintf("ephemeral-%d-%d", os.Getpid(), time.Now().UnixNano())
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
	if flags.hasDevice {
		preset := devicePresets[flags.device]
		opts = append(opts, browser.WithTouchEmulation(preset.Touch))
	}
	opts = append(opts, browser.WithMode(browser.NormalizeBrowserMode(flags.mode, browser.ModeVisible)))
	if flags.stealth {
		opts = append(opts, browser.WithStealth(true))
	}
	return opts
}

// resolveSessionPresetID 为 dw-browser session/open 链路挑选一个合理的指纹 preset。
// 优先根据显式 UA/设备推断，否则回退到当前平台默认 preset。
func resolveSessionPresetID(flags commonFlags) string {
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
	homeDir, _ := os.UserHomeDir()
	profilePath := fmt.Sprintf("%s/.deepwork/browser-cli/%s", homeDir, profileID)
	os.RemoveAll(profilePath)
}

func main() {
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
	case "view":
		runView(os.Args[2:])
	case "once":
		runOnce(os.Args[2:])
	case "muxhost":
		runMuxHost(os.Args[2:])
	case "batch":
		runBatch(os.Args[2:])
	case "tabs":
		runTabs(os.Args[2:])
	case "htr":
		runHTR(os.Args[2:])
	case "open":
		runOpen(os.Args[2:])
	case "close":
		runClose(os.Args[2:])
	case "snap":
		runSnap(os.Args[2:])
	case "act":
		runAct(os.Args[2:])
	case "get":
		runGet(os.Args[2:])
	case "wait":
		runWait(os.Args[2:])
	case "screenshot":
		runScreenshot(os.Args[2:])
	case "test":
		runTest(os.Args[2:])
	case "layout":
		runLayout(os.Args[2:])
	case "explore":
		runExplore(os.Args[2:])
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
	case "state":
		runState(os.Args[2:])
	case "journey":
		runJourney(os.Args[2:])
	case "plan":
		runPlan(os.Args[2:])
	case "do":
		runDo(os.Args[2:])
	case "test-help":
		printTestingHelp()
		os.Exit(exitOK)
	default:
		fmt.Fprintf(os.Stderr, "dw-browser: unknown command %q\n", cmd)
		printUsage()
		os.Exit(exitRunErr)
	}
}

// printDevicePresets 打印设备预设列表。
func printDevicePresets() {
	fmt.Println("  内置设备预设 (--device):")
	fmt.Println("    说明: --device 运行在 Chrome/CDP 中，只模拟 viewport/UA/touch；")
	fmt.Println("          不能替代真实 iOS Safari / WebKit / Appium 真机或模拟器验收。")
	fmt.Printf("    %-16s  %s\n", "预设名", "视口 / User-Agent 摘要")
	fmt.Printf("    %-16s  %s\n", "------", "----------------------")
	presetOrder := []string{"iphone-14", "iphone-15-pro", "ipad-air", "pixel-7", "galaxy-s23"}
	for _, name := range presetOrder {
		p := devicePresets[name]
		ua := p.UA
		if len(ua) > 50 {
			ua = ua[:47] + "..."
		}
		fmt.Printf("    %-16s  %dx%d  %s\n", name, p.Width, p.Height, ua)
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

func runView(args []string) {
	if len(args) == 1 && wantsHelp(args) {
		printCommandUsage("view")
		os.Exit(exitOK)
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser view: requires subcommand (action|reading|state|evidence)")
		os.Exit(exitRunErr)
	}
	switch args[0] {
	case "action":
		runSnap(args[1:])
	case "reading":
		runViewReading(args[1:])
	case "state":
		runViewState(args[1:])
	case "evidence":
		runScreenshot(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "dw-browser view: unknown subcommand %q (use action|reading|state|evidence)\n", args[0])
		os.Exit(exitRunErr)
	}
}

// runViewReading handles `view reading [--format plain|list|table] [--wait <ms>]`.
// plain (default) → delegates to runGet "text" (existing path)
// list            → extract <li> items via JS
// table           → extract all <table> rows via JS
func runViewReading(args []string) {
	format := "plain"
	waitMs := 0
	clean := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--format" && i+1 < len(args):
			format = strings.ToLower(strings.TrimSpace(args[i+1]))
			i++
		case strings.HasPrefix(arg, "--format="):
			format = strings.ToLower(strings.TrimSpace(arg[len("--format="):]))
		case arg == "--wait" && i+1 < len(args):
			if n, err := strconv.Atoi(args[i+1]); err == nil {
				waitMs = n
			}
			i++
		case strings.HasPrefix(arg, "--wait="):
			if n, err := strconv.Atoi(arg[len("--wait="):]); err == nil {
				waitMs = n
			}
		default:
			clean = append(clean, arg)
		}
	}

	if format == "plain" {
		// delegate to existing path; --wait not handled there (SPA heuristic only needed for structured formats)
		runGet(append([]string{"text"}, clean...))
		return
	}

	if format != "list" && format != "table" {
		fmt.Fprintf(os.Stderr, "dw-browser view reading: unknown --format %q (use plain|list|table)\n", format)
		os.Exit(exitRunErr)
	}

	_, flags := parseCommonFlags(clean, "view reading")
	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser view reading: requires --session <id> for --format list|table")
		os.Exit(exitRunErr)
	}

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser view reading: %v\n", err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	impl := connectSession(ctx, sessionInfo, "view reading", flags)

	if waitMs > 0 {
		time.Sleep(time.Duration(waitMs) * time.Millisecond)
	}

	var currentURL, title string
	impl.EvalJS(ctx, "window.location.href", &currentURL)
	impl.EvalJS(ctx, "document.title", &title)

	switch format {
	case "list":
		var items []string
		jsExpr := `Array.from(document.querySelectorAll('li')).map(li=>li.innerText.trim()).filter(Boolean)`
		if err := impl.EvalJS(ctx, jsExpr, &items); err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser view reading list: %v\n", err)
			exitSessionCore(impl, exitRunErr)
		}
		output := map[string]interface{}{
			"view":   "reading",
			"format": "list",
			"url":    currentURL,
			"title":  title,
			"items":  items,
		}
		enc, _ := json.MarshalIndent(output, "", "  ")
		fmt.Println(string(enc))

	case "table":
		var tables [][][]string
		jsExpr := `Array.from(document.querySelectorAll('table')).map(tbl=>Array.from(tbl.querySelectorAll('tr')).map(tr=>Array.from(tr.querySelectorAll('th,td')).map(td=>td.innerText.trim())))`
		if err := impl.EvalJS(ctx, jsExpr, &tables); err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser view reading table: %v\n", err)
			exitSessionCore(impl, exitRunErr)
		}
		output := map[string]interface{}{
			"view":   "reading",
			"format": "table",
			"url":    currentURL,
			"title":  title,
			"tables": tables,
		}
		enc, _ := json.MarshalIndent(output, "", "  ")
		fmt.Println(string(enc))
	}

	exitSessionCore(impl, exitOK)
}

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

// runProfile handles `dw-browser profile list|import`.
// profile list   → lists profiles under ~/.deepwork/browser-cli/
// profile import <src> [--name <name>] → copies src dir into the profile dir
//
// Both use ~/.deepwork/browser-cli/ — the same dir that --profile <name>
// resolves to (see resolveProfileID → browser-cli/{profileID}). Imported
// profiles must live here or --profile would never find them.
func runProfile(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser profile: requires subcommand (list|import)")
		os.Exit(exitRunErr)
	}
	switch args[0] {
	case "list":
		homeDir, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser profile list: %v\n", err)
			os.Exit(exitRunErr)
		}
		profilesDir := filepath.Join(homeDir, ".deepwork", "browser-cli")
		entries, err := os.ReadDir(profilesDir)
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "dw-browser profile list: %v\n", err)
			os.Exit(exitRunErr)
		}
		type profileEntry struct {
			Name   string  `json:"name"`
			Path   string  `json:"path"`
			SizeMB float64 `json:"size_mb"`
		}
		profiles := []profileEntry{}
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
		output := map[string]interface{}{"profiles": profiles}
		enc, _ := json.MarshalIndent(output, "", "  ")
		fmt.Println(string(enc))
		os.Exit(exitOK)

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

		homeDir, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser profile import: %v\n", err)
			os.Exit(exitRunErr)
		}
		profilesDir := filepath.Join(homeDir, ".deepwork", "browser-cli")
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
		fmt.Fprintf(os.Stderr, "dw-browser profile: unknown subcommand %q (use list|import)\n", args[0])
		os.Exit(exitRunErr)
	}
}

func runViewState(args []string) {
	_, flags := parseCommonFlags(args, "view state")
	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser view state: requires --id <id>")
		os.Exit(exitRunErr)
	}
	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser view state: %v\n", err)
		os.Exit(exitRunErr)
	}
	currentTarget := map[string]interface{}{}
	targets := []map[string]interface{}{}
	if sessionInfo.DebugPort > 0 {
		if fetched, fetchErr := browser.FetchChromeTargets(sessionInfo.DebugPort); fetchErr == nil {
			targets = fetched
			for _, target := range fetched {
				if browser.ExtractDevToolsTargetID(target) == sessionInfo.TargetID {
					currentTarget = target
					break
				}
			}
		}
	}
	output := map[string]interface{}{
		"browser_session_id": sessionInfo.BrowserSessionID,
		"session_kind":       sessionInfo.SessionKind,
		"authority_state":    sessionInfo.AuthorityState,
		"current_target":     currentTarget,
		"targets":            targets,
	}
	enc, _ := json.MarshalIndent(output, "", "  ")
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	defer func() {
		bc.Close(ctx)
		if flags.ephemeral || flags.isolation == browser.SessionIsolationEphemeral {
			cleanupEphemeral(profileID)
		}
	}()

	snap, err := navigateWithRetry(ctx, bc, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser once: navigate failed: %v\n", err)
		os.Exit(exitRunErr)
	}

	switch strings.ToLower(strings.TrimSpace(view)) {
	case "action":
		if action != "" {
			snap, err = actWithRetry(ctx, bc, action, true)
			if err != nil {
				fmt.Fprintf(os.Stderr, "dw-browser once: act failed: %v\n", err)
				os.Exit(exitFail)
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
				os.Exit(exitRunErr)
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
				os.Exit(exitRunErr)
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
				os.Exit(exitRunErr)
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
			os.Exit(exitRunErr)
		}
	case "evidence":
		data, err := screenshotWithRetry(ctx, bc, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser once evidence: %v\n", err)
			os.Exit(exitRunErr)
		}
		if err := os.WriteFile(outFile, data, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser once evidence: write screenshot: %v\n", err)
			os.Exit(exitRunErr)
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
		os.Exit(exitRunErr)
	}
	os.Exit(exitOK)
}

// printUsage 打印使用说明。
func printUsage() {
	fmt.Println("dw-browser — BrowserSession 驱动的 AOT/HTR 浏览器运行时")
	fmt.Println()
	fmt.Println("公开命令:")
	fmt.Println("  dw-browser session start --id <id> --kind task|interactive|service|debug|test --url <url>")
	fmt.Println("  dw-browser session start --id <id> --kind task <url>")
	fmt.Println("  dw-browser session status --id <id>")
	fmt.Println("  dw-browser session list")
	fmt.Println("  dw-browser session close --id <id>")
	fmt.Println()
	fmt.Println("视图命令:")
	fmt.Println("  dw-browser view action --id <id> [--selector <selector>] [--compact]")
	fmt.Println("  dw-browser view reading --id <id> [locator]")
	fmt.Println("  dw-browser view state --id <id>")
	fmt.Println("  dw-browser view evidence --id <id> [out.png]")
	fmt.Println()
	fmt.Println("操作命令:")
	fmt.Println("  dw-browser batch --id <id> --file actions.jsonl [--snap]")
	fmt.Println("  dw-browser tabs list|select|close|new --id <id>")
	fmt.Println("  dw-browser htr attach|takeover|yield|share --id <id>")
	fmt.Println()
	fmt.Println("运行时运维:")
	fmt.Println("  dw-browser muxhost status|release|shutdown --id <session-id>")
	fmt.Println("  dw-browser muxhost status --muxhost-id browser-mux-host-global")
	fmt.Println("  dw-browser muxhost ensure --id <session-id> --mode headed|headless")
	fmt.Println()
	fmt.Println("单次命令:")
	fmt.Println("  dw-browser once --url <url> --view action|reading|evidence [--action '<action>']")
	fmt.Println()
	fmt.Println("AI-native 测试命令:")
	fmt.Println("  dw-browser observe --id <id> [--layers structural,behavior,telemetry,layout]")
	fmt.Println("  dw-browser diff before.json after.json")
	fmt.Println("  dw-browser check --id <id> --assert \"console_errors_count == 0\"")
	fmt.Println("  dw-browser journey --file spec.yaml [--evidence dir]")
	fmt.Println("  dw-browser plan --id <id> \"natural language goal\"")
	fmt.Println("  dw-browser do --id <id> \"natural language goal\"")
	fmt.Println("  dw-browser get --id <id> \"active tab url\"")
	fmt.Println()
	fmt.Println("核心参数:")
	fmt.Println("  --id <id>              BrowserSession 本地句柄；--session 仅作为隐藏兼容别名")
	fmt.Println("  --kind <kind>          task/interactive/service/debug/test；不使用 portal/webchat 作为公开 kind")
	fmt.Println("  --goal <text>          本次 BrowserSession 目标")
	fmt.Println("  --profile <id>         dedicated profile；与 --ephemeral 互斥")
	fmt.Println("  --account <id>         service 类登录身份")
	fmt.Println("  --isolation <mode>     ephemeral/dedicated/profile-pool")
	fmt.Println("  --mode <mode>          headless/headed/visible；未指定时由 --kind 决定")
	fmt.Println("  --viewport WxH, --device <preset>, --user-agent <ua>")
	fmt.Println("  --engine <engine>      浏览器引擎: chrome (默认) | safari (iOS Simulator)")
	fmt.Println("  --safari-device <dev>  Safari 引擎: Simulator 设备名或 UDID")
	fmt.Println()
	fmt.Println("默认策略:")
	fmt.Println("  task/test: headless + ephemeral + agentic")
	fmt.Println("  interactive: headed + dedicated + human")
	fmt.Println("  service: headed + dedicated + service account")
	fmt.Println("  debug: visible + dedicated + human")
	fmt.Println()
	printDevicePresets()
	fmt.Println()
	fmt.Println("用法 (Session 模式 — Agent 实时交互):")
	fmt.Println("  dw-browser open <url> --session <id> [flags]      启动 Chrome 并导航")
	fmt.Println("  dw-browser snap --session <id> [flags]            获取 A11y 快照（@rN refs）")
	fmt.Println("  dw-browser act --session <id> <action> [flags]    执行操作（支持 @rN）")
	fmt.Println("  dw-browser get text --session <id> [locator]      提取文本")
	fmt.Println("  dw-browser wait --session <id> <condition>        等待条件")
	fmt.Println("  dw-browser screenshot --session <id> [out.png]    截图")
	fmt.Println("  dw-browser close --session <id>                   关闭会话")
	fmt.Println()
	fmt.Println("用法 (Browser Skills — 站点操作知识库):")
	fmt.Println("  dw-browser skills list                             列出所有已知 skill")
	fmt.Println("  dw-browser skills read <name> [--action <action>]  读取 skill/action")
	fmt.Println("  dw-browser skills write <name> --action <action>   写入 action (stdin)")
	fmt.Println()
	fmt.Println("用法 (Record Mode — 录制操作轨迹 → 提炼 Skill):")
	fmt.Println("  dw-browser record start  --session <id>           开始录制")
	fmt.Println("  dw-browser record stop   --session <id>           停止录制 → stdout: trace JSON")
	fmt.Println("  dw-browser record export --session <id>           导出快照（不停止录制）")
	fmt.Println()
	fmt.Println("用法 (One-shot 模式 — 向后兼容):")
	fmt.Println("  dw-browser snap <url> [flags]                     获取 A11y 快照")
	fmt.Println("  dw-browser act <url> <action> [flags]             执行操作")
	fmt.Println("  dw-browser screenshot <url> [output.png] [flags]  截图")
	fmt.Println("  dw-browser test <spec.yaml> [flags]               运行 YAML 规格测试")
	fmt.Println("  dw-browser layout <url> [flags]                   布局验证 (L2)")
	fmt.Println("  dw-browser explore <url> [flags]                  AI 探索 (见下方详细说明)")
	fmt.Println("  dw-browser version                                打印版本")
	fmt.Println()
	fmt.Println("用法 (AI-native 测试 — 观察 / 断言 / 规划 / 执行):")
	fmt.Println("  dw-browser observe --id s1 --out before.json")
	fmt.Println("  dw-browser check --id s1 --assert \"exists(role='button')\"")
	fmt.Println("  dw-browser plan --id s1 \"Open settings and inspect provider status\"")
	fmt.Println("  dw-browser do --id s1 \"Open settings and inspect provider status\"")
	fmt.Println("  dw-browser journey --file tests/bdd/portal.yaml --evidence evidence/run-001")
	fmt.Println("  dw-browser test-help                              打印完整测试命令说明")
	fmt.Println()
	fmt.Println("═══ explore — 为 Claude Code / AI Agent 设计的浏览器状态查询器 ═══")
	fmt.Println()
	fmt.Println("  explore 在一次调用中返回「浏览器 A11y 状态 + 服务端健康/错误」的组合 JSON，")
	fmt.Println("  消除 AI 需要分别调用 snap + curl /api/health + curl /api/debug/obs/recent 的摩擦。")
	fmt.Println()
	fmt.Println("  使用模式 (Claude Code 循环):")
	fmt.Println("    1. dw-browser explore <url>                       → 观察: 获取页面状态")
	fmt.Println("    2. AI 分析 JSON 输出，决定下一步操作")
	fmt.Println("    3. dw-browser explore <url> --act \"click button:'提交'\"  → 行动: 执行+观察")
	fmt.Println("    4. 重复 2-3 直到目标达成或发现问题")
	fmt.Println("    5. dw-browser explore <url> --report              → 诊断: 全量证据采集")
	fmt.Println()
	fmt.Println("  输出 JSON 结构:")
	fmt.Println("    {")
	fmt.Println("      \"command\": \"explore\",")
	fmt.Println("      \"url\": \"...\",")
	fmt.Println("      \"browser\": { \"url\", \"title\", \"refs_count\", \"refs\": [{ref,role,name,locator}...] },")
	fmt.Println("      \"server\":  { \"health\": {build,runtime}, \"recent_errors\": {entries} },")
	fmt.Println("      \"action\": \"...\",        // 仅 --act 时")
	fmt.Println("      \"screenshot\": \"path\"    // 仅 --report 时")
	fmt.Println("    }")
	fmt.Println()
	fmt.Println("  Explore flags:")
	fmt.Println("    --act <action>     执行操作后再快照 (语义选择器，同 act 命令)")
	fmt.Println("    --report           追加截图 + 完整 health 诊断")
	fmt.Println()
	fmt.Println("通用 flags (所有子命令):")
	fmt.Println("  --session <id>       会话 ID（session 模式必须）")
	fmt.Println("  --viewport WxH       视口大小 (默认: 1920x1080, 例: 390x844)")
	fmt.Println("  --device <preset>    Chrome/CDP 设备表观模拟 (viewport + UA + touch；非真实 Safari/WebKit)")
	fmt.Println("  --user-agent <ua>    自定义 User-Agent (覆盖 --device 的 UA)")
	fmt.Println("  --mode {headless|headed|visible}  浏览器模式 (CLI 默认 visible; human 兼容为 visible)")
	fmt.Println("  --headless           等价于 --mode headless (兼容老 flag)")
	fmt.Println()
	fmt.Println("  Mode 默认值优先级: --mode > --headless > env DW_BROWSER_DEFAULT_MODE > visible")
	fmt.Println("  CI/测试环境建议: export DW_BROWSER_DEFAULT_MODE=headless (Makefile 已默认设置)")
	fmt.Println("  --profile <name>     使用指定 profile 目录 (与 --ephemeral 互斥)")
	fmt.Println("  --ephemeral          自动生成临时 profile，命令结束后删除 (与 --profile 互斥)")
	fmt.Println()
	fmt.Println("定位器语法:")
	fmt.Println("  @r7                             Session ref（仅 session 模式）")
	fmt.Println("  #<testid>                       按 data-testid")
	fmt.Println("  role=button[name*=\"创建\"]        Canonical DSL")
	fmt.Println("  role=button[name=\"删除\"][nth=3]  带 nth 消歧")
	fmt.Println("  dialog:'创建' >> button='确认'    Scoped selector")
	fmt.Println("  button:'<名称>'                  按 ARIA role + name（contains）")
	fmt.Println("  button=\"<名称>\"                  按 ARIA role + name（exact）")
	fmt.Println("  css=.toolbar .btn                CSS 选择器（低稳定性）")
	fmt.Println()
	fmt.Println("act 支持的操作:")
	fmt.Println("  click <locator>                 点击")
	fmt.Println("  clickat <locator> <x> <y>       相对坐标真实点击（0.92/92% 形式）")
	fmt.Println("  tap <locator>                   触控点击（iOS/Android/coarse pointer）")
	fmt.Println("  tapat <locator> <x> <y>         相对坐标真实触控点击（0.92/92% 形式）")
	fmt.Println("  fill <locator> '<text>'         清空后输入")
	fmt.Println("  type <locator> '<text>'         输入（不清空）")
	fmt.Println("  press <key> | press <locator> <key>  按键（Ctrl+A, Enter 等）")
	fmt.Println("  hover <locator>                 鼠标悬停")
	fmt.Println("  scroll down|up                  滚动")
	fmt.Println("  select <locator> '<value>'      下拉选择")
	fmt.Println("  back | forward                  导航历史")
	fmt.Println("  focus <locator>                 聚焦")
	fmt.Println("  scrollinto <locator>            滚动到可见")
	fmt.Println("  check <locator>                 勾选复选框")
	fmt.Println("  uncheck <locator>               取消勾选")
	fmt.Println()
	fmt.Println("wait 条件语法:")
	fmt.Println("  dw-browser wait --session <id> 2000          等待 2000ms")
	fmt.Println("  dw-browser wait --session <id> 'visible #btn' 等待元素可见")
	fmt.Println("  dw-browser wait --session <id> 'gone #mask'   等待元素消失")
	fmt.Println("  dw-browser wait --session <id> 'text 创建成功'  等待文本出现")
	fmt.Println()
	fmt.Println("示例 (Session 模式):")
	fmt.Println("  dw-browser open http://localhost:8080/ws --session ws1 --ephemeral")
	fmt.Println("  dw-browser snap --session ws1")
	fmt.Println("  dw-browser act --session ws1 \"click @r7\"")
	fmt.Println("  dw-browser act --session ws1 \"fill #ws-name 'my workspace'\"")
	fmt.Println("  dw-browser close --session ws1")
	fmt.Println()
	fmt.Println("示例 (One-shot 模式):")
	fmt.Println("  dw-browser snap http://localhost:8080/ --device iphone-14 --ephemeral")
	fmt.Println("  dw-browser act http://localhost:8080/ws \"click button:'New Workspace'\" --ephemeral")
	fmt.Println("  dw-browser act http://localhost:8080/ws \"click #ws-create-btn\" --ephemeral")
	fmt.Println("  dw-browser screenshot http://localhost:8080/ out.png --device pixel-7")
	fmt.Println()
	fmt.Println("act 选择器语法 (语义选择器，不用位置编码 e{N}):")
	fmt.Println("  click #<testid>           按 data-testid 点击")
	fmt.Println("  clickat #canvas 92% 8%    点击元素内部相对坐标")
	fmt.Println("  tap button:'接管'          触控点击元素")
	fmt.Println("  tapat #browser-liveview 92% 8%  触控点击元素内部相对坐标")
	fmt.Println("  click button:'<名称>'      按 ARIA role + name 点击")
	fmt.Println("  type textbox:'<名称>' '<text>'  按 role + name 输入")
	fmt.Println("  click link                按 role 点击第一个匹配")
	fmt.Println()
	printDevicePresets()
	fmt.Println()
	fmt.Println("示例:")
	fmt.Println("  dw-browser snap http://localhost:8080/ --device iphone-14 --ephemeral")
	fmt.Println("  dw-browser act http://localhost:8080/ws \"click button:'New Workspace'\" --ephemeral")
	fmt.Println("  dw-browser act http://localhost:8080/ws \"click #ws-create-btn\" --ephemeral")
	fmt.Println("  dw-browser screenshot http://localhost:8080/ out.png --device pixel-7")
}

func printCommandUsage(command string) {
	switch strings.TrimSpace(command) {
	case "", "dw-browser":
		printUsage()
	case "session", "session status", "session list", "session close", "close":
		fmt.Println("dw-browser session — 管理 BrowserSession 生命周期")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  dw-browser session start --id <id> --kind interactive --mode headed --url <url>")
		fmt.Println("  dw-browser session status --id <id>")
		fmt.Println("  dw-browser session list")
		fmt.Println("  dw-browser session close --id <id>")
		fmt.Println()
		fmt.Println("说明:")
		fmt.Println("  interactive 默认 headed + dedicated + human，macOS 下由 BrowserMuxHost 持有 CGVirtualDisplay。")
	case "session start", "open":
		fmt.Println("dw-browser session start — 启动或恢复一个浏览器会话")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  dw-browser session start --id <id> --kind interactive --mode headed --profile <profile> --url <url>")
		fmt.Println("  dw-browser session start --id <id> --kind task --url <url>")
		fmt.Println("  dw-browser open <url> --id <id> [同等参数]")
		fmt.Println()
		fmt.Println("关键参数:")
		fmt.Println("  --id <id>              本地 BrowserSession 句柄")
		fmt.Println("  --kind <kind>          task/interactive/service/debug/test")
		fmt.Println("  --mode <mode>          headless/headed/visible")
		fmt.Println("  --profile <id>         dedicated profile；适合 Human 主浏览器和登录态")
		fmt.Println("  --isolation <mode>     ephemeral/dedicated/profile-pool")
		fmt.Println("  --url <url>            初始页面")
		fmt.Println("  --goal <text>          场景目标，会写入 session contract")
		fmt.Println("  --viewport WxH         视口大小")
	case "view", "view reading", "view state", "view evidence", "get", "screenshot":
		fmt.Println("dw-browser view — 读取当前会话状态")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  dw-browser view action --id <id> [--selector <selector>] [--compact]")
		fmt.Println("  dw-browser view reading --id <id> [locator]")
		fmt.Println("  dw-browser view state --id <id>")
		fmt.Println("  dw-browser view evidence --id <id> [out.png|--out <path>]")
	case "snap", "view action":
		fmt.Println("dw-browser view action — 获取可操作 A11y 快照")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  dw-browser view action --id <id> [--selector <selector>] [--compact] [--max-depth <n>]")
		fmt.Println("  dw-browser snap --id <id> [同等参数]")
	case "act":
		fmt.Println("dw-browser act — 对当前会话执行语义操作")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  dw-browser act --id <id> \"click button:'登录'\" [--await] [--snap]")
		fmt.Println("  dw-browser act --id <id> \"fill searchbox:'搜索' 'deepwork'\" --snap")
		fmt.Println("  dw-browser act --id <id> \"press Enter\" --await --snap")
		fmt.Println()
		fmt.Println("常用操作:")
		fmt.Println("  click/fill/type/press/scroll/hover/select/back/forward/focus/scrollinto/check/uncheck")
	case "observe", "diff", "check", "journey", "plan", "do", "test-help":
		printTestingHelp()
	case "batch":
		fmt.Println("dw-browser batch — 在同一连接里顺序执行多步操作")
		fmt.Println()
		fmt.Println("用法:")
		fmt.Println("  dw-browser batch --id <id> --file actions.jsonl [--snap]")
		fmt.Println("  dw-browser batch --id <id> \"fill searchbox 'deepwork'\" \"press Enter\" --snap")
		fmt.Println()
		fmt.Println("JSONL step:")
		fmt.Println("  {\"action\":\"click button:'Open details'\",\"await\":true,\"snap\":true}")
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
	if flags.hasDevice || flags.hasUA {
		presetID = resolveSessionPresetID(flags)
	}

	runtimeMode := browser.NormalizeBrowserMode(sessionInfo.Mode, browser.ModeVisible)
	impl, err := browser.NewBrowserCoreFromSession(ctx, sessionInfo.WSURL, targetID, presetID, runtimeMode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: connect to session failed: %v\n", cmdName, err)
		os.Exit(exitRunErr)
	}

	width := sessionInfo.ViewportW
	height := sessionInfo.ViewportH
	if flags.hasViewport || flags.hasDevice {
		width = flags.viewportW
		height = flags.viewportH
	}
	replayViewportProfile(impl, presetID, width, height, sessionInfo.Touch, cmdName)
	return impl
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
			if impl, cerr := browser.NewBrowserCoreFromSession(hostCtx, state.WSURL, targetID, sessionInfo.PresetID, sessionInfo.Mode); cerr == nil {
				if snap, nerr := impl.Navigate(hostCtx, sessionInfo.PageURL); nerr == nil && snap != nil {
					sessionInfo.PageURL = snap.URL
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

func actWithRetry(ctx context.Context, bc browser.BrowserCore, action string, observe bool) (*browser.Snapshot, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		snap, err := bc.Act(ctx, action, observe)
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

// runSnap 执行 snap 子命令: 导航到 URL 并输出 A11y 快照。
// Session 模式: dw-browser snap --session <id>
// One-shot 模式: dw-browser snap <url> [flags]
func runSnap(args []string) {
	// Parse r2 flags: --selector, --compact, --max-depth before common flags
	snapSelector, snapCompact, snapMaxDepth, cleanArgs := parseSnapFlags(args)

	positional, flags := parseCommonFlags(cleanArgs, "snap")

	// Session 模式
	if flags.sessionID != "" {
		runSnapSession(flags, snapSelector, snapCompact, snapMaxDepth)
		return
	}

	// One-shot 模式（向后兼容）
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser snap: requires <url> or --session <id>")
		os.Exit(exitRunErr)
	}
	url := positional[0]
	profileID := resolveProfileID(flags, "default")
	browserOpts := browserOptionsFromFlags(flags)

	bc := newBrowserCore(profileID, browserOpts...)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	defer func() {
		bc.Close(ctx)
		if flags.ephemeral {
			cleanupEphemeral(profileID)
		}
	}()

	// Harness warmup: chromedp ExecAllocator 在 CLI test 模式下存在首个 target
	// 刚就绪时的瞬时 attach race。给浏览器一个极短 settle 窗口，避免首步 navigate
	// 被误判成产品失败。
	time.Sleep(600 * time.Millisecond)

	snap, err := navigateWithRetry(ctx, bc, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: navigate failed: %v\n", err)
		os.Exit(exitRunErr)
	}

	// Apply SnapOptions filters (one-shot mode: no session remapping)
	if snapSelector != "" || snapCompact || snapMaxDepth > 0 {
		// For one-shot mode, re-use the already-navigated bc; cast to SessionCore
		if sc, ok := bc.(browser.SessionCore); ok {
			opts := browser.SnapOptions{
				Selector:    snapSelector,
				Compact:     snapCompact,
				MaxDepth:    snapMaxDepth,
				SessionMode: false,
			}
			snap2, err2 := sc.SnapWithOptions(ctx, opts)
			if err2 != nil {
				fmt.Fprintf(os.Stderr, "dw-browser snap: %v\n", err2)
				os.Exit(exitRunErr)
			}
			snap = snap2
		}
	}

	output := map[string]interface{}{
		"snap":       snap.Text,
		"url":        snap.URL,
		"title":      snap.PageTitle,
		"refs_count": len(snap.Refs),
		"token_est":  snap.TokenEst,
		"type":       snap.SnapshotType,
		"refs":       buildRefsOutput(snap.Refs),
	}
	injectSnapshotState(output, snap)
	if snapSelector != "" {
		output["scope"] = snapSelector
	}
	if hint := formatSkillHint(snap.URL); hint != "" {
		output["skill_hint"] = hint
	}
	enc, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(enc))
	os.Exit(exitOK)
}

// parseSnapFlags 从 args 中提取 snap 专用 flags (--selector, --compact, --max-depth)。
// 返回: selector string, compact bool, maxDepth int, 剩余 args（去掉已解析的 flags）。
func parseSnapFlags(args []string) (selector string, compact bool, maxDepth int, rest []string) {
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--selector" && i+1 < len(args):
			selector = args[i+1]
			i += 2
		case strings.HasPrefix(arg, "--selector="):
			selector = arg[len("--selector="):]
			i++
		case arg == "--compact":
			compact = true
			i++
		case arg == "--max-depth" && i+1 < len(args):
			n, err := strconv.Atoi(args[i+1])
			if err == nil && n > 0 {
				maxDepth = n
			}
			i += 2
		case strings.HasPrefix(arg, "--max-depth="):
			n, err := strconv.Atoi(arg[len("--max-depth="):])
			if err == nil && n > 0 {
				maxDepth = n
			}
			i++
		default:
			rest = append(rest, arg)
			i++
		}
	}
	return
}

// runSnapSession 执行 session 模式 snap（不导航，连接已有 Chrome）。
// selector/compact/maxDepth 对应 r2 Delta-REQ SC-20/SC-21 新增 flags。
func runSnapSession(flags commonFlags, selector string, compact bool, maxDepth int) {
	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser snap: %v\n", err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	impl := connectSession(ctx, sessionInfo, "snap", flags)

	sessionInfo.SnapEpoch++

	var snap *browser.Snapshot
	if selector != "" || compact || maxDepth > 0 {
		// r2: SnapWithOptions path (SC-20/SC-21)
		opts := browser.SnapOptions{
			Selector:    selector,
			Compact:     compact,
			MaxDepth:    maxDepth,
			SessionMode: true,
			SnapEpoch:   sessionInfo.SnapEpoch,
		}
		snap, err = impl.SnapWithOptions(ctx, opts)
	} else {
		snap, err = impl.SnapWithSessionMode(ctx, sessionInfo.SnapEpoch)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser snap: %v\n", err)
		exitSessionCore(impl, exitRunErr)
	}

	// Build session refs from snap
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
		fmt.Fprintf(os.Stderr, "dw-browser snap: save session: %v\n", err)
	}

	output := map[string]interface{}{
		"session_id":         sessionInfo.SessionID,
		"browser_session_id": sessionInfo.BrowserSessionID,
		"session_kind":       sessionInfo.SessionKind,
		"snap_epoch":         sessionInfo.SnapEpoch,
		"summary":            snap.Text,
		"url":                snap.URL,
		"title":              snap.PageTitle,
		"refs_count":         len(snap.Refs),
		"token_est":          snap.TokenEst,
		"type":               snap.SnapshotType,
		"elements":           buildRefsOutput(snap.Refs),
	}
	injectSnapshotState(output, snap)
	if selector != "" {
		output["scope"] = selector
	}
	if hint := formatSkillHint(snap.URL); hint != "" {
		output["skill_hint"] = hint
	}
	enc, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(enc))
	exitSessionCore(impl, exitOK)
}

// runState outputs only the compact indexed interactive elements for the current
// session page — the agent-friendly equivalent of BrowserAct's `state` command.
// Unlike `snap`, it omits the full page text and saves tokens by focusing purely
// on what the agent can interact with.
//
// Usage: dw-browser state --session <id>
// Output: {"url":..., "state":"[@r1 button 'Nav'] [@r2 input ...]", "refs_count":N}
// refs are persisted to session state file for subsequent `act "click @r1"` — NOT echoed to stdout.
func runState(args []string) {
	_, flags := parseCommonFlags(args, "state")
	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser state: requires --session <id>")
		os.Exit(exitRunErr)
	}

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser state: %v\n", err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	impl := connectSession(ctx, sessionInfo, "state", flags)

	sessionInfo.SnapEpoch++

	opts := browser.SnapOptions{
		Compact:     true,
		SessionMode: true,
		SnapEpoch:   sessionInfo.SnapEpoch,
	}
	snap, err := impl.SnapWithOptions(ctx, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser state: %v\n", err)
		exitSessionCore(impl, exitRunErr)
	}

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
		fmt.Fprintf(os.Stderr, "dw-browser state: save session: %v\n", err)
	}

	// refs saved to session state above — omit from stdout to maximize token efficiency.
	// Agents use @rN refs via stored session state; no need to echo the full array.
	output := map[string]interface{}{
		"url":        snap.URL,
		"title":      snap.PageTitle,
		"state":      snap.Text,
		"refs_count": len(snap.Refs),
	}
	if hint := formatSkillHint(snap.URL); hint != "" {
		output["skill_hint"] = hint
	}
	enc, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(enc))
	exitSessionCore(impl, exitOK)
}

// runAct 执行 act 子命令。
// Session 模式: dw-browser act --session <id> "<action>" [--await] [--snap]
// One-shot 模式: dw-browser act <url> "<action>" [flags]
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
			fmt.Fprintln(os.Stderr, "dw-browser act: requires <action> with --session")
			os.Exit(exitRunErr)
		}
		runActSession(flags, actPositional[0], awaitStable, snapAfterAct, skillFlag)
		return
	}

	// One-shot 模式（向后兼容）
	if len(positional) < 2 {
		fmt.Fprintln(os.Stderr, "dw-browser act: requires <url> <action>")
		os.Exit(exitRunErr)
	}
	url := positional[0]
	action := positional[1]
	profileID := resolveProfileID(flags, fmt.Sprintf("act-%d", time.Now().UnixNano()))
	browserOpts := browserOptionsFromFlags(flags)

	bc := newBrowserCore(profileID, browserOpts...)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	defer func() {
		bc.Close(ctx)
		if flags.ephemeral {
			cleanupEphemeral(profileID)
		}
	}()

	if _, err := navigateWithRetry(ctx, bc, url); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: navigate failed: %v\n", err)
		os.Exit(exitRunErr)
	}

	observe := needsPostActionSnapshot(action)
	snap, err := actWithRetry(ctx, bc, action, observe)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: act failed: %v\n", err)
		os.Exit(exitFail)
	}

	output := map[string]interface{}{
		"success": true,
	}
	if snap != nil {
		output["snap"] = snap.Text
	}
	enc, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(enc))
	os.Exit(exitOK)
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
	case "scroll", "hover", "focus", "scrollinto", "clickat", "tapat":
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
	case "click", "clickat", "tap", "tapat":
		return true
	default:
		return false
	}
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

	// snapAfterAct (--snap): 强制 observe=true，确保 action 成功后获取快照 [SC-19, TC-C5-11]
	observe := needsPostActionSnapshot(action) || snapAfterAct
	snap, err := impl.ActWithSessionMode(ctx, action, observe)
	if err != nil {
		// [SC-19, TC-C5-12] act --snap: action 失败 → 不执行 snap，直接返回错误
		fmt.Fprintf(os.Stderr, "dw-browser: act failed: %v\n", err)
		exitSessionCore(impl, exitFail)
	}

	// [Record tap — BS-09 Phase 2] 若当前 session 有活跃录制，追加此 act 动作为一个 step。
	// 零副作用：无录制状态文件 → 静默跳过；写入错误 → 静默忽略，不影响 act 主路径。
	appendRecordStep(flags.sessionID, action, sessionInfo.PageURL)

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

	// [ACT-SESSION-PERSIST] 每次 act 后把最新 A11y refs 写回 session 文件。
	// 原因: 下一次 act 从 session 文件恢复 refs (RestoreRefsFromSession)，
	// 若 act/await 后页面导航但 session 未更新，下一次 act 会用旧 refs 导致 "元素未找到"。
	// (等同于 runSnapSession 中的 sessionInfo.Refs = sessionRefs; SaveSession 逻辑)
	if snap != nil && len(snap.Refs) > 0 {
		sessionInfo.SnapEpoch++
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
		_ = browser.SaveSession(sessionInfo)
	}

	output := map[string]interface{}{
		"session_id":         sessionInfo.SessionID,
		"browser_session_id": sessionInfo.BrowserSessionID,
		"session_kind":       sessionInfo.SessionKind,
		"success":            true,
	}
	// [Browser Skills] --skill flag: resolve and inject skill execution context
	sc := resolveSkillContext(skillFlag, sessionInfo.PageURL, flags.sessionID)
	injectSkillFields(output, sc)

	// [SC-19, TC-C5-11] act --snap: 输出 action 结果 + snap 结果合并。
	if snapAfterAct {
		output["snap_requested"] = true
	}
	if snap != nil {
		output["snap"] = snap.Text
		injectSnapshotState(output, snap)
		// [P0-α user_state] 结构化 UX 状态 — 让 Agent 判断"用户能否继续交互"而非只看日志事件。
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
			// "clickable" 等自定义 role 也计入
			if ref.Role == "clickable" {
				clickableCount++
			}
		}
		userState["interactable_count"] = len(snap.Refs)
		userState["input_ready"] = inputReady
		// page_interactive: 页面有足够的可操作元素(≥3)，用户可以有意义地交互。
		// 区别于 input_ready(仅限文本输入框)。列表页/仪表盘无 textbox 但 page_interactive=true。
		userState["page_interactive"] = len(snap.Refs) >= 3
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
		userState["active_tab_url"] = sessionInfo.PageURL
		output["user_state"] = userState
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

// ============================================================
// § runEval — P2: eval 命令 [SC-23, TC-C5-17, TC-C5-18]
// [Ref: CAP-BS09-C5 §3.2b, r2 Delta-REQ TH-0418-c9x]
// ============================================================

// runEval 执行 JavaScript 表达式并输出结果。
// 用法: dw-browser eval --session <id> "<js-expression>"
// 仅支持 session 模式（需要已有 Chrome 连接）。
func runEval(args []string) {
	positional, flags := parseCommonFlags(args, "eval")

	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser eval: requires --session <id>")
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
// 用法: dw-browser cookie-import --session <id> [--browser chrome|firefox] [--domain <filter>]
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
		fmt.Fprintln(os.Stderr, "dw-browser cookie-import: requires --session <id>")
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
func runOpenSafari(url string, flags commonFlags) {
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

	var openRefs []browser.SessionRef
	if openSnap != nil {
		for _, ref := range openSnap.Refs {
			openRefs = append(openRefs, browser.SessionRef{
				Ref:         ref.Ref,
				Role:        ref.Role,
				Name:        ref.NameFull,
				TestID:      ref.TestID,
				Placeholder: ref.Placeholder,
				Locator:     ref.Locator,
				AXPath:      ref.AXPath,
				StableKey:   ref.Locator.StableKey,
			})
		}
	}

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
		fmt.Fprintln(os.Stderr, "dw-browser open: requires --session <id>")
		os.Exit(exitRunErr)
	}
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
		runOpenSafari(url, flags)
		return
	}

	sessionPresetID := resolveSessionPresetID(flags)
	browserSessionID := browser.BrowserSessionIDFromSessionID(flags.sessionID)

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

	// Resolve profile dir — ~/.deepwork/browser-cli/{profileID}
	profileID := resolveProfileID(flags, defaultProfileID(flags))
	homeDir, _ := os.UserHomeDir()
	profileDir := fmt.Sprintf("%s/.deepwork/browser-cli/%s", homeDir, profileID)
	mode := browser.NormalizeBrowserMode(flags.mode, browser.ModeVisible)

	// Phase_v2_4 startup recovery (CAP-BS09-C4 §3.5) — 与 BrowserPool 共享 4 步协议:
	//   1. SingletonLock 残留检测 (orphan PID 强杀, 避免上次 CLI 崩溃留下的 chrome 抢锁)
	//   2. profile health check (Cookies SQLite header 16 字节校验)
	//   3. 损坏 → {profileDir}.broken/{UTC ts}/ 隔离 (Chrome 自动重建空 profile)
	// 注: 跨进程语义 (SaveSession/LoadSession) 仍归 session_manager; recovery 仅是低层共享.
	ownerKey := browser.IdentityKey("dw-cli-" + browserSessionID)
	if mode != browser.ModeHeaded {
		if err := browser.RunStartupRecovery(profileDir, ownerKey); err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser open: startup recovery: %v\n", err)
			os.Exit(exitRunErr)
		}
	}

	if err := os.MkdirAll(profileDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser open: mkdir profile: %v\n", err)
		os.Exit(exitRunErr)
	}

	// Viewport
	width, height := 1920, 1080
	if flags.hasViewport {
		width, height = flags.viewportW, flags.viewportH
	} else if flags.hasDevice {
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
				os.Exit(exitRunErr)
			}
			displayBackend = "xvfb"
		}
	default:
		fmt.Fprintf(os.Stderr, "dw-browser open: unsupported browser mode %q\n", mode)
		os.Exit(exitRunErr)
	}

	chromeArgs := browser.BuildDetachedChromeArgs(browser.DetachedChromeLaunchOptions{
		DebugPort:  port,
		ProfileDir: profileDir,
		Width:      width,
		Height:     height,
		PresetID:   sessionPresetID,
		UserAgent:  flags.userAgent,
		Touch:      flags.hasDevice && devicePresets[flags.device].Touch,
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
			Width:            width,
			Height:           height,
			UserAgent:        flags.userAgent,
			Touch:            flags.hasDevice && devicePresets[flags.device].Touch,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		var err error
		hostState, err = browser.EnsureBrowserMuxHost(ctx, hostReq)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser open: ensure BrowserMuxHost: %v\n", err)
			os.Exit(exitRunErr)
		}
		chromePID = hostState.ChromePID
		wsURL = hostState.WSURL
		port = hostState.DebugPort
		displayBackend = hostState.DisplayBackend
	} else if mode == browser.ModeVisible {
		ws := browser.NewWorkspace()
		defer ws.Close()
		if spaceID, wsErr := ws.EnsureSpace(); wsErr != nil {
			fmt.Fprintf(os.Stderr, "[workspace] EnsureSpace: %v (Chrome will appear on current Space)\n", wsErr)
		} else if spaceID > 0 {
			fmt.Fprintf(os.Stderr, "[workspace] using isolation Space %d\n", spaceID)
		}
		handle, err := ws.LaunchChromeInSpace(browser.ChromeLaunchSpec{
			ChromePath:   chromePath,
			Args:         chromeArgs,
			DebugPort:    port,
			ReadyTimeout: 30 * time.Second,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser open: workspace launch chrome: %v\n", err)
			os.Exit(exitRunErr)
		}
		chromePID = handle.PID()
		wsURL = handle.WSURL()
		// Note: 不 Kill handle — dw-browser open 语义为"启动后退出, Chrome 继续运行".
		// Setpgid 已切断 process group, CLI 退出后 Chrome 不受影响.
	} else {
		var err error
		chromePID, err = startDetachedChrome(chromePath, chromeArgs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser open: %v\n", err)
			os.Exit(exitRunErr)
		}
		wsURL, err = browser.WaitForChromeReady(port, 30*time.Second)
		if err != nil {
			if chromePID > 0 {
				if proc, findErr := os.FindProcess(chromePID); findErr == nil {
					_ = proc.Kill()
				}
			}
			fmt.Fprintf(os.Stderr, "dw-browser open: %v\n", err)
			os.Exit(exitRunErr)
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
			if chromePID > 0 {
				if proc, findErr := os.FindProcess(chromePID); findErr == nil {
					_ = proc.Kill()
				}
			}
			fmt.Fprintf(os.Stderr, "dw-browser open: write profile owner marker: %v\n", err)
			os.Exit(exitRunErr)
		}
	}

	// Connect via chromedp and navigate
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	initialTargetID, err := ensurePageTargetReady(wsURL, port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser open: ensure page target: %v\n", err)
		os.Exit(exitRunErr)
	}

	impl, err := browser.NewBrowserCoreFromSession(ctx, wsURL, initialTargetID, sessionPresetID, mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser open: connect: %v\n", err)
		os.Exit(exitRunErr)
	}
	defer impl.Close(ctx)
	replayViewportProfile(impl, sessionPresetID, width, height, flags.hasDevice && devicePresets[flags.device].Touch, "open")

	openSnap, err := impl.Navigate(ctx, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser open: navigate: %v\n", err)
		os.Exit(exitRunErr)
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

	// B2 修复: open 命令保存初始快照的 refs，使后续 act "click button:'xxx'" 无需先执行 snap。
	// 与 snap 命令保持一致：将 openSnap.Refs 序列化为 SessionRef 存入 session 文件。
	var openRefs []browser.SessionRef
	if openSnap != nil {
		for _, ref := range openSnap.Refs {
			openRefs = append(openRefs, browser.SessionRef{
				Ref:           ref.Ref,
				BackendNodeID: ref.BackendNodeID,
				Role:          ref.Role,
				Name:          ref.NameFull,
				TestID:        ref.TestID,
				Placeholder:   ref.Placeholder,
			})
		}
	}

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
		ProfileDir:            profileDir,
		PageURL:               currentURL,
		CreatedAt:             time.Now().Format(time.RFC3339),
		ViewportW:             width,
		ViewportH:             height,
		UserAgent:             flags.userAgent,
		Touch:                 flags.hasDevice && devicePresets[flags.device].Touch,
		SnapEpoch:             0,
		Refs:                  openRefs,
		Ephemeral:             flags.ephemeral || flags.isolation == browser.SessionIsolationEphemeral,
		XvfbPID:               xvfbPIDFromDisplayManager(dm),
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
		os.Exit(exitRunErr)
	}

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
	os.Exit(exitOK)
}

// runClose 关闭 session（杀 Chrome 进程，删 session 文件）。
func runClose(args []string) {
	_, flags := parseCommonFlags(args, "close")
	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser close: requires --session <id>")
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
	if !hostShutdown && sessionInfo.ChromePID > 0 {
		proc, err := os.FindProcess(sessionInfo.ChromePID)
		if err == nil {
			_ = proc.Kill()
		}
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

	// Clean up profile dir if ephemeral (from session info or close flag)
	if (sessionInfo.Ephemeral || flags.ephemeral) && sessionInfo.ProfileDir != "" {
		os.RemoveAll(sessionInfo.ProfileDir)
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
func runGet(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser get: requires subcommand (text|url|title)")
		os.Exit(exitRunErr)
	}
	subCmd := args[0]
	positional, flags := parseCommonFlags(args[1:], "get")

	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser get: requires --session <id>")
		os.Exit(exitRunErr)
	}

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser get: %v\n", err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	impl := connectSession(ctx, sessionInfo, "get", flags)

	switch subCmd {
	case "text":
		var focus *string
		if len(positional) > 0 {
			focus = &positional[0]
		}
		text, err := impl.Text(ctx, focus)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser get text: %v\n", err)
			exitSessionCore(impl, exitRunErr)
		}
		// P2: SPA content retry — if text is very short, wait and retry once
		if len(text) < 200 {
			fmt.Fprintln(os.Stderr, "[dw-browser] get text: sparse content, retrying after 2s (SPA render lag)")
			time.Sleep(2 * time.Second)
			if retried, retryErr := impl.Text(ctx, focus); retryErr == nil && len(retried) > len(text) {
				text = retried
			}
		}
		enc, _ := json.MarshalIndent(map[string]interface{}{
			"session_id":         flags.sessionID,
			"browser_session_id": sessionInfo.BrowserSessionID,
			"text":               text,
		}, "", "  ")
		fmt.Println(string(enc))

	case "url":
		var url string
		impl.EvalJS(ctx, "window.location.href", &url)
		enc, _ := json.MarshalIndent(map[string]interface{}{
			"session_id":         flags.sessionID,
			"browser_session_id": sessionInfo.BrowserSessionID,
			"url":                url,
		}, "", "  ")
		fmt.Println(string(enc))

	case "title":
		var title string
		impl.EvalJS(ctx, "document.title", &title)
		enc, _ := json.MarshalIndent(map[string]interface{}{
			"session_id":         flags.sessionID,
			"browser_session_id": sessionInfo.BrowserSessionID,
			"title":              title,
		}, "", "  ")
		fmt.Println(string(enc))

	default:
		// NL query fallback: treat subCmd as the first word of the query, reconstruct full query
		// e.g. dw-browser get --id s1 "active tab url"  → subCmd="active", positional=["tab", "url"]
		// We need to pass all args to runGetNL using the original args slice (before subCmd split).
		// Close the session first since runGetNL will open a fresh observe connection.
		closeSessionCore(impl)
		cancel()
		runGetNL(args)
		return
	}
	exitSessionCore(impl, exitOK)
}

// runWait 执行 wait 子命令。
// dw-browser wait --session <id> 2000           → sleep
// dw-browser wait --session <id> "visible #btn"  → wait for element visible
// dw-browser wait --session <id> "gone #mask"    → wait for element to disappear
// dw-browser wait --session <id> "text 创建成功"  → wait for text to appear
func runWait(args []string) {
	positional, flags := parseCommonFlags(args, "wait")
	if flags.sessionID == "" {
		fmt.Fprintln(os.Stderr, "dw-browser wait: requires --session <id>")
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
// Session 模式: dw-browser screenshot --session <id> [out.png]
// One-shot 模式: dw-browser screenshot <url> [out.png]
func runScreenshot(args []string) {
	outFileFlag := ""
	clean := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--out" && i+1 < len(args):
			outFileFlag = args[i+1]
			i++
		case strings.HasPrefix(arg, "--out="):
			outFileFlag = arg[len("--out="):]
		default:
			clean = append(clean, arg)
		}
	}
	positional, flags := parseCommonFlags(clean, "screenshot")

	// Session 模式
	if flags.sessionID != "" {
		outFile := "screenshot.png"
		if outFileFlag != "" {
			outFile = outFileFlag
		}
		if len(positional) >= 1 {
			outFile = positional[0]
		}
		runScreenshotSession(flags, outFile)
		return
	}

	// One-shot 模式
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser screenshot: requires <url> or --session <id>")
		os.Exit(exitRunErr)
	}
	url := positional[0]
	outFile := "screenshot.png"
	if outFileFlag != "" {
		outFile = outFileFlag
	}
	if len(positional) >= 2 {
		outFile = positional[1]
	}
	profileID := resolveProfileID(flags, fmt.Sprintf("screenshot-%d", time.Now().UnixNano()))
	browserOpts := browserOptionsFromFlags(flags)

	bc := newBrowserCore(profileID, browserOpts...)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Codex #10: 用 exitCode 追踪错误状态，defer 清理后再 os.Exit
	// 直接 os.Exit 会跳过 defer（僵尸 Chrome），直接 return 则退出码为 0（CI 误判成功）
	exitCode := exitOK
	defer func() {
		bc.Close(ctx)
		if flags.ephemeral {
			cleanupEphemeral(profileID)
		}
		if exitCode != exitOK {
			os.Exit(exitCode)
		}
	}()

	if _, err := navigateWithRetry(ctx, bc, url); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: navigate failed: %v\n", err)
		exitCode = exitRunErr
		return
	}

	data, err := screenshotWithRetry(ctx, bc, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: screenshot failed: %v\n", err)
		exitCode = exitRunErr
		return
	}

	if err := os.WriteFile(outFile, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: write screenshot: %v\n", err)
		exitCode = exitRunErr
		return
	}
	fmt.Printf("Screenshot saved to %s (%d bytes)\n", outFile, len(data))
}

// runScreenshotSession 执行 session 模式 screenshot。
func runScreenshotSession(flags commonFlags, outFile string) {
	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser screenshot: %v\n", err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	impl := connectSession(ctx, sessionInfo, "screenshot", flags)

	data, err := impl.Screenshot(ctx, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: screenshot failed: %v\n", err)
		exitSessionCore(impl, exitRunErr)
	}

	if err := os.WriteFile(outFile, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: write screenshot: %v\n", err)
		exitSessionCore(impl, exitRunErr)
	}
	fmt.Printf("Screenshot saved to %s (%d bytes)\n", outFile, len(data))
	exitSessionCore(impl, exitOK)
}

// runLayout 执行 layout 子命令（L2 布局验证）[TC-09-I-20]。
func runLayout(args []string) {
	positional, flags := parseCommonFlags(args, "layout")
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser layout: requires <url>")
		os.Exit(exitRunErr)
	}
	url := positional[0]
	profileID := resolveProfileID(flags, "default")
	browserOpts := browserOptionsFromFlags(flags)

	bc := newBrowserCore(profileID, browserOpts...)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer func() {
		bc.Close(ctx)
		if flags.ephemeral {
			cleanupEphemeral(profileID)
		}
	}()

	snap, err := navigateWithRetry(ctx, bc, url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser: navigate failed: %v\n", err)
		os.Exit(exitRunErr)
	}

	// L2 布局断言: 验证页面有可交互元素（基本布局完整性）
	if len(snap.Refs) == 0 && snap.SnapshotType == "screenshot_fallback" {
		fmt.Fprintf(os.Stderr, "[L2 FAIL] page has no interactive elements: %s\n", url)
		os.Exit(exitFail)
	}

	fmt.Printf("[L2 PASS] layout OK: %s (refs=%d, type=%s)\n", url, len(snap.Refs), snap.SnapshotType)
	os.Exit(exitOK)
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

// runExplore 执行 explore 子命令 — 为 Claude Code / AI Agent 设计的浏览器状态查询器。
//
// 设计意图: AI Agent 在调试 UI 时需要同时看到浏览器状态和服务端状态。
// explore 将 snap + /api/health + /api/debug/obs/recent 合并为一次调用，
// 输出结构化 JSON 供 AI 分析决策。AI 决策循环在 Claude Code 层，不在此命令内。
//
// 五种模式:
//
//	explore <url>                              — 观察: 导航+快照+服务端状态 (one-shot)
//	explore <url> --act <act>                  — 行动: 导航+操作+快照+服务端状态 (one-shot)
//	explore <url> --report                     — 诊断: 导航+快照+截图+服务端状态 (one-shot)
//	explore --session <id>                     — Session 模式: 连接已有 Chrome，快照当前页+服务端状态
//	explore --session <id> --learn-baseline    — Baseline 学习: 自动探索页面生成候选基线 YAML
func runExplore(args []string) {
	positional, flags := parseCommonFlags(args, "explore")

	// 解析 explore 专用 flags（需要在路由判断前，因为 --learn-baseline 影响路由）
	var learnBaseline bool
	var learnBaselineOut string
	var learnBaselineGoal string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--learn-baseline":
			learnBaseline = true
		case args[i] == "--out" && i+1 < len(args):
			learnBaselineOut = args[i+1]
		case strings.HasPrefix(args[i], "--out="):
			learnBaselineOut = args[i][len("--out="):]
		case args[i] == "--goal" && i+1 < len(args):
			learnBaselineGoal = args[i+1]
		case strings.HasPrefix(args[i], "--goal="):
			learnBaselineGoal = args[i][len("--goal="):]
		}
	}

	// Session 模式: --session <id> 存在时连接已有 Chrome，不需要 URL 参数
	if flags.sessionID != "" {
		if learnBaseline {
			runExploreLearnBaseline(flags, learnBaselineOut, learnBaselineGoal)
			return
		}
		runExploreSession(flags)
		return
	}

	// One-shot 模式需要 URL 参数
	if len(positional) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser explore: requires <url> or --session <id>")
		os.Exit(exitRunErr)
	}
	url := positional[0]

	// 解析 explore 专用 flags
	var actAction string
	var reportMode bool
	for i := 0; i < len(positional); i++ {
		if positional[i] == "--act" && i+1 < len(positional) {
			actAction = positional[i+1]
			i++
		}
		if positional[i] == "--report" {
			reportMode = true
		}
	}
	// 也从原始 args 解析（positional 可能已过滤）
	for i := 0; i < len(args); i++ {
		if args[i] == "--act" && i+1 < len(args) {
			actAction = args[i+1]
		}
		if args[i] == "--report" {
			reportMode = true
		}
	}

	requireChrome()
	profileID := resolveProfileID(flags, fmt.Sprintf("explore-%d", time.Now().UnixNano()))
	browserOpts := browserOptionsFromFlags(flags)

	bc := newBrowserCore(profileID, browserOpts...)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	defer func() {
		bc.Close(ctx)
		if flags.ephemeral {
			cleanupEphemeral(profileID)
		}
	}()

	// 推断服务端 base URL (scheme://host[:port])
	baseURL := extractBaseURLFromString(url)

	result := make(map[string]interface{})
	result["command"] = "explore"
	result["url"] = url

	// 执行操作（如有）
	if actAction != "" {
		snap, err := navigateWithRetry(ctx, bc, url)
		if err != nil {
			result["error"] = fmt.Sprintf("navigate: %v", err)
		} else {
			actSnap, actErr := actWithRetry(ctx, bc, actAction, true)
			if actErr != nil {
				result["error"] = fmt.Sprintf("act: %v", actErr)
				result["browser"] = snapToMap(snap)
			} else {
				result["browser"] = snapToMap(actSnap)
			}
			result["action"] = actAction
		}
	} else {
		// 默认: 导航 + 快照
		snap, err := navigateWithRetry(ctx, bc, url)
		if err != nil {
			result["error"] = fmt.Sprintf("navigate: %v", err)
		} else {
			result["browser"] = snapToMap(snap)
		}
	}

	// 全量诊断（--report）
	if reportMode {
		shotFile := fmt.Sprintf("/tmp/dw-explore-report-%s.png", time.Now().Format("20060102-150405"))
		shotData, err := screenshotWithRetry(ctx, bc, false)
		if err == nil {
			os.WriteFile(shotFile, shotData, 0644)
			result["screenshot"] = shotFile
		}
	}

	// 服务端状态（始终附加）
	result["server"] = fetchServerState(baseURL)

	enc, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(enc))
}

// runExploreSession 执行 session 模式 explore — 连接已有 Chrome 快照当前页 + 服务端状态。
//
// 与 one-shot explore 不同: 不创建新 Chrome，不导航，直接连接 session 中的已有 Chrome。
// session.PageURL 用于推断服务端 base URL（无需用户额外传入）。
func runExploreSession(flags commonFlags) {
	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser explore: %v\n", err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	impl := connectSession(ctx, sessionInfo, "explore", flags)

	// 从当前页面获取快照（不导航）
	sessionInfo.SnapEpoch++
	snap, err := impl.SnapWithSessionMode(ctx, sessionInfo.SnapEpoch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser explore: snap failed: %v\n", err)
		exitSessionCore(impl, exitRunErr)
	}

	// 更新 session ref 表
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
	_ = browser.SaveSession(sessionInfo)

	// 从 session 页面 URL 推断服务端 base URL
	baseURL := extractBaseURLFromString(snap.URL)

	result := map[string]interface{}{
		"command":    "explore",
		"session_id": sessionInfo.SessionID,
		"url":        snap.URL,
		"browser":    snapToMap(snap),
		"server":     fetchServerState(baseURL),
	}

	enc, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(enc))
	closeSessionCore(impl)
}

// runExploreLearnBaseline 执行 --learn-baseline 模式：
// 连接已有 session，自动探索页面，生成候选基线 YAML。
//
// dw-browser explore --session <id> --learn-baseline [--out candidate.yaml] [--goal "描述"]
func runExploreLearnBaseline(flags commonFlags, outFile, goal string) {
	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser explore --learn-baseline: %v\n", err)
		os.Exit(exitRunErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	impl := connectSession(ctx, sessionInfo, "explore", flags)
	defer closeSessionCore(impl)
	impl.RestoreRefsFromSession(sessionInfo.Refs)

	executor := &cliActionExecutor{
		sessionID:   flags.sessionID,
		sessionInfo: sessionInfo,
		impl:        impl,
		ctx:         ctx,
	}

	explorer := btest.NewExplorer(executor)

	if goal == "" {
		goal = "explore baseline for " + sessionInfo.PageURL
	}

	cb, err := explorer.LearnBaseline(ctx, goal)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser explore --learn-baseline: %v\n", err)
		os.Exit(exitRunErr)
	}

	if outFile == "" {
		outFile = fmt.Sprintf("candidate-%s.yaml", cb.ID)
	}

	if err := btest.SaveCandidate(cb, outFile); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser explore --learn-baseline: save: %v\n", err)
		os.Exit(exitRunErr)
	}

	// 输出摘要到 stdout
	summary := map[string]interface{}{
		"command":       "explore --learn-baseline",
		"session_id":    flags.sessionID,
		"url":           cb.URL,
		"candidate_id":  cb.ID,
		"out":           outFile,
		"regions":       len(cb.Regions),
		"invariants":    len(cb.Invariants),
		"actions_tried": cb.Metadata.ActionsTried,
		"duration":      cb.Metadata.Duration,
		"review_status": cb.ReviewStatus,
		"note":          "review_status=candidate: requires Human review before use as baseline",
	}
	enc, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(enc))
	os.Exit(exitOK)
}

// extractBaseURLFromString 从完整 URL 字符串提取 scheme://host[:port]，用于服务端 API 调用。
func extractBaseURLFromString(rawURL string) string {
	if after := strings.TrimPrefix(rawURL, "http://"); after != rawURL {
		if idx := strings.Index(after, "/"); idx > 0 {
			return "http://" + after[:idx]
		}
		return "http://" + after
	}
	if after := strings.TrimPrefix(rawURL, "https://"); after != rawURL {
		if idx := strings.Index(after, "/"); idx > 0 {
			return "https://" + after[:idx]
		}
		return "https://" + after
	}
	return rawURL
}

// fetchServerState 拉取服务端健康状态和近期错误，返回统一格式 map。
func fetchServerState(baseURL string) map[string]interface{} {
	serverState := make(map[string]interface{})
	const serverTimeout = 2 * time.Second

	healthResp := fetchJSONWithTimeout(baseURL+"/api/health", serverTimeout)
	if healthResp != nil {
		var h interface{}
		json.Unmarshal(healthResp, &h)
		serverState["health"] = h
	} else {
		serverState["health"] = map[string]string{"error": "unreachable"}
	}

	recentResp := fetchJSONWithTimeout(baseURL+"/api/debug/obs/recent?limit=10&since=30s", serverTimeout)
	if recentResp != nil {
		var r interface{}
		json.Unmarshal(recentResp, &r)
		serverState["recent_errors"] = r
	} else {
		serverState["recent_errors"] = map[string]string{"error": "unreachable"}
	}
	return serverState
}

// snapToMap 将 Snapshot 转为 map（用于 JSON 输出）。
func snapToMap(snap *browser.Snapshot) map[string]interface{} {
	if snap == nil {
		return map[string]interface{}{"error": "no snapshot"}
	}
	refs := make([]map[string]interface{}, 0, len(snap.Refs))
	for _, ref := range snap.Refs {
		r := map[string]interface{}{
			"ref":  ref.Ref,
			"role": ref.Role,
			"name": ref.Name,
		}
		if ref.Placeholder != "" {
			r["placeholder"] = ref.Placeholder
		}
		if ref.RecommendedLocator != "" {
			r["locator"] = ref.RecommendedLocator
		}
		refs = append(refs, r)
	}
	return map[string]interface{}{
		"url":        snap.URL,
		"title":      snap.PageTitle,
		"refs_count": len(snap.Refs),
		"refs":       refs,
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	defer func() {
		bc.Close(ctx)
		if flags.ephemeral {
			cleanupEphemeral(profileID)
		}
	}()

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
			os.Exit(exitRunErr)
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
		os.Exit(exitFail)
	}
	os.Exit(exitOK)
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
		fmt.Fprintln(os.Stderr, "dw-browser audit: requires --session <id>")
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
