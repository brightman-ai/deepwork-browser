package browser

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
)

const (
	browserMuxHostDirName        = "dw-browser-mux-hosts"
	browserMuxHostTokenHeader    = "X-DW-Browser-MuxHost-Token"
	browserMuxHostBinaryEnv      = "DW_BROWSER_MUXHOST_BIN"
	browserMuxHostBinaryCacheEnv = "DW_BROWSER_MUXHOST_BIN_CACHE_DIR"
	browserMuxHostIdleTTLMillis  = int64(BrowserMuxHostDefaultIdleTTL / time.Millisecond)
)

// BrowserMuxHostRequest describes the runtime that should own Chrome and its
// display server. It is intentionally free of Deepwork product vocabulary so
// dw-browser can remain a general AOT/HTR runtime.
type BrowserMuxHostRequest struct {
	BrowserSessionID string
	SessionKind      BrowserSessionKind
	MuxHostID        string
	RuntimeID        string
	MuxHostBinary    string
	IdentityKey      IdentityKey
	OwnerPID         int
	Goal             string
	Owner            string
	Isolation        string
	ServiceName      string
	AccountID        string

	ChromePath string
	ProfileID  string
	ProfileDir string
	DebugPort  int
	Mode       BrowserMode
	PresetID   string
	PersonaID  string
	Width      int
	Height     int
	UserAgent  string
	Touch      bool
	IdleTTL    time.Duration
}

// BrowserMuxHostState is the on-disk manifest and loopback API response for a
// BrowserMuxHost. It is the attach contract used by Deepwork, dw-browser CLI, and
// service adapters.
type BrowserMuxHostState struct {
	MuxHostID             string                  `json:"browser_mux_host_id"`
	MuxHostPID            int                     `json:"browser_mux_host_pid"`
	ControlURL            string                  `json:"control_url"`
	Token                 string                  `json:"token,omitempty"`
	RuntimeID             string                  `json:"runtime_id,omitempty"`
	RuntimeCount          int                     `json:"runtime_count,omitempty"`
	Runtimes              []BrowserRuntimeSummary `json:"runtimes,omitempty"`
	BrowserSessionID      string                  `json:"browser_session_id"`
	SessionKind           BrowserSessionKind      `json:"session_kind,omitempty"`
	Goal                  string                  `json:"goal,omitempty"`
	Owner                 string                  `json:"owner,omitempty"`
	OwnerPID              int                     `json:"owner_pid,omitempty"`
	Isolation             string                  `json:"isolation,omitempty"`
	ServiceName           string                  `json:"service,omitempty"`
	AccountID             string                  `json:"account_id,omitempty"`
	ProfileID             string                  `json:"profile_id"`
	ProfileDir            string                  `json:"profile_dir"`
	BrowserPID            int                     `json:"browser_pid,omitempty"`
	ChromePID             int                     `json:"chrome_pid"`
	WSURL                 string                  `json:"ws_url"`
	DebugPort             int                     `json:"debug_port"`
	Mode                  BrowserMode             `json:"mode"`
	PresetID              string                  `json:"preset_id,omitempty"`
	PersonaID             string                  `json:"persona_id,omitempty"`
	ViewportW             int                     `json:"viewport_w"`
	ViewportH             int                     `json:"viewport_h"`
	UserAgent             string                  `json:"user_agent,omitempty"`
	Touch                 bool                    `json:"touch,omitempty"`
	BrowserRunID          string                  `json:"browser_run_id"`
	DisplayBackend        string                  `json:"display_backend"`
	DisplayID             uint32                  `json:"display_id,omitempty"`
	DisplayVerified       bool                    `json:"display_verified"`
	ChromeWindowContained bool                    `json:"chrome_window_contained"`
	StartedAt             string                  `json:"started_at"`
	LastTouchedAt         string                  `json:"last_touched_at"`
	IdleTTLMillis         int64                   `json:"idle_ttl_ms"`
	MuxHostAlive          bool                    `json:"browser_mux_host_alive"`
	ChromeAlive           bool                    `json:"chrome_alive"`
	ReusedExisting        bool                    `json:"-"`
}

type BrowserRuntimeSummary struct {
	RuntimeID        string             `json:"runtime_id"`
	BrowserSessionID string             `json:"browser_session_id"`
	SessionKind      BrowserSessionKind `json:"session_kind,omitempty"`
	ServiceName      string             `json:"service,omitempty"`
	AccountID        string             `json:"account_id,omitempty"`
	ProfileID        string             `json:"profile_id,omitempty"`
	BrowserPID       int                `json:"browser_pid,omitempty"`
	ChromePID        int                `json:"chrome_pid,omitempty"`
	ChromeAlive      bool               `json:"chrome_alive"`
	DisplayBackend   string             `json:"display_backend,omitempty"`
	DisplayID        uint32             `json:"display_id,omitempty"`
}

type browserMuxHostTouchRequest struct {
	OwnerPID int `json:"owner_pid"`
}

type browserMuxHostServer struct {
	mu            sync.Mutex
	launchMu      sync.Mutex
	displayMu     sync.Mutex
	hostID        string
	controlURL    string
	token         string
	ownerPID      int
	idleTTL       time.Duration
	startedAt     string
	lastTouchedAt string
	runtimes      map[string]*browserMuxHostRuntime
	displayMgr    *DisplayManager
	virtualDisp   *VirtualDisplayManager
	shutdownCh    chan struct{}
	shutdown      sync.Once
}

type browserMuxHostRuntime struct {
	mu          sync.Mutex
	state       BrowserMuxHostState
	handle      ChromeHandle
	displayMgr  *DisplayManager
	virtualDisp *VirtualDisplayManager
	ownsDisplay bool
	profileDir  string
	identityKey IdentityKey
}

func BrowserMuxHostRootDir() string {
	return filepath.Join(os.TempDir(), browserMuxHostDirName)
}

func BrowserMuxHostRuntimeDir(hostID string) string {
	return filepath.Join(BrowserMuxHostRootDir(), sanitizeBrowserRuntimeID(hostID))
}

func BrowserMuxHostManifestPath(hostID string) string {
	return filepath.Join(BrowserMuxHostRuntimeDir(hostID), "muxhost.json")
}

func BrowserRuntimeManifestPath(runtimeID string) string {
	return BrowserMuxHostManifestPath(runtimeID)
}

func BrowserMuxHostLogPath(hostID string) string {
	return filepath.Join(BrowserMuxHostRuntimeDir(hostID), "muxhost.log")
}

func LoadBrowserMuxHostState(hostID string) (*BrowserMuxHostState, error) {
	path := BrowserMuxHostManifestPath(hostID)
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state BrowserMuxHostState
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, err
	}
	normalizeBrowserMuxHostState(&state)
	return &state, nil
}

func LoadBrowserRuntimeState(runtimeID string) (*BrowserMuxHostState, error) {
	return LoadBrowserMuxHostState(runtimeID)
}

