package browser

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================
// § SessionState — 权威状态 
// ============================================================

// TabInfo 描述一个浏览器标签页的元数据 。
type TabInfo struct {
	ID string `json:"id"` // CDP target.ID
	Title string `json:"title"`
	URL string `json:"url"`
	Active bool `json:"active"` // 当前 screencast 是否指向此 tab
	Closable bool `json:"closable"` // false = browser root target, cannot be closed through tab UI
}

// SessionState 是 BrowserSession 的权威状态。
// 由 SessionAuthority 持有并管理，通过 channel fan-out 广播给所有 WS subscriber。
type SessionState struct {
	Mode string `json:"mode"` // "observe" / "takeover" / "idle" / "loading"
	Owner string `json:"owner"` // 当前 takeover 持有者 connID
	LeaseToken string `json:"leaseToken"` // owner 的租约令牌
	LeaseExpiry int64 `json:"leaseExpiry"` // 租约过期时间戳（毫秒）
	Viewport struct {
		CssW int `json:"cssW"`
		CssH int `json:"cssH"`
	} `json:"viewport"`
	Cursor string `json:"cursor"` // 远端 cursor 样式
	URL string `json:"url"` // 当前页面 URL
	Title string `json:"title"` // 当前页面标题
	Seq int64 `json:"seq"` // 单调递增序列号（原子递增）
	TargetCount int `json:"targetCount,omitempty"` // 已知 page Target 数量 
	Tabs TabInfo `json:"tabs,omitempty"` // 完整 tab 列表 
	Recording bool `json:"recording,omitempty"` // 当前是否正在录制 
	RecordElapsedMs int64 `json:"record_elapsed_ms,omitempty"` // 录制已用时间（毫秒）
}

// ============================================================
// § SessionAuthority — 状态权威 + 广播 
// ============================================================

// SessionAuthority 管理 SessionState 并 fan-out 广播给所有 WS subscriber。
//
// 设计要点:
// - 1-slot channel 保新丢旧（与 FrameBroadcastHub 保持一致）
// - seq 原子递增，确保 subscriber 能检测到跳变
type SessionAuthority struct {
	mu sync.RWMutex
	state *SessionState
	subscribers map[string]chan *SessionState // connID → 1-slot state channel（代次匹配，见 Unsubscribe）
	seq atomic.Int64 // 单调递增序列号
}

// NewSessionAuthority 创建 SessionAuthority 实例，初始模式为 "idle"。
func NewSessionAuthority(viewportW, viewportH int) *SessionAuthority {
	s := &SessionState{
		Mode: "idle"
	}
	s.Viewport.CssW = viewportW
	s.Viewport.CssH = viewportH

	return &SessionAuthority{
		state: s
		subscribers: make(map[string]chan *SessionState)
	}
}

// Subscribe 为 connID 注册 1-slot 状态 channel，返回 channel 本身（用于 Unsubscribe 匹配）。
// 重复注册同一 connID 会先关闭旧 channel 再创建新的（幂等注册）。
func (a *SessionAuthority) Subscribe(connID string) chan *SessionState {
	a.mu.Lock
	defer a.mu.Unlock

	// 旧 channel 存在则先关闭（避免旧连接的 defer Unsubscribe 关掉新 channel）
	if old, ok := a.subscribers[connID]; ok {
		close(old)
	}

	ch := make(chan *SessionState, 1)
	a.subscribers[connID] = ch
	return ch
}

// Unsubscribe 用 channel 指针做代次匹配，只关闭与传入 ch 相同的 channel。
// 若 connID 已被新连接替换（ch 不匹配），静默忽略，不影响新连接。
// 这是 SEV-H2 的关键修复：防止旧连接的 defer 关掉新连接的 channel。
func (a *SessionAuthority) Unsubscribe(connID string, ch chan *SessionState) {
	a.mu.Lock
	defer a.mu.Unlock

	// 代次匹配: 只有 channel 指针完全相同时才关闭
	if current, ok := a.subscribers[connID]; ok && current == ch {
		close(current)
		delete(a.subscribers, connID)
	}
	// 不匹配说明已被新连接替换，静默忽略
}

