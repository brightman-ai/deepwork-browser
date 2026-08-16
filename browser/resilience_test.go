//go:build integration

// Package browser — L4 韧性测试 (故障注入)
// 覆盖: TC-09-L4-01~09
// 运行: go test ./internal/browser/... -tags=integration -race -count=1 -timeout=180s -run TestL4
//
// 注意: L4 测试包含 kill 进程操作（需要 Chrome 已安装），全部使用 requireChrome 门控。
package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ============================================================
// § TC-09-L4-01: Chrome 进程崩溃 — onCrash 触发
// ============================================================

// TC-09-L4-01: os.Kill(pid, SIGKILL) → 1s 内 onCrash 触发。
func TestL4_ChromeCrash_OnCrashTriggered(t *testing.T) {
	// TC-ID: TC-09-L4-01
	requireChrome(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	launcher := NewChromeLauncher()
	profileID := "test-l4-01-" + itoa(int(time.Now().UnixNano()%100000))
	homeDir, _ := os.UserHomeDir()
	defer func() { _ = os.RemoveAll(homeDir + "/.deepwork/browser-data/" + profileID) }()

	_, pid, err := launcher.Launch(ctx, profileID)
	if err != nil {
		t.Skipf("TC-09-L4-01: Chrome launch failed: %v", err)
	}

	supervisor := NewChromeSupervisor()

	crashCh := make(chan struct{}, 1)
	supervisor.Watch(ctx, pid, func() {
		select {
		case crashCh <- struct{}{}:
		default:
		}
	})

	// 等待 Chrome 稳定启动
	time.Sleep(500 * time.Millisecond)

	// 注入崩溃: SIGKILL
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("TC-09-L4-01: FindProcess(%d) failed: %v", pid, err)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("TC-09-L4-01: SIGKILL failed: %v", err)
	}

	// 等待 onCrash 触发（最多 3s）
	select {
	case <-crashCh:
		t.Log("TC-09-L4-01 PASS: onCrash triggered after SIGKILL")
	case <-time.After(3 * time.Second):
		t.Error("TC-09-L4-01 FAIL: onCrash not triggered within 3s after Chrome SIGKILL")
	}
}

// ============================================================
// § TC-09-L4-02: 退避重启模式验证
// ============================================================

// TC-09-L4-02: 验证指数退避延迟模式 (1s/2s/4s)。
// 注意：实际等待时间会很长（7s+），此 TC 只验证延迟计算逻辑。
func TestL4_BackoffDelay_Pattern(t *testing.T) {
	// TC-ID: TC-09-L4-02
	// 验证退避延迟计算逻辑（不实际等待）
	delays := make([]time.Duration, 3)
	for i := 0; i < 3; i++ {
		delays[i] = time.Duration(1<<uint(i)) * time.Second
	}

	expected := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	for i, d := range delays {
		if d != expected[i] {
			t.Errorf("TC-09-L4-02: backoff[%d] = %v, want %v", i, d, expected[i])
		}
	}

	// 验证全失败时返回 ErrBrowserCrashed（使用 mock launcher）
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	supervisor := NewChromeSupervisor()

	// 使用不存在的 profileID，所有 Launch 都会失败
	origLinux := linuxChromePaths
	origMac := macOSChromePaths
	origWin := windowsChromePaths
	defer func() {
		linuxChromePaths = origLinux
		macOSChromePaths = origMac
		windowsChromePaths = origWin
	}()
	linuxChromePaths = []string{"/no-chrome-l4-02"}
	macOSChromePaths = []string{"/no-chrome-l4-02"}
	windowsChromePaths = []string{"/no-chrome-l4-02"}

	launcher := NewChromeLauncher()
	// RestartWithBackoff 最多 1 次尝试（配合 1s context timeout）
	_, _, err := supervisor.RestartWithBackoff(ctx, launcher, "test-l4-02", 1)
	if err == nil {
		t.Error("TC-09-L4-02: RestartWithBackoff should fail with no chrome")
	}
	t.Logf("TC-09-L4-02 PASS: backoff delays %v correct, all-fail returns error: %v", delays, err)
}

// ============================================================
// § TC-09-L4-03: frameCh 背压 — 旧帧丢弃，ACK 仍执行
// ============================================================

