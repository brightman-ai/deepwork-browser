//go:build !windows

package browser

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

func useTempChromeRegistry(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvChromeProcRegistryDir, dir)
	return dir
}

// sleeper is a stand-in for Chrome: it ignores SIGTERM and lives in its own
// process group, so the tests exercise the group-kill path rather than a
// single-pid kill, and they need no browser installed.
//
// The reaped channel matters more than it looks. A killed child stays a zombie
// until its parent waits on it, and a zombie answers kill(pid, 0) with success
// — so "is it gone" is only answerable after Wait, not after the kill.
type sleeper struct {
	pid    int
	reaped chan struct{}
}

func startSleeper(t *testing.T) *sleeper {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; while :; do sleep 0.05; done")
	ApplyOwnedChromeProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	s := &sleeper{pid: cmd.Process.Pid, reaped: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(s.reaped)
	}()
	t.Cleanup(func() {
		KillChromeProcessTree(s.pid)
		<-s.reaped
	})
	return s
}

func TestApplyOwnedChromeProcAttrPutsChildInItsOwnProcessGroup(t *testing.T) {
	sleeper := startSleeper(t)
	pid := sleeper.pid
	pgid := ChromeProcessGroupID(pid)
	if pgid != pid {
		t.Fatalf("owned chrome must lead its own process group: pid=%d pgid=%d", pid, pgid)
	}
	if pgid == syscall.Getpgrp() {
		t.Fatalf("owned chrome must not share the launcher's process group (pgid=%d)", pgid)
	}
}

// The self-protection that a broken first draft of the leak gate proved was
// necessary: it group-killed a pgid that happened to be our own and took the
// test binary, `go test` and the shell down with it.
func TestKillChromeProcessGroupIDRefusesOwnProcessGroup(t *testing.T) {
	KillChromeProcessGroupID(syscall.Getpgrp())
	// Reaching the next line at all is the assertion.
	if !isPIDAlive(os.Getpid()) {
		t.Fatal("unreachable")
	}
}

func TestKillChromeProcessTreeKillsGroupLeader(t *testing.T) {
	sleeper := startSleeper(t)
	KillChromeProcessTree(sleeper.pid)
	waitForSleeperGone(t, sleeper)
}

func TestReapAbandonedChromeProcessesKillsWhenOwnerIsGone(t *testing.T) {
	useTempChromeRegistry(t)
	sleeper := startSleeper(t)
	pid := sleeper.pid

	// owner_pid 1 is init: alive, but its start token cannot match ours, and
	// more to the point it is not this process — so the record is abandoned.
	token, err := RegisterChromeProcess(ChromeOwnedByPool, pid, t.TempDir(), false)
	if err != nil {
		t.Fatalf("RegisterChromeProcess: %v", err)
	}
	forceRecordOwner(t, token, 999999, "not-a-real-start-token")

	actions := ReapAbandonedChromeProcesses(ChromeReapOptions{})
	if len(actions) != 1 || actions[0].Action != "killed" {
		t.Fatalf("expected the abandoned record to be killed, got %+v", actions)
	}
	waitForSleeperGone(t, sleeper)
	if got := len(ListChromeProcessRecords()); got != 0 {
		t.Fatalf("reaped record must be removed, %d left", got)
	}
}

func TestReapAbandonedChromeProcessesProtectsThisProcessesOwnChrome(t *testing.T) {
	useTempChromeRegistry(t)
	sleeper := startSleeper(t)
	pid := sleeper.pid
	if _, err := RegisterChromeProcess(ChromeOwnedByCore, pid, t.TempDir(), false); err != nil {
		t.Fatalf("RegisterChromeProcess: %v", err)
	}

	actions := ReapAbandonedChromeProcesses(ChromeReapOptions{})
	if len(actions) != 1 || actions[0].Action != "protected" || actions[0].Reason != "owned_by_this_process" {
		t.Fatalf("a live owner's chrome must be protected, got %+v", actions)
	}
	if !isPIDAlive(pid) {
		t.Fatal("reaper killed a chrome whose owner is still running")
	}
}

