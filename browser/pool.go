// Package browser — BrowserPool: identity-keyed Chrome 实例池 [v2 Phase_v2_2]
//
// 设计依据:
//   - r1/r2: TH-0405-k8r (8 轮碰撞, 14 DDC, 10 要点冻结) — 单 Chrome 多 Tab
//   - v2:    TH-0419-w3p MERGED — identity-keyed 多 Chrome 多 Tab
//
// 核心架构 (v2 Phase_v2_2):
//   - 每个 IdentityKey 对应独立 chromePoolEntry (含独立 allocCtx + profileDir + chromePath)
//   - 每个 entry 内多 Tab (chromedp NewContext per consumer)
//   - lazy init: 首次该 IdentityKey 的 AcquireTab 时启动对应 Chrome
//   - graceful shutdown: 通过 GracefulShutdown 接口暴露 (Phase_v2_2 内部仍 SIGTERM-only,
//     双阶段 bounded wait + SIGKILL fallback 由 Phase_v2_4 实装 — 接口已 final)
//   - 孤儿清理: 启动时删除旧 SingletonLock
//   - maxTabs 保护: 防 OOM (全局上限, 跨 entry 计数)
//   - 跨平台 dataDir: {dataDir}/browser-data/profiles/{profile}/{preset-vN}/
//
// Caller 复用语义 (v2):
//   - 旧 r1/r2 LegacyAcquireTab(purpose) 已删除. 上层调用方 (webui-panel/tool-default 等)
//     自己缓存 TabHandle 实现"同 caller 同 Tab"语义 (见 webui/browser_routes.go 的
//     panelHandle, internal/tool/browser_tools.go 的 toolTabHandle).
//   - SwitchProfile/UpdateViewport — 对 default identity entry 操作
//     (UpdateViewport 通过 handle.WorkspaceID == "webui-panel" 反查 panel Tab).
//
// CAP 锚点: CAP-BS09-C4 §2.bis (BrowserPool interface) + §3.5/§3.6 + T5 §0.1, §0.3
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// BrowserMode 定义浏览器运行模式 [TH-0414-b3m]。
type BrowserMode string

const (
	// ModeHeadless 极速模式：Chrome --headless=new，零 display 依赖。
	ModeHeadless BrowserMode = "headless"

	// ModeHeaded 标准模式：真实 headed Chrome + 平台虚拟显示器，对 Human 不可见。
	ModeHeaded BrowserMode = "headed"

	// ModeVisible 可见模式：真实 headed Chrome 显示到用户可见工作区。
	ModeVisible BrowserMode = "visible"

	// Deprecated compatibility aliases.
	BrowserModeHeadless BrowserMode = ModeHeadless
	BrowserModeHuman    BrowserMode = "human"
)

const (
	browserPoolProxyEnv        = "DW_BROWSER_PROXY"
	browserPoolInheritProxyEnv = "DW_BROWSER_INHERIT_PROXY"
)

// PoolConfig 配置 BrowserPool 默认 (legacy default identity 用).
type PoolConfig struct {
	DataDir     string        // deepwork 数据目录 (e.g. ~/.deepwork)
	MaxTabs     int           // Tab 上限 (default 10) — 全局, 跨所有 identity entry
	IdleTimeout time.Duration // Tab 空闲回收时间 (default 5min)
	PresetID    string        // 默认 preset (legacy 入口生效)
	PersonaID   string        // 默认 persona (可选;与 PresetID 同源 rails)
	ProfileID   string        // 默认 profile (legacy 入口生效)
	Mode        BrowserMode   // "headed"(默认) / "headless" / "visible"
}

func (c *PoolConfig) isHuman() bool {
	return NormalizeBrowserMode(c.Mode, ModeHeaded) != ModeHeadless
}

func NormalizeBrowserMode(mode BrowserMode, fallback BrowserMode) BrowserMode {
	switch BrowserMode(strings.ToLower(strings.TrimSpace(string(mode)))) {
	case ModeHeadless:
		return ModeHeadless
	case ModeHeaded:
		return ModeHeaded
	case ModeVisible, BrowserModeHuman:
		return ModeVisible
	case "":
		if fallback != "" {
			return fallback
		}
		return ModeHeaded
	default:
		if fallback != "" {
			return fallback
		}
		return ModeHeaded
	}
}

// ============================================================
// § v2 errors [Ref: CAP §2.bis]
// ============================================================

// ErrPoolClosed — Pool 已 Shutdown.
var ErrPoolClosed = errors.New("browser pool: closed")

// ErrInvalidRole — AcquireTabRequest.Role 不在 4 枚举矩阵 (TC-09-U-41).
var ErrInvalidRole = errors.New("browser pool: invalid role (allowed: human/agent/council/background)")

// ErrInvalidRoleIdentity — Role=council 但 IdentityKey 对应 Profile 非 council-* 前缀 (TC-09-U-42, P1).
var ErrInvalidRoleIdentity = errors.New("browser pool: council role requires council-* profile prefix")

// ErrIdentityUnresolved — AcquireTabRequest.IdentityKey 未在 Registry 注册过.
var ErrIdentityUnresolved = errors.New("browser pool: identity not resolved (call IdentityRegistry.Resolve first)")

// ErrMaxTabsReached — Tab 总数达上限且无可回收 entry.
var ErrMaxTabsReached = errors.New("browser pool: max tabs reached")

// ============================================================
// § v2 AcquireTabRequest [Ref: CAP §2.bis lines 119-125]
// ============================================================

// AcquireTabRequest — BrowserPool.AcquireTab 入参 (CAP §2.bis lines 119-125).
type AcquireTabRequest struct {
	IdentityKey IdentityKey // 来自 IdentityRegistry.Resolve, 必填; 不允许字符串拼装
	WorkspaceID string      // 任务隔离维度 (T5 §0.3, 不影响 Chrome 资源边界)
	Role        TabRole     // 4 枚举之一; 不在矩阵 → ErrInvalidRole
	InitialURL  string      // 可选, 创建 Tab 时直接 navigate (Phase_v2_2 暂不实装 navigate)
	Ephemeral   bool        // true → 复用 ephemeral profile (dw-browser --ephemeral)
	Mode        BrowserMode // 可选; 空值使用 PoolConfig.Mode. BS-15 Fast runtime 用 headless.

	// BrowserSession contract passthrough. These fields let service callers
	// preserve their owner contract when BrowserPool coordinates a headed
	// BrowserMuxHost. Empty values keep the legacy interactive pool defaults.
	BrowserSessionID string
	SessionKind      BrowserSessionKind
	Goal             string
	Owner            string
	Isolation        string
	ServiceName      string
	AccountID        string
}

type browserPoolRuntimeContract struct {
	BrowserSessionID string
	SessionKind      BrowserSessionKind
	Goal             string
	Owner            string
	Isolation        string
	ServiceName      string
	AccountID        string
}

func browserPoolRuntimeContractFromAcquireTab(req AcquireTabRequest, desc IdentityDescriptor) browserPoolRuntimeContract {
	kind := NormalizeBrowserSessionKind(string(req.SessionKind), SessionKindInteractive)
	defaults := DefaultsForBrowserSessionKind(kind)
	browserSessionID := strings.TrimSpace(req.BrowserSessionID)
	if browserSessionID == "" {
		browserSessionID = BrowserSessionIDFromPoolIdentity(desc.Key)
	}
	owner := strings.TrimSpace(req.Owner)
	if owner == "" {
		owner = defaults.Owner
	}
	isolation := strings.TrimSpace(req.Isolation)
	if isolation == "" {
		isolation = defaults.Isolation
	}
	return browserPoolRuntimeContract{
		BrowserSessionID: BrowserSessionIDFromSessionID(browserSessionID),
		SessionKind:      kind,
		Goal:             strings.TrimSpace(req.Goal),
		Owner:            owner,
		Isolation:        isolation,
		ServiceName:      strings.TrimSpace(req.ServiceName),
		AccountID:        strings.TrimSpace(req.AccountID),
	}
}

func validateBrowserPoolRuntimeContract(existing, requested browserPoolRuntimeContract) error {
	if existing.BrowserSessionID != requested.BrowserSessionID {
		return fmt.Errorf("browser pool: runtime contract mismatch: browser_session_id existing=%s requested=%s", existing.BrowserSessionID, requested.BrowserSessionID)
	}
	if existing.SessionKind != requested.SessionKind {
		return fmt.Errorf("browser pool: runtime contract mismatch: session_kind existing=%s requested=%s", existing.SessionKind, requested.SessionKind)
	}
	if existing.Owner != requested.Owner {
		return fmt.Errorf("browser pool: runtime contract mismatch: owner existing=%s requested=%s", existing.Owner, requested.Owner)
	}
	if existing.Isolation != requested.Isolation {
		return fmt.Errorf("browser pool: runtime contract mismatch: isolation existing=%s requested=%s", existing.Isolation, requested.Isolation)
	}
	if existing.ServiceName != requested.ServiceName {
		return fmt.Errorf("browser pool: runtime contract mismatch: service existing=%s requested=%s", existing.ServiceName, requested.ServiceName)
	}
	if existing.AccountID != requested.AccountID {
		return fmt.Errorf("browser pool: runtime contract mismatch: account_id existing=%s requested=%s", existing.AccountID, requested.AccountID)
	}
	return nil
}

func browserMuxHostRequestForPoolEntry(entry *chromePoolEntry, chromePath string, preset *FingerprintPreset, cdpPort int) BrowserMuxHostRequest {
	contract := entry.runtimeContract
	if contract.BrowserSessionID == "" {
		contract = browserPoolRuntimeContractFromAcquireTab(AcquireTabRequest{
			IdentityKey: entry.identity.Key,
			Mode:        entry.mode,
		}, entry.identity)
	}
	width := DefaultViewportWidth
	height := DefaultViewportHeight
	touch := false
	if preset != nil {
		if preset.ViewportW > 0 {
			width = preset.ViewportW
		}
		if preset.ViewportH > 0 {
			height = preset.ViewportH
		}
		touch = preset.Touch
	}
	return BrowserMuxHostRequest{
		BrowserSessionID: contract.BrowserSessionID,
		SessionKind:      contract.SessionKind,
		MuxHostID:        BrowserMuxHostIDFromBrowserSessionID(contract.BrowserSessionID),
		RuntimeID:        BrowserRuntimeIDFromBrowserSessionID(contract.BrowserSessionID),
		IdentityKey:      entry.identity.Key,
		OwnerPID:         os.Getpid(),
		Goal:             contract.Goal,
		Owner:            contract.Owner,
		Isolation:        contract.Isolation,
		ServiceName:      contract.ServiceName,
		AccountID:        contract.AccountID,
		ChromePath:       chromePath,
		ProfileID:        entry.profileID,
		ProfileDir:       entry.profileDir,
		DebugPort:        cdpPort,
		Mode:             ModeHeaded,
		PresetID:         entry.presetID,
		Width:            width,
		Height:           height,
		Touch:            touch,
	}
}

