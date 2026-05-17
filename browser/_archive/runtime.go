package browser

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brightman-ai/deepwork-browser/internal/tool"
	"github.com/brightman-ai/kit/log"

	"github.com/go-rod/rod"
)

var logger = log.Module("browser")

// BrowserRuntime is the core interface for browser Browser Automation.
type BrowserRuntime interface {
	// Lifecycle
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	IsRunning bool
	State RuntimeState
	Reset(ctx context.Context) error

	// Page Automation (registered as TS-03 Tools)
	Navigate(ctx context.Context, url string, waitLoad bool) (*NavigateResult, error)
	Click(ctx context.Context, selector string, timeoutMs int) error
	Type(ctx context.Context, selector, text string, clearFirst bool) error
	Screenshot(ctx context.Context, fullPage bool, quality int) (byte, error)
	Extract(ctx context.Context, selector string, multiple bool) (*ExtractResult, error)
	Evaluate(ctx context.Context, js string) (any, error)
	WaitForSelector(ctx context.Context, selector string, timeoutMs int, visible bool) (bool, error)

	// TS-03 Registration
	RegisterTools(registry tool.ToolRegistry) error

	// LiveView
	StartScreencast(ctx context.Context, quality int, fps int) (<-chan Frame, error)
	StopScreencast(ctx context.Context) error

	// Takeover
	RequestTakeover(ctx context.Context) error
	ReleaseTakeover(ctx context.Context) error

	// Credentials
	GetCookies(ctx context.Context, domain string) (Cookie, error)
	SetCookies(ctx context.Context, cookies Cookie) error
}

// EventBus is a minimal in-process event bus.
type EventBus interface {
	Publish(event any)
	Subscribe(handler func(event any))
}

// Config holds BrowserRuntime configuration.
type Config struct {
	ChromiumURL string
	ChromeFlags string
	MaxRestarts int
	RestartIntervalSec int
	CDPReconnectAttempts int
	ProfileDir string
	DataDir string
}

// DefaultConfig returns sensible defaults.
func DefaultConfig Config {
	return Config{
		ChromiumURL: ""
		ChromeFlags: string{"--no-sandbox", "--disable-gpu"}
		MaxRestarts: 3
		RestartIntervalSec: 5
		CDPReconnectAttempts: 3
		ProfileDir: ""
		DataDir: ""
	}
}

// browserRuntime is the concrete implementation of BrowserRuntime.
type browserRuntime struct {
	cfg Config
	configDB *sql.DB // for KV credential storage

	// State
	stateMu sync.RWMutex
	state RuntimeState

	// Rod objects
	browserMu sync.Mutex
	browser *rod.Browser

	// Crash recovery
	crashCount int32 // atomic

	// Active page cancels (for crash broadcast)
	activeMu sync.Mutex
	activeCancels context.CancelFunc

	// LiveView
	liveviewMu sync.Mutex
	liveviewPage *rod.Page
	screencastCh chan Frame
	screencastCancel context.CancelFunc

	// Takeover
	takeoverLock TakeoverLock

	// EventBus
	bus EventBus

	// Lifecycle context
	runCtx context.Context
	runCancel context.CancelFunc
}

// New creates a new BrowserRuntime.
// configDB is the TS-01 ConfigDB for KV credential storage.
// bus is the application EventBus (may be nil for tests).
func New(cfg Config, configDB *sql.DB, bus EventBus) BrowserRuntime {
	if bus == nil {
		bus = &noopBus{}
	}
	return &browserRuntime{
		cfg: cfg
		configDB: configDB
		state: StateUninitialized
		bus: bus
	}
}

// State returns the current RuntimeState (thread-safe).
func (r *browserRuntime) State RuntimeState {
	r.stateMu.RLock
	defer r.stateMu.RUnlock
	return r.state
}

// IsRunning returns true if state == StateRunning.
func (r *browserRuntime) IsRunning bool {
	return r.State == StateRunning
}

// setState transitions the state (thread-safe).
func (r *browserRuntime) setState(s RuntimeState) {
	r.stateMu.Lock
	defer r.stateMu.Unlock
	r.state = s
}

// registerActiveCancel adds a cancel func to the active set.
func (r *browserRuntime) registerActiveCancel(cancel context.CancelFunc) {
	r.activeMu.Lock
	defer r.activeMu.Unlock
	r.activeCancels = append(r.activeCancels, cancel)
}

// unregisterActiveCancel removes a cancel func from the active set.
func (r *browserRuntime) unregisterActiveCancel(cancel context.CancelFunc) {
	r.activeMu.Lock
	defer r.activeMu.Unlock
	newList := r.activeCancels[:0]
	for _, c := range r.activeCancels {
		if &c != &cancel {
			newList = append(newList, c)
		}
	}
	r.activeCancels = newList
}

// cancelAllActive cancels all active page contexts (called on crash).
func (r *browserRuntime) cancelAllActive {
	r.activeMu.Lock
	list := r.activeCancels
	r.activeCancels = nil
	r.activeMu.Unlock
	for _, cancel := range list {
		cancel
	}
}

