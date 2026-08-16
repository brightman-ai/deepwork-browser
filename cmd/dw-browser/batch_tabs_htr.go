package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
	"github.com/chromedp/cdproto/cdp"
	cdpTarget "github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

const (
	cliTabsCommandTimeout  = 10 * time.Second
	cliTabCloseSettleDelay = 250 * time.Millisecond
)

func runTabs(args []string) {
	if len(args) == 1 && wantsHelp(args) {
		printCommandUsage("tabs")
		os.Exit(exitOK)
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser tabs: requires subcommand (list|select|close|new)")
		os.Exit(exitRunErr)
	}
	switch args[0] {
	case "list":
		runTabsList(args[1:])
	case "select":
		runTabsSelect(args[1:])
	case "close":
		runTabsClose(args[1:])
	case "new":
		runTabsNew(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "dw-browser tabs: unknown subcommand %q (use list|select|close|new)\n", args[0])
		os.Exit(exitRunErr)
	}
}

func runTabsList(args []string) {
	_, flags := parseCommonFlags(args, "tabs list")
	sessionInfo := mustLoadSession(flags, "tabs list")
	ctx, cancel := context.WithTimeout(context.Background(), cliTabsCommandTimeout)
	defer cancel()
	ensureSessionBrowserMuxHostReady(ctx, sessionInfo, "tabs list")
	targets := fetchSessionTargetsOrExit(sessionInfo, "tabs list")
	printJSON(map[string]interface{}{
		"browser_session_id": sessionInfo.BrowserSessionID,
		"current_target_id":  sessionInfo.TargetID,
		"targets":            targets,
	})
}

func runTabsSelect(args []string) {
	targetSelector, clean := parseTargetSelectorArg(args, "tabs select")
	_, flags := parseCommonFlags(clean, "tabs select")
	sessionInfo := mustLoadSession(flags, "tabs select")
	ctx, cancel := context.WithTimeout(context.Background(), cliTabsCommandTimeout)
	defer cancel()
	ensureSessionBrowserMuxHostReady(ctx, sessionInfo, "tabs select")
	targets := fetchSessionTargetsOrExit(sessionInfo, "tabs select")
	targetID, targetURL := resolveCLITarget(targets, targetSelector)
	if targetID == "" {
		fmt.Fprintf(os.Stderr, "dw-browser tabs select: target %q not found\n", targetSelector)
		os.Exit(exitRunErr)
	}
	ensureForegroundAllowed(ctx, sessionInfo, "tabs select")
	runBrowserLevelTargetCommand(ctx, sessionInfo.WSURL, func(ctx context.Context) error {
		return cdpTarget.ActivateTarget(cdpTarget.ID(targetID)).Do(ctx)
	}, "tabs select")
	sessionInfo.TargetID = targetID
	sessionInfo.PageURL = targetURL
	sessionInfo.Refs = nil
	_ = browser.SaveSession(sessionInfo)
	printJSON(map[string]interface{}{
		"browser_session_id": sessionInfo.BrowserSessionID,
		"selected_target_id": targetID,
		"url":                targetURL,
	})
}

func runTabsClose(args []string) {
	targetSelector, clean := parseTargetSelectorArg(args, "tabs close")
	_, flags := parseCommonFlags(clean, "tabs close")
	sessionInfo := mustLoadSession(flags, "tabs close")
	ctx, cancel := context.WithTimeout(context.Background(), cliTabsCommandTimeout)
	defer cancel()
	ensureSessionBrowserMuxHostReady(ctx, sessionInfo, "tabs close")
	targets := fetchSessionTargetsOrExit(sessionInfo, "tabs close")
	targetID, _ := resolveCLITarget(targets, targetSelector)
	if targetID == "" {
		fmt.Fprintf(os.Stderr, "dw-browser tabs close: target %q not found\n", targetSelector)
		os.Exit(exitRunErr)
	}
	runBrowserLevelTargetCommand(ctx, sessionInfo.WSURL, func(ctx context.Context) error {
		return cdpTarget.CloseTarget(cdpTarget.ID(targetID)).Do(ctx)
	}, "tabs close")
	if sessionInfo.TargetID == targetID {
		sessionInfo.TargetID = ""
		sessionInfo.Refs = nil
		time.Sleep(cliTabCloseSettleDelay)
		if targets, err := browser.FetchChromeTargets(sessionInfo.DebugPort); err == nil {
			nextID, nextURL := resolveCLITarget(targets, "0")
			sessionInfo.TargetID = nextID
			sessionInfo.PageURL = nextURL
		}
	}
	_ = browser.SaveSession(sessionInfo)
	printJSON(map[string]interface{}{
		"browser_session_id": sessionInfo.BrowserSessionID,
		"closed_target_id":   targetID,
		"current_target_id":  sessionInfo.TargetID,
	})
}

