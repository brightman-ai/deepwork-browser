package browser

import (
	"strings"
	"time"
)

const (
	DefaultViewportWidth = 1920
	DefaultViewportHeight = 1080
	DefaultMaxTabs = 10

	// ChromeInitialPageURL is Chrome/CDP's startup sentinel page. It is a
	// runtime state, not product state: callers must not use it to infer user
	// intent, session progress, or tab ownership.
	ChromeInitialPageURL = "about:blank"
)

const (
	BrowserPoolDefaultIdleTimeout = 5 * time.Minute
	BrowserPoolShutdownGrace = 5 * time.Second
	BrowserPoolMaxShutdownGrace = 30 * time.Second
	BrowserPoolMinReapInterval = 15 * time.Second
	BrowserPoolReleaseTimeout = 5 * time.Second
	BrowserPoolChromeWarmup = 15 * time.Second
	BrowserPoolCDPActionTimeout = 5 * time.Second
	ProfileOwnerMuxHostKillGrace = 2 * time.Second
	ProfileOwnerChromeKillGrace = 1 * time.Second
	ProcessExitPollInterval = 50 * time.Millisecond
	ChromeReadyPollInterval = 150 * time.Millisecond
	ChromeVersionRequestTimeout = 2 * time.Second
	ChromeTargetsRequestTimeout = 3 * time.Second
	ChromeCDPStartupAttempts = 20
	ChromeCDPStartupPollInterval = 500 * time.Millisecond

	BrowserMuxHostDefaultIdleTTL = 10 * time.Minute
	BrowserMuxHostReadyTimeout = 45 * time.Second
	BrowserMuxHostLaunchReadyTimeout = 30 * time.Second
	BrowserMuxHostControlRequestTimeout = 5 * time.Second
	BrowserMuxHostShutdownTimeout = 3 * time.Second
	BrowserMuxHostReadyPollInterval = 250 * time.Millisecond
	BrowserMuxHostProcessPollInterval = 100 * time.Millisecond
	BrowserMuxHostHealthPollInterval = 2 * time.Second
	BrowserMuxHostIdleCheckInterval = 5 * time.Second
	BrowserMuxHostWindowEnforceTimeout = 10 * time.Second
	BrowserMuxHostWindowContainmentTimeout = 2500 * time.Millisecond
	BrowserMuxHostForegroundGuardTimeout = 1500 * time.Millisecond
	BrowserMuxHostForegroundContainmentCheck = 750 * time.Millisecond
	BrowserMuxHostSnapshotContainmentCheck = 200 * time.Millisecond
	VirtualDisplayRegistrationDelay = 1 * time.Second
	VirtualDisplayContainmentPollInterval = 75 * time.Millisecond
)

const (
	TargetClaimWindow = 2 * time.Second
	TargetWindowOpenHintTTL = 2 * time.Second
	TargetMaxAttributionHints = 8
	TargetWarmTimeout = 10 * time.Second
	TargetPageListenerTimeout = 5 * time.Second
	TargetActivateTimeout = 2500 * time.Millisecond
	TargetMaterializeTimeout = 1500 * time.Millisecond
	TargetDiscoveryTimeout = 8 * time.Second
	TargetCreateTimeout = 5 * time.Second
	TargetCreateWaitTimeout = 12 * time.Second
	TargetCloseLocalWaitTimeout = 1200 * time.Millisecond
	TargetCloseLocalPollInterval = 25 * time.Millisecond
	TargetBootstrapCleanupWindow = 5 * time.Second
	TargetPostActionDiscoveryDelay = 500 * time.Millisecond
	TargetWindowOpenRefreshDelay = 250 * time.Millisecond
	DevToolsRequestTimeout = 5 * time.Second
)

var TargetBootstrapCleanupDelays = time.Duration{
	350 * time.Millisecond
	1500 * time.Millisecond
	3500 * time.Millisecond
}

func NormalizeTargetCreateURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ChromeInitialPageURL
	}
	return raw
}

func IsChromeInitialPageURL(raw string) bool {
	return strings.EqualFold(strings.TrimSpace(raw), ChromeInitialPageURL)
}

func IsBlankTargetURL(raw string) bool {
	return strings.TrimSpace(raw) == "" || IsChromeInitialPageURL(raw)
}

func IsUserPageTargetURL(raw string) bool {
	return !IsBlankTargetURL(raw)
}

type PageTargetSelection struct {
	ID string
	URL string
	Reason string
}

func ExtractDevToolsTargetID(target map[string]interface{}) string {
	if target == nil {
		return ""
	}
	if wsURL, ok := target["webSocketDebuggerUrl"].(string); ok && wsURL != "" {
		if idx := strings.LastIndexByte(wsURL, '/'); idx >= 0 && idx+1 < len(wsURL) {
			return wsURL[idx+1:]
		}
	}
	if id, ok := target["id"].(string); ok {
		return id
	}
	return ""
}

func ExtractDevToolsTargetURL(target map[string]interface{}) string {
	if target == nil {
		return ""
	}
	url, _ := target["url"].(string)
	return strings.TrimSpace(url)
}

func IsDevToolsPageTarget(target map[string]interface{}) bool {
	if target == nil {
		return false
	}
	targetType, _ := target["type"].(string)
	return targetType == "page"
}

func URLsEquivalent(a, b string) bool {
	trim := func(s string) string {
		s = strings.TrimSpace(s)
		for len(s) > 1 && strings.HasSuffix(s, "/") {
			s = strings.TrimSuffix(s, "/")
		}
		return s
	}
	return trim(a) == trim(b)
}

func URLOrigin(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	for _, prefix := range string{"http://", "https://", "ws://", "wss://"} {
		if after, found := strings.CutPrefix(rawURL, prefix); found {
			if idx := strings.IndexByte(after, '/'); idx >= 0 {
				return prefix + after[:idx]
			}
			return prefix + after
		}
	}
	return ""
}

// SelectAttachablePageTarget chooses the page target that best represents the
// user's current browser state. It never treats ChromeInitialPageURL as user
// intent; that URL is only a Chrome startup sentinel.
func SelectAttachablePageTarget(targets map[string]interface{}, expectedURL, fallbackID string) PageTargetSelection {
	var sameOrigin PageTargetSelection
	var firstUserPage PageTargetSelection
	var fallback PageTargetSelection
	var firstPage PageTargetSelection

	expectedOrigin := URLOrigin(expectedURL)
	for _, target := range targets {
		if !IsDevToolsPageTarget(target) {
			continue
		}
		id := ExtractDevToolsTargetID(target)
		if id == "" {
			continue
		}
		url := ExtractDevToolsTargetURL(target)
		current := PageTargetSelection{ID: id, URL: url}
		if firstPage.ID == "" {
			current.Reason = "first_page"
			firstPage = current
		}
		if fallbackID != "" && id == fallbackID {
			current.Reason = "fallback_target"
			fallback = current
		}
		if expectedURL != "" && URLsEquivalent(url, expectedURL) {
			current.Reason = "exact_url"
			return current
		}
		if IsUserPageTargetURL(url) {
			if firstUserPage.ID == "" {
				current.Reason = "first_user_page"
				firstUserPage = current
			}
			if expectedOrigin != "" && URLOrigin(url) == expectedOrigin && sameOrigin.ID == "" {
				current.Reason = "same_origin"
				sameOrigin = current
			}
		}
	}
	switch {
	case sameOrigin.ID != "":
		return sameOrigin
	case firstUserPage.ID != "":
		return firstUserPage
	case fallback.ID != "":
		return fallback
	case fallbackID != "":
		return PageTargetSelection{ID: fallbackID, Reason: "fallback_input"}
	default:
		return firstPage
	}
}