func EnsureBrowserMuxHost(ctx context.Context, req BrowserMuxHostRequest) (*BrowserMuxHostState, error) {
	totalStartedAt := time.Now()
	req = normalizeBrowserMuxHostRequest(req)
	var err error
	req.PresetID, err = ValidatePresetID(req.PresetID)
	if err != nil {
		return nil, err
	}
	if req.ProfileDir == "" {
		return nil, fmt.Errorf("browser_mux_host: profile_dir is required")
	}
	if req.ChromePath == "" {
		chromePath, err := NewChromeLauncher().FindChrome()
		if err != nil {
			return nil, err
		}
		req.ChromePath = chromePath
	}

	attachStartedAt := time.Now()
	if state, ok, attachErr := tryAttachBrowserMuxHost(ctx, req); ok {
		log.Printf("[BROWSER-MUX-HOST] ensure_step step=attach_existing muxhost_id=%s runtime_id=%s elapsed_ms=%d",
			req.MuxHostID, req.RuntimeID, time.Since(attachStartedAt).Milliseconds())
		return state, nil
	} else if attachErr != nil {
		return nil, attachErr
	}
	log.Printf("[BROWSER-MUX-HOST] ensure_step step=attach_existing_miss muxhost_id=%s runtime_id=%s elapsed_ms=%d",
		req.MuxHostID, req.RuntimeID, time.Since(attachStartedAt).Milliseconds())

	hostDir := BrowserMuxHostRuntimeDir(req.MuxHostID)
	hostLogPath := BrowserMuxHostLogPath(req.MuxHostID)
	if err := os.MkdirAll(hostDir, 0755); err != nil {
		return nil, fmt.Errorf("browser_mux_host: mkdir muxhost dir: %w", err)
	}

	resolveStartedAt := time.Now()
	bin, err := resolveBrowserMuxHostBinary(req.MuxHostBinary)
	if err != nil {
		return nil, err
	}
	log.Printf("[BROWSER-MUX-HOST] ensure_step step=resolve_binary muxhost_id=%s runtime_id=%s bin=%q elapsed_ms=%d",
		req.MuxHostID, req.RuntimeID, bin, time.Since(resolveStartedAt).Milliseconds())
	args := browserMuxHostServeArgs(req)
	cmd := exec.Command(bin, args...)
	ApplyDetachedProcAttr(cmd)
	logFile, logErr := os.OpenFile(hostLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if logErr == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	startProcessAt := time.Now()
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, fmt.Errorf("browser_mux_host: start muxhost process: %w", err)
	}
	exitCh := make(chan error, 1)
	go func() { exitCh <- cmd.Wait() }()
	if logFile != nil {
		_ = logFile.Close()
	}
	log.Printf("[BROWSER-MUX-HOST] AUDIT: muxhost_process_started muxhost_id=%s pid=%d browser_session_id=%s profile=%s fork_elapsed_ms=%d",
		req.MuxHostID, cmd.Process.Pid, req.BrowserSessionID, req.ProfileID, time.Since(startProcessAt).Milliseconds())

	startedAt := time.Now()
	deadline := time.Now().Add(BrowserMuxHostReadyTimeout)
	var lastErr error
	polls := 0
	for time.Now().Before(deadline) {
		polls++
		state, err := LoadBrowserRuntimeState(req.RuntimeID)
		if err == nil {
			if health, healthErr := BrowserMuxHostHealth(ctx, state); healthErr == nil {
				if validateErr := validateReusableBrowserMuxHost(health, req); validateErr != nil {
					lastErr = validateErr
				} else {
					touched, touchErr := TouchBrowserMuxHost(ctx, health, req.OwnerPID)
					if touchErr == nil {
						health = touched
					}
					log.Printf("[BROWSER-MUX-HOST] ensure_step step=ready_from_runtime_manifest muxhost_id=%s runtime_id=%s polls=%d wait_elapsed_ms=%d total_elapsed_ms=%d",
						req.MuxHostID, req.RuntimeID, polls, time.Since(startedAt).Milliseconds(), time.Since(totalStartedAt).Milliseconds())
					return health, nil
				}
			} else {
				lastErr = healthErr
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			lastErr = err
		}
		if hostState, hostErr := LoadBrowserMuxHostState(req.MuxHostID); hostErr == nil && hostState.MuxHostPID > 0 {
			if ensured, ensureErr := ensureBrowserRuntimeOnMuxHost(ctx, hostState, req); ensureErr == nil {
				if validateErr := validateReusableBrowserMuxHost(ensured, req); validateErr != nil {
					lastErr = validateErr
				} else {
					log.Printf("[BROWSER-MUX-HOST] ensure_step step=ready_from_host_api muxhost_id=%s runtime_id=%s polls=%d wait_elapsed_ms=%d total_elapsed_ms=%d",
						req.MuxHostID, req.RuntimeID, polls, time.Since(startedAt).Milliseconds(), time.Since(totalStartedAt).Milliseconds())
					return ensured, nil
				}
			} else {
				lastErr = ensureErr
			}
		}
		select {
		case err := <-exitCh:
			return nil, fmt.Errorf("browser_mux_host: muxhost %s exited before ready after %s: %v (log=%s; last_ready_err=%v)",
				req.MuxHostID, time.Since(startedAt).Round(time.Millisecond), err, hostLogPath, lastErr)
		default:
		}
		time.Sleep(BrowserMuxHostReadyPollInterval)
	}
	return nil, fmt.Errorf("browser_mux_host: muxhost %s not ready after %s (log=%s; last_ready_err=%v)",
		req.MuxHostID, BrowserMuxHostReadyTimeout, hostLogPath, lastErr)
}

func ServeBrowserMuxHost(ctx context.Context, req BrowserMuxHostRequest) error {
	req = normalizeBrowserMuxHostRequest(req)
	var err error
	req.PresetID, err = ValidatePresetID(req.PresetID)
	if err != nil {
		return err
	}
	if req.ChromePath == "" {
		chromePath, err := NewChromeLauncher().FindChrome()
		if err != nil {
			return err
		}
		req.ChromePath = chromePath
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("browser_mux_host: listen control: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	muxHost := &browserMuxHostServer{
		hostID:        req.MuxHostID,
		controlURL:    "http://" + listener.Addr().String(),
		token:         newBrowserMuxHostToken(),
		ownerPID:      req.OwnerPID,
		idleTTL:       req.IdleTTL,
		startedAt:     now,
		lastTouchedAt: now,
		runtimes:      make(map[string]*browserMuxHostRuntime),
		shutdownCh:    make(chan struct{}),
	}
	muxHost.mu.Lock()
	if err := muxHost.writeHostManifestLocked(); err != nil {
		muxHost.mu.Unlock()
		_ = listener.Close()
		return fmt.Errorf("browser_mux_host: write host manifest: %w", err)
	}
	muxHost.mu.Unlock()

	server := &http.Server{Handler: muxHost.httpHandler()}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[BROWSER-MUX-HOST] control server failed muxhost_id=%s err=%v", req.MuxHostID, err)
			muxHost.requestShutdown()
		}
	}()

	state, err := muxHost.ensureRuntime(ctx, req)
	if err != nil {
		muxHost.requestShutdown()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), BrowserMuxHostShutdownTimeout)
		_ = server.Shutdown(shutdownCtx)
		cancel()
		muxHost.cleanupAll()
		return err
	}
	log.Printf("[BROWSER-MUX-HOST] AUDIT: muxhost_ready muxhost_id=%s runtime_id=%s pid=%d chrome_pid=%d cdp_port=%d display=%s display_id=%d owner_pid=%d idle_ttl_ms=%d",
		req.MuxHostID, state.RuntimeID, os.Getpid(), state.ChromePID, state.DebugPort, state.DisplayBackend, state.DisplayID, req.OwnerPID, state.IdleTTLMillis)

	err = muxHost.wait(ctx)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), BrowserMuxHostShutdownTimeout)
	_ = server.Shutdown(shutdownCtx)
	cancel()
	muxHost.cleanupAll()
	return err
}

func (s *browserMuxHostServer) ensureRuntime(ctx context.Context, req BrowserMuxHostRequest) (*BrowserMuxHostState, error) {
	req = normalizeBrowserMuxHostRequest(req)
	if req.MuxHostID != s.hostID {
		req.MuxHostID = s.hostID
	}
	var err error
	req.PresetID, err = ValidatePresetID(req.PresetID)
	if err != nil {
		return nil, err
	}
	if req.ProfileDir == "" {
		return nil, fmt.Errorf("browser_mux_host: profile_dir is required")
	}
	if req.ChromePath == "" {
		chromePath, err := NewChromeLauncher().FindChrome()
		if err != nil {
			return nil, err
		}
		req.ChromePath = chromePath
	}

	s.mu.Lock()
	if existing := s.runtimes[req.RuntimeID]; existing != nil {
		state := existing.snapshot()
		if err := validateReusableBrowserMuxHost(&state, req); err != nil {
			if !browserMuxHostRuntimeReusableFailureRecoverable(err) {
				s.mu.Unlock()
				return nil, err
			}
			delete(s.runtimes, req.RuntimeID)
			_ = s.writeHostManifestLocked()
			s.mu.Unlock()
			log.Printf("[BROWSER-MUX-HOST] AUDIT: runtime_recreate_after_unhealthy_reuse muxhost_id=%s runtime_id=%s chrome_pid=%d err=%v",
				s.hostID, state.RuntimeID, state.ChromePID, err)
			existing.cleanup()
		} else {
			existing.touch(req.OwnerPID)
			s.ownerPID = req.OwnerPID
			s.lastTouchedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = s.writeHostManifestLocked()
			s.mu.Unlock()
			state.ReusedExisting = true
			log.Printf("[BROWSER-MUX-HOST] AUDIT: runtime_reused muxhost_id=%s runtime_id=%s chrome_pid=%d owner_pid=%d",
				s.hostID, state.RuntimeID, state.ChromePID, req.OwnerPID)
			return &state, nil
		}
	} else {
		s.mu.Unlock()
	}

	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	s.mu.Lock()
	if existing := s.runtimes[req.RuntimeID]; existing != nil {
		state := existing.snapshot()
		if err := validateReusableBrowserMuxHost(&state, req); err != nil {
			if !browserMuxHostRuntimeReusableFailureRecoverable(err) {
				s.mu.Unlock()
				return nil, err
			}
			delete(s.runtimes, req.RuntimeID)
			_ = s.writeHostManifestLocked()
			s.mu.Unlock()
			log.Printf("[BROWSER-MUX-HOST] AUDIT: runtime_recreate_after_unhealthy_reuse muxhost_id=%s runtime_id=%s chrome_pid=%d err=%v",
				s.hostID, state.RuntimeID, state.ChromePID, err)
			existing.cleanup()
		} else {
			existing.touch(req.OwnerPID)
			s.ownerPID = req.OwnerPID
			s.lastTouchedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = s.writeHostManifestLocked()
			s.mu.Unlock()
			state.ReusedExisting = true
			return &state, nil
		}
	} else {
		s.mu.Unlock()
	}

	rt, err := s.startRuntime(ctx, req)
	if err != nil {
		return nil, err
	}
	state := rt.snapshot()
	s.mu.Lock()
	if existing := s.runtimes[req.RuntimeID]; existing != nil {
		s.mu.Unlock()
		rt.cleanup()
		existingState := existing.snapshot()
		if err := validateReusableBrowserMuxHost(&existingState, req); err != nil {
			return nil, err
		}
		existing.touch(req.OwnerPID)
		existingState.ReusedExisting = true
		return &existingState, nil
	}
	s.runtimes[req.RuntimeID] = rt
	s.ownerPID = req.OwnerPID
	s.lastTouchedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := s.writeHostManifestLocked(); err != nil {
		delete(s.runtimes, req.RuntimeID)
		s.mu.Unlock()
		rt.cleanup()
		return nil, fmt.Errorf("browser_mux_host: write host manifest: %w", err)
	}
	s.mu.Unlock()
	log.Printf("[BROWSER-MUX-HOST] AUDIT: runtime_ready muxhost_id=%s runtime_id=%s browser_session_id=%s chrome_pid=%d cdp_port=%d display=%s display_id=%d",
		s.hostID, state.RuntimeID, state.BrowserSessionID, state.ChromePID, state.DebugPort, state.DisplayBackend, state.DisplayID)
	return &state, nil
}

