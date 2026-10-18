package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
	"github.com/chromedp/cdproto/cdp"
	cdpTarget "github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

const cliTabsCommandTimeout = 10 * time.Second
const cliTabCloseSettleDelay = 250 * time.Millisecond

func loadSessionArgs(args []string, cmd string) (*browser.SessionInfo, error) {
	_, f, e := parseCommonFlagsE(args, cmd)
	if e != nil {
		return nil, e
	}
	if f.sessionID == "" {
		return nil, fmt.Errorf("%s requires --id", cmd)
	}
	return browser.LoadSession(f.sessionID)
}
func sessionRuntimeHeader(info *browser.SessionInfo) map[string]any {
	return map[string]any{"browser_session_id": info.BrowserSessionID, "runtime_run_id": info.BrowserRunID}
}
func runTabsListResult(args []string) (CommandResult, error) {
	info, e := loadSessionArgs(args, "tabs list")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	targets, e := fetchSessionTargets(info)
	if e != nil {
		return CommandResult{}, commandError("observation", exitRunErr, e)
	}
	pages := make([]map[string]interface{}, 0, len(targets))
	for _, target := range targets {
		if browser.IsDevToolsPageTarget(target) {
			pages = append(pages, target)
		}
	}
	out := sessionRuntimeHeader(info)
	out["current_target_id"] = info.TargetID
	out["targets"] = pages
	return commandSuccess(out), nil
}
func parseTargetSelectorArgE(args []string, cmd string) (string, []string, error) {
	selector := ""
	clean := []string{}
	for i := 0; i < len(args); i++ {
		key, val, inline := strings.Cut(args[i], "=")
		if key == "--target" {
			if !inline {
				if i+1 >= len(args) {
					return "", nil, fmt.Errorf("--target requires a value")
				}
				i++
				val = args[i]
			}
			selector = val
		} else {
			clean = append(clean, args[i])
		}
	}
	if selector == "" {
		return "", nil, fmt.Errorf("%s requires --target", cmd)
	}
	return selector, clean, nil
}
func prepareTabsMutation(args []string, cmd string) (*browser.SessionInfo, *browser.BrowserInputLock, context.Context, context.CancelFunc, error) {
	info, e := loadSessionArgs(args, cmd)
	if e != nil {
		return nil, nil, nil, nil, e
	}
	if e = browser.SessionAllowsAIInput(info); e != nil {
		return nil, nil, nil, nil, e
	}
	ctx, cancel := context.WithTimeout(context.Background(), cliTabsCommandTimeout)
	if e = ensureSessionBrowserMuxHostReadyE(ctx, info, cmd); e != nil {
		cancel()
		return nil, nil, nil, nil, e
	}
	lock, e := acquireSessionRuntimeInputLock(info)
	if e != nil {
		cancel()
		return nil, nil, nil, nil, e
	}
	current, loadErr := browser.LoadSession(info.SessionID)
	if loadErr != nil {
		lock.Release()
		cancel()
		return nil, nil, nil, nil, loadErr
	}
	if browser.BrowserInputKey(current.WSURL, current.ChromePID) != browser.BrowserInputKey(info.WSURL, info.ChromePID) {
		lock.Release()
		cancel()
		return nil, nil, nil, nil, fmt.Errorf("runtime changed while waiting for input lock")
	}
	info = current
	if e = browser.SessionAllowsAIInput(info); e != nil {
		lock.Release()
		cancel()
		return nil, nil, nil, nil, e
	}
	return info, lock, ctx, cancel, nil
}
func runTabsSelectResult(args []string) (CommandResult, error) {
	selector, clean, e := parseTargetSelectorArgE(args, "tabs select")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	info, lock, ctx, cancel, e := prepareTabsMutation(clean, "tabs select")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	defer cancel()
	defer lock.Release()
	targets, e := fetchSessionTargets(info)
	if e != nil {
		return CommandResult{}, commandError("observation", exitRunErr, e)
	}
	tid, url := resolveCLITarget(targets, selector)
	if tid == "" {
		return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("target %q missing or ambiguous", selector))
	}
	if e = ensureForegroundAllowedE(ctx, info, "tabs select"); e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	if e = runBrowserLevelTargetCommand(ctx, info, func(c context.Context) error { return cdpTarget.ActivateTarget(cdpTarget.ID(tid)).Do(c) }, "tabs select"); e != nil {
		return CommandResult{}, dispatchCommandError(e)
	}
	info.TargetID, info.PageURL = tid, url
	browser.InvalidateSessionObservation(info)
	if e = browser.SaveSession(info); e != nil {
		return CommandResult{}, deliveryCommandError(e)
	}
	out := sessionRuntimeHeader(info)
	out["selected_target_id"] = tid
	out["url"] = url
	return commandSuccess(out), nil
}
func runTabsCloseResult(args []string) (CommandResult, error) {
	selector, clean, e := parseTargetSelectorArgE(args, "tabs close")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	info, lock, ctx, cancel, e := prepareTabsMutation(clean, "tabs close")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	defer cancel()
	defer lock.Release()
	targets, e := fetchSessionTargets(info)
	if e != nil {
		return CommandResult{}, commandError("observation", exitRunErr, e)
	}
	tid, _ := resolveCLITarget(targets, selector)
	if tid == "" {
		return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("target %q missing or ambiguous", selector))
	}
	if e = ensureForegroundAllowedE(ctx, info, "tabs close"); e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	if e = runBrowserLevelTargetCommand(ctx, info, func(c context.Context) error { return cdpTarget.CloseTarget(cdpTarget.ID(tid)).Do(c) }, "tabs close"); e != nil {
		return CommandResult{}, dispatchCommandError(e)
	}
	if info.TargetID == tid {
		info.TargetID, info.PageURL = "", ""
		browser.InvalidateSessionObservation(info)
		timer := time.NewTimer(cliTabCloseSettleDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return CommandResult{}, dispatchCommandError(ctx.Err())
		}
		targets, e = fetchSessionTargets(info)
		if e != nil {
			_ = browser.SaveSession(info)
			return CommandResult{}, dispatchCommandError(e)
		}
		info.TargetID, info.PageURL = resolveCLITarget(targets, "0")
	}
	if e = browser.SaveSession(info); e != nil {
		return CommandResult{}, deliveryCommandError(e)
	}
	out := sessionRuntimeHeader(info)
	out["closed_target_id"] = tid
	out["current_target_id"] = info.TargetID
	return commandSuccess(out), nil
}
func runTabsNewResult(args []string) (CommandResult, error) {
	url := browser.ChromeInitialPageURL
	clean := []string{}
	for i := 0; i < len(args); i++ {
		key, val, inline := strings.Cut(args[i], "=")
		if key == "--url" {
			if !inline {
				if i+1 >= len(args) {
					return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("--url requires value"))
				}
				i++
				val = args[i]
			}
			url = val
		} else {
			clean = append(clean, args[i])
		}
	}
	if err := browser.ValidateNavigationURL(url); err != nil {
		return CommandResult{}, commandError("validation", exitRunErr, err)
	}
	info, lock, ctx, cancel, e := prepareTabsMutation(clean, "tabs new")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	defer cancel()
	defer lock.Release()
	if e = ensureForegroundAllowedE(ctx, info, "tabs new"); e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	var tid cdpTarget.ID
	if e = runBrowserLevelTargetCommand(ctx, info, func(c context.Context) error { var err error; tid, err = cdpTarget.CreateTarget(url).Do(c); return err }, "tabs new"); e != nil {
		return CommandResult{}, dispatchCommandError(e)
	}
	info.TargetID, info.PageURL = string(tid), url
	browser.InvalidateSessionObservation(info)
	if e = browser.SaveSession(info); e != nil {
		return CommandResult{}, deliveryCommandError(e)
	}
	out := sessionRuntimeHeader(info)
	out["target_id"] = tid
	out["url"] = url
	return commandSuccess(out), nil
}
func runHTRStatusResult(args []string) (CommandResult, error) {
	info, e := loadSessionArgs(args, "htr status")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), cliTabsCommandTimeout)
	defer cancel()
	targets, targetErr := fetchSessionTargets(info)
	var host any
	if info.RuntimeID != "" {
		if state, err := browser.LoadBrowserRuntimeState(info.RuntimeID); err == nil {
			host = state
			if live, err := browser.BrowserMuxHostHealth(ctx, state); err == nil {
				host = live
			}
		}
	}
	out := sessionRuntimeHeader(info)
	out["authority_state"] = info.AuthorityState
	out["owner"] = info.Owner
	out["mode"] = info.Mode
	out["browser_mux_host"] = host
	out["current_target_id"] = info.TargetID
	out["targets"] = targets
	if targetErr != nil {
		out["targets_state"] = "unavailable"
		out["targets_error"] = targetErr.Error()
	} else {
		out["targets_state"] = "captured"
	}
	return commandSuccess(out), nil
}
func runHTRSetAuthorityResult(args []string, authority, cmd string) (CommandResult, error) {
	info, e := loadSessionArgs(args, cmd)
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	lock, e := acquireSessionRuntimeInputLock(info)
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	defer lock.Release()
	if e = browser.SetSessionAuthority(info, authority); e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	if e = browser.SaveSession(info); e != nil {
		return CommandResult{}, deliveryCommandError(e)
	}
	out := sessionRuntimeHeader(info)
	out["authority_state"] = info.AuthorityState
	out["owner"] = info.Owner
	return commandSuccess(out), nil
}
func fetchSessionTargets(info *browser.SessionInfo) ([]map[string]interface{}, error) {
	if info.DebugPort <= 0 {
		return nil, fmt.Errorf("session has no debug port")
	}
	return browser.FetchChromeTargets(info.DebugPort)
}
func resolveCLITarget(targets []map[string]interface{}, selector string) (string, string) {
	if idx, e := strconv.Atoi(selector); e == nil {
		n := 0
		for _, t := range targets {
			if !browser.IsDevToolsPageTarget(t) {
				continue
			}
			if n == idx {
				return browser.ExtractDevToolsTargetID(t), stringFromMap(t, "url")
			}
			n++
		}
		return "", ""
	}
	matchID, matchURL := "", ""
	for _, t := range targets {
		if !browser.IsDevToolsPageTarget(t) {
			continue
		}
		id := browser.ExtractDevToolsTargetID(t)
		if id == selector {
			return id, stringFromMap(t, "url")
		}
		if strings.HasPrefix(id, selector) {
			if matchID != "" {
				return "", ""
			}
			matchID, matchURL = id, stringFromMap(t, "url")
		}
	}
	return matchID, matchURL
}
func runBrowserLevelTargetCommand(ctx context.Context, info *browser.SessionInfo, fn func(context.Context) error, cmd string) error {
	transportCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	transport, e := chromedp.NewBrowser(transportCtx, info.WSURL)
	if e != nil {
		return fmt.Errorf("%s: browser transport: %w", cmd, e)
	}
	return fn(cdp.WithExecutor(transportCtx, transport))
}
func ensureForegroundAllowedE(ctx context.Context, info *browser.SessionInfo, cmd string) error {
	if browser.NormalizeBrowserMode(info.Mode, browser.ModeHeadless) != browser.ModeHeaded {
		return nil
	}
	if info.RuntimeID == "" {
		return fmt.Errorf("%s: runtime identity unavailable for foreground guard", cmd)
	}
	state, e := browser.LoadBrowserRuntimeState(info.RuntimeID)
	if e != nil {
		return fmt.Errorf("BrowserMuxHost state unavailable: %w", e)
	}
	live, e := browser.BrowserMuxHostHealth(ctx, state)
	if e != nil {
		return fmt.Errorf("BrowserMuxHost health failed: %w", e)
	}
	if !live.DisplayVerified || !live.ChromeWindowContained || !live.ChromeAlive {
		return fmt.Errorf("BrowserMuxHost foreground guard failed display_verified=%t chrome_window_contained=%t chrome_alive=%t", live.DisplayVerified, live.ChromeWindowContained, live.ChromeAlive)
	}
	return nil
}

