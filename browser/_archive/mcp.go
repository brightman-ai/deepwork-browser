package browser

import "context"

// MCPBridge is the Phase 2 MCP Extension interface (noop implementation).
// When Phase 2 is implemented, replace this with a real MCP bridge.
type MCPBridge interface {
	IsEnabled bool
	// Future: MCP tool registration and dispatch
}

// noopMCPBridge is the Phase 0/1 placeholder implementation.
type noopMCPBridge struct{}

// NewMCPBridge returns the noop MCP bridge for Phase 0/1.
func NewMCPBridge MCPBridge {
	return &noopMCPBridge{}
}

func (m *noopMCPBridge) IsEnabled bool { return false }

// CallMCP returns ErrMCPNotEnabled for all calls during Phase 0/1.
func CallMCP(_ context.Context, _ MCPBridge, _ string, _ any) (any, error) {
	return nil, ErrMCPNotEnabled
}
