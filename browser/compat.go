// compat.go provides backward-compatible stubs for types that were in the
// old go-rod-based browser package and are still referenced by
// internal/desktop (Wails UI). These types are deprecated and will be
// removed after the desktop package is migrated to BS-09 BrowserCore API.
//
// DO NOT add new usages of these types. Use BrowserCore instead.
package browser

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrBrowserUnavailable is returned by deprecated stub methods.
var ErrBrowserUnavailable = errors.New("browser: unavailable (deprecated stub)")

// ============================================================
// § Old data types — kept for internal/desktop compat
// ============================================================

// ActionResult is a deprecated result type from the old browser package.
// Kept for backward compatibility.
type ActionResult struct {
	Success  bool
	Error    string
	Duration time.Duration
}

// Box represents a bounding box for an element.
// Deprecated: kept for compat with internal/desktop.
type Box struct {
	X      float64
	Y      float64
	Width  float64
	Height float64
}

// StableKey is the stable key descriptor for a SuperNode.
// Deprecated: kept for compat.
type StableKey struct {
	ID        string
	AriaLabel string
	Name      string
	Role      string
	Href      string
	Type      string
}

// SuperNode is a stable node descriptor from the old browser architecture.
// Deprecated: kept for compat with internal/desktop.
type SuperNode struct {
	ID        int
	TagName   string
	Text      string
	Clickable bool
	Box       Box
	StableKey StableKey
}

// HUDConfig is the config for the Phantom HUD renderer.
// Deprecated: kept for compat with internal/desktop.
type HUDConfig struct {
	MaxNodes            int
	LabelFontSize       int
	ClickRippleDuration time.Duration
}

// DefaultHUDConfig returns the default HUD configuration.
// Deprecated.
func DefaultHUDConfig() HUDConfig {
	return HUDConfig{
		MaxNodes:            100,
		LabelFontSize:       12,
		ClickRippleDuration: 500 * time.Millisecond,
	}
}

// RippleType is the type of ripple animation.
// Deprecated.
type RippleType string

const (
	// RippleTypeClick indicates a click ripple.
	RippleTypeClick RippleType = "click"
	// RippleTypeDrag indicates a drag ripple.
	RippleTypeDrag RippleType = "drag"
	// RippleTypeKey indicates a keyboard ripple.
	RippleTypeKey RippleType = "key"
)

// AgentState represents the visual state of the agent in the HUD.
// Deprecated.
type AgentState string

const (
	// AgentStateThinking indicates the agent is thinking.
	AgentStateThinking AgentState = "thinking"
	// AgentStateActing indicates the agent is acting.
	AgentStateActing AgentState = "acting"
)

// HUDRenderer is the interface for Phantom HUD rendering.
// Deprecated: kept for compat with internal/desktop.
type HUDRenderer interface {
	Init(ctx context.Context, cfg HUDConfig) error
	Show() error
	Hide() error
	Render(nodes []SuperNode) error
	RenderRipple(x, y int, t RippleType) error
	SetStatus(status string) error
	Highlight(nodeID int) error
	ClearHighlight() error
	SetPosition(x, y, width, height int) error
	Close() error
}

// CDPConfig holds CDP connection configuration.
// Deprecated: use browser.BrowserCore instead.
type CDPConfig struct {
	Headless   bool
	ControlURL string
	DataDir    string
}

// DefaultCDPConfig returns a default CDP configuration.
// Deprecated.
func DefaultCDPConfig() CDPConfig {
	return CDPConfig{
		Headless: true,
	}
}

// CDPManager is the old browser manager type.
// Deprecated: use BrowserCore instead.
type CDPManager struct{}

// NewCDPManager creates a deprecated CDPManager stub.
func NewCDPManager(cfg CDPConfig) *CDPManager {
	return &CDPManager{}
}

// IsConnected always returns false on the stub CDPManager.
func (m *CDPManager) IsConnected() bool { return false }

// Connect is a no-op on the stub CDPManager.
func (m *CDPManager) Connect(ctx context.Context) error {
	return ErrBrowserUnavailable
}

// Disconnect is a no-op on the stub CDPManager.
func (m *CDPManager) Disconnect() error { return nil }

// NewPage returns a stub page on the deprecated CDPManager.
func (m *CDPManager) NewPage(ctx context.Context, url string) (*CompatPage, string, error) {
	return nil, "", ErrBrowserUnavailable
}

// Navigate is a no-op on the stub CDPManager.
func (m *CDPManager) Navigate(ctx context.Context, pageID, url string) error {
	return ErrBrowserUnavailable
}

// GetCurrentPage returns nil on the stub CDPManager.
func (m *CDPManager) GetCurrentPage() (*CompatPage, error) {
	return nil, ErrBrowserUnavailable
}

// GetAllPages returns empty on the stub CDPManager.
func (m *CDPManager) GetAllPages() ([]*CompatPage, error) {
	return nil, nil
}

// CompatPage is a stub page type for backward compatibility.
// Deprecated.
type CompatPage struct{}

// CompatTargetInfo is a stub for target info in compat layer.
type CompatTargetInfo struct {
	URL   string
	Title string
}

