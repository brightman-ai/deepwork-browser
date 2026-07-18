package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/brightman-ai/kit/obs"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

const (
	navigateReadyPollInterval  = 200 * time.Millisecond
	navigateReadyTimeout       = 5 * time.Second
	navigateDOMIdleMs          = 500
	navigateDOMSettleTimeoutMs = 5000
)

// ============================================================
// § BrowserOption — 可选参数模式（扩展 NewBrowserCore）
// ============================================================

// browserOptions 存储可选浏览器配置。
type browserOptions struct {
	viewportW   int
	viewportH   int
	userAgent   string
	presetID    string
	touch       bool
	mode        BrowserMode // "human" / "headless"
	stealth     bool
	hasViewport bool
	hasUA       bool
	hasMode     bool
	persona     *Persona // 可选:携带 Shell/Network/Env facet,运行时经 applyPersonaEmulation realize
}

// BrowserOption 是可选参数函数类型。
type BrowserOption func(*browserOptions)

// WithViewport 设置视口大小。
func WithViewport(w, h int) BrowserOption {
	return func(o *browserOptions) {
		o.viewportW = w
		o.viewportH = h
		o.hasViewport = true
	}
}

// WithUserAgent 设置自定义 User-Agent。
func WithUserAgent(ua string) BrowserOption {
	return func(o *browserOptions) {
		o.userAgent = ua
		o.hasUA = true
	}
}

// WithFingerprintPreset 设置运行时指纹 preset。
// dw-browser 的 device/UA 模拟必须同时驱动启动参数与 JS/runtime 指纹。
func WithFingerprintPreset(presetID string) BrowserOption {
	return func(o *browserOptions) {
		o.presetID = NormalizePresetID(presetID)
	}
}

// WithTouchEmulation 设置触控模拟（Mobile 设备预设使用）。
func WithTouchEmulation(enabled bool) BrowserOption {
	return func(o *browserOptions) {
		o.touch = enabled
	}
}

// WithMode 设置浏览器模式。默认 headless（CLI/测试场景）。
func WithMode(mode BrowserMode) BrowserOption {
	return func(o *browserOptions) {
		o.mode = mode
		o.hasMode = true
	}
}

// WithStealth 启用反检测模式（只在 headless 下生效）。
// headed/visible Chrome 使用 CGVirtualDisplay + 真实指纹，不需要此选项。
func WithStealth(enabled bool) BrowserOption {
	return func(o *browserOptions) {
		o.stealth = enabled
	}
}

// WithPersona 携带完整 persona,其 Shell/Network/Env facet 由 applyPersonaEmulation
// 在会话建立与每次视口重放时 realize(身份指纹仍走 WithFingerprintPreset)。
func WithPersona(p *Persona) BrowserOption {
	return func(o *browserOptions) {
		o.persona = p
	}
}

// ============================================================
// § BrowserCoreImpl — BrowserCore 接口的实现 [Ref: BP §B2]
// ============================================================

// CursorDetector 是 cursor 检测接口。
// browserCoreImpl 实现此接口；BrowserSession 通过类型断言调用。
type CursorDetector interface {
	SetupCursorDetection(onCursor func(cursor string)) error
}

// CDPContextProvider 暴露 Chrome 的 CDP context 给外层（BrowserSession 注入 InputGateway 用）。
// browserCoreImpl 实现此接口。
type CDPContextProvider interface {
	CDPContext() context.Context
}

// CDPContext 返回 browserCoreImpl 内部的 Chrome CDP context。
func (impl *browserCoreImpl) CDPContext() context.Context {
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	return impl.browserCtx
}

// currentCtx 返回"当前活跃 target"的 CDP context。
//
// 所有对外 read/act 入口 (Navigate/Snap/Act/Text/Screenshot 及其 Session 变体)
// 必须经此方法选择 ctx, 消除 "Navigate 走 tracker, 其它走 browserCtx" 的歧视模式 —
// 该歧视模式是多 tab 场景下 screenshot 拍错 tab / snap 返回 0 元素的根因。
//
// [DDC-I-22, BRR-I-15, TH-0419-q5b]
//
// 调用方必须已持有 impl.mu 读/写锁 — 此方法不自行加锁。
func (impl *browserCoreImpl) currentCtx() context.Context {
	_, ctx := impl.currentTargetRef()
	return ctx
}

func (impl *browserCoreImpl) currentTargetRef() (string, context.Context) {
	if impl.targetTracker != nil {
		return impl.targetTracker.ActiveTargetRef()
	}
	return "", impl.browserCtx
}

func deriveTargetContext(parent context.Context, target context.Context) (context.Context, context.CancelFunc) {
	if parent == nil || parent.Done() == nil {
		return target, func() {}
	}
	var (
		runCtx context.Context
		cancel context.CancelFunc
	)
	if deadline, ok := parent.Deadline(); ok {
		runCtx, cancel = context.WithDeadline(target, deadline)
	} else {
		runCtx, cancel = context.WithCancel(target)
	}
	go func() {
		select {
		case <-parent.Done():
			cancel()
		case <-runCtx.Done():
		}
	}()
	return runCtx, cancel
}

func inferFingerprintPresetID(bopts browserOptions) string {
	if bopts.presetID != "" {
		return NormalizePresetID(bopts.presetID)
	}

	ua := bopts.userAgent
	switch {
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"), strings.Contains(ua, "iPod"):
		return PresetIPhoneSafariUA
	case strings.Contains(ua, "Version/") && strings.Contains(ua, "Safari/") && strings.Contains(ua, "Macintosh") && !strings.Contains(ua, "Chrome/"):
		return PresetMacOSSafariUA
	case strings.Contains(ua, "Macintosh"):
		return PresetMacOSChrome
	case strings.Contains(ua, "Windows"):
		return PresetWindowsChrome
	case strings.Contains(ua, "Android"):
		return PresetAndroidChrome
	case strings.Contains(ua, "Linux"):
		return PresetLinuxChrome
	}

	if bopts.touch && bopts.hasViewport {
		shortEdge := bopts.viewportW
		if bopts.viewportH < shortEdge {
			shortEdge = bopts.viewportH
		}
		if shortEdge > 0 && shortEdge <= 820 {
			return PresetIPhoneSafariUA
		}
	}

	return DefaultPresetID()
}

func applyViewportProfile(targetCtx context.Context, width, height int, dpr float64, mobile bool, touch bool, maxTouchPoints int64) error {
	if width <= 0 {
		width = DefaultViewportWidth
	}
	if height <= 0 {
		height = DefaultViewportHeight
	}
	if dpr <= 0 {
		dpr = 1
	}
	if maxTouchPoints <= 0 {
		maxTouchPoints = 1
	}

	return runCDPWithSoftTimeout(targetCtx, BrowserPoolCDPActionTimeout,
		chromedp.ActionFunc(func(ctx context.Context) error {
			override := emulation.SetDeviceMetricsOverride(int64(width), int64(height), dpr, mobile)
			if width < height {
				override = override.WithScreenOrientation(&emulation.ScreenOrientation{
					Type: emulation.OrientationTypePortraitPrimary, Angle: 0,
				})
			} else {
				override = override.WithScreenOrientation(&emulation.ScreenOrientation{
					Type: emulation.OrientationTypeLandscapePrimary, Angle: 90,
				})
			}
			return override.Do(ctx)
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			cfg := emulation.SetTouchEmulationEnabled(touch)
			if touch {
				cfg = cfg.WithMaxTouchPoints(maxTouchPoints)
			}
			return cfg.Do(ctx)
		}),
	)
}

func shouldUseSyntheticViewport(mode BrowserMode, mobile bool, touch bool) bool {
	mode = NormalizeBrowserMode(mode, ModeHeadless)
	return mode == ModeHeadless || mobile || touch
}

