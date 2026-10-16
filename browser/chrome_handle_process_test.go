//go:build !windows

package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The executable is this test binary, never a real browser. The leader exits
// while a helper in its own group keeps the inherited stderr pipe open.
func TestChromeHandleControlledProcess(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--chrome-handle-helper" || i+2 >= len(os.Args) {
			continue
		}
		role, dir := os.Args[i+1], os.Args[i+2]
		if role == "writer" {
			_ = os.WriteFile(filepath.Join(dir, "writer-ready"), []byte("pipe held"), 0600)
			fmt.Fprintln(os.Stderr, "owned helper holds stderr")
			until := time.Now().Add(15 * time.Second)
			for time.Now().Before(until) {
				if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
					os.Exit(0)
				}
				time.Sleep(10 * time.Millisecond)
			}
			os.Exit(3)
		}
		if role == "leader" || role == "leader-group" || role == "leader-held" {
			child := exec.Command(os.Args[0], "-test.run=^TestChromeHandleControlledProcess$", "--", "--chrome-handle-helper", "writer", dir)
			child.Stderr = os.Stderr
			if role != "leader-group" {
				child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			}
			if err := child.Start(); err != nil {
				os.Exit(4)
			}
			_ = os.WriteFile(filepath.Join(dir, "writer-pid"), []byte(strconv.Itoa(child.Process.Pid)), 0600)
			if role != "leader" {
				_ = child.Wait()
			}
			os.Exit(0)
		}
		os.Exit(5)
	}
}

func TestDocumentHandleWaitBoundsInheritedStderr(t *testing.T) {
	t.Run("normal-exit", func(t *testing.T) { checkDocumentInheritedStderr(t, false) })
	t.Run("killed-leader", func(t *testing.T) { checkDocumentInheritedStderr(t, true) })
}

func checkDocumentInheritedStderr(t *testing.T, killLeader bool) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(EnvChromeProcRegistryDir, filepath.Join(dir, "registry"))
	ctx, cancel := context.WithCancel(context.Background())
	owner := &documentLaunchOwner{}
	launchDone := make(chan error, 1)
	launchExited := make(chan struct{})
	go func() {
		defer close(launchExited)
		role := "leader"
		if killLeader {
			role = "leader-held"
		}
		_, err := startChromeProcess(ChromeLaunchSpec{
			ChromePath: os.Args[0], Args: []string{"-test.run=^TestChromeHandleControlledProcess$", "--", "--chrome-handle-helper", role, dir},
			DebugPort: 0, documentRead: true, documentOwner: owner, LaunchContext: ctx, ReadyTimeout: 4 * time.Second,
		})
		launchDone <- err
	}()
	var writerPID int
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(dir, "release"), nil, 0600)
		cancel()
		// Only fixture-owned PIDs may be signalled. Never run registry reaping.
		if writerPID > 1 {
			_ = syscall.Kill(writerPID, syscall.SIGKILL)
		}
		if handle := owner.handle.Load(); handle != nil {
			handle.signalKill()
			select {
			case <-handle.doneCh:
			case <-time.After(3 * time.Second):
				t.Error("fixture leader wait did not settle after writer cleanup")
			}
		}
		select {
		case <-launchExited:
		case <-time.After(time.Second):
			t.Error("fixture launch did not settle during cleanup")
		}
		if writerPID > 1 {
			until := time.Now().Add(time.Second)
			for !errors.Is(syscall.Kill(writerPID, 0), syscall.ESRCH) {
				if time.Now().After(until) {
					t.Error("fixture stderr writer did not exit during cleanup")
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	})
	waitFor := func(label string, condition func() bool) {
		t.Helper()
		until := time.Now().Add(3 * time.Second)
		for !condition() {
			if time.Now().After(until) {
				t.Fatalf("timed out waiting for %s", label)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("helper pid", func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "writer-pid"))
		if err != nil {
			return false
		}
		writerPID, err = strconv.Atoi(string(data))
		return err == nil && writerPID > 1
	})
	waitFor("inherited stderr writer", func() bool {
		_, err := os.Stat(filepath.Join(dir, "writer-ready"))
		return err == nil
	})
	if killLeader {
		owner.handle.Load().signalKill()
	}
	waitFor("leader OS exit", func() bool {
		return owner.pid.Load() > 1 && errors.Is(syscall.Kill(int(owner.pid.Load()), 0), syscall.ESRCH)
	})
	if err := syscall.Kill(writerPID, 0); err != nil {
		t.Fatalf("stderr writer must still be alive: %v", err)
	}
	t.Logf("leader exited pid=%d; inherited stderr held by live fixture pid=%d", owner.pid.Load(), writerPID)
	handle := owner.handle.Load()
	if handle == nil {
		t.Fatal("launch did not publish owned handle")
	}
	select {
	case <-handle.doneCh:
		t.Log("handle done settled while inherited stderr writer remained alive")
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("leader exited but handle done is blocked by inherited stderr")
	}
	if handle.stderrErr == nil {
		t.Fatal("inherited writer must remain an explicit cleanup failure")
	}
	if err := handle.killWithContext(context.Background()); err == nil {
		t.Fatal("settled handle must not claim clean cleanup while stderr writer remains")
	}
	if err := syscall.Kill(writerPID, 0); err != nil {
		t.Fatalf("fixture must retain the writer through the bounded wait: %v", err)
	}
	select {
	case err := <-launchDone:
		if err == nil {
			t.Fatal("fake executable supplied no CDP endpoint; launch must fail")
		}
	case <-time.After(time.Second):
		t.Fatal("failed startup did not observe settled handle")
	}
}

