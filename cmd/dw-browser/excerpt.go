package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// runExcerptResult — 观察级只读摘录：按 spec 从已打开页面裁出带红框+荧光
// 高亮的段落 PNG。
//
//	dw-browser excerpt --id X --spec '<json>|@file|-' [--out DIR] [--settle 500ms]
//
// 一次 JS 采集（只读，Range.getClientRects 取矩形）+ 一次全页 PNG 截图 +
// Go 离线合成。不改 DOM、不需要写权限、不作废观测（批 20 张不用重 observe）。
// missing（未命中的 target/短语）是业务结果：显式进 JSON、exit 0；页面级
// 故障（连不上/截图失败/解析失败）才 error exit。
func runExcerptResult(args []string) (commandOutput CommandResult, resultErr error) {
	var specSrc, outDir, settleRaw string
	clean := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--spec" && i+1 < len(args):
			specSrc = args[i+1]
			i++
		case strings.HasPrefix(arg, "--spec="):
			specSrc = arg[len("--spec="):]
		case arg == "--out" && i+1 < len(args):
			outDir = args[i+1]
			i++
		case strings.HasPrefix(arg, "--out="):
			outDir = arg[len("--out="):]
		case arg == "--settle" && i+1 < len(args):
			settleRaw = args[i+1]
			i++
		case strings.HasPrefix(arg, "--settle="):
			settleRaw = arg[len("--settle="):]
		default:
			clean = append(clean, arg)
		}
	}
	settle := 500 * time.Millisecond
	if settleRaw != "" {
		d, err := time.ParseDuration(settleRaw)
		if err != nil || d < 0 || d > 30*time.Second {
			return commandOutput, commandError("validation", exitRunErr, fmt.Errorf("dw-browser excerpt: invalid --settle %q (want duration 0–30s)", settleRaw))
		}
		settle = d
	}
	if err := validateReadSessionArgsE(clean, "excerpt"); err != nil {
		return commandOutput, commandError("validation", exitRunErr, err)
	}
	_, flags, parseErr := parseCommonFlagsE(clean, "excerpt")
	if parseErr != nil {
		return commandOutput, commandError("validation", exitRunErr, parseErr)
	}
	if flags.sessionID == "" {
		return commandOutput, commandError("validation", exitRunErr, fmt.Errorf("%s", "dw-browser excerpt: requires --id <session-id>"))
	}
	if specSrc == "" {
		return commandOutput, commandError("validation", exitRunErr, fmt.Errorf("%s", "dw-browser excerpt: requires --spec <json|@file|->"))
	}
	specs, err := loadExcerptSpec(specSrc)
	if err != nil {
		return commandOutput, commandError("validation", exitRunErr, err)
	}

	sessionInfo, err := browser.LoadSession(flags.sessionID)
	if err != nil {
		return commandOutput, commandError("observation", exitRunErr, fmt.Errorf("dw-browser excerpt: %v\n", err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	impl, connectErr := connectSessionE(ctx, sessionInfo, "excerpt", flags)
	if connectErr != nil {
		return commandOutput, commandError("attachment", exitRunErr, connectErr)
	}
	defer closeSessionCore(impl)

	capable, ok := impl.(browser.ExcerptCapable)
	if !ok {
		return commandOutput, commandError("validation", exitRunErr, fmt.Errorf("%s", "dw-browser excerpt: this runtime core does not support excerpt (requires chromium engine)"))
	}

	if settle > 0 {
		time.Sleep(settle)
	}

	collected, err := browser.RunExcerptCollector(ctx, impl, specs)
	if err != nil {
		return commandOutput, commandError("observation", exitRunErr, err)
	}
	fullPage, err := capable.CaptureFullPagePNG(ctx)
	if err != nil {
		return commandOutput, commandError("observation", exitRunErr, err)
	}

	if outDir == "" {
		if sessionInfo.EvidenceRoot != "" {
			outDir = filepath.Join(sessionInfo.EvidenceRoot, "excerpts")
		} else {
			outDir = filepath.Join("evidence", "excerpts", flags.sessionID)
		}
	}
	outDir, err = filepath.Abs(outDir)
	if err != nil {
		return commandOutput, commandError("delivery", exitRunErr, err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return commandOutput, commandError("delivery", exitRunErr, err)
	}

	type resultEntry struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Width   int    `json:"width,omitempty"`
		Height  int    `json:"height,omitempty"`
		Missing struct {
			Targets    []string `json:"targets"`
			Highlights []string `json:"highlights"`
		} `json:"missing"`
		Warnings []string `json:"warnings"`
	}
	ensureSlice := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	results := make([]resultEntry, 0, len(collected.Items))
	for i, item := range collected.Items {
		entry := resultEntry{
			Name:     item.Name,
			Warnings: []string{},
		}
		entry.Missing.Targets = ensureSlice(item.MissingTargets)
		entry.Missing.Highlights = ensureSlice(item.MissingHighlights)
		spec := specs[i]
		style := browser.DefaultExcerptStyle()
		style.DPR = collected.DPR
		if spec.Box != nil {
			if spec.Box.Color != "" {
				c, err := browser.ParseHexColor(spec.Box.Color)
				if err != nil {
					return commandOutput, commandError("validation", exitRunErr, err)
				}
				style.BoxColor = c
			}
			if spec.Box.Width > 0 {
				style.BoxWidth = spec.Box.Width
			}
		}
		if spec.HighlightColor != "" {
			c, err := browser.ParseHexColor(spec.HighlightColor)
			if err != nil {
				return commandOutput, commandError("validation", exitRunErr, err)
			}
			style.HlColor = c
		}
		if spec.CropMargin != nil && *spec.CropMargin >= 0 {
			style.Margin = *spec.CropMargin
		}

		if item.Box == nil {
			results = append(results, entry)
			continue
		}

		// fixed/sticky 遮挡诊断：只报不遮（隐藏需要改 DOM）。
		overlapped := 0
		for _, fr := range collected.FixedRects {
			if browser.RectsOverlap(fr, *item.Box) {
				overlapped++
			}
		}
		if overlapped > 0 {
			entry.Warnings = append(entry.Warnings, fmt.Sprintf("overlapped_by_fixed:%d", overlapped))
		}
		if item.PendingImages > 0 {
			entry.Warnings = append(entry.Warnings, fmt.Sprintf("pending_images:%d", item.PendingImages))
		}

		out, w, h, compErr := compositeItem(fullPage, item, style)
		if compErr != nil {
			return commandOutput, commandError("delivery", exitRunErr, compErr)
		}
		name := sanitizeExcerptName(item.Name, i)
		path := filepath.Join(outDir, name+".png")
		if err := os.WriteFile(path, out, 0o644); err != nil {
			return commandOutput, commandError("delivery", exitRunErr, err)
		}
		entry.Path = path
		entry.Width = w
		entry.Height = h
		results = append(results, entry)
	}

	payload := map[string]any{
		"results": results,
		"page": map[string]any{
			"url":       collected.URL,
			"title":     collected.Title,
			"width_px":  collected.PageW,
			"height_px": collected.PageH,
			"dpr":       collected.DPR,
		},
		"session_id": flags.sessionID,
	}
	enc, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return commandOutput, commandError("delivery", exitRunErr, err)
	}
	commandOutput.Payload = json.RawMessage(enc)
	return finishCommand(commandOutput, exitOK)
}

// compositeItem 把单条采集结果合成成图。
func compositeItem(fullPage []byte, item browser.ExcerptCollectItem, style browser.ExcerptStyle) ([]byte, int, int, error) {
	var hl []browser.ExcerptRect
	for _, h := range item.Highlights {
		hl = append(hl, h.Rects...)
	}
	return browser.CompositeExcerpt(fullPage, *item.Box, hl, style)
}

// loadExcerptSpec 读 spec：inline JSON ｜ @file ｜ -（stdin）。
func loadExcerptSpec(src string) ([]browser.ExcerptSpec, error) {
	var raw []byte
	var err error
	switch {
	case src == "-":
		raw, err = os.ReadFile("/dev/stdin")
		if err != nil {
			return nil, fmt.Errorf("dw-browser excerpt: read stdin: %w", err)
		}
	case strings.HasPrefix(src, "@"):
		raw, err = os.ReadFile(strings.TrimPrefix(src, "@"))
		if err != nil {
			return nil, fmt.Errorf("dw-browser excerpt: read spec file: %w", err)
		}
	default:
		raw = []byte(src)
	}
	var specs []browser.ExcerptSpec
	if err := json.Unmarshal(raw, &specs); err != nil {
		return nil, fmt.Errorf("dw-browser excerpt: spec must be a JSON array of {name,targets,highlight,...}: %w", err)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("%s", "dw-browser excerpt: spec array is empty")
	}
	for i, s := range specs {
		if strings.TrimSpace(s.Name) == "" {
			return nil, fmt.Errorf("dw-browser excerpt: spec[%d].name is required", i)
		}
		if len(s.Targets) == 0 {
			return nil, fmt.Errorf("dw-browser excerpt: spec[%d] (%s) needs at least one target", i, s.Name)
		}
	}
	return specs, nil
}

// sanitizeExcerptName 把 spec 名变成安全文件名（去路径分隔符/控制符）。
func sanitizeExcerptName(name string, index int) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == 0 || r < 0x20:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" || out == "." || out == ".." {
		return fmt.Sprintf("excerpt-%d", index+1)
	}
	return out
}
