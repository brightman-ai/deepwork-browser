package browser

// InputGateway — 输入网关 [CAP-BS09-C3 r2, DDC-I-06, DDC-I-07]
//
// 替代 TakeoverController，增加:
//   - lease 校验（CAS 抢锁 + token 比对 + 过期检测）
//   - input-ack 响应（Seq 回传 + status + reason）
//   - pressedKeys/pressedBtns 跟踪（断连合成释放所需）
//   - 断连 grace timer（5s 内重连可恢复，超时 auto-release + releaseAll）
//   - idle timer（每次 accepted input 续租，超时 auto-release + releaseAll）

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand"
	"strings"
	"sync"
	"time"

	"github.com/brightman-ai/kit/obs"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// humanJitter 产生 Gaussian 分布的延迟 (均值 12ms, 标准差 5ms, 截断到 [3ms, 30ms])。
// 用于在每次 CDP input 事件前注入 timing 抖动,模拟真人输入节奏,
// 规避 Cloudflare Turnstile 的 behavior analysis (它能在 <100 事件内
// 区分"程序化零抖动"和"真人输入")。
//
// 分布选择依据:
//   - 真人按键间隔: 50-200ms 量级,但 CDP 事件粒度更细 (press/release 对)
//   - 3ms 下界避免 UI 仍可感知的阻塞
//   - 30ms 上界避免累计延迟过大 (100 事件 ≤ 3s)
//   - Gaussian 分布 > 均匀分布, 后者在大样本下方差过小仍被识别
func humanJitter() time.Duration {
	const mean = 12.0
	const stddev = 5.0
	ms := mrand.NormFloat64()*stddev + mean
	if ms < 3 {
		ms = 3
	}
	if ms > 30 {
		ms = 30
	}
	return time.Duration(ms * float64(time.Millisecond))
}

func inputLogContext() context.Context {
	return obs.WithStage(context.Background(), STGLiveView)
}

// ============================================================
// § 消息类型 [Ref: CAP-BS09-C3 r2]
// ============================================================

// InputMessage 是前端通过 WebSocket 发送的输入消息（接管模式）。
type InputMessage struct {
	Type  string     `json:"type"`  // 固定为 "input"
	Seq   int64      `json:"seq"`   // 客户端单调递增序号
	Lease string     `json:"lease"` // 当前租约令牌（必须与 leaseToken 匹配）
	Event InputEvent `json:"event"` // 具体输入事件
}

// InputAck 是服务端对 InputMessage 的 ACK 响应。
type InputAck struct {
	Type   string `json:"type"`             // 固定为 "input-ack"
	Seq    int64  `json:"seq"`              // 对应客户端 seq
	Status string `json:"status"`           // "accepted" / "rejected" / "error"
	Reason string `json:"reason,omitempty"` // 拒绝原因（仅 rejected/error 时填充）
}

// ============================================================
// § InputGateway 结构体
// ============================================================

// DefaultIdleTimeout 是默认 idle 超时（5分钟），每次 accepted input 后重置。
const DefaultIdleTimeout = 5 * time.Minute

// DefaultGraceTimeout 是断连 grace 超时（5秒），超时前重连可恢复。
const DefaultGraceTimeout = 5 * time.Second

// pressedKeyInfo 记录按住的键的完整元数据（合成 keyUp 时需要 Key 和 Code 分开）。
type pressedKeyInfo struct {
	Key  string // 逻辑键名（如 "a", "Enter", "Shift"）
	Code string // 物理键码（如 "KeyA", "Enter", "ShiftLeft"）
}

// pressedBtnInfo 记录按住的鼠标按钮元数据（合成 mouseReleased 需要最后坐标）。
type pressedBtnInfo struct {
	Button string  // 按钮名（"left"/"right"/"middle"）
	LastX  float64 // 最后一次 mousePressed 的 X 坐标
	LastY  float64 // 最后一次 mousePressed 的 Y 坐标
}

