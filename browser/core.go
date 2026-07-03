// Package browser 实现 BS-09 Browser Runtime。
// 以 A11y+Element Refs 为核心，为 LLM 提供低 token 高精度 Web 感知通道。
//
// 铁律 IR-01: 本包零依赖 Deepwork 上下文（conversation/topic/webui/memory/agent/llm 均不得引入）。
// 铁律 IR-02: 使用 chromedp Go 直连 CDP，不引入 Node.js/TS 运行时。
package browser

import (
	"context"
	"errors"
)

// ============================================================
// § 错误定义（不可新增，不可删除）[Ref: T5-B3.3, BP §A2]
// ============================================================

var (
	// ErrBrowserNotFound — 无 Chrome/Edge 安装。
	ErrBrowserNotFound = errors.New("browser: no Chrome/Edge found")

	// ErrBrowserCrashed — Chrome 进程崩溃。
	ErrBrowserCrashed = errors.New("browser: Chrome process crashed")

	// ErrCDPDisconnected — CDP 连接断开。
	ErrCDPDisconnected = errors.New("browser: CDP connection lost")

	// ErrActFailed — 操作执行失败。
	ErrActFailed = errors.New("browser: action execution failed")

	// ErrRefNotFound — Element Ref 不存在。
	ErrRefNotFound = errors.New("browser: element ref not found")

	// ErrTakeoverActive — 接管模式激活，AI 操作被拒绝。
	ErrTakeoverActive = errors.New("browser: takeover mode active")

	// ErrPasswordField — 密码字段需要安全输入，不经 LLM/WS 明文通道处理 [IR-03]。
	ErrPasswordField = errors.New("browser: password field requires secure input")

	// ErrSnapshotEmpty — A11y 快照返回空。
	ErrSnapshotEmpty = errors.New("browser: A11y snapshot returned empty")

	// ErrAmbiguousLocator — 定位器匹配多个元素。
	ErrAmbiguousLocator = errors.New("browser: ambiguous locator matches multiple elements")

	// ErrStaleRef — ref 已失效（页面自上次 snap 后已变化）。
	ErrStaleRef = errors.New("browser: ref is stale (page has changed since last snap)")

	// ErrSessionNotFound — 会话不存在。
	ErrSessionNotFound = errors.New("browser: session not found")

	// ErrInvalidRefInOneShot — @rN ref 在无 session 模式下不可用。
	ErrInvalidRefInOneShot = errors.New("browser: @rN refs require --session mode")

	// r2 Delta-REQ (TH-0418-c9x) 新增错误 [Ref: BP §A2]

	// ErrSelectorNotFound — CSS 选择器无匹配元素。
	ErrSelectorNotFound = errors.New("browser: CSS selector matched no elements")

	// ErrEvalFailed — JavaScript 求值失败。
	ErrEvalFailed = errors.New("browser: JavaScript evaluation failed")

	// ErrCookieDecryptFailed — Cookie 解密失败（macOS Keychain/Linux SecretService 拒绝）。
	ErrCookieDecryptFailed = errors.New("browser: cookie decryption failed")

	// ErrCookieDBLocked — Cookie 数据库锁定且无法复制。
	ErrCookieDBLocked = errors.New("browser: cookie database locked")
)

// ============================================================
// § 核心数据结构 [Ref: T5-B3.2, BP §B2]
// ============================================================

// Snapshot 是页面语义快照（不可变）。
type Snapshot struct {
	PageTitle        string
	URL              string
	TargetID         string       // CDP target.ID that produced this snapshot, when known
	Text             string       // compact A11y 文本（含 Element Refs）
	Refs             []ElementRef // 可交互元素列表（DFS 顺序）
	SnapshotType     string       // "a11y" | "dom_fallback" | "screenshot_fallback" | "progressive_loading"
	TokenEst         int          // 估算 token 数
	Progressive      bool         // true 表示页面可观测但暂不可稳定操作，调用方应稍后重试 action view
	LoadState        string       // "actionable" | "readable" | "visual" | "loading" | "waiting_for_app" | "unavailable"
	ReadyState       string       // document.readyState: "loading" | "interactive" | "complete" | ""
	RetryAfterMillis int          // 建议调用方下一次刷新 action view 的最小等待时间
	ProgressReason   string       // 渐进状态原因，面向日志和 CLI 输出
	Diagnostics      map[string]interface{}
}

