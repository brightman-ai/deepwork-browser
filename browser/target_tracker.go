package browser

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/brightman-ai/kit/obs"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// ============================================================
// § TargetTracker — 多 Target 自动跟随 
// ============================================================
//
// 当用户在 LiveView 接管模式点击 target=_blank 链接或触发 window.open 时，
// Chrome 创建新 Target（标签页）。CDP Screencast 绑定单一 Target（ 基岩），
// 不会自动跟随。TargetTracker 检测新 Target 并切换 Screencast，实现自动跟随。

// trackedTarget 存储一个 CDP Target 的元数据和 chromedp context。
type trackedTarget struct {
	ID target.ID
	URL string
	Title string
	OpenerID target.ID
	Ctx context.Context
	Cancel context.CancelFunc
	Created time.Time
	Closable bool
}

type gestureClaim struct {
	SourceID target.ID
	Created time.Time
}

type windowOpenHint struct {
	SourceID target.ID
	URL string
	WindowName string
	Created time.Time
}

// JavaScriptDialogEvent describes a page-level alert/confirm/prompt that must
// be surfaced through LiveView when Chrome itself is running on a virtual display.
type JavaScriptDialogEvent struct {
	ID string `json:"id"`
	TargetID string `json:"target_id"`
	URL string `json:"url"`
	FrameID string `json:"frame_id"`
	Message string `json:"message"`
	Type string `json:"type"`
	DefaultPrompt string `json:"default_prompt,omitempty"`
}

// TargetTracker 管理所有已知的 page Target，自动跟随活跃 Target 的 Screencast。
type TargetTracker struct {
	mu sync.RWMutex
	browserCtx context.Context // 原始 browser context（创建子 context 的 parent）
	primaryID target.ID // browserCtx 对应的 primary target；对外必须呈现真实 CDP target.ID
	targets map[target.ID]*trackedTarget
	activeID target.ID // 当前 foreground/screencast 的真实 TargetID；空只表示尚未识别
	order target.ID // 插入顺序，用于关闭时回退
	pendingSwitch map[target.ID]bool // window.open('') pattern: 等待真实 URL 后切换
	liveEngine *liveViewEngine
	hub *FrameBroadcastHub
	onSwitch func(url, title string, targetCount int) // 切换回调 → SessionAuthority 更新
	onCDPSwitch func(newCtx context.Context) // CDP context 切换回调 → InputGateway 更新
	onDialog func(JavaScriptDialogEvent) // JS dialog bridge → WebUI modal
	foregroundGuard func(target.ID, string) error // fail-closed guard before Target.activateTarget/Page.bringToFront
	closeTarget func(target.ID) error // Target close strategy; BrowserMuxHost uses DevTools HTTP close
	gestureClaims gestureClaim // 本地 takeover 输入成功 dispatch 后的弱证据兜底
	windowHints windowOpenHint // CDP Page.windowOpen(userGesture=true) 强证据
	pageListeners map[string]bool // 每个 target 只绑定一次 Page.windowOpen listener
}

func (tt *TargetTracker) totalTabCountLocked int {
	return len(tt.targets)
}

func (tt *TargetTracker) activeNavigationLocked (string, string) {
	activeID := tt.activeID
	if activeID == "" {
		activeID = tt.primaryID
	}
	if tracked := tt.targets[activeID]; tracked != nil {
		return tracked.URL, tracked.Title
	}
	return "", ""
}

func (tt *TargetTracker) fallbackActiveTargetLocked target.ID {
	for i := len(tt.order) - 1; i >= 0; i-- {
		id := tt.order[i]
		if tt.targets[id] != nil {
			return id
		}
	}
	return ""
}

func targetLogContext context.Context {
	return obs.WithStage(context.Background, STGLiveView)
}

func targetIDInOrder(order target.ID, id target.ID) bool {
	for _, existing := range order {
		if existing == id {
			return true
		}
	}
	return false
}

func removeTargetFromOrder(order target.ID, id target.ID) target.ID {
	for i, existing := range order {
		if existing == id {
			return append(order[:i], order[i+1:]...)
		}
	}
	return order
}

func (tt *TargetTracker) addTargetOrderLocked(id target.ID, front bool) {
	if id == "" || targetIDInOrder(tt.order, id) {
		return
	}
	if front {
		tt.order = append(target.ID{id}, tt.order...)
		return
	}
	tt.order = append(tt.order, id)
}

func runCDPWithSoftTimeout(ctx context.Context, timeout time.Duration, actions ...chromedp.Action) error {
	done := make(chan error, 1)
	go func {
		done <- chromedp.Run(ctx, actions...)
	}
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("cdp operation timed out after %s", timeout)
	}
}

func runBrowserCDPWithSoftTimeout(ctx context.Context, timeout time.Duration, fn func(context.Context) error) error {
	c := chromedp.FromContext(ctx)
	if c == nil || c.Browser == nil {
		return fmt.Errorf("browser executor unavailable")
	}
	opCtx, cancel := context.WithTimeout(context.Background, timeout)
	defer cancel
	return fn(cdp.WithExecutor(opCtx, c.Browser))
}

func warmTargetContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := runCDPWithSoftTimeout(ctx, TargetWarmTimeout); err != nil {
		logger.Warn(targetLogContext, "warm target context failed", "error", err)
		return err
	}
	return nil
}

func targetListenerKey(targetID target.ID) string {
	if targetID == "" {
		return "__unknown_target__"
	}
	return string(targetID)
}

// NewTargetTracker 创建 TargetTracker。initialID 是 Chrome 启动时的第一个 tab。
func NewTargetTracker(browserCtx context.Context) *TargetTracker {
	tt := &TargetTracker{
		browserCtx: browserCtx
		targets: make(map[target.ID]*trackedTarget)
		pendingSwitch: make(map[target.ID]bool)
		pageListeners: make(map[string]bool)
	}
	if browserCtx != nil {
		if cdpCtx := chromedp.FromContext(browserCtx); cdpCtx != nil && cdpCtx.Target != nil {
			tt.primaryID = cdpCtx.Target.TargetID
			tt.activeID = tt.primaryID
			tt.targets[tt.primaryID] = &trackedTarget{
				ID: tt.primaryID
				Ctx: browserCtx
				Created: time.Now
				Closable: false
			}
			tt.order = append(tt.order, tt.primaryID)
		}
	}
	return tt
}