func (s *browserMuxHostServer) startRuntime(ctx context.Context, req BrowserMuxHostRequest) (*browserMuxHostRuntime, error) {
	_ = ctx
	totalStartedAt := time.Now()
	log.Printf("[BROWSER-MUX-HOST] runtime_start muxhost_id=%s runtime_id=%s mode=%s profile_id=%s",
		req.MuxHostID, req.RuntimeID, req.Mode, req.ProfileID)
	recoveryStartedAt := time.Now()
	if err := RunStartupRecovery(req.ProfileDir, browserMuxHostIdentityKey(req)); err != nil {
		return nil, fmt.Errorf("browser_mux_host: startup recovery: %w", err)
	}
	log.Printf("[BROWSER-MUX-HOST] runtime_start_step step=startup_recovery muxhost_id=%s runtime_id=%s elapsed_ms=%d",
		req.MuxHostID, req.RuntimeID, time.Since(recoveryStartedAt).Milliseconds())
	profileStartedAt := time.Now()
	if err := os.MkdirAll(req.ProfileDir, 0755); err != nil {
		return nil, fmt.Errorf("browser_mux_host: mkdir profile: %w", err)
	}
	if err := PrepareProfileForControlledLaunch(req.ProfileDir); err != nil {
		log.Printf("[BROWSER-MUX-HOST] profile launch hygiene failed muxhost_id=%s runtime_id=%s profile=%s err=%v",
			req.MuxHostID, req.RuntimeID, req.ProfileID, err)
	}
	log.Printf("[BROWSER-MUX-HOST] runtime_start_step step=profile_prepare muxhost_id=%s runtime_id=%s elapsed_ms=%d",
		req.MuxHostID, req.RuntimeID, time.Since(profileStartedAt).Milliseconds())

	displayStartedAt := time.Now()
	displayBackend, displayID, posX, posY, virtualDisp, displayMgr, ownsDisplay, err := s.ensureRuntimeDisplay(req.Mode)
	if err != nil {
		return nil, err
	}
	log.Printf("[BROWSER-MUX-HOST] runtime_start_step step=ensure_display muxhost_id=%s runtime_id=%s backend=%s display_id=%d pos=(%d,%d) elapsed_ms=%d",
		req.MuxHostID, req.RuntimeID, displayBackend, displayID, posX, posY, time.Since(displayStartedAt).Milliseconds())
	port := req.DebugPort
	if port <= 0 {
		portStartedAt := time.Now()
		port, err = FindFreePort()
		if err != nil {
			if ownsDisplay {
				cleanupBrowserMuxHostDisplay(virtualDisp, displayMgr)
			}
			return nil, fmt.Errorf("browser_mux_host: allocate CDP port: %w", err)
		}
		log.Printf("[BROWSER-MUX-HOST] runtime_start_step step=allocate_cdp_port muxhost_id=%s runtime_id=%s debug_port=%d elapsed_ms=%d",
			req.MuxHostID, req.RuntimeID, port, time.Since(portStartedAt).Milliseconds())
	}
	argsStartedAt := time.Now()
	launchArgs := BuildDetachedChromeArgs(DetachedChromeLaunchOptions{
		DebugPort:  port,
		ProfileDir: req.ProfileDir,
		Width:      req.Width,
		Height:     req.Height,
		PresetID:   req.PresetID,
		UserAgent:  req.UserAgent,
		Touch:      req.Touch,
		Mode:       req.Mode,
	})
	if req.Mode == ModeHeaded && runtime.GOOS == "darwin" {
		launchArgs = appendChromeArgBeforeURL(launchArgs, fmt.Sprintf("--window-position=%d,%d", posX, posY))
	}
	if proxy, source := resolveBrowserPoolProxy(); proxy != "" {
		launchArgs = appendChromeArgBeforeURL(launchArgs, "--proxy-server="+proxy)
		log.Printf("[BROWSER-MUX-HOST] proxy-server=%s source=%s muxhost_id=%s runtime_id=%s", proxy, source, req.MuxHostID, req.RuntimeID)
	}
	log.Printf("[BROWSER-MUX-HOST] runtime_start_step step=build_chrome_args muxhost_id=%s runtime_id=%s args=%d elapsed_ms=%d",
		req.MuxHostID, req.RuntimeID, len(launchArgs), time.Since(argsStartedAt).Milliseconds())

	chromeStartedAt := time.Now()
	handle, err := startChromeProcessOwned(ChromeLaunchSpec{
		ChromePath:   req.ChromePath,
		Args:         launchArgs,
		DebugPort:    port,
		ReadyTimeout: BrowserMuxHostLaunchReadyTimeout,
	})
	if err != nil {
		if ownsDisplay {
			cleanupBrowserMuxHostDisplay(virtualDisp, displayMgr)
		}
		return nil, fmt.Errorf("browser_mux_host: launch chrome: %w", err)
	}
	log.Printf("[BROWSER-MUX-HOST] runtime_start_step step=launch_chrome muxhost_id=%s runtime_id=%s chrome_pid=%d debug_port=%d elapsed_ms=%d",
		req.MuxHostID, req.RuntimeID, handle.PID(), port, time.Since(chromeStartedAt).Milliseconds())
	displayVerified := displayBackend == "none" || displayBackend == "xvfb" || displayBackend == "native"
	windowContained := displayVerified
	if req.Mode == ModeHeaded && runtime.GOOS == "darwin" && virtualDisp != nil {
		containmentStartedAt := time.Now()
		containmentErr := virtualDisp.VerifyChromeContained(handle.PID(), BrowserMuxHostWindowContainmentTimeout)
		log.Printf("[BROWSER-MUX-HOST] runtime_start_step step=verify_containment phase=launch_flags muxhost_id=%s runtime_id=%s chrome_pid=%d elapsed_ms=%d err=%v",
			req.MuxHostID, req.RuntimeID, handle.PID(), time.Since(containmentStartedAt).Milliseconds(), containmentErr)
		if containmentErr != nil {
			enforceStartedAt := time.Now()
			if err := enforceBrowserMuxHostWindow(handle.WSURL(), req.Width, req.Height, posX, posY); err != nil {
				_ = handle.Kill()
				if ownsDisplay {
					cleanupBrowserMuxHostDisplay(virtualDisp, displayMgr)
				}
				return nil, err
			}
			log.Printf("[BROWSER-MUX-HOST] runtime_start_step step=enforce_window muxhost_id=%s runtime_id=%s chrome_pid=%d elapsed_ms=%d",
				req.MuxHostID, req.RuntimeID, handle.PID(), time.Since(enforceStartedAt).Milliseconds())
			containmentStartedAt = time.Now()
			if err := virtualDisp.VerifyChromeContained(handle.PID(), BrowserMuxHostWindowContainmentTimeout); err != nil {
				_ = handle.Kill()
				if ownsDisplay {
					cleanupBrowserMuxHostDisplay(virtualDisp, displayMgr)
				}
				return nil, err
			}
			log.Printf("[BROWSER-MUX-HOST] runtime_start_step step=verify_containment phase=repair muxhost_id=%s runtime_id=%s chrome_pid=%d elapsed_ms=%d",
				req.MuxHostID, req.RuntimeID, handle.PID(), time.Since(containmentStartedAt).Milliseconds())
		}
		displayVerified = true
		windowContained = true
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	state := BrowserMuxHostState{
		MuxHostID:             s.hostID,
		MuxHostPID:            os.Getpid(),
		ControlURL:            s.controlURL,
		Token:                 s.token,
		RuntimeID:             req.RuntimeID,
		BrowserSessionID:      req.BrowserSessionID,
		SessionKind:           req.SessionKind,
		Goal:                  req.Goal,
		Owner:                 req.Owner,
		OwnerPID:              req.OwnerPID,
		Isolation:             req.Isolation,
		ServiceName:           req.ServiceName,
		AccountID:             req.AccountID,
		ProfileID:             req.ProfileID,
		ProfileDir:            req.ProfileDir,
		BrowserPID:            handle.PID(),
		ChromePID:             handle.PID(),
		WSURL:                 handle.WSURL(),
		DebugPort:             port,
		Mode:                  req.Mode,
		PresetID:              req.PresetID,
		ViewportW:             req.Width,
		ViewportH:             req.Height,
		UserAgent:             req.UserAgent,
		Touch:                 req.Touch,
		BrowserRunID:          NewBrowserRunID(req.BrowserSessionID, handle.PID()),
		DisplayBackend:        displayBackend,
		DisplayID:             displayID,
		DisplayVerified:       displayVerified,
		ChromeWindowContained: windowContained,
		StartedAt:             now,
		LastTouchedAt:         now,
		IdleTTLMillis:         int64(req.IdleTTL / time.Millisecond),
		MuxHostAlive:          true,
		ChromeAlive:           true,
	}
	rt := &browserMuxHostRuntime{
		state:       state,
		handle:      handle,
		displayMgr:  displayMgr,
		virtualDisp: virtualDisp,
		ownsDisplay: ownsDisplay,
		profileDir:  req.ProfileDir,
		identityKey: browserMuxHostIdentityKey(req),
	}

	if err := WriteProfileOwnerMarkerWithMetadata(req.ProfileDir, rt.identityKey, handle.PID(), port, ProfileOwnerMetadata{
		BrowserSessionID:      req.BrowserSessionID,
		SessionKind:           req.SessionKind,
		BrowserMuxHostID:      req.MuxHostID,
		BrowserMuxHostPID:     os.Getpid(),
		RuntimeID:             req.RuntimeID,
		BrowserRunID:          state.BrowserRunID,
		ProfileID:             req.ProfileID,
		DisplayBackend:        displayBackend,
		DisplayID:             displayID,
		DisplayVerified:       displayVerified,
		ChromeWindowContained: windowContained,
	}); err != nil {
		_ = handle.Kill()
		if ownsDisplay {
			cleanupBrowserMuxHostDisplay(virtualDisp, displayMgr)
		}
		return nil, fmt.Errorf("browser_mux_host: write owner marker: %w", err)
	}
	if err := writeBrowserRuntimeManifest(&state); err != nil {
		RemoveProfileOwnerMarker(req.ProfileDir, rt.identityKey)
		_ = handle.Kill()
		if ownsDisplay {
			cleanupBrowserMuxHostDisplay(virtualDisp, displayMgr)
		}
		return nil, fmt.Errorf("browser_mux_host: write runtime manifest: %w", err)
	}
	log.Printf("[BROWSER-MUX-HOST] runtime_start_completed muxhost_id=%s runtime_id=%s chrome_pid=%d total_elapsed_ms=%d",
		req.MuxHostID, req.RuntimeID, handle.PID(), time.Since(totalStartedAt).Milliseconds())
	return rt, nil
}

func BrowserMuxHostHealth(ctx context.Context, state *BrowserMuxHostState) (*BrowserMuxHostState, error) {
	return browserMuxHostRequest(ctx, state, http.MethodGet, "/health", nil)
}

func TouchBrowserMuxHost(ctx context.Context, state *BrowserMuxHostState, ownerPID int) (*BrowserMuxHostState, error) {
	return browserMuxHostRequest(ctx, state, http.MethodPost, "/touch", browserMuxHostTouchRequest{OwnerPID: ownerPID})
}

func ReleaseBrowserMuxHost(ctx context.Context, state *BrowserMuxHostState) (*BrowserMuxHostState, error) {
	return browserMuxHostRequest(ctx, state, http.MethodPost, "/release", browserMuxHostTouchRequest{})
}

func ShutdownBrowserMuxHost(ctx context.Context, state *BrowserMuxHostState) (*BrowserMuxHostState, error) {
	return browserMuxHostRequest(ctx, state, http.MethodPost, "/shutdown", nil)
}

func NewBrowserMuxHostChromeHandle(state *BrowserMuxHostState) ChromeHandle {
	h := &browserMuxHostChromeHandle{
		state:  *state,
		doneCh: make(chan struct{}),
	}
	go h.watch()
	return h
}

type browserMuxHostChromeHandle struct {
	mu       sync.Mutex
	state    BrowserMuxHostState
	doneCh   chan struct{}
	doneOnce sync.Once
}

func (h *browserMuxHostChromeHandle) WSURL() string { return h.state.WSURL }
func (h *browserMuxHostChromeHandle) PID() int      { return h.state.ChromePID }
func (h *browserMuxHostChromeHandle) Done() <-chan struct{} {
	return h.doneCh
}
func (h *browserMuxHostChromeHandle) Wait() error {
	<-h.doneCh
	return nil
}
func (h *browserMuxHostChromeHandle) Kill() error {
	ctx, cancel := context.WithTimeout(context.Background(), BrowserMuxHostControlRequestTimeout)
	defer cancel()
	_, err := ShutdownBrowserMuxHost(ctx, &h.state)
	h.waitForDead(BrowserMuxHostControlRequestTimeout)
	h.doneOnce.Do(func() { close(h.doneCh) })
	return err
}

func (h *browserMuxHostChromeHandle) watch() {
	ticker := time.NewTicker(BrowserMuxHostHealthPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		h.mu.Lock()
		pid := h.state.ChromePID
		h.mu.Unlock()
		if pid <= 0 || !isPIDAlive(pid) {
			h.doneOnce.Do(func() { close(h.doneCh) })
			return
		}
	}
}

func (h *browserMuxHostChromeHandle) waitForDead(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.state.ChromePID <= 0 || !isPIDAlive(h.state.ChromePID) {
			return
		}
		time.Sleep(BrowserMuxHostProcessPollInterval)
	}
}

