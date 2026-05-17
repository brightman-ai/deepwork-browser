package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/chromedp/cdproto/target"
)

// targetGraphPageFilter is the Browser Portal target boundary: user-visible
// tabs are CDP "page" targets. Browser/tab/meta targets are runtime plumbing.
func targetGraphPageFilter target.Filter {
	return target.Filter{{Type: "page"}}
}

func targetGraphListPages(ctx context.Context, timeout time.Duration) (*target.Info, error) {
	var infos *target.Info
	err := runBrowserCDPWithSoftTimeout(ctx, timeout, func(execCtx context.Context) error {
		var err error
		infos, err = target.GetTargets.WithFilter(targetGraphPageFilter).Do(execCtx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return infos, nil
}

func targetGraphGetPageInfo(ctx context.Context, targetID target.ID, timeout time.Duration) (*target.Info, error) {
	var info *target.Info
	err := runBrowserCDPWithSoftTimeout(ctx, timeout, func(execCtx context.Context) error {
		var err error
		info, err = target.GetTargetInfo.WithTargetID(targetID).Do(execCtx)
		return err
	})
	if err != nil {
		return nil, err
	}
	if info == nil || info.Type != "page" {
		return nil, nil
	}
	return info, nil
}

func targetGraphCreatePage(ctx context.Context, url string, timeout time.Duration) (target.ID, error) {
	var targetID target.ID
	err := runBrowserCDPWithSoftTimeout(ctx, timeout, func(execCtx context.Context) error {
		var err error
		targetID, err = target.CreateTarget(url).Do(execCtx)
		return err
	})
	if err != nil {
		return "", err
	}
	return targetID, nil
}

func targetGraphClose(ctx context.Context, targetID target.ID, timeout time.Duration) error {
	return runBrowserCDPWithSoftTimeout(ctx, timeout, func(execCtx context.Context) error {
		return target.CloseTarget(targetID).Do(execCtx)
	})
}

func targetGraphDevToolsBaseURL(wsURL string) (string, error) {
	parsed, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("parse devtools ws url: %w", err)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("devtools ws url missing host")
	}
	scheme := "http"
	if parsed.Scheme == "wss" {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: parsed.Host}).String, nil
}

func targetGraphCloseViaDevTools(wsURL string, targetID target.ID) error {
	baseURL, err := targetGraphDevToolsBaseURL(wsURL)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: DevToolsRequestTimeout}
	resp, err := client.Get(baseURL + "/json/close/" + url.PathEscape(string(targetID)))
	if err != nil {
		return fmt.Errorf("devtools close target %s: %w", targetID, err)
	}
	defer resp.Body.Close
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("devtools close target %s: HTTP %d", targetID, resp.StatusCode)
	}
	return nil
}

func targetGraphActivate(ctx context.Context, targetID target.ID, timeout time.Duration) error {
	return runBrowserCDPWithSoftTimeout(ctx, timeout, func(execCtx context.Context) error {
		return target.ActivateTarget(targetID).Do(execCtx)
	})
}

func targetGraphEnableDiscovery(ctx context.Context, timeout time.Duration) error {
	return runBrowserCDPWithSoftTimeout(ctx, timeout, func(execCtx context.Context) error {
		if err := target.SetDiscoverTargets(true).WithFilter(targetGraphPageFilter).Do(execCtx); err != nil {
			return err
		}
		return target.SetAutoAttach(true, false).WithFlatten(true).WithFilter(targetGraphPageFilter).Do(execCtx)
	})
}