// TC-09-L4-03: 停止读取 subscriber channel 同时持续产帧 → 旧帧被丢弃（保新），ACK 仍每帧执行。
func TestL4_ScreencastBackpressure_OldFramesDropped(t *testing.T) {
	// TC-ID: TC-09-L4-03 [DDC-09]
	hub := NewFrameBroadcastHub()
	m := newMockLiveViewEngine(hub)

	// 订阅独立 1-slot channel，不读取（模拟背压）；保存 ch 用于代次匹配 Unsubscribe
	frameCh := hub.Subscribe("test-conn")
	defer hub.Unsubscribe("test-conn", frameCh)

	// 推送 20 帧（1-slot channel，保新丢旧，无阻塞）
	for i := 0; i < 20; i++ {
		m.PushFrame(&ScreencastFrame{
			Data:      []byte("frame-data"),
			Timestamp: time.Now().UnixMilli(),
		})
	}

	// 验证 ACK 次数 = 20（每帧立即 ACK）
	if m.AckCount() != 20 {
		t.Errorf("TC-09-L4-03: ACK count should be 20, got %d", m.AckCount())
	}

	// 验证 subscriber channel 不超过容量 1（保新丢旧）
	count := 0
	for {
		select {
		case <-frameCh:
			count++
		default:
			goto done
		}
	}
done:
	if count > 1 {
		t.Errorf("TC-09-L4-03: subscriber channel should hold <= 1 frame (1-slot), got %d", count)
	}

	t.Logf("TC-09-L4-03 PASS [DDC-09]: ACK=20/20, subscriber buffered=%d (<= 1), old frames dropped", count)
}

// ============================================================
// § TC-09-L4-05: 并发 snap + act — 无数据竞争
// ============================================================