func normalizeBrowserMuxHostRequest(req BrowserMuxHostRequest) BrowserMuxHostRequest {
	req.BrowserSessionID = BrowserSessionIDFromSessionID(req.BrowserSessionID)
	req.SessionKind = NormalizeBrowserSessionKind(string(req.SessionKind), SessionKindTask)
	req.Goal = strings.TrimSpace(req.Goal)
	req.Owner = strings.TrimSpace(req.Owner)
	req.Isolation = strings.TrimSpace(req.Isolation)
	req.ServiceName = strings.TrimSpace(req.ServiceName)
	req.AccountID = strings.TrimSpace(req.AccountID)
	if req.RuntimeID == "" {
		req.RuntimeID = BrowserRuntimeIDFromBrowserSessionID(req.BrowserSessionID)
	}
	req.RuntimeID = sanitizeBrowserRuntimeID(req.RuntimeID)
	if req.MuxHostID == "" {
		req.MuxHostID = GlobalBrowserMuxHostID()
	}
	if strings.HasPrefix(req.MuxHostID, "browser-mux-host-browser-session-") {
		req.RuntimeID = strings.TrimPrefix(req.MuxHostID, "browser-mux-host-")
		req.RuntimeID = "browser-runtime-" + sanitizeBrowserRuntimeID(req.RuntimeID)
		req.MuxHostID = GlobalBrowserMuxHostID()
	}
	req.MuxHostID = sanitizeBrowserRuntimeID(req.MuxHostID)
	defaults := DefaultsForBrowserSessionKind(req.SessionKind)
	if req.Owner == "" {
		req.Owner = defaults.Owner
	}
	if req.Isolation == "" {
		req.Isolation = defaults.Isolation
	}
	req.Mode = NormalizeBrowserMode(req.Mode, defaults.Mode)
	if req.Mode == "" {
		req.Mode = defaults.Mode
	}
	req.PresetID = NormalizePresetID(req.PresetID)
	req.ProfileID = NormalizeProfileID(req.ProfileID)
	if req.Width <= 0 {
		req.Width = DefaultViewportWidth
	}
	if req.Height <= 0 {
		req.Height = DefaultViewportHeight
	}
	if req.IdleTTL <= 0 {
		req.IdleTTL = BrowserMuxHostDefaultIdleTTL
	}
	return req
}

func normalizeBrowserMuxHostState(state *BrowserMuxHostState) {
	if state == nil {
		return
	}
	state.MuxHostID = sanitizeBrowserRuntimeID(state.MuxHostID)
	state.BrowserSessionID = BrowserSessionIDFromSessionID(state.BrowserSessionID)
	if state.RuntimeID == "" && state.BrowserSessionID != "" {
		state.RuntimeID = BrowserRuntimeIDFromBrowserSessionID(state.BrowserSessionID)
	}
	if state.RuntimeID != "" {
		state.RuntimeID = sanitizeBrowserRuntimeID(state.RuntimeID)
	}
	if state.BrowserPID == 0 && state.ChromePID > 0 {
		state.BrowserPID = state.ChromePID
	}
	if state.ChromePID == 0 && state.BrowserPID > 0 {
		state.ChromePID = state.BrowserPID
	}
	isRuntimeState := state.RuntimeID != "" || state.BrowserSessionID != "" || state.BrowserPID > 0 || state.ChromePID > 0
	if isRuntimeState {
		state.SessionKind = NormalizeBrowserSessionKind(string(state.SessionKind), SessionKindTask)
	}
	state.Goal = strings.TrimSpace(state.Goal)
	state.Owner = strings.TrimSpace(state.Owner)
	state.Isolation = strings.TrimSpace(state.Isolation)
	state.ServiceName = strings.TrimSpace(state.ServiceName)
	state.AccountID = strings.TrimSpace(state.AccountID)
	if isRuntimeState {
		state.Mode = NormalizeBrowserMode(state.Mode, DefaultsForBrowserSessionKind(state.SessionKind).Mode)
	}
	if state.IdleTTLMillis <= 0 {
		state.IdleTTLMillis = browserMuxHostIdleTTLMillis
	}
	state.MuxHostAlive = state.MuxHostPID > 0 && isPIDAlive(state.MuxHostPID)
	state.ChromeAlive = state.ChromePID > 0 && isPIDAlive(state.ChromePID)
}

