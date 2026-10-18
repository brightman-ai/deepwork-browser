package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
	"github.com/brightman-ai/deepwork-browser/pdftool"
)

// pdf 命令族 — 无会话后台工具（同 document/fetch 定位）：本地/URL 的 PDF 与
// office 文档（docx/pptx/xlsx…经 soffice 转 PDF）渲染与词级标注截图。
//
//	dw-browser pdf preview --file <path|url> [--pages 1-3] [--dpi 150] [--out DIR]
//	dw-browser pdf excerpt --file <path|url> --spec '<json|@file|->' [--out DIR]
//
// 语义与 web excerpt 同构：红框 = boxes ∪ 命中词并集，荧光 = 命中词逐词覆盖，
// 裁剪 = 框 + margin。missing 显式报、exit 0；工具级故障非零退。
const pdfExcerptDPI = 200

// PdfExcerptSpec 是单条 pdf 摘录的输入契约。boxes/坐标单位 pt。
type PdfExcerptSpec struct {
	Name           string                   `json:"name"`
	Page           int                      `json:"page"`
	Phrases        []string                 `json:"phrases,omitempty"`
	Boxes          [][]float64              `json:"boxes,omitempty"` // [[x0,y0,x1,y1]pt...]
	BoxStyle       *browser.ExcerptBoxStyle `json:"box_style,omitempty"`
	HighlightColor string                   `json:"highlight_color,omitempty"`
	CropMargin     *int                     `json:"crop_margin,omitempty"`
}

// acquirePDFSource 把 --file 参数解析成本地 PDF 路径。
// 返回 (pdfPath, 来源描述, cleanup)。URL 走 fetch 的网络边界；office 扩展经
// soffice 转换（临时目录，cleanup 移除）。
func acquirePDFSource(ctx context.Context, fileArg, networkRoute string) (string, string, func(), error) {
	cleanup := func() {}
	if networkRoute != "" && networkRoute != "system" && networkRoute != "direct" {
		return "", "", cleanup, fmt.Errorf("dw-browser pdf: --network-route must be system or direct, got %q", networkRoute)
	}
	if strings.HasPrefix(fileArg, "http://") || strings.HasPrefix(fileArg, "https://") {
		if _, err := browser.ValidatePublicURL(fileArg, nil); err != nil {
			return "", "", cleanup, err
		}
		route := browser.NetworkRouteSystem
		if networkRoute == "direct" {
			route = browser.NetworkRouteDirect
		}
		req := browser.ResourceRequest{
			URL: fileArg, NetworkRoute: route,
			Timeout: 120 * time.Second, MaxBytes: 256 << 20, MaxNetworkBytes: 512 << 20,
		}
		resource, err := browser.FetchResource(ctx, req)
		if err != nil {
			return "", "", cleanup, fmt.Errorf("dw-browser pdf: download %s: %w", fileArg, err)
		}
		tmp, err := os.CreateTemp("", "dwpdf-dl-*.bin")
		if err != nil {
			return "", "", cleanup, fmt.Errorf("dw-browser pdf: temp file: %w", err)
		}
		if _, err := tmp.Write(resource.Body); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", "", cleanup, fmt.Errorf("dw-browser pdf: persist download: %w", err)
		}
		tmp.Close()
		cleanup = func() { os.Remove(tmp.Name()) }
		return tmp.Name(), "url", cleanup, nil
	}
	if st, err := os.Stat(fileArg); err != nil || st.IsDir() {
		return "", "", cleanup, fmt.Errorf("dw-browser pdf: --file %q is not a readable file", fileArg)
	}
	if pdftool.IsPDF(fileArg) {
		return fileArg, "pdf", cleanup, nil
	}
	conv, err := pdftool.ConvertToPDF(ctx, fileArg)
	if err != nil {
		return "", "", cleanup, err
	}
	cleanup = func() { pdftool.CleanupConverted(conv) }
	return conv, "office→pdf", cleanup, nil
}

// parsePdfPages 解析 "3" / "1-3" / "2,5-7"。
func parsePdfPages(raw string, total int) ([]int, error) {
	var pages []int
	seen := map[int]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, found := strings.Cut(part, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("dw-browser pdf: bad --pages %q", raw)
		}
		b := a
		if found {
			if b, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				return nil, fmt.Errorf("dw-browser pdf: bad --pages %q", raw)
			}
		}
		if a < 1 || b < a {
			return nil, fmt.Errorf("dw-browser pdf: bad --pages %q", raw)
		}
		if b > total {
			b = total
		}
		for p := a; p <= b; p++ {
			if !seen[p] {
				seen[p] = true
				pages = append(pages, p)
			}
		}
	}
	sort.Ints(pages)
	return pages, nil
}

