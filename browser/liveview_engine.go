package browser

import (
	"context"
	"encoding/base64"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// ============================================================
// § LiveViewEngine
// ============================================================

// liveViewEngine 实现 Screencast 帧推送（事件驱动 + 即时 ACK）。
type liveViewEngine struct {
	mu sync.RWMutex
	ctx context.Context
	hub *FrameBroadcastHub
	lastFrameTime time.Time
	viewportW int // Screencast 分辨率跟随 preset [mobile fix #4]
	viewportH int
	registeredCtxSet map[context.Context]bool // Codex R2 #3: 防止 SwitchTarget 重复注册 listener
	switchSeq atomic.Uint64
	switchStartedAt time.Time
	switchLogSeq uint64
	activeTargetID string
}

// newLiveViewEngine 创建 LiveViewEngine 实例。
func newLiveViewEngine(viewportW, viewportH int) *liveViewEngine {
	if viewportW <= 0 {
		viewportW = 1920
	}
	if viewportH <= 0 {
		viewportH = 1080
	}
	return &liveViewEngine{viewportW: viewportW, viewportH: viewportH, registeredCtxSet: make(map[context.Context]bool)}
}

func (e *liveViewEngine) bindTargetListener(ctx context.Context) {
	if ctx == nil {
		return
	}

	e.mu.Lock
	if e.registeredCtxSet[ctx] {
		e.mu.Unlock
		return
	}
	e.registeredCtxSet[ctx] = true
	e.mu.Unlock

	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch event := ev.(type) {
		case *page.EventScreencastFrame:
			e.handleFrameFrom(ctx, event)
		}
	})
}

func (e *liveViewEngine) ackFrame(ctx context.Context, sessionID int64) {
	if ctx == nil {
		return
	}
	go func {
		_ = chromedp.Run(ctx, page.ScreencastFrameAck(sessionID))
	}
}

// StartScreencast 启动 Screencast 帧推送，将帧 fan-out 到 hub 。
//
// 关键约束:
// - 每帧必须立即 ACK（Page.ScreencastFrameAck），不等前端消费 
// - 帧通过 FrameBroadcastHub.Publish 广播，subscriber channel 满时保新丢旧 
func (e *liveViewEngine) StartScreencast(ctx context.Context, hub *FrameBroadcastHub) error {
	e.mu.Lock
	e.ctx = ctx
	e.hub = hub
	e.lastFrameTime = time.Time{}
	viewportW := e.viewportW
	viewportH := e.viewportH
	e.mu.Unlock
	logger.Info(targetLogContext, "liveview screencast start requested"
		"viewport_w", viewportW
		"viewport_h", viewportH)

	// 注册 Screencast 帧事件监听（去重：同一 context 只注册一次）
	e.bindTargetListener(ctx)

	// 启动 Screencast
	err := runCDPWithSoftTimeout(ctx, 5*time.Second, page.StartScreencast.
		WithFormat(page.ScreencastFormatJpeg).
		WithQuality(80).
		WithMaxWidth(int64(viewportW)).
		WithMaxHeight(int64(viewportH)).
		WithEveryNthFrame(1)
	)
	if err != nil {
		logger.Warn(targetLogContext, "liveview screencast start failed"
			"viewport_w", viewportW
			"viewport_h", viewportH
			"error", err)
		return err
	}

	logger.Info(targetLogContext, "liveview screencast start completed"
		"viewport_w", viewportW
		"viewport_h", viewportH)
	return nil
}