// Info returns stub target info.
func (p *CompatPage) Info() (*CompatTargetInfo, error) {
	return &CompatTargetInfo{}, nil
}

// ============================================================
// § noopHUDRenderer — internal no-op implementation
// ============================================================

// noopHUDRenderer is the default no-op HUD renderer.
type noopHUDRenderer struct{}

func (h *noopHUDRenderer) Init(_ context.Context, _ HUDConfig) error { return nil }
func (h *noopHUDRenderer) Show() error                                { return nil }
func (h *noopHUDRenderer) Hide() error                                { return nil }
func (h *noopHUDRenderer) Render(_ []SuperNode) error                 { return nil }
func (h *noopHUDRenderer) RenderRipple(_, _ int, _ RippleType) error  { return nil }
func (h *noopHUDRenderer) SetStatus(_ string) error                   { return nil }
func (h *noopHUDRenderer) Highlight(_ int) error                      { return nil }
func (h *noopHUDRenderer) ClearHighlight() error                      { return nil }
func (h *noopHUDRenderer) SetPosition(_, _, _, _ int) error           { return nil }
func (h *noopHUDRenderer) Close() error                               { return nil }

// NewCDPHUDRenderer creates a deprecated HUD renderer stub.
// Deprecated: use LiveView WebSocket API instead.
func NewCDPHUDRenderer(controlURL string) HUDRenderer {
	return &noopHUDRenderer{}
}

// ============================================================
// § MemoryHUDRenderer — in-memory implementation for testing
// ============================================================

// MemoryRippleCall records a single RenderRipple call.
type MemoryRippleCall struct {
	X          int
	Y          int
	RippleType RippleType
}

// MemoryHUDRenderer is an in-memory HUDRenderer for use in unit tests.
type MemoryHUDRenderer struct {
	mu            sync.Mutex
	rippleHistory []MemoryRippleCall
	nodes         []SuperNode
	status        string
	visible       bool
}

// NewMemoryHUDRenderer creates a new in-memory HUD renderer for testing.
func NewMemoryHUDRenderer() *MemoryHUDRenderer {
	return &MemoryHUDRenderer{}
}

func (r *MemoryHUDRenderer) Init(_ context.Context, _ HUDConfig) error { return nil }
func (r *MemoryHUDRenderer) Show() error {
	r.mu.Lock()
	r.visible = true
	r.mu.Unlock()
	return nil
}
func (r *MemoryHUDRenderer) Hide() error {
	r.mu.Lock()
	r.visible = false
	r.mu.Unlock()
	return nil
}
func (r *MemoryHUDRenderer) Render(nodes []SuperNode) error {
	r.mu.Lock()
	r.nodes = nodes
	r.mu.Unlock()
	return nil
}
func (r *MemoryHUDRenderer) RenderRipple(x, y int, t RippleType) error {
	r.mu.Lock()
	r.rippleHistory = append(r.rippleHistory, MemoryRippleCall{X: x, Y: y, RippleType: t})
	r.mu.Unlock()
	return nil
}
func (r *MemoryHUDRenderer) SetStatus(status string) error {
	r.mu.Lock()
	r.status = status
	r.mu.Unlock()
	return nil
}
func (r *MemoryHUDRenderer) Highlight(_ int) error  { return nil }
func (r *MemoryHUDRenderer) ClearHighlight() error  { return nil }
func (r *MemoryHUDRenderer) SetPosition(_, _, _, _ int) error { return nil }
func (r *MemoryHUDRenderer) Close() error           { return nil }

// GetRippleHistory returns a copy of all recorded RippleRender calls.
func (r *MemoryHUDRenderer) GetRippleHistory() []MemoryRippleCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]MemoryRippleCall, len(r.rippleHistory))
	copy(result, r.rippleHistory)
	return result
}

// ============================================================
// § HumanOverride — stub for internal/desktop tests
// ============================================================

// HumanOverride is a stub for the old human-override mechanism.
// Deprecated.
type HumanOverride struct {
	mu     sync.Mutex
	active bool
}

// NewHumanOverride creates a new HumanOverride stub.
func NewHumanOverride() *HumanOverride {
	return &HumanOverride{}
}

// Toggle switches the override state.
func (h *HumanOverride) Toggle() {
	h.mu.Lock()
	h.active = !h.active
	h.mu.Unlock()
}

// IsActive returns true if human override is active.
func (h *HumanOverride) IsActive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.active
}

// ============================================================
// § PhantomHUD — stub for internal/desktop tests
// ============================================================

// PhantomHUD is a stub for the old Phantom HUD orchestrator.
// Deprecated.
type PhantomHUD struct {
	mu       sync.Mutex
	config   HUDConfig
	renderer HUDRenderer
}

// NewPhantomHUD creates a new PhantomHUD stub.
func NewPhantomHUD(cfg HUDConfig, renderer HUDRenderer) *PhantomHUD {
	return &PhantomHUD{config: cfg, renderer: renderer}
}

// Update renders the given nodes via the underlying renderer.
func (p *PhantomHUD) Update(nodes []SuperNode) error {
	p.mu.Lock()
	r := p.renderer
	p.mu.Unlock()
	if r == nil {
		return nil
	}
	return r.Render(nodes)
}