// SetLiveEngine 注入 liveViewEngine 和 hub（在 StartLiveView 后调用）。
func (tt *TargetTracker) SetLiveEngine(engine *liveViewEngine, hub *FrameBroadcastHub) {
	tt.mu.Lock
	defer tt.mu.Unlock
	tt.liveEngine = engine
	tt.hub = hub
	logger.Info(targetLogContext, "target tracker liveview attached"
		"has_engine", engine != nil
		"has_hub", hub != nil
		"primary_target_id", tt.primaryID
		"active_target_id", tt.activeID
		"target_count", tt.totalTabCountLocked)
}

// SetOnSwitch 注册 Target 切换回调。
func (tt *TargetTracker) SetOnSwitch(fn func(url, title string, targetCount int)) {
	tt.mu.Lock
	defer tt.mu.Unlock
	tt.onSwitch = fn
}

// SetOnCDPSwitch 注册 CDP context 切换回调。
// 每次 Screencast 切换到新 Target 时调用，newCtx 是新活跃 Target 的 chromedp context。
// 用途: 通知 InputGateway 更新 cdpCtx，确保输入事件分发到正确的 Target（修复 popup/new-tab 导航卡住）。
func (tt *TargetTracker) SetOnCDPSwitch(fn func(newCtx context.Context)) {
	tt.mu.Lock
	defer tt.mu.Unlock
	tt.onCDPSwitch = fn
}

// SetOnJavaScriptDialog registers the LiveView bridge for alert/confirm/prompt.
func (tt *TargetTracker) SetOnJavaScriptDialog(fn func(JavaScriptDialogEvent)) {
	tt.mu.Lock
	defer tt.mu.Unlock
	tt.onDialog = fn
}

// SetForegroundGuard registers a fail-closed safety check before a target is
// activated or brought to front. macOS headed mode uses this to prove Chrome is
// still contained by CGVirtualDisplay before any foregrounding CDP command.
func (tt *TargetTracker) SetForegroundGuard(fn func(target.ID, string) error) {
	tt.mu.Lock
	defer tt.mu.Unlock
	tt.foregroundGuard = fn
}

func (tt *TargetTracker) SetTargetCloser(fn func(target.ID) error) {
	tt.mu.Lock
	defer tt.mu.Unlock
	tt.closeTarget = fn
}

// ActiveTargetID 返回当前活跃 Target ID。
func (tt *TargetTracker) ActiveTargetID target.ID {
	tt.mu.RLock
	defer tt.mu.RUnlock
	if tt.activeID == "" {
		return tt.primaryID
	}
	return tt.activeID
}

// TargetCount 返回已知 page Target 数量。
func (tt *TargetTracker) TargetCount int {
	tt.mu.RLock
	defer tt.mu.RUnlock
	return tt.totalTabCountLocked
}

// GetActiveCDPContext 返回当前活跃 Target 的 CDP context。
// activeID 有值时返回该 Target 的 chromedp context；否则返回 browserCtx（primary target）。
func (tt *TargetTracker) GetActiveCDPContext context.Context {
	_, ctx := tt.ActiveTargetRef
	return ctx
}

// ActiveTargetRef returns the current target id and its CDP context atomically.
// Async operations must keep this id with their result; reading ActiveTargetID on
// completion can update the wrong tab if the user switched while the work ran.
func (tt *TargetTracker) ActiveTargetRef (string, context.Context) {
	tt.mu.RLock
	defer tt.mu.RUnlock

	activeID := tt.activeID
	if activeID == "" {
		activeID = tt.primaryID
	}
	return string(activeID), tt.targetContextLocked(activeID)
}

func (tt *TargetTracker) TargetCDPContext(targetID string) context.Context {
	if targetID == "" {
		return nil
	}
	tt.mu.RLock
	defer tt.mu.RUnlock
	tracked := tt.targets[target.ID(targetID)]
	if tracked == nil {
		return nil
	}
	if tracked.Ctx != nil && tracked.Ctx.Err == nil {
		return tracked.Ctx
	}
	return tracked.Ctx
}

func (tt *TargetTracker) targetContextLocked(targetID target.ID) context.Context {
	var activeCtx context.Context
	if tracked := tt.targets[targetID]; tracked != nil {
		activeCtx = tracked.Ctx
	}
	if activeCtx != nil && activeCtx.Err == nil {
		return activeCtx
	}
	if tt.browserCtx != nil && tt.browserCtx.Err == nil {
		return tt.browserCtx
	}
	if activeCtx != nil {
		return activeCtx
	}
	return tt.browserCtx
}

func shouldArmTargetClaim(event *InputEvent) bool {
	if event == nil {
		return false
	}
	switch event.Type {
	case "mouse":
		return (event.Event == "mousePressed" || event.Event == "mouseReleased") &&
			(event.Button == "left" || event.Button == "middle")
	case "touch":
		return event.Event == "touchStart" || event.Event == "touchEnd"
	case "keyboard":
		return (event.Event == "keyDown" || event.Event == "char") &&
			(event.Key == "Enter" || event.Code == "Enter" || event.Code == "NumpadEnter")
	default:
		return false
	}
}

func (tt *TargetTracker) currentSourceTargetLocked target.ID {
	if tt.activeID != "" {
		return tt.activeID
	}
	return tt.primaryID
}

func (tt *TargetTracker) pruneAttributionLocked(now time.Time) {
	if len(tt.gestureClaims) > 0 {
		keep := tt.gestureClaims[:0]
		for _, claim := range tt.gestureClaims {
			if now.Sub(claim.Created) <= TargetClaimWindow {
				keep = append(keep, claim)
			}
		}
		tt.gestureClaims = keep
	}
	if len(tt.windowHints) > 0 {
		keep := tt.windowHints[:0]
		for _, hint := range tt.windowHints {
			if now.Sub(hint.Created) <= TargetWindowOpenHintTTL {
				keep = append(keep, hint)
			}
		}
		tt.windowHints = keep
	}
}

func trimHintQueue[T any](items T) T {
	if len(items) <= TargetMaxAttributionHints {
		return items
	}
	return append(T(nil), items[len(items)-TargetMaxAttributionHints:]...)
}

func (tt *TargetTracker) openerMatchesCurrentSourceLocked(info *target.Info) bool {
	if info == nil || info.Type != "page" || info.OpenerID == "" {
		return false
	}
	sourceID := tt.currentSourceTargetLocked
	return sourceID != "" && info.OpenerID == sourceID
}