func applyNativeWindowViewport(targetCtx context.Context, width, height int) error {
	if targetCtx == nil {
		return nil
	}
	if width <= 0 {
		width = DefaultViewportWidth
	}
	if height <= 0 {
		height = DefaultViewportHeight
	}
	return runCDPWithSoftTimeout(targetCtx, BrowserMuxHostShutdownTimeout, chromedp.ActionFunc(func(ctx context.Context) error {
		windowID, _, err := cdpbrowser.GetWindowForTarget().Do(ctx)
		if err != nil {
			return fmt.Errorf("get chrome window for target: %w", err)
		}
		return cdpbrowser.SetWindowBounds(windowID, &cdpbrowser.Bounds{
			Width:       int64(width),
			Height:      int64(height),
			WindowState: cdpbrowser.WindowStateNormal,
		}).Do(ctx)
	}))
}

func (impl *browserCoreImpl) applyLiveViewport(targetCtx context.Context, width, height int, dpr float64, mobile bool, touch bool, maxTouchPoints int64) error {
	mode := NormalizeBrowserMode(impl.runtimeMode, ModeHeadless)
	if shouldUseSyntheticViewport(mode, mobile, touch) {
		// Fingerprint emulation failure (e.g. "invalid context" during tab init) is non-fatal:
		// the viewport profile must still be applied. Log and continue.
		if err := applyFingerprintEmulation(targetCtx, impl.chromePath, impl.fingerprintPreset); err != nil {
			log.Printf("[BROWSER-LIVEVIEW] fingerprint emulation skipped (context not ready): %v", err)
		}
		if impl.persona != nil {
			if err := applyPersonaEmulation(targetCtx, impl.persona); err != nil {
				log.Printf("[BROWSER-LIVEVIEW] persona emulation skipped (context not ready): %v", err)
			}
		}
		if err := applyViewportProfile(targetCtx, width, height, dpr, mobile, touch, maxTouchPoints); err != nil {
			return fmt.Errorf("sync target viewport: %w", err)
		}
		return nil
	}
	if err := applyNativeWindowViewport(targetCtx, width, height); err != nil {
		return fmt.Errorf("sync native window viewport: %w", err)
	}
	return nil
}

// cursorDetectScript 注入到每个新 document，监听 mouseover 并上报 cursor 样式变更。
// 使用 CDP binding "__dwReportCursor" 回调 Go 侧。
const cursorDetectScript = `
(function() {
  var last = '';
  document.addEventListener('mouseover', function(e) {
    var c = getComputedStyle(e.target).cursor;
    if (c !== last) { last = c; __dwReportCursor(JSON.stringify(c)); }
  }, { capture: true, passive: true });
})();
`

// browserCoreImpl 实现 BrowserCore 接口。
// 组合 SnapshotEngine + ActionEngine + LiveViewEngine + TakeoverController。
type browserCoreImpl struct {
	mu                 sync.RWMutex
	allocCtx           context.Context
	allocCancel        context.CancelFunc
	browserCtx         context.Context
	browserCancel      context.CancelFunc
	snapEngine         *snapshotEngine
	actEngine          *actionEngine
	liveEngine         *liveViewEngine
	hub                *FrameBroadcastHub // 帧广播 hub，fan-out 给多 WS subscriber [CAP-BS09-C3]
	takeoverCtrl       *takeoverController
	fingerprintPreset  string
	persona            *Persona // 可选:Shell/Network/Env facet,经 applyPersonaEmulation realize
	runtimeMode        BrowserMode
	chromePath         string
	profileID          string
	profilePath        string
	launcher           *chromeLauncherImpl
	supervisor         *chromeSupervisorImpl
	chromePID          int
	liveViewActive     bool
	liveViewportW      int
	liveViewportH      int
	liveViewportDPR    float64
	liveViewportMobile bool
	liveViewportTouch  bool
	liveViewportMaxTP  int64
	onCursorChange     func(cursor string) // cursor 变更回调（由 SetupCursorDetection 设置）
	cursorDetectActive bool                // 防止重复注入 cursor 检测脚本
	targetTracker      *TargetTracker      // 多 Target 自动跟随 [CAP-BS09-C3 r3]
	displayMgr         *DisplayManager     // 虚拟 display 管理 (human 模式)
	sessionAttached    bool                // true = attach 到已有 session target，不拥有其生命周期
	chromeHandle       ChromeHandle        // direct/Workspace 启动路径的 Chrome 进程句柄
	virtualDisplay     *VirtualDisplayManager
	workspace          Workspace
	ownerIdentityKey   IdentityKey
	chromeSim          *BrowserChromeSpec // 非 nil = browser chrome 仿真启用（open 定死；SSOT 经 preset）

	// policyMu 独立保护 policy/lastURL，与 mu 解耦：Navigate 在持有 mu.RLock
	// 期间也需刷新 lastURL，若复用 mu 会触发 RWMutex 不可重入的写锁死锁。
	policyMu sync.RWMutex
	// policy 是 per-session 的安全/确定性策略（远程写门控）。零值 == deny。
	// lastURL 追踪 top-level URL，供 per-act origin 分类（EvaluateAct）。
	policy  SessionPolicy
	lastURL string
}

// SetPolicy 设置本 session 的安全策略，并（若给定）刷新用于 origin 分类的
// current URL。connectSession 在 attach 时以 sessionInfo.PageURL 调用，
// Navigate 成功后也会更新 lastURL 以保持进程内多 act 的 origin 新鲜。
func (impl *browserCoreImpl) SetPolicy(p SessionPolicy, currentURL string) {
	impl.policyMu.Lock()
	defer impl.policyMu.Unlock()
	impl.policy = p
	if currentURL != "" {
		impl.lastURL = currentURL
	}
}

// checkActPolicy 在执行前对单个 action 做远程写门控。它是 policy.go SSOT 的唯一
// 强制点：解析 op → 用当前 top-level origin 评估。解析失败时返回 nil，交由执行器
// 报原生错误。takeover-forwarded act 早在 Act/ActWithSessionMode 入口即被 ErrTakeoverActive
// 拒绝，永不到达此处，故无需坐标/takeover 特判。
//
// 关键(防 fail-open): origin 必须取 *实时* top-level URL——页面可能在两次 dw-browser
// Navigate 之间自导航(JS 重定向 / 点击跳到远程站)，若沿用旧 lastURL(如 localhost)
// 会把远程写误判为可信。故这里现场 EvalJS(window.location.href) 取实时 URL；取不到
// 时才回退 lastURL；两者皆空 → origin "" → 保守阻断。
func (impl *browserCoreImpl) checkActPolicy(ctx context.Context, action string) error {
	impl.policyMu.RLock()
	policy := impl.policy
	lastURL := impl.lastURL
	impl.policyMu.RUnlock()

	// 不持锁调用 ParseAction/EvaluateAct/EvalJS，避免与 Act 的 RLock 嵌套。
	parsed, err := ParseAction(action)
	if err != nil {
		return nil
	}
	// 读操作恒放行——跳过实时 URL 取用，省一次 CDP 往返。
	if !IsMutatingOp(parsed.Op) {
		return nil
	}
	curURL := impl.liveTopURL(ctx)
	if strings.TrimSpace(curURL) == "" {
		curURL = lastURL
	} else {
		// 用实时 URL 刷新 lastURL，保持后续分类与回退新鲜。
		impl.policyMu.Lock()
		impl.lastURL = curURL
		impl.policyMu.Unlock()
	}
	if d := policy.EvaluateAct(parsed.Op, URLOrigin(curURL)); !d.Allowed {
		// P3 TODO: RemoteWriteConfirm 命中时（d.NeedsConfirm）本轮最小实现——
		// confirm 与 deny 一样阻断（无 scenario 会映射到 confirm, 故此分支当前不可达）。
		return fmt.Errorf("dw-browser: %s", d.Reason)
	}
	return nil
}

