//go:build !windows

package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// 全局 mux host 是长寿进程。升级之后旧版 host 可能还活着；新 CLI 若悄悄复用它，新版的承诺（清单要求的模拟钥匙串参数、
// 同步关闭、封存）就静默失效，登录态会在下次打开时悄悄丢失。规则只有一条：host 版本 ≠ 自己的版本就不复用。

type fakeHost struct {
	pid       int
	srv       *httptest.Server
	shutdowns atomic.Int32
	ensures   atomic.Int32
}

// newFakeHost 起一个说 BrowserMuxHost 控制协议的假 host。hostVersion 为空 = 旧版 host（状态里没有版本字段）。
// runtimeChromePIDs 是它声称持有的 runtime；killOnShutdown 决定它收到 /shutdown 之后是否真的退出。
func newFakeHost(t *testing.T, hostVersion string, runtimeChromePIDs []int, killOnShutdown bool) *fakeHost {
	t.Helper()
	return newFakeHostWithPID(t, hostVersion, runtimeChromePIDs, killOnShutdown, spawnMuxHostLookalike(t, "test-host"))
}

func newFakeHostWithPID(t *testing.T, hostVersion string, runtimeChromePIDs []int, killOnShutdown bool, pid int) *fakeHost {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir()) // host manifest 写在 os.TempDir() 下
	h := &fakeHost{pid: pid}
	mux := http.NewServeMux()
	hostState := func() BrowserMuxHostState {
		st := BrowserMuxHostState{MuxHostID: "test-host", MuxHostPID: h.pid, BinaryVersion: hostVersion, DisplayBackend: "muxhost", ControlURL: h.srv.URL}
		for i, pid := range runtimeChromePIDs {
			st.Runtimes = append(st.Runtimes, BrowserRuntimeSummary{
				RuntimeID: "rt-" + string(rune('a'+i)), BrowserSessionID: "browser-session-live-" + string(rune('a'+i)), ChromePID: pid, ChromeAlive: true,
			})
		}
		return st
	}
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, hostState()) })
	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		h.shutdowns.Add(1)
		writeJSON(w, hostState())
		if killOnShutdown {
			_ = syscall.Kill(-h.pid, syscall.SIGKILL)
		}
	})
	mux.HandleFunc("/touch", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, hostState()) })
	mux.HandleFunc("/runtime/ensure", func(w http.ResponseWriter, r *http.Request) {
		h.ensures.Add(1)
		var req BrowserMuxHostRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		req = normalizeBrowserMuxHostRequest(req)
		writeJSON(w, BrowserMuxHostState{
			MuxHostID: "test-host", MuxHostPID: h.pid, RuntimeID: req.RuntimeID, BrowserSessionID: req.BrowserSessionID,
			SessionKind: req.SessionKind, Owner: req.Owner, Isolation: req.Isolation, ChromePID: h.pid, Mode: req.Mode,
			DisplayBackend: "none", BinaryVersion: hostVersion,
		})
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	manifest := BrowserMuxHostState{MuxHostID: "test-host", MuxHostPID: h.pid, ControlURL: h.srv.URL, Token: "tok", BinaryVersion: hostVersion}
	if err := writeBrowserMuxHostManifest(&manifest); err != nil {
		t.Fatal(err)
	}
	return h
}

func versionReq() BrowserMuxHostRequest {
	return normalizeBrowserMuxHostRequest(BrowserMuxHostRequest{
		MuxHostID: "test-host", BrowserSessionID: "browser-session-new", ProfileID: "p", ProfileDir: "/tmp/p", Mode: ModeHeadless,
	})
}

func withBinaryVersion(t *testing.T, v string) {
	t.Helper()
	old := BinaryVersion
	BinaryVersion = v
	t.Cleanup(func() { BinaryVersion = old })
}

func TestMuxHostSameVersionIsReused(t *testing.T) {
	withBinaryVersion(t, "9.9.9-test")
	h := newFakeHost(t, "9.9.9-test", nil, true)
	state, ok, err := tryAttachBrowserMuxHost(context.Background(), versionReq())
	if err != nil || !ok || state == nil {
		t.Fatalf("a host of the same version must be reused: ok=%v err=%v", ok, err)
	}
	if h.shutdowns.Load() != 0 || h.ensures.Load() != 1 || !isPIDAlive(h.pid) {
		t.Fatalf("same version: shutdowns=%d ensures=%d alive=%v", h.shutdowns.Load(), h.ensures.Load(), isPIDAlive(h.pid))
	}
}

// 版本不同、host 上没有任何存活的会话：请它退出，等它真的没了，让调用方用当前二进制重新拉起。
func TestMuxHostDifferentVersionAndIdleIsRetired(t *testing.T) {
	withBinaryVersion(t, "9.9.9-test")
	for name, hostVersion := range map[string]string{"older version": "0.19.0-v2", "predates version reporting": ""} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost(t, hostVersion, nil, true)
			_, ok, err := tryAttachBrowserMuxHost(context.Background(), versionReq())
			if err != nil || ok {
				t.Fatalf("an idle mismatched host is retired, not reused and not an error: ok=%v err=%v", ok, err)
			}
			if h.shutdowns.Load() != 1 || h.ensures.Load() != 0 {
				t.Fatalf("shutdowns=%d ensures=%d: the stale host must be asked to exit and must not be asked to run anything", h.shutdowns.Load(), h.ensures.Load())
			}
			if isPIDAlive(h.pid) {
				t.Fatal("the old host process is still alive: the new host would race it for the manifest")
			}
		})
	}
}