func TestReapAbandonedChromeProcessesSkipsDetachedSessionsByDefault(t *testing.T) {
	useTempChromeRegistry(t)
	sleeper := startSleeper(t)
	pid := sleeper.pid
	token, err := RegisterChromeProcess(ChromeDetachedSession, pid, t.TempDir(), false)
	if err != nil {
		t.Fatalf("RegisterChromeProcess: %v", err)
	}
	forceRecordOwner(t, token, 999999, "not-a-real-start-token")

	actions := ReapAbandonedChromeProcesses(ChromeReapOptions{})
	if len(actions) != 1 || actions[0].Reason != "detached_session_by_design" {
		t.Fatalf("`dw-browser open` chrome must survive its launcher, got %+v", actions)
	}
	if !isPIDAlive(pid) {
		t.Fatal("reaper killed an intentionally-detached session chrome")
	}
}

func TestReapAbandonedChromeProcessesDropsRecordForDeadChrome(t *testing.T) {
	useTempChromeRegistry(t)
	sleeper := startSleeper(t)
	if _, err := RegisterChromeProcess(ChromeOwnedByPool, sleeper.pid, t.TempDir(), false); err != nil {
		t.Fatalf("RegisterChromeProcess: %v", err)
	}
	KillChromeProcessTree(sleeper.pid)
	waitForSleeperGone(t, sleeper)

	actions := ReapAbandonedChromeProcesses(ChromeReapOptions{})
	if len(actions) != 1 || actions[0].Reason != "chrome_already_exited" {
		t.Fatalf("stale bookkeeping must be dropped, got %+v", actions)
	}
	if got := len(ListChromeProcessRecords()); got != 0 {
		t.Fatalf("record for a dead chrome must be removed, %d left", got)
	}
}

func TestRegisterChromeProcessRecordsPGIDForLaterGroupKill(t *testing.T) {
	useTempChromeRegistry(t)
	sleeper := startSleeper(t)
	pid := sleeper.pid
	if _, err := RegisterChromeProcess(ChromeOwnedByPool, pid, "/tmp/does-not-matter", false); err != nil {
		t.Fatalf("RegisterChromeProcess: %v", err)
	}
	records := ListChromeProcessRecords()
	if len(records) != 1 {
		t.Fatalf("want 1 record, got %d", len(records))
	}
	if records[0].PGID != pid {
		t.Fatalf("pgid must be captured while the leader lives: want %d got %d", pid, records[0].PGID)
	}
}

// ParseProcCmdline is the fix for the parsing bug that made the first leak scan
// blind: Chrome rewrites its argv into one space-joined blob, so a NUL split
// returns a single token and every flag check silently answers no.
func TestParseProcCmdlineHandlesChromeSpaceJoinedArgv(t *testing.T) {
	nulStyle := []byte("chrome\x00--type=renderer\x00--user-data-dir=/tmp/x\x00")
	got := ParseProcCmdline(nulStyle)
	if len(got) != 3 || got[1] != "--type=renderer" {
		t.Fatalf("NUL-separated argv: got %#v", got)
	}

	chromeStyle := []byte("/snap/chromium/current/chrome --type=zygote --user-data-dir=/tmp/chromedp-runner1 about:blank\x00")
	got = ParseProcCmdline(chromeStyle)
	if ChromeProfileDirFromArgs(got) != "/tmp/chromedp-runner1" {
		t.Fatalf("space-joined argv: profile dir not recovered from %#v", got)
	}
	sawType := false
	for _, arg := range got {
		if strings.HasPrefix(arg, "--type=") {
			sawType = true
		}
	}
	if !sawType {
		t.Fatalf("space-joined argv: --type= not recovered from %#v", got)
	}
}

