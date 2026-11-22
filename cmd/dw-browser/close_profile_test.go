//go:build !windows

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// close 的契约：Chrome 怎么消失、profile 封存得怎么样、临时 profile 删没删——都写在回执里。
// 这里用一个真实的子进程（sleep）充当"Chrome"，profile 目录是 hermetic CLI profile 根下的真实目录，
// 封存用文件密钥库（见 hermetic_test.go），不碰真实钥匙串，也不碰真实的 ~/.deepwork/browser-cli。

func startFakeChrome(t *testing.T) int {
	t.Helper()
	// 命令行带 --remote-debugging-port（close 只对认得出是自己启动的 Chrome 的进程发信号）；真正干活的是 sleep。
	cmd := exec.Command("sh", "-c", "sleep 120; true", "--", "--remote-debugging-port=0")
	browser.ApplyDetachedProcAttr(cmd) // 自成进程组，和 dw-browser 启动的 Chrome 一样
	if err := cmd.Start(); err != nil {
		t.Skipf("no sleep binary: %v", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // 回收僵尸：isPIDAlive 才不会把它当活的
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return pid
}

func newCLIProfile(t *testing.T, name string, vault string) string {
	t.Helper()
	dir := filepath.Join(browser.BrowserCLIProfileRoot(""), name)
	for rel, body := range map[string]string{
		"Default/Cookies":     "SQLite format 3\x00SECRET-COOKIE-MARKER",
		"Default/Preferences": `{"x":1}`,
		"Local State":         `{"os_crypt":{}}`,
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := browser.NewProfileManifest(browser.ProfileOSCryptMock, vault, "headless")
	if err != nil {
		t.Fatal(err)
	}
	if err := browser.WriteProfileManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = browser.RemoveCLIProfileDir(dir) })
	return dir
}

func saveCloseSession(t *testing.T, id, profileDir string, pid int, ephemeral bool) {
	t.Helper()
	err := browser.SaveSession(&browser.SessionInfo{
		SessionID: id, BrowserSessionID: browser.BrowserSessionIDFromSessionID(id),
		ProfileID: filepath.Base(profileDir), ProfileDir: profileDir,
		ChromePID: pid, Ephemeral: ephemeral, Mode: browser.ModeHeadless, CreatedAt: time.Now().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = browser.DeleteSession(id) })
}

type closeOut struct {
	Closed         bool   `json:"closed"`
	Shutdown       string `json:"shutdown"`
	ShutdownMs     int64  `json:"shutdown_ms"`
	ShutdownReason string `json:"shutdown_reason"`
	Profile        struct {
		Persistent  bool   `json:"persistent"`
		OSCrypt     string `json:"os_crypt"`
		Vault       string `json:"vault"`
		Protection  string `json:"protection"`
		Error       string `json:"error"`
		ErrorKind   string `json:"error_kind"`
		Note        string `json:"note"`
		Removed     *bool  `json:"removed"`
		RemoveError string `json:"remove_error"`
		Seal        *struct {
			Action   string `json:"action"`
			Verified bool   `json:"verified"`
		} `json:"seal"`
	} `json:"profile"`
}

func runClose(t *testing.T, args ...string) (closeOut, CommandResult) {
	t.Helper()
	res, err := runCloseResult(args)
	if err != nil || res.Code != exitOK {
		t.Fatalf("close failed: code=%d err=%v", res.Code, err)
	}
	raw, ok := res.Payload.(json.RawMessage)
	if !ok {
		t.Fatalf("payload type %T", res.Payload)
	}
	var out closeOut
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	return out, res
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestClosePersistentProfileExitsThenSealsAndSaysSo(t *testing.T) {
	dir := newCLIProfile(t, "persist-close-1", browser.ProfileVaultOpen)
	id := fmt.Sprintf("close-persist-%d", time.Now().UnixNano())
	pid := startFakeChrome(t)
	saveCloseSession(t, id, dir, pid, false)

	out, _ := runClose(t, "--id", id)

	if !out.Closed || out.Shutdown == "" {
		t.Fatalf("%+v", out)
	}
	// 没有 CDP 端点可发 Browser.close：回落到 SIGTERM，回执如实写 terminated 与原因。
	if out.Shutdown != browser.ShutdownTerminated || !strings.Contains(out.ShutdownReason, "no CDP endpoint") {
		t.Fatalf("shutdown %q reason %q", out.Shutdown, out.ShutdownReason)
	}
	if !out.Profile.Persistent || out.Profile.Vault != "sealed" || out.Profile.OSCrypt != "mock" || out.Profile.Protection != "file" || out.Profile.Error != "" {
		t.Fatalf("profile %+v", out.Profile)
	}
	if out.Profile.Seal == nil || out.Profile.Seal.Action != "seal" || !out.Profile.Seal.Verified {
		t.Fatalf("seal %+v", out.Profile.Seal)
	}
	if names := listNames(t, dir); len(names) != 2 {
		t.Fatalf("a sealed profile is exactly the manifest and the vault, got %v", names)
	}
	if browser.WaitForProcessExit(pid, 2*time.Second) == false {
		t.Fatal("chrome is still alive after close returned")
	}
	if _, err := browser.LoadSession(id); err == nil {
		t.Fatal("the session file must be deleted")
	}
}

func TestCloseTemporaryProfileKillsAtOnceAndRemovesTheDirectory(t *testing.T) {
	dir := newCLIProfile(t, "task-close-eph", browser.ProfileVaultNone)
	id := fmt.Sprintf("close-eph-%d", time.Now().UnixNano())
	pid := startFakeChrome(t)
	saveCloseSession(t, id, dir, pid, true)

	out, _ := runClose(t, "--id", id)

	if out.Shutdown != browser.ShutdownKilled || out.ShutdownReason != "ephemeral-profile" {
		t.Fatalf("%+v", out)
	}
	if out.Profile.Persistent || out.Profile.Removed == nil || !*out.Profile.Removed || out.Profile.Seal != nil {
		t.Fatalf("profile %+v", out.Profile)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("temporary profile still on disk: %v", err)
	}
}

func TestCloseForceKillsButStillSealsAPersistentProfile(t *testing.T) {
	dir := newCLIProfile(t, "persist-close-force", browser.ProfileVaultOpen)
	id := fmt.Sprintf("close-force-%d", time.Now().UnixNano())
	pid := startFakeChrome(t)
	saveCloseSession(t, id, dir, pid, false)

	out, _ := runClose(t, "--id", id, "--force")

	if out.Shutdown != browser.ShutdownKilled || out.ShutdownReason != "forced" {
		t.Fatalf("%+v", out)
	}
	if out.Profile.Vault != "sealed" || out.Profile.Error != "" {
		t.Fatalf("--force still seals: %+v", out.Profile)
	}
}

// D5：封存失败（遇到普通 symlink）不吞：回执写明，明文完好，close 本身仍然成功关闭了会话。
func TestCloseReportsASealFailureAndKeepsThePlaintext(t *testing.T) {
	dir := newCLIProfile(t, "persist-close-fail", browser.ProfileVaultOpen)
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "Default", "sneaky")); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("close-fail-%d", time.Now().UnixNano())
	pid := startFakeChrome(t)
	saveCloseSession(t, id, dir, pid, false)

	out, res := runClose(t, "--id", id)

	if !out.Closed {
		t.Fatal("the session itself must still be closed")
	}
	if out.Profile.ErrorKind != "unsupported_entry" || out.Profile.Error == "" || out.Profile.Vault != "open" || !strings.Contains(out.Profile.Note, "NOT sealed") {
		t.Fatalf("the receipt must say the profile is still plaintext: %+v", out.Profile)
	}
	if _, err := os.Stat(filepath.Join(dir, "Default", "Cookies")); err != nil {
		t.Fatalf("plaintext must be intact: %v", err)
	}
	if res.Code != exitOK {
		t.Fatalf("exit code %d", res.Code)
	}
}

