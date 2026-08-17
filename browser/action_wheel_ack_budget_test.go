package browser

import (
	"context"
	"sync"
	"testing"
	"time"
)

// 回归契约 [BUG-WHEEL-DISPATCH-HANGS]
//
// 直连 headless 会话下 `scroll down N` / `wheelat x,y` 会打满 15s 动作看门狗、无输出，
// 而同一页的 click/fill/observe 全部正常。根因见 action_engine.go 中
// wheelGestureAckBudget 处的说明：Input.dispatchMouseEvent 要等渲染器 ack 才返回，
// 后台 target 上这个 ack 永不到来，而一次 scroll 会展开成 9 个脉冲逐个串行等。
//
// 下面锁住的是"等待有上界 + 位移不丢"，不是"超时更长"。

// recordingGateway 返回一个记录每次派发、并且永不 ack（挂到 ctx 结束）的输入网关，
// 用来在没有 Chrome 的情况下模拟"渲染器一次都不 ack"。
func recordingGateway(ack bool) (*InputGateway, func() []InputEvent) {
	var mu sync.Mutex
	var seen []InputEvent
	gw := NewInputGateway(nil, nil)
	gw.dispatchMouse = func(ctx context.Context, event *InputEvent) error {
		mu.Lock()
		seen = append(seen, *event)
		mu.Unlock()
		if ack {
			return nil
		}
		<-ctx.Done()
		return ctx.Err()
	}
	return gw, func() []InputEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]InputEvent(nil), seen...)
	}
}

func TestHumanWheelGestureReturnsWhenRendererNeverAcks(t *testing.T) {
	gw, events := recordingGateway(false)
	engine := newActionEngine(nil)
	engine.humanInput = gw

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 9 = 1080 视口下 `scroll down` 的实际脉冲数 ceil(1080*0.9/120)，也就是线上超时的那条路径。
	const steps = 9
	started := time.Now()
	err := engine.dispatchHumanMouseWheel(ctx, 960, 540, humanWheelStepCSSPixels, steps)
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("wheel gesture returned error %v, want success: the events were dispatched, only the ack is missing", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("wheel gesture returned after %s, want < 2s even when the renderer never acks", elapsed)
	}

	got := events()
	if len(got) < 2 {
		t.Fatalf("dispatched %d events, want at least a mouseMoved and one wheel pulse", len(got))
	}
	if got[0].Event != "mouseMoved" {
		t.Fatalf("first event = %q, want mouseMoved so the pointer is over the target before the wheel", got[0].Event)
	}
	var total float64
	for i, event := range got[1:] {
		if event.Event != "mouseWheel" {
			t.Fatalf("event %d = %q, want mouseWheel", i+1, event.Event)
		}
		total += event.DeltaY
	}
	// 背压下剩余位移被合并成一个事件，但位移总量一分不能少 —— 否则就是"返回快了、
	// 页面少滚了"的静默错报。
	if want := humanWheelStepCSSPixels * steps; total != want {
		t.Fatalf("dispatched total deltaY = %v across %d wheel events, want %v: coalescing must not drop scroll distance",
			total, len(got)-1, want)
	}
	// 合并的意义就是把 N 次串行往返压下来；否则预算会被逐个脉冲耗尽。
	if wheels := len(got) - 1; wheels >= steps {
		t.Fatalf("dispatched %d wheel events, want fewer than %d once backpressure is detected", wheels, steps)
	}
}

func TestHumanWheelGestureKeepsPerTickFidelityWhenRendererAcks(t *testing.T) {
	gw, events := recordingGateway(true)
	engine := newActionEngine(nil)
	engine.humanInput = gw

	started := time.Now()
	if err := engine.dispatchHumanMouseWheel(context.Background(), 100, 100, humanWheelStepCSSPixels, 9); err != nil {
		t.Fatalf("wheel gesture failed: %v", err)
	}
	// 健康页面上 ack 是毫秒级的，预算永远不该生效 —— 否则每次滚动都白等一份下界。
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("wheel gesture took %s on an acking renderer, want the ack budget to stay out of the way", elapsed)
	}
	got := events()
	if len(got) != 10 {
		t.Fatalf("dispatched %d events, want 10 (one mouseMoved + 9 discrete wheel ticks)", len(got))
	}
	// 渲染器跟得上时必须保持一格一格的 tick 语义：按 tick 计数的页面（轮播、
	// 缩放步进）不能因为工具"优化"就收到一个大 delta。
	for i, event := range got[1:] {
		if event.DeltaY != humanWheelStepCSSPixels {
			t.Fatalf("wheel %d deltaY = %v, want the %v single-notch step", i+1, event.DeltaY, humanWheelStepCSSPixels)
		}
	}
}