// liveTopURL best-effort 取活跃 target 的实时 top-level URL，供 origin 分类。
// ctx 为 nil 或 eval 失败/为空时返回 ""，让 checkActPolicy 回退 lastURL（并最终
// fail-closed）。用活跃 target 的 window.location.href——act 也作用于该 target，
// 语义一致（新 tab / 自导航都被捕获）。
func (impl *browserCoreImpl) liveTopURL(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	var href string
	if err := impl.EvalJS(ctx, "window.location.href", &href); err != nil {
		return ""
	}
	return strings.TrimSpace(href)
}

// NewBrowserCore 创建并初始化 BrowserCore 实例。
// optFns 为可选参数: WithViewport / WithUserAgent / WithTouchEmulation / WithMode。
//
// 该 one-shot 入口必须与 BrowserPool 使用同一套三模式启动语义。所有 mode 都
// 先用 BuildDetachedChromeArgs fork 本机 Chrome，再通过 RemoteAllocator attach，
// 避免 CLI/测试入口绕过 CGVirtualDisplay/Workspace。
func NewBrowserCore(ctx context.Context, profileID string, optFns ...BrowserOption) (BrowserCore, error) {
	launcher := NewChromeLauncher()
	supervisor := NewChromeSupervisor()

	chromePath, err := launcher.FindChrome()
	if err != nil {
		return nil, err
	}

	bopts := &browserOptions{}
	for _, fn := range optFns {
		fn(bopts)
	}

	runtimeMode := ModeHeadless
	if bopts.hasMode {
		runtimeMode = bopts.mode
	}
	runtimeMode = NormalizeBrowserMode(runtimeMode, ModeHeadless)

	width, height := DefaultViewportWidth, DefaultViewportHeight
	if bopts.hasViewport {
		width, height = bopts.viewportW, bopts.viewportH
	}

	homeDir, _ := os.UserHomeDir()
	baseDir := filepath.Join(homeDir, ".deepwork", "browser-cli")
	profilePath := filepath.Join(baseDir, profileID)
	ownerKey := IdentityKey("browser-core-" + NormalizeProfileID(profileID))
	if err := RunStartupRecovery(profilePath, ownerKey); err != nil {
		return nil, fmt.Errorf("browser: startup recovery: %w", err)
	}
	if err := os.MkdirAll(profilePath, 0755); err != nil {
		return nil, fmt.Errorf("browser: create profile dir: %w", err)
	}
	if err := PrepareProfileForControlledLaunch(profilePath); err != nil {
		log.Printf("[BROWSER] profile launch hygiene failed profile=%s err=%v", profileID, err)
	}

	fingerprintPresetID := inferFingerprintPresetID(*bopts)
	defaultPreset := ResolveRuntimeFingerprintPreset(fingerprintPresetID, chromePath)
	if defaultPreset == nil {
		defaultPreset = BuiltinPresets[fingerprintPresetID]
	}
	if defaultPreset == nil {
		return nil, fmt.Errorf("browser: unknown preset_id %q", fingerprintPresetID)
	}

	launch, err := launchControlledBrowserCoreChrome(ctx, controlledBrowserCoreLaunchOptions{
		chromePath:   chromePath,
		profilePath:  profilePath,
		ownerKey:     ownerKey,
		mode:         runtimeMode,
		presetID:     fingerprintPresetID,
		preset:       defaultPreset,
		width:        width,
		height:       height,
		userAgent:    bopts.userAgent,
		hasUserAgent: bopts.hasUA,
		touch:        bopts.touch,
		stealth:      bopts.stealth,
	})
	if err != nil {
		return nil, err
	}

	allocCtx, allocCancel := chromedp.NewRemoteAllocator(ctx, launch.wsURL)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(chromedpErrorf))
	cleanupLaunch := func() {
		browserCancel()
		allocCancel()
		cleanupControlledBrowserCoreLaunch(launch, profilePath, ownerKey)
	}

	if err := chromedp.Run(browserCtx); err != nil {
		cleanupLaunch()
		return nil, fmt.Errorf("%w: CDP connection failed: %v", ErrCDPDisconnected, err)
	}

	if defaultPreset != nil {
		if runtimeMode == ModeHeadless {
			_ = applyFingerprintEmulation(browserCtx, chromePath, fingerprintPresetID)
			_ = chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
				_, err := page.AddScriptToEvaluateOnNewDocument(GenerateStealthScript(defaultPreset)).Do(ctx)
				return err
			}))
		} else {
			_ = chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
				_, err := page.AddScriptToEvaluateOnNewDocument(MinimalWebdriverStealthScript).Do(ctx)
				return err
			}))
		}
	}

	// Persona Shell/Network/Env facet realization(与 stealth 姿态正交,两模式都应用)。
	if bopts.persona != nil {
		_ = chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			return applyPersonaEmulation(ctx, bopts.persona)
		}))
	}

	snapEngine := newSnapshotEngine()
	actEngine := newActionEngine(snapEngine)
	liveEngine := newLiveViewEngine(width, height)
	takeoverCtrl := newTakeoverController(browserCtx)

	tracker := NewTargetTracker(browserCtx)
	if launch.virtualDisplay != nil && launch.chromePID > 0 {
		virtualDisplay := launch.virtualDisplay
		chromePID := launch.chromePID
		tracker.SetForegroundGuard(func(_ target.ID, _ string) error {
			return virtualDisplay.VerifyChromeContained(chromePID, BrowserMuxHostForegroundContainmentCheck)
		})
		tracker.SetNoFrontMode(true)
	}
	tracker.SetupListeners(browserCtx)

	impl := &browserCoreImpl{
		allocCtx:          allocCtx,
		allocCancel:       allocCancel,
		browserCtx:        browserCtx,
		browserCancel:     browserCancel,
		snapEngine:        snapEngine,
		actEngine:         actEngine,
		liveEngine:        liveEngine,
		takeoverCtrl:      takeoverCtrl,
		fingerprintPreset: fingerprintPresetID,
		persona:           bopts.persona,
		runtimeMode:       runtimeMode,
		chromePath:        chromePath,
		profileID:         profileID,
		profilePath:       profilePath,
		launcher:          launcher,
		supervisor:        supervisor,
		chromePID:         launch.chromePID,
		liveViewportW:     width,
		liveViewportH:     height,
		liveViewportDPR:   1,
		liveViewportTouch: bopts.touch,
		liveViewportMaxTP: 1,
		targetTracker:     tracker,
		displayMgr:        launch.displayMgr,
		chromeHandle:      launch.chromeHandle,
		virtualDisplay:    launch.virtualDisplay,
		workspace:         launch.workspace,
		ownerIdentityKey:  ownerKey,
	}
	if defaultPreset != nil {
		impl.liveViewportDPR = defaultPreset.DeviceScaleFactor
		impl.liveViewportMobile = defaultPreset.Mobile
		if defaultPreset.Touch {
			impl.liveViewportTouch = true
			impl.liveViewportMaxTP = int64(defaultPreset.MaxTouchPoints)
		}
	}

	return impl, nil
}

type controlledBrowserCoreLaunchOptions struct {
	chromePath   string
	profilePath  string
	ownerKey     IdentityKey
	mode         BrowserMode
	presetID     string
	preset       *FingerprintPreset
	width        int
	height       int
	userAgent    string
	hasUserAgent bool
	touch        bool
	stealth      bool
}

type controlledBrowserCoreLaunchResult struct {
	wsURL          string
	chromePID      int
	chromeHandle   ChromeHandle
	displayMgr     *DisplayManager
	virtualDisplay *VirtualDisplayManager
	workspace      Workspace
}

