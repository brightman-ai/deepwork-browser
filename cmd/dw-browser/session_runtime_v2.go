package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func runSessionViewportResult(args []string) (CommandResult, error) {
	size := ""
	clean := []string{}
	for i := 0; i < len(args); i++ {
		key, val, inline := strings.Cut(args[i], "=")
		if key == "--size" {
			if !inline {
				if i+1 >= len(args) {
					return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("--size requires WxH"))
				}
				i++
				val = args[i]
			}
			size = val
		} else {
			clean = append(clean, args[i])
		}
	}
	w, h, e := parseViewport(size)
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	_, f, e := parseCommonFlagsE(clean, "session viewport")
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	info, e := browser.LoadSession(f.sessionID)
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	if e = browser.SessionAllowsAIInput(info); e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	core, e := connectSessionE(ctx, info, "session viewport", f)
	if e != nil {
		return CommandResult{}, commandError("connect", exitRunErr, e)
	}
	defer closeSessionCore(core)
	lock, e := acquireSessionRuntimeInputLock(info)
	if e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	defer lock.Release()
	if e = browser.SessionAllowsAIInput(info); e != nil {
		return CommandResult{}, commandError("validation", exitRunErr, e)
	}
	syncer, ok := core.(browser.LiveViewportSyncer)
	if !ok {
		return CommandResult{}, commandError("validation", exitRunErr, fmt.Errorf("engine has no live viewport resize"))
	}
	dpr, mobile := 1.0, false
	if preset, ok := browser.BuiltinPresets[browser.NormalizePresetID(info.PresetID)]; ok {
		dpr, mobile = preset.DeviceScaleFactor, preset.Mobile
	}
	if e = syncer.SetLiveViewport(w, h, dpr, mobile); e != nil {
		return CommandResult{}, dispatchCommandError(e)
	}
	info.ViewportW, info.ViewportH = w, h
	browser.InvalidateSessionObservation(info)
	if e = browser.SaveSession(info); e != nil {
		return CommandResult{}, deliveryCommandError(e)
	}
	return commandSuccess(map[string]any{"session_id": info.SessionID, "runtime_run_id": info.BrowserRunID, "viewport": map[string]int{"width": w, "height": h}, "observation_invalidated": true}), nil
}
