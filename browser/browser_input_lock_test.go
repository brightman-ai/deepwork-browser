package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readPackageSource(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func inputLockTestKey(t *testing.T) string {
	t.Helper()
	key := "ws://test-" + t.Name() + "-" + time.Now().Format("150405.000000000")
	t.Cleanup(func() {
		_ = os.Remove(browserInputLockPath(key))
		_ = os.Remove(browserFrontLedgerPath(key))
	})
	return key
}

// 两个操作者共享同一个浏览器实例时,输入必须串行化:一个人还没派发完,
// 另一个人不能把前台抢走。第二把锁按浏览器实例键控,只覆盖派发窗口。
func TestBrowserInputLockSerializesTheDispatchWindow(t *testing.T) {
	key := inputLockTestKey(t)

	first, err := AcquireBrowserInputLock(key, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil {
		t.Fatal("非空 key 却没有取到锁 —— 输入临界区形同虚设")
	}

	started := time.Now()
	second, err := AcquireBrowserInputLock(key, 300*time.Millisecond)
	elapsed := time.Since(started)
	if err == nil {
		second.Release()
		t.Fatal("同一浏览器实例上两个输入派发窗口同时成立 —— 输入没有被串行化")
	}
	if elapsed < 300*time.Millisecond {
		t.Fatalf("等了 %s 就放弃, 少于请求的 300ms", elapsed)
	}
	if !strings.Contains(err.Error(), "输入必须串行") && !strings.Contains(err.Error(), "输入临界区") {
		t.Fatalf("错误信息没说清发生了什么: %v", err)
	}

	first.Release()
	third, err := AcquireBrowserInputLock(key, time.Second)
	if err != nil {
		t.Fatalf("前一个派发窗口关闭后仍取不到锁: %v", err)
	}
	third.Release()
}

// 不同浏览器实例之间互不阻塞 —— 串行化的粒度是浏览器实例,不是全局。
func TestBrowserInputLocksAreScopedPerBrowserInstance(t *testing.T) {
	a := inputLockTestKey(t)
	b := inputLockTestKey(t) + "-other"
	t.Cleanup(func() {
		_ = os.Remove(browserInputLockPath(b))
		_ = os.Remove(browserFrontLedgerPath(b))
	})

	lockA, err := AcquireBrowserInputLock(a, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer lockA.Release()
	lockB, err := AcquireBrowserInputLock(b, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("另一个浏览器实例的输入被挡住了: %v", err)
	}
	lockB.Release()
}

// key 构造不出来(既没有 ws_url 也没有 chrome pid)时不该挡住真实输入:
// 记账问题不配否决用户的一次点击。
func TestEmptyBrowserKeyDoesNotBlockInput(t *testing.T) {
	if BrowserInputKey("", 0) != "" {
		t.Fatal("既没有 ws_url 也没有 pid 却构造出了键")
	}
	lock, err := AcquireBrowserInputLock("", time.Second)
	if err != nil {
		t.Fatalf("空 key 应当直接放行: %v", err)
	}
	lock.Release() // nil 上调用必须安全
}

// ws_url 优先于 pid:同一个浏览器可能被不同进程以不同方式认识,ws_url 才是
// 跨进程稳定的那个身份。
func TestBrowserInputKeyPrefersWebSocketURL(t *testing.T) {
	if got := BrowserInputKey("ws://127.0.0.1:9222/devtools/browser/abc", 4242); got != "ws:ws://127.0.0.1:9222/devtools/browser/abc" {
		t.Fatalf("BrowserInputKey=%q, 期望以 ws_url 为准", got)
	}
	if got := BrowserInputKey("  ", 4242); got != "pid:4242" {
		t.Fatalf("BrowserInputKey=%q, 没有 ws_url 时应退到 chrome pid", got)
	}
}

// 前台被反复抢来抢去 = 多操作者在同一个浏览器上互扰。输入仍是串行的(锁在),
// 但节奏被打乱,吞吐和时序都会变差 —— 这件事必须说出来,不能只是变慢。
func TestFrontFlipBetweenTargetsRaisesContentionWarning(t *testing.T) {
	key := inputLockTestKey(t)

	var warning string
	for i := 0; i < frontFlipThreshold; i++ {
		target := "target-A"
		if i%2 == 1 {
			target = "target-B"
		}
		warning = RecordBringToFront(key, target)
	}
	if warning == "" {
		t.Fatalf("%d 次交替切换前台没有触发互扰告警", frontFlipThreshold)
	}
	for _, want := range []string{"多操作者共享浏览器", "输入已串行化但节奏互扰"} {
		if !strings.Contains(warning, want) {
			t.Fatalf("告警文案缺少 %q:\n%s", want, warning)
		}
	}
}

// 实测最常见的互扰形态:同一个 tab 在不到 1s 内被四个不同的 CLI 进程各自重新
// 提了一次前台。只认"不同 target"会把这种形态整个漏掉 —— 而它恰恰是共享浏览器
// 下两个 agent 盯同一页时的样子。
func TestSameTargetFlippedByDifferentOperatorsWarns(t *testing.T) {
	now := time.Now()
	events := []frontEvent{
		{Target: "T", PID: 1001, At: now.Add(-700 * time.Millisecond)},
		{Target: "T", PID: 1002, At: now.Add(-460 * time.Millisecond)},
		{Target: "T", PID: 1003, At: now.Add(-230 * time.Millisecond)},
		{Target: "T", PID: 1004, At: now},
	}
	warning := frontFlipWarning(events)
	if warning == "" {
		t.Fatal("同一个 tab 被四个不同进程反复抢前台没有触发互扰告警")
	}
	if !strings.Contains(warning, "4 个进程") {
		t.Fatalf("告警没有点出有几个进程在抢:\n%s", warning)
	}
}

// 单操作者在自己的一个 tab 上连续动作不该被误报。误报会训练人忽略告警,
// 那比不报还糟。
func TestSingleTargetRepeatsDoNotWarn(t *testing.T) {
	key := inputLockTestKey(t)
	for i := 0; i < frontFlipThreshold+2; i++ {
		if warning := RecordBringToFront(key, "target-A"); warning != "" {
			t.Fatalf("同一个 target 上反复提前台被误报成互扰:\n%s", warning)
		}
	}
}

// 窗口之外的老切换不算数:两小时里切了四次不是互扰,五秒里切了四次才是。
func TestFrontFlipsOutsideTheWindowDoNotWarn(t *testing.T) {
	now := time.Now()
	spread := []frontEvent{
		{Target: "A", PID: 1, At: now.Add(-60 * time.Second)},
		{Target: "B", PID: 2, At: now.Add(-40 * time.Second)},
		{Target: "A", PID: 1, At: now.Add(-20 * time.Second)},
		{Target: "B", PID: 2, At: now},
	}
	if warning := frontFlipWarning(spread); warning != "" {
		t.Fatalf("跨 60s 的四次切换被判成互扰:\n%s", warning)
	}

	dense := []frontEvent{
		{Target: "A", PID: 1, At: now.Add(-800 * time.Millisecond)},
		{Target: "B", PID: 2, At: now.Add(-600 * time.Millisecond)},
		{Target: "A", PID: 1, At: now.Add(-300 * time.Millisecond)},
		{Target: "B", PID: 2, At: now},
	}
	if warning := frontFlipWarning(dense); warning == "" {
		t.Fatal("5s 内四次交替切换没有被判成互扰")
	}
}

// 账本是跨进程共享的诊断附注,不该无限增长。
func TestFrontLedgerIsPrunedToTheWindow(t *testing.T) {
	now := time.Now()
	events := []frontEvent{{Target: "old", PID: 1, At: now.Add(-2 * frontFlipWindow)}}
	for i := 0; i < 100; i++ {
		events = append(events, frontEvent{Target: "x", PID: 1, At: now})
	}
	pruned := pruneFrontEvents(events, now)
	if len(pruned) > 32 {
		t.Fatalf("账本长度 %d, 没有被裁剪", len(pruned))
	}
	for _, e := range pruned {
		if e.Target == "old" {
			t.Fatal("窗口之外的旧记录仍留在账本里")
		}
	}
}

// 锁与账本落在 /tmp 下的固定目录:TMPDIR 因人而异,不是跨进程的同一命名空间,
// 拿它做键控目录等于两个操作者各锁各的。
func TestBrowserInputLockDirIsProcessGlobal(t *testing.T) {
	if !strings.HasPrefix(browserInputLocksDir, "/tmp") {
		t.Fatalf("browserInputLocksDir=%q 不在进程全局命名空间下", browserInputLocksDir)
	}
	key := "ws://x"
	if filepath.Dir(browserInputLockPath(key)) != browserInputLocksDir {
		t.Fatal("锁文件没有落在键控目录下")
	}
}