// 版本不同、host 上还有存活的会话：拒绝，不动它；错误里写明 pid、版本、占用的会话与恢复办法。
func TestMuxHostDifferentVersionWithLiveSessionsIsRefused(t *testing.T) {
	withBinaryVersion(t, "9.9.9-test")
	live := spawnLongSleeper(t)
	h := newFakeHost(t, "0.19.0-v2", []int{live}, true)
	_, ok, err := tryAttachBrowserMuxHost(context.Background(), versionReq())
	var mismatch *MuxHostVersionMismatchError
	if ok || !errors.As(err, &mismatch) {
		t.Fatalf("want MuxHostVersionMismatchError, got ok=%v err=%v", ok, err)
	}
	if mismatch.HostPID != h.pid || mismatch.HostVersion != "0.19.0-v2" || mismatch.WantVersion != "9.9.9-test" ||
		len(mismatch.Sessions) != 1 || mismatch.Sessions[0] != "browser-session-live-a" {
		t.Fatalf("%+v", mismatch)
	}
	msg := err.Error()
	for _, want := range []string{"pid", "0.19.0-v2", "9.9.9-test", "browser-session-live-a", "dw-browser close --id", "muxhost shutdown --muxhost-id test-host"} {
		if !stringsContains(msg, want) {
			t.Errorf("the refusal must name %q:\n%s", want, msg)
		}
	}
	if h.shutdowns.Load() != 0 || h.ensures.Load() != 0 || !isPIDAlive(h.pid) || !isPIDAlive(live) {
		t.Fatalf("a refusal must not touch the host or its sessions: shutdowns=%d ensures=%d", h.shutdowns.Load(), h.ensures.Load())
	}
}

func TestMuxHostRefusalNamesAnUnreportedVersion(t *testing.T) {
	withBinaryVersion(t, "9.9.9-test")
	live := spawnLongSleeper(t)
	newFakeHost(t, "", []int{live}, true)
	_, _, err := tryAttachBrowserMuxHost(context.Background(), versionReq())
	var mismatch *MuxHostVersionMismatchError
	if !errors.As(err, &mismatch) || mismatch.HostVersion != "" || !stringsContains(err.Error(), "predates version reporting") {
		t.Fatalf("an old host without a version field is a mismatch too: %v", err)
	}
}

// 会话摘要里的 Chrome 已经不在了（host 还没来得及回收）：不算占用。
func TestMuxHostRuntimeWhoseChromeIsGoneDoesNotBlockRetirement(t *testing.T) {
	withBinaryVersion(t, "9.9.9-test")
	dead := deadPID(t)
	h := newFakeHost(t, "0.19.0-v2", []int{dead}, true)
	_, ok, err := tryAttachBrowserMuxHost(context.Background(), versionReq())
	if err != nil || ok || h.shutdowns.Load() != 1 {
		t.Fatalf("ok=%v err=%v shutdowns=%d", ok, err, h.shutdowns.Load())
	}
}

