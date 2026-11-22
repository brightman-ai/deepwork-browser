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

// 登记表（chrome_registry.go）和 `profile prune --reap-orphaned`（cli_profile_cleanup.go）会对"记着的 pid"发 SIGKILL。
// 这些记录可能在崩溃或重启之后残留，pid（甚至 pgid）早就被系统分给了不相干的程序。发信号之前必须核对该 pid 的命令行
// 仍是登记时的那种 Chrome（带这个 profile 目录的 --user-data-dir）；对不上：一个信号都不发，把它当陈旧记录清掉，那个进程仍然活着。
// 反例用测试自己起的 sleep——和 other-muxd 之类的无关守护进程在"命令行里没有 --user-data-dir"这一点上等价。

func stillAlive(t *testing.T, pid int, what string) {
	t.Helper()
	time.Sleep(200 * time.Millisecond) // KILL 是同步发出的；给一点时间才看得出"没发"
	if !isPIDAlive(pid) {
		t.Fatalf("%s: pid %d was signalled although its command line is not the recorded Chrome", what, pid)
	}
}

// forgeRecord 登记一条记录，再把它改成"owner 已死"，并可改写 pgid。
func forgeRecord(t *testing.T, kind ChromeOwnershipKind, chromePID int, profileDir string, pgid int) string {
	t.Helper()
	token, err := RegisterChromeProcess(kind, chromePID, profileDir, false)
	if err != nil {
		t.Fatalf("RegisterChromeProcess: %v", err)
	}
	forceRecordOwner(t, token, 999999, "not-a-real-start-token")
	if pgid != 0 {
		path := filepath.Join(ChromeProcRegistryDir(), token+".json")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		patched := strings.Replace(string(body), fmt.Sprintf(`"pgid":%d`, ChromeProcessGroupID(chromePID)), fmt.Sprintf(`"pgid":%d`, pgid), 1)
		if err := os.WriteFile(path, []byte(patched), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return token
}

// 陈旧登记：pid 现在属于一个无关进程（它还是自己进程组的 leader，pgid 也对得上）。
func TestReapNeverSignalsARecordWhoseChromePidIsNotThatChrome(t *testing.T) {
	useTempChromeRegistry(t)
	stranger := spawnLongSleeper(t)
	if ChromeProcessGroupID(stranger) != stranger {
		t.Fatal("test setup: the stranger must lead its own process group")
	}
	forgeRecord(t, ChromeOwnedByPool, stranger, t.TempDir(), 0)

	actions := ReapAbandonedChromeProcesses(ChromeReapOptions{})
	if len(actions) != 1 || actions[0].Action != "record_removed" || !strings.HasPrefix(actions[0].Reason, "pid_not_ours") {
		t.Fatalf("a record whose pid is not its Chrome must be dropped as stale: %+v", actions)
	}
	if !strings.Contains(actions[0].Reason, "sleep") {
		t.Fatalf("the reason must show what the pid actually is: %q", actions[0].Reason)
	}
	stillAlive(t, stranger, "unrelated process with a recorded pid")
	if got := len(ListChromeProcessRecords()); got != 0 {
		t.Fatalf("the stale record must be removed, %d left", got)
	}
}

// dry-run：一样不发信号，也不动记录；报告和真跑一致（record_removed，而不是 would_kill）。
func TestReapDryRunReportsStaleRecordAndTouchesNothing(t *testing.T) {
	useTempChromeRegistry(t)
	stranger := spawnLongSleeper(t)
	forgeRecord(t, ChromeOwnedByPool, stranger, t.TempDir(), 0)

	actions := ReapAbandonedChromeProcesses(ChromeReapOptions{DryRun: true})
	if len(actions) != 1 || actions[0].Action != "record_removed" || !strings.HasPrefix(actions[0].Reason, "pid_not_ours") {
		t.Fatalf("dry run must say the same thing as a real run: %+v", actions)
	}
	stillAlive(t, stranger, "dry run")
	if got := len(ListChromeProcessRecords()); got != 1 {
		t.Fatalf("a dry run must leave the record alone, %d records", got)
	}
}

// 记录里的 pgid 不可信：pid 核验通过也不能因为记录写了某个 pgid 就去杀别人的进程组。
func TestReapNeverGroupKillsARecordedPgidThatIsNotTheVerifiedPidsGroup(t *testing.T) {
	useTempChromeRegistry(t)
	profileDir := t.TempDir()
	chrome := spawnChromeLookalike(t, profileDir)
	bystander := spawnLongSleeper(t) // 自己的进程组；pgid == pid
	forgeRecord(t, ChromeOwnedByPool, chrome, profileDir, bystander)

	actions := ReapAbandonedChromeProcesses(ChromeReapOptions{})
	if len(actions) != 1 || actions[0].Action != "killed" {
		t.Fatalf("the genuine Chrome is still reaped: %+v", actions)
	}
	waitPIDGone(t, chrome)
	stillAlive(t, bystander, "process group named by a forged pgid")
}

// 已退出的 Chrome 的记录：pgid 即使还有活人，也不去杀。
func TestReapNeverGroupKillsThePgidOfADeadChromeRecord(t *testing.T) {
	useTempChromeRegistry(t)
	bystander := spawnLongSleeper(t)
	// 退出的 pid 读不出 live pgid，forgeRecord 直接把记录里的 pgid 写成 bystander 的组。
	forgeRecord(t, ChromeOwnedByPool, deadPID(t), t.TempDir(), bystander)

	actions := ReapAbandonedChromeProcesses(ChromeReapOptions{})
	if len(actions) != 1 || actions[0].Reason != "chrome_already_exited" {
		t.Fatalf("a dead chrome's record is only bookkeeping: %+v", actions)
	}
	stillAlive(t, bystander, "pgid of a dead chrome's record")
}

// 兄弟 profile：/x/task-1 的记录不能因为 /x/task-10 的 Chrome 命令行"以它开头"就被认成自己的。
func TestReapDoesNotMistakeASiblingProfilesChromeForTheRecordedOne(t *testing.T) {
	useTempChromeRegistry(t)
	root := t.TempDir()
	recorded := filepath.Join(root, "task-1")
	sibling := spawnChromeLookalike(t, filepath.Join(root, "task-10"))
	forgeRecord(t, ChromeOwnedByPool, sibling, recorded, 0)

	actions := ReapAbandonedChromeProcesses(ChromeReapOptions{})
	if len(actions) != 1 || actions[0].Action != "record_removed" || !strings.HasPrefix(actions[0].Reason, "pid_not_ours") {
		t.Fatalf("sibling profile's chrome is not the recorded one: %+v", actions)
	}
	stillAlive(t, sibling, "sibling profile's Chrome")
}

// 登记里没有 profile 目录（RegisterChromeProcess 把空目录 Clean 成 "."）：退而求其次，至少得是带 --remote-debugging-port 的 Chrome。
func TestReapWithoutProfileDirRequiresADebuggableChrome(t *testing.T) {
	useTempChromeRegistry(t)
	stranger := spawnLongSleeper(t)
	forgeRecord(t, ChromeOwnedByLauncher, stranger, "", 0)

	actions := ReapAbandonedChromeProcesses(ChromeReapOptions{})
	if len(actions) != 1 || actions[0].Action != "record_removed" || !strings.HasPrefix(actions[0].Reason, "pid_not_ours") {
		t.Fatalf("no profile dir and not a CDP Chrome: %+v", actions)
	}
	stillAlive(t, stranger, "unrelated process, record without profile dir")

	chrome := spawnChromeLookalike(t, t.TempDir()) // 命令行带 --remote-debugging-port=0
	forgeRecord(t, ChromeOwnedByLauncher, chrome, "", 0)
	actions = ReapAbandonedChromeProcesses(ChromeReapOptions{})
	if len(actions) != 1 || actions[0].Action != "killed" {
		t.Fatalf("a CDP Chrome with no recorded profile dir is still reaped: %+v", actions)
	}
	waitPIDGone(t, chrome)
}

// 正例：真属于这个 profile 的 Chrome 照旧被清理，记录删除。
func TestReapStillKillsTheRecordedChromeOfTheProfile(t *testing.T) {
	useTempChromeRegistry(t)
	profileDir := t.TempDir() + "/" // 带尾斜杠写的目录也是同一个目录
	chrome := spawnChromeLookalike(t, profileDir)
	forgeRecord(t, ChromeOwnedByPool, chrome, profileDir, 0)

	actions := ReapAbandonedChromeProcesses(ChromeReapOptions{})
	if len(actions) != 1 || actions[0].Action != "killed" || actions[0].Reason != "owner_gone" {
		t.Fatalf("the abandoned Chrome of the recorded profile must be reaped: %+v", actions)
	}
	waitPIDGone(t, chrome)
	if got := len(ListChromeProcessRecords()); got != 0 {
		t.Fatalf("reaped record must be removed, %d left", got)
	}
}

// ---- profile prune --reap-orphaned ----

func pruneReap(t *testing.T, root string) BrowserCLIEphemeralPruneResult {
	t.Helper()
	result, err := PruneBrowserCLIEphemeralProfiles(BrowserCLIEphemeralPruneOptions{
		Roots:        []string{root},
		MinAge:       30 * time.Minute,
		ReapOrphaned: true,
		Now:          time.Now(),
	})
	if err != nil {
		t.Fatalf("PruneBrowserCLIEphemeralProfiles: %v", err)
	}
	return result
}

func orphanProfile(t *testing.T, root, name, marker string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	writeProfileFile(t, dir, "payload")
	if marker != "" {
		writeMarker(t, dir, marker)
	}
	mustChtimes(t, dir, time.Now().Add(-2*time.Hour))
	return dir
}

// marker 里的 chrome/owner/host pid 都已被系统分给无关进程：一个信号都不发，profile 当作没人在用而清掉。
func TestPruneReapNeverSignalsPidsThatAreNotTheProfilesChrome(t *testing.T) {
	root := t.TempDir()
	stranger := spawnLongSleeper(t)
	dir := orphanProfile(t, root, "task-stale-marker", fmt.Sprintf(
		`{"owner_pid":%d,"chrome_pid":%d,"identity_key":"k","browser_mux_host_id":"browser-mux-host-global","browser_mux_host_pid":%d}`,
		stranger, stranger, stranger))

	result := pruneReap(t, root)
	if result.Removed != 1 || result.Protected != 0 {
		t.Fatalf("a profile held only by stale pids is not in use: %+v", result)
	}
	stillAlive(t, stranger, "unrelated process named by the owner marker")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the profile dir should be removed: %v", err)
	}
	notes := result.Entries[0].Notes
	if len(notes) != 1 || !strings.Contains(notes[0], fmt.Sprintf("pid %d not signalled", stranger)) || !strings.Contains(notes[0], "sleep") {
		t.Fatalf("the skipped pid must be recorded with what it really is: %q", notes)
	}
}

// 同一个根目录下 task-1 和 task-10：ps 扫描是子串匹配，会把 task-10 的 Chrome 当成 task-1 的；核验必须把它挡住。
func TestPruneReapDoesNotKillASiblingProfilesChrome(t *testing.T) {
	root := t.TempDir()
	sibling := spawnChromeLookalike(t, filepath.Join(root, "task-10")) // task-10 的目录本身不存在
	dir := orphanProfile(t, root, "task-1", "")

	result := pruneReap(t, root)
	if result.Removed != 1 {
		t.Fatalf("task-1 has no Chrome of its own and should be removed: %+v", result)
	}
	stillAlive(t, sibling, "Chrome of the sibling profile task-10")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("task-1 should be removed: %v", err)
	}
}

// 共享的 BrowserMuxHost 不会因为一个孤儿 profile 被杀（它是 `muxhost shutdown` 的事）；只杀这个 profile 自己的 Chrome。
func TestPruneReapKillsTheProfilesChromeButNeverTheMuxHost(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "task-with-host")
	chrome := spawnChromeLookalike(t, dir)
	host := spawnMuxHostLookalike(t, "browser-mux-host-global")
	orphanProfile(t, root, "task-with-host", fmt.Sprintf(
		`{"owner_pid":%d,"chrome_pid":%d,"identity_key":"k","browser_mux_host_id":"browser-mux-host-global","browser_mux_host_pid":%d}`,
		host, chrome, host))

	result := pruneReap(t, root)
	if result.Removed != 1 {
		t.Fatalf("the orphaned profile should be reaped and removed: %+v", result)
	}
	waitPIDGone(t, chrome)
	stillAlive(t, host, "shared BrowserMuxHost")
	if n := len(result.Entries[0].Notes); n != 1 {
		t.Fatalf("exactly the host pid should be noted as not signalled: %q", result.Entries[0].Notes)
	}
}

func waitPIDGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !isPIDAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d should have been killed", pid)
}