// GetState 返回当前状态快照（深拷贝，避免调用方修改内部状态）。
func (a *SessionAuthority) GetState SessionState {
	a.mu.RLock
	defer a.mu.RUnlock
	return cloneSessionState(a.state)
}

// UpdateMode 更新模式（owner / leaseToken / leaseExpiry）→ seq++ → 广播。
func (a *SessionAuthority) UpdateMode(mode, owner, leaseToken string, leaseExpiry int64) {
	a.mu.Lock
	a.state.Mode = mode
	a.state.Owner = owner
	a.state.LeaseToken = leaseToken
	a.state.LeaseExpiry = leaseExpiry
	a.state.Seq = a.seq.Add(1)
	snapshot := cloneSessionState(a.state)
	a.mu.Unlock

	a.broadcast(&snapshot)
}

// UpdateNavigation 更新 URL/Title → seq++ → 广播。
func (a *SessionAuthority) UpdateNavigation(url, title string) {
	a.mu.Lock
	a.state.URL = url
	a.state.Title = title
	a.state.Seq = a.seq.Add(1)
	snapshot := cloneSessionState(a.state)
	a.mu.Unlock

	a.broadcast(&snapshot)
}

// UpdateCursor 更新远端 cursor 样式 → seq++ → 广播。
func (a *SessionAuthority) UpdateCursor(cursor string) {
	a.mu.Lock
	a.state.Cursor = cursor
	a.state.Seq = a.seq.Add(1)
	snapshot := cloneSessionState(a.state)
	a.mu.Unlock

	a.broadcast(&snapshot)
}

// UpdateViewport 更新 viewport 尺寸 → seq++ → 广播。
func (a *SessionAuthority) UpdateViewport(cssW, cssH int) {
	a.mu.Lock
	a.state.Viewport.CssW = cssW
	a.state.Viewport.CssH = cssH
	a.state.Seq = a.seq.Add(1)
	snapshot := cloneSessionState(a.state)
	a.mu.Unlock

	a.broadcast(&snapshot)
}

// UpdateRecording 更新录制状态 → seq++ → 广播 。
func (a *SessionAuthority) UpdateRecording(recording bool, elapsedMs int64) {
	a.mu.Lock
	a.state.Recording = recording
	a.state.RecordElapsedMs = elapsedMs
	a.state.Seq = a.seq.Add(1)
	snapshot := cloneSessionState(a.state)
	a.mu.Unlock

	a.broadcast(&snapshot)
}

// UpdateTargetInfo 更新 Target 信息（URL/Title/Count/Tabs）→ seq++ → 广播 。
func (a *SessionAuthority) UpdateTargetInfo(url, title string, targetCount int, tabs ...TabInfo) {
	a.mu.Lock
	a.state.URL = url
	a.state.Title = title
	a.state.TargetCount = targetCount
	if len(tabs) > 0 {
		a.state.Tabs = cloneTabInfo(tabs[0])
	}
	a.state.Seq = a.seq.Add(1)
	snapshot := cloneSessionState(a.state)
	a.mu.Unlock

	a.broadcast(&snapshot)
}

func cloneSessionState(state *SessionState) SessionState {
	if state == nil {
		return SessionState{}
	}
	snapshot := *state
	snapshot.Tabs = cloneTabInfo(state.Tabs)
	return snapshot
}

func cloneTabInfo(tabs TabInfo) TabInfo {
	if tabs == nil {
		return nil
	}
	out := make(TabInfo, len(tabs))
	copy(out, tabs)
	return out
}

// broadcast 将状态快照 fan-out 给所有 subscriber（非阻塞，保新丢旧）。
// 与 FrameBroadcastHub.Publish 逻辑保持一致 。
func (a *SessionAuthority) broadcast(state *SessionState) {
	a.mu.RLock
	defer a.mu.RUnlock

	for _, ch := range a.subscribers {
		select {
		case ch <- state:
		default:
			// channel 满 → 丢旧保新
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- state:
			default:
			}
		}
	}
}