// 请它退出它不退：与陈旧 host 同样处理，强制终止，调用方才能放心拉起新的。
func TestMuxHostThatIgnoresTheShutdownRequestIsTerminated(t *testing.T) {
	withBinaryVersion(t, "9.9.9-test")
	old := muxHostVersionRetireWait
	muxHostVersionRetireWait = 200 * time.Millisecond
	t.Cleanup(func() { muxHostVersionRetireWait = old })
	h := newFakeHost(t, "0.19.0-v2", nil, false) // 收到 /shutdown 什么也不做
	_, ok, err := tryAttachBrowserMuxHost(context.Background(), versionReq())
	if err != nil || ok || isPIDAlive(h.pid) {
		t.Fatalf("ok=%v err=%v alive=%v", ok, err, isPIDAlive(h.pid))
	}
}

// 库调用方（BrowserPool 等）没有 CLI 的版本身份：不做版本检查，保持原有行为。
func TestMuxHostVersionCheckIsSkippedWithoutABinaryVersion(t *testing.T) {
	withBinaryVersion(t, "")
	h := newFakeHost(t, "0.19.0-v2", nil, true)
	_, ok, err := tryAttachBrowserMuxHost(context.Background(), versionReq())
	if err != nil || !ok || h.shutdowns.Load() != 0 {
		t.Fatalf("ok=%v err=%v shutdowns=%d", ok, err, h.shutdowns.Load())
	}
}

// host 的状态里带着自己的二进制版本（旧 host 没有）。
func TestMuxHostReportsItsBinaryVersion(t *testing.T) {
	withBinaryVersion(t, "9.9.9-test")
	s := &browserMuxHostServer{hostID: "h", runtimes: map[string]*browserMuxHostRuntime{}}
	if got := s.snapshot(false).BinaryVersion; got != "9.9.9-test" {
		t.Fatalf("host snapshot version %q", got)
	}
	b, _ := json.Marshal(BrowserMuxHostState{})
	if stringsContains(string(b), "binary_version") {
		t.Fatalf("an empty version must not be serialized: %s", b)
	}
}

// Chrome 没有被确认退出时，封存根本不尝试（一个仍在写盘的进程会让封存读到不一致的内容）。
func TestMuxHostDoesNotSealWhileChromeIsStillAlive(t *testing.T) {
	useTestKeyFile(t)
	dir, _ := newTestProfile(t)
	want := snapshotTree(t, dir)
	rig, fc := newMuxRig(t, fakeStubborn, dir, true)
	rig.srv.runtimes["rt-1"].handle = stuckHandle{fakeHandle{fc}} // Kill 报错、进程还在

	out := rig.shutdown(t, true) // force：直接走强杀这一步
	if out.Closed.Shutdown.Error == "" {
		t.Fatalf("the receipt must carry the kill failure: %+v", out.Closed.Shutdown)
	}
	p := out.Closed.Profile
	if p == nil || p.ErrorKind != ProfileErrChromeNotExited || p.Vault != ProfileVaultOpen || p.Seal != nil {
		t.Fatalf("no seal may be attempted while Chrome is alive: %+v", p)
	}
	diffTrees(t, want, snapshotTree(t, dir))
}

// stuckHandle 的 Kill 失败且不杀进程：模拟"强杀之后进程仍然没有确认消失"。
type stuckHandle struct{ fakeHandle }

func (stuckHandle) Kill() error { return errors.New("chrome did not exit after SIGKILL") }

func stringsContains(s, sub string) bool { return strings.Contains(s, sub) }

// 身份核对三个条件缺一不可：可执行文件是 dw-browser、子命令恰好是 `muxhost serve`、--muxhost-id 恰好等于目标 host。
func TestCommandLineIdentityOfAMuxHost(t *testing.T) {
	good := "/Users/x/.local/bin/dw-browser muxhost serve --muxhost-id browser-mux-host-global --runtime-id r --mode headed"
	for cmdline, want := range map[string]bool{
		good: true,
		"/Applications/Some App.app/Contents/MacOS/dw-browser muxhost serve --muxhost-id browser-mux-host-global": true,  // 路径含空格
		"C:\\tools\\dw-browser.exe muxhost serve --muxhost-id browser-mux-host-global":                            false, // 非 unix 路径分隔符：filepath.Base 在 unix 上认不出，宁可拒绝
		"/opt/other/bin/other-muxd": false, // 名字带 mux 的无关守护进程
		"/opt/other/bin/other-muxd muxhost serve --muxhost-id browser-mux-host-global": false, // 子命令和 id 都对，可执行文件不是 dw-browser
		"/usr/libexec/usbmuxd -launchd":                                                    false,
		"/bin/sh -c x muxhost serve --muxhost-id browser-mux-host-global":                  false, // 可执行文件不是 dw-browser
		"/usr/local/bin/not-dw-browser muxhost serve --muxhost-id browser-mux-host-global": false, // 文件名必须恰好是 dw-browser
		"dw-browser muxhost serve --muxhost-id someone-else":                               false, // 别的 host
		"dw-browser muxhost serve --muxhost-id browser-mux-host-global-2":                  false, // id 必须恰好相等，不是前缀
		"dw-browser muxhost status --muxhost-id browser-mux-host-global":                   false, // 不是 serve
		"dw-browser open http://x --muxhost-id browser-mux-host-global muxhost serve-not":  false,
		"": false,
	} {
		if got := commandLineIsBrowserMuxHost(cmdline, "browser-mux-host-global"); got != want {
			t.Errorf("%q: got %v, want %v", cmdline, got, want)
		}
	}
	if commandLineIsBrowserMuxHost(good, "") {
		t.Error("an empty host id must never match")
	}
}

