package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func runMuxHost(args []string) {
	if len(args) == 1 && wantsHelp(args) {
		printCommandUsage("muxhost")
		os.Exit(exitOK)
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser muxhost: requires subcommand (serve|ensure|status|release|shutdown)")
		os.Exit(exitRunErr)
	}
	switch args[0] {
	case "serve":
		runMuxHostServe(args[1:])
	case "ensure":
		runMuxHostEnsure(args[1:])
	case "status":
		runMuxHostStatus(args[1:])
	case "release":
		runMuxHostRelease(args[1:])
	case "shutdown":
		runMuxHostShutdown(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "dw-browser muxhost: unknown subcommand %q (use serve|ensure|status|release|shutdown)\n", args[0])
		os.Exit(exitRunErr)
	}
}

func runMuxHostServe(args []string) {
	req := parseBrowserMuxHostFlags(args, "muxhost serve")
	if err := browser.ServeBrowserMuxHost(context.Background(), req); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser muxhost serve: %v\n", err)
		os.Exit(exitRunErr)
	}
	os.Exit(exitOK)
}

func runMuxHostEnsure(args []string) {
	req := parseBrowserMuxHostFlags(args, "muxhost ensure")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	state, err := browser.EnsureBrowserMuxHost(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser muxhost ensure: %v\n", err)
		os.Exit(exitRunErr)
	}
	printMuxHostState(state)
	os.Exit(exitOK)
}

