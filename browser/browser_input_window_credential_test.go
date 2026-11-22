//go:build !windows

package browser

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---- Covers:凭证校验 ----

func TestCoversRejectsMissingReleasedAndForeignCredentials(t *testing.T) {
	key := inputLockTestKey(t)
	other := key + "-other"
	t.Cleanup(func() {
		_ = os.Remove(browserInputLockPath(other))
		_ = os.Remove(browserInputOwnerPath(other))
	})

	var none *BrowserInputLock
	if err := none.Covers(key); !errors.Is(err, ErrInputWindowNotHeld) {
		t.Fatalf("nil 凭证: want ErrInputWindowNotHeld, got %v", err)
	}

	w, err := AcquireBrowserInputLock(key, "test.cover", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Covers(key); err != nil {
		t.Fatalf("有效凭证被拒绝: %v", err)
	}
	if err := w.Covers(other); !errors.Is(err, ErrInputWindowNotHeld) {
		t.Fatalf("key 不同的凭证: want ErrInputWindowNotHeld, got %v", err)
	}
	if err := w.Covers(""); !errors.Is(err, ErrInputWindowNotHeld) {
		t.Fatalf("持有凭证却要覆盖空 key(两端绑定不一致): want ErrInputWindowNotHeld, got %v", err)
	}
	w.Release()
	if err := w.Covers(key); !errors.Is(err, ErrInputWindowNotHeld) {
		t.Fatalf("已释放的凭证: want ErrInputWindowNotHeld, got %v", err)
	}
	w.Release() // 重复释放必须安全
}

// key 为空 = 输入临界区退化为不加锁:此时 nil 凭证就是"覆盖"。
func TestCoversAcceptsNilCredentialWhenNoRuntimeKeyExists(t *testing.T) {
	var none *BrowserInputLock
	if err := none.Covers(""); err != nil {
		t.Fatalf("没有 key 的运行时不该因为 nil 凭证失败: %v", err)
	}
}

// ---- 超时诊断:同进程 ----

func TestTimeoutReportsInProcessHolderWithBothOps(t *testing.T) {
	key := inputLockTestKey(t)
	held, err := AcquireBrowserInputLock(key, "action.dispatch", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	_, err = AcquireBrowserInputLock(key, "tracker.activate", 200*time.Millisecond)
	if !errors.Is(err, ErrInputWindowReentrant) {
		t.Fatalf("同进程占用: want ErrInputWindowReentrant, got %v", err)
	}
	var re *InputWindowReentrantError
	if !errors.As(err, &re) || re.HolderOp != "action.dispatch" || re.RequesterOp != "tracker.activate" {
		t.Fatalf("没有同时点出双方 op: %#v", re)
	}
	msg := err.Error()
	// D1:文案不得断言比证据更强的结论——既说"疑似同进程重入",也说"可能是持有者卡住",且承认无法区分。
	for _, want := range []string{"本进程内已持有", "疑似同进程重入", "持有者卡住", "无法区分", "action.dispatch", "tracker.activate", "输入临界区"} {
		if !strings.Contains(msg, want) {
			t.Errorf("报错缺少 %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "内部缺陷") {
		t.Fatalf("同进程超时不能断言成 dw-browser 内部缺陷(也可能是持有者卡住):\n%s", msg)
	}
	if strings.Contains(msg, "另一个操作者已持有") {
		t.Fatalf("自锁被伪装成了另一个操作者:\n%s", msg)
	}
	t.Logf("同进程重入报错文案:\n%s", msg)
}

// 释放后登记必须清空:否则之后的他进程争用会被误判成本进程重入。
func TestReleaseClearsInProcessHolderAndOwner(t *testing.T) {
	key := inputLockTestKey(t)
	SetInputWindowCommand("act click --id s1")
	t.Cleanup(func() { SetInputWindowCommand("") })
	w, err := AcquireBrowserInputLock(key, "action.dispatch", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := readInputWindowOwner(browserInputOwnerPath(key))
	if !ok || owner.PID != os.Getpid() || owner.Op != "action.dispatch" || owner.Command != "act click --id s1" {
		t.Fatalf("持锁后 .owner 内容不对: ok=%t %+v", ok, owner)
	}
	w.Release()
	if inProcessHolder(key) != nil {
		t.Fatal("释放后进程内登记没有清空")
	}
	if _, err := os.Stat(browserInputOwnerPath(key)); !os.IsNotExist(err) {
		t.Fatalf("释放后 .owner 残留: %v", err)
	}
}

// ---- 超时诊断:他进程(helper process) ----

const inputLockHolderEnv = "DW_TEST_INPUT_LOCK_HOLDER_KEY"

// 作为子进程运行:持锁并登记身份,直到 stdin 关闭。
func TestInputLockHolderHelperProcess(t *testing.T) {
	key := os.Getenv(inputLockHolderEnv)
	if key == "" {
		return // 只在被 startInputLockHolder 以子进程方式拉起时才有内容(不产生 SKIP)
	}
	SetInputWindowCommand("act click --id helper-session")
	w, err := AcquireBrowserInputLock(key, "action.dispatch", 2*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stdout, "ERR", err)
		os.Exit(2)
	}
	defer w.Release()
	fmt.Fprintln(os.Stdout, "READY", os.Getpid())
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func startInputLockHolder(t *testing.T, key string) (pid int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInputLockHolderHelperProcess$")
	cmd.Env = append(os.Environ(), inputLockHolderEnv+"="+key)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "READY") {
		t.Fatalf("helper 没有就绪: %q err=%v", line, err)
	}
	pid, err = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "READY")))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestTimeoutReportsOtherProcessHolderPidAndCommand(t *testing.T) {
	key := inputLockTestKey(t)
	pid := startInputLockHolder(t, key)

	_, err := AcquireBrowserInputLock(key, "cli.tabs.select", 300*time.Millisecond)
	var busy *BrowserInputBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("他进程占用: want *BrowserInputBusyError, got %T %v", err, err)
	}
	if errors.Is(err, ErrInputWindowReentrant) {
		t.Fatal("他进程持有被误判为同进程重入")
	}
	if !busy.OwnerKnown || busy.Owner.PID != pid {
		t.Fatalf("没有点出持有者 pid=%d: %+v", pid, busy)
	}
	if busy.Owner.Command != "act click --id helper-session" || busy.Owner.Op != "action.dispatch" {
		t.Fatalf("持有者身份不对: %+v", busy.Owner)
	}
	msg := err.Error()
	for _, want := range []string{fmt.Sprintf("pid %d", pid), "act click --id helper-session", "op=action.dispatch", "cli.tabs.select", "输入临界区"} {
		if !strings.Contains(msg, want) {
			t.Errorf("报错缺少 %q:\n%s", want, msg)
		}
	}
	t.Logf("他进程占用报错文案:\n%s", msg)
}

// 持有者没留下身份(.owner 不存在或进程已死)时保留原有措辞,不编造身份。
func TestTimeoutWithUnknownHolderKeepsGenericMessage(t *testing.T) {
	key := inputLockTestKey(t)
	if err := os.MkdirAll(browserInputLocksDir, 0755); err != nil {
		t.Fatal(err)
	}
	// 绕过 Acquire 直接占 flock:既不登记进程内持有者,也不写 .owner。
	lease, ok, err := acquireExclusiveFileLock(browserInputLockPath(key), time.Second)
	if err != nil || !ok {
		t.Fatalf("raw flock: ok=%t err=%v", ok, err)
	}
	defer lease.Close()

	_, err = AcquireBrowserInputLock(key, "action.dispatch", 100*time.Millisecond)
	var busy *BrowserInputBusyError
	if !errors.As(err, &busy) || busy.OwnerKnown {
		t.Fatalf("want BrowserInputBusyError{OwnerKnown:false}, got %#v", err)
	}
	if !strings.Contains(err.Error(), "另一个操作者已持有该浏览器实例的输入临界区") {
		t.Fatalf("未知持有者没有保留原有措辞:\n%s", err)
	}

	// 残留的死进程身份不得拿来误导人。
	dead := InputWindowOwner{PID: 0x7ffffffe, Op: "action.dispatch", Since: time.Now()}
	if _, ok := readInputWindowOwner(writeOwnerFixture(t, key, dead)); ok {
		t.Fatal("已不存在的进程被当成了持有者")
	}
}

func writeOwnerFixture(t *testing.T, key string, o InputWindowOwner) string {
	t.Helper()
	path := browserInputOwnerPath(key)
	data := fmt.Sprintf(`{"pid":%d,"op":%q,"since":%q}`, o.PID, o.Op, o.Since.Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ---- 取锁点审计(防回退) ----

// 全仓库的取锁点都已逐个审计过(见下表)。新增取锁点会让本测试失败:请先判断它是
// "窗口外入口"还是"窗口内必须持凭证",再更新这张表 —— 而不是在窗口内随手取锁。
//
// 归类:
//
//	窗口外入口(自己取锁,持锁期间不再取同 key 的锁):
//	  action_engine.go        action.dispatch      → 钩子/authorityCheck 只接凭证,不取锁
//	  browser_core_impl.go    takeover.dispatch    → takeoverCtrl.DispatchInput 不取锁
//	  input_gateway.go        gateway.input / gateway.release-held(wrapper 定义 1 + 调用 2)
//	  target_tracker.go       tracker.activate ×2 / tracker.close-target / tracker.create-tab
//	                          (wrapper 定义 1;activate 的窗口内版本是 ActivateIn/ActivateNoFrontIn)
//	  safari/action_typed.go  safari.action
func TestInputLockAcquireSitesAreAudited(t *testing.T) {
	type site struct {
		file    string
		pattern *regexp.Regexp
		want    int
	}
	sites := []site{
		// 直接 Acquire:wrapper 内部各 1 处 + 窗口外入口 2 处(action 派发、takeover 派发)
		{"action_engine.go", regexp.MustCompile(`\bAcquireBrowserInputLock\(`), 1},
		{"browser_core_impl.go", regexp.MustCompile(`\bAcquireBrowserInputLock\(`), 1},
		{"input_gateway.go", regexp.MustCompile(`\bAcquireBrowserInputLock\(`), 1},
		{"target_tracker.go", regexp.MustCompile(`\bAcquireBrowserInputLock\(`), 1},
		// wrapper 的调用点
		{"input_gateway.go", regexp.MustCompile(`\bg\.acquireRuntimeInputLock\(`), 2},
		{"target_tracker.go", regexp.MustCompile(`\btt\.acquireInputLock\(`), 4},
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, s := range sites {
		covered[s.file] = true
		src := stripGoComments(readPackageSource(t, s.file))
		if got := len(s.pattern.FindAllString(src, -1)); got != s.want {
			t.Errorf("%s: %s = %d 处, want %d —— 新增/移除了取锁点,请先审计(窗口外入口 or 窗口内持凭证)并更新本表",
				s.file, s.pattern, got, s.want)
		}
	}
	anyAcquire := regexp.MustCompile(`\bAcquireBrowserInputLock\(`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "browser_input_lock.go" || covered[f] {
			continue
		}
		if n := len(anyAcquire.FindAllString(stripGoComments(readPackageSource(t, f)), -1)); n > 0 {
			t.Errorf("%s 出现未审计的取锁点 (%d 处)", f, n)
		}
	}
}

var lineComment = regexp.MustCompile(`(?m)//.*$`)

func stripGoComments(src string) string { return lineComment.ReplaceAllString(src, "") }