// handleFrame 处理 Screencast 帧事件。
// 关键: 立即 ACK，不可等前端消费 。
// 帧速率限制: 最小间隔 100ms (≈10fps) 防止 burst 压垮 WS 
// 远程浏览器场景 10fps 足够（Chrome Remote Desktop 也是 ~10fps），节省 CPU 编码开销
func (e *liveViewEngine) handleFrameFrom(sourceCtx context.Context, event *page.EventScreencastFrame) {
	e.mu.Lock
	activeCtx := e.ctx
	if activeCtx == nil || sourceCtx != activeCtx {
		e.mu.Unlock
		e.ackFrame(sourceCtx, event.SessionID)
		return
	}

	now := time.Now
	switchStartedAt := e.switchStartedAt
	switchSeq := e.switchLogSeq
	targetID := e.activeTargetID
	if now.Sub(e.lastFrameTime) < 100*time.Millisecond {
		// 帧间隔太短，跳过（仍然 ACK 告诉 Chrome 继续）
		e.mu.Unlock
		e.ackFrame(sourceCtx, event.SessionID)
		return
	}
	e.lastFrameTime = now
	if !switchStartedAt.IsZero {
		e.switchStartedAt = time.Time{}
		e.switchLogSeq = 0
	}
	hub := e.hub
	e.mu.Unlock
	if !switchStartedAt.IsZero {
		logger.Info(targetLogContext, "liveview switch first screencast frame"
			"switch_seq", switchSeq
			"elapsed_ms", time.Since(switchStartedAt).Milliseconds)
	}

	// event.Data 是 base64 编码的 JPEG 字符串，解码为 byte
	imgData, _ := base64.StdEncoding.DecodeString(event.Data)

	var tsMs int64
	if event.Metadata != nil && event.Metadata.Timestamp != nil {
		tsMs = event.Metadata.Timestamp.Time.UnixMilli
	} else {
		tsMs = time.Now.UnixMilli
	}

	// 将帧广播给所有 subscriber（保新丢旧，由 hub 管理）
	frame := &ScreencastFrame{
		Data: imgData
		Timestamp: tsMs
		SessionID: int64(event.SessionID)
		TargetID: targetID
	}
	if hub != nil {
		hub.Publish(frame)
		browserLiveViewFrames.Inc
	}

	// 立即 ACK — 在 goroutine 中执行，不阻塞当前帧处理 
	e.ackFrame(sourceCtx, event.SessionID)
}

// RestartScreencast 仅重启 Screencast CDP 命令（不重复注册 ListenTarget）。
// 用于 viewport 变更后更新分辨率。避免 StartScreencast 中 ListenTarget 累积泄漏 (Codex #4)。
func (e *liveViewEngine) RestartScreencast(ctx context.Context) error {
	e.mu.Lock
	e.ctx = ctx
	e.lastFrameTime = time.Time{}
	viewportW := e.viewportW
	viewportH := e.viewportH
	e.mu.Unlock
	e.bindTargetListener(ctx)
	_ = runCDPWithSoftTimeout(ctx, 3*time.Second, page.StopScreencast)
	return runCDPWithSoftTimeout(ctx, 5*time.Second, page.StartScreencast.
		WithFormat(page.ScreencastFormatJpeg).
		WithQuality(80).
		WithMaxWidth(int64(viewportW)).
		WithMaxHeight(int64(viewportH)).
		WithEveryNthFrame(1)
	)
}

// StopScreencast 停止 Screencast。
// subscriber channel 生命周期由 hub 管理（Unsubscribe 时关闭），此处不操作 hub。
func (e *liveViewEngine) StopScreencast(ctx context.Context) error {
	e.mu.Lock
	if e.ctx == ctx {
		e.ctx = nil
	}
	e.lastFrameTime = time.Time{}
	e.mu.Unlock
	return runCDPWithSoftTimeout(ctx, 3*time.Second, page.StopScreencast)
}

// SwitchTarget 切换 Screencast 到新 Target 。
// 状态切换必须是同步且快速的；CDP Stop/Start 在后台执行。
// 这样 tab click 不会等待旧 target stop、新 target session warm 或页面加载。
func (e *liveViewEngine) SwitchTarget(oldCtx, newCtx context.Context, targetID string) {
	started := time.Now
	seq := e.switchSeq.Add(1)
	e.mu.Lock
	e.ctx = newCtx
	e.activeTargetID = targetID
	e.lastFrameTime = time.Time{}
	e.switchStartedAt = started
	e.switchLogSeq = seq
	viewportW := e.viewportW
	viewportH := e.viewportH
	hub := e.hub
	e.mu.Unlock

	logger.Info(targetLogContext, "liveview target switch queued"
		"switch_seq", seq
		"target_id", targetID
		"viewport_w", viewportW
		"viewport_h", viewportH
		"has_old_ctx", oldCtx != nil
		"has_new_ctx", newCtx != nil)
	// 1. 立即注册新 target listener。旧 target 的后续帧会因 sourceCtx != activeCtx
	// 被 ACK 但不会发布，避免 tab 快速切换时串帧。
	e.bindTargetListener(newCtx)

	// 2. 立即抓取一帧静态截图并发布。Screencast 依赖页面 repaint；
	// ChromeInitialPageURL、已静止页面或后台 target 经常不会立刻产出新帧，前端就会卡在
	// "正在显示标签页..."。这条首帧路径与后续 Screencast 共用 FrameHub，
	// 只在 seq/context 仍然是当前活跃 target 时发布，避免快速切换串帧。
	go e.publishImmediateFrame(newCtx, hub, seq, started, targetID)

	// 3. 停止旧 Target（忽略错误 — Target 可能已关闭），后台执行避免阻塞 UX。
	if oldCtx != nil {
		go func(ctx context.Context, switchSeq uint64) {
			if err := runCDPWithSoftTimeout(ctx, 1500*time.Millisecond, page.StopScreencast); err != nil {
				logger.Debug(targetLogContext, "liveview old screencast stop skipped"
					"switch_seq", switchSeq
					"error", err)
			}
		}(oldCtx, seq)
	}

	// 4. 启动新 Target 的 Screencast，后台执行。若用户已切到别的 tab，
	// 启动成功后立即停止这个过期 target 的 screencast，避免后台泄漏。
	go func(ctx context.Context, w, h int, switchSeq uint64) {
		if ctx == nil {
			return
		}
		start := time.Now
		if err := runCDPWithSoftTimeout(ctx, 3500*time.Millisecond, page.StartScreencast.
			WithFormat(page.ScreencastFormatJpeg).
			WithQuality(80).
			WithMaxWidth(int64(w)).
			WithMaxHeight(int64(h)).
			WithEveryNthFrame(1)
		); err != nil {
			logger.Warn(targetLogContext, "liveview switch screencast start failed"
				"switch_seq", switchSeq
				"elapsed_ms", time.Since(start).Milliseconds
				"error", err)
			return
		}
		e.mu.RLock
		activeCtx := e.ctx
		e.mu.RUnlock
		if activeCtx != ctx {
			_ = runCDPWithSoftTimeout(ctx, 1500*time.Millisecond, page.StopScreencast)
			logger.Info(targetLogContext, "liveview switch screencast stale stopped"
				"switch_seq", switchSeq
				"elapsed_ms", time.Since(start).Milliseconds)
			return
		}
		logger.Info(targetLogContext, "liveview switch screencast start completed"
			"switch_seq", switchSeq
			"elapsed_ms", time.Since(start).Milliseconds)
	}(newCtx, viewportW, viewportH, seq)
}

