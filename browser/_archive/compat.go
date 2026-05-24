// compat.go provides backward-compatible stubs for types used by
// internal/desktop and internal/skill that were in the old browser package.
// These types are deprecated and will be removed in Wave-2.
package browser

import (
	"context"
	"time"
)

// ActionResult is a deprecated result type from the old browser package.
// Kept for backward compatibility with internal/skill.
// Use ToolResult from TS-03 instead.
type ActionResult struct {
	Success  bool
	Error    string
	Duration time.Duration
}

// Box represents a bounding box for an element.
// Deprecated: kept for compat with internal/desktop and internal/skill.
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
// Deprecated: kept for compat with internal/skill and internal/desktop.
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
	MaxNodes        int
	LabelFontSize   int
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
	Close() error
}

// CDPConfig holds CDP connection configuration.
// Deprecated: use browser.Config instead.
type CDPConfig struct {
	Headless    bool
	ControlURL  string
	DataDir     string
}

// DefaultCDPConfig returns a default CDP configuration.
// Deprecated.
func DefaultCDPConfig() CDPConfig {
	return CDPConfig{
		Headless: true,
	}
}

// CDPManager is the old browser manager type.
// Deprecated: use BrowserRuntime instead.
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

// noopHUDRenderer is the default no-op HUD renderer.
type noopHUDRenderer struct{}

func (h *noopHUDRenderer) Init(_ context.Context, _ HUDConfig) error    { return nil }
func (h *noopHUDRenderer) Show() error                                    { return nil }
func (h *noopHUDRenderer) Hide() error                                    { return nil }
func (h *noopHUDRenderer) Render(_ []SuperNode) error                    { return nil }
func (h *noopHUDRenderer) RenderRipple(_, _ int, _ RippleType) error { return nil }
func (h *noopHUDRenderer) SetStatus(_ string) error                       { return nil }
func (h *noopHUDRenderer) Close() error                                   { return nil }

// NewCDPHUDRenderer creates a deprecated HUD renderer stub.
// Deprecated: use LiveView WebSocket API instead.
func NewCDPHUDRenderer(controlURL string) HUDRenderer {
	return &noopHUDRenderer{}
}