func launchControlledBrowserCoreChrome(ctx context.Context, opts controlledBrowserCoreLaunchOptions) (*controlledBrowserCoreLaunchResult, error) {
	mode := opts.mode
	mode = NormalizeBrowserMode(mode, ModeHeadless)

	width, height := opts.width, opts.height
	if width <= 0 {
		width = DefaultViewportWidth
	}
	if height <= 0 {
		height = DefaultViewportHeight
	}

	result := &controlledBrowserCoreLaunchResult{}
	var extraArgs []string
	var virtualX, virtualY int

	switch mode {
	case ModeHeadless:
	case ModeHeaded:
		switch runtime.GOOS {
		case "linux":
			dm := &DisplayManager{}
			if !dm.EnsureDisplay() {
				return nil, fmt.Errorf("browser: headed mode unavailable on linux: Xvfb display setup failed")
			}
			result.displayMgr = dm
		case "darwin":
			vd := &VirtualDisplayManager{}
			if err := vd.Ensure(); err != nil {
				return nil, fmt.Errorf("browser: headed mode unavailable on macOS: CGVirtualDisplay setup failed: %w", err)
			}
			virtualX, virtualY = vd.WindowPosition()
			extraArgs = append(extraArgs, fmt.Sprintf("--window-position=%d,%d", virtualX, virtualY))
			result.virtualDisplay = vd
		default:
			return nil, fmt.Errorf("browser: headed mode unavailable on %s: invisible headed display strategy is not implemented", runtime.GOOS)
		}
	case ModeVisible:
		if runtime.GOOS == "linux" {
			dm := &DisplayManager{}
			if !dm.EnsureDisplay() {
				return nil, fmt.Errorf("browser: visible mode unavailable on linux: Xvfb display setup failed")
			}
			result.displayMgr = dm
		}
		result.workspace = NewWorkspace()
	default:
		return nil, fmt.Errorf("browser: unsupported browser mode %q", mode)
	}

	cdpPort, err := findAvailableCDPPort()
	if err != nil {
		cleanupControlledBrowserCoreLaunch(result, opts.profilePath, opts.ownerKey)
		return nil, fmt.Errorf("browser: allocate CDP port: %w", err)
	}

	userAgent := ""
	if opts.hasUserAgent {
		userAgent = opts.userAgent
	}
	launchArgs := BuildDetachedChromeArgs(DetachedChromeLaunchOptions{
		DebugPort:  cdpPort,
		ProfileDir: opts.profilePath,
		Width:      width,
		Height:     height,
		PresetID:   opts.presetID,
		UserAgent:  userAgent,
		Touch:      opts.touch,
		Mode:       mode,
		Stealth:    opts.stealth,
	})
	for _, arg := range extraArgs {
		launchArgs = appendChromeArgBeforeURL(launchArgs, arg)
	}
	if proxy, source := resolveBrowserPoolProxy(); proxy != "" {
		launchArgs = appendChromeArgBeforeURL(launchArgs, "--proxy-server="+proxy)
		log.Printf("[BROWSER] controlled launch proxy-server=%s (from %s)", proxy, source)
	}

	var handle ChromeHandle
	if mode == ModeVisible {
		h, err := result.workspace.LaunchChromeInSpace(ChromeLaunchSpec{
			ChromePath:   opts.chromePath,
			Args:         launchArgs,
			DebugPort:    cdpPort,
			ReadyTimeout: BrowserMuxHostLaunchReadyTimeout,
		})
		if err != nil {
			cleanupControlledBrowserCoreLaunch(result, opts.profilePath, opts.ownerKey)
			return nil, fmt.Errorf("browser: workspace launch chrome: %w", err)
		}
		handle = h
	} else {
		h, err := startChromeProcess(ChromeLaunchSpec{
			ChromePath:   opts.chromePath,
			Args:         launchArgs,
			DebugPort:    cdpPort,
			ReadyTimeout: BrowserMuxHostLaunchReadyTimeout,
		})
		if err != nil {
			cleanupControlledBrowserCoreLaunch(result, opts.profilePath, opts.ownerKey)
			return nil, fmt.Errorf("browser: launch chrome: %w", err)
		}
		handle = h
	}
	result.chromeHandle = handle
	result.chromePID = handle.PID()
	result.wsURL = handle.WSURL()

	if err := WriteProfileOwnerMarker(opts.profilePath, opts.ownerKey, result.chromePID, cdpPort); err != nil {
		cleanupControlledBrowserCoreLaunch(result, opts.profilePath, opts.ownerKey)
		return nil, fmt.Errorf("browser: write profile owner marker: %w", err)
	}

	if result.virtualDisplay != nil {
		allocCtx, allocCancel := chromedp.NewRemoteAllocator(ctx, result.wsURL)
		windowCtx, windowCancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(chromedpErrorf))
		boundsW, boundsH := width, height
		if opts.preset != nil {
			boundsW, boundsH = opts.preset.ViewportW, opts.preset.ViewportH
		}
		err := runCDPWithSoftTimeout(windowCtx, BrowserMuxHostWindowEnforceTimeout, chromedp.ActionFunc(func(ctx context.Context) error {
			windowID, _, err := cdpbrowser.GetWindowForTarget().Do(ctx)
			if err != nil {
				return fmt.Errorf("get chrome window for target: %w", err)
			}
			bounds := &cdpbrowser.Bounds{
				Left:        int64(virtualX),
				Top:         int64(virtualY),
				Width:       int64(boundsW),
				Height:      int64(boundsH),
				WindowState: cdpbrowser.WindowStateNormal,
			}
			if err := cdpbrowser.SetWindowBounds(windowID, bounds).Do(ctx); err != nil {
				return fmt.Errorf("set chrome window bounds to virtual display: %w", err)
			}
			return nil
		}))
		windowCancel()
		allocCancel()
		if err != nil {
			cleanupControlledBrowserCoreLaunch(result, opts.profilePath, opts.ownerKey)
			return nil, fmt.Errorf("browser: enforce CGVirtualDisplay window bounds: %w", err)
		}
		if err := result.virtualDisplay.VerifyChromeContained(result.chromePID, BrowserMuxHostWindowContainmentTimeout); err != nil {
			cleanupControlledBrowserCoreLaunch(result, opts.profilePath, opts.ownerKey)
			return nil, err
		}
	}

	return result, nil
}

func cleanupControlledBrowserCoreLaunch(result *controlledBrowserCoreLaunchResult, profilePath string, ownerKey IdentityKey) {
	if result == nil {
		return
	}
	if result.chromeHandle != nil {
		_ = result.chromeHandle.Kill()
		result.chromeHandle = nil
	}
	if result.displayMgr != nil {
		_ = result.displayMgr.Close()
		result.displayMgr = nil
	}
	if result.virtualDisplay != nil {
		_ = result.virtualDisplay.Close()
		result.virtualDisplay = nil
	}
	if result.workspace != nil {
		_ = result.workspace.Close()
		result.workspace = nil
	}
	RemoveProfileOwnerMarker(profilePath, ownerKey)
}

// handleCrash 处理 Chrome 崩溃，尝试自动重启。
func (impl *browserCoreImpl) handleCrash(ctx context.Context) {
	impl.mu.Lock()
	defer impl.mu.Unlock()

	// 尝试重启
	cdpURL, pid, err := impl.supervisor.RestartWithBackoff(ctx, impl.launcher, impl.profileID, 3)
	if err != nil {
		return
	}

	// 重建 CDP 连接
	if impl.allocCancel != nil {
		impl.allocCancel()
	}
	if impl.browserCancel != nil {
		impl.browserCancel()
	}

	allocCtx, allocCancel := chromedp.NewRemoteAllocator(ctx, cdpURL)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(chromedpErrorf))

	impl.allocCtx = allocCtx
	impl.allocCancel = allocCancel
	impl.browserCtx = browserCtx
	impl.browserCancel = browserCancel
	impl.chromePID = pid

	// 重建 TakeoverController（新 CDP context）
	impl.takeoverCtrl = newTakeoverController(browserCtx)
}

