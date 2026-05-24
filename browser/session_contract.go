package browser

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// BrowserSessionKind is the public scenario identity for a browser session.
// It must stay domain-neutral: product terms such as portal or webchat belong
// to callers, not to dw-browser's public contract.
type BrowserSessionKind string

const (
	SessionKindTask        BrowserSessionKind = "task"
	SessionKindInteractive BrowserSessionKind = "interactive"
	SessionKindService     BrowserSessionKind = "service"
	SessionKindDebug       BrowserSessionKind = "debug"
	SessionKindTest        BrowserSessionKind = "test"
)

const (
	SessionOwnerAgent   = "agent"
	SessionOwnerHuman   = "human"
	SessionOwnerService = "service"

	SessionIsolationEphemeral = "ephemeral"
	SessionIsolationDedicated = "dedicated"
	SessionIsolationPool      = "profile-pool"

	AuthorityAgentic = "agentic"
	AuthorityHuman   = "human"
	AuthorityShared  = "shared"
)

// BrowserSessionDefaults captures the policy implied by a session kind.
type BrowserSessionDefaults struct {
	Kind           BrowserSessionKind
	Owner          string
	Mode           BrowserMode
	Isolation      string
	AuthorityState string
}

// NormalizeBrowserSessionKind accepts only public, general-purpose kind names.
func NormalizeBrowserSessionKind(raw string, fallback BrowserSessionKind) BrowserSessionKind {
	switch BrowserSessionKind(strings.ToLower(strings.TrimSpace(raw))) {
	case SessionKindTask:
		return SessionKindTask
	case SessionKindInteractive:
		return SessionKindInteractive
	case SessionKindService:
		return SessionKindService
	case SessionKindDebug:
		return SessionKindDebug
	case SessionKindTest:
		return SessionKindTest
	default:
		if fallback != "" {
			return fallback
		}
		return SessionKindTask
	}
}

// DefaultsForBrowserSessionKind is the single policy table for kind-driven
// runtime defaults. Callers may override mode/profile/account explicitly.
func DefaultsForBrowserSessionKind(kind BrowserSessionKind) BrowserSessionDefaults {
	kind = NormalizeBrowserSessionKind(string(kind), SessionKindTask)
	switch kind {
	case SessionKindInteractive:
		return BrowserSessionDefaults{
			Kind:           kind,
			Owner:          SessionOwnerHuman,
			Mode:           ModeHeaded,
			Isolation:      SessionIsolationDedicated,
			AuthorityState: AuthorityHuman,
		}
	case SessionKindService:
		return BrowserSessionDefaults{
			Kind:           kind,
			Owner:          SessionOwnerService,
			Mode:           ModeHeaded,
			Isolation:      SessionIsolationDedicated,
			AuthorityState: AuthorityAgentic,
		}
	case SessionKindDebug:
		return BrowserSessionDefaults{
			Kind:           kind,
			Owner:          SessionOwnerHuman,
			Mode:           ModeVisible,
			Isolation:      SessionIsolationDedicated,
			AuthorityState: AuthorityHuman,
		}
	case SessionKindTest:
		return BrowserSessionDefaults{
			Kind:           kind,
			Owner:          SessionOwnerAgent,
			Mode:           ModeHeadless,
			Isolation:      SessionIsolationEphemeral,
			AuthorityState: AuthorityAgentic,
		}
	default:
		return BrowserSessionDefaults{
			Kind:           SessionKindTask,
			Owner:          SessionOwnerAgent,
			Mode:           ModeHeadless,
			Isolation:      SessionIsolationEphemeral,
			AuthorityState: AuthorityAgentic,
		}
	}
}

// BrowserSessionIDFromSessionID keeps old local session files readable while
// making the public identifier explicit.
func BrowserSessionIDFromSessionID(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	if strings.HasPrefix(sessionID, "browser-session-") {
		return sessionID
	}
	return "browser-session-" + sessionID
}

// BrowserSessionIDFromPoolIdentity returns the stable BrowserSession id used by
// BrowserPool's legacy interactive coordination path for an identity.
func BrowserSessionIDFromPoolIdentity(identityKey IdentityKey) string {
	return BrowserSessionIDFromSessionID("pool-" + sanitizeBrowserRuntimeID(string(identityKey)))
}

