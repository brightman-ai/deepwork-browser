//go:build windows

// Package browser — Windows Workspace stub (Phase 0).
//
// TODO (Phase 0 Windows): Implement IVirtualDesktopManager COM bridge.
// Windows Virtual Desktop API requires COM + IVirtualDesktopManager2.
// Stub returns no-op to unblock macOS/Linux without introducing Windows CI failures.
//
// [windows-specific bug class: DDC-I-12]
// [Ref: Round 6, TH-0418-c9x — Windows VDesktop is isomorphic to Linux Xvfb / macOS Spaces]
package browser

import "log"

// windowsWorkspace is a stub implementing Workspace for Windows.
// TODO: implement IVirtualDesktopManager COM bridge.
type windowsWorkspace struct{}

// NewWorkspace returns a stub Workspace for Windows.
// WARNING: Chrome windows will appear on the user's current desktop (no isolation).
func NewWorkspace() Workspace {
	log.Printf("[WORKSPACE-WIN] WARNING: Windows Virtual Desktop isolation not yet implemented. Chrome will appear on current desktop. TODO: IVirtualDesktopManager COM bridge.")
	return &windowsWorkspace{}
}

func (w *windowsWorkspace) EnsureSpace() (int64, error) {
	log.Printf("[WORKSPACE-WIN] EnsureSpace: stub — no isolation on Windows yet")
	return 0, nil
}

// LaunchChromeInSpace just forks Chrome — Windows Virtual Desktop isolation TODO.
// Chrome appears on the user's current desktop (no isolation yet).
func (w *windowsWorkspace) LaunchChromeInSpace(spec ChromeLaunchSpec) (ChromeHandle, error) {
	log.Printf("[WORKSPACE-WIN] LaunchChromeInSpace: stub — no isolation on Windows yet")
	return startChromeProcess(spec)
}

func (w *windowsWorkspace) Close() error { return nil }