// InputGateway 输入网关 — 替代 TakeoverController [CAP-BS09-C3 r2, DDC-I-06, DDC-I-07]
type InputGateway struct {
	mu             sync.Mutex
	mode           TakeoverMode
	owner          string                                   // 当前 owner 的 connID
	leaseToken     string                                   // 当前 lease token（16字节随机 hex）
	leaseExpiry    time.Time                                // lease 过期时间（= 上次续租时间 + idleTimeout）
	idleTimeout    time.Duration                            // idle timeout（默认 5min）
	idleTimer      *time.Timer                              // idle timer — 每次 accepted input 续租
	graceTimeout   time.Duration                            // 断连 grace（默认 5s）
	graceTimer     *time.Timer                              // grace timer — 断连后启动
	pressedKeys    map[string]pressedKeyInfo                // 当前按住的键（code → {Key, Code}）
	pressedBtns    map[string]pressedBtnInfo                // 当前按住的鼠标按钮（button → {Button, LastX, LastY}）
	cdpCtx         context.Context                          // CDP 操作 context（可为 nil，由 SetCDPContext 延迟注入）
	cdpCtxProvider func() context.Context                   // 当前活跃 CDP context 解析器（优先于 cdpCtx 快照）
	onStateChange  func()                                   // 状态变化回调（通知 SessionAuthority 广播）
	onAccepted     func(event *InputEvent)                  // 已成功 dispatch 的 takeover 用户输入回调（用于 target 归属弱证据）
	dispatchMouse  func(context.Context, *InputEvent) error // test hook: 覆盖真实 mouse dispatch
	dispatchKey    func(context.Context, *InputEvent) error // test hook: 覆盖真实 keyboard dispatch
	dispatchTouch  func(context.Context, *InputEvent) error // test hook: 覆盖真实 touch dispatch
	recordBuffer   *RecordBuffer                            // 录制缓冲 (nil = 未启用, SK-D4)
}

// NewInputGateway 创建 InputGateway，初始状态为 OBSERVE。
//
// cdpCtx: chromedp 会话 context，用于 CDP dispatch。
// onStateChange: 模式变化时触发的回调（由 browser_session 注册，用于向前端广播状态）。
func NewInputGateway(cdpCtx context.Context, onStateChange func()) *InputGateway {
	return &InputGateway{
		mode:          TakeoverModeObserve,
		idleTimeout:   DefaultIdleTimeout,
		graceTimeout:  DefaultGraceTimeout,
		pressedKeys:   make(map[string]pressedKeyInfo),
		pressedBtns:   make(map[string]pressedBtnInfo),
		cdpCtx:        cdpCtx,
		onStateChange: onStateChange,
	}
}

// ============================================================
// § 公开方法
// ============================================================

// RequestTakeover CAS 抢锁，切换到 TAKEOVER 模式。
//
// 已有 owner 时返回 error，包含 currentOwner 和 leaseRemaining 信息。
// 成功返回 leaseToken 和 expiresAt（= now + idleTimeout）。
func (g *InputGateway) RequestTakeover(connID string) (leaseToken string, expiresAt time.Time, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// 如果已有 owner（且不是同一个 connID），拒绝抢锁
	if g.mode == TakeoverModeTakeover && g.owner != connID {
		remaining := time.Until(g.leaseExpiry)
		if remaining < 0 {
			remaining = 0
		}
		return "", time.Time{}, fmt.Errorf(
			"browser: takeover already held by %s (lease remaining: %s)",
			g.owner, remaining.Round(time.Second),
		)
	}

	// 生成新的 lease token
	token, genErr := generateLeaseToken()
	if genErr != nil {
		return "", time.Time{}, fmt.Errorf("browser: failed to generate lease token: %w", genErr)
	}

	expiry := time.Now().Add(g.idleTimeout)

	g.mode = TakeoverModeTakeover
	g.owner = connID
	g.leaseToken = token
	g.leaseExpiry = expiry

	// 重置 idle timer
	g.resetIdleTimerLocked()

	// 取消可能存在的 grace timer（重连抢锁场景）
	if g.graceTimer != nil {
		g.graceTimer.Stop()
		g.graceTimer = nil
	}

	// 通知状态变化
	go g.notifyStateChange()

	return token, expiry, nil
}

// ReleaseTakeover 主动释放接管，恢复 OBSERVE 模式，并合成释放所有按压输入。
//
// 只有当前 owner 才能释放。
func (g *InputGateway) ReleaseTakeover(connID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.mode != TakeoverModeTakeover {
		// 已经是 OBSERVE 模式，幂等操作
		return nil
	}
	if g.owner != connID {
		return fmt.Errorf("browser: release denied — caller %s is not the owner %s", connID, g.owner)
	}

	g.releaseAllLocked()
	g.clearTakeoverStateLocked()

	go g.notifyStateChange()
	return nil
}