// gc's allowlist is what makes "kill anything reparented to init" safe to say.
// A regression here would let gc reach the Human's own browser.
func TestChromeGCProfileDirAllowlistExcludesForeignProfiles(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	ours := []string{
		filepath.Join(os.TempDir(), "chromedp-runner12345"),
		filepath.Join(home, ".deepwork", "browser-cli", "task-abc"),
	}
	for _, dir := range ours {
		if !chromeGCProfileDirIsOurs(dir) {
			t.Errorf("gc must recognise its own profile dir: %s", dir)
		}
	}
	foreign := []string{
		"",
		"/",
		filepath.Join(home, ".config", "chromium"),
		filepath.Join(home, "snap", "chromium", "common", "chromium"),
		filepath.Join(home, ".deepwork", "browser-cli"), // the root itself, not a profile
	}
	for _, dir := range foreign {
		if chromeGCProfileDirIsOurs(dir) {
			t.Errorf("gc must never claim a foreign profile dir: %q", dir)
		}
	}
}

func TestIsDisposableChromeProfileDirRefusesArbitraryPaths(t *testing.T) {
	for _, dir := range []string{"", "/", ".", "/home/ubuntu", "/home/ubuntu/.deepwork/browser-cli/keepme"} {
		if isDisposableChromeProfileDir(dir) {
			t.Errorf("reaper must not RemoveAll %q", dir)
		}
	}
	for _, dir := range []string{"/tmp/chromedp-runner99", "/tmp/x/" + BrowserCLIEphemeralProfilePrefix + "1"} {
		if !isDisposableChromeProfileDir(dir) {
			t.Errorf("throwaway profile should be disposable: %q", dir)
		}
	}
}

// The root cause, as a regression test: a TargetTracker with no browser behind
// it used to fabricate a chromedp ExecAllocator, and the pending-target warm
// goroutine then forked a real Chrome that nothing would ever cancel.
func TestTargetTrackerWithoutBrowserNeverFabricatesAnAllocator(t *testing.T) {
	tracker := NewTargetTracker(context.Background())
	seedTrackerPrimary(t, tracker, "root", "https://example.test/", "root")

	tracker.mu.Lock()
	tracked, created := tracker.registerTargetLocked(&target.Info{
		TargetID: "popup", Type: "page", URL: "", OpenerID: "root",
	})
	tracker.mu.Unlock()
	if !created || tracked == nil {
		t.Fatal("target should have been registered")
	}
	if tracked.Ctx != nil {
		t.Fatalf("a tracker with no chromedp browser must not hand out a chromedp ctx: %v", tracked.Ctx)
	}
	if chromedp.FromContext(context.Background()) != nil {
		t.Fatal("sanity: a bare context must carry no chromedp Context")
	}
}

func TestWarmTargetContextRefusesToAllocateABrowser(t *testing.T) {
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background(), chromedp.DefaultExecAllocatorOptions[:]...)
	defer cancel()
	ctx, cancelCtx := chromedp.NewContext(allocCtx)
	defer cancelCtx()

	// Browser is still nil here. Before the fix this call ran chromedp.Run,
	// which forks Chrome; now it must refuse without starting anything.
	if err := warmTargetContext(ctx); err == nil {
		t.Fatal("warmTargetContext must refuse a context with no live browser")
	}
}

func forceRecordOwner(t *testing.T, token string, ownerPID int, ownerBoot string) {
	t.Helper()
	path := filepath.Join(ChromeProcRegistryDir(), token+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	patched := strings.Replace(string(body),
		`"owner_pid":`+itoa(os.Getpid()), `"owner_pid":`+itoa(ownerPID), 1)
	patched = replaceOwnerBoot(patched, ownerBoot)
	if err := os.WriteFile(path, []byte(patched), 0o600); err != nil {
		t.Fatalf("write record: %v", err)
	}
}

func replaceOwnerBoot(body, ownerBoot string) string {
	start := strings.Index(body, `"owner_boot":"`)
	if start < 0 {
		return strings.Replace(body, `,"chrome_pid"`, `,"owner_boot":"`+ownerBoot+`","chrome_pid"`, 1)
	}
	valueStart := start + len(`"owner_boot":"`)
	end := strings.Index(body[valueStart:], `"`)
	if end < 0 {
		return body
	}
	return body[:valueStart] + ownerBoot + body[valueStart+end:]
}

func waitForSleeperGone(t *testing.T, s *sleeper) {
	t.Helper()
	select {
	case <-s.reaped:
	case <-time.After(5 * time.Second):
		t.Fatalf("pid %d still alive after kill", s.pid)
	}
}