func TestRunWheelGestureCoalescesRemainderOnce(t *testing.T) {
	var deltas []float64
	acked, err := runWheelGesture(9, 0, 120, func() bool { return false },
		func(dx, dy float64, budget time.Duration) (bool, error) {
			deltas = append(deltas, dy)
			if len(deltas) == 1 {
				return false, nil // 第一个脉冲就撞上背压
			}
			return true, nil
		})
	if err != nil {
		t.Fatalf("gesture err = %v", err)
	}
	if !acked {
		t.Fatal("acked = false, want true: the coalesced tail was acknowledged")
	}
	if len(deltas) != 2 {
		t.Fatalf("dispatched %d wheel events (%v), want 2: one probe pulse plus one coalesced tail", len(deltas), deltas)
	}
	if deltas[0] != 120 || deltas[1] != 120*8 {
		t.Fatalf("deltas = %v, want [120 960] so the full 9-notch distance still lands", deltas)
	}
}

// 背压下"页面滚不动"和"页面滚得慢"必须分开处置：前者立刻返回（再等也不会被处理），
// 后者放宽预算等完（提前返回 = 页面少滚了一段的静默错报）。
func TestRunWheelGestureExtendsBudgetOnlyForABusyRenderer(t *testing.T) {
	for _, tc := range []struct {
		name        string
		busy        bool
		wantAtLeast time.Duration
		wantAtMost  time.Duration
	}{
		{"idle renderer swallows input: return fast", false, 0, wheelGestureAckBudget},
		{"busy renderer is still working: wait it out", true, wheelGestureAckBudget, wheelBusyPageAckBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tailBudget time.Duration
			calls := 0
			_, err := runWheelGesture(9, 0, 120, func() bool { return tc.busy },
				func(dx, dy float64, budget time.Duration) (bool, error) {
					calls++
					if calls > 1 {
						tailBudget = budget
					}
					return false, nil
				})
			if err != nil {
				t.Fatalf("gesture err = %v", err)
			}
			if calls != 2 {
				t.Fatalf("dispatched %d wheel events, want 2", calls)
			}
			if tailBudget < tc.wantAtLeast || tailBudget > tc.wantAtMost {
				t.Fatalf("coalesced tail budget = %s, want within [%s, %s]", tailBudget, tc.wantAtLeast, tc.wantAtMost)
			}
		})
	}
}

func TestAwaitDispatchWithBudgetReportsPendingAck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := time.Now()
	acked, err := awaitDispatchWithBudget(ctx, 40*time.Millisecond, func() error {
		<-ctx.Done()
		return ctx.Err()
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("err = %v, want nil: an outstanding ack is not a dispatch failure", err)
	}
	if acked {
		t.Fatal("acked = true, want false so callers know the page may still be working")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("returned after %s, want ~40ms", elapsed)
	}

	acked, err = awaitDispatchWithBudget(ctx, time.Second, func() error { return nil })
	if err != nil || !acked {
		t.Fatalf("fast dispatch = (%v, %v), want (true, nil)", acked, err)
	}
}

func TestAwaitDispatchWithBudgetPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	acked, err := awaitDispatchWithBudget(ctx, time.Minute, func() error {
		<-ctx.Done()
		return ctx.Err()
	})
	if acked {
		t.Fatal("acked = true on a canceled context")
	}
	if err == nil {
		t.Fatal("err = nil, want the canceled context error surfaced to the caller")
	}
}

func TestWheelGestureWaitStaysUnderTheActionWatchdog(t *testing.T) {
	// 整段手势的等待预算必须明显低于 15s 动作看门狗，否则滚动仍然以"无输出超时"
	// 的形式失败 —— 那正是这次要修掉的症状。
	if wheelGestureAckBudget >= actionExecutionTimeout {
		t.Fatalf("gesture budget %s >= action watchdog %s", wheelGestureAckBudget, actionExecutionTimeout)
	}
	if targetActivationBudget+wheelGestureAckBudget >= 2*time.Second {
		t.Fatalf("activation %s + gesture %s >= 2s", targetActivationBudget, wheelGestureAckBudget)
	}

	deadline := time.Now().Add(wheelGestureAckBudget)
	if budget := wheelPulseAckBudget(deadline, 1); budget > wheelAckPulseCap {
		t.Fatalf("single-pulse budget = %s, want <= %s so one slow pulse cannot eat the gesture", budget, wheelAckPulseCap)
	}
	if budget := wheelPulseAckBudget(deadline, 1000); budget != wheelAckMinWait {
		t.Fatalf("budget with many pulses left = %s, want the %s floor that keeps pulses ordered", budget, wheelAckMinWait)
	}
	if budget := wheelPulseAckBudget(time.Now().Add(-time.Hour), 4); budget != wheelAckMinWait {
		t.Fatalf("budget past the gesture deadline = %s, want the %s floor", budget, wheelAckMinWait)
	}
}