// HandleInput 校验 lease → CDP dispatch → 返回 ACK → 续租。
//
// 校验顺序:
//  1. mode == TakeoverModeTakeover
//  2. connID == g.owner
//  3. msg.Lease == g.leaseToken
//  4. time.Now() < g.leaseExpiry
//
// 任何校验失败返回 rejected + reason。
// dispatch 成功返回 accepted，并在成功后续租（reset idle timer）。
func (g *InputGateway) HandleInput(connID string, msg *InputMessage) *InputAck {
	g.mu.Lock()

	// 校验 1: 模式
	if g.mode != TakeoverModeTakeover {
		if shouldLogInputEvent(&msg.Event) {
			logger.Warn(inputLogContext(), "input dispatch rejected",
				"seq", msg.Seq,
				"conn", shortLogID(connID),
				"reason", "not_in_takeover",
				"event", summarizeInputEvent(&msg.Event))
		}
		g.mu.Unlock()
		return &InputAck{Type: "input-ack", Seq: msg.Seq, Status: "rejected", Reason: "not in takeover mode"}
	}
	// 校验 2: owner
	if g.owner != connID {
		if shouldLogInputEvent(&msg.Event) {
			logger.Warn(inputLogContext(), "input dispatch rejected",
				"seq", msg.Seq,
				"conn", shortLogID(connID),
				"owner", shortLogID(g.owner),
				"reason", "not_owner",
				"event", summarizeInputEvent(&msg.Event))
		}
		g.mu.Unlock()
		return &InputAck{Type: "input-ack", Seq: msg.Seq, Status: "rejected", Reason: "not the current owner"}
	}
	// 校验 3: lease token
	if msg.Lease != g.leaseToken {
		if shouldLogInputEvent(&msg.Event) {
			logger.Warn(inputLogContext(), "input dispatch rejected",
				"seq", msg.Seq,
				"conn", shortLogID(connID),
				"reason", "invalid_lease",
				"event", summarizeInputEvent(&msg.Event))
		}
		g.mu.Unlock()
		return &InputAck{Type: "input-ack", Seq: msg.Seq, Status: "rejected", Reason: "invalid lease token"}
	}
	// 校验 4: lease 过期
	if time.Now().After(g.leaseExpiry) {
		if shouldLogInputEvent(&msg.Event) {
			logger.Warn(inputLogContext(), "input dispatch rejected",
				"seq", msg.Seq,
				"conn", shortLogID(connID),
				"reason", "lease_expired",
				"event", summarizeInputEvent(&msg.Event))
		}
		g.mu.Unlock()
		return &InputAck{Type: "input-ack", Seq: msg.Seq, Status: "rejected", Reason: "lease expired"}
	}

	g.mu.Unlock()

	// CDP dispatch（锁外执行，避免 CDP 阻塞持锁）
	dispatchStart := time.Now()
	logInput := shouldLogInputEvent(&msg.Event)
	if logInput {
		logger.Debug(inputLogContext(), "input dispatch start",
			"seq", msg.Seq,
			"conn", shortLogID(connID),
			"event", summarizeInputEvent(&msg.Event))
	}
	var dispatchErr error
	dispatchCtx := g.resolveCDPContext()
	switch msg.Event.Type {
	case "mouse":
		dispatchErr = g.dispatchMouseEvent(dispatchCtx, &msg.Event)
		if dispatchErr != nil {
			logger.Warn(inputLogContext(), "mouse dispatch error",
				"event", msg.Event.Event,
				"x", msg.Event.X,
				"y", msg.Event.Y,
				"button", msg.Event.Button,
				"error", dispatchErr)
		}
	case "keyboard":
		dispatchErr = g.dispatchKeyEvent(dispatchCtx, &msg.Event)
	case "touch":
		dispatchErr = g.dispatchTouchEvent(dispatchCtx, &msg.Event)
		if dispatchErr != nil {
			logger.Warn(inputLogContext(), "touch dispatch error",
				"event", msg.Event.Event,
				"points", len(msg.Event.TouchPoints),
				"error", dispatchErr)
		}
	}
	if dispatchErr != nil && isCanceledContextErr(dispatchErr) {
		retryCtx := g.resolveCDPContext()
		if retryCtx != nil && retryCtx.Err() == nil {
			if logInput {
				logger.Info(inputLogContext(), "input dispatch retry after context cancellation",
					"seq", msg.Seq,
					"conn", shortLogID(connID),
					"event", summarizeInputEvent(&msg.Event))
			}
			switch msg.Event.Type {
			case "mouse":
				dispatchErr = g.dispatchMouseEvent(retryCtx, &msg.Event)
			case "keyboard":
				dispatchErr = g.dispatchKeyEvent(retryCtx, &msg.Event)
			case "touch":
				dispatchErr = g.dispatchTouchEvent(retryCtx, &msg.Event)
			}
		}
	}

	if dispatchErr != nil {
		if logInput {
			logger.Warn(inputLogContext(), "input dispatch error",
				"seq", msg.Seq,
				"conn", shortLogID(connID),
				"duration_ms", time.Since(dispatchStart).Milliseconds(),
				"error", dispatchErr)
		}
		return &InputAck{Type: "input-ack", Seq: msg.Seq, Status: "error", Reason: dispatchErr.Error()}
	}
	if logInput {
		logger.Info(inputLogContext(), "input dispatch accepted",
			"seq", msg.Seq,
			"conn", shortLogID(connID),
			"event", summarizeInputEvent(&msg.Event),
			"duration_ms", time.Since(dispatchStart).Milliseconds())
	}

	g.mu.Lock()
	shouldNotify := false
	var onAccepted func(event *InputEvent)
	if g.mode == TakeoverModeTakeover && g.owner == connID && msg.Lease == g.leaseToken {
		oldExpiry := g.leaseExpiry
		g.leaseExpiry = time.Now().Add(g.idleTimeout)
		g.resetIdleTimerLocked()
		// 续租广播节流: leaseExpiry 变化超过 1s 才通知 Authority（避免每次输入都触发全员广播）
		shouldNotify = g.leaseExpiry.Sub(oldExpiry) > time.Second
		g.trackInputStateLocked(&msg.Event)
		onAccepted = g.onAccepted
	}
	g.mu.Unlock()

	if shouldNotify {
		go g.notifyStateChange()
	}
	if onAccepted != nil {
		onAccepted(&msg.Event)
	}

	// Recording tap — 零侵入，仅在 recording 时执行 (SK-D4, DDC-I-21)
	// 位于 accepted 路径末尾，与 onAccepted 回调并列，不阻塞主路径。
	if rb := g.recordBuffer; rb != nil && rb.IsRecording() {
		switch msg.Event.Type {
		case "mouse":
			rb.AppendMouseEvent(&msg.Event)
		case "keyboard":
			rb.AppendKeyEvent(&msg.Event)
		}
	}

	return &InputAck{Type: "input-ack", Seq: msg.Seq, Status: "accepted"}
}