func (tt *TargetTracker) consumeWindowOpenHintLocked(info *target.Info) bool {
	tt.pruneAttributionLocked(time.Now)
	if info == nil || info.Type != "page" || info.OpenerID != "" || len(tt.windowHints) == 0 {
		return false
	}

	matchURL := !IsBlankTargetURL(info.URL)
	matchIdx := -1
	if matchURL {
		for i, hint := range tt.windowHints {
			if !IsBlankTargetURL(hint.URL) && hint.URL == info.URL {
				matchIdx = i
				break
			}
		}
	}
	if matchIdx == -1 {
		sourceID := tt.currentSourceTargetLocked
		if sourceID != "" {
			for i, hint := range tt.windowHints {
				if hint.SourceID == sourceID {
					matchIdx = i
					break
				}
			}
		}
	}
	if matchIdx == -1 {
		matchIdx = 0
	}

	tt.windowHints = append(tt.windowHints[:matchIdx], tt.windowHints[matchIdx+1:]...)
	return true
}

func (tt *TargetTracker) consumeGestureClaimLocked(info *target.Info) bool {
	tt.pruneAttributionLocked(time.Now)
	if info == nil || info.Type != "page" || info.OpenerID != "" || len(tt.gestureClaims) == 0 {
		return false
	}

	sourceID := tt.currentSourceTargetLocked
	matchIdx := -1
	for i, claim := range tt.gestureClaims {
		if sourceID == "" || claim.SourceID == sourceID {
			matchIdx = i
			break
		}
	}
	if matchIdx == -1 {
		return false
	}

	tt.gestureClaims = append(tt.gestureClaims[:matchIdx], tt.gestureClaims[matchIdx+1:]...)
	return true
}

func (tt *TargetTracker) recordWindowOpenHint(sourceID target.ID, event *page.EventWindowOpen) {
	if event == nil || !event.UserGesture {
		return
	}

	hint := windowOpenHint{
		URL: event.URL
		WindowName: event.WindowName
		Created: time.Now
	}
	tt.mu.Lock
	if sourceID == "" {
		sourceID = tt.currentSourceTargetLocked
	}
	hint.SourceID = sourceID
	tt.pruneAttributionLocked(time.Now)
	tt.windowHints = append(tt.windowHints, hint)
	tt.windowHints = trimHintQueue(tt.windowHints)
	tt.mu.Unlock

	logger.Info(targetLogContext, "window open hint recorded"
		"source_target_id", sourceID
		"url", event.URL
		"window_name", event.WindowName
		"ttl_ms", TargetWindowOpenHintTTL.Milliseconds)

	tt.scheduleWindowOpenTargetRefresh(hint)
}

func (tt *TargetTracker) scheduleWindowOpenTargetRefresh(hint windowOpenHint) {
	if tt.browserCtx == nil || chromedp.FromContext(tt.browserCtx) == nil {
		return
	}
	go func {
		time.Sleep(TargetWindowOpenRefreshDelay)
		tt.mu.RLock
		pending := false
		for _, candidate := range tt.windowHints {
			if candidate.SourceID == hint.SourceID &&
				candidate.URL == hint.URL &&
				candidate.WindowName == hint.WindowName &&
				candidate.Created.Equal(hint.Created) {
				pending = true
				break
			}
		}
		tt.mu.RUnlock
		if !pending {
			return
		}
		logger.Info(targetLogContext, "window open target refresh scheduled"
			"source_target_id", hint.SourceID
			"url", hint.URL
			"window_name", hint.WindowName
			"delay_ms", TargetWindowOpenRefreshDelay.Milliseconds)
		if err := tt.RefreshTargets; err != nil {
			logger.Warn(targetLogContext, "window open target refresh failed"
				"source_target_id", hint.SourceID
				"url", hint.URL
				"window_name", hint.WindowName
				"error", err)
		}
	}
}

func (tt *TargetTracker) handleJavaScriptDialogOpening(sourceID target.ID, event *page.EventJavascriptDialogOpening) {
	if event == nil {
		return
	}
	dialog := JavaScriptDialogEvent{
		ID: fmt.Sprintf("%s:%s:%d", sourceID, event.FrameID, time.Now.UnixNano)
		TargetID: string(sourceID)
		URL: event.URL
		FrameID: string(event.FrameID)
		Message: event.Message
		Type: string(event.Type)
		DefaultPrompt: event.DefaultPrompt
	}
	tt.mu.RLock
	onDialog := tt.onDialog
	tt.mu.RUnlock

	logger.Warn(targetLogContext, "javascript dialog opening"
		"target_id", dialog.TargetID
		"frame_id", dialog.FrameID
		"type", dialog.Type
		"url", dialog.URL
		"message_len", len(dialog.Message)
		"has_prompt_default", dialog.DefaultPrompt != "")
	if onDialog != nil {
		onDialog(dialog)
	}
}

func (tt *TargetTracker) bindPageListener(ctx context.Context, sourceID target.ID) {
	if ctx == nil {
		return
	}

	key := targetListenerKey(sourceID)
	tt.mu.Lock
	if tt.pageListeners[key] {
		tt.mu.Unlock
		return
	}
	tt.pageListeners[key] = true
	tt.mu.Unlock

	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch event := ev.(type) {
		case *page.EventWindowOpen:
			tt.recordWindowOpenHint(sourceID, event)
		case *page.EventJavascriptDialogOpening:
			tt.handleJavaScriptDialogOpening(sourceID, event)
		}
	})

	go func {
		cdpCtx := chromedp.FromContext(ctx)
		if cdpCtx == nil || cdpCtx.Browser == nil {
			tt.mu.Lock
			delete(tt.pageListeners, key)
			tt.mu.Unlock
			return
		}
		if err := runCDPWithSoftTimeout(ctx, TargetPageListenerTimeout, chromedp.ActionFunc(func(execCtx context.Context) error {
			return page.Enable.Do(execCtx)
		})); err != nil {
			tt.mu.Lock
			delete(tt.pageListeners, key)
			tt.mu.Unlock
			logger.Warn(targetLogContext, "page window-open listener enable failed"
				"target_id", sourceID
				"error", err)
		}
	}
}

// RecordUserGesture 记录一次已成功 dispatch 的 takeover 用户手势。
// 它是 opener-less target 归属的弱证据兜底，只在没有 opener/windowOpen 证据时使用。
func (tt *TargetTracker) RecordUserGesture(event *InputEvent) {
	if !shouldArmTargetClaim(event) {
		return
	}
	tt.mu.Lock
	defer tt.mu.Unlock

	sourceID := tt.currentSourceTargetLocked
	tt.pruneAttributionLocked(time.Now)
	tt.gestureClaims = append(tt.gestureClaims, gestureClaim{
		SourceID: sourceID
		Created: time.Now
	})
	tt.gestureClaims = trimHintQueue(tt.gestureClaims)
	logger.Info(targetLogContext, "target claim armed"
		"source_target_id", sourceID
		"ttl_ms", TargetClaimWindow.Milliseconds
		"event_type", event.Type
		"event_name", event.Event)
}