// validateAttachedRuntimeIdentity prevents a stale session file from lending
// an input guard to a different mux-host runtime.
func validateAttachedRuntimeIdentity(info *browser.SessionInfo, state *browser.BrowserMuxHostState) error {
	if info == nil || state == nil {
		return fmt.Errorf("attached runtime identity unavailable")
	}
	for name, value := range map[string]string{
		"runtime_id":          info.RuntimeID,
		"browser_mux_host_id": info.BrowserMuxHostID,
		"ws_url":              info.WSURL,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("attached runtime identity missing %s", name)
		}
	}
	if browser.NormalizeBrowserMode(info.Mode, browser.ModeVisible) == browser.ModeHeaded {
		for name, value := range map[string]string{"runtime_id": info.RuntimeID, "browser_mux_host_id": info.BrowserMuxHostID, "ws_url": info.WSURL} {
			if value == "" {
				return fmt.Errorf("attached runtime identity missing %s", name)
			}
		}
	}
	checks := []struct{ name, expected, actual string }{
		{"runtime_id", info.RuntimeID, state.RuntimeID},
		{"browser_mux_host_id", info.BrowserMuxHostID, state.MuxHostID},
		{"browser_session_id", info.BrowserSessionID, state.BrowserSessionID},
		{"ws_url", info.WSURL, state.WSURL},
		{"browser_run_id", info.BrowserRunID, state.BrowserRunID},
	}
	for _, check := range checks {
		if check.expected != "" && check.expected != check.actual {
			return fmt.Errorf("attached runtime identity mismatch %s", check.name)
		}
	}
	if info.ChromePID != 0 && info.ChromePID != state.ChromePID {
		return fmt.Errorf("attached runtime identity mismatch chrome_pid")
	}
	if info.DisplayID != 0 && info.DisplayID != state.DisplayID {
		return fmt.Errorf("attached runtime identity mismatch display_id")
	}
	if info.Mode != "" && browser.NormalizeBrowserMode(info.Mode, browser.ModeVisible) != browser.NormalizeBrowserMode(state.Mode, browser.ModeVisible) {
		return fmt.Errorf("attached runtime identity mismatch mode")
	}
	if browser.NormalizeBrowserMode(info.Mode, browser.ModeVisible) == browser.ModeHeaded && browser.NormalizeBrowserMode(state.Mode, browser.ModeVisible) != browser.ModeHeaded {
		return fmt.Errorf("attached runtime identity mismatch mode")
	}
	return nil
}
func acquireSessionRuntimeInputLock(info *browser.SessionInfo) (*browser.BrowserInputLock, error) {
	return browser.AcquireBrowserInputLock(browser.BrowserInputKey(info.WSURL, info.ChromePID), browser.DefaultBrowserInputLockWait)
}
func stringFromMap(m map[string]interface{}, key string) string { s, _ := m[key].(string); return s }