func tryAttachBrowserMuxHost(ctx context.Context, req BrowserMuxHostRequest) (*BrowserMuxHostState, bool, error) {
	state, err := LoadBrowserMuxHostState(req.MuxHostID)
	if err != nil {
		return nil, false, nil
	}
	if state.MuxHostPID <= 0 || !isPIDAlive(state.MuxHostPID) {
		return nil, false, nil
	}
	health, err := BrowserMuxHostHealth(ctx, state)
	if err != nil {
		log.Printf("[BROWSER-MUX-HOST] AUDIT: stale_muxhost_unresponsive muxhost_id=%s pid=%d err=%v", req.MuxHostID, state.MuxHostPID, err)
		killAndWait(state.MuxHostPID, ProfileOwnerMuxHostKillGrace)
		for _, rt := range state.Runtimes {
			if rt.ChromePID > 0 && isPIDAlive(rt.ChromePID) {
				killAndWait(rt.ChromePID, ProfileOwnerMuxHostKillGrace)
			}
		}
		return nil, false, nil
	}
	runtimeState, err := ensureBrowserRuntimeOnMuxHost(ctx, health, req)
	if err != nil {
		return nil, false, fmt.Errorf("browser_mux_host: ensure runtime %s on %s: %w", req.RuntimeID, req.MuxHostID, err)
	}
	if err := validateReusableBrowserMuxHost(runtimeState, req); err != nil {
		return nil, false, err
	}
	if runtimeState.ReusedExisting {
		log.Printf("[BROWSER-MUX-HOST] AUDIT: runtime_reused muxhost_id=%s runtime_id=%s pid=%d chrome_pid=%d owner_pid=%d",
			req.MuxHostID, runtimeState.RuntimeID, runtimeState.MuxHostPID, runtimeState.ChromePID, req.OwnerPID)
	} else {
		log.Printf("[BROWSER-MUX-HOST] AUDIT: runtime_created_on_reused_muxhost muxhost_id=%s runtime_id=%s pid=%d chrome_pid=%d owner_pid=%d",
			req.MuxHostID, runtimeState.RuntimeID, runtimeState.MuxHostPID, runtimeState.ChromePID, req.OwnerPID)
	}
	return runtimeState, true, nil
}

func ensureBrowserRuntimeOnMuxHost(ctx context.Context, hostState *BrowserMuxHostState, req BrowserMuxHostRequest) (*BrowserMuxHostState, error) {
	host := *hostState
	host.RuntimeID = ""
	host.BrowserSessionID = ""
	host.ChromePID = 0
	return browserMuxHostRequest(ctx, &host, http.MethodPost, "/runtime/ensure", req)
}

func validateReusableBrowserMuxHost(state *BrowserMuxHostState, req BrowserMuxHostRequest) error {
	if state == nil {
		return fmt.Errorf("empty muxhost state")
	}
	req = normalizeBrowserMuxHostRequest(req)
	normalizeBrowserMuxHostState(state)
	if state.BrowserSessionID != req.BrowserSessionID {
		return fmt.Errorf("browser_session_id mismatch: state=%s requested=%s", state.BrowserSessionID, req.BrowserSessionID)
	}
	if state.RuntimeID != req.RuntimeID {
		return fmt.Errorf("runtime_id mismatch: state=%s requested=%s", state.RuntimeID, req.RuntimeID)
	}
	if state.SessionKind != req.SessionKind {
		return fmt.Errorf("session_kind mismatch: state=%s requested=%s", state.SessionKind, req.SessionKind)
	}
	if state.Owner != "" && req.Owner != "" && state.Owner != req.Owner {
		return fmt.Errorf("owner mismatch: state=%s requested=%s", state.Owner, req.Owner)
	}
	if state.Isolation != "" && req.Isolation != "" && state.Isolation != req.Isolation {
		return fmt.Errorf("isolation mismatch: state=%s requested=%s", state.Isolation, req.Isolation)
	}
	if state.ServiceName != req.ServiceName {
		return fmt.Errorf("service mismatch: state=%s requested=%s", state.ServiceName, req.ServiceName)
	}
	if state.AccountID != req.AccountID {
		return fmt.Errorf("account_id mismatch: state=%s requested=%s", state.AccountID, req.AccountID)
	}
	if !state.MuxHostAlive {
		return fmt.Errorf("browser mux host process is not alive")
	}
	if !state.ChromeAlive {
		return fmt.Errorf("chrome process is not alive")
	}
	mode := NormalizeBrowserMode(req.Mode, ModeHeaded)
	if mode == ModeHeaded && state.DisplayBackend == "cgvirtualdisplay" {
		if !state.DisplayVerified || !state.ChromeWindowContained {
			return fmt.Errorf("virtual display not contained: display_verified=%t chrome_window_contained=%t",
				state.DisplayVerified, state.ChromeWindowContained)
		}
	}
	return nil
}

func browserMuxHostRuntimeReusableFailureRecoverable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "chrome process is not alive") ||
		strings.Contains(msg, "virtual display not contained")
}

func browserMuxHostIdentityKey(req BrowserMuxHostRequest) IdentityKey {
	if req.IdentityKey != "" {
		return req.IdentityKey
	}
	if req.RuntimeID != "" {
		return IdentityKey(req.RuntimeID)
	}
	return IdentityKey("browser-mux-host-" + req.MuxHostID)
}

func resolveBrowserMuxHostBinary(explicit string) (string, error) {
	exe, _ := os.Executable()
	cwd, _ := os.Getwd()
	return resolveBrowserMuxHostBinaryFrom(explicit, exe, cwd)
}

func resolveBrowserMuxHostBinaryFrom(explicit, currentExecutable, cwd string) (string, error) {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		return explicit, nil
	}
	if env := strings.TrimSpace(os.Getenv(browserMuxHostBinaryEnv)); env != "" {
		return env, nil
	}
	binaryName := browserMuxHostBinaryName()
	if currentExecutable != "" {
		base := filepath.Base(currentExecutable)
		if base == "dw-browser" || base == "dw-browser.exe" {
			if !pathLooksInsideMacAppBundle(currentExecutable) {
				return currentExecutable, nil
			}
		}
		if candidate := browserMuxHostExternalPeerBinary(currentExecutable); candidate != "" && isExecutableFile(candidate) {
			return candidate, nil
		}
	}
	if cwd != "" {
		for _, candidate := range []string{
			filepath.Join(cwd, "bin", binaryName),
			filepath.Join(cwd, binaryName),
		} {
			if isExecutableFile(candidate) && !pathLooksInsideMacAppBundle(candidate) {
				abs, _ := filepath.Abs(candidate)
				return abs, nil
			}
		}
	}
	for _, candidate := range []string{
		filepath.Join("bin", binaryName),
		binaryName,
	} {
		if isExecutableFile(candidate) && !pathLooksInsideMacAppBundle(candidate) {
			abs, _ := filepath.Abs(candidate)
			return abs, nil
		}
	}
	if currentExecutable != "" {
		for _, candidate := range []string{
			filepath.Join(filepath.Dir(currentExecutable), binaryName),
			currentExecutable,
		} {
			if !isExecutableFile(candidate) || !pathLooksInsideMacAppBundle(candidate) {
				continue
			}
			stable, err := installStableBrowserMuxHostBinary(candidate)
			if err == nil {
				log.Printf("[BROWSER-MUX-HOST] AUDIT: muxhost_binary_installed source=%s target=%s", candidate, stable)
				return stable, nil
			}
			log.Printf("[BROWSER-MUX-HOST] AUDIT: muxhost_binary_install_failed source=%s err=%v", candidate, err)
			return candidate, nil
		}
	}
	if path, err := exec.LookPath(binaryName); err == nil {
		return path, nil
	}
	return "", fmt.Errorf("browser_mux_host: cannot locate dw-browser muxhost binary; set %s", browserMuxHostBinaryEnv)
}

func browserMuxHostBinaryName() string {
	if runtime.GOOS == "windows" {
		return "dw-browser.exe"
	}
	return "dw-browser"
}

func browserMuxHostExternalPeerBinary(currentExecutable string) string {
	clean := filepath.Clean(currentExecutable)
	marker := string(os.PathSeparator) + "Contents" + string(os.PathSeparator) + "MacOS" + string(os.PathSeparator)
	idx := strings.Index(clean, marker)
	if idx < 0 {
		return ""
	}
	appPath := clean[:idx]
	return filepath.Join(filepath.Dir(appPath), browserMuxHostBinaryName())
}

func pathLooksInsideMacAppBundle(path string) bool {
	clean := filepath.Clean(path)
	marker := ".app" + string(os.PathSeparator) + "Contents" + string(os.PathSeparator) + "MacOS"
	return strings.Contains(clean, marker)
}

func stableBrowserMuxHostBinaryPath() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(browserMuxHostBinaryCacheEnv)); dir != "" {
		return filepath.Join(dir, browserMuxHostBinaryName()), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("resolve home for muxhost binary cache: %w", err)
	}
	return filepath.Join(home, ".dw-browser", "runtime", "bin", browserMuxHostBinaryName()), nil
}

