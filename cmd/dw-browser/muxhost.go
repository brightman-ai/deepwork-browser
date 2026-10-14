package main

import (
	"context"
	"fmt"
	"github.com/brightman-ai/deepwork-browser/browser"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func parseBrowserMuxHostFlagsE(args []string, cmd string) (browser.BrowserMuxHostRequest, error) {
	opts, e := parseDeclaredOptions(args, cmd)
	if e != nil {
		return browser.BrowserMuxHostRequest{}, e
	}
	req := browser.BrowserMuxHostRequest{SessionKind: browser.SessionKindInteractive, Mode: browser.ModeHeaded, OwnerPID: os.Getppid(), IdleTTL: 10 * time.Minute}
	req.MuxHostID, req.RuntimeID, req.BrowserSessionID = opts["--muxhost-id"], opts["--runtime-id"], opts["--browser-session-id"]
	req.ProfileID, req.ProfileDir, req.ChromePath = opts["--profile-id"], opts["--profile-dir"], opts["--chrome-path"]
	req.PresetID, req.PersonaID, req.UserAgent = opts["--preset"], opts["--persona"], opts["--user-agent"]
	req.Goal, req.Owner, req.Isolation = opts["--goal"], opts["--owner"], opts["--isolation"]
	req.ServiceName, req.AccountID = opts["--service"], opts["--account"]
	req.IdentityKey = browser.IdentityKey(opts["--identity-key"])
	req.Touch = opts["--touch"] != ""
	if raw := opts["--kind"]; raw != "" {
		kind, ok := parseBrowserSessionKind(raw)
		if !ok {
			return req, fmt.Errorf("invalid infrastructure --kind")
		}
		req.SessionKind = kind
	}
	if raw := opts["--mode"]; raw != "" {
		mode, ok := parseBrowserMode(raw)
		if !ok {
			return req, fmt.Errorf("invalid --mode")
		}
		req.Mode = mode
	}
	for _, field := range []struct {
		name string
		dest *int
	}{{"--width", &req.Width}, {"--height", &req.Height}, {"--debug-port", &req.DebugPort}, {"--owner-pid", &req.OwnerPID}} {
		if raw := opts[field.name]; raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil {
				return req, err
			}
			*field.dest = n
		}
	}
	if raw := opts["--idle-ttl"]; raw != "" {
		req.IdleTTL, e = time.ParseDuration(raw)
		if e != nil {
			return req, e
		}
	}
	if req.BrowserSessionID == "" {
		req.BrowserSessionID = "browser-session-muxhost-" + strconv.Itoa(os.Getpid())
	}
	if req.ProfileID == "" {
		req.ProfileID = "muxhost-" + strings.TrimPrefix(browser.BrowserSessionIDFromSessionID(req.BrowserSessionID), "browser-session-")
	}
	if req.ProfileDir == "" {
		home, e := os.UserHomeDir()
		if e != nil {
			return req, e
		}
		req.ProfileDir = filepath.Join(home, ".deepwork", "browser-cli", req.ProfileID)
	}
	return req, nil
}
func runMuxHostServeResult(args []string) (CommandResult, error) {
	req, e := parseBrowserMuxHostFlagsE(args, "muxhost serve")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	if e = browser.ServeBrowserMuxHost(context.Background(), req); e != nil {
		return CommandResult{}, commandError("runtime", exitRunErr, e)
	}
	return commandSuccess(map[string]any{"stopped": true}), nil
}
func runMuxHostEnsureResult(args []string) (CommandResult, error) {
	req, e := parseBrowserMuxHostFlagsE(args, "muxhost ensure")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	state, e := browser.EnsureBrowserMuxHost(ctx, req)
	if e != nil {
		return CommandResult{}, commandError("runtime", exitRunErr, e)
	}
	return commandSuccess(state), nil
}
func parseMuxHostIDArgE(args []string, cmd string) (string, error) {
	opts, e := parseDeclaredOptions(args, cmd)
	if e != nil {
		return "", e
	}
	if id := opts["--muxhost-id"]; id != "" {
		return id, nil
	}
	runtimeID := opts["--runtime-id"]
	if sid := opts["--browser-session-id"]; sid != "" {
		runtimeID = browser.BrowserRuntimeIDFromBrowserSessionID(browser.BrowserSessionIDFromSessionID(sid))
	}
	state, e := browser.LoadBrowserRuntimeState(runtimeID)
	if e != nil {
		return "", e
	}
	return state.MuxHostID, nil
}
func runMuxHostStatusResult(args []string) (CommandResult, error) {
	id, e := parseMuxHostIDArgE(args, "muxhost status")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	state, e := browser.LoadBrowserMuxHostState(id)
	if e != nil {
		return CommandResult{}, commandError("observation", exitRunErr, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	live, e := browser.BrowserMuxHostHealth(ctx, state)
	if e != nil {
		return commandSuccess(map[string]any{"state": state, "health": "unavailable", "health_error": e.Error()}), nil
	}
	return commandSuccess(live), nil
}
func runMuxHostReleaseResult(args []string) (CommandResult, error) {
	id, e := parseMuxHostIDArgE(args, "muxhost release")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	state, e := browser.LoadBrowserMuxHostState(id)
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, e := browser.ReleaseBrowserMuxHost(ctx, state)
	if e != nil {
		return CommandResult{}, dispatchCommandError(e)
	}
	return commandSuccess(out), nil
}
func runMuxHostShutdownResult(args []string) (CommandResult, error) {
	id, e := parseMuxHostIDArgE(args, "muxhost shutdown")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	state, e := browser.LoadBrowserMuxHostState(id)
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, e := browser.ShutdownBrowserMuxHost(ctx, state)
	if e != nil {
		return CommandResult{}, dispatchCommandError(e)
	}
	return commandSuccess(out), nil
}