// Navigate 导航到 URL，返回 A11y 快照。
func (impl *browserCoreImpl) Navigate(ctx context.Context, url string) (*Snapshot, error) {
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	targetID, targetCtx := impl.currentTargetRef()
	runCtx, cancel := deriveTargetContext(ctx, targetCtx)
	defer cancel()
	logCtx := obs.WithStage(ctx, STGSessionNavigate)
	totalStart := time.Now()
	logger.Info(logCtx, "browser navigate core started",
		"url", url,
		"target_id", targetID)

	// 使用低级 Page.navigate 避免 chromedp.Navigate 在 ERR_ABORTED 时损坏 session。
	// SPA (Vue/React) 的 module preload 常触发 ERR_ABORTED，但页面最终可正常渲染。
	pageNavigateStart := time.Now()
	if err := chromedp.Run(runCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, _, _, err := page.Navigate(url).Do(ctx)
		return err
	})); err != nil {
		logger.Warn(logCtx, "browser navigate cdp failed",
			"url", url,
			"target_id", targetID,
			"elapsed_ms", time.Since(pageNavigateStart).Milliseconds(),
			"error", err)
		return nil, fmt.Errorf("browser: navigate to %q: %w", url, err)
	}
	logger.Info(logCtx, "browser navigate cdp completed",
		"url", url,
		"target_id", targetID,
		"elapsed_ms", time.Since(pageNavigateStart).Milliseconds())

	// 等待页面进入可交互状态，再交给 DOM settle 收敛动态内容。
	// readyState=complete 会被广告、长连接或慢资源拖慢；Human Portal 更需要首屏可用。
	readyStart := time.Now()
	if err := chromedp.Run(runCtx, chromedp.Poll(
		`document.readyState === "interactive" || document.readyState === "complete" || !!document.body`,
		nil,
		chromedp.WithPollingInterval(navigateReadyPollInterval),
		chromedp.WithPollingTimeout(navigateReadyTimeout),
	)); err != nil {
		logger.Warn(logCtx, "browser navigate ready wait failed",
			"url", url,
			"target_id", targetID,
			"elapsed_ms", time.Since(readyStart).Milliseconds(),
			"timeout_ms", navigateReadyTimeout.Milliseconds(),
			"error", err)
	} else {
		logger.Info(logCtx, "browser navigate ready completed",
			"url", url,
			"target_id", targetID,
			"elapsed_ms", time.Since(readyStart).Milliseconds())
	}

	// SPA 等待 DOM 稳定（替代硬编码 Sleep，使用 MutationObserver idle 检测）
	domSettleStart := time.Now()
	if err := waitForDOMSettle(runCtx, navigateDOMIdleMs, navigateDOMSettleTimeoutMs); err != nil {
		logger.Warn(logCtx, "browser navigate dom settle failed",
			"url", url,
			"target_id", targetID,
			"elapsed_ms", time.Since(domSettleStart).Milliseconds(),
			"error", err)
	} else {
		logger.Info(logCtx, "browser navigate dom settled",
			"url", url,
			"target_id", targetID,
			"elapsed_ms", time.Since(domSettleStart).Milliseconds())
	}

	// 终局语义: 地址栏导航应始终作用于"当前活跃 target"。
	// 之前无条件回切 root target，会让新 tab / auth target 的 URL 输入回车误导航 root tab，
	// 直接破坏多 tab 基本语义，也会让 ChatGPT/Apple 登录流程回错标签。
	snapshotStart := time.Now()
	snap, err := impl.snapEngine.GetSnapshot(runCtx)
	if snap != nil {
		snap.TargetID = targetID
	}
	if err != nil {
		logger.Warn(logCtx, "browser navigate snapshot failed",
			"url", url,
			"target_id", targetID,
			"elapsed_ms", time.Since(snapshotStart).Milliseconds(),
			"total_elapsed_ms", time.Since(totalStart).Milliseconds(),
			"error", err)
	} else {
		refsCount := 0
		snapshotType := ""
		if snap != nil {
			refsCount = len(snap.Refs)
			snapshotType = snap.SnapshotType
		}
		logger.Info(logCtx, "browser navigate snapshot completed",
			"url", url,
			"target_id", targetID,
			"elapsed_ms", time.Since(snapshotStart).Milliseconds(),
			"total_elapsed_ms", time.Since(totalStart).Milliseconds(),
			"refs_count", refsCount,
			"snapshot_type", snapshotType)
	}
	// 更新 lastURL，保持进程内后续 act 的 origin 分类新鲜（policy 门控依赖）。
	// 用 policyMu 而非 mu：本函数已持有 mu.RLock，复用会死锁（RWMutex 不可重入）。
	if snap != nil && snap.URL != "" {
		impl.policyMu.Lock()
		impl.lastURL = snap.URL
		impl.policyMu.Unlock()
	}
	return snap, err
}

// NavigateCommand sends a navigation command to the active target and returns
// after Chrome accepts Page.navigate. Human-facing live views use this path so
// address-bar input is not blocked by A11y snapshot and screenshot collection.
func (impl *browserCoreImpl) NavigateCommand(ctx context.Context, url string) (string, error) {
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	targetID, targetCtx := impl.currentTargetRef()
	runCtx, cancel := deriveTargetContext(ctx, targetCtx)
	defer cancel()
	logCtx := obs.WithStage(ctx, STGSessionNavigate)
	start := time.Now()
	logger.Info(logCtx, "browser navigate command started",
		"url", url,
		"target_id", targetID)
	if err := chromedp.Run(runCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, _, _, err := page.Navigate(url).Do(ctx)
		return err
	})); err != nil {
		logger.Warn(logCtx, "browser navigate command failed",
			"url", url,
			"target_id", targetID,
			"elapsed_ms", time.Since(start).Milliseconds(),
			"error", err)
		return targetID, fmt.Errorf("browser: navigate command to %q: %w", url, err)
	}
	logger.Info(logCtx, "browser navigate command completed",
		"url", url,
		"target_id", targetID,
		"elapsed_ms", time.Since(start).Milliseconds())
	return targetID, nil
}

// NavigateTargetCommand sends a navigation command to a concrete target. It is
// used by Human Browser Portal async navigation, where the target is captured
// at user intent time and must not drift if the active tab changes later.
func (impl *browserCoreImpl) NavigateTargetCommand(ctx context.Context, targetID string, url string) (string, error) {
	if targetID == "" {
		return impl.NavigateCommand(ctx, url)
	}
	impl.mu.RLock()
	var targetCtx context.Context
	if impl.targetTracker != nil {
		targetCtx = impl.targetTracker.TargetCDPContext(targetID)
	}
	if targetCtx == nil {
		_, targetCtx = impl.currentTargetRef()
	}
	runCtx, cancel := deriveTargetContext(ctx, targetCtx)
	defer cancel()
	impl.mu.RUnlock()

	logCtx := obs.WithStage(ctx, STGSessionNavigate)
	start := time.Now()
	logger.Info(logCtx, "browser navigate target command started",
		"url", url,
		"target_id", targetID)
	if err := chromedp.Run(runCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, _, _, err := page.Navigate(url).Do(ctx)
		return err
	})); err != nil {
		logger.Warn(logCtx, "browser navigate target command failed",
			"url", url,
			"target_id", targetID,
			"elapsed_ms", time.Since(start).Milliseconds(),
			"error", err)
		return targetID, fmt.Errorf("browser: navigate target command to %q: %w", url, err)
	}
	logger.Info(logCtx, "browser navigate target command completed",
		"url", url,
		"target_id", targetID,
		"elapsed_ms", time.Since(start).Milliseconds())
	return targetID, nil
}

