//go:build linux

// Package browser — Linux Workspace (no-op: Xvfb provides isolation).
//
// On Linux, display isolation is already achieved by running Chrome inside
// an Xvfb virtual display (managed by DisplayManager.ensureDisplayLinux).
// The Workspace abstraction is a no-op on Linux — Xvfb IS the isolated workspace.
//
// [Ref: DDC-I-11 — Linux Xvfb / macOS Spaces are isomorphic Isolated Workspace primitives]
// [Ref: Iron Rule — Linux display_manager.go::ensureDisplayLinux() MUST NOT be modified]
package browser

import "log"

// linuxWorkspace is a no-op Workspace for Linux (Xvfb handles isolation).
type linuxWorkspace struct{}

// NewWorkspace returns a no-op Workspace for Linux.
// Actual isolation is provided by DisplayManager.ensureDisplayLinux (Xvfb).
func NewWorkspace() Workspace {
	log.Printf("[WORKSPACE-LINUX] using Xvfb isolation (no-op Workspace)")
	return &linuxWorkspace{}
}

func (w *linuxWorkspace) EnsureSpace() (int64, error) { return 0, nil }

// LaunchChromeInSpace forks Chrome directly — Xvfb (set up by DisplayManager)
// already provides isolation. Chrome inherits DISPLAY=:99 from parent env.
func (w *linuxWorkspace) LaunchChromeInSpace(spec ChromeLaunchSpec) (ChromeHandle, error) {
	return startChromeProcess(spec)
}

func (w *linuxWorkspace) Close() error { return nil }
