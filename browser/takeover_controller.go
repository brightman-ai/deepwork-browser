package browser

import (
	"context"
	"sync"
	"time"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/chromedp"
)

// ============================================================
// § TakeoverController
// ============================================================

// TakeoverMode 是接管模式枚举。
type TakeoverMode string

const (
	// TakeoverModeObserve — AI 控制，Human 监视。
	TakeoverModeObserve TakeoverMode = "observe"
	// TakeoverModeTakeover — Human 接管，AI 操作被拒绝。
	TakeoverModeTakeover TakeoverMode = "takeover"
)

// DefaultTakeoverTimeout 是默认接管超时时间（5分钟）[TC-09-L4-10]。
const DefaultTakeoverTimeout = 5 * time.Minute

// takeoverController 实现双模式状态机 。
type takeoverController struct {
	mu sync.RWMutex
	mode TakeoverMode
	timeout time.Duration
	timer *time.Timer
	onRelease func // 超时自动释放回调
	cdpCtx context.Context // CDP 操作 context
}

// newTakeoverController 创建 TakeoverController，初始状态为 OBSERVE 。
func newTakeoverController(cdpCtx context.Context) *takeoverController {
	return &takeoverController{
		mode: TakeoverModeObserve
		timeout: DefaultTakeoverTimeout
		cdpCtx: cdpCtx
	}
}

// Mode 返回当前接管模式 。
func (c *takeoverController) Mode TakeoverMode {
	c.mu.RLock
	defer c.mu.RUnlock
	return c.mode
}

// EnableTakeover 切换到 TAKEOVER 模式 。
// 启动超时计时器（默认 5min）。
func (c *takeoverController) EnableTakeover(onRelease func) error {
	c.mu.Lock
	defer c.mu.Unlock

	c.mode = TakeoverModeTakeover
	c.onRelease = onRelease

	// 启动超时计时器 [TC-09-L4-10]
	if c.timer != nil {
		c.timer.Stop
	}
	c.timer = time.AfterFunc(c.timeout, func {
		c.autoRelease
	})

	return nil
}

// DisableTakeover 释放接管，恢复 OBSERVE 模式。
func (c *takeoverController) DisableTakeover error {
	c.mu.Lock
	defer c.mu.Unlock

	if c.timer != nil {
		c.timer.Stop
		c.timer = nil
	}
	c.mode = TakeoverModeObserve
	return nil
}

// autoRelease 超时自动释放接管 [TC-09-L4-10]。
func (c *takeoverController) autoRelease {
	c.mu.Lock
	c.mode = TakeoverModeObserve
	c.timer = nil
	onRelease := c.onRelease
	c.mu.Unlock

	if onRelease != nil {
		onRelease
	}
}

// IsTakeover 返回当前是否处于接管模式。
func (c *takeoverController) IsTakeover bool {
	c.mu.RLock
	defer c.mu.RUnlock
	return c.mode == TakeoverModeTakeover
}

// DispatchInput 在接管模式下转发输入事件到 CDP 。
// 非接管模式返回 ErrTakeoverActive（含义: 不在接管模式，操作无效）。
func (c *takeoverController) DispatchInput(ctx context.Context, event *InputEvent) error {
	if !c.IsTakeover {
		// 非接管模式，Human 输入无效（需先 EnableTakeover）
		return ErrTakeoverActive
	}

	switch event.Type {
	case "mouse":
		return c.dispatchMouseEvent(ctx, event)
	case "keyboard":
		return c.dispatchKeyEvent(ctx, event)
	default:
		return nil
	}
}

// dispatchMouseEvent 分发鼠标事件到 CDP — 支持 click/move/scroll/dblclick。
func (c *takeoverController) dispatchMouseEvent(ctx context.Context, event *InputEvent) error {
	// mouseWheel 是特殊类型
	if event.Event == "mouseWheel" {
		return chromedp.Run(c.cdpCtx
			chromedp.ActionFunc(func(ctx context.Context) error {
				return input.DispatchMouseEvent(input.MouseWheel, event.X, event.Y).
					WithDeltaX(event.DeltaX).
					WithDeltaY(event.DeltaY).
					Do(ctx)
			})
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

	return chromedp.Run(c.cdpCtx
		chromedp.ActionFunc(func(ctx context.Context) error {
			return input.DispatchMouseEvent(mouseType, event.X, event.Y).
				WithButton(button).
				WithClickCount(int64(clickCount)).
				Do(ctx)
		})
	)
}

// dispatchKeyEvent 分发键盘事件到 CDP。
func (c *takeoverController) dispatchKeyEvent(ctx context.Context, event *InputEvent) error {
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

	return chromedp.Run(c.cdpCtx
		chromedp.ActionFunc(func(ctx context.Context) error {
			return input.DispatchKeyEvent(keyType).
				WithKey(event.Key).
				WithCode(event.Code).
				WithText(event.Text).
				WithModifiers(input.Modifier(event.Modifiers)).
				Do(ctx)
		})
	)
}

// SetTimeout 设置接管超时时间（测试用）。
func (c *takeoverController) SetTimeout(d time.Duration) {
	c.mu.Lock
	defer c.mu.Unlock
	c.timeout = d
}
