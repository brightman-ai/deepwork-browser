// Package browser — chromeHandleImpl: cross-platform ChromeHandle owning *exec.Cmd.
//
// Used by:
//   - All Workspace implementations (darwin/linux/windows/other) — wraps the
//     Chrome fork they spawn and exposes lifecycle to caller via ChromeHandle.
//   - NoopWorkspace fallback for tests.
//
// Invariants:
//   - cmd.Process != nil after successful startChromeProcess return.
//   - doneCh is closed when cmd.Wait returns (any cause).
//   - Kill is idempotent — multiple callers wait on the same doneCh.
//   - Wait blocks until process exits and returns cmd.Wait's error.
//
// [Ref: TH-0419 — Workspace as visible-Chrome SSOT]
package browser

import (
	"bytes"
	"fmt"
	"log"
	"os/exec"
	"sync"
	"time"
)

// chromeHandleImpl — cross-platform ChromeHandle owning *exec.Cmd.
type chromeHandleImpl struct {
	cmd      *exec.Cmd
	pid      int
	wsURL    string
	doneCh   chan struct{}
	waitErr  error
	killOnce sync.Once
	// regToken is the on-disk ownership note (chrome_registry.go). Dropped on
	// Kill; left behind on an unclean death so the next dw-browser process can
	// finish the kill this one never got to run.
	regToken string
	// pgid is read once, while the leader is still alive. Reading it later is
	// too late: after the browser process exits, getpgid(pid) fails and the
	// surviving zygote/renderer children become unreachable by group.
	pgid int
}

func (h *chromeHandleImpl) WSURL() string         { return h.wsURL }
func (h *chromeHandleImpl) PID() int              { return h.pid }
func (h *chromeHandleImpl) Done() <-chan struct{} { return h.doneCh }

// Wait blocks until the Chrome process exits and returns cmd.Wait's error.
func (h *chromeHandleImpl) Wait() error {
	<-h.doneCh
	return h.waitErr
}

// Kill SIGKILLs the Chrome process *group* and waits for exit. Idempotent.
//
// Group, not pid: cmd.Process.Kill() only ever reached the browser process, and
// Chrome's zygote/renderer/gpu-process/utility/crashpad_handler children go on
// running (and writing into the profile dir) after it. They inherit the pgid
// set at fork by ApplyOwnedChromeProcAttr / ApplyDetachedProcAttr, so
// kill(-pgid) is the one signal that reaches all of them.
func (h *chromeHandleImpl) Kill() error {
	var killErr error
	h.killOnce.Do(func() {
		if h.cmd != nil && h.cmd.Process != nil {
			killErr = h.cmd.Process.Kill()
			KillChromeProcessGroupID(h.pgid)
		}
		UnregisterChromeProcess(h.regToken)
	})
	select {
	case <-h.doneCh:
	case <-time.After(5 * time.Second):
		if killErr != nil {
			return fmt.Errorf("workspace: kill chrome pid=%d timed out waiting for exit: %w", h.pid, killErr)
		}
		return fmt.Errorf("workspace: kill chrome pid=%d timed out waiting for exit", h.pid)
	}
	return killErr
}

// startChromeProcess forks Chrome as a detached process, waits for CDP ready,
// and returns a chromeHandleImpl with WSURL populated.
//
// Detached Chrome is correct for legacy CLI open semantics where dw-browser
// exits after creating a persistent session. Long-lived headed runtimes should
// use startChromeProcessOwned from BrowserMuxHost so Chrome cannot outlive the
// display owner.
func startChromeProcess(spec ChromeLaunchSpec) (*chromeHandleImpl, error) {
	return startChromeProcessWithOwnership(spec, true)
}

// startChromeProcessOwned forks Chrome as a BrowserMuxHost-owned child. Chrome
// does not detach from the host process; BrowserMuxHost is responsible for killing
// Chrome before releasing the display server.
func startChromeProcessOwned(spec ChromeLaunchSpec) (*chromeHandleImpl, error) {
	return startChromeProcessWithOwnership(spec, false)
}

