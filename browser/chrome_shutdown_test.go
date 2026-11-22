//go:build !windows

package browser

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// ============================================================
// § 假 Chrome：本测试二进制自己重新执行，扮演一个"浏览器进程"
// ============================================================
//
// 真 Chrome 的退出行为（Browser.close 后自己退出 / 只认 SIGTERM / 谁都杀不死只能 SIGKILL）没法在单测里
// 按需构造，所以用一个确定性的假进程覆盖三条路径与它们的回执。真 Chrome 的端到端行为另有测试
// （TestOwnedCorePersistentProfile*）与本机真实运行证据。

const fakeChromeModeEnv = "DWB_FAKE_CHROME_MODE"

// fake chrome 模式
const (
	fakeGraceful = "graceful" // 收到 Browser.close 后回应并自己退出
	fakeTermOnly = "term"     // 收到 Browser.close 只回应、不退出；SIGTERM 才退出
	fakeStubborn = "stubborn" // Browser.close 与 SIGTERM 都不理，只有 SIGKILL 能杀
	fakeNoWS     = "nows"     // 不开 CDP 端点；SIGTERM 才退出
)

// TestFakeChromeHelper 不是测试：它是被 spawnFakeChrome 重新执行的"假 Chrome"。普通运行时立刻返回。
func TestFakeChromeHelper(t *testing.T) {
	mode := os.Getenv(fakeChromeModeEnv)
	if mode == "" {
		return
	}
	// 一个同进程组的"帮手进程"，用来验证退出之后的进程组清扫。
	child := exec.Command("sleep", "120")
	if err := child.Start(); err == nil {
		fmt.Printf("CHILD=%d\n", child.Process.Pid)
	}
	switch mode {
	case fakeStubborn:
		signal.Ignore(syscall.SIGTERM)
	case fakeTermOnly, fakeNoWS:
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM)
		go func() { <-sig; os.Exit(0) }()
	}
	if mode != fakeNoWS {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(3)
		}
		srv := &http.Server{Handler: &websocket.Server{
			Handshake: func(*websocket.Config, *http.Request) error { return nil },
			Handler: func(ws *websocket.Conn) {
				for {
					var req struct {
						ID     int64  `json:"id"`
						Method string `json:"method"`
					}
					if err := websocket.JSON.Receive(ws, &req); err != nil {
						return
					}
					_ = websocket.JSON.Send(ws, map[string]any{"id": req.ID, "result": map[string]any{}})
					if req.Method == "Browser.close" && mode == fakeGraceful {
						time.AfterFunc(150*time.Millisecond, func() { os.Exit(0) })
					}
				}
			},
		}}
		go func() { _ = srv.Serve(ln) }()
		fmt.Printf("WS=ws://%s/devtools/browser/fake\n", ln.Addr())
	}
	fmt.Println("READY")
	select {}
}

type fakeChrome struct {
	cmd   *exec.Cmd
	ws    string
	child int
	done  chan struct{}
}

func spawnFakeChrome(t *testing.T, mode string) *fakeChrome {
	t.Helper()
	skipOnWindows(t)
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("no sleep binary for the helper child")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestFakeChromeHelper$")
	cmd.Env = append(os.Environ(), fakeChromeModeEnv+"="+mode)
	ApplyOwnedChromeProcAttr(cmd) // 自成进程组：组长就是它自己，和真 Chrome 一样
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	fc := &fakeChrome{cmd: cmd, done: make(chan struct{})}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if fc.child > 0 {
			_ = syscall.Kill(fc.child, syscall.SIGKILL)
		}
	})
	sc := bufio.NewScanner(out)
	deadline := time.After(20 * time.Second)
	ready := make(chan struct{})
	go func() {
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "WS="):
				fc.ws = strings.TrimPrefix(line, "WS=")
			case strings.HasPrefix(line, "CHILD="):
				fc.child, _ = strconv.Atoi(strings.TrimPrefix(line, "CHILD="))
			case line == "READY":
				close(ready)
			}
		}
	}()
	select {
	case <-ready:
	case <-deadline:
		t.Fatal("fake chrome did not become ready")
	}
	go func() { _ = cmd.Wait(); close(fc.done) }()
	return fc
}