func installStableBrowserMuxHostBinary(source string) (string, error) {
	source = filepath.Clean(source)
	target, err := stableBrowserMuxHostBinaryPath()
	if err != nil {
		return "", err
	}
	target = filepath.Clean(target)
	if samePath(source, target) {
		return target, nil
	}
	srcInfo, err := os.Stat(source)
	if err != nil {
		return "", fmt.Errorf("stat source: %w", err)
	}
	if srcInfo.IsDir() || srcInfo.Mode()&0111 == 0 {
		return "", fmt.Errorf("source is not executable: %s", source)
	}
	if dstInfo, err := os.Stat(target); err == nil && !dstInfo.IsDir() && dstInfo.Mode()&0111 != 0 &&
		dstInfo.Size() == srcInfo.Size() && !dstInfo.ModTime().Before(srcInfo.ModTime()) {
		return target, nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return "", fmt.Errorf("mkdir target dir: %w", err)
	}
	in, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open source: %w", err)
	}
	defer in.Close()
	tmp := fmt.Sprintf("%s.tmp-%d", target, os.Getpid())
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode().Perm()|0700)
	if err != nil {
		return "", fmt.Errorf("open temp target: %w", err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("copy binary: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("close temp target: %w", closeErr)
	}
	if err := os.Chmod(tmp, srcInfo.Mode().Perm()|0700); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("chmod temp target: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("rename temp target: %w", err)
	}
	return target, nil
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil && errB == nil {
		a, b = absA, absB
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func isExecutableFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir() && st.Mode()&0111 != 0
}

func browserMuxHostServeArgs(req BrowserMuxHostRequest) []string {
	args := []string{
		"muxhost", "serve",
		"--muxhost-id", req.MuxHostID,
		"--runtime-id", req.RuntimeID,
		"--browser-session-id", req.BrowserSessionID,
		"--kind", string(req.SessionKind),
		"--profile-id", req.ProfileID,
		"--profile-dir", req.ProfileDir,
		"--chrome-path", req.ChromePath,
		"--mode", string(req.Mode),
		"--preset", req.PresetID,
		"--persona", req.PersonaID,
		"--width", strconv.Itoa(req.Width),
		"--height", strconv.Itoa(req.Height),
		"--owner-pid", strconv.Itoa(req.OwnerPID),
		"--idle-ttl", req.IdleTTL.String(),
		"--owner", req.Owner,
		"--isolation", req.Isolation,
	}
	if req.DebugPort > 0 {
		args = append(args, "--debug-port", strconv.Itoa(req.DebugPort))
	}
	if req.UserAgent != "" {
		args = append(args, "--user-agent", req.UserAgent)
	}
	if req.Touch {
		args = append(args, "--touch")
	}
	if req.Goal != "" {
		args = append(args, "--goal", req.Goal)
	}
	if req.ServiceName != "" {
		args = append(args, "--service", req.ServiceName)
	}
	if req.AccountID != "" {
		args = append(args, "--account", req.AccountID)
	}
	if req.IdentityKey != "" {
		args = append(args, "--identity-key", string(req.IdentityKey))
	}
	return args
}

func (s *browserMuxHostServer) ensureRuntimeDisplay(mode BrowserMode) (backend string, displayID uint32, x int, y int, vd *VirtualDisplayManager, dm *DisplayManager, ownsDisplay bool, err error) {
	mode = NormalizeBrowserMode(mode, ModeHeaded)
	if mode == ModeHeaded && runtime.GOOS == "darwin" {
		s.displayMu.Lock()
		defer s.displayMu.Unlock()
		if s.virtualDisp == nil {
			s.virtualDisp = &VirtualDisplayManager{}
		}
		if err := s.virtualDisp.Ensure(); err != nil {
			return "", 0, 0, 0, nil, nil, false, fmt.Errorf("browser_mux_host: headed mode unavailable on macOS: CGVirtualDisplay setup failed: %w", err)
		}
		x, y = s.virtualDisp.WindowPosition()
		return "cgvirtualdisplay", s.virtualDisp.DisplayID(), x, y, s.virtualDisp, nil, false, nil
	}
	if mode == ModeHeaded && runtime.GOOS == "linux" {
		s.displayMu.Lock()
		defer s.displayMu.Unlock()
		if s.displayMgr == nil {
			s.displayMgr = &DisplayManager{}
		}
		if !s.displayMgr.EnsureDisplay() {
			return "", 0, 0, 0, nil, nil, false, fmt.Errorf("browser_mux_host: headed mode unavailable on linux: Xvfb display setup failed")
		}
		return "xvfb", 0, 0, 0, nil, s.displayMgr, false, nil
	}
	backend, displayID, x, y, vd, dm, err = ensureBrowserMuxHostDisplay(mode)
	return backend, displayID, x, y, vd, dm, true, err
}

func ensureBrowserMuxHostDisplay(mode BrowserMode) (backend string, displayID uint32, x int, y int, vd *VirtualDisplayManager, dm *DisplayManager, err error) {
	mode = NormalizeBrowserMode(mode, ModeHeaded)
	switch mode {
	case ModeHeadless:
		return "none", 0, 0, 0, nil, nil, nil
	case ModeVisible:
		if runtime.GOOS == "linux" {
			dm = &DisplayManager{}
			if !dm.EnsureDisplay() {
				return "", 0, 0, 0, nil, nil, fmt.Errorf("browser_mux_host: visible mode unavailable on linux: Xvfb display setup failed")
			}
			return "xvfb", 0, 0, 0, nil, dm, nil
		}
		return "native", 0, 0, 0, nil, nil, nil
	case ModeHeaded:
		switch runtime.GOOS {
		case "linux":
			dm = &DisplayManager{}
			if !dm.EnsureDisplay() {
				return "", 0, 0, 0, nil, nil, fmt.Errorf("browser_mux_host: headed mode unavailable on linux: Xvfb display setup failed")
			}
			return "xvfb", 0, 0, 0, nil, dm, nil
		case "darwin":
			vd = &VirtualDisplayManager{}
			if err := vd.Ensure(); err != nil {
				return "", 0, 0, 0, nil, nil, fmt.Errorf("browser_mux_host: headed mode unavailable on macOS: CGVirtualDisplay setup failed: %w", err)
			}
			x, y = vd.WindowPosition()
			return "cgvirtualdisplay", vd.DisplayID(), x, y, vd, nil, nil
		default:
			return "", 0, 0, 0, nil, nil, fmt.Errorf("browser_mux_host: headed mode unavailable on %s", runtime.GOOS)
		}
	default:
		return "", 0, 0, 0, nil, nil, fmt.Errorf("browser_mux_host: unsupported mode %q", mode)
	}
}

func cleanupBrowserMuxHostDisplay(vd *VirtualDisplayManager, dm *DisplayManager) {
	if dm != nil {
		_ = dm.Close()
	}
	if vd != nil {
		_ = vd.Close()
	}
}

func enforceBrowserMuxHostWindow(wsURL string, width, height, posX, posY int) error {
	startedAt := time.Now()
	if width <= 0 {
		width = 1280
	}
	if height <= 0 {
		height = 800
	}
	allocStartedAt := time.Now()
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), wsURL)
	log.Printf("[BROWSER-MUX-HOST] window_enforce_step step=new_remote_allocator elapsed_ms=%d",
		time.Since(allocStartedAt).Milliseconds())
	defer allocCancel()
	contextStartedAt := time.Now()
	windowCtx, windowCancel := chromedp.NewContext(allocCtx, chromedp.WithErrorf(chromedpErrorf))
	log.Printf("[BROWSER-MUX-HOST] window_enforce_step step=new_context elapsed_ms=%d",
		time.Since(contextStartedAt).Milliseconds())
	defer windowCancel()
	var windowID cdpbrowser.WindowID
	runStartedAt := time.Now()
	err := runCDPWithSoftTimeout(windowCtx, BrowserMuxHostWindowEnforceTimeout, chromedp.ActionFunc(func(ctx context.Context) error {
		getStartedAt := time.Now()
		id, _, err := cdpbrowser.GetWindowForTarget().Do(ctx)
		if err != nil {
			return fmt.Errorf("browser_mux_host: get chrome window for target: %w", err)
		}
		windowID = id
		log.Printf("[BROWSER-MUX-HOST] window_enforce_step step=get_window window_id=%d elapsed_ms=%d",
			id, time.Since(getStartedAt).Milliseconds())
		setStartedAt := time.Now()
		if err := cdpbrowser.SetWindowBounds(id, &cdpbrowser.Bounds{
			Left:        int64(posX),
			Top:         int64(posY),
			Width:       int64(width),
			Height:      int64(height),
			WindowState: cdpbrowser.WindowStateNormal,
		}).Do(ctx); err != nil {
			return err
		}
		log.Printf("[BROWSER-MUX-HOST] window_enforce_step step=set_bounds window_id=%d pos=(%d,%d) size=%dx%d elapsed_ms=%d",
			id, posX, posY, width, height, time.Since(setStartedAt).Milliseconds())
		return nil
	}))
	log.Printf("[BROWSER-MUX-HOST] window_enforce_step step=chromedp_run elapsed_ms=%d err=%v",
		time.Since(runStartedAt).Milliseconds(), err)
	log.Printf("[BROWSER-MUX-HOST] window_enforce_completed window_id=%d pos=(%d,%d) size=%dx%d elapsed_ms=%d err=%v",
		windowID, posX, posY, width, height, time.Since(startedAt).Milliseconds(), err)
	return err
}

