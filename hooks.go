package dwbrowser

import "context"

// Hooks allows the host application to intercept browser events.
type Hooks struct {
	// OnNavigate is called when a session navigates to a new URL.
	OnNavigate func(ctx context.Context, sessionID string, url string) error

	// OnPageLoad is called when a page finishes loading.
	OnPageLoad func(ctx context.Context, sessionID string, title string) error

	// WorkspaceLauncher integrates with an external workspace manager
	// (e.g., macOS virtual display spaces). Optional.
	WorkspaceLauncher WorkspaceLauncher
}

// WorkspaceLauncher abstracts launching Chrome inside a managed workspace/space.
type WorkspaceLauncher interface {
	// LaunchChromeInSpace launches Chrome in a dedicated workspace space.
	// Returns the Chrome DevTools WebSocket URL and the process PID.
	LaunchChromeInSpace(ctx context.Context, profile string) (wsURL string, pid int, err error)
}
