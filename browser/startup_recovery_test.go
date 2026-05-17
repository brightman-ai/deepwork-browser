// Package browser — startup recovery tests [v2 ]
//
// 绑定 TC (T6 v2):
// - (新): Singleton lock 残留检测 (dead PID → 清理)
// - (新): orphan PID alive → SIGTERM/SIGKILL 强杀路径 (skipped on CI: 需 spawn sleep)
// - TC-09-L4-15 (新): profile 损坏 → .broken/ 隔离
// - 健康路径: 完整 SQLite Cookies → recovery 通过, profileDir 不变
package browser

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRecovery_HealthyEmptyProfile_NoOp — 不存在的 profileDir → 视为首次启动, 不报错.
func TestRecovery_HealthyEmptyProfile_NoOp(t *testing.T) {
	dir := filepath.Join(t.TempDir, "no-such-profile")
	if err := RunStartupRecovery(dir, IdentityKey("test-key")); err != nil {
		t.Fatalf("non-existent profile should be no-op, got: %v", err)
	}
	// 确认未创建 (recovery 不应该 mkdir, mkdir 由 startChromeLocked 做)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("recovery should not create profile dir, got stat err=%v", err)
	}
}

// TestRecovery_HealthyCookies_NoOp — 合法 SQLite header → 通过, profileDir 不变.
func TestRecovery_HealthyCookies_NoOp(t *testing.T) {
	dir := t.TempDir
	cookiesPath := filepath.Join(dir, "Cookies")
	// 合法 SQLite header + 一些 padding
	content := append(byte("SQLite format 3\x00"), make(byte, 100)...)
	if err := os.WriteFile(cookiesPath, content, 0644); err != nil {
		t.Fatal(err)
	}

	if err := RunStartupRecovery(dir, IdentityKey("k")); err != nil {
		t.Fatalf("healthy profile recovery: %v", err)
	}
	// profileDir 仍存在
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("profile dir should still exist: %v", err)
	}
	// Cookies 仍在
	if _, err := os.Stat(cookiesPath); err != nil {
		t.Fatalf("Cookies should still exist: %v", err)
	}
}

func TestRecovery_ClearsChromeCrashRestoreState(t *testing.T) {
	dir := t.TempDir
	defaultDir := filepath.Join(dir, "Default")
	if err := os.MkdirAll(defaultDir, 0755); err != nil {
		t.Fatal(err)
	}
	for _, path := range string{
		filepath.Join(dir, "Local State")
		filepath.Join(defaultDir, "Preferences")
	} {
		body := byte(`{"profile":{"exit_type":"Crashed","exited_cleanly":false},"keep":"value"}`)
		if err := os.WriteFile(path, body, 0644); err != nil {
			t.Fatal(err)
		}
	}

	if err := RunStartupRecovery(dir, IdentityKey("k-restore")); err != nil {
		t.Fatalf("recovery: %v", err)
	}

	for _, path := range string{
		filepath.Join(dir, "Local State")
		filepath.Join(defaultDir, "Preferences")
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal(body, &data); err != nil {
			t.Fatal(err)
		}
		profile := data["profile"].(map[string]any)
		if profile["exit_type"] != "Normal" || profile["exited_cleanly"] != true {
			t.Fatalf("crash restore state not cleared in %s: %#v", path, profile)
		}
		if data["keep"] != "value" {
			t.Fatalf("unrelated JSON field changed in %s: %#v", path, data)
		}
	}
}

func TestRecovery_ProfileOwnerDeadMarkerCleaned(t *testing.T) {
	dir := t.TempDir
	if err := os.WriteFile(filepath.Join(dir, deepworkProfileOwnerFile), byte(`{"owner_pid":9999998,"chrome_pid":9999997,"identity_key":"k-owner"}`), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RunStartupRecovery(dir, IdentityKey("k-owner")); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, deepworkProfileOwnerFile)); !os.IsNotExist(err) {
		t.Fatalf("owner marker should be removed, got stat err=%v", err)
	}
}