// ============================================================
// § Pool 内部数据结构
// ============================================================

// chromePoolEntry — 单个 IdentityKey 对应的 Chrome 实例 + 所属 Tab 集合.
//
// 不变式:
//   - 同 IdentityKey 在 BrowserPool.entries 中只有一个 entry (TC-09-U-40 守护).
//   - allocCtx / allocCancel 在 lazy launch 时填充, 在 entry 销毁时 cancel.
//   - tabs map[targetID]*tabEntry 维护该 Chrome 内所有 Tab.
type chromePoolEntry struct {
	identity      IdentityDescriptor
	allocCtx      context.Context
	allocCancel   context.CancelFunc
	browserCtx    context.Context    // chromedp Browser owner ctx (持有 Chrome 进程; 派生所有 tab)
	browserCancel context.CancelFunc // cancel browserCtx → 关闭 Chrome 进程 (chromedp 语义)
	chromePath    string
	profileDir    string
	presetID      string // resolved preset ID for this identity (default = config.PresetID)
	personaID     string // resolved persona ID (可选;与 presetID 同源,空=纯指纹)
	profileID     string // resolved profile ID (default = config.ProfileID)
	mode          BrowserMode
	started       bool
	tabs          map[string]*tabEntry // targetID → entry
	rootTargetID  string               // Chrome 启动时的首个 target（可被长驻 panel 复用）
	rootClaimed   bool                 // root target 是否已被某个 caller 占用为 managed tab

	// chromeHandle: human-mode Chrome 进程句柄 (workspace 派生).
	// nil → headless 模式 (chromedp.ExecAllocator 自管 Chrome 生命周期).
	// non-nil → headed/visible 模式 (RemoteAllocator + 显式 Kill).
	// [DDC-I-21: visible Chrome 必须经 Workspace SSOT 走 D1 序列以绑定到目标 Space]
	chromeHandle    ChromeHandle
	virtualDisplay  *VirtualDisplayManager
	browserMuxHost  *BrowserMuxHostState
	runtimeContract browserPoolRuntimeContract
}

type tabEntry struct {
	targetID   string
	core       BrowserCore
	tabCtx     context.Context
	tabCancel  context.CancelFunc
	handle     *TabHandle // v2: identity/ws/role 元数据
	lastActive time.Time
}

// BrowserPool — identity-keyed Chrome 实例池 (实现 CAP §2.bis BrowserPool interface).
type BrowserPool struct {
	config PoolConfig

	mu sync.Mutex

	// v2: identity-keyed entries (each entry = 1 Chrome)
	entries map[IdentityKey]*chromePoolEntry

	// v2: TargetID → handle (用于 ReleaseTab(targetID) / Inspect)
	tabIndex TabIndex

	// v2: identity registry (默认 identity 解析 + 校验)
	registry IdentityRegistry

	// 默认 IdentityKey (Switch*/UpdateViewport + 上层 DefaultIdentity() 使用)
	defaultIdentityKey IdentityKey

	closed     bool
	displayMgr *DisplayManager // 虚拟 display 管理 (跨平台, 进程级单例)
	reaperStop chan struct{}

	// workspace: visible Chrome 启动 SSOT (DDC-I-21, BRR-12).
	// 仅 visible 模式经此创建 Chrome — 保证 Chrome 窗口绑定到隔离 Space (macOS) /
	// Xvfb display (Linux). headless/headed 模式不触碰 workspace.
	// [Iron Rule: Linux Xvfb 路径 (display_manager.go::ensureDisplayLinux) 不变]
	workspace Workspace

	// virtualDisplay: macOS headed 模式 CGVirtualDisplay 管理。
	virtualDisplay *VirtualDisplayManager
}

// NewBrowserPool 创建 BrowserPool (不启动 Chrome — lazy init).
func NewBrowserPool(config PoolConfig) *BrowserPool {
	if config.MaxTabs <= 0 {
		config.MaxTabs = DefaultMaxTabs
	}
	if config.IdleTimeout <= 0 {
		config.IdleTimeout = BrowserPoolDefaultIdleTimeout
	}
	config.PresetID = NormalizePresetID(config.PresetID)
	config.ProfileID = NormalizeProfileID(config.ProfileID)
	config.Mode = NormalizeBrowserMode(config.Mode, ModeHeaded)
	if err := RecoverBrowserRuntimeState(config.DataDir); err != nil {
		log.Printf("[BROWSER-RECOVERY] AUDIT: startup_recovery_sweep_failed data_dir=%s err=%v", config.DataDir, err)
	}
	p := &BrowserPool{
		config:         config,
		entries:        make(map[IdentityKey]*chromePoolEntry),
		tabIndex:       NewTabIndex(),
		registry:       NewIdentityRegistry(),
		displayMgr:     &DisplayManager{},
		reaperStop:     make(chan struct{}),
		workspace:      NewWorkspace(),
		virtualDisplay: &VirtualDisplayManager{},
	}
	// 预解析 default identity (DefaultIdentity / Switch* 操作走此 key)
	defKey, _ := p.registry.Resolve(config.ProfileID, defaultPresetForConfig(config), NoopPolicy{})
	p.defaultIdentityKey = defKey
	go p.idleReaper()
	return p
}

// defaultPresetForConfig — 把 PoolConfig 翻译为 v2 Preset (legacy 路径用).
//
// 设计: legacy purpose-keyed 调用方未提供 viewport/UA, 用 BuiltinPresets[PresetID] 默认值
// (与 r1/r2 行为对齐 — startChromeLocked 也是这样查的). FingerprintTag 使用 PresetID 本身,
// 保证 IdentityKey 与"PresetID 切换"语义对齐.
func defaultPresetForConfig(c PoolConfig) Preset {
	presetID := NormalizePresetID(c.PresetID)
	preset := BuiltinPresets[presetID]
	out := Preset{FingerprintTag: presetID}
	if preset != nil {
		out.UserAgent = preset.UserAgent
		out.Viewport = Viewport{Width: preset.ViewportW, Height: preset.ViewportH, DPR: preset.DeviceScaleFactor}
		// FingerprintPreset 无 Locale/Timezone 字段; 用 Languages 作为 Locale 近似 (与 Cloudflare 探测一致)
		out.Locale = preset.Languages
		// Timezone 留空: r1/r2 也未单独追踪; Phase_v2_4 IsolationPolicy 引入显式时区时再补
	}
	return out
}

func resolveBrowserPoolProxy() (proxy string, source string) {
	return resolveBrowserPoolProxyForGOOS(runtime.GOOS)
}

func resolveBrowserPoolProxyForGOOS(goos string) (proxy string, source string) {
	if proxy = strings.TrimSpace(os.Getenv(browserPoolProxyEnv)); proxy != "" {
		return proxy, browserPoolProxyEnv
	}
	if goos != "darwin" {
		for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
			if proxy = strings.TrimSpace(os.Getenv(name)); proxy != "" {
				return proxy, name
			}
		}
		return "", ""
	}
	if !envFlagEnabled(browserPoolInheritProxyEnv) {
		return "", ""
	}
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if proxy = strings.TrimSpace(os.Getenv(name)); proxy != "" {
			return proxy, name
		}
	}
	return "", ""
}

func envFlagEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// shutdownGraceEnv — env name for double-phase shutdown bounded wait (CAP §3.5).
const shutdownGraceEnv = "BS09_SHUTDOWN_GRACE_SEC"

// envShutdownGraceSec 返回双阶段退出 bounded wait (CAP §3.5: 默认 5s, 上限 30s).
//
// 解析规则:
//   - 未设 / 空 / 非数 / ≤0 → 默认 5s
//   - >30s → 截断 30s (CAP §3.5 上限)
func envShutdownGraceSec() time.Duration {
	raw := strings.TrimSpace(os.Getenv(shutdownGraceEnv))
	if raw == "" {
		return BrowserPoolShutdownGrace
	}
	var sec int
	if _, err := fmt.Sscanf(raw, "%d", &sec); err != nil || sec <= 0 {
		return BrowserPoolShutdownGrace
	}
	d := time.Duration(sec) * time.Second
	if d > BrowserPoolMaxShutdownGrace {
		return BrowserPoolMaxShutdownGrace
	}
	return d
}

// ============================================================
// § v2 BrowserPool API [Ref: CAP §2.bis]
// ============================================================