// Snap 获取当前页面 A11y 快照。
func (impl *browserCoreImpl) Snap(ctx context.Context) (*Snapshot, error) {
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	targetCtx := impl.currentCtx()
	runCtx, cancel := deriveTargetContext(ctx, targetCtx)
	defer cancel()
	return impl.snapEngine.GetSnapshot(runCtx)
}

// Act 执行操作，observe=false 时不返回 Snapshot。
func (impl *browserCoreImpl) Act(ctx context.Context, action string, observe bool) (*Snapshot, error) {
	// 接管模式下，AI 操作被拒绝 [TC-09-U-11]
	if impl.takeoverCtrl.IsTakeover() {
		return nil, ErrTakeoverActive
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// 用同一 ctx 取实时 origin 做远程写门控（须在 nil 保护之后）。
	if err := impl.checkActPolicy(ctx, action); err != nil {
		return nil, err
	}

	impl.mu.RLock()
	targetCtx := impl.currentCtx()
	runCtx, cancelRun := deriveTargetContext(ctx, targetCtx)

	type actionResult struct {
		snap *Snapshot
		err  error
	}
	done := make(chan actionResult, 1)
	go func() {
		defer impl.mu.RUnlock()
		snap, err := impl.actEngine.Execute(runCtx, action, observe)
		done <- actionResult{snap: snap, err: err}
	}()
	defer cancelRun()

	timeout := 20 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		if remain := time.Until(deadline); remain <= 0 {
			return nil, ctx.Err()
		} else if remain < timeout {
			timeout = remain
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-done:
		return result.snap, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("browser: action %q timed out after %s", action, timeout)
	}
}

// Text 提取当前页面纯文本。
func (impl *browserCoreImpl) Text(ctx context.Context, focus *string) (string, error) {
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	targetCtx := impl.currentCtx()
	runCtx, cancel := deriveTargetContext(ctx, targetCtx)
	defer cancel()
	return impl.snapEngine.GetText(runCtx, focus)
}

// Screenshot 截图。chrome 仿真会话在此合成 Safari chrome 层（页面外绘制，
// 所有截图消费者——observe/screenshot/evidence——统一经过这个唯一出口）。
func (impl *browserCoreImpl) Screenshot(ctx context.Context, annotate bool) ([]byte, error) {
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	targetCtx := impl.currentCtx()
	runCtx, cancel := deriveTargetContext(ctx, targetCtx)
	defer cancel()
	data, err := impl.snapEngine.Screenshot(runCtx, annotate)
	if err != nil || impl.chromeSim == nil {
		return data, err
	}
	// 主题取色（真机行为：底栏随页面 theme-color/背景）；探测失败用浅色缺省。
	var theme string
	_ = chromedp.Run(runCtx, chromedp.Evaluate(browserChromeThemeProbeJS, &theme))
	composed, cErr := CompositeBrowserChrome(data, impl.chromeSim, theme, annotate)
	if cErr != nil {
		log.Printf("[BROWSER] browser-chrome composite failed (returning raw screenshot): %v", cErr)
		return data, nil
	}
	return composed, nil
}

func (impl *browserCoreImpl) ScreenshotTarget(ctx context.Context, targetID string, annotate bool) ([]byte, error) {
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	var targetCtx context.Context
	if impl.targetTracker != nil && targetID != "" {
		targetCtx = impl.targetTracker.TargetCDPContext(targetID)
	}
	if targetCtx == nil {
		targetCtx = impl.currentCtx()
	}
	runCtx, cancel := deriveTargetContext(ctx, targetCtx)
	defer cancel()
	return impl.snapEngine.Screenshot(runCtx, annotate)
}

// StartLiveView 启动 Screencast 帧推送，返回 hub（多 WS 连接共享同一 Screencast 流）。
//
// 多次调用安全: 已活跃时直接返回现有 hub，不重启 Screencast。
// subscriber 通过 hub.Subscribe(connID) 获取独立帧 channel [CAP-BS09-C3 r2]。
func (impl *browserCoreImpl) StartLiveView(ctx context.Context) (*FrameBroadcastHub, error) {
	impl.mu.Lock()
	defer impl.mu.Unlock()

	// 已有活跃 LiveView → 返回现有 hub（多 WS 连接共享同一个 Screencast 流）
	// 不重启 — 重启会中断现有 subscriber [TH-0405-k8r]
	if impl.liveViewActive && impl.hub != nil {
		if impl.targetTracker != nil {
			impl.targetTracker.SetLiveEngine(impl.liveEngine, impl.hub)
		}
		return impl.hub, nil
	}

	impl.hub = NewFrameBroadcastHub()
	if err := impl.liveEngine.StartScreencast(impl.browserCtx, impl.hub); err != nil {
		impl.hub = nil
		return nil, err
	}
	impl.liveViewActive = true

	// 连接 TargetTracker → 新 Target 创建时自动切换 Screencast [r3]
	if impl.targetTracker != nil {
		impl.targetTracker.SetLiveEngine(impl.liveEngine, impl.hub)
	}

	return impl.hub, nil
}

// StopLiveView 停止 Screencast。
func (impl *browserCoreImpl) StopLiveView(ctx context.Context) error {
	impl.mu.Lock()
	defer impl.mu.Unlock()

	if !impl.liveViewActive {
		return nil
	}
	impl.liveViewActive = false
	return impl.liveEngine.StopScreencast(impl.browserCtx)
}

// EnableTakeover 切换到 Takeover 模式。
func (impl *browserCoreImpl) EnableTakeover(ctx context.Context) error {
	return impl.takeoverCtrl.EnableTakeover(func() {
		// 超时自动释放时的回调（广播 WS 消息由 webui 层处理）
	})
}

// DisableTakeover 释放接管，恢复 OBSERVE 模式。
func (impl *browserCoreImpl) DisableTakeover(ctx context.Context) error {
	return impl.takeoverCtrl.DisableTakeover()
}

// DispatchInput 在接管模式下转发输入事件。
func (impl *browserCoreImpl) DispatchInput(ctx context.Context, event *InputEvent) error {
	return impl.takeoverCtrl.DispatchInput(ctx, event)
}

// Close 关闭 Browser，释放资源。
func (impl *browserCoreImpl) Close(ctx context.Context) error {
	impl.mu.Lock()
	defer impl.mu.Unlock()

	if impl.liveViewActive {
		_ = impl.liveEngine.StopScreencast(impl.browserCtx)
		impl.liveViewActive = false
	}

	// Session attach 模式只是在已有 Chrome/page target 上短暂附着。
	// 若这里调用 chromedp context cancel，会把用户正在复用的 page target 一起关掉，
	// 导致 dw-browser session 命令执行几次后外层页面退化成 ChromeInitialPageURL。
	// 这些 attach-only CLI 命令本身是短进程；连接资源由进程退出时回收即可。
	if impl.sessionAttached {
		return nil
	}

	if impl.browserCancel != nil {
		impl.browserCancel()
		impl.browserCancel = nil
	}
	if impl.allocCancel != nil {
		impl.allocCancel()
		impl.allocCancel = nil
	}
	if impl.chromeHandle != nil {
		_ = impl.chromeHandle.Kill()
		impl.chromeHandle = nil
	} else if impl.chromePID > 0 {
		_ = impl.launcher.Kill(impl.chromePID)
	}
	if impl.ownerIdentityKey != "" && impl.profilePath != "" {
		RemoveProfileOwnerMarker(impl.profilePath, impl.ownerIdentityKey)
	}

	// 清理虚拟 display (Xvfb)
	if impl.displayMgr != nil {
		_ = impl.displayMgr.Close()
		impl.displayMgr = nil
	}
	if impl.virtualDisplay != nil {
		_ = impl.virtualDisplay.Close()
		impl.virtualDisplay = nil
	}
	if impl.workspace != nil {
		_ = impl.workspace.Close()
		impl.workspace = nil
	}

	return nil
}

// EvalJS 在浏览器中执行 JavaScript 表达式。
func (impl *browserCoreImpl) EvalJS(ctx context.Context, expr string, result interface{}) error {
	if ctx == nil {
		ctx = context.Background()
	}
	impl.mu.RLock()
	targetCtx := impl.currentCtx()
	impl.mu.RUnlock()
	runCtx, cancelRun := deriveTargetContext(ctx, targetCtx)
	defer cancelRun()
	// [BUG-FIX] GoBack/Forward/Reload + URL sync 必须在活跃 Target 执行。
	// [TH-0419-q5b] 走 currentCtx() 统一入口, 不再内联 tracker lookup.
	return evalJSInContext(ctx, runCtx, expr, result)
}

func (impl *browserCoreImpl) EvalJSTarget(ctx context.Context, targetID string, expr string, result interface{}) error {
	if ctx == nil {
		ctx = context.Background()
	}
	impl.mu.RLock()
	var targetCtx context.Context
	if impl.targetTracker != nil && targetID != "" {
		targetCtx = impl.targetTracker.TargetCDPContext(targetID)
	}
	impl.mu.RUnlock()
	if targetCtx == nil {
		return fmt.Errorf("browser: target %q is not available", targetID)
	}
	runCtx, cancelRun := deriveTargetContext(ctx, targetCtx)
	defer cancelRun()
	return evalJSInContext(ctx, runCtx, expr, result)
}

func evalJSInContext(parent context.Context, runCtx context.Context, expr string, result interface{}) error {
	timeout := 8 * time.Second
	if deadline, ok := parent.Deadline(); ok {
		if remain := time.Until(deadline); remain <= 0 {
			return parent.Err()
		} else if remain < timeout {
			timeout = remain
		}
	}
	done := make(chan error, 1)
	go func() {
		done <- chromedp.Run(runCtx, chromedp.Evaluate(expr, result))
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-parent.Done():
		return parent.Err()
	case <-timer.C:
		return fmt.Errorf("browser: JavaScript evaluation timed out after %s", timeout)
	}
}

// GetTargetTracker 返回 TargetTracker [r3]。
func (impl *browserCoreImpl) GetTargetTracker() *TargetTracker {
	return impl.targetTracker
}

// SetLiveViewport 记录并应用当前 liveview viewport 配置。
// 这份配置属于“当前人类可见容器语义”，必须在活跃 target 与后续新 target 间保持一致，
// 否则多 tab / auth popup 会回退到启动时 preset viewport，产生黑边或操作区裁剪。
func (impl *browserCoreImpl) SetLiveViewport(width, height int, dpr float64, mobile bool) error {
	impl.mu.Lock()
	impl.liveViewportW = width
	impl.liveViewportH = height
	impl.liveViewportDPR = dpr
	impl.liveViewportMobile = mobile
	impl.liveEngine.viewportW = width
	impl.liveEngine.viewportH = height

	targetCtx := impl.currentCtx()
	screencastCtx := impl.liveEngine.ctx
	liveViewActive := impl.liveViewActive
	touch := impl.liveViewportTouch
	maxTouchPoints := impl.liveViewportMaxTP
	impl.mu.Unlock()

	// Callers rely on the interface contract: once this returns, subsequent eval,
	// pointer input, and screenshots must observe the requested device semantics.
	if err := impl.applyLiveViewport(targetCtx, width, height, dpr, mobile, touch, maxTouchPoints); err != nil {
		return fmt.Errorf("apply live viewport: %w", err)
	}

	// Screencast restart is presentation plumbing; it need not delay the device
	// semantics postcondition above.
	if liveViewActive {
		if screencastCtx == nil {
			screencastCtx = targetCtx
		}
		go func() {
			if err := impl.liveEngine.RestartScreencast(screencastCtx); err != nil {
				log.Printf("[BROWSER-LIVEVIEW] viewport update screencast restart failed: %v", err)
			} else {
				log.Printf("[BROWSER-LIVEVIEW] viewport updated: %dx%d dpr=%.1f mobile=%v", width, height, dpr, mobile)
			}
		}()
	}

	return nil
}

// SyncActiveTargetViewport 将最近一次 liveview viewport 配置重放到新活跃 target。
// 典型场景:
//   - 多 tab 新建后切到新 target
//   - 登录流程跳到 auth popup / 新标签页
//   - iOS Safari 新 target 需继续保留 mobile/touch 语义
func (impl *browserCoreImpl) SyncActiveTargetViewport(targetCtx context.Context) error {
	impl.mu.RLock()
	width := impl.liveViewportW
	height := impl.liveViewportH
	dpr := impl.liveViewportDPR
	mobile := impl.liveViewportMobile
	touch := impl.liveViewportTouch
	maxTouchPoints := impl.liveViewportMaxTP
	impl.mu.RUnlock()

	if targetCtx == nil {
		return nil
	}
	if width <= 0 || height <= 0 {
		return nil
	}
	if err := impl.applyLiveViewport(targetCtx, width, height, dpr, mobile, touch, maxTouchPoints); err != nil {
		return err
	}

	log.Printf("[BROWSER-LIVEVIEW] target viewport replayed: %dx%d dpr=%.1f mobile=%v touch=%v", width, height, dpr, mobile, touch)
	return nil
}

// SetupCursorDetection 实现 CursorDetector 接口。
// 向 Chrome 注入 cursor 检测脚本，每个新 document 自动运行；通过 CDP binding 回调 Go 侧。
// 幂等: 重复调用仅更新回调，不重复注入脚本。
// onCursor 会在 cursor 样式变更时被调用（goroutine safe）。
func (impl *browserCoreImpl) SetupCursorDetection(onCursor func(cursor string)) error {
	impl.mu.Lock()
	impl.onCursorChange = onCursor
	alreadyActive := impl.cursorDetectActive
	if !alreadyActive {
		impl.cursorDetectActive = true
	}
	impl.mu.Unlock()

	// 已激活 → 仅更新回调，不重复注入
	if alreadyActive {
		return nil
	}

	impl.mu.RLock()
	browserCtx := impl.browserCtx
	impl.mu.RUnlock()

	// 1. 绑定 JS→Go 命名 binding
	if err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return cdpruntime.AddBinding("__dwReportCursor").Do(ctx)
	})); err != nil {
		return fmt.Errorf("browser: cursor binding: %w", err)
	}

	// 2. 监听 binding 调用事件，解析 cursor 字符串并回调
	chromedp.ListenTarget(browserCtx, func(ev interface{}) {
		bindingEv, ok := ev.(*cdpruntime.EventBindingCalled)
		if !ok || bindingEv.Name != "__dwReportCursor" {
			return
		}
		var cursor string
		if err := json.Unmarshal([]byte(bindingEv.Payload), &cursor); err != nil {
			return
		}
		impl.mu.RLock()
		cb := impl.onCursorChange
		impl.mu.RUnlock()
		if cb != nil {
			cb(cursor)
		}
	})

	// 3. 注入脚本到每个新 document（CSP 绕过）
	if err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(cursorDetectScript).Do(ctx)
		return err
	})); err != nil {
		return fmt.Errorf("browser: cursor script inject: %w", err)
	}

	// 4. 绕过 CSP，确保 binding 在严格 CSP 站点也能生效
	_ = chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return page.SetBypassCSP(true).Do(ctx)
	}))

	// 5. 在当前已加载页面立即执行 cursor 检测脚本
	// AddScriptToEvaluateOnNewDocument 只对未来加载的页面生效,当前页面需要手动 Evaluate
	_ = chromedp.Run(browserCtx, chromedp.Evaluate(cursorDetectScript, nil))

	return nil
}