// ============================================================
// § BrowserSession — LiveView 监督层底座 
// ============================================================

// BrowserSession 是 LiveView 监督层的底座对象。
// 统一管理三层:
// - FrameHub (L1): 帧广播 — Screencast 帧 fan-out 给所有 WS subscriber
// - Authority (L2): 状态权威 — SessionState 单一权威 + 广播
// - InputGW (L3): 输入网关 — 接管模式 + 租约 + 输入转发
type BrowserSession struct {
	mu sync.RWMutex
	FrameHub *FrameBroadcastHub
	Authority *SessionAuthority
	InputGW *InputGateway
	browserCore BrowserCore // 当前 Browser Portal LiveView 绑定的 canonical core
	bcAttached bool // AttachBrowserCore 幂等标记
	baseCallback func // 原始 onStateChange 回调（不含 BrowserCore 包装）
	RecordBuf *RecordBuffer // 当前录制缓冲区（nil = 未录制）
	RecordStartTime time.Time // 录制开始时间（用于 elapsed 计算）
}

// NewBrowserSession 创建 BrowserSession 底座实例（不需要 cdpCtx）。
//
// 内部流程:
// 1. 创建 SessionAuthority（viewportW/H 初始化）
// 2. 创建 InputGateway（cdpCtx 为 nil，后续由 AttachBrowserCore / InputGW.SetCDPContext 注入）
// 3. FrameHub 初始为 nil，由 AttachFrameHub(hub) 连接到 BrowserCore 内部 hub
//
// 启动时序:
//
//	NewBrowserSession → SetBrowserSession(bs) →（第一个 WS 连接到来时）→
//	bc.StartLiveView(ctx) → bs.AttachFrameHub(hub) → bs.AttachBrowserCore(bc)
func NewBrowserSession(viewportW, viewportH int) *BrowserSession {
	authority := NewSessionAuthority(viewportW, viewportH)

	// 占位指针，待 InputGateway 创建后由闭包捕获
	var inputGW *InputGateway

	// onStateChange 回调: InputGateway 模式变更 → 读取网关快照 → Authority 广播
	onStateChange := func {
		if inputGW == nil {
			return
		}
		mode := string(inputGW.Mode)
		owner := inputGW.Owner
		leaseToken := inputGW.LeaseToken
		var leaseExpiry int64
		if exp := inputGW.LeaseExpiry; !exp.IsZero {
			leaseExpiry = exp.UnixMilli
		}
		authority.UpdateMode(mode, owner, leaseToken, leaseExpiry)
	}

	// cdpCtx 传 nil，后续通过 AttachBrowserCore / InputGW.SetCDPContext 注入
	inputGW = NewInputGateway(nil, onStateChange)

	return &BrowserSession{
		FrameHub: nil, // 由 AttachFrameHub 连接 BrowserCore 内部 hub
		Authority: authority
		InputGW: inputGW
		baseCallback: onStateChange, // 保存原始回调，ResetAttach 时恢复 (Codex #1)
	}
}

// AttachFrameHub 连接 BrowserCore 内部帧广播 hub（问题 2 修复）。
// 必须在 BrowserCore.StartLiveView 返回后调用。
// 统一真相源: 帧由 BrowserCore 的 liveViewEngine 直接 Publish 到此 hub。
func (bs *BrowserSession) AttachFrameHub(hub *FrameBroadcastHub) {
	bs.FrameHub = hub
}

// AttachedBrowserCore 返回 Browser Portal LiveView 当前绑定的 canonical core。
// REST API 必须优先复用它，否则 tabs/status 等读路径会创建第二个 TargetTracker，
// 造成 Chrome 已有标签页但 UI 投影不切换的状态分裂。
func (bs *BrowserSession) AttachedBrowserCore BrowserCore {
	bs.mu.RLock
	defer bs.mu.RUnlock
	return bs.browserCore
}

