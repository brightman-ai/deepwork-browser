//go:build !windows

package browser

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeHandle 把 fakeChrome 包成 mux host 持有的 ChromeHandle。
type fakeHandle struct{ fc *fakeChrome }

func (h fakeHandle) WSURL() string         { return h.fc.ws }
func (h fakeHandle) PID() int              { return h.fc.cmd.Process.Pid }
func (h fakeHandle) Done() <-chan struct{} { return h.fc.done }
func (h fakeHandle) Wait() error           { <-h.fc.done; return nil }
func (h fakeHandle) Kill() error {
	KillChromeProcessGroup(h.PID())
	select {
	case <-h.fc.done:
		return nil
	case <-time.After(5 * time.Second):
		return os.ErrDeadlineExceeded
	}
}

type muxRig struct {
	srv   *browserMuxHostServer
	http  *httptest.Server
	state BrowserMuxHostState // 调用方用的 state：只有 control URL / token / runtime id
}

// newMuxRig 起一个真的 mux host HTTP 处理器，里面放一个由 fakeChrome 支撑的 runtime。
// profileDir 为空表示非托管 profile。
func newMuxRig(t *testing.T, mode string, profileDir string, persistent bool) (*muxRig, *fakeChrome) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir()) // mux host 的 manifest 写在 os.TempDir() 下：别写进真实的 /tmp 区
	fc := spawnFakeChrome(t, mode)
	srv := &browserMuxHostServer{
		hostID:     "test-host",
		token:      "tok",
		runtimes:   map[string]*browserMuxHostRuntime{},
		shutdownCh: make(chan struct{}),
	}
	rt := &browserMuxHostRuntime{
		state: BrowserMuxHostState{
			MuxHostID: "test-host", RuntimeID: "rt-1", BrowserSessionID: "browser-session-1",
			ChromePID: fc.cmd.Process.Pid, WSURL: fc.ws, ProfileDir: profileDir, PersistentProfile: persistent,
		},
		handle:      fakeHandle{fc},
		profileDir:  profileDir,
		identityKey: IdentityKey("dw-cli-test"),
		persistent:  persistent,
	}
	srv.runtimes["rt-1"] = rt
	ts := httptest.NewServer(srv.httpHandler())
	t.Cleanup(ts.Close)
	return &muxRig{srv: srv, http: ts, state: BrowserMuxHostState{ControlURL: ts.URL, Token: "tok", RuntimeID: "rt-1"}}, fc
}

func (r *muxRig) shutdown(t *testing.T, force bool) *BrowserMuxHostState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st := r.state
	var out *BrowserMuxHostState
	var err error
	if force {
		out, err = ShutdownBrowserMuxHostForce(ctx, &st)
	} else {
		out, err = ShutdownBrowserMuxHost(ctx, &st)
	}
	if err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if out.Closed == nil {
		t.Fatal("the response carries no close receipt")
	}
	return out
}

// 同步：响应回来的时候 Chrome 已经消失、profile 已经封存。过去是先回响应、再异步关闭。
func TestMuxHostShutdownIsSynchronousAndSealsAPersistentProfile(t *testing.T) {
	useTestKeyFile(t)
	dir, _ := newTestProfile(t)
	rig, fc := newMuxRig(t, fakeGraceful, dir, true)

	out := rig.shutdown(t, false)

	fc.requireGone(t) // 响应已收到，进程必须已经没了——不是"马上就没"
	if out.Closed.Shutdown.Mode != ShutdownGraceful {
		t.Fatalf("a persistent profile must exit normally: %+v", out.Closed.Shutdown)
	}
	p := out.Closed.Profile
	if p == nil || p.Error != "" || p.Vault != ProfileVaultSealed || p.Seal == nil || !p.Seal.Verified {
		t.Fatalf("profile receipt %+v", p)
	}
	requireOnlyManifestAndVault(t, dir) // 响应回来时磁盘上已经只剩 2 个文件
	if _, err := rig.srv.runtime("rt-1"); err == nil {
		t.Fatal("the runtime must be gone from the host")
	}
}

