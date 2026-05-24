// Package browser — observability: STG constants, package logger, metrics, ValidateDeps.
// [T5-BS-09, pkg/obs, IR-01: zero deepwork context dependency]
package browser

import "github.com/brightman-ai/kit/obs"

// ─────────────────────────────────────────
// STG constants — business stage coordinates [pkg/obs CAP-TSOBS-C1]
// Format: {package}/{phase}
// ─────────────────────────────────────────

const (
	// STGSessionCreate is the stage for acquiring/creating a browser tab or session.
	STGSessionCreate = "browser/session/create"

	// STGSessionNavigate is the stage for Navigate calls (URL load + settle wait).
	STGSessionNavigate = "browser/session/navigate"

	// STGSessionAction is the stage for Act / ActWithSessionMode calls.
	STGSessionAction = "browser/session/action"

	// STGSnapshot is the stage for Snap / SnapWithSessionMode A11y snapshot capture.
	STGSnapshot = "browser/snapshot"

	// STGLiveView is the stage for StartLiveView / Screencast frame pipeline.
	STGLiveView = "browser/liveview"
)

// ─────────────────────────────────────────
// Package Logger
// ─────────────────────────────────────────

// logger is the package-level structured logger. mod matches the directory name "browser".
var logger = obs.Module("browser")

// ─────────────────────────────────────────
// Package Metrics
// ─────────────────────────────────────────

var (
	// Navigate RED triple — Rate / Error / Duration for Navigate operations.
	browserNavigateRequests, browserNavigateErrors, browserNavigateDurationSeconds = obs.NewRED("browser_navigate")

	// Action RED triple — Rate / Error / Duration for Act operations.
	browserActionRequests, browserActionErrors, browserActionDurationSeconds = obs.NewRED("browser_action")

	// Snapshot RED triple — Rate / Error / Duration for Snap operations.
	browserSnapshotRequests, browserSnapshotErrors, browserSnapshotDurationSeconds = obs.NewRED("browser_snapshot")

	// browserActiveSessions tracks the number of active BrowserPool tabs (AcquireTab − ReleaseTab).
	browserActiveSessions = obs.NewGauge("browser_active_sessions")

	// browserActiveConnections tracks live WebSocket / SSE connections to the browser panel.
	browserActiveConnections = obs.NewGauge("browser_active_connections")

	// browserCDPErrors counts CDP-level errors (ErrCDPDisconnected, CDP run failures).
	browserCDPErrors = obs.NewCounter("browser_cdp_errors_total")

	// browserLiveViewFrames counts Screencast frames published to FrameBroadcastHub.
	browserLiveViewFrames = obs.NewCounter("browser_liveview_frames_total")

	// browserSnapshotFallbacks counts A11y→DOM→screenshot fallback activations [IR-07].
	browserSnapshotFallbacks = obs.NewCounter("browser_snapshot_fallbacks_total")

	// browserTakeoverSwitches counts Takeover enable/disable transitions.
	browserTakeoverSwitches = obs.NewCounter("browser_takeover_switches_total")

	// browserTargetSwitches counts TargetTracker automatic target-follow switches [CAP-BS09-C3 r3].
	browserTargetSwitches = obs.NewCounter("browser_target_switches_total")
)

// ─────────────────────────────────────────
// ValidateDeps — startup DI guard
// ─────────────────────────────────────────

// ValidatePoolConfig panics if the BrowserPool configuration violates required invariants.
// Call in NewBrowserPool after field defaults are applied.
func ValidatePoolConfig(cfg PoolConfig) {
	obs.True(cfg.MaxTabs > 0, "browser.PoolConfig.MaxTabs must be > 0, got %d", cfg.MaxTabs)
	obs.True(cfg.IdleTimeout > 0, "browser.PoolConfig.IdleTimeout must be > 0")
	obs.True(cfg.DataDir != "", "browser.PoolConfig.DataDir must not be empty")
}

// ValidateCoreImpl panics if a BrowserCoreImpl's essential internal engines are nil.
// Pass the concrete impl fields; call after construction before serving operations.
func ValidateCoreImpl(snapshotEng *snapshotEngine, liveEng *liveViewEngine) {
	obs.NotNil(snapshotEng, "browser.BrowserCoreImpl.snapshotEngine")
	obs.NotNil(liveEng, "browser.BrowserCoreImpl.liveViewEngine")
}