func runPdfPreviewResult(args []string) (commandOutput CommandResult, resultErr error) {
	var fileArg, pagesRaw, outDir, networkRoute string
	dpi := 150
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--file" && i+1 < len(args):
			fileArg, i = args[i+1], i+1
		case strings.HasPrefix(arg, "--file="):
			fileArg = arg[len("--file="):]
		case arg == "--network-route" && i+1 < len(args):
			networkRoute, i = args[i+1], i+1
		case strings.HasPrefix(arg, "--network-route="):
			networkRoute = arg[len("--network-route="):]
		case arg == "--pages" && i+1 < len(args):
			pagesRaw, i = args[i+1], i+1
		case strings.HasPrefix(arg, "--pages="):
			pagesRaw = arg[len("--pages="):]
		case arg == "--out" && i+1 < len(args):
			outDir, i = args[i+1], i+1
		case strings.HasPrefix(arg, "--out="):
			outDir = arg[len("--out="):]
		case arg == "--dpi" && i+1 < len(args):
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 36 || n > 600 {
				return commandOutput, commandError("validation", exitRunErr, fmt.Errorf("dw-browser pdf preview: --dpi %q out of range 36–600", args[i+1]))
			}
			dpi, i = n, i+1
		case strings.HasPrefix(arg, "--dpi="):
			if n, err := strconv.Atoi(arg[len("--dpi="):]); err == nil && n >= 36 && n <= 600 {
				dpi = n
			} else {
				return commandOutput, commandError("validation", exitRunErr, fmt.Errorf("dw-browser pdf preview: --dpi %q out of range 36–600", arg[len("--dpi="):]))
			}
		}
	}
	if fileArg == "" {
		return commandOutput, commandError("validation", exitRunErr, fmt.Errorf("%s", "dw-browser pdf preview: requires --file <path|url>"))
	}
	if pagesRaw == "" {
		pagesRaw = "1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pdfPath, kind, cleanup, err := acquirePDFSource(ctx, fileArg, networkRoute)
	if err != nil {
		return commandOutput, commandError("validation", exitRunErr, err)
	}
	defer cleanup()

	info, err := pdftool.Info(pdfPath)
	if err != nil {
		return commandOutput, commandError("runtime", exitRunErr, err)
	}
	pages, err := parsePdfPages(pagesRaw, info.Pages)
	if err != nil {
		return commandOutput, commandError("validation", exitRunErr, err)
	}
	if len(pages) == 0 {
		return commandOutput, commandError("validation", exitRunErr, fmt.Errorf("dw-browser pdf preview: document has %d pages; --pages %q resolves to none", info.Pages, pagesRaw))
	}
	if outDir == "" {
		outDir = "evidence/pdf-preview"
	}
	if outDir, err = filepath.Abs(outDir); err != nil {
		return commandOutput, commandError("validation", exitRunErr, err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return commandOutput, commandError("delivery", exitRunErr, err)
	}
	base := sanitizeExcerptName(strings.TrimSuffix(filepath.Base(fileArg), filepath.Ext(fileArg)), 0)
	type entry struct {
		Page   int    `json:"page"`
		Path   string `json:"path"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	results := make([]entry, 0, len(pages))
	for _, p := range pages {
		png, err := pdftool.RenderPage(pdfPath, p, dpi)
		if err != nil {
			return commandOutput, commandError("runtime", exitRunErr, err)
		}
		cfg, _, derr := image.DecodeConfig(bytes.NewReader(png))
		if derr != nil {
			return commandOutput, commandError("runtime", exitRunErr, fmt.Errorf("dw-browser pdf preview: decode page %d: %w", p, derr))
		}
		path := filepath.Join(outDir, fmt.Sprintf("%s-p%02d.png", base, p))
		if err := os.WriteFile(path, png, 0o644); err != nil {
			return commandOutput, commandError("delivery", exitRunErr, err)
		}
		results = append(results, entry{Page: p, Path: path, Width: cfg.Width, Height: cfg.Height})
	}
	payload := map[string]any{
		"results": results,
		"pdf":     map[string]any{"pages": info.Pages, "page_width_pt": info.Width, "page_height_pt": info.Height},
		"source":  map[string]any{"input": fileArg, "kind": kind},
		"dpi":     dpi,
	}
	enc, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return commandOutput, commandError("delivery", exitRunErr, err)
	}
	commandOutput.Payload = json.RawMessage(enc)
	return finishCommand(commandOutput, exitOK)
}

func runPdfExcerptResult(args []string) (commandOutput CommandResult, resultErr error) {
	var fileArg, specSrc, outDir, networkRoute string
	dpi := pdfExcerptDPI
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--file" && i+1 < len(args):
			fileArg, i = args[i+1], i+1
		case strings.HasPrefix(arg, "--file="):
			fileArg = arg[len("--file="):]
		case arg == "--network-route" && i+1 < len(args):
			networkRoute, i = args[i+1], i+1
		case strings.HasPrefix(arg, "--network-route="):
			networkRoute = arg[len("--network-route="):]
		case arg == "--spec" && i+1 < len(args):
			specSrc, i = args[i+1], i+1
		case strings.HasPrefix(arg, "--spec="):
			specSrc = arg[len("--spec="):]
		case arg == "--out" && i+1 < len(args):
			outDir, i = args[i+1], i+1
		case strings.HasPrefix(arg, "--out="):
			outDir = arg[len("--out="):]
		}
	}
	if fileArg == "" || specSrc == "" {
		return commandOutput, commandError("validation", exitRunErr, fmt.Errorf("%s", "dw-browser pdf excerpt: requires --file <path|url> and --spec <json|@file|->"))
	}
	specs, err := loadPdfExcerptSpec(specSrc)
	if err != nil {
		return commandOutput, commandError("validation", exitRunErr, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pdfPath, kind, cleanup, err := acquirePDFSource(ctx, fileArg, networkRoute)
	if err != nil {
		return commandOutput, commandError("validation", exitRunErr, err)
	}
	defer cleanup()
	info, err := pdftool.Info(pdfPath)
	if err != nil {
		return commandOutput, commandError("runtime", exitRunErr, err)
	}
	if outDir == "" {
		outDir = "evidence/pdf-excerpts"
	}
	if outDir, err = filepath.Abs(outDir); err != nil {
		return commandOutput, commandError("validation", exitRunErr, err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return commandOutput, commandError("delivery", exitRunErr, err)
	}

	// 按页分组：同页只渲染/取词一次。
	byPage := map[int][]int{}
	for i, s := range specs {
		byPage[s.Page] = append(byPage[s.Page], i)
	}
	type pageWork struct {
		png   []byte
		words []pdftool.Word
	}
	pages := map[int]*pageWork{}
	for p := range byPage {
		if p < 1 || p > info.Pages {
			continue
		}
		png, err := pdftool.RenderPage(pdfPath, p, dpi)
		if err != nil {
			return commandOutput, commandError("runtime", exitRunErr, err)
		}
		words, err := pdftool.Words(pdfPath, p)
		if err != nil {
			return commandOutput, commandError("runtime", exitRunErr, err)
		}
		pages[p] = &pageWork{png: png, words: words}
	}

	type resultEntry struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Width   int    `json:"width,omitempty"`
		Height  int    `json:"height,omitempty"`
		Missing struct {
			Phrases []string `json:"phrases"`
		} `json:"missing"`
		Warnings []string `json:"warnings"`
	}
	ensure := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	scale := float64(dpi) / 72
	results := make([]resultEntry, 0, len(specs))
	for i, spec := range specs {
		entry := resultEntry{Name: spec.Name, Warnings: []string{}}
		entry.Missing.Phrases = []string{}
		if spec.Page < 1 || spec.Page > info.Pages {
			entry.Warnings = append(entry.Warnings, fmt.Sprintf("page_out_of_range:%d>%d", spec.Page, info.Pages))
			results = append(results, entry)
			continue
		}
		w := pages[spec.Page]

		style := browser.DefaultExcerptStyle()
		style.DPR = 1 // rect 已是渲染像素
		if spec.BoxStyle != nil {
			if spec.BoxStyle.Color != "" {
				c, err := browser.ParseHexColor(spec.BoxStyle.Color)
				if err != nil {
					return commandOutput, commandError("validation", exitRunErr, err)
				}
				style.BoxColor = c
			}
			if spec.BoxStyle.Width > 0 {
				style.BoxWidth = spec.BoxStyle.Width
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

		// 命中词 → 像素矩形；missing 显式。
		var hlRects []browser.ExcerptRect
		var misses []string
		minX, minY, maxX, maxY := 1e18, 1e18, -1e18, -1e18
		for _, ph := range spec.Phrases {
			hit := pdftool.FindPhrase(w.words, ph)
			if hit == nil {
				misses = append(misses, ph)
				continue
			}
			for _, wd := range hit {
				r := browser.ExcerptRect{X: wd.XMin * scale, Y: wd.YMin * scale, W: (wd.XMax - wd.XMin) * scale, H: (wd.YMax - wd.YMin) * scale}
				hlRects = append(hlRects, r)
				minX, minY = minF(minX, r.X), minF(minY, r.Y)
				maxX, maxY = maxF(maxX, r.X+r.W), maxF(maxY, r.Y+r.H)
			}
		}
		entry.Missing.Phrases = ensure(misses)
		for _, bx := range spec.Boxes {
			if len(bx) != 4 {
				entry.Warnings = append(entry.Warnings, fmt.Sprintf("bad_box_len:%d", len(bx)))
				continue
			}
			x0, y0, x1, y1 := bx[0]*scale, bx[1]*scale, bx[2]*scale, bx[3]*scale
			minX, minY = minF(minX, x0), minF(minY, y0)
			maxX, maxY = maxF(maxX, x1), maxF(maxY, y1)
		}
		if len(hlRects) == 0 && len(spec.Boxes) == 0 {
			results = append(results, entry)
			continue
		}
		out, pw, ph2, compErr := browser.CompositeExcerpt(w.png,
			browser.ExcerptRect{X: minX, Y: minY, W: maxX - minX, H: maxY - minY},
			hlRects, style)
		if compErr != nil {
			return commandOutput, commandError("delivery", exitRunErr, compErr)
		}
		name := sanitizeExcerptName(spec.Name, i)
		path := filepath.Join(outDir, name+".png")
		if err := os.WriteFile(path, out, 0o644); err != nil {
			return commandOutput, commandError("delivery", exitRunErr, err)
		}
		entry.Path = path
		entry.Width = pw
		entry.Height = ph2
		results = append(results, entry)
	}
	payload := map[string]any{
		"results": results,
		"pdf":     map[string]any{"pages": info.Pages, "page_width_pt": info.Width, "page_height_pt": info.Height},
		"source":  map[string]any{"input": fileArg, "kind": kind},
		"dpi":     dpi,
	}
	enc, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return commandOutput, commandError("delivery", exitRunErr, err)
	}
	commandOutput.Payload = json.RawMessage(enc)
	return finishCommand(commandOutput, exitOK)
}

func loadPdfExcerptSpec(src string) ([]PdfExcerptSpec, error) {
	var raw []byte
	var err error
	switch {
	case src == "-":
		raw, err = os.ReadFile("/dev/stdin")
	case strings.HasPrefix(src, "@"):
		raw, err = os.ReadFile(strings.TrimPrefix(src, "@"))
	default:
		raw = []byte(src)
	}
	if err != nil {
		return nil, fmt.Errorf("dw-browser pdf excerpt: read spec: %w", err)
	}
	var rawList []json.RawMessage
	if err := json.Unmarshal(raw, &rawList); err != nil {
		return nil, fmt.Errorf("%s", "dw-browser pdf excerpt: spec must be a JSON array of {name,page,phrases,boxes} entries")
	}
	var specs []PdfExcerptSpec
	if err := json.Unmarshal(raw, &specs); err != nil {
		return nil, fmt.Errorf("dw-browser pdf excerpt: spec entry is malformed: %w", err)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("%s", "dw-browser pdf excerpt: spec array is empty")
	}
	for i, s := range specs {
		if strings.TrimSpace(s.Name) == "" {
			return nil, fmt.Errorf("dw-browser pdf excerpt: spec[%d].name is required", i)
		}
		if s.Page < 1 {
			return nil, fmt.Errorf("dw-browser pdf excerpt: spec[%d] (%s) needs page>=1", i, s.Name)
		}
		if len(s.Phrases) == 0 && len(s.Boxes) == 0 {
			return nil, fmt.Errorf("dw-browser pdf excerpt: spec[%d] (%s) needs phrases or boxes", i, s.Name)
		}
	}
	return specs, nil
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