// AcquireTab 获取或创建一个 Tab (CAP §2.bis lines 99-102).
//
// 不变式:
//   - 同 IdentityKey 多次 AcquireTab 复用同一 Chrome 实例 (TC-09-U-40, TC-09-I-47).
//   - 不同 IdentityKey 的 AcquireTab 启动独立 Chrome 实例 (TC-09-U-40 反向断言).
//   - Role 不在 4 枚举 → ErrInvalidRole + 不启动 Chrome (TC-09-U-41).
//   - Role=council 但 IdentityKey 对应 Profile 非 council-* 前缀 → ErrInvalidRoleIdentity (TC-09-U-42, P1).
//   - lazy launch: IdentityKey 未见过 → 调 ChromeLauncher + ProfileManager + IsolationPolicy.Apply (TC-09-I-48).
//
// 校验顺序:
//  1. p.closed → ErrPoolClosed
//  2. !IsValidRole → ErrInvalidRole (无副作用)
//  3. registry.Inspect → ErrIdentityUnresolved (上层必先 Resolve)
//  4. council role + 非 council-* profile → ErrInvalidRoleIdentity
//  5. tab 上限检查 (跨 entry 总和)
//  6. lazy launch + Tab 创建
//  7. TabIndex.Register
//
// 返回值: TabHandle 副本 (调用方持有, 通过 TargetID 反向 ReleaseTab).
func (p *BrowserPool) AcquireTab(ctx context.Context, req AcquireTabRequest) (*TabHandle, error) {
	totalStartedAt := time.Now()
	// 入参校验 (无副作用部分先行)
	if !IsValidRole(req.Role) {
		return nil, ErrInvalidRole
	}
	if req.IdentityKey == "" {
		return nil, ErrIdentityUnresolved
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, ErrPoolClosed
	}

	// Identity 反查 (确保上层走过 Registry.Resolve)
	desc, err := p.registry.Inspect(req.IdentityKey)
	if err != nil {
		return nil, fmt.Errorf("%w: key=%s", ErrIdentityUnresolved, req.IdentityKey)
	}

	// council role + 非 council-* profile → 拒绝
	if req.Role == RoleCouncil && !strings.HasPrefix(desc.Profile, "council-") {
		return nil, fmt.Errorf("%w: profile=%s", ErrInvalidRoleIdentity, desc.Profile)
	}

	// Tab 上限检查 (全局, 跨 entry 总和)
	if p.tabIndex.Len() >= p.config.MaxTabs {
		if !p.evictOldestLocked() {
			return nil, ErrMaxTabsReached
		}
	}

	// 取/建 entry (lazy)
	runtimeContract := browserPoolRuntimeContractFromAcquireTab(req, *desc)
	entryStartedAt := time.Now()
	entry, err := p.getOrCreateEntryLocked(ctx, *desc, req.Mode, runtimeContract)
	if err != nil {
		return nil, fmt.Errorf("browser pool: ensure chrome for identity %s: %w", req.IdentityKey, err)
	}
	log.Printf("[BROWSER-POOL/v2] acquire_step step=ensure_entry identity=%s ws=%s role=%s elapsed_ms=%d",
		req.IdentityKey, req.WorkspaceID, req.Role, time.Since(entryStartedAt).Milliseconds())

	// 创建 Tab
	tabStartedAt := time.Now()
	tabCtx, tabCancel, core, targetID, terr := p.createTabLocked(ctx, entry, req.WorkspaceID)
	if terr != nil {
		return nil, terr
	}
	log.Printf("[BROWSER-POOL/v2] acquire_step step=create_tab identity=%s ws=%s role=%s targetID=%s elapsed_ms=%d",
		req.IdentityKey, req.WorkspaceID, req.Role, targetID, time.Since(tabStartedAt).Milliseconds())

	handle := &TabHandle{
		TargetID:    targetID,
		IdentityKey: req.IdentityKey,
		WorkspaceID: req.WorkspaceID,
		Role:        req.Role,
		AcquiredAt:  time.Now(),
	}
	if err := p.tabIndex.Register(handle); err != nil {
		// rollback Tab
		tabCancel()
		return nil, fmt.Errorf("browser pool: register tab in index: %w", err)
	}

	entry.tabs[targetID] = &tabEntry{
		targetID:   targetID,
		core:       core,
		tabCtx:     tabCtx,
		tabCancel:  tabCancel,
		handle:     handle,
		lastActive: time.Now(),
	}
	log.Printf("[BROWSER-POOL/v2] AcquireTab: identity=%s ws=%s role=%s targetID=%s total_tabs=%d total_elapsed_ms=%d",
		req.IdentityKey, req.WorkspaceID, req.Role, targetID, p.tabIndex.Len(), time.Since(totalStartedAt).Milliseconds())

	out := *handle
	return &out, nil
}

// ReleaseTab 释放指定 TargetID 的 Tab (CAP §2.bis lines 105-107).
//
// 不立即关闭 Chrome — 由 Pool 自身决定 idle eviction (Phase_v2_4 实装 idle policy).
func (p *BrowserPool) ReleaseTab(ctx context.Context, targetID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	handle, ok := p.tabIndex.Lookup(targetID)
	if !ok {
		return ErrTabNotFound
	}

	entry, ok := p.entries[handle.IdentityKey]
	if !ok {
		_ = p.tabIndex.Unregister(targetID)
		return fmt.Errorf("browser pool: orphan tab %s (no entry for identity %s)", targetID, handle.IdentityKey)
	}
	if te, ok := entry.tabs[targetID]; ok {
		p.closeTabEntryLocked(entry, te)
	}
	return nil
}

// Inspect 返回 Pool 当前状态快照 (CAP §2.bis lines 116-117).
//
// Identities 按 IdentityKey 字典序; 每 identity 内 Tabs 按 TargetID 字典序.
// ChromePID 当前由 Pool 反查 entry.allocCtx 推断 (Phase_v2_2 简化为 entry.started ? -1 : 0,
// 真实 PID 提取由 Phase_v2_4 双阶段退出实装时引入 process tracking).
func (p *BrowserPool) Inspect() PoolSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	rows := p.tabIndex.Snapshot()
	for i := range rows {
		if entry, ok := p.entries[rows[i].Key]; ok {
			rows[i].Mode = entry.mode
			if entry.started {
				rows[i].ChromePID = -1 // sentinel: started but PID extraction deferred to Phase_v2_4
			}
		}
	}
	return PoolSnapshot{
		Identities: rows,
		TotalTabs:  p.tabIndex.Len(),
	}
}

// GracefulShutdown 双阶段退出 (CAP §2.bis lines 109-113 + §3.5) [Phase_v2_4].
//
// 协议 (per entry):
//  1. cancel 所有 tab context (释放 chromedp 资源)
//  2. cancel browserCtx 异步触发 Chrome graceful close (Browser.close → flush cookie/storage)
//  3. bounded wait (default 5s, env BS09_SHUTDOWN_GRACE_SEC, max 30s):
//     - chromedp.Cancel 在 goroutine 中执行 graceful Browser.close + 等待清理
//     - 超时 → cancel allocCtx (chromedp 内部 SIGKILL fallback) + audit event
//  4. 清理 entry 状态
//
// audit event (CAP §3.5): wait 超时 → log "[BROWSER-POOL/v2] AUDIT: chrome_killed_after_grace"
// 包含 identity + grace_sec, 便于运维定位 stuck Chrome.
//
// 注: 单 mutex 持有期间为所有 entry 串行执行 bounded wait. N 个 identity → 最多 N*grace 串行.
// 实际生产 N 通常 ≤2 (default + council), 串行 10s 上限可接受.
func (p *BrowserPool) GracefulShutdown(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true
	close(p.reaperStop)

	grace := envShutdownGraceSec()

	for key, entry := range p.entries {
		// Step 1: cancel 所有 tab context (释放 chromedp tab 资源)
		for tid, te := range entry.tabs {
			te.tabCancel()
			delete(entry.tabs, tid)
			_ = p.tabIndex.Unregister(tid)
		}

		// BrowserMuxHost-owned headed Chrome must survive a Deepwork UI/process
		// restart. On pool shutdown we only detach and release the owner lease;
		// BrowserMuxHost exits after its idle TTL unless a new Deepwork process
		// touches it first. Explicit reset/close paths still call Kill().
		if entry.browserMuxHost != nil {
			if entry.browserCancel != nil {
				entry.browserCancel()
				entry.browserCancel = nil
			}
			if entry.allocCancel != nil {
				entry.allocCancel()
				entry.allocCancel = nil
			}
			releaseCtx, cancel := context.WithTimeout(context.Background(), BrowserPoolReleaseTimeout)
			if _, err := ReleaseBrowserMuxHost(releaseCtx, entry.browserMuxHost); err != nil {
				log.Printf("[BROWSER-POOL/v2] AUDIT: browser_mux_host_release_error identity=%s muxhost_id=%s err=%v", key, entry.browserMuxHost.MuxHostID, err)
			} else {
				log.Printf("[BROWSER-POOL/v2] AUDIT: browser_mux_host_released identity=%s muxhost_id=%s idle_ttl_ms=%d",
					key, entry.browserMuxHost.MuxHostID, entry.browserMuxHost.IdleTTLMillis)
			}
			cancel()
			entry.browserCtx = nil
			entry.chromeHandle = nil
			entry.browserMuxHost = nil
			entry.started = false
			continue
		}

		// Step 2-3: chromedp.Cancel 异步发送 Browser.close + 等待清理, 配合 bounded wait
		// 使用 chromedp.Cancel (而非裸 browserCancel()): 走 graceful Browser.close 路径,
		// 让 Chrome 主动 flush cookie/storage 后退出 (CAP §3.5 持久化 invariant, BRR-04).
		if entry.browserCtx != nil {
			done := make(chan struct{})
			browserCtx := entry.browserCtx
			go func() {
				defer close(done)
				_ = chromedp.Cancel(browserCtx)
			}()

			select {
			case <-done:
				log.Printf("[BROWSER-POOL/v2] GracefulShutdown: identity=%s exited gracefully within grace=%v", key, grace)
			case <-time.After(grace):
				log.Printf("[BROWSER-POOL/v2] AUDIT: chrome_killed_after_grace identity=%s grace_sec=%d", key, int(grace.Seconds()))
				if entry.allocCancel != nil {
					entry.allocCancel() // 强制 chromedp ExecAllocator → SIGKILL fallback
				}
			}

			entry.browserCtx = nil
			entry.browserCancel = nil
		}

		// 终态 cleanup: 释放 ExecAllocator (graceful 路径上 chromedp.Cancel 已传染取消, 此处 idempotent)
		if entry.allocCancel != nil {
			entry.allocCancel()
			entry.allocCancel = nil
		}

		// human 模式 Chrome 由 Workspace 启动 (RemoteAllocator 不持有进程):
		// 必须显式 Kill, 否则 chromedp.Cancel + allocCancel 都不会终止 Chrome 进程.
		// [DDC-I-21]: Workspace 是 Chrome 进程 SSOT, Pool 通过 ChromeHandle.Kill 释放.
		if entry.chromeHandle != nil {
			if err := entry.chromeHandle.Kill(); err != nil {
				log.Printf("[BROWSER-POOL/v2] AUDIT: chrome_kill_error identity=%s pid=%d err=%v",
					key, entry.chromeHandle.PID(), err)
			}
			RemoveProfileOwnerMarker(entry.profileDir, key)
			entry.chromeHandle = nil
			entry.browserMuxHost = nil
		}

		entry.started = false
	}
	p.entries = make(map[IdentityKey]*chromePoolEntry)

	// 关闭 Xvfb 进程 (进程级单例)
	if p.displayMgr != nil {
		_ = p.displayMgr.Close()
	}
	if p.virtualDisplay != nil {
		_ = p.virtualDisplay.Close()
	}
	return nil
}