// noopBus is a no-op EventBus for testing and optional use.
type noopBus struct{}

func (b *noopBus) Publish(event any) {}
func (b *noopBus) Subscribe(handler func(any)) {}

// simpleEventBus is a basic in-process pub/sub bus.
type simpleEventBus struct {
	mu sync.RWMutex
	handlers func(any)
}

// NewEventBus creates a new simple in-process event bus.
func NewEventBus EventBus {
	return &simpleEventBus{}
}

func (b *simpleEventBus) Publish(event any) {
	b.mu.RLock
	handlers := make(func(any), len(b.handlers))
	copy(handlers, b.handlers)
	b.mu.RUnlock
	for _, h := range handlers {
		h(event)
	}
}

func (b *simpleEventBus) Subscribe(handler func(any)) {
	b.mu.Lock
	defer b.mu.Unlock
	b.handlers = append(b.handlers, handler)
}

// TakeoverLock implements mutual exclusion for Takeover protocol.
type TakeoverLock struct {
	mu sync.Mutex
	active bool
	holder string
}

// Acquire occupies the TakeoverLock. Returns ErrTakeoverConflict if already active.
func (l *TakeoverLock) Acquire(holder string) error {
	l.mu.Lock
	defer l.mu.Unlock
	if l.active {
		return ErrTakeoverConflict
	}
	l.active = true
	l.holder = holder
	return nil
}

// Release frees the TakeoverLock.
func (l *TakeoverLock) Release {
	l.mu.Lock
	defer l.mu.Unlock
	l.active = false
	l.holder = ""
}

// IsActive reports whether the lock is held.
func (l *TakeoverLock) IsActive bool {
	l.mu.Lock
	defer l.mu.Unlock
	return l.active
}

// Start initializes and starts the BrowserRuntime.
// Performs Chrome detection/download then connects via Rod.
func (r *browserRuntime) Start(ctx context.Context) error {
	r.setState(StateInitializing)

	execPath, err := r.detectChrome
	if err != nil {
		// Chrome not found — trigger download
		logger.Info("system Chrome not found, downloading Chromium")
		if err2 := r.downloadChromium(ctx); err2 != nil {
			r.setState(StateUnavailable)
			return ErrBrowserUnavailable
		}
		execPath, err = r.detectChrome
		if err != nil {
			r.setState(StateUnavailable)
			return ErrBrowserUnavailable
		}
	}

	if err := r.startChrome(ctx, execPath); err != nil {
		r.setState(StateUnavailable)
		return err
	}

	r.runCtx, r.runCancel = context.WithCancel(context.Background)
	atomic.StoreInt32(&r.crashCount, 0)
	r.setState(StateRunning)
	logger.Info("browser runtime started")
	return nil
}

// Stop gracefully shuts down the BrowserRuntime.
func (r *browserRuntime) Stop(ctx context.Context) error {
	if r.runCancel != nil {
		r.runCancel
	}
	r.cancelAllActive

	r.liveviewMu.Lock
	if r.liveviewPage != nil {
		_ = r.liveviewPage.Close
		r.liveviewPage = nil
	}
	r.liveviewMu.Unlock

	r.browserMu.Lock
	if r.browser != nil {
		_ = r.browser.Close
		r.browser = nil
	}
	r.browserMu.Unlock

	r.setState(StateStopped)
	logger.Info("browser runtime stopped")
	return nil
}

// Reset transitions from Unavailable → Uninitialized.
func (r *browserRuntime) Reset(ctx context.Context) error {
	r.setState(StateUninitialized)
	atomic.StoreInt32(&r.crashCount, 0)
	return nil
}

// handleCrash manages Chrome crash detection and auto-restart.
func (r *browserRuntime) handleCrash(ctx context.Context) {
	count := atomic.AddInt32(&r.crashCount, 1)
	if int(count) > r.cfg.MaxRestarts {
		r.setState(StateUnavailable)
		r.bus.Publish(EventBrowserCrashed{})
		logger.Warn("browser max restarts exceeded", "count", count)
		return
	}

	r.setState(StateRecovering)
	r.bus.Publish(EventBrowserCrashed{})
	r.cancelAllActive

	restartInterval := time.Duration(r.cfg.RestartIntervalSec) * time.Second
	// Exponential backoff: 5s/10s/20s
	delay := restartInterval * time.Duration(int64(1)<<uint(count-1))
	logger.Info("browser crash recovery", "attempt", count, "delay", delay)

	select {
	case <-time.After(delay):
	case <-ctx.Done:
		return
	}

	execPath, err := r.detectChrome
	if err != nil {
		r.handleCrash(ctx)
		return
	}

	if err := r.startChrome(ctx, execPath); err != nil {
		r.handleCrash(ctx)
		return
	}

	r.setState(StateRunning)
	r.bus.Publish(EventBrowserRestarted{})
	logger.Info("browser restarted successfully", "attempt", count)
}