func (tt *TargetTracker) registerTargetLocked(info *target.Info) (*trackedTarget, bool) {
	if info == nil || info.Type != "page" || info.TargetID == "" {
		return nil, false
	}
	if tracked := tt.targets[info.TargetID]; tracked != nil {
		tracked.URL = info.URL
		tracked.Title = info.Title
		tracked.OpenerID = info.OpenerID
		if info.TargetID == tt.primaryID {
			tracked.Ctx = tt.browserCtx
			tracked.Closable = false
		}
		return tracked, false
	}

	closable := info.TargetID != tt.primaryID
	var newCtx context.Context
	var newCancel context.CancelFunc
	if closable {
		parent := tt.browserCtx
		if parent == nil {
			parent = context.Background
		}
		newCtx, newCancel = chromedp.NewContext(parent, chromedp.WithTargetID(info.TargetID))
	} else {
		newCtx = tt.browserCtx
		newCancel = func {}
	}
	tracked := &trackedTarget{
		ID: info.TargetID
		URL: info.URL
		Title: info.Title
		OpenerID: info.OpenerID
		Ctx: newCtx
		Cancel: newCancel
		Created: time.Now
		Closable: closable
	}
	tt.targets[info.TargetID] = tracked
	if tt.activeID == "" {
		tt.activeID = info.TargetID
	}
	if closable {
		tt.addTargetOrderLocked(info.TargetID, false)
		go tt.bindPageListener(newCtx, info.TargetID)
	} else {
		tt.addTargetOrderLocked(info.TargetID, true)
	}
	return tracked, true
}

func (tt *TargetTracker) ensureTargetRegistered(targetID target.ID, url string) (*trackedTarget, error) {
	tt.mu.RLock
	if tracked := tt.targets[targetID]; tracked != nil {
		tt.mu.RUnlock
		return tracked, nil
	}
	tt.mu.RUnlock

	if err := tt.RefreshTargets; err != nil {
		logger.Warn(targetLogContext, "refresh before ensure target registered failed"
			"target_id", targetID
			"error", err)
	}

	tt.mu.RLock
	if tracked := tt.targets[targetID]; tracked != nil {
		tt.mu.RUnlock
		return tracked, nil
	}
	tt.mu.RUnlock

	info := &target.Info{
		TargetID: targetID
		Type: "page"
		URL: url
		Title: url
	}
	tt.mu.Lock
	tracked, _ := tt.registerTargetLocked(info)
	tt.mu.Unlock
	if tracked == nil {
		return nil, fmt.Errorf("target %s could not be registered", targetID)
	}
	return tracked, nil
}

func (tt *TargetTracker) registerKnownTarget(targetID target.ID, url string) (*trackedTarget, error) {
	if targetID == "" {
		return nil, fmt.Errorf("target id is empty")
	}
	tt.mu.Lock
	defer tt.mu.Unlock
	if tracked := tt.targets[targetID]; tracked != nil {
		return tracked, nil
	}
	tracked, _ := tt.registerTargetLocked(&target.Info{
		TargetID: targetID
		Type: "page"
		URL: url
		Title: url
	})
	if tracked == nil {
		return nil, fmt.Errorf("target %s could not be registered", targetID)
	}
	return tracked, nil
}

func (tt *TargetTracker) activateBrowserTarget(targetID target.ID, reason string) error {
	if targetID == "" {
		return nil
	}
	if tt.browserCtx == nil {
		return nil
	}
	tt.mu.RLock
	foregroundGuard := tt.foregroundGuard
	tt.mu.RUnlock
	if foregroundGuard != nil {
		if err := foregroundGuard(targetID, reason); err != nil {
			logger.Warn(targetLogContext, "browser target foreground guard failed"
				"target_id", targetID
				"reason", reason
				"error", err)
			return fmt.Errorf("foreground guard failed: %w", err)
		}
	}
	cdpCtx := chromedp.FromContext(tt.browserCtx)
	if cdpCtx == nil || cdpCtx.Browser == nil {
		logger.Debug(targetLogContext, "browser target activation skipped without cdp browser"
			"target_id", targetID
			"reason", reason)
		return nil
	}
	start := time.Now
	activateErr := targetGraphActivate(tt.browserCtx, targetID, TargetActivateTimeout)
	if activateErr != nil {
		logger.Warn(targetLogContext, "browser target activate failed"
			"target_id", targetID
			"reason", reason
			"elapsed_ms", time.Since(start).Milliseconds
			"error", activateErr)
		return activateErr
	}

	var targetCtx context.Context
	tt.mu.RLock
	if tracked := tt.targets[targetID]; tracked != nil {
		targetCtx = tracked.Ctx
	} else if targetID == tt.primaryID {
		targetCtx = tt.browserCtx
	}
	tt.mu.RUnlock

	bringToFront := false
	if targetCtx != nil {
		// chromedp.NewContext(parent, WithTargetID) materializes Target lazily.
		// CreateTarget returns before that target context necessarily has a CDP
		// session, so warm briefly before Page.bringToFront. This is safe on the
		// virtual display and is required to avoid background-tab throttling.
		if err := runCDPWithSoftTimeout(targetCtx, TargetMaterializeTimeout); err != nil {
			logger.Warn(targetLogContext, "browser target context warm failed before bringToFront"
				"target_id", targetID
				"reason", reason
				"elapsed_ms", time.Since(start).Milliseconds
				"error", err)
		}
		if cdpCtx := chromedp.FromContext(targetCtx); cdpCtx != nil && cdpCtx.Target != nil {
			bringErr := runCDPWithSoftTimeout(targetCtx, TargetActivateTimeout, chromedp.ActionFunc(func(ctx context.Context) error {
				return page.BringToFront.Do(ctx)
			}))
			if bringErr != nil {
				logger.Warn(targetLogContext, "browser page bring to front failed"
					"target_id", targetID
					"reason", reason
					"elapsed_ms", time.Since(start).Milliseconds
					"error", bringErr)
				return bringErr
			}
			bringToFront = true
		}
	}

	logger.Info(targetLogContext, "browser target foregrounded"
		"target_id", targetID
		"reason", reason
		"activate_browser", true
		"bring_to_front", bringToFront
		"elapsed_ms", time.Since(start).Milliseconds)
	return nil
}