// Shutdown 是 GracefulShutdown 的 r1/r2 兼容签名 (无返回值, 不返回 error).
//
// 现有 cmd/deepwork/main.go 走此入口; Phase_v2_3+ 调用方应改用 GracefulShutdown(ctx) error.
func (p *BrowserPool) Shutdown(ctx context.Context) {
	_ = p.GracefulShutdown(ctx)
}

// ============================================================
// § Identity Registry 暴露 (供 LegacyAcquireTab + 上层调用方使用)
// ============================================================

// Registry 返回 Pool 内置的 IdentityRegistry (供上层 Resolve 后传入 AcquireTab).
//
// 设计: Pool 内部和外部共享同一 registry, 避免上层用独立 registry 导致 IdentityKey 来源
// 不一致 (registry.Inspect 反查会失败).
func (p *BrowserPool) Registry() IdentityRegistry {
	return p.registry
}

// DefaultIdentity 返回 Pool 在 NewBrowserPool 时由 PoolConfig.Profile/Preset + NoopPolicy
// 预解析的默认 IdentityKey. 上层调用方 (webui-panel / tool-default) 没有特定 identity 需求时
// 用此默认值, 保证与 PoolConfig 配置语义一致.
func (p *BrowserPool) DefaultIdentity() IdentityKey {
	return p.defaultIdentityKey
}

// ResolveProfileIdentity resolves a profile against the pool's canonical
// default preset/policy. Callers that only need "this saved profile" must use
// this instead of Registry().Resolve(profile, Preset{FingerprintTag: ...}) so
// one physical profile dir maps to one BrowserPool entry.
func (p *BrowserPool) ResolveProfileIdentity(profileID string) (IdentityKey, error) {
	p.mu.Lock()
	cfg := p.config
	p.mu.Unlock()
	cfg.ProfileID = NormalizeProfileID(profileID)
	return p.registry.Resolve(cfg.ProfileID, defaultPresetForConfig(cfg), NoopPolicy{})
}

// GetCore 返回指定 TargetID 对应的 BrowserCore (AcquireTab 之后取操作句柄).
//
// 设计: AcquireTab 只返回 *TabHandle (CAP §2.bis 冻结签名); 上层执行浏览器动作需要
// BrowserCore. GetCore 把 TargetID → BrowserCore 反查暴露给调用方, 不引入新所有权.
//
// 返回 (nil, false) 当 TargetID 未在 TabIndex / entry.tabs 中找到 (Tab 已被 Release/Evict).
func (p *BrowserPool) GetCore(targetID string) (BrowserCore, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	handle, ok := p.tabIndex.Lookup(targetID)
	if !ok {
		return nil, false
	}
	entry, ok := p.entries[handle.IdentityKey]
	if !ok {
		return nil, false
	}
	te, ok := entry.tabs[targetID]
	if !ok {
		return nil, false
	}
	te.lastActive = time.Now()
	return te.core, true
}

// ============================================================
// § Internal: entry 管理 + Chrome 启动 (per identity)
// ============================================================

// getOrCreateEntryLocked 取或建一个 IdentityKey 对应的 chromePoolEntry (锁内调用).
//
// lazy: 该 IdentityKey 第一次见 → 创建 entry + 启动 Chrome (TC-09-I-48).
// 第二次起 → 复用 (TC-09-U-40 + TC-09-I-47).
func (p *BrowserPool) getOrCreateEntryLocked(ctx context.Context, desc IdentityDescriptor, requestedMode BrowserMode, runtimeContract browserPoolRuntimeContract) (*chromePoolEntry, error) {
	if existing, ok := p.entries[desc.Key]; ok {
		if requestedMode != "" && existing.mode != "" && existing.mode != requestedMode {
			return nil, fmt.Errorf("mode mismatch for active identity: existing=%s requested=%s", existing.mode, requestedMode)
		}
		if err := validateBrowserPoolRuntimeContract(existing.runtimeContract, runtimeContract); err != nil {
			return nil, err
		}
		return existing, nil
	}

	// 解析 entry 用的 preset/profile (来自 Identity 三元组, fallback 到 config 默认)
	presetID := desc.Preset.FingerprintTag
	if presetID == "" {
		presetID = NormalizePresetID(p.config.PresetID)
	} else {
		presetID = NormalizePresetID(presetID)
	}
	if _, err := ValidatePresetID(presetID); err != nil {
		return nil, err
	}
	profileID := NormalizeProfileID(desc.Profile)
	if profileID == "" {
		profileID = NormalizeProfileID(p.config.ProfileID)
	}

	entry := &chromePoolEntry{
		identity:        desc,
		presetID:        presetID,
		personaID:       p.config.PersonaID, // persona 与 preset 同源(pool 默认;空=纯指纹)
		profileID:       profileID,
		mode:            requestedMode,
		tabs:            make(map[string]*tabEntry),
		runtimeContract: runtimeContract,
	}
	entry.mode = NormalizeBrowserMode(entry.mode, p.config.Mode)
	if err := p.startChromeForEntryLocked(ctx, entry); err != nil {
		return nil, err
	}
	p.entries[desc.Key] = entry
	return entry, nil
}

// startChromeForEntryLocked 启动一个 entry 对应的 Chrome (锁内调用).
//
// 复用 r1/r2 启动逻辑 (BuildDetachedChromeArgs + ExecAllocator + 预热), 但参数化:
//   - profileDir: 来自 entry.profileID + entry.presetID (resolveBrowserPoolDir)
//   - presetID: entry.presetID
//   - mode: entry.mode (与 config 默认一致)
//
// IsolationPolicy.Apply 在 Chrome 启动后通过 BrowserSessionHandle 落地 (Phase_v2_2: NoopPolicy 直接 nil).
func (p *BrowserPool) startChromeForEntryLocked(ctx context.Context, entry *chromePoolEntry) error {
	totalStartedAt := time.Now()
	entry.mode = NormalizeBrowserMode(entry.mode, p.config.Mode)

	launcher := NewChromeLauncher()
	findStartedAt := time.Now()
	chromePath, err := launcher.FindChrome()
	if err != nil {
		return err
	}
	log.Printf("[BROWSER-POOL/v2] startChrome_step step=find_chrome identity=%s path=%q elapsed_ms=%d",
		entry.identity.Key, chromePath, time.Since(findStartedAt).Milliseconds())
	entry.chromePath = chromePath

	// 解析 preset (与 startChromeLocked 对齐)
	presetStartedAt := time.Now()
	preset, ok := BuiltinPresets[entry.presetID]
	if !ok {
		return fmt.Errorf("browser pool: unknown preset_id %q", entry.presetID)
	}
	if resolved := ResolveRuntimeFingerprintPreset(entry.presetID, chromePath); resolved != nil {
		preset = resolved
	}
	log.Printf("[BROWSER-POOL/v2] startChrome_step step=resolve_preset identity=%s preset=%s elapsed_ms=%d",
		entry.identity.Key, entry.presetID, time.Since(presetStartedAt).Milliseconds())

	entry.profileDir = resolveBrowserPoolDir(p.config.DataDir, chromePath, entry.profileID, entry.presetID)

	// Headed runtime is BrowserMuxHost-owned. Recovery/prepare must happen inside
	// BrowserMuxHost, otherwise a restarted Deepwork client would reject a live
	// BrowserMuxHost marker before it has a chance to attach.
	if entry.mode != ModeHeaded {
		// Phase_v2_4 启动期 recovery (CAP §3.5):
		//   1. Singleton lock 残留检测 (orphan PID 强杀)
		//   2. profile health check (Cookies SQLite header)
		//   3. 损坏 → .broken/{ts}/ 隔离 (Chrome 自动重建空 profile)
		// 注: recovery 在 mkdir 之前 (隔离场景需 rename 原 profileDir, 不能预先 mkdir).
		if err := RunStartupRecovery(entry.profileDir, entry.identity.Key); err != nil {
			return fmt.Errorf("startup recovery: %w", err)
		}

		if err := os.MkdirAll(entry.profileDir, 0755); err != nil {
			return fmt.Errorf("create profile dir: %w", err)
		}
		if err := PrepareProfileForControlledLaunch(entry.profileDir); err != nil {
			log.Printf("[BROWSER-POOL/v2] profile launch hygiene failed identity=%s err=%v", entry.identity.Key, err)
		}
	}

	mode := entry.mode
	log.Printf("[BROWSER-POOL/v2] startChrome: identity=%s mode=%s profile_dir=%s preset=%s",
		entry.identity.Key, mode, entry.profileDir, entry.presetID)

	var launchErr error
	switch mode {
	case ModeHeadless:
		launchErr = p.startChromeHeadlessLocked(ctx, entry, chromePath, preset)
	case ModeHeaded:
		launchErr = p.startChromeHeadedLocked(ctx, entry, chromePath, preset)
	case ModeVisible:
		launchErr = p.startChromeVisibleLocked(ctx, entry, chromePath, preset)
	default:
		launchErr = fmt.Errorf("unsupported browser mode %q", mode)
	}
	if launchErr != nil {
		return launchErr
	}

	// IsolationPolicy.Apply 落地 (Phase_v2_2: NoopPolicy 直接 nil; ProxyPolicy P1)
	if entry.identity.Policy != nil {
		if err := entry.identity.Policy.Apply(ctx, BrowserSessionHandle(nil)); err != nil {
			log.Printf("[BROWSER-POOL/v2] IsolationPolicy.Apply failed for identity=%s: %v", entry.identity.Key, err)
		}
	}

	entry.started = true
	log.Printf("[BROWSER-POOL/v2] startChrome: identity=%s preset=%s viewport=%dx%d profile_dir=%s total_elapsed_ms=%d",
		entry.identity.Key, entry.presetID, preset.ViewportW, preset.ViewportH, entry.profileDir, time.Since(totalStartedAt).Milliseconds())
	return nil
}