// OnDisconnect 断连处理 — 启动 grace timer。
//
// 5s 内重连（OnReconnect）可取消 grace timer 恢复接管。
// 超时后 auto-release + 合成释放所有按压输入。
func (g *InputGateway) OnDisconnect(connID string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// 只有当前 owner 断连才启动 grace timer
	if g.mode != TakeoverModeTakeover || g.owner != connID {
		return
	}

	// 已有 grace timer 则不重复启动
	if g.graceTimer != nil {
		return
	}

	g.graceTimer = time.AfterFunc(g.graceTimeout, func() {
		g.mu.Lock()
		// 二次检查：grace 期间可能已被重连取消或被其他操作清理
		if g.mode == TakeoverModeTakeover && g.owner == connID {
			g.releaseAllLocked()
			g.clearTakeoverStateLocked()
			g.mu.Unlock()
			g.notifyStateChange()
		} else {
			g.mu.Unlock()
		}
	})
}

// OnReconnect 重连处理 — 取消 grace timer（如果 connID 匹配 owner）。
//
// 重连后 owner 可继续使用原有 lease token 发送输入。
func (g *InputGateway) OnReconnect(connID string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.mode != TakeoverModeTakeover || g.owner != connID {
		return
	}

	if g.graceTimer != nil {
		g.graceTimer.Stop()
		g.graceTimer = nil
	}
}

// Mode 返回当前接管模式。
func (g *InputGateway) Mode() TakeoverMode {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.mode
}

// Owner 返回当前 owner connID（OBSERVE 模式下返回空字符串）。
func (g *InputGateway) Owner() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.owner
}

// LeaseToken 返回当前 lease token（OBSERVE 模式下返回空字符串）。
func (g *InputGateway) LeaseToken() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.leaseToken
}

// LeaseExpiry 返回 lease 过期时间（OBSERVE 模式下返回零值）。
func (g *InputGateway) LeaseExpiry() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.leaseExpiry
}