// RefreshTargets reconciles TargetTracker state from Chrome's authoritative target list.
// It repairs missed TargetCreated/Destroyed events and is safe to call from REST handlers.
func (tt *TargetTracker) RefreshTargets error {
	if tt.browserCtx == nil {
		return nil
	}
	infos, err := targetGraphListPages(tt.browserCtx, TargetDiscoveryTimeout)
	if err != nil {
		browserCDPErrors.Add(1)
		return err
	}

	seen := make(map[target.ID]bool, len(infos))
	var autoFollowID target.ID
	var autoFollowURL string
	var autoFollowTitle string
	var stateChanged bool
	var stateURL string
	var stateTitle string
	var stateCount int
	var onSwitch func(url, title string, targetCount int)
	tt.mu.Lock
	for _, info := range infos {
		if info == nil || info.Type != "page" || info.TargetID == "" {
			continue
		}
		seen[info.TargetID] = true
		if _, created := tt.registerTargetLocked(info); created {
			logger.Info(targetLogContext, "refreshed missing target"
				"target_id", info.TargetID
				"url", info.URL
				"title", info.Title
				"opener_id", info.OpenerID)

			claimedByOpener := tt.openerMatchesCurrentSourceLocked(info)
			claimedByWindowOpen := false
			claimedByGesture := false
			if !claimedByOpener {
				claimedByWindowOpen = tt.consumeWindowOpenHintLocked(info)
				if !claimedByWindowOpen {
					claimedByGesture = tt.consumeGestureClaimLocked(info)
				}
			}
			userVisibleTarget := claimedByOpener || claimedByWindowOpen || claimedByGesture
			shouldAutoFollow := userVisibleTarget && !IsBlankTargetURL(info.URL)
			if shouldAutoFollow {
				autoFollowID = info.TargetID
				autoFollowURL = info.URL
				autoFollowTitle = info.Title
			}
			if userVisibleTarget && IsBlankTargetURL(info.URL) {
				tt.pendingSwitch[info.TargetID] = true
			}
			logger.Info(targetLogContext, "refresh target visibility resolved"
				"target_id", info.TargetID
				"opener_id", info.OpenerID
				"claimed_by_opener", claimedByOpener
				"claimed_by_window_open", claimedByWindowOpen
				"claimed_by_gesture", claimedByGesture
				"user_visible_target", userVisibleTarget
				"auto_follow", shouldAutoFollow)
			stateChanged = true
		}
	}
	for id, tracked := range tt.targets {
		if seen[id] {
			continue
		}
		logger.Info(targetLogContext, "pruning stale target"
			"target_id", id
			"url", tracked.URL)
		if tracked.Cancel != nil {
			tracked.Cancel
		}
		delete(tt.targets, id)
		delete(tt.pendingSwitch, id)
		tt.order = removeTargetFromOrder(tt.order, id)
		if tt.activeID == id {
			tt.activeID = tt.fallbackActiveTargetLocked
		}
		stateChanged = true
	}
	if stateChanged && autoFollowID == "" {
		stateURL, stateTitle = tt.activeNavigationLocked
		stateCount = tt.totalTabCountLocked
		onSwitch = tt.onSwitch
	}
	tt.mu.Unlock
	if autoFollowID != "" {
		logger.Info(targetLogContext, "refresh auto-follow target"
			"target_id", autoFollowID
			"url", autoFollowURL
			"title", autoFollowTitle)
		if err := tt.SwitchToTarget(string(autoFollowID)); err != nil {
			logger.Warn(targetLogContext, "refresh auto-follow failed"
				"target_id", autoFollowID
				"error", err)
		}
	} else if stateChanged && onSwitch != nil {
		onSwitch(stateURL, stateTitle, stateCount)
	}
	return nil
}

func (tt *TargetTracker) refreshTargetInfo(targetID target.ID, reason string) {
	if targetID == "" || tt.browserCtx == nil {
		return
	}
	info, err := targetGraphGetPageInfo(tt.browserCtx, targetID, TargetActivateTimeout)
	if err != nil {
		logger.Warn(targetLogContext, "refresh target info failed"
			"target_id", targetID
			"reason", reason
			"error", err)
		return
	}
	if info == nil || info.Type != "page" {
		return
	}
	tt.mu.Lock
	tracked, _ := tt.registerTargetLocked(info)
	tt.mu.Unlock
	if tracked == nil {
		return
	}
	logger.Info(targetLogContext, "refresh target info applied"
		"target_id", targetID
		"reason", reason
		"url", info.URL
		"title", info.Title)
}

func (tt *TargetTracker) hasTarget(targetID target.ID) bool {
	tt.mu.RLock
	defer tt.mu.RUnlock
	return tt.targets[targetID] != nil
}

func (tt *TargetTracker) waitLocalTargetGone(targetID target.ID, timeout time.Duration) bool {
	deadline := time.Now.Add(timeout)
	for {
		if !tt.hasTarget(targetID) {
			return true
		}
		if time.Now.After(deadline) {
			return false
		}
		time.Sleep(TargetCloseLocalPollInterval)
	}
}

// HandleTargetCreated 处理新 Target 创建事件。
// 仅跟踪 type=page 的 Target。自动跟随必须通过归属协议判定：
// 当前 source target 的 OpenerID、Page.windowOpen(userGesture=true) 或 dispatch 成功后的本地 gesture claim。
func (tt *TargetTracker) HandleTargetCreated(info *target.Info) {
	if info.Type != "page" {
		return
	}

	tt.mu.Lock
	tracked, created := tt.registerTargetLocked(info)
	if !created {
		tt.mu.Unlock
		return
	}

	claimedByOpener := tt.openerMatchesCurrentSourceLocked(info)
	claimedByWindowOpen := false
	claimedByGesture := false
	if !claimedByOpener {
		claimedByWindowOpen = tt.consumeWindowOpenHintLocked(info)
		if !claimedByWindowOpen {
			claimedByGesture = tt.consumeGestureClaimLocked(info)
		}
	}
	userVisibleTarget := claimedByOpener || claimedByWindowOpen || claimedByGesture
	shouldSwitch := userVisibleTarget && !IsBlankTargetURL(info.URL)
	pendingForSwitch := userVisibleTarget && IsBlankTargetURL(info.URL)
	liveEngine := tt.liveEngine
	hub := tt.hub
	oldActiveID := tt.activeID
	onSwitch := tt.onSwitch
	onCDPSwitch := tt.onCDPSwitch
	targetCount := tt.totalTabCountLocked
	activeURL, activeTitle := tt.activeNavigationLocked
	newCtx := tracked.Ctx

	if shouldSwitch {
		tt.activeID = info.TargetID
		activeURL = info.URL
		activeTitle = info.Title
	}
	if pendingForSwitch {
		tt.pendingSwitch[info.TargetID] = true
	}
	tt.mu.Unlock

	logger.Info(targetLogContext, "target created"
		"target_id", info.TargetID
		"url", info.URL
		"title", info.Title
		"opener_id", info.OpenerID
		"claimed_by_opener", claimedByOpener
		"claimed_by_window_open", claimedByWindowOpen
		"claimed_by_gesture", claimedByGesture
		"user_visible_target", userVisibleTarget
		"should_switch", shouldSwitch
		"pending_for_switch", pendingForSwitch
		"target_count", targetCount)

	if pendingForSwitch && !shouldSwitch {
		go func {
			if err := warmTargetContext(newCtx); err != nil {
				logger.Warn(targetLogContext, "pending target warm failed"
					"target_id", info.TargetID
					"error", err)
			}
		}
	}

	if !shouldSwitch {
		if onSwitch != nil {
			onSwitch(activeURL, activeTitle, targetCount)
		}
		return
	}

	// 在独立 goroutine 执行阻塞操作，避免阻塞 ListenBrowser 事件分发 goroutine
	go func {
		if err := tt.activateBrowserTarget(info.TargetID, "target_created_auto_follow"); err != nil {
			logger.Warn(targetLogContext, "target created auto-follow activation failed"
				"target_id", info.TargetID
				"error", err)
		}
		// 切换 Screencast。SwitchTarget 内部异步处理 CDP stop/start，
		// 不等待页面加载或 target warm。
		if liveEngine != nil && hub != nil {
			tt.mu.RLock
			oldTarget := tt.targets[oldActiveID]
			tt.mu.RUnlock

			if oldTarget != nil && oldTarget.Ctx != nil {
				liveEngine.SwitchTarget(oldTarget.Ctx, newCtx, string(info.TargetID))
			} else {
				// 原始 tab（没有注册到 tracker 中）→ 用 browserCtx 停止
				liveEngine.SwitchTarget(tt.browserCtx, newCtx, string(info.TargetID))
			}
		}

		// 更新 InputGateway 的 CDP context → 输入事件跟随活跃 Target
		if onCDPSwitch != nil {
			onCDPSwitch(newCtx)
		}

		if onSwitch != nil {
			onSwitch(info.URL, info.Title, targetCount)
		}
	}
}