func TestDocumentHandleKillReapsOwnedGroup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvChromeProcRegistryDir, filepath.Join(dir, "registry"))
	ctx, cancel := context.WithCancel(context.Background())
	owner := &documentLaunchOwner{}
	launchDone := make(chan error, 1)
	launchExited := make(chan struct{})
	go func() {
		defer close(launchExited)
		_, err := startChromeProcess(ChromeLaunchSpec{
			ChromePath: os.Args[0], Args: []string{"-test.run=^TestChromeHandleControlledProcess$", "--", "--chrome-handle-helper", "leader-group", dir},
			DebugPort: 0, documentRead: true, documentOwner: owner, LaunchContext: ctx, ReadyTimeout: 4 * time.Second,
		})
		launchDone <- err
	}()
	t.Cleanup(func() {
		cancel()
		if handle := owner.handle.Load(); handle != nil {
			handle.signalKill()
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cleanupCancel()
			if err := handle.killWithContext(cleanupCtx); err != nil {
				t.Error(err)
			}
			if err := waitDocumentProcessGroup(cleanupCtx, handle.pgid); err != nil {
				t.Error(err)
			}
		}
		select {
		case <-launchExited:
		case <-time.After(time.Second):
			t.Error("fixture launch did not settle during cleanup")
		}
	})
	until := time.Now().Add(3 * time.Second)
	var writerPID int
	for {
		data, err := os.ReadFile(filepath.Join(dir, "writer-pid"))
		if err == nil {
			writerPID, _ = strconv.Atoi(string(data))
		}
		_, readyErr := os.Stat(filepath.Join(dir, "writer-ready"))
		if writerPID > 1 && readyErr == nil && owner.handle.Load() != nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("owned fixture group was not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	handle := owner.handle.Load()
	pgid, err := syscall.Getpgid(writerPID)
	if err != nil || pgid != handle.pgid {
		t.Fatalf("helper must be in owned leader group: pgid=%d leader-pgid=%d err=%v", pgid, handle.pgid, err)
	}
	killCtx, killCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer killCancel()
	if err := handle.killWithContext(killCtx); err != nil {
		t.Fatal(err)
	}
	// WaitDelay alone proves nothing about helpers: exercise the separate
	// production process-group verification, rather than equating done with it.
	if err := waitDocumentProcessGroup(killCtx, handle.pgid); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(writerPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("owned helper survived group cleanup: %v", err)
	}
	select {
	case err := <-launchDone:
		if err == nil {
			t.Fatal("fixture has no CDP endpoint")
		}
	case <-time.After(time.Second):
		t.Fatal("launch did not settle after group exit")
	}
	t.Logf("owned leader pid=%d and helper pid=%d exited; group=%d verified absent", handle.pid, writerPID, handle.pgid)
}

func TestDocumentStderrTailConcurrentBounded(t *testing.T) {
	buffer := &documentStderrTail{}
	var writers sync.WaitGroup
	for i := 0; i < 4; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for j := 0; j < 100; j++ {
				_, _ = buffer.Write([]byte(strings.Repeat("diagnostic noise", 2048)))
			}
		}()
	}
	finished := make(chan struct{})
	go func() { writers.Wait(); close(finished) }()
	for {
		if snapshot := buffer.String(); len(snapshot) > 16<<10 {
			t.Fatalf("stderr diagnostics grew beyond 16 KiB: %d", len(snapshot))
		}
		select {
		case <-finished:
			_, _ = buffer.Write([]byte("final failure marker"))
			if !strings.HasSuffix(buffer.String(), "final failure marker") {
				t.Fatal("bounded diagnostics lost the final failure")
			}
			return
		default:
		}
	}
}