func (f *fakeChrome) target() ChromeShutdownTarget {
	return ChromeShutdownTarget{PID: f.cmd.Process.Pid, WSURL: f.ws, Done: f.done}
}

func (f *fakeChrome) requireGone(t *testing.T) {
	t.Helper()
	select {
	case <-f.done:
	default:
		t.Fatal("the browser process is still alive after ShutdownChrome returned")
	}
	if f.child > 0 {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if !isPIDAlive(f.child) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("helper child pid=%d survived: the process group was not swept", f.child)
	}
}

var fastPolicy = ChromeShutdownPolicy{Graceful: true, GracefulWait: 1500 * time.Millisecond, TermWait: 600 * time.Millisecond, KillWait: 3 * time.Second}

func TestShutdownChromeGraceful(t *testing.T) {
	fc := spawnFakeChrome(t, fakeGraceful)
	rc := ShutdownChrome(fc.target(), fastPolicy)
	if rc.Mode != ShutdownGraceful || rc.Reason != "" || rc.Error != "" {
		t.Fatalf("want graceful, got %+v", rc)
	}
	if rc.Ms >= int64(fastPolicy.GracefulWait/time.Millisecond) {
		t.Fatalf("graceful exit took %dms: it must not wait out the whole budget", rc.Ms)
	}
	fc.requireGone(t)
}

// 我们不是它的父进程时（CLI close 一个别的进程启动的 Chrome），没有 Done 通道，按 PID 轮询存活。
func TestShutdownChromeGracefulWithoutDoneChannelPollsPID(t *testing.T) {
	fc := spawnFakeChrome(t, fakeGraceful)
	target := fc.target()
	target.Done = nil
	rc := ShutdownChrome(target, fastPolicy)
	if rc.Mode != ShutdownGraceful {
		t.Fatalf("want graceful, got %+v", rc)
	}
	fc.requireGone(t)
}

func TestShutdownChromeFallsBackToSIGTERM(t *testing.T) {
	fc := spawnFakeChrome(t, fakeTermOnly)
	rc := ShutdownChrome(fc.target(), fastPolicy)
	if rc.Mode != ShutdownTerminated {
		t.Fatalf("want terminated, got %+v", rc)
	}
	if !strings.Contains(rc.Reason, "Browser.close") {
		t.Fatalf("receipt must say why it was not graceful: %+v", rc)
	}
	fc.requireGone(t)
}

func TestShutdownChromeEscalatesToSIGKILL(t *testing.T) {
	fc := spawnFakeChrome(t, fakeStubborn)
	killed := false
	target := fc.target()
	target.Kill = func() error { killed = true; KillChromeProcessGroup(target.PID); <-fc.done; return nil }
	rc := ShutdownChrome(target, fastPolicy)
	if rc.Mode != ShutdownKilled || !killed {
		t.Fatalf("want killed via the target's Kill, got %+v (killed=%v)", rc, killed)
	}
	if !strings.Contains(rc.Reason, "Browser.close") || !strings.Contains(rc.Reason, "SIGTERM") {
		t.Fatalf("receipt must name both steps that failed: %+v", rc)
	}
	fc.requireGone(t)
}

func TestShutdownChromeWithoutCDPEndpointGoesStraightToSIGTERM(t *testing.T) {
	fc := spawnFakeChrome(t, fakeNoWS)
	target := fc.target()
	target.WSURL = ""
	start := time.Now()
	rc := ShutdownChrome(target, fastPolicy)
	if rc.Mode != ShutdownTerminated || !strings.Contains(rc.Reason, "no CDP endpoint") {
		t.Fatalf("got %+v", rc)
	}
	if time.Since(start) >= fastPolicy.GracefulWait {
		t.Fatalf("without an endpoint there is nothing to wait for before SIGTERM (took %s)", time.Since(start))
	}
	fc.requireGone(t)
}