// HandleTargetDestroyed 处理 Target 关闭事件。活跃 Target 关闭时回退到上一个。
func (tt *TargetTracker) HandleTargetDestroyed(targetID target.ID) {
	tt.mu.Lock
	tracked, ok := tt.targets[targetID]
	if !ok {
		tt.mu.Unlock
		return
	}

	logger.Info(targetLogContext, "target destroyed"
		"target_id", targetID
		"url", tracked.URL)

	delete(tt.targets, targetID)
	delete(tt.pendingSwitch, targetID)
	tt.order = removeTargetFromOrder(tt.order, targetID)

	wasActive := tt.activeID == targetID
	liveEngine := tt.liveEngine
	onSwitch := tt.onSwitch
	onCDPSwitch := tt.onCDPSwitch

	var fallbackID target.ID
	var fallback *trackedTarget
	if wasActive {
		// 回退到 Chrome target graph 中最近的存活 page target。primary target 也在 order
		// 里，且以 Closable=false 表达，不再通过 activeID="" 作为隐式主状态。
		for i := len(tt.order) - 1; i >= 0; i-- {
			if candidate := tt.targets[tt.order[i]]; candidate != nil {
				fallbackID = candidate.ID
				fallback = candidate
				break
			}
		}
		tt.activeID = fallbackID
	}
	targetCount := tt.totalTabCountLocked
	activeURL, activeTitle := tt.activeNavigationLocked
	tt.mu.Unlock

	// 清理已关闭 Target 的 context
	if tracked.Cancel != nil {
		tracked.Cancel
	}

	if !wasActive {
		if onSwitch != nil {
			onSwitch(activeURL, activeTitle, targetCount)
		}
		return
	}

	// 回退 Screencast + InputGateway CDP context
	if fallbackID != "" && liveEngine != nil {
		if fallback != nil {
			if err := tt.activateBrowserTarget(fallback.ID, "target_destroyed_fallback"); err != nil {
				logger.Warn(targetLogContext, "fallback target activation failed"
					"target_id", fallback.ID
					"error", err)
			}
			liveEngine.SwitchTarget(tracked.Ctx, fallback.Ctx, string(fallback.ID))
			// 回退 InputGateway 到上一个 Target 的 CDP context
			if onCDPSwitch != nil {
				onCDPSwitch(fallback.Ctx)
			}
			if onSwitch != nil {
				onSwitch(fallback.URL, fallback.Title, targetCount)
			}
		}
	} else if liveEngine != nil {
		liveEngine.SwitchTarget(tracked.Ctx, tt.browserCtx, string(tt.primaryID))
		if onCDPSwitch != nil {
			onCDPSwitch(tt.browserCtx)
		}
		if onSwitch != nil {
			onSwitch(activeURL, activeTitle, targetCount)
		}
	}
}