func writeBrowserMuxHostManifest(state *BrowserMuxHostState) error {
	normalizeBrowserMuxHostState(state)
	path := BrowserMuxHostManifestPath(state.MuxHostID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return writeFileAtomic(path, body, 0644)
}

func writeBrowserRuntimeManifest(state *BrowserMuxHostState) error {
	normalizeBrowserMuxHostState(state)
	if strings.TrimSpace(state.RuntimeID) == "" {
		return fmt.Errorf("runtime_id is required")
	}
	path := BrowserRuntimeManifestPath(state.RuntimeID)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return writeFileAtomic(path, body, 0644)
}

func removeBrowserMuxHostManifest(hostID string) {
	path := BrowserMuxHostManifestPath(hostID)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[BROWSER-MUX-HOST] remove manifest failed path=%s err=%v", path, err)
	}
}

func removeBrowserRuntimeManifest(runtimeID string) {
	path := BrowserRuntimeManifestPath(runtimeID)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[BROWSER-MUX-HOST] remove runtime manifest failed path=%s err=%v", path, err)
	}
}

func newBrowserMuxHostToken() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func (s *browserMuxHostServer) httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		runtimeID := runtimeIDFromRequest(r)
		if runtimeID != "" {
			rt, err := s.runtime(runtimeID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			state := rt.snapshot()
			state.Token = ""
			writeJSON(w, state)
			return
		}
		state := s.snapshot(false)
		writeJSON(w, state)
	})
	mux.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		runtimeID := runtimeIDFromRequest(r)
		if runtimeID != "" {
			rt, err := s.runtime(runtimeID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			writeJSON(w, rt.snapshot())
			return
		}
		writeJSON(w, s.snapshot(true))
	})
	mux.HandleFunc("/runtime/ensure", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req BrowserMuxHostRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
			return
		}
		req.MuxHostID = s.hostID
		state, err := s.ensureRuntime(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, state)
	})
	mux.HandleFunc("/touch", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req browserMuxHostTouchRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		runtimeID := runtimeIDFromRequest(r)
		if runtimeID != "" {
			rt, err := s.runtime(runtimeID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			rt.touch(req.OwnerPID)
			s.touch(req.OwnerPID)
			writeJSON(w, rt.snapshot())
			return
		}
		s.touch(req.OwnerPID)
		writeJSON(w, s.snapshot(true))
	})
	mux.HandleFunc("/release", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		runtimeID := runtimeIDFromRequest(r)
		if runtimeID != "" {
			rt, err := s.runtime(runtimeID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			rt.touch(0)
			s.touch(0)
			writeJSON(w, rt.snapshot())
			return
		}
		s.touch(0)
		writeJSON(w, s.snapshot(true))
	})
	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		runtimeID := runtimeIDFromRequest(r)
		if runtimeID != "" {
			rt, err := s.runtime(runtimeID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			state := rt.stateCopy()
			writeJSON(w, state)
			go func() {
				_, _ = s.closeRuntime(runtimeID, "api_shutdown")
			}()
			return
		}
		state := s.snapshot(true)
		writeJSON(w, state)
		go s.requestShutdown()
	})
	return mux
}

func (s *browserMuxHostServer) runtime(runtimeID string) (*browserMuxHostRuntime, error) {
	runtimeID = sanitizeBrowserRuntimeID(runtimeID)
	s.mu.Lock()
	rt := s.runtimes[runtimeID]
	s.mu.Unlock()
	if rt == nil {
		return nil, fmt.Errorf("browser runtime not found: %s", runtimeID)
	}
	return rt, nil
}

func (s *browserMuxHostServer) authorized(r *http.Request) bool {
	s.mu.Lock()
	token := s.token
	s.mu.Unlock()
	return token != "" && r.Header.Get(browserMuxHostTokenHeader) == token
}

func (s *browserMuxHostServer) snapshot(includeToken bool) BrowserMuxHostState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.snapshotLocked(includeToken)
	return state
}

func (s *browserMuxHostServer) snapshotLocked(includeToken bool) BrowserMuxHostState {
	runtimes := make([]BrowserRuntimeSummary, 0, len(s.runtimes))
	for _, rt := range s.runtimes {
		rtState := rt.stateCopy()
		runtimes = append(runtimes, BrowserRuntimeSummary{
			RuntimeID:        rtState.RuntimeID,
			BrowserSessionID: rtState.BrowserSessionID,
			SessionKind:      rtState.SessionKind,
			ServiceName:      rtState.ServiceName,
			AccountID:        rtState.AccountID,
			ProfileID:        rtState.ProfileID,
			BrowserPID:       rtState.BrowserPID,
			ChromePID:        rtState.ChromePID,
			ChromeAlive:      rtState.ChromeAlive,
			DisplayBackend:   rtState.DisplayBackend,
			DisplayID:        rtState.DisplayID,
		})
	}
	state := BrowserMuxHostState{
		MuxHostID:      s.hostID,
		MuxHostPID:     os.Getpid(),
		ControlURL:     s.controlURL,
		RuntimeCount:   len(runtimes),
		Runtimes:       runtimes,
		OwnerPID:       s.ownerPID,
		StartedAt:      s.startedAt,
		LastTouchedAt:  s.lastTouchedAt,
		IdleTTLMillis:  int64(s.idleTTL / time.Millisecond),
		MuxHostAlive:   true,
		DisplayBackend: "muxhost",
	}
	if includeToken {
		state.Token = s.token
	}
	normalizeBrowserMuxHostState(&state)
	return state
}

func (s *browserMuxHostServer) writeHostManifestLocked() error {
	state := s.snapshotLocked(true)
	return writeBrowserMuxHostManifest(&state)
}

func (s *browserMuxHostServer) touch(ownerPID int) {
	s.mu.Lock()
	s.ownerPID = ownerPID
	s.lastTouchedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = s.writeHostManifestLocked()
	s.mu.Unlock()
}

func (s *browserMuxHostServer) requestShutdown() {
	s.shutdown.Do(func() {
		close(s.shutdownCh)
	})
}

func (s *browserMuxHostServer) wait(ctx context.Context) error {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	ticker := time.NewTicker(BrowserMuxHostIdleCheckInterval)
	defer ticker.Stop()

	// Rescue ticker is darwin-only; on other platforms rescueCh is nil and
	// never fires. A select on a nil channel blocks forever — zero overhead.
	var rescueCh <-chan time.Time
	if runtime.GOOS == "darwin" {
		rescueTicker := time.NewTicker(VirtualDisplayForeignWindowRescueInterval)
		defer rescueTicker.Stop()
		rescueCh = rescueTicker.C
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sigCh:
			return nil
		case <-s.shutdownCh:
			return nil
		case <-ticker.C:
			s.reapStoppedRuntimes()
			if s.shouldIdleExit() {
				log.Printf("[BROWSER-MUX-HOST] AUDIT: muxhost_idle_ttl_elapsed muxhost_id=%s owner_pid=%d ttl_ms=%d",
					s.hostID, s.ownerPID, int64(s.idleTTL/time.Millisecond))
				return nil
			}
		case <-rescueCh:
			s.runForeignWindowRescue()
		}
	}
}

func (s *browserMuxHostServer) runForeignWindowRescue() {
	s.displayMu.Lock()
	hasVD := s.virtualDisp != nil
	s.displayMu.Unlock()
	if !hasVD {
		return
	}
	result, err := RescueForeignWindowsFromVirtualDisplay()
	if err != nil {
		log.Printf("[BROWSER-MUX-HOST] rescue_foreign_windows err=%v", err)
		return
	}
	if result.UnavailableReason != "" || result.Moved == 0 {
		return
	}
	log.Printf("[BROWSER-MUX-HOST] AUDIT: rescue_foreign_windows display_id=%d scanned=%d matched=%d moved=%d skipped=%d",
		result.DisplayID, result.Scanned, result.Matched, result.Moved, result.Skipped)
	for _, w := range result.Windows {
		if w.Moved {
			log.Printf("[BROWSER-MUX-HOST] rescued window_id=%d pid=%d owner=%q from=(%d,%d) to=(%d,%d)",
				w.WindowID, w.PID, w.Owner, w.X, w.Y, w.TargetX, w.TargetY)
		}
	}
}

func (s *browserMuxHostServer) reapStoppedRuntimes() {
	s.mu.Lock()
	runtimeIDs := make([]string, 0, len(s.runtimes))
	for runtimeID, rt := range s.runtimes {
		select {
		case <-rt.handle.Done():
			runtimeIDs = append(runtimeIDs, runtimeID)
		default:
			state := rt.stateCopy()
			if !state.ChromeAlive {
				runtimeIDs = append(runtimeIDs, runtimeID)
			}
		}
	}
	s.mu.Unlock()
	for _, runtimeID := range runtimeIDs {
		_, _ = s.closeRuntime(runtimeID, "chrome_exited")
	}
}

func (s *browserMuxHostServer) shouldIdleExit() bool {
	s.mu.Lock()
	runtimeIDs := make([]string, 0, len(s.runtimes))
	for runtimeID := range s.runtimes {
		runtimeIDs = append(runtimeIDs, runtimeID)
	}
	ownerPID := s.ownerPID
	lastTouchedAt := s.lastTouchedAt
	ttl := s.idleTTL
	s.mu.Unlock()

	for _, runtimeID := range runtimeIDs {
		rt, err := s.runtime(runtimeID)
		if err != nil {
			continue
		}
		if rt.shouldIdleExit() {
			_, _ = s.closeRuntime(runtimeID, "runtime_idle_ttl")
		}
	}

	s.mu.Lock()
	hasRuntimes := len(s.runtimes) > 0
	s.mu.Unlock()
	if hasRuntimes {
		return false
	}
	if ownerPID > 0 && isPIDAlive(ownerPID) {
		s.touch(ownerPID)
		return false
	}
	last, err := time.Parse(time.RFC3339Nano, lastTouchedAt)
	if err != nil {
		return false
	}
	if ttl <= 0 {
		ttl = BrowserMuxHostDefaultIdleTTL
	}
	return time.Since(last) >= ttl
}