func shortLogID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func shouldLogInputEvent(event *InputEvent) bool {
	if event == nil {
		return false
	}
	return !(event.Type == "mouse" && event.Event == "mouseMoved")
}

func summarizeInputEvent(event *InputEvent) string {
	if event == nil {
		return "nil"
	}
	switch event.Type {
	case "mouse":
		return fmt.Sprintf("mouse/%s x=%.0f y=%.0f button=%s click=%d delta=(%.0f,%.0f)",
			event.Event, event.X, event.Y, event.Button, event.ClickCount, event.DeltaX, event.DeltaY)
	case "keyboard":
		return fmt.Sprintf("keyboard/%s code=%s modifiers=%d text_len=%d",
			event.Event, event.Code, event.Modifiers, len(event.Text))
	case "touch":
		return fmt.Sprintf("touch/%s points=%d", event.Event, len(event.TouchPoints))
	default:
		return fmt.Sprintf("%s/%s", event.Type, event.Event)
	}
}

// SetCDPContext 延迟注入 Chrome CDP context（Chrome 启动后由 AttachBrowserCore 调用）。
// 允许 BrowserSession 在 Chrome 启动前创建，Chrome 就绪后再连接。
func (g *InputGateway) SetCDPContext(ctx context.Context) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cdpCtx = ctx
}

// SetCDPContextProvider 注册“当前活跃 Target 的 CDP context”解析器。
// InputGateway 在每次 dispatch 时动态取值，避免持有已经被取消的快照 ctx。
func (g *InputGateway) SetCDPContextProvider(fn func() context.Context) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cdpCtxProvider = fn
}

// GetOnStateChange 返回当前 onStateChange 回调（供 AttachBrowserCore 链式包装用）。
func (g *InputGateway) GetOnStateChange() func() {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.onStateChange
}

// SetOnStateChange 替换 onStateChange 回调（供 AttachBrowserCore 链式包装用）。
// 用于 SEV-H1: AttachBrowserCore 将原有回调包装后重新注入，实现 InputGateway → BrowserCore 状态同步。
func (g *InputGateway) SetOnStateChange(fn func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onStateChange = fn
}

// SetOnAcceptedInput 注册”已通过接管校验的真实用户输入”回调。
// 用于把输入链与 TargetTracker 的 target-claim 协议衔接起来，避免 liveview 侧靠 heuristic 猜 target 归属。
func (g *InputGateway) SetOnAcceptedInput(fn func(event *InputEvent)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onAccepted = fn
}

// SetRecordBuffer 注入录制缓冲区。nil 表示禁用录制 tap。
// Recording tap 在 dispatch 成功 (accepted) 后触发，零侵入主路径 (SK-D4)。
func (g *InputGateway) SetRecordBuffer(rb *RecordBuffer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.recordBuffer = rb
}

// GetRecordBuffer 返回当前注入的录制缓冲区（nil 表示未启用）。
func (g *InputGateway) GetRecordBuffer() *RecordBuffer {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.recordBuffer
}

// ============================================================
// § 内部方法（锁内调用，调用前必须持锁）
// ============================================================

// releaseAllLocked 合成释放所有当前按压的键和鼠标按钮（使用完整元数据）。
// 调用前必须持 g.mu 锁（CDP dispatch 本身在锁外执行，此处仅收集需释放的列表）。
func (g *InputGateway) releaseAllLocked() {
	// 收集需释放的列表（避免在锁内执行 CDP 操作）
	keysToRelease := make([]pressedKeyInfo, 0, len(g.pressedKeys))
	for _, info := range g.pressedKeys {
		keysToRelease = append(keysToRelease, info)
	}
	btnsToRelease := make([]pressedBtnInfo, 0, len(g.pressedBtns))
	for _, info := range g.pressedBtns {
		btnsToRelease = append(btnsToRelease, info)
	}

	// 清空跟踪状态
	g.pressedKeys = make(map[string]pressedKeyInfo)
	g.pressedBtns = make(map[string]pressedBtnInfo)

	// 异步 CDP dispatch（锁外执行）
	if len(keysToRelease) > 0 || len(btnsToRelease) > 0 {
		go func() {
			dispatchCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			// Key 和 Code 分开存储，合成 keyUp 时使用正确的元数据
			for _, info := range keysToRelease {
				_ = g.dispatchKeyEvent(dispatchCtx, &InputEvent{
					Type:  "keyboard",
					Event: "keyUp",
					Key:   info.Key,
					Code:  info.Code,
				})
			}
			// mouseReleased 携带最后一次按下的坐标
			for _, info := range btnsToRelease {
				_ = g.dispatchMouseEvent(dispatchCtx, &InputEvent{
					Type:   "mouse",
					Event:  "mouseReleased",
					Button: info.Button,
					X:      info.LastX,
					Y:      info.LastY,
				})
			}
		}()
	}
}