// HandleTargetInfoChanged 更新 Target 元数据（URL/Title 变化）。
// 关键: 处理 window.open(”) pendingSwitch 模式 — 空 URL 新标签拿到真实 URL 后立即切换 Screencast。
func (tt *TargetTracker) HandleTargetInfoChanged(info *target.Info) {
	if info.Type != "page" {
		return
	}
	tt.mu.Lock
	tracked, created := tt.registerTargetLocked(info)
	ok := tracked != nil
	if created {
		logger.Info(targetLogContext, "infoChanged registered missing target"
			"target_id", info.TargetID
			"url", info.URL
			"opener_id", info.OpenerID)
	}
	isActive := tt.activeID == info.TargetID

	// pendingSwitch: window.open('') 模式 — 空白新 tab 已拿到真实 URL → 触发切换
	claimedByOpener := false
	claimedByWindowOpen := false
	claimedByGesture := false
	if created {
		claimedByOpener = tt.openerMatchesCurrentSourceLocked(info)
		if !claimedByOpener {
			claimedByWindowOpen = tt.consumeWindowOpenHintLocked(info)
			if !claimedByWindowOpen {
				claimedByGesture = tt.consumeGestureClaimLocked(info)
			}
		}
		if (claimedByOpener || claimedByWindowOpen || claimedByGesture) && IsBlankTargetURL(info.URL) {
			tt.pendingSwitch[info.TargetID] = true
		}
	}
	isPending := tt.pendingSwitch[info.TargetID] || claimedByOpener || claimedByWindowOpen || claimedByGesture
	shouldSwitchNow := !isActive && isPending && ok &&
		!IsBlankTargetURL(info.URL)

	var oldActiveCtx context.Context
	var newCtx context.Context
	if shouldSwitchNow {
		delete(tt.pendingSwitch, info.TargetID)
		// 保存旧活跃 context（用于停止旧 Screencast）
		if tt.activeID != "" {
			if old := tt.targets[tt.activeID]; old != nil {
				oldActiveCtx = old.Ctx
			}
		}
		if oldActiveCtx == nil {
			oldActiveCtx = tt.browserCtx
		}
		newCtx = tracked.Ctx
		tt.activeID = info.TargetID
	}

	onSwitch := tt.onSwitch
	onCDPSwitch := tt.onCDPSwitch
	liveEngine := tt.liveEngine
	targetCount := tt.totalTabCountLocked
	activeURL, activeTitle := tt.activeNavigationLocked
	tt.mu.Unlock

	logger.Info(targetLogContext, "target info changed"
		"target_id", info.TargetID
		"url", info.URL
		"title", info.Title
		"opener_id", info.OpenerID
		"created", created
		"claimed_by_opener", claimedByOpener
		"claimed_by_window_open", claimedByWindowOpen
		"claimed_by_gesture", claimedByGesture
		"is_active", isActive
		"is_pending", isPending
		"should_switch_now", shouldSwitchNow
		"target_count", targetCount)

	if shouldSwitchNow {
		// 在独立 goroutine 执行阻塞操作，避免阻塞 ListenBrowser 事件分发 goroutine
		go func {
			logger.Info(targetLogContext, "pending switch resolved"
				"target_id", info.TargetID
				"url", info.URL)
			if err := tt.activateBrowserTarget(info.TargetID, "pending_switch_resolved"); err != nil {
				logger.Warn(targetLogContext, "pending switch activation failed"
					"target_id", info.TargetID
					"error", err)
			}
			// 切换 Screencast。实际 CDP start 由 liveEngine 异步完成。
			if liveEngine != nil {
				liveEngine.SwitchTarget(oldActiveCtx, newCtx, string(info.TargetID))
			}
			// 更新 InputGateway CDP context
			if onCDPSwitch != nil {
				onCDPSwitch(newCtx)
			}
			// 通知前端更新地址栏
			if onSwitch != nil {
				onSwitch(info.URL, info.Title, targetCount)
			}
		}
		return
	}

	// 活跃 Target 的 URL 变化 → 通知前端更新地址栏
	if ok && isActive && onSwitch != nil {
		onSwitch(info.URL, info.Title, targetCount)
		return
	}
	if ok && onSwitch != nil {
		onSwitch(activeURL, activeTitle, targetCount)
	}
}

// UpdateTabInfo updates one concrete target. Async operations must use this
// with the target id captured at operation start, so completion cannot write
// into a newly active tab.
func (tt *TargetTracker) UpdateTabInfo(targetID string, url, title string) bool {
	tt.mu.Lock
	defer tt.mu.Unlock

	tid := target.ID(targetID)
	if tid == "" {
		return false
	}
	tracked := tt.targets[tid]
	if tracked == nil {
		return false
	}
	if url != "" {
		tracked.URL = url
	}
	if title != "" {
		tracked.Title = title
	}
	return true
}

// ListTargets 返回当前所有已知 page Target 的元数据列表 。
// 返回值中 Active 标记当前 screencast 指向的 Target。
func (tt *TargetTracker) ListTargets TabInfo {
	tt.mu.RLock
	defer tt.mu.RUnlock

	var tabs TabInfo
	activeID := tt.activeID
	if activeID == "" {
		activeID = tt.primaryID
	}
	seen := make(map[target.ID]bool, len(tt.targets))
	appendTab := func(id target.ID) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		t := tt.targets[id]
		if t == nil {
			return
		}
		tabs = append(tabs, TabInfo{
			ID: string(t.ID)
			Title: t.Title
			URL: t.URL
			Active: activeID == t.ID
			Closable: t.Closable
		})
	}
	for _, id := range tt.order {
		appendTab(id)
	}
	for id := range tt.targets {
		appendTab(id)
	}
	return tabs
}

// SwitchToTarget 手动切换 Screencast 到指定 Target 。
// 终局约束: Browser LiveView 的 tab 切换必须同步切换真实 Chrome 活跃 tab。
// headed Chrome 的后台 tab 不稳定产帧，且 Input.dispatch* 在后台标签上不可依赖；
// 因此 Target.activateTarget 是语义的一部分。窗口隔离由 DisplayStrategy/CGVirtualDisplay 负责。
func (tt *TargetTracker) SwitchToTarget(targetID string) error {
	if targetID == "" {
		return fmt.Errorf("target id is empty")
	}

	tid := target.ID(targetID)
	tt.mu.RLock
	_, exists := tt.targets[tid]
	tt.mu.RUnlock
	if !exists {
		if err := tt.RefreshTargets; err != nil {
			logger.Warn(targetLogContext, "switch target refresh failed"
				"target_id", targetID
				"error", err)
		}
	}

	if err := tt.activateBrowserTarget(tid, "manual_switch"); err != nil {
		return fmt.Errorf("activate target %s: %w", targetID, err)
	}
	tt.refreshTargetInfo(tid, "manual_switch")

	tt.mu.Lock
	tracked, ok := tt.targets[tid]
	if !ok {
		tt.mu.Unlock
		return fmt.Errorf("target %s not found", targetID)
	}
	if tt.activeID == tid {
		tt.mu.Unlock
		return nil // 已经是活跃 tab
	}

	oldActiveID := tt.activeID
	tt.activeID = tid
	liveEngine := tt.liveEngine
	onSwitch := tt.onSwitch
	onCDPSwitch := tt.onCDPSwitch
	targetCount := tt.totalTabCountLocked
	newCtx := tracked.Ctx

	var oldCtx context.Context
	if oldActiveID != "" {
		if old := tt.targets[oldActiveID]; old != nil {
			oldCtx = old.Ctx
		}
	}
	if oldCtx == nil {
		oldCtx = tt.browserCtx
	}
	tt.mu.Unlock

	logger.Info(targetLogContext, "switch target requested"
		"target_id", targetID
		"old_active_id", oldActiveID
		"target_url", tracked.URL
		"target_title", tracked.Title
		"target_count", targetCount
		"activate_browser", true)

	// 切换 Screencast
	if liveEngine != nil {
		liveEngine.SwitchTarget(oldCtx, newCtx, string(tid))
	} else {
		logger.Warn(targetLogContext, "switch target missing liveview engine"
			"target_id", targetID
			"old_active_id", oldActiveID
			"target_count", targetCount)
	}
	if onCDPSwitch != nil {
		onCDPSwitch(newCtx)
	}
	if onSwitch != nil {
		onSwitch(tracked.URL, tracked.Title, targetCount)
	}
	logger.Info(targetLogContext, "switch target applied"
		"target_id", targetID
		"target_url", tracked.URL
		"target_title", tracked.Title
		"target_count", targetCount)
	return nil
}