// startChromeVisibleLocked — visible 模式: 经 Workspace SSOT 启动 visible Chrome (D1 序列).
//
// 流程:
//  1. 预分配 CDP port (Workspace 需要 port 才能发起 D1 序列)
//  2. 构造 launch args (含 proxy 注入)
//  3. Workspace.LaunchChromeInSpace → ChromeHandle (绑定到目标 Space)
//  4. chromedp.NewRemoteAllocator → 通过 WSURL 接管已启动的 Chrome
//  5. NewContext + warmup
//  6. 启动 crash watcher goroutine (Done() 触发 → log AUDIT)
//
// 失败时确保 chromeHandle 被 Kill (workspace.LaunchChromeInSpace 失败已自清理, warmup 失败需要补救).
func (p *BrowserPool) startChromeVisibleLocked(ctx context.Context, entry *chromePoolEntry, chromePath string, preset *FingerprintPreset) error {
	cdpPort, err := findAvailableCDPPort()
	if err != nil {
		return fmt.Errorf("allocate CDP port: %w", err)
	}

	launchArgs := BuildDetachedChromeArgs(DetachedChromeLaunchOptions{
		DebugPort:  cdpPort,
		ProfileDir: entry.profileDir,
		Width:      preset.ViewportW,
		Height:     preset.ViewportH,
		PresetID:   entry.presetID,
		Touch:      preset.Touch,
		Mode:       ModeVisible,
	})
	if proxy, source := resolveBrowserPoolProxy(); proxy != "" {
		launchArgs = append(launchArgs[:len(launchArgs)-1], "--proxy-server="+proxy, launchArgs[len(launchArgs)-1])
		log.Printf("[BROWSER-POOL/v2] human mode: proxy-server=%s (from %s)", proxy, source)
	}

	handle, err := p.workspace.LaunchChromeInSpace(ChromeLaunchSpec{
		ChromePath:   chromePath,
		Args:         launchArgs,
		DebugPort:    cdpPort,
		ReadyTimeout: BrowserMuxHostLaunchReadyTimeout,
	})
	if err != nil {
		return fmt.Errorf("workspace launch chrome: %w", err)
	}
	entry.chromeHandle = handle
	log.Printf("[BROWSER-POOL/v2] AUDIT: chrome_launched_via_workspace identity=%s pid=%d cdp_port=%d ws=%s",
		entry.identity.Key, handle.PID(), cdpPort, handle.WSURL())
	if err := WriteProfileOwnerMarker(entry.profileDir, entry.identity.Key, handle.PID(), cdpPort); err != nil {
		_ = handle.Kill()
		entry.chromeHandle = nil
		return fmt.Errorf("write profile owner marker: %w", err)
	}

	// chromedp 通过 RemoteAllocator 接管 Workspace 已启动的 Chrome (而非自己 fork).
	entry.allocCtx, entry.allocCancel = chromedp.NewRemoteAllocator(context.Background(), handle.WSURL())
	entry.browserCtx, entry.browserCancel = chromedp.NewContext(entry.allocCtx, chromedp.WithErrorf(chromedpErrorf))
	if err := runCDPWithSoftTimeout(entry.browserCtx, BrowserPoolChromeWarmup); err != nil {
		entry.browserCancel()
		entry.browserCtx = nil
		entry.browserCancel = nil
		entry.allocCancel()
		entry.allocCancel = nil
		_ = handle.Kill()
		RemoveProfileOwnerMarker(entry.profileDir, entry.identity.Key)
		entry.chromeHandle = nil
		return fmt.Errorf("chrome warmup (remote): %w", err)
	}
	entry.rootTargetID = captureTargetID(entry.browserCtx)
	if err := closeBootstrapBlankTargets(entry.browserCtx, entry.rootTargetID); err != nil {
		log.Printf("[BROWSER-POOL/v2] bootstrap target cleanup failed identity=%s err=%v", entry.identity.Key, err)
	}

	// Crash watcher: Chrome 异常退出 → 记录 audit (后续重启策略由 Pool 上层决定).
	identityKey := entry.identity.Key
	pid := handle.PID()
	go func() {
		<-handle.Done()
		log.Printf("[BROWSER-POOL/v2] AUDIT: chrome_exited identity=%s pid=%d", identityKey, pid)
	}()
	return nil
}

// startChromeHeadedLocked — headed 模式: 真实 headed Chrome + 对 Human 不可见的显示策略。
//
// Linux 使用 Xvfb；macOS 使用 CGVirtualDisplay。macOS headed 的主路径依赖
// Chrome 启动参数定位窗口，并用 CGWindowList 做 containment 校验；只有校验
// 失败时才走 CDP SetWindowBounds 修复路径。失败时直接返回错误，不静默降级。
func (p *BrowserPool) startChromeHeadedLocked(ctx context.Context, entry *chromePoolEntry, chromePath string, preset *FingerprintPreset) error {
	totalStartedAt := time.Now()
	portStartedAt := time.Now()
	cdpPort, err := findAvailableCDPPort()
	if err != nil {
		return fmt.Errorf("allocate CDP port: %w", err)
	}
	log.Printf("[BROWSER-POOL/v2] headed_start_step step=allocate_cdp_port identity=%s debug_port=%d elapsed_ms=%d",
		entry.identity.Key, cdpPort, time.Since(portStartedAt).Milliseconds())

	muxStartedAt := time.Now()
	hostState, err := EnsureBrowserMuxHost(ctx, browserMuxHostRequestForPoolEntry(entry, chromePath, preset, cdpPort))
	if err != nil {
		return fmt.Errorf("headed BrowserMuxHost ensure: %w", err)
	}
	log.Printf("[BROWSER-POOL/v2] headed_start_step step=ensure_muxhost identity=%s muxhost_id=%s muxhost_pid=%d chrome_pid=%d reused=%t elapsed_ms=%d",
		entry.identity.Key, hostState.MuxHostID, hostState.MuxHostPID, hostState.ChromePID, hostState.ReusedExisting, time.Since(muxStartedAt).Milliseconds())
	handle := NewBrowserMuxHostChromeHandle(hostState)
	entry.chromeHandle = handle
	entry.browserMuxHost = hostState
	log.Printf("[BROWSER-POOL/v2] AUDIT: chrome_attached_headed_muxhost identity=%s muxhost_id=%s muxhost_pid=%d chrome_pid=%d cdp_port=%d display=%s ws=%s",
		entry.identity.Key, hostState.MuxHostID, hostState.MuxHostPID, handle.PID(), hostState.DebugPort, hostState.DisplayBackend, handle.WSURL())

	allocStartedAt := time.Now()
	entry.allocCtx, entry.allocCancel = chromedp.NewRemoteAllocator(context.Background(), handle.WSURL())
	log.Printf("[BROWSER-POOL/v2] headed_start_step step=new_remote_allocator identity=%s elapsed_ms=%d",
		entry.identity.Key, time.Since(allocStartedAt).Milliseconds())
	ctxOpts := []chromedp.ContextOption{chromedp.WithErrorf(chromedpErrorf)}
	selectStartedAt := time.Now()
	if attachTargetID, selectErr := selectReusablePageTarget(handle.WSURL()); selectErr == nil && attachTargetID != "" {
		ctxOpts = append(ctxOpts, chromedp.WithTargetID(target.ID(attachTargetID)))
		log.Printf("[BROWSER-POOL/v2] headed_start_step step=select_attach_target identity=%s muxhost_id=%s target_id=%s reused=%t elapsed_ms=%d",
			entry.identity.Key, hostState.MuxHostID, attachTargetID, hostState.ReusedExisting, time.Since(selectStartedAt).Milliseconds())
	} else if selectErr != nil {
		log.Printf("[BROWSER-POOL/v2] headed_start_step step=select_attach_target identity=%s muxhost_id=%s elapsed_ms=%d err=%v",
			entry.identity.Key, hostState.MuxHostID, time.Since(selectStartedAt).Milliseconds(), selectErr)
	}
	contextStartedAt := time.Now()
	entry.browserCtx, entry.browserCancel = chromedp.NewContext(entry.allocCtx, ctxOpts...)
	log.Printf("[BROWSER-POOL/v2] headed_start_step step=new_context identity=%s elapsed_ms=%d",
		entry.identity.Key, time.Since(contextStartedAt).Milliseconds())
	warmupStartedAt := time.Now()
	if err := runCDPWithSoftTimeout(entry.browserCtx, BrowserPoolChromeWarmup); err != nil {
		entry.browserCancel()
		entry.browserCtx = nil
		entry.browserCancel = nil
		entry.allocCancel()
		entry.allocCancel = nil
		_ = handle.Kill()
		entry.chromeHandle = nil
		entry.browserMuxHost = nil
		return fmt.Errorf("chrome warmup (headed remote): %w", err)
	}
	log.Printf("[BROWSER-POOL/v2] headed_start_step step=warmup_remote identity=%s elapsed_ms=%d",
		entry.identity.Key, time.Since(warmupStartedAt).Milliseconds())
	entry.rootTargetID = captureTargetID(entry.browserCtx)

	if !hostState.ReusedExisting {
		cleanupStartedAt := time.Now()
		if err := closeBootstrapBlankTargets(entry.browserCtx, entry.rootTargetID); err != nil {
			log.Printf("[BROWSER-POOL/v2] headed bootstrap target cleanup failed identity=%s err=%v", entry.identity.Key, err)
		}
		log.Printf("[BROWSER-POOL/v2] headed_start_step step=cleanup_bootstrap_targets identity=%s root_target=%s elapsed_ms=%d",
			entry.identity.Key, entry.rootTargetID, time.Since(cleanupStartedAt).Milliseconds())
	} else {
		log.Printf("[BROWSER-POOL/v2] AUDIT: headed_muxhost_preserved_existing_tabs identity=%s muxhost_id=%s root_target=%s",
			entry.identity.Key, hostState.MuxHostID, entry.rootTargetID)
	}

	identityKey := entry.identity.Key
	pid := handle.PID()
	go func() {
		<-handle.Done()
		log.Printf("[BROWSER-POOL/v2] AUDIT: chrome_exited identity=%s pid=%d mode=headed", identityKey, pid)
	}()
	log.Printf("[BROWSER-POOL/v2] headed_start_completed identity=%s chrome_pid=%d total_elapsed_ms=%d",
		entry.identity.Key, pid, time.Since(totalStartedAt).Milliseconds())
	return nil
}