// SnapWithSessionMode 获取快照（session 模式，使用 @rN ref）。
func (impl *browserCoreImpl) SnapWithSessionMode(ctx context.Context, snapEpoch int) (*Snapshot, error) {
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	targetCtx := impl.currentCtx()
	runCtx, cancel := deriveTargetContext(ctx, targetCtx)
	defer cancel()
	return impl.snapEngine.GetSnapshotWithSessionMode(runCtx, snapEpoch)
}

// SnapWithOptions 获取快照并应用 SnapOptions 过滤 [r2 Delta-REQ TH-0418-c9x]。
func (impl *browserCoreImpl) SnapWithOptions(ctx context.Context, opts SnapOptions) (*Snapshot, error) {
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	targetCtx := impl.currentCtx()
	runCtx, cancel := deriveTargetContext(ctx, targetCtx)
	defer cancel()
	return impl.snapEngine.SnapWithOptions(runCtx, opts)
}

// ActWithSessionMode 执行操作（session 模式，允许 @rN ref）。
func (impl *browserCoreImpl) ActWithSessionMode(ctx context.Context, action string, observe bool) (*Snapshot, error) {
	if impl.takeoverCtrl.IsTakeover() {
		return nil, ErrTakeoverActive
	}
	if err := impl.checkActPolicy(ctx, action); err != nil {
		return nil, err
	}
	impl.mu.RLock()
	defer impl.mu.RUnlock()
	targetCtx := impl.currentCtx()
	runCtx, cancel := deriveTargetContext(ctx, targetCtx)
	defer cancel()
	return impl.actEngine.ExecuteWithSessionMode(runCtx, action, observe, true)
}