func TestPidIdentityOfRealProcesses(t *testing.T) {
	plain := spawnLongSleeper(t)
	host := spawnMuxHostLookalike(t, "test-host")
	if pidIsBrowserMuxHost(plain, "test-host") {
		t.Error("a plain process was taken for a mux host")
	}
	if !pidIsBrowserMuxHost(host, "test-host") || pidIsBrowserMuxHost(host, "another-host") {
		t.Error("the look-alike must match its own host id and no other")
	}
	if pidIsBrowserMuxHost(0, "test-host") || pidIsBrowserMuxHost(1, "test-host") || pidIsBrowserMuxHost(deadPID(t), "test-host") {
		t.Error("pid 0, pid 1 and dead pids must never match")
	}
}

// 请它退出它不退、而且这个 PID 不是我们的 host（manifest 陈旧、PID 被别的程序占了）：不发任何信号，报错。
func TestMuxHostVersionRetireNeverSignalsAProcessThatIsNotTheHost(t *testing.T) {
	withBinaryVersion(t, "9.9.9-test")
	old := muxHostVersionRetireWait
	muxHostVersionRetireWait = 200 * time.Millisecond
	t.Cleanup(func() { muxHostVersionRetireWait = old })
	stranger := spawnLongSleeper(t)
	h := newFakeHostWithPID(t, "0.19.0-v2", nil, false, stranger)
	_, ok, err := tryAttachBrowserMuxHost(context.Background(), versionReq())
	var notOurs *NotOurProcessError
	if ok || !errors.As(err, &notOurs) || notOurs.PID != stranger || !stringsContains(err.Error(), "sleep") || !stringsContains(err.Error(), "nothing was sent") {
		t.Fatalf("the refusal must carry the pid and the actual command line: ok=%v err=%v", ok, err)
	}
	if !isPIDAlive(stranger) {
		t.Fatal("a process that is not the host was signalled")
	}
	if h.shutdowns.Load() != 1 {
		t.Fatalf("the control API is still asked first: %d", h.shutdowns.Load())
	}
}

// 陈旧 host 清理分支（health 不通）同样先核对身份：PID 被别的程序占了就只当 manifest 陈旧，不动那个进程。
func TestStaleHostCleanupDoesNotSignalAForeignPid(t *testing.T) {
	stranger := spawnLongSleeper(t)
	h := newFakeHostWithPID(t, "", nil, false, stranger)
	h.srv.Close() // control endpoint 不通 → 走"陈旧 host"分支
	_, ok, err := tryAttachBrowserMuxHost(context.Background(), versionReq())
	var notOurs *NotOurProcessError
	if ok || !errors.As(err, &notOurs) || notOurs.PID != stranger || !stringsContains(err.Error(), "sleep") {
		t.Fatalf("an unidentifiable pid is refused with a clear error naming pid and command line: ok=%v err=%v", ok, err)
	}
	if !isPIDAlive(stranger) {
		t.Fatal("the stale-host cleanup killed a pid whose command line is not a dw-browser muxhost")
	}
}

func TestStaleHostCleanupStillStopsTheRealHost(t *testing.T) {
	host := spawnMuxHostLookalike(t, "test-host")
	h := newFakeHostWithPID(t, "", nil, false, host)
	h.srv.Close()
	_, ok, err := tryAttachBrowserMuxHost(context.Background(), versionReq())
	if ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if isPIDAlive(host) {
		t.Fatal("an unresponsive host of ours must still be cleaned up")
	}
}