func runTabsNew(args []string) {
	url := browser.ChromeInitialPageURL
	var clean []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--url" && i+1 < len(args):
			url = args[i+1]
			i++
		case strings.HasPrefix(arg, "--url="):
			url = strings.TrimPrefix(arg, "--url=")
		default:
			clean = append(clean, arg)
		}
	}
	_, flags := parseCommonFlags(clean, "tabs new")
	sessionInfo := mustLoadSession(flags, "tabs new")
	ctx, cancel := context.WithTimeout(context.Background(), cliTabsCommandTimeout)
	defer cancel()
	ensureSessionBrowserMuxHostReady(ctx, sessionInfo, "tabs new")
	ensureForegroundAllowed(ctx, sessionInfo, "tabs new")
	var targetID cdpTarget.ID
	runBrowserLevelTargetCommand(ctx, sessionInfo.WSURL, func(ctx context.Context) error {
		var err error
		targetID, err = cdpTarget.CreateTarget(url).Do(ctx)
		return err
	}, "tabs new")
	sessionInfo.TargetID = string(targetID)
	sessionInfo.PageURL = url
	sessionInfo.Refs = nil
	_ = browser.SaveSession(sessionInfo)
	printJSON(map[string]interface{}{
		"browser_session_id": sessionInfo.BrowserSessionID,
		"target_id":          targetID,
		"url":                url,
	})
}

func runHTR(args []string) {
	if len(args) == 1 && wantsHelp(args) {
		printCommandUsage("htr")
		os.Exit(exitOK)
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser htr: requires subcommand (attach|status|takeover|yield|share)")
		os.Exit(exitRunErr)
	}
	switch args[0] {
	case "attach", "status":
		runHTRAttach(args[1:])
	case "takeover", "acquire":
		runHTRSetAuthority(args[1:], browser.AuthorityHuman, "htr takeover")
	case "yield", "release":
		runHTRSetAuthority(args[1:], browser.AuthorityAgentic, "htr yield")
	case "share":
		runHTRSetAuthority(args[1:], browser.AuthorityShared, "htr share")
	default:
		fmt.Fprintf(os.Stderr, "dw-browser htr: unknown subcommand %q (use attach|status|takeover|yield|share)\n", args[0])
		os.Exit(exitRunErr)
	}
}

func runHTRAttach(args []string) {
	_, flags := parseCommonFlags(args, "htr attach")
	sessionInfo := mustLoadSession(flags, "htr attach")
	ctx, cancel := context.WithTimeout(context.Background(), cliTabsCommandTimeout)
	defer cancel()
	ensureSessionBrowserMuxHostReady(ctx, sessionInfo, "htr attach")
	targets := []map[string]interface{}{}
	if sessionInfo.DebugPort > 0 {
		targets, _ = browser.FetchChromeTargets(sessionInfo.DebugPort)
	}
	var host interface{} = nil
	if sessionInfo.RuntimeID == "" && sessionInfo.BrowserSessionID != "" {
		sessionInfo.RuntimeID = browser.BrowserRuntimeIDFromBrowserSessionID(sessionInfo.BrowserSessionID)
	}
	if sessionInfo.RuntimeID != "" {
		if state, err := browser.LoadBrowserRuntimeState(sessionInfo.RuntimeID); err == nil {
			if live, healthErr := browser.BrowserMuxHostHealth(ctx, state); healthErr == nil {
				host = live
			} else {
				host = state
			}
		}
	}
	printJSON(map[string]interface{}{
		"browser_session_id": sessionInfo.BrowserSessionID,
		"authority_state":    sessionInfo.AuthorityState,
		"owner":              sessionInfo.Owner,
		"mode":               sessionInfo.Mode,
		"browser_mux_host":   host,
		"current_target_id":  sessionInfo.TargetID,
		"targets":            targets,
	})
}

func runHTRSetAuthority(args []string, authority string, cmdName string) {
	_, flags := parseCommonFlags(args, cmdName)
	sessionInfo := mustLoadSession(flags, cmdName)
	sessionInfo.AuthorityState = authority
	if authority == browser.AuthorityHuman {
		sessionInfo.Owner = browser.SessionOwnerHuman
	}
	if err := browser.SaveSession(sessionInfo); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: save session: %v\n", cmdName, err)
		os.Exit(exitRunErr)
	}
	printJSON(map[string]interface{}{
		"browser_session_id": sessionInfo.BrowserSessionID,
		"authority_state":    sessionInfo.AuthorityState,
		"owner":              sessionInfo.Owner,
	})
}