// RestoreRefsFromSession 从 session 文件恢复 ref 表（供 act 命令使用）。
func (impl *browserCoreImpl) RestoreRefsFromSession(refs []SessionRef) {
	impl.mu.Lock()
	defer impl.mu.Unlock()
	impl.snapEngine.refTable = make(map[string]int64, len(refs))
	impl.snapEngine.refMeta = make(map[string]*ElementRef, len(refs)*2)
	for i := range refs {
		r := refs[i]
		elem := &ElementRef{
			Ref:           r.Ref,
			BackendNodeID: r.BackendNodeID,
			Role:          r.Role,
			Name:          r.Name,
			NameFull:      r.Name,
			NameShort:     r.Name,
			TestID:        r.TestID,
			Placeholder:   r.Placeholder,
			Interactable:  true,
		}
		impl.snapEngine.refTable[r.Ref] = r.BackendNodeID
		impl.snapEngine.refMeta[r.Ref] = elem
		if r.TestID != "" {
			impl.snapEngine.refMeta["#"+r.TestID] = elem
		}
	}
}

// NewBrowserCoreFromSession 连接到已有 Chrome 实例（通过 CDP WebSocket URL）。
// 用于 session 模式：open 后 snap/act/get/wait 都连接到同一个 Chrome 进程。
func NewBrowserCoreFromSession(ctx context.Context, wsURL string, targetID string, presetID string, personaID string, modes ...BrowserMode) (SessionCore, error) {
	var allocCtxOpts []chromedp.ExecAllocatorOption
	_ = allocCtxOpts
	runtimeMode := ModeVisible
	if len(modes) > 0 {
		runtimeMode = NormalizeBrowserMode(modes[0], ModeVisible)
	}

	// Session attach 命令复用的是“已经存在的页面 target”。
	// 若把 chromedp allocator/context 直接挂到调用方的超时 ctx 下面，
	// 命令退出时的 ctx cancel 会顺带把目标页一并关闭。
	// 这里改为独立于命令超时的 background 根 context，命令进程结束时仅丢连接，不关页面。
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), wsURL)

	var ctxOpts []chromedp.ContextOption
	if targetID != "" {
		ctxOpts = append(ctxOpts, chromedp.WithTargetID(target.ID(targetID)))
	}
	ctxOpts = append([]chromedp.ContextOption{chromedp.WithErrorf(chromedpErrorf)}, ctxOpts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, ctxOpts...)

	// Warm up CDP connection
	if err := chromedp.Run(browserCtx); err != nil {
		allocCancel()
		browserCancel()
		return nil, fmt.Errorf("%w: session CDP connection failed: %v", ErrCDPDisconnected, err)
	}

	// session/open 链路也必须遵守三模式隔离：headless 才补完整
	// fingerprint/stealth；headed/visible 保留真实浏览器面，只隐藏 webdriver。
	presetID = NormalizePresetID(presetID)
	chromePath, _ := NewChromeLauncher().FindChrome()

	sessionPreset := ResolveRuntimeFingerprintPreset(presetID, chromePath)
	if sessionPreset == nil {
		sessionPreset = BuiltinPresets[presetID]
	}
	if sessionPreset == nil {
		allocCancel()
		browserCancel()
		return nil, fmt.Errorf("browser: unknown preset_id %q", presetID)
	}
	if sessionPreset != nil {
		if runtimeMode == ModeHeadless {
			_ = applyFingerprintEmulation(browserCtx, chromePath, presetID)
			_ = chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
				_, err := page.AddScriptToEvaluateOnNewDocument(GenerateStealthScript(sessionPreset)).Do(ctx)
				return err
			}))
		} else {
			_ = chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
				_, err := page.AddScriptToEvaluateOnNewDocument(MinimalWebdriverStealthScript).Do(ctx)
				return err
			}))
		}
	}

	// persona facet 随 preset 同源施加(单一机制;open 命令经此 attach 路径生效)。
	sessionPersona := resolvePersonaOrNil(personaID)
	if sessionPersona != nil {
		_ = chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			return applyPersonaEmulation(ctx, sessionPersona)
		}))
	}

	snapEngine := newSnapshotEngine()
	actEngine := newActionEngine(snapEngine)
	liveEngine := newLiveViewEngine(DefaultViewportWidth, DefaultViewportHeight)
	takeoverCtrl := newTakeoverController(browserCtx)

	impl := &browserCoreImpl{
		allocCtx:          allocCtx,
		allocCancel:       allocCancel,
		browserCtx:        browserCtx,
		browserCancel:     browserCancel,
		snapEngine:        snapEngine,
		actEngine:         actEngine,
		liveEngine:        liveEngine,
		takeoverCtrl:      takeoverCtrl,
		fingerprintPreset: presetID,
		persona:           sessionPersona,
		runtimeMode:       runtimeMode,
		chromePath:        chromePath,
		profileID:         "session",
		launcher:          NewChromeLauncher(),
		supervisor:        NewChromeSupervisor(),
		chromePID:         0,
		liveViewportW:     DefaultViewportWidth,
		liveViewportH:     DefaultViewportHeight,
		liveViewportDPR:   1,
		sessionAttached:   true,
	}
	if sessionPreset != nil {
		impl.liveViewportW = sessionPreset.ViewportW
		impl.liveViewportH = sessionPreset.ViewportH
		impl.liveViewportDPR = sessionPreset.DeviceScaleFactor
		impl.liveViewportMobile = sessionPreset.Mobile
		impl.liveViewportTouch = sessionPreset.Touch
		impl.liveViewportMaxTP = int64(sessionPreset.MaxTouchPoints)
	}

	return impl, nil
}