func (e *liveViewEngine) publishImmediateFrame(ctx context.Context, hub *FrameBroadcastHub, seq uint64, switchStartedAt time.Time, targetID string) {
	if ctx == nil || hub == nil {
		return
	}
	start := time.Now
	var imgData byte
	err := runCDPWithSoftTimeout(ctx, 1500*time.Millisecond, chromedp.ActionFunc(func(actCtx context.Context) error {
		var e error
		imgData, e = page.CaptureScreenshot.
			WithFormat(page.CaptureScreenshotFormatJpeg).
			WithQuality(80).
			Do(actCtx)
		return e
	}))
	if err != nil {
		logger.Warn(targetLogContext, "liveview switch immediate frame failed"
			"switch_seq", seq
			"elapsed_ms", time.Since(start).Milliseconds
			"error", err)
		return
	}
	if len(imgData) == 0 {
		logger.Warn(targetLogContext, "liveview switch immediate frame empty"
			"switch_seq", seq
			"elapsed_ms", time.Since(start).Milliseconds)
		return
	}
	e.mu.RLock
	activeCtx := e.ctx
	activeSeq := e.switchSeq.Load
	e.mu.RUnlock
	if activeCtx != ctx || activeSeq != seq {
		logger.Info(targetLogContext, "liveview switch immediate frame stale"
			"switch_seq", seq
			"active_switch_seq", activeSeq
			"elapsed_ms", time.Since(start).Milliseconds)
		return
	}
	hub.Publish(&ScreencastFrame{
		Data: imgData
		Timestamp: time.Now.UnixMilli
		TargetID: targetID
	})
	browserLiveViewFrames.Inc
	logger.Info(targetLogContext, "liveview switch immediate frame published"
		"switch_seq", seq
		"capture_ms", time.Since(start).Milliseconds
		"switch_elapsed_ms", time.Since(switchStartedAt).Milliseconds
		"bytes", len(imgData))
}

// ============================================================
// § MockLiveViewEngine — 用于单元测试
// ============================================================

// mockLiveViewEngine 是用于测试的 mock 实现，记录 ACK 次数。
type mockLiveViewEngine struct {
	ackCount int
	hub *FrameBroadcastHub
}

// newMockLiveViewEngine 创建 mock LiveViewEngine，注入 hub。
func newMockLiveViewEngine(hub *FrameBroadcastHub) *mockLiveViewEngine {
	return &mockLiveViewEngine{
		hub: hub
	}
}

// PushFrame 推送模拟帧到 hub（测试用，模拟 ACK）。
func (m *mockLiveViewEngine) PushFrame(frame *ScreencastFrame) {
	// 立即 ACK（模拟 ）
	m.ackCount++

	if m.hub != nil {
		m.hub.Publish(frame)
	}
}

// AckCount 返回 ACK 次数（测试验证用）。
func (m *mockLiveViewEngine) AckCount int {
	return m.ackCount
}
