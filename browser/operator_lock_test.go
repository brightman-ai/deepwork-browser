package browser

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// helperHoldsLockEnv 让被重新执行的测试二进制变成"第二个真实进程"。
// 用真进程而不是同进程内两次取锁,是因为 flock 的语义恰恰是按打开的文件描述符
// 算的 —— 同进程内的自我竞争根本测不到我们依赖的那条内核行为(进程死 → 锁自动放)。
const helperHoldsLockEnv = "DW_TEST_HOLD_SESSION_LOCK"

// TestHelperHoldsSessionLock 只在被父测试重新执行时才真的跑。
// 它取锁 → 打印自己的 pid → 一直持有,等父进程决定怎么弄死它。
func TestHelperHoldsSessionLock(t *testing.T) {
	sessionID := os.Getenv(helperHoldsLockEnv)
	if sessionID == "" {
		t.Skip("helper process: only runs when re-executed by the concurrency test")
	}
	lock, err := AcquireSessionOperatorLock(sessionID, "act --id "+sessionID+" \"click @r1\"", time.Second)
	if err != nil {
		fmt.Printf("HELPER-FAILED %v\n", err)
		os.Stdout.Sync()
		os.Exit(1)
	}
	fmt.Printf("HELPER-ACQUIRED %d\n", os.Getpid())
	os.Stdout.Sync()
	// 持锁不放,直到父进程杀掉我们。上界只是防止测试挂掉后留下孤儿。
	time.Sleep(60 * time.Second)
	lock.Release()
}

// startLockHolder 起一个真实的第二进程并等它确认已经持锁。
func startLockHolder(t *testing.T, sessionID string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldsSessionLock$", "-test.v")
	cmd.Env = append(os.Environ(), helperHoldsLockEnv+"="+sessionID)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	acquired := make(chan int, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "HELPER-ACQUIRED ") {
				pid, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "HELPER-ACQUIRED ")))
				acquired <- pid
				return
			}
			if strings.HasPrefix(line, "HELPER-FAILED") {
				acquired <- 0
				return
			}
		}
		acquired <- 0
	}()

	select {
	case pid := <-acquired:
		if pid <= 0 {
			t.Fatal("helper process failed to acquire the session lock")
		}
		if pid != cmd.Process.Pid {
			t.Fatalf("helper reported pid %d but process is %d", pid, cmd.Process.Pid)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("helper process never confirmed it holds the lock")
	}
	return cmd
}

func lockTestSessionID(t *testing.T) string {
	t.Helper()
	id := fmt.Sprintf("locktest-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		_ = os.Remove(SessionOperatorLockPath(id))
		_ = os.Remove(sessionOperatorOwnerPath(id))
	})
	return id
}

// 第二操作者必须有界等待后响亮失败,并且点名前一个操作者的 pid。
//
// 这是整个机制的可用性核心:被拒的人如果只看到"失败了",他会去重试、去怀疑工具;
// 只有看到"pid N 正在操作这个会话",他才知道该去找谁、或者该等。
func TestSecondOperatorWaitsBoundedThenNamesTheHolder(t *testing.T) {
	sessionID := lockTestSessionID(t)
	holder := startLockHolder(t, sessionID)

	wait := 700 * time.Millisecond
	started := time.Now()
	lock, err := AcquireSessionOperatorLock(sessionID, "observe --id "+sessionID, wait)
	elapsed := time.Since(started)
	if err == nil {
		lock.Release()
		t.Fatal("第二操作者拿到了锁 —— 一个会话同时只该接受一个操作者")
	}

	// 有界:既不能立刻放弃(那不叫等待),也不能超出请求的等待时长太多。
	if elapsed < wait {
		t.Fatalf("等待 %s 就放弃了, 少于请求的 %s —— 等待没有真正发生", elapsed, wait)
	}
	if elapsed > wait+2*time.Second {
		t.Fatalf("等了 %s, 远超请求的 %s —— 等待不是有界的", elapsed, wait)
	}

	var busy *SessionBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("错误类型 %T, want *SessionBusyError: %v", err, err)
	}
	if busy.Owner.PID != holder.Process.Pid {
		t.Fatalf("错误点名 pid %d, 真正的占有者是 pid %d", busy.Owner.PID, holder.Process.Pid)
	}
	msg := err.Error()
	for _, want := range []string{
		sessionID,
		fmt.Sprintf("pid %d", holder.Process.Pid),
		"一个会话同时只接受一个操作者",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误信息缺少 %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "click @r1") {
		t.Fatalf("错误信息没有点出占有者在做什么:\n%s", msg)
	}
}

