package browser

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
)

// SiteDataInfo describes storage tied to the active page origin.
// It is intentionally origin-scoped so Browser Portal can clear the current
// site's state without touching other profiles or unrelated domains.
type SiteDataInfo struct {
	URL                string `json:"url"`
	Origin             string `json:"origin"`
	Host               string `json:"host"`
	Protocol           string `json:"protocol"`
	Secure             bool   `json:"secure"`
	CookieCount        int    `json:"cookie_count"`
	CookieBytes        int64  `json:"cookie_bytes"`
	LocalStorageKeys   int    `json:"local_storage_keys"`
	SessionStorageKeys int    `json:"session_storage_keys"`
	Clearable          bool   `json:"clearable"`
	UnsupportedReason  string `json:"unsupported_reason,omitempty"`
}

type SiteDataClearResult struct {
	Origin       string `json:"origin"`
	Cleared      bool   `json:"cleared"`
	StorageTypes string `json:"storage_types"`
}

// SiteData returns cookie/storage summary for the active target's origin.
func (impl *browserCoreImpl) SiteData(ctx context.Context) (*SiteDataInfo, error) {
	runCtx, cancel := impl.activeRunContext(ctx, 8*time.Second)
	defer cancel()

	var info SiteDataInfo
	script := `(() => {
	  const loc = window.location;
	  let localKeys = 0;
	  let sessionKeys = 0;
	  try { localKeys = window.localStorage ? window.localStorage.length : 0; } catch (e) {}
	  try { sessionKeys = window.sessionStorage ? window.sessionStorage.length : 0; } catch (e) {}
	  return {
	    url: loc.href || '',
	    origin: loc.origin || '',
	    host: loc.host || '',
	    protocol: loc.protocol || '',
	    secure: loc.protocol === 'https:',
	    local_storage_keys: localKeys,
	    session_storage_keys: sessionKeys
	  };
	})()`
	if err := chromedp.Run(runCtx, chromedp.Evaluate(script, &info)); err != nil {
		return nil, fmt.Errorf("site data inspect origin: %w", err)
	}
	if !isClearableOrigin(info.Origin) {
		info.Clearable = false
		info.UnsupportedReason = "current page has no clearable web origin"
		return &info, nil
	}

	cookies, err := network.GetCookies().WithURLs([]string{info.URL}).Do(runCtx)
	if err != nil {
		return nil, fmt.Errorf("site data inspect cookies: %w", err)
	}
	info.CookieCount = len(cookies)
	for _, cookie := range cookies {
		if cookie != nil {
			info.CookieBytes += cookie.Size
		}
	}
	info.Clearable = true
	return &info, nil
}

// ClearSiteDataForOrigin clears all CDP-supported storage for the supplied
// origin. The caller must pass the active origin returned by SiteData; this
// method refuses non-origin values to avoid accidental global cleanup.
func (impl *browserCoreImpl) ClearSiteDataForOrigin(ctx context.Context, origin string) (*SiteDataClearResult, error) {
	origin = strings.TrimSpace(origin)
	if !isClearableOrigin(origin) {
		return nil, fmt.Errorf("site data clear: invalid origin %q", origin)
	}
	runCtx, cancel := impl.activeRunContext(ctx, 12*time.Second)
	defer cancel()
	const storageTypes = "all"
	if err := storage.ClearDataForOrigin(origin, storageTypes).Do(runCtx); err != nil {
		return nil, fmt.Errorf("site data clear origin %s: %w", origin, err)
	}
	return &SiteDataClearResult{Origin: origin, Cleared: true, StorageTypes: storageTypes}, nil
}

func (impl *browserCoreImpl) activeRunContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	impl.mu.RLock()
	targetCtx := impl.currentCtx()
	impl.mu.RUnlock()
	runCtx, cancelTarget := deriveTargetContext(parent, targetCtx)
	if timeout <= 0 {
		return runCtx, cancelTarget
	}
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) < timeout {
		return runCtx, cancelTarget
	}
	timeoutCtx, cancelTimeout := context.WithTimeout(runCtx, timeout)
	return timeoutCtx, func() {
		cancelTimeout()
		cancelTarget()
	}
}

func isClearableOrigin(origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" || origin == "null" {
		return false
	}
	return strings.HasPrefix(origin, "https://") || strings.HasPrefix(origin, "http://")
}