func parseTargetSelectorArg(args []string, cmdName string) (string, []string) {
	targetSelector := ""
	var clean []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case (arg == "--target" || arg == "--target-id") && i+1 < len(args):
			targetSelector = args[i+1]
			i++
		case strings.HasPrefix(arg, "--target="):
			targetSelector = strings.TrimPrefix(arg, "--target=")
		case strings.HasPrefix(arg, "--target-id="):
			targetSelector = strings.TrimPrefix(arg, "--target-id=")
		default:
			clean = append(clean, arg)
		}
	}
	if targetSelector == "" {
		fmt.Fprintf(os.Stderr, "dw-browser %s: requires --target <id-or-index>\n", cmdName)
		os.Exit(exitRunErr)
	}
	return targetSelector, clean
}

func mustLoadSession(flags commonFlags, cmdName string) *browser.SessionInfo {
	if flags.sessionID == "" {
		fmt.Fprintf(os.Stderr, "dw-browser %s: requires --id <id>\n", cmdName)
		os.Exit(exitRunErr)
	}
	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: %v\n", cmdName, err)
		os.Exit(exitRunErr)
	}
	return sessionInfo
}

func fetchSessionTargetsOrExit(sessionInfo *browser.SessionInfo, cmdName string) []map[string]interface{} {
	if sessionInfo.DebugPort <= 0 {
		fmt.Fprintf(os.Stderr, "dw-browser %s: session has no debug port\n", cmdName)
		os.Exit(exitRunErr)
	}
	targets, err := browser.FetchChromeTargets(sessionInfo.DebugPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: fetch targets: %v\n", cmdName, err)
		os.Exit(exitRunErr)
	}
	return targets
}

func resolveCLITarget(targets []map[string]interface{}, selector string) (targetID string, targetURL string) {
	if idx, err := strconv.Atoi(selector); err == nil {
		pageIndex := 0
		for _, target := range targets {
			if !browser.IsDevToolsPageTarget(target) {
				continue
			}
			if pageIndex == idx {
				return browser.ExtractDevToolsTargetID(target), stringFromMap(target, "url")
			}
			pageIndex++
		}
	}
	for _, target := range targets {
		id := browser.ExtractDevToolsTargetID(target)
		if id == selector || strings.HasPrefix(id, selector) {
			return id, stringFromMap(target, "url")
		}
	}
	return "", ""
}

func runBrowserLevelTargetCommand(ctx context.Context, wsURL string, fn func(context.Context) error, cmdName string) {
	allocCtx, allocCancel := chromedp.NewRemoteAllocator(ctx, wsURL)
	defer allocCancel()
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()
	if err := chromedp.Run(browserCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return fn(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
	})); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: target command failed: %v\n", cmdName, err)
		os.Exit(exitFail)
	}
	_ = ctx
}

func ensureForegroundAllowed(ctx context.Context, sessionInfo *browser.SessionInfo, cmdName string) {
	if sessionInfo.RuntimeID == "" && sessionInfo.BrowserSessionID != "" {
		sessionInfo.RuntimeID = browser.BrowserRuntimeIDFromBrowserSessionID(sessionInfo.BrowserSessionID)
	}
	if sessionInfo.RuntimeID == "" {
		return
	}
	state, err := browser.LoadBrowserRuntimeState(sessionInfo.RuntimeID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: BrowserMuxHost state unavailable: %v\n", cmdName, err)
		os.Exit(exitRunErr)
	}
	live, err := browser.BrowserMuxHostHealth(ctx, state)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: BrowserMuxHost health failed: %v\n", cmdName, err)
		os.Exit(exitRunErr)
	}
	if !live.DisplayVerified || !live.ChromeWindowContained || !live.ChromeAlive {
		fmt.Fprintf(os.Stderr, "dw-browser %s: BrowserMuxHost foreground guard failed display_verified=%t chrome_window_contained=%t chrome_alive=%t\n",
			cmdName, live.DisplayVerified, live.ChromeWindowContained, live.ChromeAlive)
		os.Exit(exitFail)
	}
}

func stringFromMap(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

func printJSON(value interface{}) {
	enc, _ := json.MarshalIndent(value, "", "  ")
	fmt.Println(string(enc))
}