// 会话文件里的 ProfileDir 不可信：不在 CLI profile 根下的临时 profile 不会被删，并且如实报告。
func TestCloseRefusesToRemoveATemporaryProfileOutsideTheCLIRoot(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "task-elsewhere")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "precious"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("close-outside-%d", time.Now().UnixNano())
	saveCloseSession(t, id, outside, 0, true)

	out, _ := runClose(t, "--id", id)

	if out.Profile.Removed == nil || *out.Profile.Removed || out.Profile.RemoveError == "" {
		t.Fatalf("a refused removal must be reported: %+v", out.Profile)
	}
	if _, err := os.Stat(filepath.Join(outside, "precious")); err != nil {
		t.Fatalf("a directory outside the CLI profile root was deleted: %v", err)
	}
}

func TestProfileListShowsCryptoIdentityAndVaultState(t *testing.T) {
	sealed := newCLIProfile(t, "list-sealed", browser.ProfileVaultOpen)
	if rc := browser.SealPersistentProfile(sealed); rc.Error != "" {
		t.Fatalf("seal: %+v", rc)
	}
	newCLIProfile(t, "list-open", browser.ProfileVaultOpen)
	legacy := filepath.Join(browser.BrowserCLIProfileRoot(""), "list-legacy")
	if err := os.MkdirAll(filepath.Join(legacy, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = browser.RemoveCLIProfileDir(legacy) })

	res, err := runProfileResult([]string{"list"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res.Payload)
	var payload struct {
		Profiles []struct {
			Name       string `json:"name"`
			OSCrypt    string `json:"os_crypt"`
			Vault      string `json:"vault"`
			Protection string `json:"protection"`
			Managed    bool   `json:"managed"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	by := map[string]int{}
	for i, p := range payload.Profiles {
		by[p.Name] = i
	}
	get := func(name string) (idx int) {
		i, ok := by[name]
		if !ok {
			t.Fatalf("%s missing from %s", name, raw)
		}
		return i
	}
	if p := payload.Profiles[get("list-sealed")]; !p.Managed || p.OSCrypt != "mock" || p.Vault != "sealed" || p.Protection != "file" {
		t.Fatalf("sealed: %+v", p)
	}
	if p := payload.Profiles[get("list-open")]; p.Vault != "open" {
		t.Fatalf("open: %+v", p)
	}
	if p := payload.Profiles[get("list-legacy")]; p.Managed || p.OSCrypt != "unclassified" {
		t.Fatalf("a profile without a manifest is listed as unclassified, never guessed: %+v", p)
	}
	// 列出不会改动任何东西：没有清单的 profile 仍然没有清单。
	if _, err := os.Stat(filepath.Join(legacy, browser.ProfileManifestFile)); err == nil {
		t.Fatal("listing must not classify")
	}
}

func TestMuxHostFlagsParseTheProfilePolicy(t *testing.T) {
	base := []string{"--muxhost-id", "h", "--runtime-id", "r", "--profile-dir", "/p"}
	req, err := parseBrowserMuxHostFlagsE(base, "muxhost serve")
	if err != nil || req.ManagedProfile || req.PersistentProfile {
		t.Fatalf("default is unmanaged: %+v %v", req, err)
	}
	req, err = parseBrowserMuxHostFlagsE(append(append([]string{}, base...), "--persistent-profile"), "muxhost serve")
	if err != nil || !req.ManagedProfile || !req.PersistentProfile {
		t.Fatalf("persistent implies managed: %+v %v", req, err)
	}
	req, err = parseBrowserMuxHostFlagsE(append(append([]string{}, base...), "--managed-profile"), "muxhost serve")
	if err != nil || !req.ManagedProfile || req.PersistentProfile {
		t.Fatalf("managed only: %+v %v", req, err)
	}
}

// 旧版 BrowserMuxHost：先回响应、后台关闭，响应里没有关闭回执。CLI 如实报 unknown，并且只在 Chrome 被确认退出之后才封存。
func legacyHostSession(t *testing.T, profileDir string, chromePID int, killOnShutdown bool) string {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir()) // 运行时清单写在 os.TempDir() 下
	id := fmt.Sprintf("close-legacy-%d", time.Now().UnixNano())
	runtimeID := browser.BrowserRuntimeIDFromBrowserSessionID(browser.BrowserSessionIDFromSessionID(id))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"browser_mux_host_id": "legacy", "runtime_id": runtimeID}) // 没有 "closed"
		if killOnShutdown {
			go func() { time.Sleep(150 * time.Millisecond); _ = syscall.Kill(-chromePID, syscall.SIGKILL) }()
		}
	}))
	t.Cleanup(srv.Close)
	state := browser.BrowserMuxHostState{MuxHostID: "legacy", RuntimeID: runtimeID, ControlURL: srv.URL, Token: "t"}
	body, _ := json.Marshal(state)
	path := browser.BrowserRuntimeManifestPath(runtimeID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	err := browser.SaveSession(&browser.SessionInfo{
		SessionID: id, BrowserSessionID: browser.BrowserSessionIDFromSessionID(id), RuntimeID: runtimeID, BrowserMuxHostID: "legacy",
		ProfileID: filepath.Base(profileDir), ProfileDir: profileDir, ChromePID: chromePID, Mode: browser.ModeHeaded, CreatedAt: time.Now().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = browser.DeleteSession(id) })
	return id
}

func TestCloseOnALegacyHostReportsUnknownAndSealsOnlyAfterChromeIsGone(t *testing.T) {
	old := legacyHostChromeExitWait
	legacyHostChromeExitWait = 2 * time.Second
	t.Cleanup(func() { legacyHostChromeExitWait = old })

	dir := newCLIProfile(t, "persist-legacy-gone", browser.ProfileVaultOpen)
	pid := startFakeChrome(t)
	id := legacyHostSession(t, dir, pid, true)
	out, _ := runClose(t, "--id", id)
	if out.Shutdown != "unknown" || !strings.Contains(out.ShutdownReason, "predates synchronous shutdown") {
		t.Fatalf("an old host reports no exit mode, and the receipt must say so: %+v", out)
	}
	if out.Profile.Vault != "sealed" || out.Profile.Error != "" {
		t.Fatalf("Chrome was confirmed gone, so the profile is sealed: %+v", out.Profile)
	}
}

func TestCloseOnALegacyHostDoesNotSealWhileChromeIsStillRunning(t *testing.T) {
	old := legacyHostChromeExitWait
	legacyHostChromeExitWait = 300 * time.Millisecond
	t.Cleanup(func() { legacyHostChromeExitWait = old })

	dir := newCLIProfile(t, "persist-legacy-alive", browser.ProfileVaultOpen)
	pid := startFakeChrome(t) // 旧 host 说"我在关了"，但这个进程一直不退
	id := legacyHostSession(t, dir, pid, false)
	out, _ := runClose(t, "--id", id)
	if out.Shutdown != "unknown" {
		t.Fatalf("%+v", out)
	}
	if out.Profile.ErrorKind != "chrome_not_exited" || out.Profile.Vault != "open" || out.Profile.Seal != nil {
		t.Fatalf("sealing must not even be attempted while Chrome may still be writing: %+v", out.Profile)
	}
	if _, err := os.Stat(filepath.Join(dir, "Default", "Cookies")); err != nil {
		t.Fatalf("plaintext must be intact: %v", err)
	}
}

// open --mode headed 遇到另一个版本的 host 且它还占着会话：拒绝，错误写明原因，并且不留下任何东西
// （没有会话文件、没有这次调用创建出来的 profile 目录、没有被封存的空 profile）。
func TestOpenHeadedRefusesAHostOfAnotherVersionAndLeavesNoProfileBehind(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	hostPID, liveChrome := startFakeChrome(t), startFakeChrome(t)
	hostID := browser.GlobalBrowserMuxHostID()
	var shutdowns int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/shutdown" {
			atomic.AddInt32(&shutdowns, 1)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{ // 旧版 host：没有 binary_version
			"browser_mux_host_id": hostID, "browser_mux_host_pid": hostPID, "control_url": "http://" + r.Host,
			"runtimes": []map[string]any{{"runtime_id": "rt-other", "browser_session_id": "browser-session-other", "chrome_pid": liveChrome, "chrome_alive": true}},
		})
	}))
	t.Cleanup(srv.Close)
	body, _ := json.Marshal(map[string]any{"browser_mux_host_id": hostID, "browser_mux_host_pid": hostPID, "control_url": srv.URL, "token": "t"})
	path := browser.BrowserMuxHostManifestPath(hostID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	id := fmt.Sprintf("open-refused-%d", time.Now().UnixNano())
	profile := "persist-" + id
	profileDir := filepath.Join(browser.BrowserCLIProfileRoot(""), profile)
	res, err := runOpenResult([]string{"http://127.0.0.1:1/x", "--id", id, "--scenario", "app-test-explore", "--profile", profile, "--mode", "headed"})
	if err == nil && res.Code == exitOK {
		t.Fatal("opening on a host of another version that still holds a session must fail")
	}
	text := commandCause(res).Error()
	if err != nil {
		text += " " + err.Error()
	}
	if !strings.Contains(text, "refusing to reuse BrowserMuxHost") {
		t.Skipf("environment gate: the open failed before reaching the host: %s", text)
	}
	for _, want := range []string{fmt.Sprintf("pid %d", hostPID), "predates version reporting", version, "browser-session-other", "muxhost shutdown --muxhost-id " + hostID} {
		if !strings.Contains(text, want) {
			t.Errorf("the refusal must name %q:\n%s", want, text)
		}
	}
	if atomic.LoadInt32(&shutdowns) != 0 {
		t.Fatal("a refusal must not ask the host to shut down")
	}
	if _, err := os.Lstat(profileDir); !os.IsNotExist(err) {
		t.Fatalf("the refused open left a profile directory behind: %v", err)
	}
	if _, err := browser.LoadSession(id); err == nil {
		t.Fatal("the refused open left a session file behind")
	}
}

// 会话文件里的 ChromePID 陈旧（进程早没了，号码被别的程序占了）：close 绝不对它发信号。
func TestCloseNeverSignalsAPidThatIsNotTheSessionsChrome(t *testing.T) {
	dir := newCLIProfile(t, "persist-close-stale", browser.ProfileVaultOpen)
	stranger := exec.Command("sleep", "120") // 一个不相干的进程，号码恰好写在会话文件里
	browser.ApplyDetachedProcAttr(stranger)
	if err := stranger.Start(); err != nil {
		t.Skip(err)
	}
	pid := stranger.Process.Pid
	done := make(chan struct{})
	go func() { _ = stranger.Wait(); close(done) }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	id := fmt.Sprintf("close-stale-%d", time.Now().UnixNano())
	saveCloseSession(t, id, dir, pid, false)

	for _, args := range [][]string{{"--id", id}} {
		out, _ := runClose(t, args...)
		if out.Shutdown != browser.ShutdownGone || !strings.Contains(out.ShutdownReason, "nothing was signalled") {
			t.Fatalf("%+v", out)
		}
	}
	select {
	case <-done:
		t.Fatal("close signalled a process that is not the session's Chrome")
	case <-time.After(300 * time.Millisecond):
	}
}