// startChromeProcessWithOwnership starts Chrome and waits for CDP readiness.
// On any failure (start error or CDP timeout), the partial process is killed
// before return — no leak.
func startChromeProcessWithOwnership(spec ChromeLaunchSpec, detached bool) (*chromeHandleImpl, error) {
	totalStartedAt := time.Now()
	if spec.ChromePath == "" {
		return nil, fmt.Errorf("workspace: empty chrome path")
	}
	if spec.DebugPort <= 0 {
		return nil, fmt.Errorf("workspace: invalid debug port %d", spec.DebugPort)
	}
	timeout := spec.ReadyTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	EnsureChromeRegistryReaped()
	cmd := exec.Command(spec.ChromePath, spec.Args...)
	if detached {
		// Detached process group — Chrome survives parent exit (dw-browser CLI
		// scenario), and signals to the parent's pgrp don't fan out to Chrome.
		// Windows: no-op (Setpgid not available; CREATE_NEW_PROCESS_GROUP via
		// SysProcAttr is a future TODO if needed).
		ApplyDetachedProcAttr(cmd)
	} else {
		// Owned Chrome must die with its owner. Previously this branch set no
		// SysProcAttr at all, which left the fork in *our* process group with
		// no death signal: the only thing that ever killed it was h.Kill()
		// running, and a SIGKILLed owner never runs anything. Now it gets its
		// own process group (so Kill can group-kill) plus Pdeathsig where the
		// kernel honours it.
		ApplyOwnedChromeProcAttr(cmd)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	log.Printf("[CHROME-LAUNCH] start path=%q debug_port=%d args=%d detached=%t ready_timeout_ms=%d",
		spec.ChromePath, spec.DebugPort, len(spec.Args), detached, timeout.Milliseconds())
	forkStartedAt := time.Now()
	if err := cmd.Start(); err != nil {
		log.Printf("[CHROME-LAUNCH] start_failed debug_port=%d detached=%t fork_elapsed_ms=%d err=%v",
			spec.DebugPort, detached, time.Since(forkStartedAt).Milliseconds(), err)
		return nil, fmt.Errorf("workspace: start chrome: %w", err)
	}
	log.Printf("[CHROME-LAUNCH] process_started pid=%d debug_port=%d detached=%t fork_elapsed_ms=%d",
		cmd.Process.Pid, spec.DebugPort, detached, time.Since(forkStartedAt).Milliseconds())

	h := &chromeHandleImpl{
		cmd:    cmd,
		pid:    cmd.Process.Pid,
		pgid:   ChromeProcessGroupID(cmd.Process.Pid),
		doneCh: make(chan struct{}),
	}
	// Register before waiting for CDP readiness — a launch that times out is
	// precisely the case where nobody is left holding this pid.
	//
	// ChromeOwnedByCore for *both* ownership modes, deliberately. `detached`
	// here means "own process group so parent signals don't fan out", not "may
	// outlive this process": every caller of startChromeProcess (NewBrowserCore
	// for act/eval/observe/once/journey) closes it before returning, so if the
	// caller is SIGKILLed that Chrome is abandoned and must be reaped. The one
	// genuinely-persistent Chrome, `dw-browser open`, is forked by
	// startDetachedChrome in cmd/dw-browser and registers ChromeDetachedSession
	// itself. A live session file protects a record regardless of its kind, so
	// a hand-off that does happen is still safe.
	h.regToken, _ = RegisterChromeProcess(ChromeOwnedByCore, h.pid, ChromeProfileDirFromArgs(spec.Args), false)

	// Reap the child + signal exit. Single goroutine owns cmd.Wait().
	go func() {
		h.waitErr = cmd.Wait()
		log.Printf("[CHROME-LAUNCH] process_exited pid=%d debug_port=%d err=%v",
			h.pid, spec.DebugPort, h.waitErr)
		// The browser process is gone; its children are not. Same reason Kill
		// group-kills — a crashed Chrome leaves the same residue as a killed
		// one, and residue keeps writing into the profile dir.
		KillChromeProcessGroupID(h.pgid)
		UnregisterChromeProcess(h.regToken)
		close(h.doneCh)
	}()

	readyStartedAt := time.Now()
	wsURL, err := WaitForChromeReady(spec.DebugPort, timeout)
	if err != nil {
		_ = h.Kill()
		stderr := stderrBuf.String()
		if len(stderr) > 200 {
			stderr = stderr[:200]
		}
		log.Printf("[CHROME-LAUNCH] ready_failed pid=%d debug_port=%d ready_elapsed_ms=%d total_elapsed_ms=%d err=%v",
			h.pid, spec.DebugPort, time.Since(readyStartedAt).Milliseconds(), time.Since(totalStartedAt).Milliseconds(), err)
		return nil, fmt.Errorf("workspace: CDP not ready on port %d: %w (chrome stderr: %s)", spec.DebugPort, err, stderr)
	}
	h.wsURL = wsURL
	log.Printf("[CHROME-LAUNCH] ready pid=%d debug_port=%d ready_elapsed_ms=%d total_elapsed_ms=%d",
		h.pid, spec.DebugPort, time.Since(readyStartedAt).Milliseconds(), time.Since(totalStartedAt).Milliseconds())
	return h, nil
}
