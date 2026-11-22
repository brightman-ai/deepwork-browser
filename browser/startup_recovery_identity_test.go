//go:build !windows

package browser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// profile owner marker 里的 PID 可能是陈旧的（重启之后被系统分给了不相干的程序，例如用户正在用的别的守护进程）。
// 启动恢复只对命令行确实是"我们自己启动的那种进程"的 PID 发信号：host 必须是
// `<…/dw-browser> muxhost serve --muxhost-id <marker 里的 host id>`，Chrome 必须带 `--user-data-dir=<本 profile>`。
// 对不上：一个信号都不发、不把 profile 判为被占用而永久卡死、留下审计日志——并且那个进程仍然活着。

func writeMarker(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, deepworkProfileOwnerFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stillAliveAfterRecovery(t *testing.T, pid int) {
	t.Helper()
	time.Sleep(150 * time.Millisecond) // killAndWait 的 SIGTERM 是同步发出的；给它一点时间，才看得出"没发"
	if !isPIDAlive(pid) {
		t.Fatalf("pid %d was signalled although its command line is not what the marker claims", pid)
	}
}

// 陈旧 marker：host pid 现在属于一个无关进程。
func TestRecoveryNeverSignalsAHostPidThatIsNotTheHost(t *testing.T) {
	dir := t.TempDir()
	stranger := spawnLongSleeper(t)
	writeMarker(t, dir, fmt.Sprintf(`{"owner_pid":%d,"chrome_pid":%d,"identity_key":"k","browser_mux_host_id":"browser-mux-host-global","browser_mux_host_pid":%d}`,
		stranger, deadPID(t), stranger))
	if err := RunStartupRecovery(dir, IdentityKey("k")); err != nil {
		t.Fatalf("an unidentifiable owner must not block the profile forever: %v", err)
	}
	stillAliveAfterRecovery(t, stranger)
	if _, err := os.Stat(filepath.Join(dir, deepworkProfileOwnerFile)); !os.IsNotExist(err) {
		t.Fatalf("the stale marker is removed (it was only a record; nothing was signalled): %v", err)
	}
}

// marker 里没有 host id：无从核对，同样不发信号。
func TestRecoveryNeverSignalsAHostPidWhenTheMarkerHasNoHostID(t *testing.T) {
	dir := t.TempDir()
	stranger := spawnLongSleeper(t)
	writeMarker(t, dir, fmt.Sprintf(`{"owner_pid":%d,"chrome_pid":%d,"identity_key":"k","browser_mux_host_pid":%d}`, stranger, deadPID(t), stranger))
	if err := RunStartupRecovery(dir, IdentityKey("k")); err != nil {
		t.Fatal(err)
	}
	stillAliveAfterRecovery(t, stranger)
}

// host id 对不上（别的 host 的进程）：同样不发信号。
func TestRecoveryNeverSignalsAnotherHostsProcess(t *testing.T) {
	dir := t.TempDir()
	other := spawnMuxHostLookalike(t, "some-other-host")
	writeMarker(t, dir, fmt.Sprintf(`{"owner_pid":%d,"chrome_pid":%d,"identity_key":"k","browser_mux_host_id":"browser-mux-host-global","browser_mux_host_pid":%d}`,
		other, deadPID(t), other))
	if err := RunStartupRecovery(dir, IdentityKey("k")); err != nil {
		t.Fatal(err)
	}
	stillAliveAfterRecovery(t, other)
}

// 陈旧 marker：Chrome pid 现在属于一个无关进程（没有 --user-data-dir=<本 profile>）。
func TestRecoveryNeverSignalsAChromePidThatIsNotThisProfilesChrome(t *testing.T) {
	dir := t.TempDir()
	stranger := spawnLongSleeper(t)
	writeMarker(t, dir, fmt.Sprintf(`{"owner_pid":%d,"chrome_pid":%d,"identity_key":"k"}`, deadPID(t), stranger))
	if err := RunStartupRecovery(dir, IdentityKey("k")); err != nil {
		t.Fatalf("a marker pointing at an unidentifiable process must not block the profile: %v", err)
	}
	stillAliveAfterRecovery(t, stranger)
}

// 别的 profile 的 Chrome（目录是本目录的前缀/兄弟）不是本 profile 的 Chrome。
func TestRecoveryNeverSignalsAChromeOfAnotherProfile(t *testing.T) {
	parent := t.TempDir()
	dir, sibling := filepath.Join(parent, "p"), filepath.Join(parent, "p2")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	other := spawnChromeLookalike(t, sibling)
	writeMarker(t, dir, fmt.Sprintf(`{"owner_pid":%d,"chrome_pid":%d,"identity_key":"k"}`, deadPID(t), other))
	if err := RunStartupRecovery(dir, IdentityKey("k")); err != nil {
		t.Fatal(err)
	}
	stillAliveAfterRecovery(t, other)
}

// 正例：确实是我们自己的 host / 这个 profile 的 Chrome——照旧被清理（身份核对没有把清理能力一并拿掉）。
func TestRecoveryStillStopsOurOwnStaleHostAndChrome(t *testing.T) {
	dir := t.TempDir()
	host := spawnMuxHostLookalike(t, "browser-mux-host-test")
	chrome := spawnChromeLookalike(t, dir)
	writeMarker(t, dir, fmt.Sprintf(`{"owner_pid":%d,"chrome_pid":%d,"identity_key":"k","browser_mux_host_id":"browser-mux-host-test","browser_mux_host_pid":%d}`,
		host, chrome, host))
	// host 活着 + Chrome 活着：profile 确实有主人，拒绝（而不是杀）。
	if err := RunStartupRecovery(dir, IdentityKey("k")); err == nil || !strings.Contains(err.Error(), "profile_owned_by_live_browser_mux_host") {
		t.Fatalf("a live, identifiable owner must still block: %v", err)
	}
	if !isPIDAlive(host) || !isPIDAlive(chrome) {
		t.Fatal("a live owner must not be killed")
	}
	// host 活着、Chrome 已死：host 是陈旧的，杀它（它确实是我们的 host）。
	if !WaitForProcessExit(chrome, 0) {
		_ = killPIDForce(chrome)
		if !WaitForProcessExit(chrome, 2*time.Second) {
			t.Fatal("could not stop the chrome look-alike")
		}
	}
	if err := RunStartupRecovery(dir, IdentityKey("k")); err != nil {
		t.Fatal(err)
	}
	if !WaitForProcessExit(host, 3*time.Second) {
		t.Fatal("our own stale mux host must still be stopped")
	}
}

func TestCommandLineIdentityOfAProfilesChrome(t *testing.T) {
	for _, tc := range []struct {
		cmdline, dir string
		want         bool
	}{
		{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome --remote-debugging-port=9 --user-data-dir=/x/p --no-first-run", "/x/p", true},
		{"chrome --user-data-dir=/x/p", "/x/p", true},
		{"chrome --user-data-dir=/x/p2 --flag", "/x/p", false},
		{"chrome --user-data-dir=/x/p2", "/x/p", false},
		{"chrome --user-data-dir=/x/pp --user-data-dir=/x/p", "/x/p", true},
		{"/opt/other/bin/other-muxd", "/x/p", false},
		{"", "/x/p", false},
		{"chrome --user-data-dir=/x/p", "", false},
	} {
		if got := commandLineIsChromeOfProfile(tc.cmdline, tc.dir); got != tc.want {
			t.Errorf("%q in %q: got %v want %v", tc.cmdline, tc.dir, got, tc.want)
		}
	}
}

// SingletonLock 是陈旧的（"host-<pid>"，pid 现在属于别的程序）：不发信号，Singleton* 残留照常清掉。
func TestRecoveryNeverSignalsASingletonLockPidThatIsNotThisProfilesChrome(t *testing.T) {
	dir := t.TempDir()
	stranger := spawnLongSleeper(t)
	if err := os.Symlink(fmt.Sprintf("somehost-%d", stranger), filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	if err := RunStartupRecovery(dir, IdentityKey("k")); err != nil {
		t.Fatal(err)
	}
	stillAliveAfterRecovery(t, stranger)
	if _, err := os.Lstat(filepath.Join(dir, "SingletonLock")); !os.IsNotExist(err) {
		t.Fatalf("the stale lock itself is still cleaned: %v", err)
	}
}

// 启动失败后回收现场（marker / runtime 清单里的 PID）同样先核对身份。
func TestFailedLaunchReapNeverSignalsAnUnrelatedPid(t *testing.T) {
	dir := t.TempDir()
	stranger := spawnLongSleeper(t)
	writeMarker(t, dir, fmt.Sprintf(`{"owner_pid":1,"chrome_pid":%d,"identity_key":"k"}`, stranger))
	cleanupFailedBrowserMuxProfile(dir)
	stillAliveAfterRecovery(t, stranger)

	t.Setenv("TMPDIR", t.TempDir())
	if err := writeBrowserRuntimeManifest(&BrowserMuxHostState{MuxHostID: "h", RuntimeID: "rt-x", ChromePID: stranger}); err != nil {
		t.Fatal(err)
	}
	cleanupFailedBrowserMuxRuntime("rt-x")
	stillAliveAfterRecovery(t, stranger)
}

func TestFailedLaunchReapStillStopsThisProfilesChrome(t *testing.T) {
	dir := t.TempDir()
	chrome := spawnChromeLookalike(t, dir)
	writeMarker(t, dir, fmt.Sprintf(`{"owner_pid":1,"chrome_pid":%d,"identity_key":"k"}`, chrome))
	cleanupFailedBrowserMuxProfile(dir)
	if !WaitForProcessExit(chrome, 3*time.Second) {
		t.Fatal("this profile's own Chrome must still be reaped")
	}
}