// BrowserEngine 标识浏览器引擎类型。
type BrowserEngine string

const (
	EngineChrome BrowserEngine = "chrome"
	EngineSafari BrowserEngine = "safari"
)

// Rect 表示元素矩形区域。
type Rect struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// NodeLocator 是引擎无关的元素定位器。Chrome 使用 BackendNodeID，Safari 使用 AXPath + StableKey。
type NodeLocator struct {
	Engine        BrowserEngine `json:"engine"`
	BackendNodeID int64         `json:"backend_node_id,omitempty"` // Chrome only
	AXPath        string        `json:"ax_path,omitempty"`         // Safari: AX 树路径 (如 "0.3.1")
	StableKey     string        `json:"stable_key,omitempty"`      // Safari: role+name+identifier 组合，用于漂移重定位
	Ordinal       int           `json:"ordinal,omitempty"`         // DFS 序号
	Frame         Rect          `json:"frame,omitempty"`           // 元素位置 (Safari coordinate tap fallback)
}

// NormalizeEngine 标准化引擎名称。空值和未知值默认为 Chrome。
func NormalizeEngine(e BrowserEngine) BrowserEngine {
	switch e {
	case EngineSafari:
		return EngineSafari
	default:
		return EngineChrome
	}
}

// ElementRef 是可交互元素的语义引用（每次 snap 后重建）。
type ElementRef struct {
	Ref                string      // "e1", "e2", ... 或 session 模式下 "@r1", "@r2", ...
	BackendNodeID      int64       // CDP BackendNodeID
	Locator            NodeLocator `json:"-"` // 引擎无关定位器（内部使用，不序列化到 CLI 输出）
	AXPath             string      `json:"-"` // Safari AX 树路径（便捷别名，Chrome 为空）
	Role               string      // ARIA role
	Name               string      // accessible name
	Placeholder        string      // input placeholder
	TestID             string      // data-testid 属性值
	Interactable       bool        // 是否可交互（button/input/link/select...）
	NameFull           string      // 完整 accessible name（不截断）
	NameShort          string      // 截断后的 name（≤50 字符，用于显示）
	RecommendedLocator string      // 推荐的 locator（供 Agent 直接使用）
	MatchCount         int         // 同 role+name 的元素数量
}

// ScreencastFrame 是 Screencast 帧（LiveView 推送）。
type ScreencastFrame struct {
	Data      []byte // JPEG 字节
	Timestamp int64  // Unix ms
	SessionID int64  // CDP SessionID（用于 ACK）
	TargetID  string // Chrome TargetID，用于前端确认首帧归属
}

// TouchPoint 代表一个触摸点（多指触控时区分）。
type TouchPoint struct {
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
	ID int64   `json:"id"` // 手指标识符
}

// InputEvent 是前端接管模式下的输入事件。
type InputEvent struct {
	Type        string       `json:"type"`  // "mouse" | "keyboard" | "touch"
	Event       string       `json:"event"` // mousePressed/mouseReleased/mouseMoved/mouseWheel/keyDown/keyUp/char/touchStart/touchMove/touchEnd/touchCancel
	X           float64      `json:"x"`
	Y           float64      `json:"y"`
	Button      string       `json:"button"`
	ClickCount  int          `json:"clickCount"`
	DeltaX      float64      `json:"deltaX"` // scroll
	DeltaY      float64      `json:"deltaY"` // scroll
	Key         string       `json:"key"`
	Code        string       `json:"code"`
	Text        string       `json:"text"`
	Modifiers   int          `json:"modifiers"`             // bit flags: Alt=1, Ctrl=2, Meta=4, Shift=8
	TouchPoints []TouchPoint `json:"touchPoints,omitempty"` // touch 事件的触摸点列表
}

// ============================================================
// § BrowserCore 接口 [Ref: T5-B3.1, BP §B2]
// ============================================================

// SessionCore — BrowserCore 的 session 扩展接口（含 @rN ref 支持）。
type SessionCore interface {
	BrowserCore
	// SnapWithSessionMode 获取快照（session 模式，使用 @rN ref）。
	SnapWithSessionMode(ctx context.Context, snapEpoch int) (*Snapshot, error)
	// SnapWithOptions 获取快照并应用 SnapOptions 过滤（selector/compact/max-depth）[r2]。
	SnapWithOptions(ctx context.Context, opts SnapOptions) (*Snapshot, error)
	// ActWithSessionMode 执行操作（session 模式，允许 @rN ref）。
	ActWithSessionMode(ctx context.Context, action string, observe bool) (*Snapshot, error)
	// RestoreRefsFromSession 从 session 文件恢复 ref 表。
	RestoreRefsFromSession(refs []SessionRef)
}

