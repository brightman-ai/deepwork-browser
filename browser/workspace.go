// Package browser — Workspace: Cross-Platform Isolated Workspace abstraction.
//
// Phase 0 (terminal architecture v7) — refined 2026-04-19 (TH-0419):
// Workspace is the SSOT for **visible** Chrome launches. Headless Chrome
// callers do NOT go through Workspace (no NSWindow → no Space binding to
// manage). Mode-conditional split lives at every caller.
//
// Strategy (DDC-I-21, BRR-12, TH-0418-c9x PoC verified 2026-04-19):
//   - macOS: own-process SLSManagedDisplaySetCurrentSpace switch (SIP-safe).
//     The cross-process SLSMoveWindowsToManagedSpace is silent no-op under SIP;
//     instead we switch view → fork Chrome (it inherits current Space) → wait
//     window committed → switch back.
//   - Linux: Xvfb already isolates; LaunchChromeInSpace just exec's Chrome
//     (Chrome inherits DISPLAY=:99 from parent process env).
//   - Windows: stub — direct exec (TODO: IVirtualDesktopManager COM bridge).
//
// Lifecycle: ChromeHandle owns the spawned process. Caller attaches via
// chromedp.NewRemoteAllocator(ctx, h.WSURL()). Caller MUST call h.Kill() (or
// rely on process-exit) for explicit teardown — chromedp.Cancel only closes
// the CDP session, not the process (RemoteAllocator does not own the fork).
//
// [Ref: DDC-I-11, DDC-I-21, BRR-12, BRR-MODE-1]
package browser

import "time"

// ChromeLaunchSpec — input to Workspace.LaunchChromeInSpace.
type ChromeLaunchSpec struct {
	ChromePath   string        // absolute path to chrome binary
	Args         []string      // full Chrome args (must include --remote-debugging-port=N)
	DebugPort    int           // must match --remote-debugging-port in Args
	ReadyTimeout time.Duration // CDP ready timeout (default 30s if zero)
}

// ChromeHandle owns a Chrome process spawned via Workspace.LaunchChromeInSpace.
//
// Invariants:
//   - WSURL() is non-empty when LaunchChromeInSpace returns nil error.
//   - PID() is the OS pid of the spawned chrome process.
//   - Done() is closed when the process exits (any cause).
//   - Kill() is idempotent and waits for process exit before returning.
type ChromeHandle interface {
	WSURL() string         // CDP WebSocket URL (ready when LaunchChromeInSpace returns)
	PID() int              // Chrome process PID
	Kill() error           // SIGKILL the process and wait for exit (idempotent)
	Done() <-chan struct{} // closed when process exits (any cause)
	Wait() error           // block until process exits, return cmd.Wait error
}

// Workspace manages an isolated OS workspace (Space/VDesktop/Xvfb) for
// **visible** Chrome instances. Headless Chrome does not need this and
// MUST NOT go through this interface.
type Workspace interface {
	// EnsureSpace ensures an isolated workspace exists and returns its ID.
	// On macOS: returns a non-current Space ID (Human must have ≥2 Spaces).
	// On Linux/Windows: returns (0, nil) — no Space concept needed.
	// Idempotent.
	EnsureSpace() (int64, error)

	// LaunchChromeInSpace forks Chrome inside the isolated workspace,
	// waits for CDP ready (with postcondition window-bound check on macOS),
	// and returns a ChromeHandle owning the process.
	//
	// On error, no process is leaked (the partial fork is killed before return).
	LaunchChromeInSpace(spec ChromeLaunchSpec) (ChromeHandle, error)

	// Close releases workspace resources (does NOT destroy user Spaces and
	// does NOT kill outstanding ChromeHandles — caller owns those).
	Close() error
}

// NoopWorkspace is a no-op for tests / unsupported platforms.
// LaunchChromeInSpace falls back to a direct fork (no Space switching).
type NoopWorkspace struct{}

func (n *NoopWorkspace) EnsureSpace() (int64, error) { return 0, nil }
func (n *NoopWorkspace) LaunchChromeInSpace(spec ChromeLaunchSpec) (ChromeHandle, error) {
	return startChromeProcess(spec)
}
func (n *NoopWorkspace) Close() error { return nil }