func (s *browserMuxHostServer) closeRuntime(runtimeID string, cause string) (*BrowserMuxHostState, error) {
	runtimeID = sanitizeBrowserRuntimeID(runtimeID)
	s.mu.Lock()
	rt := s.runtimes[runtimeID]
	if rt == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("browser runtime not found: %s", runtimeID)
	}
	delete(s.runtimes, runtimeID)
	s.lastTouchedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = s.writeHostManifestLocked()
	s.mu.Unlock()

	state := rt.snapshot()
	log.Printf("[BROWSER-MUX-HOST] AUDIT: runtime_cleanup_requested muxhost_id=%s runtime_id=%s cause=%s chrome_pid=%d",
		s.hostID, runtimeID, cause, state.ChromePID)
	rt.cleanup()
	return &state, nil
}

func (s *browserMuxHostServer) cleanupAll() {
	s.mu.Lock()
	runtimes := make([]*browserMuxHostRuntime, 0, len(s.runtimes))
	for runtimeID, rt := range s.runtimes {
		runtimes = append(runtimes, rt)
		delete(s.runtimes, runtimeID)
	}
	_ = s.writeHostManifestLocked()
	s.mu.Unlock()
	for _, rt := range runtimes {
		rt.cleanup()
	}
	s.displayMu.Lock()
	cleanupBrowserMuxHostDisplay(s.virtualDisp, s.displayMgr)
	s.virtualDisp = nil
	s.displayMgr = nil
	s.displayMu.Unlock()
	removeBrowserMuxHostManifest(s.hostID)
	log.Printf("[BROWSER-MUX-HOST] AUDIT: muxhost_cleanup_done muxhost_id=%s", s.hostID)
}

func runtimeIDFromRequest(r *http.Request) string {
	raw := strings.TrimSpace(r.URL.Query().Get("runtime_id"))
	if raw == "" {
		return ""
	}
	return sanitizeBrowserRuntimeID(raw)
}

func (rt *browserMuxHostRuntime) snapshot() BrowserMuxHostState {
	state := rt.stateCopy()
	if rt.virtualDisp != nil && state.ChromeAlive {
		if err := rt.virtualDisp.VerifyChromeContained(state.ChromePID, BrowserMuxHostSnapshotContainmentCheck); err != nil {
			state = rt.repairVirtualDisplay(state, err)
		} else {
			state.DisplayVerified = true
			state.ChromeWindowContained = true
		}
	}
	return state
}

func (rt *browserMuxHostRuntime) stateCopy() BrowserMuxHostState {
	rt.mu.Lock()
	state := rt.state
	rt.mu.Unlock()
	normalizeBrowserMuxHostState(&state)
	return state
}

func (rt *browserMuxHostRuntime) repairVirtualDisplay(state BrowserMuxHostState, cause error) BrowserMuxHostState {
	if rt.virtualDisp == nil || !state.ChromeAlive {
		state.DisplayVerified = false
		state.ChromeWindowContained = false
		return state
	}
	log.Printf("[BROWSER-MUX-HOST] AUDIT: display_repair_start muxhost_id=%s chrome_pid=%d display_id=%d cause=%v",
		state.MuxHostID, state.ChromePID, state.DisplayID, cause)
	if err := rt.virtualDisp.EnsurePresent(); err != nil {
		state.DisplayVerified = false
		state.ChromeWindowContained = false
		log.Printf("[BROWSER-MUX-HOST] AUDIT: display_repair_failed muxhost_id=%s chrome_pid=%d stage=ensure err=%v",
			state.MuxHostID, state.ChromePID, err)
		return state
	}
	posX, posY := rt.virtualDisp.WindowPositionAt(0)
	width, height := state.ViewportW, state.ViewportH
	if width <= 0 {
		width = DefaultViewportWidth
	}
	if height <= 0 {
		height = DefaultViewportHeight
	}
	if err := enforceBrowserMuxHostWindow(state.WSURL, width, height, posX, posY); err != nil {
		state.DisplayVerified = false
		state.ChromeWindowContained = false
		log.Printf("[BROWSER-MUX-HOST] AUDIT: display_repair_failed muxhost_id=%s chrome_pid=%d stage=enforce err=%v",
			state.MuxHostID, state.ChromePID, err)
		return state
	}
	if err := rt.virtualDisp.VerifyChromeContained(state.ChromePID, BrowserMuxHostWindowContainmentTimeout); err != nil {
		state.DisplayVerified = false
		state.ChromeWindowContained = false
		log.Printf("[BROWSER-MUX-HOST] AUDIT: display_repair_failed muxhost_id=%s chrome_pid=%d stage=containment err=%v",
			state.MuxHostID, state.ChromePID, err)
		return state
	}

	state.DisplayID = rt.virtualDisp.DisplayID()
	state.DisplayVerified = true
	state.ChromeWindowContained = true
	rt.mu.Lock()
	rt.state.DisplayID = state.DisplayID
	rt.state.DisplayVerified = true
	rt.state.ChromeWindowContained = true
	rt.state.LastTouchedAt = time.Now().UTC().Format(time.RFC3339Nano)
	manifestState := rt.state
	rt.mu.Unlock()
	if err := writeBrowserRuntimeManifest(&manifestState); err != nil {
		log.Printf("[BROWSER-MUX-HOST] AUDIT: display_repair_manifest_failed muxhost_id=%s runtime_id=%s err=%v", state.MuxHostID, state.RuntimeID, err)
	}
	log.Printf("[BROWSER-MUX-HOST] AUDIT: display_repair_done muxhost_id=%s chrome_pid=%d display_id=%d",
		state.MuxHostID, state.ChromePID, state.DisplayID)
	return state
}

func (rt *browserMuxHostRuntime) touch(ownerPID int) {
	rt.mu.Lock()
	rt.state.OwnerPID = ownerPID
	rt.state.LastTouchedAt = time.Now().UTC().Format(time.RFC3339Nano)
	state := rt.state
	rt.mu.Unlock()
	_ = writeBrowserRuntimeManifest(&state)
}

func (rt *browserMuxHostRuntime) shouldIdleExit() bool {
	state := rt.stateCopy()
	if state.OwnerPID > 0 && isPIDAlive(state.OwnerPID) {
		rt.touch(state.OwnerPID)
		return false
	}
	last, err := time.Parse(time.RFC3339Nano, state.LastTouchedAt)
	if err != nil {
		return false
	}
	ttl := time.Duration(state.IdleTTLMillis) * time.Millisecond
	if ttl <= 0 {
		ttl = BrowserMuxHostDefaultIdleTTL
	}
	return time.Since(last) >= ttl
}

func (rt *browserMuxHostRuntime) cleanup() {
	state := rt.stateCopy()
	log.Printf("[BROWSER-MUX-HOST] AUDIT: runtime_cleanup_start muxhost_id=%s runtime_id=%s pid=%d chrome_pid=%d", state.MuxHostID, state.RuntimeID, os.Getpid(), state.ChromePID)
	if rt.handle != nil {
		_ = rt.handle.Kill()
	}
	if rt.profileDir != "" {
		RemoveProfileOwnerMarker(rt.profileDir, rt.identityKey)
	}
	if rt.ownsDisplay {
		cleanupBrowserMuxHostDisplay(rt.virtualDisp, rt.displayMgr)
	}
	removeBrowserRuntimeManifest(state.RuntimeID)
	log.Printf("[BROWSER-MUX-HOST] AUDIT: runtime_cleanup_done muxhost_id=%s runtime_id=%s", state.MuxHostID, state.RuntimeID)
}

func browserMuxHostRequest(ctx context.Context, state *BrowserMuxHostState, method string, path string, payload interface{}) (*BrowserMuxHostState, error) {
	if state == nil {
		return nil, fmt.Errorf("browser_mux_host: empty state")
	}
	base := strings.TrimRight(state.ControlURL, "/")
	if base == "" {
		return nil, fmt.Errorf("browser_mux_host: empty control url")
	}
	var body io.Reader
	if payload != nil {
		data, _ := json.Marshal(payload)
		body = bytes.NewReader(data)
	}
	requestPath := path
	if runtimeID := strings.TrimSpace(state.RuntimeID); runtimeID != "" {
		sep := "?"
		if strings.Contains(requestPath, "?") {
			sep = "&"
		}
		requestPath += sep + "runtime_id=" + url.QueryEscape(runtimeID)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+requestPath, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if state.Token != "" {
		req.Header.Set(browserMuxHostTokenHeader, state.Token)
	}
	timeout := BrowserMuxHostControlRequestTimeout
	if path == "/runtime/ensure" {
		timeout = BrowserMuxHostReadyTimeout
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("browser_mux_host: %s %s returned %s", method, requestPath, resp.Status)
	}
	var out BrowserMuxHostState
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Token == "" {
		out.Token = state.Token
	}
	normalizeBrowserMuxHostState(&out)
	return &out, nil
}

func writeJSON(w http.ResponseWriter, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}