func NewBrowserRunID(browserSessionID string, chromePID int) string {
	browserSessionID = strings.TrimSpace(browserSessionID)
	if browserSessionID == "" {
		browserSessionID = "browser-session-unknown"
	}
	return fmt.Sprintf("%s-run-%d-%d", browserSessionID, chromePID, time.Now().UnixNano())
}

func NewBrowserMuxHostID(browserSessionID string, pid int) string {
	browserSessionID = strings.TrimSpace(browserSessionID)
	if browserSessionID == "" {
		browserSessionID = "browser-session-unknown"
	}
	if pid <= 0 {
		pid = os.Getpid()
	}
	return browserSessionID + "-muxhost-" + strconv.Itoa(pid)
}

// BrowserMuxHostIDFromBrowserSessionID returns the stable host key for a
// BrowserSession. New BrowserMuxHost manifests use this stable id so Deepwork
// and CLI clients can reconnect to the same muxhost after a client restart.
func BrowserMuxHostIDFromBrowserSessionID(browserSessionID string) string {
	return GlobalBrowserMuxHostID()
}

func GlobalBrowserMuxHostID() string {
	return "browser-mux-host-global"
}

func BrowserRuntimeIDFromBrowserSessionID(browserSessionID string) string {
	browserSessionID = strings.TrimSpace(browserSessionID)
	if browserSessionID == "" {
		browserSessionID = "browser-session-unknown"
	}
	return "browser-runtime-" + sanitizeBrowserRuntimeID(browserSessionID)
}

func LegacyBrowserMuxHostIDFromBrowserSessionID(browserSessionID string) string {
	browserSessionID = strings.TrimSpace(browserSessionID)
	if browserSessionID == "" {
		browserSessionID = "browser-session-unknown"
	}
	return "browser-mux-host-" + sanitizeBrowserRuntimeID(browserSessionID)
}

func sanitizeBrowserRuntimeID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "unknown"
	}
	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-'
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}

// NormalizeSessionInfo backfills the r8 BrowserSession contract on session
// files loaded from older dw-browser builds.
func NormalizeSessionInfo(info *SessionInfo) {
	if info == nil {
		return
	}
	if strings.TrimSpace(info.SessionID) == "" && strings.TrimSpace(info.BrowserSessionID) != "" {
		info.SessionID = strings.TrimPrefix(info.BrowserSessionID, "browser-session-")
	}
	if strings.TrimSpace(info.BrowserSessionID) == "" {
		info.BrowserSessionID = BrowserSessionIDFromSessionID(info.SessionID)
	}
	info.SessionKind = NormalizeBrowserSessionKind(string(info.SessionKind), SessionKindTask)
	defaults := DefaultsForBrowserSessionKind(info.SessionKind)
	if info.Owner == "" {
		info.Owner = defaults.Owner
	}
	if info.AuthorityState == "" {
		info.AuthorityState = defaults.AuthorityState
	}
	if info.Isolation == "" {
		info.Isolation = defaults.Isolation
	}
	if info.Mode == "" {
		info.Mode = defaults.Mode
	}
	info.Mode = NormalizeBrowserMode(info.Mode, defaults.Mode)
	if info.ProfileID == "" && info.ProfileDir != "" {
		parts := strings.Split(strings.TrimRight(info.ProfileDir, "/"), "/")
		info.ProfileID = parts[len(parts)-1]
	}
	if info.ProfileID == "" {
		info.ProfileID = "default"
	}
	if info.BrowserRunID == "" && info.ChromePID > 0 {
		info.BrowserRunID = NewBrowserRunID(info.BrowserSessionID, info.ChromePID)
	}
	if info.BrowserMuxHostID == "" && info.BrowserMuxHostPID > 0 {
		info.BrowserMuxHostID = GlobalBrowserMuxHostID()
	}
	if strings.HasPrefix(info.BrowserMuxHostID, "browser-mux-host-browser-session-") {
		info.BrowserMuxHostID = GlobalBrowserMuxHostID()
	}
	if info.RuntimeID == "" {
		info.RuntimeID = BrowserRuntimeIDFromBrowserSessionID(info.BrowserSessionID)
	}
}