func runMuxHostStatus(args []string) {
	muxHostID := parseMuxHostIDArg(args, "muxhost status")
	state, err := browser.LoadBrowserMuxHostState(muxHostID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser muxhost status: %v\n", err)
		os.Exit(exitRunErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if live, err := browser.BrowserMuxHostHealth(ctx, state); err == nil {
		printMuxHostState(live)
	} else {
		printMuxHostState(state)
	}
	os.Exit(exitOK)
}

func runMuxHostRelease(args []string) {
	muxHostID := parseMuxHostIDArg(args, "muxhost release")
	state, err := browser.LoadBrowserMuxHostState(muxHostID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser muxhost release: %v\n", err)
		os.Exit(exitRunErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := browser.ReleaseBrowserMuxHost(ctx, state)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser muxhost release: %v\n", err)
		os.Exit(exitRunErr)
	}
	printMuxHostState(out)
	os.Exit(exitOK)
}

func runMuxHostShutdown(args []string) {
	muxHostID := parseMuxHostIDArg(args, "muxhost shutdown")
	state, err := browser.LoadBrowserMuxHostState(muxHostID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser muxhost shutdown: %v\n", err)
		os.Exit(exitRunErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := browser.ShutdownBrowserMuxHost(ctx, state)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser muxhost shutdown: %v\n", err)
		os.Exit(exitRunErr)
	}
	printMuxHostState(out)
	os.Exit(exitOK)
}

func parseBrowserMuxHostFlags(args []string, cmdName string) browser.BrowserMuxHostRequest {
	req := browser.BrowserMuxHostRequest{
		SessionKind: browser.SessionKindInteractive,
		Mode:        browser.ModeHeaded,
		OwnerPID:    os.Getppid(),
		IdleTTL:     10 * time.Minute,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		next := func() string {
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "dw-browser %s: %s requires a value\n", cmdName, arg)
				os.Exit(exitRunErr)
			}
			i++
			return args[i]
		}
		switch {
		case arg == "--help" || arg == "-h":
			printCommandUsage("muxhost")
			os.Exit(exitOK)
		case arg == "--muxhost-id":
			req.MuxHostID = next()
		case strings.HasPrefix(arg, "--muxhost-id="):
			req.MuxHostID = strings.TrimPrefix(arg, "--muxhost-id=")
		case arg == "--runtime-id" || arg == "--runtime":
			req.RuntimeID = next()
		case strings.HasPrefix(arg, "--runtime-id="):
			req.RuntimeID = strings.TrimPrefix(arg, "--runtime-id=")
		case strings.HasPrefix(arg, "--runtime="):
			req.RuntimeID = strings.TrimPrefix(arg, "--runtime=")
		case arg == "--browser-session-id" || arg == "--id" || arg == "--session":
			req.BrowserSessionID = next()
		case strings.HasPrefix(arg, "--browser-session-id="):
			req.BrowserSessionID = strings.TrimPrefix(arg, "--browser-session-id=")
		case strings.HasPrefix(arg, "--id="):
			req.BrowserSessionID = strings.TrimPrefix(arg, "--id=")
		case strings.HasPrefix(arg, "--session="):
			req.BrowserSessionID = strings.TrimPrefix(arg, "--session=")
		case arg == "--kind":
			kind, ok := parseBrowserSessionKind(next())
			if !ok {
				fmt.Fprintf(os.Stderr, "dw-browser %s: invalid --kind\n", cmdName)
				os.Exit(exitRunErr)
			}
			req.SessionKind = kind
		case strings.HasPrefix(arg, "--kind="):
			kind, ok := parseBrowserSessionKind(strings.TrimPrefix(arg, "--kind="))
			if !ok {
				fmt.Fprintf(os.Stderr, "dw-browser %s: invalid --kind\n", cmdName)
				os.Exit(exitRunErr)
			}
			req.SessionKind = kind
		case arg == "--profile-id" || arg == "--profile":
			req.ProfileID = next()
		case strings.HasPrefix(arg, "--profile-id="):
			req.ProfileID = strings.TrimPrefix(arg, "--profile-id=")
		case strings.HasPrefix(arg, "--profile="):
			req.ProfileID = strings.TrimPrefix(arg, "--profile=")
		case arg == "--profile-dir":
			req.ProfileDir = next()
		case strings.HasPrefix(arg, "--profile-dir="):
			req.ProfileDir = strings.TrimPrefix(arg, "--profile-dir=")
		case arg == "--chrome-path":
			req.ChromePath = next()
		case strings.HasPrefix(arg, "--chrome-path="):
			req.ChromePath = strings.TrimPrefix(arg, "--chrome-path=")
		case arg == "--debug-port":
			req.DebugPort = atoiOrExit(next(), cmdName, "--debug-port")
		case strings.HasPrefix(arg, "--debug-port="):
			req.DebugPort = atoiOrExit(strings.TrimPrefix(arg, "--debug-port="), cmdName, "--debug-port")
		case arg == "--mode":
			mode, ok := parseBrowserMode(next())
			if !ok {
				fmt.Fprintf(os.Stderr, "dw-browser %s: invalid --mode\n", cmdName)
				os.Exit(exitRunErr)
			}
			req.Mode = mode
		case strings.HasPrefix(arg, "--mode="):
			mode, ok := parseBrowserMode(strings.TrimPrefix(arg, "--mode="))
			if !ok {
				fmt.Fprintf(os.Stderr, "dw-browser %s: invalid --mode\n", cmdName)
				os.Exit(exitRunErr)
			}
			req.Mode = mode
		case arg == "--preset":
			req.PresetID = next()
		case strings.HasPrefix(arg, "--preset="):
			req.PresetID = strings.TrimPrefix(arg, "--preset=")
		case arg == "--persona":
			req.PersonaID = next()
		case strings.HasPrefix(arg, "--persona="):
			req.PersonaID = strings.TrimPrefix(arg, "--persona=")
		case arg == "--width":
			req.Width = atoiOrExit(next(), cmdName, "--width")
		case strings.HasPrefix(arg, "--width="):
			req.Width = atoiOrExit(strings.TrimPrefix(arg, "--width="), cmdName, "--width")
		case arg == "--height":
			req.Height = atoiOrExit(next(), cmdName, "--height")
		case strings.HasPrefix(arg, "--height="):
			req.Height = atoiOrExit(strings.TrimPrefix(arg, "--height="), cmdName, "--height")
		case arg == "--user-agent":
			req.UserAgent = next()
		case strings.HasPrefix(arg, "--user-agent="):
			req.UserAgent = strings.TrimPrefix(arg, "--user-agent=")
		case arg == "--touch":
			req.Touch = true
		case arg == "--owner-pid":
			req.OwnerPID = atoiOrExit(next(), cmdName, "--owner-pid")
		case strings.HasPrefix(arg, "--owner-pid="):
			req.OwnerPID = atoiOrExit(strings.TrimPrefix(arg, "--owner-pid="), cmdName, "--owner-pid")
		case arg == "--idle-ttl":
			req.IdleTTL = durationOrExit(next(), cmdName, "--idle-ttl")
		case strings.HasPrefix(arg, "--idle-ttl="):
			req.IdleTTL = durationOrExit(strings.TrimPrefix(arg, "--idle-ttl="), cmdName, "--idle-ttl")
		case arg == "--idle-ttl-ms":
			req.IdleTTL = time.Duration(atoiOrExit(next(), cmdName, "--idle-ttl-ms")) * time.Millisecond
		case strings.HasPrefix(arg, "--idle-ttl-ms="):
			req.IdleTTL = time.Duration(atoiOrExit(strings.TrimPrefix(arg, "--idle-ttl-ms="), cmdName, "--idle-ttl-ms")) * time.Millisecond
		case arg == "--goal":
			req.Goal = next()
		case strings.HasPrefix(arg, "--goal="):
			req.Goal = strings.TrimPrefix(arg, "--goal=")
		case arg == "--owner":
			req.Owner = next()
		case strings.HasPrefix(arg, "--owner="):
			req.Owner = strings.TrimPrefix(arg, "--owner=")
		case arg == "--isolation":
			req.Isolation = next()
		case strings.HasPrefix(arg, "--isolation="):
			req.Isolation = strings.TrimPrefix(arg, "--isolation=")
		case arg == "--service":
			req.ServiceName = next()
		case strings.HasPrefix(arg, "--service="):
			req.ServiceName = strings.TrimPrefix(arg, "--service=")
		case arg == "--account" || arg == "--provider-account":
			req.AccountID = next()
		case strings.HasPrefix(arg, "--account="):
			req.AccountID = strings.TrimPrefix(arg, "--account=")
		case strings.HasPrefix(arg, "--provider-account="):
			req.AccountID = strings.TrimPrefix(arg, "--provider-account=")
		case arg == "--identity-key":
			req.IdentityKey = browser.IdentityKey(next())
		case strings.HasPrefix(arg, "--identity-key="):
			req.IdentityKey = browser.IdentityKey(strings.TrimPrefix(arg, "--identity-key="))
		default:
			fmt.Fprintf(os.Stderr, "dw-browser %s: unknown flag %q\n", cmdName, arg)
			os.Exit(exitRunErr)
		}
	}
	if req.BrowserSessionID == "" {
		req.BrowserSessionID = "browser-session-muxhost-" + strconv.Itoa(os.Getpid())
	}
	if req.ProfileID == "" {
		req.ProfileID = "muxhost-" + strings.TrimPrefix(browser.BrowserSessionIDFromSessionID(req.BrowserSessionID), "browser-session-")
	}
	if req.ProfileDir == "" {
		home, _ := os.UserHomeDir()
		req.ProfileDir = filepath.Join(home, ".deepwork", "browser-cli", req.ProfileID)
	}
	return req
}

func parseMuxHostIDArg(args []string, cmdName string) string {
	muxHostID := ""
	runtimeID := ""
	sessionID := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--help" || arg == "-h":
			printCommandUsage("muxhost")
			os.Exit(exitOK)
		case arg == "--muxhost-id" && i+1 < len(args):
			muxHostID = args[i+1]
			i++
		case strings.HasPrefix(arg, "--muxhost-id="):
			muxHostID = strings.TrimPrefix(arg, "--muxhost-id=")
		case (arg == "--runtime-id" || arg == "--runtime") && i+1 < len(args):
			runtimeID = args[i+1]
			i++
		case strings.HasPrefix(arg, "--runtime-id="):
			runtimeID = strings.TrimPrefix(arg, "--runtime-id=")
		case strings.HasPrefix(arg, "--runtime="):
			runtimeID = strings.TrimPrefix(arg, "--runtime=")
		case (arg == "--id" || arg == "--session" || arg == "--browser-session-id") && i+1 < len(args):
			sessionID = args[i+1]
			i++
		case strings.HasPrefix(arg, "--id="):
			sessionID = strings.TrimPrefix(arg, "--id=")
		case strings.HasPrefix(arg, "--session="):
			sessionID = strings.TrimPrefix(arg, "--session=")
		case strings.HasPrefix(arg, "--browser-session-id="):
			sessionID = strings.TrimPrefix(arg, "--browser-session-id=")
		default:
			fmt.Fprintf(os.Stderr, "dw-browser %s: unknown arg %q\n", cmdName, arg)
			os.Exit(exitRunErr)
		}
	}
	if runtimeID != "" {
		return runtimeID
	}
	if muxHostID != "" {
		return muxHostID
	}
	if sessionID != "" {
		if info, err := browser.LoadSession(sessionID); err == nil && info.RuntimeID != "" {
			return info.RuntimeID
		}
		return browser.BrowserRuntimeIDFromBrowserSessionID(browser.BrowserSessionIDFromSessionID(sessionID))
	}
	fmt.Fprintf(os.Stderr, "dw-browser %s: requires --runtime-id <id>, --muxhost-id <id>, or --id <browser-session-id>\n", cmdName)
	os.Exit(exitRunErr)
	return ""
}

func atoiOrExit(raw string, cmdName string, flagName string) int {
	n, err := strconv.Atoi(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: %s must be an integer\n", cmdName, flagName)
		os.Exit(exitRunErr)
	}
	return n
}

func durationOrExit(raw string, cmdName string, flagName string) time.Duration {
	d, err := time.ParseDuration(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: %s must be a Go duration like 10m or 300ms\n", cmdName, flagName)
		os.Exit(exitRunErr)
	}
	return d
}

func printMuxHostState(state *browser.BrowserMuxHostState) {
	enc, _ := json.MarshalIndent(state, "", "  ")
	fmt.Println(string(enc))
}