func (p *BrowserPool) enforceVirtualDisplayWindow(ctx context.Context, preset *FingerprintPreset, posX, posY int) error {
	if runtime.GOOS != "darwin" || ctx == nil {
		return nil
	}
	width, height := int64(1280), int64(800)
	if preset != nil {
		width = int64(preset.ViewportW)
		height = int64(preset.ViewportH)
	}
	return runCDPWithSoftTimeout(ctx, BrowserPoolCDPActionTimeout, chromedp.ActionFunc(func(ctx context.Context) error {
		windowID, _, err := cdpbrowser.GetWindowForTarget().Do(ctx)
		if err != nil {
			return fmt.Errorf("get chrome window for target: %w", err)
		}
		bounds := &cdpbrowser.Bounds{
			Left:        int64(posX),
			Top:         int64(posY),
			Width:       width,
			Height:      height,
			WindowState: cdpbrowser.WindowStateNormal,
		}
		if err := cdpbrowser.SetWindowBounds(windowID, bounds).Do(ctx); err != nil {
			return fmt.Errorf("set chrome window bounds to virtual display: %w", err)
		}
		return nil
	}))
}

func cleanupEntryLaunch(entry *chromePoolEntry) {
	if entry == nil {
		return
	}
	if entry.browserCancel != nil {
		entry.browserCancel()
		entry.browserCancel = nil
	}
	if entry.allocCancel != nil {
		entry.allocCancel()
		entry.allocCancel = nil
	}
	if entry.chromeHandle != nil {
		_ = entry.chromeHandle.Kill()
		RemoveProfileOwnerMarker(entry.profileDir, entry.identity.Key)
		entry.chromeHandle = nil
		entry.browserMuxHost = nil
	}
	entry.allocCtx = nil
	entry.browserCtx = nil
	entry.rootTargetID = ""
	entry.rootClaimed = false
	entry.started = false
}

// startChromeHeadlessLocked — headless 模式: chromedp.ExecAllocator 自管 Chrome 进程.
//
// 不经 Workspace — headless 无窗口, 不需要 Space 绑定. 进程隔离由 chromedp ExecAllocator
// 接管 (cancel allocCtx → SIGTERM Chrome).
func (p *BrowserPool) startChromeHeadlessLocked(ctx context.Context, entry *chromePoolEntry, chromePath string, preset *FingerprintPreset) error {
	launchArgs := BuildDetachedChromeArgs(DetachedChromeLaunchOptions{
		DebugPort:  0,
		ProfileDir: entry.profileDir,
		Width:      preset.ViewportW,
		Height:     preset.ViewportH,
		PresetID:   entry.presetID,
		Touch:      preset.Touch,
		Mode:       BrowserModeHeadless,
	})
	opts := ExecAllocatorOptionsFromArgs(chromePath, launchArgs)

	entry.allocCtx, entry.allocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
	entry.browserCtx, entry.browserCancel = chromedp.NewContext(entry.allocCtx, chromedp.WithErrorf(chromedpErrorf))
	if err := runCDPWithSoftTimeout(entry.browserCtx, BrowserPoolChromeWarmup); err != nil {
		entry.browserCancel()
		entry.browserCtx = nil
		entry.browserCancel = nil
		entry.allocCancel()
		entry.allocCancel = nil
		return fmt.Errorf("chrome warmup (exec): %w", err)
	}
	entry.rootTargetID = captureTargetID(entry.browserCtx)
	return nil
}

// createTabLocked 在 entry 上创建一个新 Tab (锁内调用).
//
// 返回: tabCtx + tabCancel + BrowserCore + 新 TargetID + error.
//
// 复用 r1/r2 createTab 逻辑 (chromedp.NewContext + 指纹注入 + stealth + emulation).
// purposeHint: 仅供日志 (legacy 路径有值, v2 路径为空).
func (p *BrowserPool) createTabLocked(_ context.Context, entry *chromePoolEntry, purposeHint string) (context.Context, context.CancelFunc, BrowserCore, string, error) {
	if entry.browserCtx == nil {
		return nil, nil, nil, "", fmt.Errorf("create tab (purpose=%q): entry browserCtx not initialized (chrome not warmed up)", purposeHint)
	}
	if !entry.rootClaimed && entry.rootTargetID != "" {
		entry.rootClaimed = true
		return p.materializeManagedTabLocked(entry, entry.browserCtx, func() {}, entry.rootTargetID, purposeHint)
	}
	// 必须从 browserCtx 派生 (复用同一 Chrome 进程), 不能从 allocCtx 派生 (会启新 Chrome).
	tabCtx, tabCancel := chromedp.NewContext(entry.browserCtx)
	if err := runCDPWithSoftTimeout(tabCtx, BrowserPoolChromeWarmup); err != nil {
		tabCancel()
		return nil, nil, nil, "", fmt.Errorf("create tab (purpose=%q): %w", purposeHint, err)
	}

	// 提取 TargetID — chromedp 把它存在 tabCtx 的 internal target.
	// 通过 chromedp.FromContext 拿到 c.Target 信息.
	cdpCtx := chromedp.FromContext(tabCtx)
	var targetID string
	if cdpCtx != nil && cdpCtx.Target != nil {
		targetID = string(cdpCtx.Target.TargetID)
	}
	if targetID == "" {
		// fallback: 以 tabCtx 指针地址生成稳定 ID (诊断用; 不可能命中, 防御)
		targetID = fmt.Sprintf("local-%p", tabCtx)
	}
	return p.materializeManagedTabLocked(entry, tabCtx, tabCancel, targetID, purposeHint)
}

func (p *BrowserPool) materializeManagedTabLocked(entry *chromePoolEntry, tabCtx context.Context, tabCancel context.CancelFunc, targetID, purposeHint string) (context.Context, context.CancelFunc, BrowserCore, string, error) {
	// 指纹/stealth/emulation 注入 (与 r1/r2 startChromeLocked 之后那段逻辑对齐)
	preset := ResolveRuntimeFingerprintPreset(entry.presetID, entry.chromePath)
	if preset == nil {
		preset = BuiltinPresets[entry.presetID]
	}
	if preset == nil {
		return nil, nil, nil, "", fmt.Errorf("browser pool: unknown preset_id %q", entry.presetID)
	}
	mode := NormalizeBrowserMode(entry.mode, ModeHeaded)
	if preset != nil {
		needsFullStealth := mode == ModeHeadless || p.displayMgr.UsesHeadlessHumanMode()
		if needsFullStealth {
			_ = runCDPWithSoftTimeout(tabCtx, BrowserPoolCDPActionTimeout, chromedp.ActionFunc(func(ctx context.Context) error {
				return applyFingerprintEmulation(ctx, entry.chromePath, entry.presetID)
			}))
			_ = runCDPWithSoftTimeout(tabCtx, BrowserPoolCDPActionTimeout, chromedp.ActionFunc(func(ctx context.Context) error {
				_, err := cdppage.AddScriptToEvaluateOnNewDocument(GenerateStealthScript(preset)).Do(ctx)
				return err
			}))
			// persona facet 随 preset 同源施加(单一机制 applyPersonaEmulation)
			if persona := resolvePersonaOrNil(entry.personaID); persona != nil {
				_ = runCDPWithSoftTimeout(tabCtx, BrowserPoolCDPActionTimeout, chromedp.ActionFunc(func(ctx context.Context) error {
					return applyPersonaEmulation(ctx, persona)
				}))
			}
			_ = runCDPWithSoftTimeout(tabCtx, BrowserPoolCDPActionTimeout, chromedp.ActionFunc(func(ctx context.Context) error {
				override := emulation.SetDeviceMetricsOverride(
					int64(preset.ViewportW), int64(preset.ViewportH),
					preset.DeviceScaleFactor, preset.Mobile,
				)
				if preset.ViewportW < preset.ViewportH {
					override = override.WithScreenOrientation(&emulation.ScreenOrientation{
						Type: emulation.OrientationTypePortraitPrimary, Angle: 0,
					})
				} else {
					override = override.WithScreenOrientation(&emulation.ScreenOrientation{
						Type: emulation.OrientationTypeLandscapePrimary, Angle: 90,
					})
				}
				return override.Do(ctx)
			}))
			if preset.Touch {
				_ = runCDPWithSoftTimeout(tabCtx, BrowserPoolCDPActionTimeout, chromedp.ActionFunc(func(ctx context.Context) error {
					return emulation.SetTouchEmulationEnabled(true).
						WithMaxTouchPoints(int64(preset.MaxTouchPoints)).Do(ctx)
				}))
			}
		} else {
			_ = runCDPWithSoftTimeout(tabCtx, BrowserPoolCDPActionTimeout, chromedp.ActionFunc(func(ctx context.Context) error {
				_, err := cdppage.AddScriptToEvaluateOnNewDocument(MinimalWebdriverStealthScript).Do(ctx)
				return err
			}))
		}
	}

	// viewport 元数据 (供 Screencast / target switch 重放)
	vpW, vpH := DefaultViewportWidth, DefaultViewportHeight
	vpDPR := 1.0
	vpMobile := false
	vpTouch := false
	vpMaxTouchPoints := int64(1)
	if preset != nil {
		vpW, vpH = preset.ViewportW, preset.ViewportH
		vpDPR = preset.DeviceScaleFactor
		vpMobile = preset.Mobile
		vpTouch = preset.Touch
		vpMaxTouchPoints = int64(preset.MaxTouchPoints)
	}

	snapEngine := newSnapshotEngine()
	actEngine := newActionEngine(snapEngine)
	liveEngine := newLiveViewEngine(vpW, vpH)
	takeoverCtrl := newTakeoverController(tabCtx)

	tracker := NewTargetTracker(tabCtx)
	if entry.chromeHandle != nil && entry.chromeHandle.WSURL() != "" {
		wsURL := entry.chromeHandle.WSURL()
		tracker.SetTargetCloser(func(targetID target.ID) error {
			return targetGraphCloseViaDevTools(wsURL, targetID)
		})
	}
	if mode == ModeHeaded && entry.browserMuxHost != nil {
		hostState := *entry.browserMuxHost
		tracker.SetForegroundGuard(func(_ target.ID, _ string) error {
			ctx, cancel := context.WithTimeout(context.Background(), BrowserMuxHostForegroundGuardTimeout)
			defer cancel()
			live, err := BrowserMuxHostHealth(ctx, &hostState)
			if err != nil {
				return fmt.Errorf("browser_mux_host: foreground guard health failed muxhost_id=%s: %w", hostState.MuxHostID, err)
			}
			if !live.DisplayVerified || !live.ChromeWindowContained || !live.ChromeAlive {
				return fmt.Errorf("browser_mux_host: foreground guard failed muxhost_id=%s display_verified=%t chrome_window_contained=%t chrome_alive=%t",
					live.MuxHostID, live.DisplayVerified, live.ChromeWindowContained, live.ChromeAlive)
			}
			return nil
		})
		// Headed Chrome on CGVirtualDisplay must never receive Page.bringToFront.
		// BringToFront triggers [NSWindow makeKeyAndOrderFront:] which causes Chrome
		// to escape the virtual display and appear on the user's main Space.
		tracker.SetNoFrontMode(true)
	} else if mode == ModeHeaded && runtime.GOOS == "darwin" && p.virtualDisplay != nil && entry.chromeHandle != nil {
		virtualDisplay := p.virtualDisplay
		chromePID := entry.chromeHandle.PID()
		tracker.SetForegroundGuard(func(_ target.ID, _ string) error {
			return virtualDisplay.VerifyChromeContained(chromePID, BrowserMuxHostForegroundContainmentCheck)
		})
		tracker.SetNoFrontMode(true)
	}
	// 终局约束: main tab 仍以当前 workspace tab (tabCtx) 为语义锚点，
	// 但 Target discovery / auto-attach / browser-level target events 必须绑定 entry.browserCtx。
	// 之前错误地把监听挂在 tabCtx，page-opened target=_blank / window.open 的 TargetCreated
	// 可能漏收，只能在后续 RefreshTargets() 中被动补登记，导致 liveview 不自动切 tab，
	// 真实 headed Chrome 却已经打开了新标签，表现为焦点/Space 被抢走。
	tracker.SetupListeners(entry.browserCtx)

	core := &browserCoreImpl{
		allocCtx:           entry.allocCtx,
		allocCancel:        nil, // Pool owns alloc lifecycle, not individual tabs
		browserCtx:         tabCtx,
		browserCancel:      tabCancel,
		snapEngine:         snapEngine,
		actEngine:          actEngine,
		liveEngine:         liveEngine,
		takeoverCtrl:       takeoverCtrl,
		fingerprintPreset:  entry.presetID,
		runtimeMode:        mode,
		chromePath:         entry.chromePath,
		liveViewportW:      vpW,
		liveViewportH:      vpH,
		liveViewportDPR:    vpDPR,
		liveViewportMobile: vpMobile,
		liveViewportTouch:  vpTouch,
		liveViewportMaxTP:  vpMaxTouchPoints,
		targetTracker:      tracker,
		profileID:          purposeHint, // 与 r1/r2 行为一致 (legacy 调用方期望)
		launcher:           NewChromeLauncher(),
		supervisor:         NewChromeSupervisor(),
	}
	if mode == ModeVisible && entry.chromeHandle != nil {
		scheduleBootstrapBlankCleanup(entry.browserCtx, string(entry.identity.Key), targetID, entry.chromeHandle.WSURL())
	}
	return tabCtx, tabCancel, core, targetID, nil
}