// clearTakeoverStateLocked 清除接管状态，恢复 OBSERVE 模式。
// 调用前必须持 g.mu 锁。
func (g *InputGateway) clearTakeoverStateLocked() {
	g.mode = TakeoverModeObserve
	g.owner = ""
	g.leaseToken = ""
	g.leaseExpiry = time.Time{}

	if g.idleTimer != nil {
		g.idleTimer.Stop()
		g.idleTimer = nil
	}
	if g.graceTimer != nil {
		g.graceTimer.Stop()
		g.graceTimer = nil
	}
	// 重置 pressed 状态跟踪
	g.pressedKeys = make(map[string]pressedKeyInfo)
	g.pressedBtns = make(map[string]pressedBtnInfo)
}

// resetIdleTimerLocked 重置 idle timer（每次 accepted input 调用）。
// 调用前必须持 g.mu 锁。
func (g *InputGateway) resetIdleTimerLocked() {
	if g.idleTimer != nil {
		g.idleTimer.Stop()
	}
	// 捕获当前 owner，用于 timer 触发时的幂等校验
	owner := g.owner
	g.idleTimer = time.AfterFunc(g.idleTimeout, func() {
		g.mu.Lock()
		if g.mode == TakeoverModeTakeover && g.owner == owner {
			g.releaseAllLocked()
			g.clearTakeoverStateLocked()
			g.mu.Unlock()
			g.notifyStateChange()
		} else {
			g.mu.Unlock()
		}
	})
}

// trackInputStateLocked 根据输入事件更新 pressedKeys/pressedBtns 跟踪状态（完整元数据）。
// 调用前必须持 g.mu 锁。
func (g *InputGateway) trackInputStateLocked(event *InputEvent) {
	switch event.Type {
	case "mouse":
		switch event.Event {
		case "mousePressed":
			if event.Button != "" && event.Button != "none" {
				// 记录按钮名 + 按下坐标（合成 mouseReleased 时恢复坐标）
				g.pressedBtns[event.Button] = pressedBtnInfo{
					Button: event.Button,
					LastX:  event.X,
					LastY:  event.Y,
				}
			}
		case "mouseReleased":
			delete(g.pressedBtns, event.Button)
		}
	case "keyboard":
		switch event.Event {
		case "keyDown":
			if event.Code != "" {
				// 以 Code 为 key，分开存储 Key 和 Code（合成 keyUp 时使用正确的 Key 逻辑名）
				g.pressedKeys[event.Code] = pressedKeyInfo{
					Key:  event.Key,
					Code: event.Code,
				}
			}
		case "keyUp":
			delete(g.pressedKeys, event.Code)
		}
	}
}

// notifyStateChange 触发状态变化回调（锁外调用）。
func (g *InputGateway) notifyStateChange() {
	if g.onStateChange != nil {
		g.onStateChange()
	}
}

// ============================================================
// § CDP Dispatch（复用自 takeover_controller.go）
// ============================================================

// dispatchMouseEvent 分发鼠标事件到 CDP — 支持 click/move/scroll/dblclick。
// Cloudflare 反爬终局: 每次 dispatch 前 Gaussian 抖动 + mousePressed 前
// 插入 1-2px 坐标漂移的 mouseMoved (模拟真人落点微调)。
func (g *InputGateway) dispatchMouseEvent(dispatchCtx context.Context, event *InputEvent) error {
	_, err := g.dispatchMouseEventWithAckBudget(dispatchCtx, event, 0)
	return err
}