// AttachBrowserCore 注入 BrowserCore（SEV-H1 + SEV-H2 修复）。
//
// 1. 注入 cdpCtx 给 InputGateway，使输入事件可以分发到 Chrome。
// 2. SEV-H1: 将 InputGateway 的 onStateChange 回调扩展为同步调用 BrowserCore 的
// EnableTakeover/DisableTakeover，消除 takeoverCtrl 与 InputGateway 的状态分裂。
// InputGateway 是唯一入口，旧 takeoverCtrl 自动跟随。
func (bs *BrowserSession) AttachBrowserCore(bc BrowserCore) {
	if bc == nil {
		return
	}
	// cdpCtx 总是更新（Chrome 可能重启产生新 context）
	if cdpProvider, ok := bc.(CDPContextProvider); ok {
		bs.InputGW.SetCDPContext(cdpProvider.CDPContext)
		bs.InputGW.SetCDPContextProvider(func context.Context {
			return cdpProvider.CDPContext
		})
	}
	if tracker := bc.GetTargetTracker; tracker != nil {
		bs.InputGW.SetCDPContextProvider(func context.Context {
			return tracker.GetActiveCDPContext
		})
		bs.InputGW.SetOnAcceptedInput(func(event *InputEvent) {
			tracker.RecordUserGesture(event)
		})
	}

	bs.mu.Lock
	alreadyAttached := bs.bcAttached
	bs.browserCore = bc
	bs.bcAttached = true
	bs.mu.Unlock
	if alreadyAttached {
		log.Printf("[BROWSER-SESSION] AttachBrowserCore: rebind canonical BrowserCore and refresh callback")
	} else {
		log.Printf("[BROWSER-SESSION] AttachBrowserCore: wrapping EnableTakeover/DisableTakeover callbacks")
	}

	// SEV-H1: 始终从 baseCallback 构建回调（不从当前回调包装，防止 Codex #1 链式增长）。
	// BrowserCore 可能因 profile/runtime 重启而变化；重复 attach 时必须替换闭包中的 core。
	base := bs.baseCallback
	bs.InputGW.SetOnStateChange(func {
		if base != nil {
			base
		}
		mode := bs.InputGW.Mode
		if mode == TakeoverModeTakeover {
			_ = bc.EnableTakeover(context.Background)
		} else {
			_ = bc.DisableTakeover(context.Background)
		}
	})

	// Codex R2 #2: 立即同步当前 takeover 状态到新 BrowserCore
	// preset restart 期间用户可能已在 takeover 模式，新 core 必须立刻知道
	if bs.InputGW.Mode == TakeoverModeTakeover {
		_ = bc.EnableTakeover(context.Background)
	}
}

// ResetAttach 重置 attach 状态 + 恢复原始回调（不含旧 BrowserCore 包装）。
// 必须在 preset switch (Chrome 重启) 后调用。
// Codex #1 修复: 恢复 baseCallback 而非在旧包装上再包装，防止回调链无限增长。
func (bs *BrowserSession) ResetAttach {
	bs.mu.Lock
	bs.browserCore = nil
	bs.bcAttached = false
	bs.mu.Unlock
	// 恢复原始回调，去掉旧 BrowserCore 的 EnableTakeover/DisableTakeover 包装
	if bs.baseCallback != nil {
		bs.InputGW.SetOnStateChange(bs.baseCallback)
	}
	log.Printf("[BROWSER-SESSION] ResetAttach: callback restored to base, ready for new BrowserCore")
}

// AttachCursorDetection 向 BrowserCore 注入 cursor 检测，将 cursor 变更广播到 Authority。
// 如果 bc 未实现 CursorDetector 接口则静默忽略（向后兼容）。
func (bs *BrowserSession) AttachCursorDetection(bc BrowserCore) {
	cd, ok := bc.(CursorDetector)
	if !ok {
		return
	}
	_ = cd.SetupCursorDetection(func(cursor string) {
		bs.Authority.UpdateCursor(cursor)
	})
}
