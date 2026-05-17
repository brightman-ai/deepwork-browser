//go:build !darwin && !linux && !windows

// Package browser — Workspace stub for unsupported platforms.
package browser

import "log"

// NewWorkspace returns a no-op Workspace for unsupported platforms.
func NewWorkspace() Workspace {
	log.Printf("[WORKSPACE] unsupported platform — using no-op workspace (no isolation)")
	return &NoopWorkspace{}
}