// evictOldestLocked 回收最久未使用的 Tab (锁内). 返回 true 如果成功回收.
//
// v2: 跨 entry 选择最久未使用 Tab; webui-panel Tab 长驻不回收 (handle.WorkspaceID 判定).
func (p *BrowserPool) evictOldestLocked() bool {
	var oldestEntry *chromePoolEntry
	var oldestTab *tabEntry
	var oldestTime time.Time

	for _, entry := range p.entries {
		for _, te := range entry.tabs {
			if te.handle != nil && te.handle.WorkspaceID == "webui-panel" {
				continue
			}
			if oldestTab == nil || te.lastActive.Before(oldestTime) {
				oldestTab = te
				oldestEntry = entry
				oldestTime = te.lastActive
			}
		}
	}
	if oldestTab == nil {
		return false
	}
	p.closeTabEntryLocked(oldestEntry, oldestTab)
	return true
}

func (p *BrowserPool) closeTabEntryLocked(entry *chromePoolEntry, te *tabEntry) {
	if te == nil {
		return
	}
	if te.targetID == entry.rootTargetID {
		entry.rootClaimed = false
	} else if entry.browserCtx != nil && te.targetID != "" {
		if err := targetGraphClose(entry.browserCtx, target.ID(te.targetID), BrowserPoolCDPActionTimeout); err != nil {
			log.Printf("[BROWSER-POOL/v2] close target failed target=%s err=%v", te.targetID, err)
		}
	}
	if te.tabCancel != nil {
		te.tabCancel()
	}
	delete(entry.tabs, te.targetID)
	_ = p.tabIndex.Unregister(te.targetID)
}

func (p *BrowserPool) resetEntryLocked(identityKey IdentityKey, reason string) bool {
	entry, ok := p.entries[identityKey]
	if !ok {
		return false
	}
	tabCount := len(entry.tabs)
	for tid, te := range entry.tabs {
		if te.tabCancel != nil {
			te.tabCancel()
		}
		delete(entry.tabs, tid)
		_ = p.tabIndex.Unregister(tid)
	}
	if entry.browserCtx != nil {
		done := make(chan struct{})
		browserCtx := entry.browserCtx
		go func() {
			defer close(done)
			_ = chromedp.Cancel(browserCtx)
		}()
		grace := envShutdownGraceSec()
		select {
		case <-done:
			log.Printf("[BROWSER-POOL/v2] ResetIdentity: identity=%s closed browser within grace=%v", identityKey, grace)
		case <-time.After(grace):
			log.Printf("[BROWSER-POOL/v2] AUDIT: reset_chrome_killed_after_grace identity=%s reason=%s grace_sec=%d", identityKey, reason, int(grace.Seconds()))
		}
	}
	if entry.browserCancel != nil {
		entry.browserCancel()
		entry.browserCancel = nil
	}
	if entry.allocCancel != nil {
		entry.allocCancel()
		entry.allocCancel = nil
	}
	if entry.chromeHandle != nil {
		_ = entry.chromeHandle.Kill()
		RemoveProfileOwnerMarker(entry.profileDir, identityKey)
		entry.chromeHandle = nil
		entry.browserMuxHost = nil
	}
	entry.browserCtx = nil
	entry.rootTargetID = ""
	entry.rootClaimed = false
	entry.started = false
	delete(p.entries, identityKey)
	log.Printf("[BROWSER-POOL/v2] ResetIdentity: identity=%s reason=%s destroyed_tabs=%d", identityKey, reason, tabCount)
	return true
}

func captureTargetID(ctx context.Context) string {
	cdpCtx := chromedp.FromContext(ctx)
	if cdpCtx != nil && cdpCtx.Target != nil {
		return string(cdpCtx.Target.TargetID)
	}
	return ""
}

func closeBootstrapBlankTargets(browserCtx context.Context, keepTargetIDs ...string) error {
	if browserCtx == nil {
		return nil
	}
	keep := make(map[string]bool, len(keepTargetIDs))
	for _, targetID := range keepTargetIDs {
		if targetID != "" {
			keep[targetID] = true
		}
	}
	infos, err := targetGraphListPages(browserCtx, TargetDiscoveryTimeout)
	if err != nil {
		return err
	}
	for _, info := range infos {
		if info == nil {
			continue
		}
		if keep[string(info.TargetID)] {
			continue
		}
		if IsUserPageTargetURL(info.URL) {
			continue
		}
		if err := targetGraphClose(browserCtx, info.TargetID, BrowserPoolCDPActionTimeout); err != nil {
			log.Printf("[BROWSER-POOL/v2] close bootstrap target failed target=%s err=%v", info.TargetID, err)
		}
	}
	return nil
}

type devToolsTargetInfo struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	URL  string `json:"url"`
}

func selectReusablePageTarget(wsURL string) (string, error) {
	parsed, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("parse devtools ws url: %w", err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("devtools ws url missing host")
	}
	scheme := "http"
	if parsed.Scheme == "wss" {
		scheme = "https"
	}
	listURL := (&url.URL{Scheme: scheme, Host: parsed.Host, Path: "/json/list"}).String()
	resp, err := (&http.Client{Timeout: DevToolsRequestTimeout}).Get(listURL)
	if err != nil {
		return "", fmt.Errorf("list devtools targets: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("list devtools targets status=%d", resp.StatusCode)
	}
	var infos []devToolsTargetInfo
	if err := json.NewDecoder(resp.Body).Decode(&infos); err != nil {
		return "", fmt.Errorf("decode devtools targets: %w", err)
	}
	fallback := ""
	for _, info := range infos {
		if info.Type != "page" || info.ID == "" {
			continue
		}
		if fallback == "" {
			fallback = info.ID
		}
		if IsUserPageTargetURL(info.URL) {
			return info.ID, nil
		}
	}
	return fallback, nil
}

func closeBootstrapBlankTargetsViaDevTools(wsURL string, keepTargetIDs ...string) error {
	baseURL, err := targetGraphDevToolsBaseURL(wsURL)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: DevToolsRequestTimeout}

	resp, err := client.Get(baseURL + "/json/list")
	if err != nil {
		return fmt.Errorf("devtools list targets: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("devtools list targets: HTTP %d", resp.StatusCode)
	}

	var infos []devToolsTargetInfo
	if err := json.NewDecoder(resp.Body).Decode(&infos); err != nil {
		return fmt.Errorf("decode devtools targets: %w", err)
	}
	keep := make(map[string]bool, len(keepTargetIDs))
	for _, targetID := range keepTargetIDs {
		if targetID != "" {
			keep[targetID] = true
		}
	}
	for _, info := range infos {
		if info.Type != "page" {
			continue
		}
		if keep[info.ID] {
			continue
		}
		if IsUserPageTargetURL(info.URL) {
			continue
		}
		if closeErr := targetGraphCloseViaDevTools(wsURL, target.ID(info.ID)); closeErr != nil {
			log.Printf("[BROWSER-POOL/v2] devtools close bootstrap target failed target=%s err=%v", info.ID, closeErr)
			continue
		}
	}
	return nil
}

func scheduleBootstrapBlankCleanup(browserCtx context.Context, identityKey, keepTargetID, wsURL string) {
	if browserCtx == nil || keepTargetID == "" || wsURL == "" {
		return
	}
	go func() {
		for _, delay := range TargetBootstrapCleanupDelays {
			select {
			case <-browserCtx.Done():
				return
			case <-time.After(delay):
			}
			if err := closeBootstrapBlankTargetsViaDevTools(wsURL, keepTargetID); err != nil {
				log.Printf("[BROWSER-POOL/v2] delayed bootstrap target cleanup failed identity=%s keep=%s err=%v", identityKey, keepTargetID, err)
			}
		}
	}()
}

func (p *BrowserPool) idleReaper() {
	interval := p.config.IdleTimeout / 2
	if interval <= 0 {
		interval = time.Minute
	}
	if interval < BrowserPoolMinReapInterval {
		interval = BrowserPoolMinReapInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.reapIdleTabs()
		case <-p.reaperStop:
			return
		}
	}
}