// CloseTarget 关闭指定 Target 。
// 不能关闭不可关闭 Target。关闭后 TargetTracker 的 HandleTargetDestroyed 会自动处理 fallback。
func (tt *TargetTracker) CloseTarget(targetID string) error {
	if targetID == "" {
		return fmt.Errorf("target id is empty")
	}

	tt.mu.RLock
	tid := target.ID(targetID)
	tracked, ok := tt.targets[tid]
	closeTarget := tt.closeTarget
	wasActive := tt.activeID == tid
	var fallbackID target.ID
	if wasActive {
		for i := len(tt.order) - 1; i >= 0; i-- {
			candidateID := tt.order[i]
			if candidateID != tid && tt.targets[candidateID] != nil {
				fallbackID = candidateID
				break
			}
		}
	}
	tt.mu.RUnlock
	if !ok {
		return fmt.Errorf("target %s not found", targetID)
	}
	if !tracked.Closable {
		return fmt.Errorf("target %s is not closable", targetID)
	}

	logger.Info(targetLogContext, "close target requested", "target_id", targetID, "active", tt.ActiveTargetID == tid)

	if wasActive && fallbackID != "" {
		if err := tt.SwitchToTarget(string(fallbackID)); err != nil {
			logger.Warn(targetLogContext, "close target fallback switch failed"
				"target_id", targetID
				"fallback_id", fallbackID
				"error", err)
		}
	}
	if tracked.Cancel != nil {
		tracked.Cancel
	}

	// Target.closeTarget 对 active target 可能先产生 TargetDestroyed 事件，
	// 但 CDP response 被 target session teardown 拖住。关闭 UX 以 TargetDestroyed
	// 事件或本地 tracker 收敛为完成条件，不再反向调用 Target.getTargets 轮询。
	closeResult := make(chan error, 1)
	go func {
		if closeTarget != nil {
			closeResult <- closeTarget(tid)
			return
		}
		closeResult <- targetGraphClose(tt.browserCtx, tid, TargetCreateTimeout)
	}
	select {
	case err := <-closeResult:
		if err != nil {
			if !tt.hasTarget(tid) {
				logger.Info(targetLogContext, "close target completed after cdp error and local destroy"
					"target_id", targetID
					"error", err)
				return nil
			}
			logger.Warn(targetLogContext, "close target browser graph path failed"
				"target_id", targetID
				"error", err)
			if tracked.Cancel != nil {
				tracked.Cancel
			}
		}
	case <-time.After(TargetCloseLocalWaitTimeout):
		if !tt.hasTarget(tid) {
			logger.Info(targetLogContext, "close target completed before cdp response"
				"target_id", targetID
				"wait_ms", TargetCloseLocalWaitTimeout.Milliseconds)
			return nil
		}
		logger.Warn(targetLogContext, "close target cdp response pending; applying local close"
			"target_id", targetID
			"wait_ms", TargetCloseLocalWaitTimeout.Milliseconds)
	}
	if !tt.waitLocalTargetGone(tid, TargetCloseLocalWaitTimeout) {
		if tracked.Cancel != nil {
			tracked.Cancel
		}
		tt.HandleTargetDestroyed(tid)
	}
	logger.Info(targetLogContext, "close target completed", "target_id", targetID)
	return nil
}

// CreateTab 创建新标签页并切换 Screencast 到新 tab。
// url 为空时默认 ChromeInitialPageURL。该 URL 只表示 Chrome 初始化页，
// 不能被上层当作用户导航意图。
// 返回新 Target ID。createTarget 产生的 tab 没有 OpenerID，
// HandleTargetCreated 不会自动切换 Screencast，因此显式调用 SwitchToTarget。
func (tt *TargetTracker) CreateTab(url string) (string, error) {
	url = NormalizeTargetCreateURL(url)
	logger.Info(targetLogContext, "create tab requested", "url", url)

	// Target.createTarget 是 Browser Target Graph 操作，返回即得到 Chrome SSoT targetID。
	tid, err := targetGraphCreatePage(tt.browserCtx, url, TargetCreateWaitTimeout)
	if err != nil {
		return "", fmt.Errorf("create target: %w", err)
	}
	logger.Info(targetLogContext, "create tab success", "target_id", tid)

	if _, err := tt.registerKnownTarget(tid, url); err != nil {
		return "", err
	}
	// 显式切换 Screencast 到新 tab（因 CreateTarget 无 OpenerID，HandleTargetCreated 不自动切换）
	if err := tt.SwitchToTarget(string(tid)); err != nil {
		return "", err
	}
	return string(tid), nil
}

// SetupListeners 注册两类监听:
// - browserCtx: Browser 级 TargetCreated/Destroyed/InfoChanged + discover/auto-attach
// - tt.browserCtx: 当前 liveview primary target 的 Page.windowOpen/source-page 监听
//
// 关键约束:
// - Browser 级 Target 事件必须绑定 browserCtx（entry.browserCtx），否则新 tab 可能漏收
// - source page 的 windowOpen 证据必须绑定 primary target ctx（tt.browserCtx/tabCtx），
// 不能错误绑到 browserCtx，否则 Baidu 这类 opener-less page-opened target 会失去归属证据，
// 只能在 RefreshTargets 时被动补登记
func (tt *TargetTracker) SetupListeners(browserCtx context.Context) {
	chromedp.ListenBrowser(browserCtx, func(ev interface{}) {
		switch e := ev.(type) {
		case *target.EventTargetCreated:
			tt.HandleTargetCreated(e.TargetInfo)
		case *target.EventTargetDestroyed:
			tt.HandleTargetDestroyed(e.TargetID)
		case *target.EventTargetInfoChanged:
			tt.HandleTargetInfoChanged(e.TargetInfo)
		}
	})

	// source-page 监听必须挂在 liveview primary target，而不是 browser-level ctx。
	tt.bindPageListener(tt.browserCtx, tt.primaryID)

	// TargetCreated/Destroyed 事件必须显式开启 Target Graph discovery。
	// waitForDebuggerOnStart 必须为 false；否则 page-opened target 会暂停在空 URL。
	if err := targetGraphEnableDiscovery(browserCtx, TargetPageListenerTimeout); err != nil {
		logger.Warn(targetLogContext, "target graph discovery enable failed", "error", err)
	}

	// 主 source target 也要显式启用 Page domain，Page.windowOpen 才会可靠上报。
	_ = runCDPWithSoftTimeout(tt.browserCtx, TargetPageListenerTimeout, chromedp.ActionFunc(func(ctx context.Context) error {
		return page.Enable.Do(ctx)
	}))
}