// dispatchMouseEventWithAckBudget 是 dispatchMouseEvent 的限时版本：最多等 budget
// 拿渲染器 ack，返回 acked=false 表示事件已按序送达但 ack 未到。
//
// budget<=0 时：滚轮取默认上界 wheelAckPulseCap（滚轮的 ack 等待没有上界，见
// action_engine.go 中 wheelGestureAckBudget 处的根因说明），其余事件保持既有的
// "等到 ack 为止"语义 —— 点击/按键的调用方依赖 ack 来确认输入被消费。
func (g *InputGateway) dispatchMouseEventWithAckBudget(dispatchCtx context.Context, event *InputEvent, budget time.Duration) (bool, error) {
	if g.dispatchMouse != nil {
		if budget > 0 {
			return awaitDispatchWithBudget(dispatchCtx, budget, func() error {
				return g.dispatchMouse(dispatchCtx, event)
			})
		}
		return true, g.dispatchMouse(dispatchCtx, event)
	}
	if dispatchCtx == nil {
		return false, fmt.Errorf("no CDP context available")
	}

	// mouseWheel 是特殊类型
	if event.Event == "mouseWheel" {
		wheelBudget := budget
		if wheelBudget <= 0 {
			wheelBudget = wheelAckPulseCap
		}
		time.Sleep(humanJitter())
		return dispatchInputWithAckBudget(dispatchCtx, wheelBudget,
			chromedp.ActionFunc(func(ctx context.Context) error {
				return input.DispatchMouseEvent(input.MouseWheel, event.X, event.Y).
					WithDeltaX(event.DeltaX).
					WithDeltaY(event.DeltaY).
					Do(ctx)
			}),
		)
	}

	var mouseType input.MouseType
	switch event.Event {
	case "mousePressed":
		mouseType = input.MousePressed
	case "mouseReleased":
		mouseType = input.MouseReleased
	case "mouseMoved":
		mouseType = input.MouseMoved
	default:
		mouseType = input.MouseMoved
	}

	var button input.MouseButton
	switch event.Button {
	case "left":
		button = input.Left
	case "right":
		button = input.Right
	case "middle":
		button = input.Middle
	default:
		button = input.None
	}

	clickCount := event.ClickCount
	if clickCount == 0 && mouseType == input.MousePressed {
		clickCount = 1
	}

	// buttons 位掩码 — headed Chrome (Wayland) 严格要求此参数
	// mousePressed 时必须标记按钮按下 (left=1, right=2, middle=4)
	// mouseReleased/mouseMoved 时 buttons=0 (无按钮按下)
	var buttons int64
	if mouseType == input.MousePressed {
		switch button {
		case input.Left:
			buttons = 1
		case input.Right:
			buttons = 2
		case input.Middle:
			buttons = 4
		}
	}

	time.Sleep(humanJitter())

	action := chromedp.ActionFunc(func(ctx context.Context) error {
		return input.DispatchMouseEvent(mouseType, event.X, event.Y).
			WithButton(button).
			WithButtons(buttons).
			WithClickCount(int64(clickCount)).
			Do(ctx)
	})
	if budget > 0 {
		return dispatchInputWithAckBudget(dispatchCtx, budget, action)
	}
	return true, chromedp.Run(dispatchCtx, action)
}

// keyVirtualCodeMap 将 key 名映射到 Windows Virtual Key Code。
// CDP Input.dispatchKeyEvent 需要此字段才能让百度等依赖 keyCode/which 的网站正确识别特殊键。
// 参考: https://developer.mozilla.org/en-US/docs/Web/API/UI_Events/Keyboard_event_key_values
var keyVirtualCodeMap = map[string]int{
	"Backspace":   8,
	"Tab":         9,
	"Enter":       13,
	"Shift":       16,
	"Control":     17,
	"Alt":         18,
	"Pause":       19,
	"CapsLock":    20,
	"Escape":      27,
	" ":           32, // Space
	"PageUp":      33,
	"PageDown":    34,
	"End":         35,
	"Home":        36,
	"ArrowLeft":   37,
	"ArrowUp":     38,
	"ArrowRight":  39,
	"ArrowDown":   40,
	"Insert":      45,
	"Delete":      46,
	"Meta":        91, // Windows/Command 键
	"ContextMenu": 93,
	"F1":          112,
	"F2":          113,
	"F3":          114,
	"F4":          115,
	"F5":          116,
	"F6":          117,
	"F7":          118,
	"F8":          119,
	"F9":          120,
	"F10":         121,
	"F11":         122,
	"F12":         123,
}