func TestMuxHostShutdownKillsATemporaryProfileImmediatelyWithoutSealing(t *testing.T) {
	useTestKeyFile(t)
	dir, _ := newTestProfile(t)
	want := snapshotTree(t, dir)
	rig, fc := newMuxRig(t, fakeGraceful, dir, false)

	out := rig.shutdown(t, false)

	fc.requireGone(t)
	if out.Closed.Shutdown.Mode != ShutdownKilled || out.Closed.Shutdown.Reason != "ephemeral-profile" {
		t.Fatalf("a temporary profile has nothing to preserve: %+v", out.Closed.Shutdown)
	}
	if out.Closed.Profile != nil {
		t.Fatalf("a temporary profile is never sealed: %+v", out.Closed.Profile)
	}
	diffTrees(t, want, snapshotTree(t, dir))
}

// --force：直接强杀；持久 profile 照样尽力封存。
func TestMuxHostShutdownForceKillsButStillSeals(t *testing.T) {
	useTestKeyFile(t)
	dir, _ := newTestProfile(t)
	rig, fc := newMuxRig(t, fakeGraceful, dir, true)

	out := rig.shutdown(t, true)

	fc.requireGone(t)
	if out.Closed.Shutdown.Mode != ShutdownKilled || out.Closed.Shutdown.Reason != "forced" {
		t.Fatalf("%+v", out.Closed.Shutdown)
	}
	if out.Closed.Profile == nil || out.Closed.Profile.Vault != ProfileVaultSealed {
		t.Fatalf("force must still seal a persistent profile: %+v", out.Closed.Profile)
	}
	requireOnlyManifestAndVault(t, dir)
}

// D5：封存失败不吞——回执写明，明文完好，Chrome 照样已经关掉。
func TestMuxHostShutdownReportsASealFailureWithoutHidingIt(t *testing.T) {
	useTestKeyFile(t)
	dir, _ := newTestProfile(t)
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "Default", "link")); err != nil {
		t.Fatal(err)
	}
	want := snapshotTree(t, dir)
	rig, fc := newMuxRig(t, fakeGraceful, dir, true)

	out := rig.shutdown(t, false)

	fc.requireGone(t)
	p := out.Closed.Profile
	if p == nil || p.ErrorKind != ProfileErrUnsupportedEntry || p.Vault != ProfileVaultOpen {
		t.Fatalf("receipt %+v", p)
	}
	diffTrees(t, want, snapshotTree(t, dir))
}

func TestMuxHostShutdownOfAnUnknownRuntimeIs404(t *testing.T) {
	rig, _ := newMuxRig(t, fakeGraceful, "", false)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st := rig.state
	st.RuntimeID = "nope"
	if _, err := ShutdownBrowserMuxHost(ctx, &st); err == nil {
		t.Fatal("closing a runtime that does not exist must fail so the CLI falls back to the PID path")
	}
}

// 空闲回收与 chrome_exited 走的是同一个入口：持久 profile 同样正常退出并封存。
func TestMuxHostIdleReclaimGracefullySealsAPersistentProfile(t *testing.T) {
	useTestKeyFile(t)
	dir, _ := newTestProfile(t)
	rig, fc := newMuxRig(t, fakeGraceful, dir, true)

	state, closed, err := rig.srv.closeRuntimeWith("rt-1", "runtime_idle_ttl", false)
	if err != nil || state == nil {
		t.Fatalf("%v", err)
	}
	fc.requireGone(t)
	if closed.Shutdown.Mode != ShutdownGraceful || closed.Profile == nil || closed.Profile.Vault != ProfileVaultSealed {
		t.Fatalf("%+v", closed)
	}
	requireOnlyManifestAndVault(t, dir)
}

// Chrome 在 close 之前就死了（崩溃）：回执是 gone，不是 graceful；profile 仍然要封存。
func TestMuxHostClosingACrashedRuntimeReportsGoneAndStillSeals(t *testing.T) {
	useTestKeyFile(t)
	dir, _ := newTestProfile(t)
	rig, fc := newMuxRig(t, fakeGraceful, dir, true)
	KillChromeProcessGroup(fc.cmd.Process.Pid)
	<-fc.done

	_, closed, err := rig.srv.closeRuntimeWith("rt-1", "chrome_exited", false)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Shutdown.Mode != ShutdownGone {
		t.Fatalf("%+v", closed.Shutdown)
	}
	if closed.Profile == nil || closed.Profile.Vault != ProfileVaultSealed {
		t.Fatalf("a crashed persistent session must still be sealed: %+v", closed.Profile)
	}
}