// Browser.close 发不出去（端点已不可达）时不能傻等 GracefulWait：直接升级到 SIGTERM。
func TestShutdownChromeUnreachableEndpointFailsFast(t *testing.T) {
	fc := spawnFakeChrome(t, fakeNoWS)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	_ = ln.Close()
	target := fc.target()
	target.WSURL = "ws://" + dead + "/devtools/browser/x"
	start := time.Now()
	rc := ShutdownChrome(target, fastPolicy)
	if rc.Mode != ShutdownTerminated || !strings.Contains(rc.Reason, "Browser.close failed") {
		t.Fatalf("got %+v", rc)
	}
	if time.Since(start) >= fastPolicy.GracefulWait {
		t.Fatalf("a refused connection must not cost the whole graceful budget (took %s)", time.Since(start))
	}
	fc.requireGone(t)
}

func TestShutdownChromeForcedSkipsEveryGentleStep(t *testing.T) {
	fc := spawnFakeChrome(t, fakeGraceful) // 本来会乖乖退出——但策略要求直接强杀
	policy := ChromeShutdownPolicy{Graceful: false, Reason: "forced"}
	target := fc.target()
	target.Kill = func() error { KillChromeProcessGroup(target.PID); <-fc.done; return nil }
	rc := ShutdownChrome(target, policy)
	if rc.Mode != ShutdownKilled || rc.Reason != "forced" {
		t.Fatalf("got %+v", rc)
	}
	fc.requireGone(t)
}

func TestShutdownChromeReportsGoneWhenAlreadyExited(t *testing.T) {
	fc := spawnFakeChrome(t, fakeGraceful)
	_ = syscall.Kill(-fc.cmd.Process.Pid, syscall.SIGKILL)
	<-fc.done
	rc := ShutdownChrome(fc.target(), fastPolicy)
	if rc.Mode != ShutdownGone {
		t.Fatalf("a process that was already gone must not be reported as graceful: %+v", rc)
	}
}

func TestShutdownReceiptJSONShape(t *testing.T) {
	b, _ := json.Marshal(ShutdownReceipt{Mode: ShutdownGraceful, Ms: 12})
	if string(b) != `{"mode":"graceful","ms":12}` {
		t.Fatalf("receipt shape changed: %s", b)
	}
}

// 生产预算（Coordinator 裁定）：Browser.close 后 8s、SIGTERM 后 2s。满载下实测过 4.1s 才退出，3s 会误杀。
func TestShutdownProductionBudgets(t *testing.T) {
	if ChromeShutdownGracefulWait != 8*time.Second || ChromeShutdownTermWait != 2*time.Second {
		t.Fatalf("budgets changed: graceful=%s term=%s", ChromeShutdownGracefulWait, ChromeShutdownTermWait)
	}
}

// 外层超时必须装得下内层的全部步骤，否则会出现"CLI 已经超时报错、host 里还在封存"。
func TestShutdownOuterTimeoutsFitEveryInnerStep(t *testing.T) {
	steps := ChromeShutdownGracefulWait + ChromeShutdownTermWait + ChromeShutdownKillWait
	if BrowserMuxHostShutdownRequestTimeout < steps+chromeShutdownSealBudget {
		t.Fatalf("HTTP timeout %s cannot hold the shutdown steps %s plus the seal budget %s", BrowserMuxHostShutdownRequestTimeout, steps, chromeShutdownSealBudget)
	}
	if BrowserMuxHostShutdownCallerTimeout <= BrowserMuxHostShutdownRequestTimeout {
		t.Fatalf("the caller's context (%s) must outlive the HTTP client timeout (%s)", BrowserMuxHostShutdownCallerTimeout, BrowserMuxHostShutdownRequestTimeout)
	}
	if chromeShutdownSealBudget <= 30*time.Second {
		t.Fatalf("the seal budget must exceed the 30s vault lock wait, got %s", chromeShutdownSealBudget)
	}
}

// ShutdownChrome 的零值策略用的就是上面这组生产预算：一个"收到 Browser.close 只回应、不退出"的进程，
// 在零值预算下不会在 SIGTERM 之前被放弃——这里用一个很快就退出的假进程验证"默认值不是 0"。
func TestShutdownChromeZeroPolicyBudgetsAreTheProductionDefaults(t *testing.T) {
	fc := spawnFakeChrome(t, fakeGraceful) // 150ms 后自己退出
	rc := ShutdownChrome(fc.target(), ChromeShutdownPolicy{Graceful: true})
	if rc.Mode != ShutdownGraceful {
		t.Fatalf("zero-valued waits must fall back to the defaults, not to 'do not wait': %+v", rc)
	}
}