func (p *BrowserPool) reapIdleTabs() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	cutoff := time.Now().Add(-p.config.IdleTimeout)
	for _, entry := range p.entries {
		for _, te := range entry.tabs {
			if te == nil || te.handle == nil {
				continue
			}
			if te.handle.WorkspaceID == "webui-panel" {
				continue
			}
			if te.lastActive.After(cutoff) {
				continue
			}
			p.closeTabEntryLocked(entry, te)
		}
	}
}

// ============================================================
// § Switch / Update legacy methods (作用于 default identity)
// ============================================================

// SwitchProfile 切换 default identity 的 Profile.
func (p *BrowserPool) SwitchProfile(ctx context.Context, profileID string) (restarted bool, err error) {
	profileID = NormalizeProfileID(profileID)

	p.mu.Lock()
	currentID := NormalizeProfileID(p.config.ProfileID)
	if currentID == profileID {
		p.mu.Unlock()
		return false, nil
	}

	if entry, ok := p.entries[p.defaultIdentityKey]; ok {
		tabCount := len(entry.tabs)
		for tid, te := range entry.tabs {
			te.tabCancel()
			delete(entry.tabs, tid)
			_ = p.tabIndex.Unregister(tid)
		}
		if entry.allocCancel != nil {
			entry.allocCancel()
			entry.allocCancel = nil
		}
		if entry.chromeHandle != nil {
			_ = entry.chromeHandle.Kill()
			RemoveProfileOwnerMarker(entry.profileDir, p.defaultIdentityKey)
			entry.chromeHandle = nil
			entry.browserMuxHost = nil
		}
		entry.started = false
		delete(p.entries, p.defaultIdentityKey)
		log.Printf("[BROWSER-POOL/v2] SwitchProfile: from=%s to=%s preset=%s destroyed_tabs=%d",
			currentID, profileID, p.config.PresetID, tabCount)
	}
	p.config.ProfileID = profileID
	newKey, _ := p.registry.Resolve(p.config.ProfileID, defaultPresetForConfig(p.config), NoopPolicy{})
	p.defaultIdentityKey = newKey
	p.mu.Unlock()
	return true, nil
}

// UpdateViewport 更新 default identity 上 webui-panel Tab 的 viewport.
func (p *BrowserPool) UpdateViewport(ctx context.Context, width, height int, dpr float64, mobile, touch bool, maxTouchPoints int64) error {
	return p.UpdateViewportForIdentity(ctx, p.defaultIdentityKey, "webui-panel", width, height, dpr, mobile, touch, maxTouchPoints)
}

// UpdateViewportForIdentity updates the LiveView viewport for the tab owned by
// a scoped BrowserPool identity/workspace pair.
//
// v2: 通过 entry.tabs 扫描 handle.WorkspaceID 定位 caller-owned Tab
// (legacy purpose index 已删除).
func (p *BrowserPool) UpdateViewportForIdentity(ctx context.Context, identityKey IdentityKey, workspaceID string, width, height int, dpr float64, mobile, touch bool, maxTouchPoints int64) error {
	workspaceID = strings.TrimSpace(workspaceID)
	if workspaceID == "" {
		workspaceID = "webui-panel"
	}
	p.mu.Lock()
	if identityKey == "" {
		identityKey = p.defaultIdentityKey
	}
	entry, ok := p.entries[identityKey]
	if !ok {
		p.mu.Unlock()
		return fmt.Errorf("no identity entry: %s", identityKey)
	}
	var te *tabEntry
	for _, candidate := range entry.tabs {
		if candidate.handle != nil && candidate.handle.WorkspaceID == workspaceID {
			te = candidate
			break
		}
	}
	if te == nil {
		p.mu.Unlock()
		return fmt.Errorf("no active tab for workspace %s", workspaceID)
	}
	core, ok := te.core.(*browserCoreImpl)
	if !ok {
		p.mu.Unlock()
		return fmt.Errorf("unexpected core type")
	}
	p.mu.Unlock()

	if maxTouchPoints <= 0 {
		maxTouchPoints = 1
	}
	core.mu.Lock()
	core.liveViewportTouch = touch
	core.liveViewportMaxTP = maxTouchPoints
	core.mu.Unlock()
	if err := core.SetLiveViewport(width, height, dpr, mobile); err != nil {
		return fmt.Errorf("update device metrics: %w", err)
	}
	log.Printf("[BROWSER-POOL/v2] UpdateViewport: %dx%d dpr=%.1f mobile=%v touch=%v max_tp=%d",
		width, height, dpr, mobile, touch, maxTouchPoints)
	return nil
}

// ResetDefaultIdentity forcefully tears down the current default-identity Chrome
// entry so callers can reacquire a clean BrowserCore after interruption or
// target/core corruption. It is a no-op when the default entry does not exist.
func (p *BrowserPool) ResetDefaultIdentity(reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resetEntryLocked(p.defaultIdentityKey, reason)
}

// ResetIdentity tears down a specific identity so callers can reacquire it with
// a different execution mode. This is the primitive BS-15 uses for Fast →
// Trusted BrowserRun migration; profile ownership remains with the identity.
func (p *BrowserPool) ResetIdentity(identityKey IdentityKey, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.resetEntryLocked(identityKey, reason)
}

// GetPresetID 返回当前默认 Preset ID (legacy).
func (p *BrowserPool) GetPresetID() string {
	if !p.mu.TryLock() {
		return NormalizePresetID(p.config.PresetID)
	}
	defer p.mu.Unlock()
	return NormalizePresetID(p.config.PresetID)
}

// GetProfileID 返回当前默认 Profile ID (legacy).
func (p *BrowserPool) GetProfileID() string {
	if !p.mu.TryLock() {
		return NormalizeProfileID(p.config.ProfileID)
	}
	defer p.mu.Unlock()
	return NormalizeProfileID(p.config.ProfileID)
}

// DataDir 返回 BrowserPool 绑定的 deepwork dataDir (legacy).
func (p *BrowserPool) DataDir() string {
	if !p.mu.TryLock() {
		return p.config.DataDir
	}
	defer p.mu.Unlock()
	return p.config.DataDir
}

// IsStarted 返回 default identity 的 Chrome 是否已启动.
func (p *BrowserPool) IsStarted() bool {
	if !p.mu.TryLock() {
		return false
	}
	defer p.mu.Unlock()
	if entry, ok := p.entries[p.defaultIdentityKey]; ok {
		return entry.started
	}
	return false
}

// ============================================================
// § Profile schema versioning (与 r1/r2 一致, 不动)
// ============================================================

// defaultProfileSchemaVersion / presetProfileSchemaVersion / resolveBrowserPoolDir / isSandboxedChrome
// 见后段 (与 r1/r2 实现一致, 仅 import 路径).

const defaultProfileSchemaVersion = "v2"

var presetProfileSchemaVersion = map[string]string{
	PresetMacOSChrome:   "v6",
	PresetMacOSSafariUA: "v3",
}

func profileSchemaVersionForPreset(presetID string) string {
	presetID = NormalizePresetID(presetID)
	if version, ok := presetProfileSchemaVersion[presetID]; ok {
		return version
	}
	return defaultProfileSchemaVersion
}

// resolveBrowserPoolDir 解析 BrowserPool profile 目录 (与 r1/r2 一致).
func resolveBrowserPoolDir(dataDir, chromePath, profileID, presetID string) string {
	profileID = NormalizeProfileID(profileID)
	presetID = NormalizePresetID(presetID)
	dirName := presetID + "-" + profileSchemaVersionForPreset(presetID)
	if !isSandboxedChrome(chromePath) {
		return filepath.Join(dataDir, "browser-data", "profiles", profileID, dirName)
	}
	home := os.Getenv("HOME")
	if home != "" {
		snapCommon := filepath.Join(home, "snap", "chromium", "common")
		if fi, err := os.Stat(snapCommon); err == nil && fi.IsDir() {
			return filepath.Join(snapCommon, "deepwork-"+profileID+"-"+dirName)
		}
	}
	return filepath.Join(os.TempDir(), "dw-browser-"+profileID+"-"+dirName)
}

// BrowserPoolProfileDir returns the canonical on-disk profile directory used by
// BrowserPool for a logical profile and preset on the local runtime.
func BrowserPoolProfileDir(dataDir, profileID, presetID string) string {
	return resolveBrowserPoolDir(dataDir, "", profileID, presetID)
}

// isSandboxedChrome 检测 Chrome 是否运行在 sandbox 环境中 (snap/flatpak).
func isSandboxedChrome(chromePath string) bool {
	if strings.Contains(chromePath, "/snap/") || strings.Contains(chromePath, "flatpak") {
		return true
	}
	if strings.Contains(chromePath, "chromium") {
		home := os.Getenv("HOME")
		if home != "" {
			snapDataDir := filepath.Join(home, "snap", "chromium")
			if fi, err := os.Stat(snapDataDir); err == nil && fi.IsDir() {
				return true
			}
		}
	}
	return false
}

// ============================================================
// § 防御性 import marker (避免 unused-import 在某些路径)
// ============================================================
var _ = target.SessionID("") // keep target import live (reserved for Phase_v2_4 PID extraction)