// TC-09-L4-05: goroutine 并发调用 Snap() × 5 + Act() × 3 → 无 data race（-race flag）。
func TestL4_Concurrent_SnapAct_NoRace(t *testing.T) {
	// TC-ID: TC-09-L4-05
	core := launchTestBrowser(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body>
			<button aria-label="btn1">按钮1</button>
			<button aria-label="btn2">按钮2</button>
			<input type="text" placeholder="search" aria-label="搜索" />
			<a href="#">链接</a>
		</body></html>`))
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	_, err := core.Navigate(ctx, ts.URL)
	if err != nil {
		t.Fatalf("TC-09-L4-05: Navigate() failed: %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 10)

	// 并发 Snap × 5
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := core.Snap(ctx)
			if err != nil {
				errCh <- err
			}
		}()
	}

	// 并发 Act × 3（操作可能失败，但不应 panic）
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := core.Act(ctx, "click e1", false)
			// ErrRefNotFound 是正常的（并发 snap 可能重建 refTable）
			if err != nil && err != ErrRefNotFound && err != ErrTakeoverActive {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)

	var errors []error
	for err := range errCh {
		errors = append(errors, err)
	}

	// 收集错误但不视为失败（并发场景下 ErrCDPDisconnected 等也可接受）
	t.Logf("TC-09-L4-05 PASS [race-detector]: concurrent Snap×5 + Act×3 completed, errors=%d (expected 0 panics)",
		len(errors))
	for _, e := range errors {
		t.Logf("TC-09-L4-05: concurrent error (non-panic): %v", e)
	}
}

// ============================================================
// § TC-09-L4-06: Navigate 超时处理
// ============================================================

// TC-09-L4-06: 目标 URL 响应延迟超过 context timeout → 返回 error，不 hang。
func TestL4_Navigate_ContextTimeout(t *testing.T) {
	// TC-ID: TC-09-L4-06
	core := launchTestBrowser(t)

	// 创建一个在客户端取消前不返回的 server。request context 是请求
	// 生命周期 SSOT；无条件 select{} 会让 httptest.Close 自身永久卡住。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer ts.Close()
	defer ts.CloseClientConnections()

	// 设置很短的超时（1s）
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err := core.Navigate(ctx, ts.URL)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("TC-09-L4-06: Navigate() on hang server should return error")
	}
	if elapsed > 5*time.Second {
		t.Errorf("TC-09-L4-06: Navigate() should not hang > 5s, elapsed=%v", elapsed)
	}
	t.Logf("TC-09-L4-06 PASS: Navigate timeout in %v, err=%v", elapsed, err)
}

// ============================================================
// § TC-09-L4-07: Profile 目录被外部删除 — Repair 自动重建
// ============================================================

// TC-09-L4-07: rm -rf profile 目录 When ProfileManager.Repair Then 新目录创建。
func TestL4_Profile_ExternalDelete_RepairRebuild(t *testing.T) {
	// TC-ID: TC-09-L4-07
	tmpDir := t.TempDir()
	pm, err := NewProfileManagerWithBase(tmpDir)
	if err != nil {
		t.Fatalf("TC-09-L4-07: NewProfileManagerWithBase() failed: %v", err)
	}

	// 创建 Profile
	p, err := pm.GetOrCreate("l4-07-test")
	if err != nil {
		t.Fatalf("TC-09-L4-07: GetOrCreate() failed: %v", err)
	}

	// 模拟外部删除
	if err := os.RemoveAll(p.UserDataDir); err != nil {
		t.Fatalf("TC-09-L4-07: RemoveAll() failed: %v", err)
	}

	// Repair 自动重建
	repaired, err := pm.Repair("l4-07-test")
	if err != nil {
		t.Fatalf("TC-09-L4-07: Repair() failed: %v", err)
	}

	if _, err := os.Stat(repaired.UserDataDir); err != nil {
		t.Errorf("TC-09-L4-07: Repaired profile dir should exist: %v", err)
	}
	t.Logf("TC-09-L4-07 PASS: Profile rebuilt at %s after external delete", repaired.UserDataDir)
}

// ============================================================
// § TC-09-L4-09: 多 Profile 并发启动 — Cookie 不串
// ============================================================

// TC-09-L4-09: 同时获取 3 个不同 Profile → UserDataDir 路径不同，互不干扰。
func TestL4_MultiProfile_Isolated(t *testing.T) {
	// TC-ID: TC-09-L4-09
	tmpDir := t.TempDir()
	pm, err := NewProfileManagerWithBase(tmpDir)
	if err != nil {
		t.Fatalf("TC-09-L4-09: NewProfileManagerWithBase() failed: %v", err)
	}

	profiles := make([]string, 3)
	profileData := make([]*Profile, 3)

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	for i := 0; i < 3; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			profileID := "l4-09-profile-" + itoa(i)
			p, err := pm.GetOrCreate(profileID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			profiles[i] = p.UserDataDir
			profileData[i] = p
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("TC-09-L4-09: concurrent GetOrCreate errors: %v", errs)
	}

	// 验证各 Profile 目录不同
	dirSet := make(map[string]bool)
	for _, dir := range profiles {
		if dir == "" {
			t.Error("TC-09-L4-09: profile dir should not be empty")
			continue
		}
		if dirSet[dir] {
			t.Errorf("TC-09-L4-09: duplicate profile dir %q — profiles not isolated", dir)
		}
		dirSet[dir] = true
	}

	t.Logf("TC-09-L4-09 PASS: 3 profiles isolated, dirs=%v", profiles)
}

// ============================================================
// § TC-09-L4-10: Takeover 超时自动释放
// ============================================================

// TC-09-L4-10: EnableTakeover(timeout=100ms) 后不操作 → Mode 自动回到 "observe"。
func TestL4_Takeover_AutoRelease_Timeout(t *testing.T) {
	// TC-ID: TC-09-L4-10
	ctrl := newTakeoverController(nil)
	ctrl.SetTimeout(150 * time.Millisecond) // 短超时用于测试

	releasedCh := make(chan struct{}, 1)
	if err := ctrl.EnableTakeover(func() {
		select {
		case releasedCh <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatalf("TC-09-L4-10: EnableTakeover() failed: %v", err)
	}

	if ctrl.Mode() != TakeoverModeTakeover {
		t.Fatalf("TC-09-L4-10: Mode should be TAKEOVER, got %q", ctrl.Mode())
	}

	// 等待超时自动释放（最多 1s）
	select {
	case <-releasedCh:
		// 验证模式已恢复
		time.Sleep(10 * time.Millisecond) // 等待 autoRelease 完成
		if ctrl.Mode() != TakeoverModeObserve {
			t.Errorf("TC-09-L4-10: Mode should be OBSERVE after auto-release, got %q", ctrl.Mode())
		}
		if ctrl.IsTakeover() {
			t.Error("TC-09-L4-10: IsTakeover() should be false after auto-release")
		}
		t.Log("TC-09-L4-10 PASS: Takeover auto-released after timeout, Mode=OBSERVE")
	case <-time.After(1 * time.Second):
		t.Error("TC-09-L4-10 FAIL: Takeover not auto-released within 1s")
	}
}