// BrowserCore — internal/browser/ 对外服务接口（Core 层，零 Deepwork 依赖）。
type BrowserCore interface {
	// Navigate 导航到 URL，等待页面加载完成，返回 A11y 快照。
	Navigate(ctx context.Context, url string) (*Snapshot, error)

	// Snap 获取当前页面 A11y 快照（不导航）。
	Snap(ctx context.Context) (*Snapshot, error)

	// Act 执行操作，返回操作后快照。
	// action 语法: "click e3" | "type e5 'hello'" | "scroll down" | "hover e7" | "select e4 'opt2'"
	// observe=false 时不返回 snap（连续操作优化）[D.2-C2]。
	Act(ctx context.Context, action string, observe bool) (*Snapshot, error)

	// Text 提取当前页面纯文本（~500-800 tok，focus 为可选 ref 或 CSS selector）。
	Text(ctx context.Context, focus *string) (string, error)

	// Screenshot 截图，annotate=true 时叠加 Element Ref 标注。
	Screenshot(ctx context.Context, annotate bool) ([]byte, error)

	// StartLiveView 启动 Screencast 帧推送，返回 FrameBroadcastHub。
	// 多次调用安全：已活跃时返回同一 hub，不重启 Screencast。
	// WS handler 通过 hub.Subscribe(connID) 获取独立帧 channel [CAP-BS09-C3 r2]。
	StartLiveView(ctx context.Context) (*FrameBroadcastHub, error)

	// StopLiveView 停止 Screencast。
	StopLiveView(ctx context.Context) error

	// EnableTakeover 切换到 Takeover 模式（Human 接管）。
	EnableTakeover(ctx context.Context) error

	// DisableTakeover 释放 Takeover，恢复 OBSERVE 模式。
	DisableTakeover(ctx context.Context) error

	// DispatchInput 在接管模式下转发输入事件。
	DispatchInput(ctx context.Context, event *InputEvent) error

	// Close 关闭 Browser，释放资源。
	Close(ctx context.Context) error

	// EvalJS 在当前活跃标签上执行 JavaScript 表达式，结果写入 result 指针。
	// 活跃标签由 TargetTracker 决定: 有新标签(如 target=_blank)时在新标签执行,
	// 否则在主标签(browserCtx)执行。[BUG-FIX + TH-0414-b3m]
	EvalJS(ctx context.Context, expr string, result interface{}) error

	// GetTargetTracker 返回 TargetTracker（多 Target 自动跟随）[r3]。
	// 返回 nil 表示不支持（如 session 模式）。
	GetTargetTracker() *TargetTracker

	// SetPolicy 设置本 session 的安全/确定性策略（远程写门控），并（若非空）
	// 刷新用于 per-act origin 分类的 current URL。domain-neutral：分类逻辑单源于
	// policy.go（EvaluateAct/IsMutatingOp/URLOrigin），实现方不重复分类。
	SetPolicy(policy SessionPolicy, currentURL string)
}

// LiveViewportSyncer 暴露 LiveView viewport 的跨 target 同步能力。
// 适用于 BrowserPool/WebUI 场景：前端真实容器尺寸变化后，当前活跃 target 与
// 后续切换到的新 target（新 tab / auth popup）都应继承同一套 viewport 语义。
//
// 设计目标:
//   - PC / iOS 共用一套接口，不拆平台特判
//   - 不仅同步 Screencast 分辨率，也同步 CDP DeviceMetrics / Touch 语义
//   - 新 target 激活时可重放当前 liveview viewport，避免多 tab 出现黑边或错位
type LiveViewportSyncer interface {
	// SetLiveViewport 更新当前 liveview 目标 viewport 配置，并立即应用到活跃 target。
	SetLiveViewport(width, height int, dpr float64, mobile bool) error
	// SyncActiveTargetViewport 将最近一次 liveview viewport 配置重放到指定活跃 target。
	SyncActiveTargetViewport(targetCtx context.Context) error
}