// 占有者被 SIGKILL 后,锁必须立刻可得 —— 不留脏锁是选 flock 而不是 pidfile 的
// 全部理由。SIGKILL 不给进程任何清理机会,所以这条只能靠内核保证,不能靠代码。
func TestLockIsImmediatelyAvailableAfterHolderIsSIGKILLed(t *testing.T) {
	sessionID := lockTestSessionID(t)
	holder := startLockHolder(t, sessionID)

	if err := holder.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_, _ = holder.Process.Wait()

	// 等待时长给 0:不允许靠"等一等"蒙混过关,必须是立刻可得。
	// (内核在进程回收时放锁,Wait 返回即已完成。)
	started := time.Now()
	lock, err := AcquireSessionOperatorLock(sessionID, "observe --id "+sessionID, 0)
	if err != nil {
		t.Fatalf("占有者被 SIGKILL 后锁仍不可得(等待 0s): %v —— 这就是脏锁", err)
	}
	defer lock.Release()
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("取锁花了 %s —— SIGKILL 之后本该是立刻可得", elapsed)
	}
}

// 死掉的占有者不该继续被当成"正在操作的人":owner 文件是释放时不删的附注,
// 拿一条僵尸身份去指责别人比不指责更糟。
func TestStaleOwnerNoteIsNotUsedToNameADeadHolder(t *testing.T) {
	sessionID := lockTestSessionID(t)
	holder := startLockHolder(t, sessionID)
	deadPID := holder.Process.Pid
	_ = holder.Process.Signal(syscall.SIGKILL)
	_, _ = holder.Process.Wait()

	// owner 文件还在(释放不删),但里面那个 pid 已经不存在了。
	if owner, ok := readOperatorOwner(sessionOperatorOwnerPath(sessionID)); ok {
		t.Fatalf("僵尸 owner 记录被当成有效占有者: pid %d (已死)", owner.PID)
	}
	if _, err := os.Stat(sessionOperatorOwnerPath(sessionID)); err != nil {
		t.Fatalf("owner 文件应当还在(释放不删,以持锁为准): %v", err)
	}
	_ = deadPID
}

// 同一进程内的第二次取锁也必须被拒 —— 会话锁是"一个操作者",不是"一个进程"。
func TestSameProcessCannotTakeTheSameSessionTwice(t *testing.T) {
	sessionID := lockTestSessionID(t)
	first, err := AcquireSessionOperatorLock(sessionID, "act --id "+sessionID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	second, err := AcquireSessionOperatorLock(sessionID, "observe --id "+sessionID, 100*time.Millisecond)
	if err == nil {
		second.Release()
		t.Fatal("同一进程内重复取到了同一把会话锁")
	}
	var busy *SessionBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("错误类型 %T, want *SessionBusyError", err)
	}
}

// 不同会话之间互不阻塞 —— 串行化的粒度是会话,不是整个工具。
func TestDifferentSessionsDoNotBlockEachOther(t *testing.T) {
	a := lockTestSessionID(t)
	b := lockTestSessionID(t)
	lockA, err := AcquireSessionOperatorLock(a, "act --id "+a, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer lockA.Release()
	lockB, err := AcquireSessionOperatorLock(b, "act --id "+b, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("会话 %s 的锁被会话 %s 挡住了: %v", b, a, err)
	}
	lockB.Release()
}

// 命令摘要会被原样打印到另一个操作者的终端上,所以密码不该走这条路。
func TestOperatorCommandSummaryMasksSecrets(t *testing.T) {
	summary := SummarizeOperatorCommand([]string{"act", "--id", "s1", "fillsecret @r3 'hunter2'"})
	if strings.Contains(summary, "hunter2") {
		t.Fatalf("fillsecret 的值泄漏进了占有者摘要: %s", summary)
	}
	if !strings.Contains(summary, "fillsecret") {
		t.Fatalf("摘要连动词都丢了, 说不清占有者在干嘛: %s", summary)
	}

	long := SummarizeOperatorCommand([]string{"eval", "--id", "s1", strings.Repeat("x", 500)})
	if len([]rune(long)) > 120 {
		t.Fatalf("摘要没有截断, 会把半屏 JS 甩进别人的错误信息: %d runes", len([]rune(long)))
	}
}
