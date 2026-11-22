package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
)

// headedWindowRig 是"真实 TargetTracker + 真实输入锁 + 真实 headed 附着激活钩子"的
// 最小装配:只有 Chrome 本身缺席(tracker 没有 CDP 浏览器,激活落到"无事可做"),
// 取锁、凭证传递、guard 调用次数、key 一致性校验全部是真实路径。
type headedWindowRig struct {
	wsURL      string
	key        string
	tracker    *TargetTracker
	engine     *actionEngine
	guardCalls *int
}

func newHeadedWindowRig(t *testing.T) *headedWindowRig {
	t.Helper()
	wsURL := fmt.Sprintf("ws://test-headed-window-%s-%d-%d", t.Name(), os.Getpid(), time.Now().UnixNano())
	key := BrowserInputKey(wsURL, 0)
	t.Cleanup(func() {
		_ = os.Remove(browserInputLockPath(key))
		_ = os.Remove(browserInputOwnerPath(key))
		_ = os.Remove(browserFrontLedgerPath(key))
	})

	guardCalls := 0
	tracker := NewTargetTracker(context.Background())
	tracker.SetBrowserInputKey(wsURL, 0)
	tracker.SetNoFrontMode(true)
	tracker.SetForegroundGuard(func(target.ID, string) error { guardCalls++; return nil })

	e := newActionEngine(newSnapshotEngine())
	e.setBrowserInputKey(wsURL, 0)
	e.targetActivation = newAttachedHeadedActivation(tracker)
	return &headedWindowRig{wsURL: wsURL, key: key, tracker: tracker, engine: e, guardCalls: &guardCalls}
}

// E1-E3 的回归:headed 附着激活在 action 派发窗口里调用 tracker。
// 修复前,激活闭包在窗口内又取了同一个 key 的锁 —— 同进程第二次 open 同一个锁文件,
// flock 按打开的文件描述判冲突,自己等自己 8s 后报"另一个操作者"。
func TestAttachedHeadedActivationRunsInsideInputWindow(t *testing.T) {
	rig := newHeadedWindowRig(t)

	started := time.Now()
	release, brought, _, err := rig.engine.enterInputDispatchWindowForRequest(context.Background(), false, "", true)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("headed 附着激活在输入窗口内失败(%s): %v", elapsed.Round(time.Millisecond), err)
	}
	defer release()
	if brought {
		t.Fatal("attached headed activation must never report bringToFront")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("进入输入窗口耗时 %s, want < 200ms", elapsed)
	}
	if *rig.guardCalls != 1 {
		t.Fatalf("foreground guard 调用 %d 次, want 1(窗口内只在 ActivateNoFrontIn 里调一次)", *rig.guardCalls)
	}
	// 窗口仍然有效:别人此刻进不来。
	if _, err := AcquireBrowserInputLock(rig.key, "test.intruder", 0); err == nil {
		t.Fatal("动作窗口持有期间,第二个申请者拿到了锁")
	}
}

// 没有 foreground guard 的 headed 附着会话必须响亮失败(display 围栏缺失 = 不许输入)。
func TestAttachedHeadedActivationFailsClosedWithoutGuard(t *testing.T) {
	rig := newHeadedWindowRig(t)
	rig.tracker.SetForegroundGuard(nil)

	release, _, _, err := rig.engine.enterInputDispatchWindowForRequest(context.Background(), false, "", true)
	if err == nil {
		release()
		t.Fatal("没有 foreground guard 却放行了输入")
	}
	if _, acquireErr := AcquireBrowserInputLock(rig.key, "test.after-failure", 0); acquireErr != nil {
		t.Fatalf("激活失败后输入窗口没有释放: %v", acquireErr)
	}
}

// engine 与 tracker 绑定了不同的运行时 key,互斥就失效了:必须立刻、点明地失败,
// 而不是静默放行(旧行为)或各取各的锁。
func TestAttachedHeadedActivationRejectsTrackerBoundToAnotherRuntime(t *testing.T) {
	rig := newHeadedWindowRig(t)
	rig.tracker.SetBrowserInputKey(rig.wsURL+"-other", 0)
	t.Cleanup(func() { _ = os.Remove(browserInputLockPath(BrowserInputKey(rig.wsURL+"-other", 0))) })

	started := time.Now()
	release, _, _, err := rig.engine.enterInputDispatchWindowForRequest(context.Background(), false, "", true)
	if err == nil {
		release()
		t.Fatal("engine 与 tracker 的 key 不一致却放行了")
	}
	if !errors.Is(err, ErrInputWindowNotHeld) {
		t.Fatalf("want ErrInputWindowNotHeld, got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("key 不一致应当立即报错,却用了 %s", time.Since(started))
	}
	if *rig.guardCalls != 0 {
		t.Fatalf("凭证校验失败前不该调用 guard, 实际 %d 次", *rig.guardCalls)
	}
}

// 不变量:在窗口被持有、等待上界为 0 的情况下,逐个执行所有"窗口内"钩子,
// 本进程内的重入次数必须为 0(任何一次再取锁,都会在 lockWait=0 下立刻失败并计数)。
func TestInputWindowHooksNeverReacquireTheWindow(t *testing.T) {
	rig := newHeadedWindowRig(t)
	rig.tracker.inputLockWait = 0

	w, err := AcquireBrowserInputLock(rig.key, "test.window", 0)
	if err != nil || w == nil {
		t.Fatalf("取窗口失败: %v", err)
	}
	defer w.Release()

	type hook struct {
		name string
		run  func() error
	}
	ctx := context.Background()
	hooks := []hook{
		{"newAttachedHeadedActivation", func() error {
			_, err := newAttachedHeadedActivation(rig.tracker)(ctx, w)
			return err
		}},
		{"tracker.ActivateNoFrontIn", func() error { return rig.tracker.ActivateNoFrontIn(w, "page", "test") }},
		{"tracker.ActivateIn", func() error { return rig.tracker.ActivateIn(w, "page", "test") }},
		{"engine.activateFrontmostIn", func() error {
			// 没有 CDP 上下文,ensureTargetFrontmost 本身会失败;这里只关心不是重入/凭证错误。
			_, err := rig.engine.activateFrontmostIn(ctx, w)
			if errors.Is(err, ErrInputWindowNotHeld) || errors.Is(err, ErrInputWindowReentrant) {
				return err
			}
			return nil
		}},
	}
	before := inProcessInputWindowContention.Load()
	for _, h := range hooks {
		if err := h.run(); err != nil {
			t.Errorf("%s 在持有窗口时失败(疑似再次取锁): %v", h.name, err)
		}
	}
	if got := inProcessInputWindowContention.Load() - before; got != 0 {
		t.Fatalf("窗口内钩子造成 %d 次同进程重入, want 0", got)
	}
}

// 窗口外入口仍然自己取锁(语义不变):窗口被占时必须等待并失败,放开后恢复。
func TestOutOfWindowActivationStillAcquiresTheWindow(t *testing.T) {
	rig := newHeadedWindowRig(t)
	rig.tracker.inputLockWait = 0
	w, err := AcquireBrowserInputLock(rig.key, "test.window", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.tracker.activateBrowserTargetNoFront("page", "test"); !errors.Is(err, ErrInputWindowReentrant) {
		t.Fatalf("窗口被占时窗口外入口应当等待失败并说明持有者, got %v", err)
	}
	w.Release()
	if err := rig.tracker.activateBrowserTargetNoFront("page", "test"); err != nil {
		t.Fatalf("放开后窗口外入口没有恢复: %v", err)
	}
}