// getVirtualKeyCode 返回 key 对应的 Windows Virtual Key Code。
// 对于字母键 a-z/A-Z，返回大写字母的 ASCII 码 (65-90)。
// 对于数字键 0-9，返回 ASCII 码 (48-57)。
// 未知键返回 0（CDP 不设置此字段）。
func getVirtualKeyCode(key string) int {
	if code, ok := keyVirtualCodeMap[key]; ok {
		return code
	}
	if len(key) == 1 {
		ch := key[0]
		if ch >= 'a' && ch <= 'z' {
			return int(ch - 'a' + 'A') // 小写字母映射到大写 ASCII
		}
		if ch >= 'A' && ch <= 'Z' {
			return int(ch)
		}
		if ch >= '0' && ch <= '9' {
			return int(ch)
		}
	}
	return 0
}

// dispatchKeyEvent 分发键盘事件到 CDP。
// 同时设置 windowsVirtualKeyCode / nativeVirtualKeyCode，确保百度等
// 依赖 keyCode/which 属性的网站能正确识别 Ctrl+A / Backspace / Delete 等特殊键。
func (g *InputGateway) dispatchKeyEvent(dispatchCtx context.Context, event *InputEvent) error {
	if g.dispatchKey != nil {
		return g.dispatchKey(dispatchCtx, event)
	}
	if dispatchCtx == nil {
		return fmt.Errorf("no CDP context available")
	}

	if event.Event == "insertText" {
		if event.Text == "" {
			return nil
		}
		return chromedp.Run(dispatchCtx,
			chromedp.ActionFunc(func(ctx context.Context) error {
				return input.InsertText(event.Text).Do(ctx)
			}),
		)
	}

	var keyType input.KeyType
	switch event.Event {
	case "keyDown":
		keyType = input.KeyDown
	case "keyUp":
		keyType = input.KeyUp
	case "char":
		keyType = input.KeyChar
	default:
		keyType = input.KeyDown
	}

	vkCode := getVirtualKeyCode(event.Key)

	// Cloudflare 反爬: Gaussian 抖动模拟真人按键 timing
	time.Sleep(humanJitter())

	return chromedp.Run(dispatchCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			cmd := input.DispatchKeyEvent(keyType).
				WithKey(event.Key).
				WithCode(event.Code).
				WithText(event.Text).
				WithModifiers(input.Modifier(event.Modifiers))

			if vkCode > 0 {
				// 设置 windowsVirtualKeyCode + nativeVirtualKeyCode
				// 网页通过 e.keyCode / e.which 读取这些值
				cmd = cmd.
					WithWindowsVirtualKeyCode(int64(vkCode)).
					WithNativeVirtualKeyCode(int64(vkCode))
			}

			return cmd.Do(ctx)
		}),
	)
}

// dispatchTouchEvent 分发触摸事件到 CDP。
// 用于移动 profile (preset.Touch=true) 的真实触摸模拟。
func (g *InputGateway) dispatchTouchEvent(dispatchCtx context.Context, event *InputEvent) error {
	if g.dispatchTouch != nil {
		return g.dispatchTouch(dispatchCtx, event)
	}
	if dispatchCtx == nil {
		return fmt.Errorf("no CDP context available")
	}

	var touchType input.TouchType
	switch event.Event {
	case "touchStart":
		touchType = input.TouchStart
	case "touchMove":
		touchType = input.TouchMove
	case "touchEnd":
		touchType = input.TouchEnd
	case "touchCancel":
		touchType = input.TouchCancel
	default:
		touchType = input.TouchStart
	}

	// 构建 CDP touchPoints
	points := make([]*input.TouchPoint, 0, len(event.TouchPoints))
	for _, tp := range event.TouchPoints {
		points = append(points, &input.TouchPoint{
			X:  tp.X,
			Y:  tp.Y,
			ID: float64(tp.ID),
		})
	}

	time.Sleep(humanJitter())

	return chromedp.Run(dispatchCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return input.DispatchTouchEvent(touchType, points).Do(ctx)
		}),
	)
}

func (g *InputGateway) resolveCDPContext() context.Context {
	g.mu.Lock()
	provider := g.cdpCtxProvider
	fallback := g.cdpCtx
	g.mu.Unlock()

	if provider != nil {
		if ctx := provider(); ctx != nil && ctx.Err() == nil {
			return ctx
		}
	}
	if fallback != nil && fallback.Err() == nil {
		return fallback
	}
	if provider != nil {
		return provider()
	}
	return fallback
}

func isCanceledContextErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.Canceled) || strings.Contains(err.Error(), context.Canceled.Error())
}

// ============================================================
// § 工具函数
// ============================================================

// generateLeaseToken 用 crypto/rand 生成 16 字节随机 hex 字符串（32 字符）。
func generateLeaseToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