func TestRecovery_LegacyLiveOwnerDeadChromeMarkerCleaned(t *testing.T) {
	dir := t.TempDir
	owner := exec.Command("sleep", "30")
	if err := owner.Start; err != nil {
		t.Fatalf("start owner process: %v", err)
	}
	t.Cleanup(func {
		_ = owner.Process.Kill
		_, _ = owner.Process.Wait
	})
	body := fmt.Sprintf(`{"owner_pid":%d,"chrome_pid":9999997,"identity_key":"k-owner"}`, owner.Process.Pid)
	if err := os.WriteFile(filepath.Join(dir, deepworkProfileOwnerFile), byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RunStartupRecovery(dir, IdentityKey("k-owner")); err != nil {
		t.Fatalf("legacy live owner with dead chrome should be recoverable, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, deepworkProfileOwnerFile)); !os.IsNotExist(err) {
		t.Fatalf("owner marker should be removed, got stat err=%v", err)
	}
}

func TestRecovery_LegacyLiveOwnerLiveChromeBlocks(t *testing.T) {
	dir := t.TempDir
	owner := exec.Command("sleep", "30")
	chrome := exec.Command("sleep", "30")
	if err := owner.Start; err != nil {
		t.Fatalf("start owner process: %v", err)
	}
	if err := chrome.Start; err != nil {
		_ = owner.Process.Kill
		_, _ = owner.Process.Wait
		t.Fatalf("start chrome process: %v", err)
	}
	t.Cleanup(func {
		_ = owner.Process.Kill
		_, _ = owner.Process.Wait
		_ = chrome.Process.Kill
		_, _ = chrome.Process.Wait
	})
	body := fmt.Sprintf(`{"owner_pid":%d,"chrome_pid":%d,"identity_key":"k-owner"}`, owner.Process.Pid, chrome.Process.Pid)
	if err := os.WriteFile(filepath.Join(dir, deepworkProfileOwnerFile), byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	err := RunStartupRecovery(dir, IdentityKey("k-owner"))
	if err == nil || !strings.Contains(err.Error, "profile_owned_by_live_deepwork") {
		t.Fatalf("expected live legacy owner+chrome to block, got: %v", err)
	}
}

func TestRecoverySweep_DeadBrowserMuxHostKillsOrphanChrome(t *testing.T) {
	dataDir := t.TempDir
	profileDir := filepath.Join(dataDir, "browser-data", "profiles", "ok3", "macos-chrome-v6")
	if err := os.MkdirAll(profileDir, 0755); err != nil {
		t.Fatal(err)
	}
	chrome := exec.Command("sleep", "30")
	if err := chrome.Start; err != nil {
		t.Fatalf("start chrome process: %v", err)
	}
	chromeDone := make(chan error, 1)
	go func {
		chromeDone <- chrome.Wait
		close(chromeDone)
	}
	t.Cleanup(func {
		if isPIDAlive(chrome.Process.Pid) {
			_ = chrome.Process.Kill
		}
		select {
		case <-chromeDone:
		case <-time.After(2 * time.Second):
		}
	})
	body := fmt.Sprintf(`{
		"owner_pid": 9999998
		"chrome_pid": %d
		"identity_key": "k-muxhost-orphan"
		"browser_mux_host_id": "browser-mux-host-test"
		"browser_mux_host_pid": 9999997
	}`, chrome.Process.Pid)
	if err := os.WriteFile(filepath.Join(profileDir, deepworkProfileOwnerFile), byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RecoverBrowserRuntimeState(dataDir); err != nil {
		t.Fatalf("sweep recovery: %v", err)
	}
	select {
	case <-chromeDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("orphan chrome pid %d should exit after recovery", chrome.Process.Pid)
	}
	if _, err := os.Stat(filepath.Join(profileDir, deepworkProfileOwnerFile)); !os.IsNotExist(err) {
		t.Fatalf("owner marker should be removed, got stat err=%v", err)
	}
}

func TestRecoverySweep_LiveBrowserMuxHostSkipped(t *testing.T) {
	dataDir := t.TempDir
	profileDir := filepath.Join(dataDir, "browser-data", "profiles", "ok3", "macos-chrome-v6")
	if err := os.MkdirAll(profileDir, 0755); err != nil {
		t.Fatal(err)
	}
	host := exec.Command("sleep", "30")
	chrome := exec.Command("sleep", "30")
	if err := host.Start; err != nil {
		t.Fatalf("start host process: %v", err)
	}
	if err := chrome.Start; err != nil {
		_ = host.Process.Kill
		_, _ = host.Process.Wait
		t.Fatalf("start chrome process: %v", err)
	}
	t.Cleanup(func {
		_ = host.Process.Kill
		_, _ = host.Process.Wait
		_ = chrome.Process.Kill
		_, _ = chrome.Process.Wait
	})
	body := fmt.Sprintf(`{
		"owner_pid": %d
		"chrome_pid": %d
		"identity_key": "k-live-muxhost"
		"browser_mux_host_id": "browser-mux-host-test-live"
		"browser_mux_host_pid": %d
	}`, host.Process.Pid, chrome.Process.Pid, host.Process.Pid)
	if err := os.WriteFile(filepath.Join(profileDir, deepworkProfileOwnerFile), byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RecoverBrowserRuntimeState(dataDir); err != nil {
		t.Fatalf("sweep should skip live muxhost without failing, got: %v", err)
	}
	if !isPIDAlive(host.Process.Pid) {
		t.Fatalf("live muxhost pid %d should not be killed", host.Process.Pid)
	}
	if !isPIDAlive(chrome.Process.Pid) {
		t.Fatalf("live chrome pid %d should not be killed", chrome.Process.Pid)
	}
	if _, err := os.Stat(filepath.Join(profileDir, deepworkProfileOwnerFile)); err != nil {
		t.Fatalf("live owner marker should remain, got stat err=%v", err)
	}
}

func TestRecovery_ProfileOwnedBySameProcessRecoverable(t *testing.T) {
	dir := t.TempDir
	body := fmt.Sprintf(`{"owner_pid":%d,"chrome_pid":9999997,"identity_key":"k-owner"}`, os.Getpid)
	if err := os.WriteFile(filepath.Join(dir, deepworkProfileOwnerFile), byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RunStartupRecovery(dir, IdentityKey("k-owner")); err != nil {
		t.Fatalf("same-process owner marker should be treated as recoverable, got: %v", err)
	}
}

// TestRecovery_CorruptedSQLiteHeader_Quarantined — 损坏 header → 隔离.
func TestRecovery_CorruptedSQLiteHeader_Quarantined(t *testing.T) {
	dir := t.TempDir
	cookiesPath := filepath.Join(dir, "Cookies")
	// 错误 magic header
	if err := os.WriteFile(cookiesPath, byte("CORRUPTED_HEADER"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RunStartupRecovery(dir, IdentityKey("k-corrupt")); err != nil {
		t.Fatalf("recovery should succeed after quarantine: %v", err)
	}
	// 原 dir 应已被 rename 走
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected profile dir quarantined (gone), got stat err=%v", err)
	}
	// .broken/ 应有内容
	brokenRoot := dir + ".broken"
	st, err := os.Stat(brokenRoot)
	if err != nil {
		t.Fatalf(".broken root not created: %v", err)
	}
	if !st.IsDir {
		t.Fatalf(".broken should be dir")
	}
	entries, err := os.ReadDir(brokenRoot)
	if err != nil || len(entries) == 0 {
		t.Fatalf(".broken should have at least 1 timestamped entry, got %d (err=%v)", len(entries), err)
	}
}

// TestRecovery_EmptyCookies_Quarantined — 空 Cookies 文件 = Chrome init crash → 隔离.
func TestRecovery_EmptyCookies_Quarantined(t *testing.T) {
	dir := t.TempDir
	cookiesPath := filepath.Join(dir, "Cookies")
	if err := os.WriteFile(cookiesPath, byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	if err := RunStartupRecovery(dir, IdentityKey("k-empty")); err != nil {
		t.Fatalf("recovery should succeed after quarantine: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("expected profile dir quarantined (gone), got stat err=%v", err)
	}
}

// TestRecovery_SingletonLockDeadPID_Cleaned — SingletonLock 指向已死 PID → 删除.
//
// 用 PID=1 (init) 反向: 选一个肯定不会存活的 PID (max int32 - 1).
// init pid=1 可能存活, 用 9999998 这种几乎不可能被分配的 PID.
func TestRecovery_SingletonLockDeadPID_Cleaned(t *testing.T) {
	dir := t.TempDir
	lockPath := filepath.Join(dir, "SingletonLock")
	// 创建一个指向"已死"PID 的 symlink (Chrome 的格式: hostname-PID)
	deadPID := 9999998
	if err := os.Symlink(fmt.Sprintf("hostname-%d", deadPID), lockPath); err != nil {
		t.Skipf("symlink creation failed (filesystem limitation): %v", err)
	}
	// 同时创建一个 SingletonCookie 普通文件
	if err := os.WriteFile(filepath.Join(dir, "SingletonCookie"), byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := RunStartupRecovery(dir, IdentityKey("k-deadlock")); err != nil {
		t.Fatalf("recovery: %v", err)
	}

	// SingletonLock 应已删除
	if _, err := os.Lstat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("SingletonLock should be removed, got stat err=%v", err)
	}
	// SingletonCookie 也应被清理
	if _, err := os.Stat(filepath.Join(dir, "SingletonCookie")); !os.IsNotExist(err) {
		t.Fatalf("SingletonCookie should be removed, got stat err=%v", err)
	}
	// profileDir 仍存在 (没有 Cookies → 不触发隔离)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("profile dir should still exist after lock cleanup: %v", err)
	}
}

// TestRecovery_readSingletonLockPID — symlink target 解析正反例.
func TestRecovery_readSingletonLockPID(t *testing.T) {
	dir := t.TempDir
	cases := struct {
		name string
		target string
		expect int
		err bool
	}{
		{"standard chrome format", "MacBook-Pro-12345", 12345, false}
		{"long hostname", "my.host.example.com-99999", 99999, false}
		{"no PID", "MacBook-Pro", 0, true}
		{"non-numeric", "MacBook-Pro-abc", 0, true}
		{"zero PID", "MacBook-Pro-0", 0, true}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lockPath := filepath.Join(dir, c.name+"-lock")
			if err := os.Symlink(c.target, lockPath); err != nil {
				t.Skip(err)
			}
			defer os.Remove(lockPath)

			pid, err := readSingletonLockPID(lockPath)
			if c.err {
				if err == nil {
					t.Fatalf("expected error for target=%q", c.target)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pid != c.expect {
				t.Fatalf("expected pid=%d, got %d", c.expect, pid)
			}
		})
	}
}

// TestRecovery_isPIDAlive — 自己 PID 存活, 大数 PID 不存活.
func TestRecovery_isPIDAlive(t *testing.T) {
	if !isPIDAlive(os.Getpid) {
		t.Fatalf("self PID should be alive")
	}
	if isPIDAlive(9999998) {
		t.Skip("PID 9999998 happens to exist on this system; cannot test dead-PID path")
	}
	if isPIDAlive(0) || isPIDAlive(-1) {
		t.Fatalf("invalid PIDs should be reported dead")
	}
}

// TestRecovery_QuarantineAuditMessage — 隔离时 reason 包含在 .broken/ 路径? 用 log capture 验证不便
// 此处仅断言 .broken/{ts}/ 子目录格式正确 (UTC 时间戳).
func TestRecovery_QuarantineTimestampFormat(t *testing.T) {
	dir := t.TempDir
	if err := os.WriteFile(filepath.Join(dir, "Cookies"), byte("BAD"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := RunStartupRecovery(dir, IdentityKey("k-ts")); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	entries, _ := os.ReadDir(dir + ".broken")
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry in .broken/, got %d", len(entries))
	}
	name := entries[0].Name
	// 格式: YYYYMMDDTHHMMSSZ (16 字符)
	if len(name) != 16 || !strings.HasSuffix(name, "Z") {
		t.Fatalf("expected timestamp format YYYYMMDDTHHMMSSZ (len=16, ends with Z), got %q", name)
	}
}
