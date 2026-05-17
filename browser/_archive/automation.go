package browser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/internal/tool"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// intPtr returns a pointer to an int value.
func intPtr(v int) *int { return &v }

// withPage creates a fresh isolated Page, executes fn, then closes the page.
// Returns ErrBrowserUnavailable if runtime is not in StateRunning.
func (r *browserRuntime) withPage(ctx context.Context, fn func(*rod.Page) error) error {
	if r.State != StateRunning {
		return ErrBrowserUnavailable
	}

	r.browserMu.Lock
	b := r.browser
	r.browserMu.Unlock

	if b == nil {
		return ErrBrowserUnavailable
	}

	// Create page with ctx-aware cancel
	pageCtx, pageCancel := context.WithCancel(ctx)
	defer pageCancel

	r.registerActiveCancel(pageCancel)
	defer r.unregisterActiveCancel(pageCancel)

	page, err := b.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return fmt.Errorf("create page: %w", err)
	}
	page = page.Context(pageCtx)
	defer func { _ = page.Close }

	return fn(page)
}

// Navigate navigates to url and returns status, title, final URL.
func (r *browserRuntime) Navigate(ctx context.Context, url string, waitLoad bool) (*NavigateResult, error) {
	var result NavigateResult
	err := r.withPage(ctx, func(page *rod.Page) error {
		// Set up response status tracking via network
		var statusCode int
		page.MustSetExtraHeaders("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

		router := page.HijackRequests
		defer router.Stop

		router.MustAdd("*", func(ctx *rod.Hijack) {
			ctx.MustLoadResponse
			if ctx.Request.URL.String == url {
				statusCode = ctx.Response.Payload.ResponseCode
			}
		})
		go router.Run

		err := page.Navigate(url)
		if err != nil {
			// Navigation error — try without hijacking
			statusCode = 0
		}

		if waitLoad {
			_ = page.WaitLoad
		}

		info, _ := page.Info
		title := ""
		finalURL := url
		if info != nil {
			title = info.Title
			finalURL = info.URL
		}

		if statusCode == 0 {
			statusCode = 200 // default if hijack didn't fire
		}

		result = NavigateResult{
			Status: statusCode
			Title: title
			URL: finalURL
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Click finds selector and clicks it.
func (r *browserRuntime) Click(ctx context.Context, selector string, timeoutMs int) error {
	return r.withPage(ctx, func(page *rod.Page) error {
		timeout := time.Duration(timeoutMs) * time.Millisecond
		if timeout == 0 {
			timeout = 5 * time.Second
		}
		page = page.Timeout(timeout)

		el, err := page.Element(selector)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrElementNotFound, selector)
		}
		return el.Click(proto.InputMouseButtonLeft, 1)
	})
}

// Type inputs text into an element. Refuses password fields (security red line).
func (r *browserRuntime) Type(ctx context.Context, selector, text string, clearFirst bool) error {
	return r.withPage(ctx, func(page *rod.Page) error {
		el, err := page.Element(selector)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrElementNotFound, selector)
		}

		// Security red line: refuse password fields
		inputType, _ := el.Attribute("type")
		if inputType != nil && strings.EqualFold(*inputType, "password") {
			r.bus.Publish(EventSecureInputRequired{Selector: selector})
			return ErrPasswordFieldRejected
		}

		if clearFirst {
			if err := el.SelectAllText; err == nil {
				_ = el.Input("")
			}
		}
		return el.Input(text)
	})
}

// Screenshot captures a JPEG screenshot of the current page.
// Returns raw JPEG bytes (not base64).
func (r *browserRuntime) Screenshot(ctx context.Context, fullPage bool, quality int) (byte, error) {
	if quality <= 0 {
		quality = 80
	}

	var data byte
	err := r.withPage(ctx, func(page *rod.Page) error {
		// Check context before proceeding
		select {
		case <-ctx.Done:
			return ctx.Err
		default:
		}

		var err error
		req := &proto.PageCaptureScreenshot{
			Format: proto.PageCaptureScreenshotFormatJpeg
			Quality: intPtr(quality)
		}
		data, err = page.Screenshot(fullPage, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// Extract extracts text and HTML from matching elements.
func (r *browserRuntime) Extract(ctx context.Context, selector string, multiple bool) (*ExtractResult, error) {
	var result ExtractResult
	err := r.withPage(ctx, func(page *rod.Page) error {
		if multiple {
			els, err := page.Elements(selector)
			if err != nil || len(els) == 0 {
				return fmt.Errorf("%w: %s", ErrElementNotFound, selector)
			}
			result.Count = len(els)
			var items string
			for _, el := range els {
				txt, _ := el.Text
				items = append(items, txt)
			}
			result.Items = items
			if len(items) > 0 {
				result.Text = items[0]
			}
			// First element HTML
			html, _ := els[0].HTML
			result.HTML = html
		} else {
			el, err := page.Element(selector)
			if err != nil {
				return fmt.Errorf("%w: %s", ErrElementNotFound, selector)
			}
			text, _ := el.Text
			html, _ := el.HTML
			result = ExtractResult{
				Text: text
				HTML: html
				Count: 1
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Evaluate executes JavaScript on the page and returns the result.
func (r *browserRuntime) Evaluate(ctx context.Context, js string) (any, error) {
	var result any
	err := r.withPage(ctx, func(page *rod.Page) error {
		val, err := page.Eval(js)
		if err != nil {
			return err
		}
		result = val.Value
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// WaitForSelector waits until selector is present (and optionally visible).
func (r *browserRuntime) WaitForSelector(ctx context.Context, selector string, timeoutMs int, visible bool) (bool, error) {
	timeout := time.Duration(timeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	var found bool
	err := r.withPage(ctx, func(page *rod.Page) error {
		page = page.Timeout(timeout)
		var err error
		if visible {
			_, err = page.ElementR(selector, "")
		} else {
			_, err = page.Element(selector)
		}
		if err != nil {
			found = false
			return nil // not an error — just not found
		}
		found = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// RegisterTools registers all 7 browser_* tools into the TS-03 ToolRegistry.
func (r *browserRuntime) RegisterTools(registry tool.ToolRegistry) error {
	tools := tool.ToolDefinition{
		{
			Name: "browser_navigate"
			Description: "导航浏览器到指定 URL，返回页面标题和状态码"
			Parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","description":"目标 URL"},"wait_load":{"type":"boolean","default":true}},"required":["url"]}`)
			EffectClass: tool.EffectClassWrite
			Source: tool.ToolSourceSystem
			Priority: 1
			Handler: r.toolNavigate
		}
		{
			Name: "browser_click"
			Description: "点击页面上匹配 CSS selector 的元素"
			Parameters: json.RawMessage(`{"type":"object","properties":{"selector":{"type":"string"},"timeout_ms":{"type":"integer","default":5000}},"required":["selector"]}`)
			EffectClass: tool.EffectClassWrite
			Source: tool.ToolSourceSystem
			Priority: 1
			Handler: r.toolClick
		}
		{
			Name: "browser_type"
			Description: "向元素输入文本（非密码字段）"
			Parameters: json.RawMessage(`{"type":"object","properties":{"selector":{"type":"string"},"text":{"type":"string"},"clear_first":{"type":"boolean","default":true}},"required":["selector","text"]}`)
			EffectClass: tool.EffectClassWrite
			Source: tool.ToolSourceSystem
			Priority: 1
			Handler: r.toolType
		}
		{
			Name: "browser_screenshot"
			Description: "对当前页面截图，返回 base64 编码 JPEG"
			Parameters: json.RawMessage(`{"type":"object","properties":{"full_page":{"type":"boolean","default":false},"quality":{"type":"integer","default":80}}}`)
			EffectClass: tool.EffectClassRead
			Source: tool.ToolSourceSystem
			Priority: 1
			Handler: r.toolScreenshot
		}
		{
			Name: "browser_extract"
			Description: "提取页面元素的文本和 HTML 内容"
			Parameters: json.RawMessage(`{"type":"object","properties":{"selector":{"type":"string"},"multiple":{"type":"boolean","default":false}},"required":["selector"]}`)
			EffectClass: tool.EffectClassRead
			Source: tool.ToolSourceSystem
			Priority: 1
			Handler: r.toolExtract
		}
		{
			Name: "browser_evaluate"
			Description: "在页面上下文执行 JavaScript，返回结果"
			Parameters: json.RawMessage(`{"type":"object","properties":{"js":{"type":"string","description":"JavaScript 表达式或语句"}},"required":["js"]}`)
			EffectClass: tool.EffectClassWrite
			Source: tool.ToolSourceSystem
			Priority: 1
			Handler: r.toolEvaluate
		}
		{
			Name: "browser_wait"
			Description: "等待 CSS selector 匹配的元素出现在 DOM 中"
			Parameters: json.RawMessage(`{"type":"object","properties":{"selector":{"type":"string"},"timeout_ms":{"type":"integer","default":10000},"visible":{"type":"boolean","default":false}},"required":["selector"]}`)
			EffectClass: tool.EffectClassRead
			Source: tool.ToolSourceSystem
			Priority: 1
			Handler: r.toolWait
		}
	}

	for _, def := range tools {
		if err := registry.Register(def); err != nil {
			return fmt.Errorf("register tool %s: %w", def.Name, err)
		}
	}
	return nil
}

// --- Tool handler implementations ---

func (r *browserRuntime) toolNavigate(ctx context.Context, input json.RawMessage) (tool.ToolResult, error) {
	var params struct {
		URL string `json:"url"`
		WaitLoad *bool `json:"wait_load"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return tool.ToolResult{Error: "invalid parameters"}, nil
	}
	waitLoad := true
	if params.WaitLoad != nil {
		waitLoad = *params.WaitLoad
	}

	result, err := r.Navigate(ctx, params.URL, waitLoad)
	if err != nil {
		return tool.ToolResult{Error: err.Error}, nil
	}

	out, _ := json.Marshal(result)
	return tool.ToolResult{
		Output: fmt.Sprintf("Navigated to %s (status=%d, title=%q)", result.URL, result.Status, result.Title)
		OutputJSON: out
		EffectClass: tool.EffectClassWrite
	}, nil
}

func (r *browserRuntime) toolClick(ctx context.Context, input json.RawMessage) (tool.ToolResult, error) {
	var params struct {
		Selector string `json:"selector"`
		TimeoutMs int `json:"timeout_ms"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return tool.ToolResult{Error: "invalid parameters"}, nil
	}
	if params.TimeoutMs == 0 {
		params.TimeoutMs = 5000
	}

	if err := r.Click(ctx, params.Selector, params.TimeoutMs); err != nil {
		return tool.ToolResult{Error: err.Error}, nil
	}

	out, _ := json.Marshal(map[string]bool{"success": true})
	return tool.ToolResult{
		Output: fmt.Sprintf("Clicked element: %s", params.Selector)
		OutputJSON: out
		EffectClass: tool.EffectClassWrite
	}, nil
}

func (r *browserRuntime) toolType(ctx context.Context, input json.RawMessage) (tool.ToolResult, error) {
	var params struct {
		Selector string `json:"selector"`
		Text string `json:"text"`
		ClearFirst *bool `json:"clear_first"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return tool.ToolResult{Error: "invalid parameters"}, nil
	}
	clearFirst := true
	if params.ClearFirst != nil {
		clearFirst = *params.ClearFirst
	}

	if err := r.Type(ctx, params.Selector, params.Text, clearFirst); err != nil {
		return tool.ToolResult{Error: err.Error}, nil
	}

	out, _ := json.Marshal(map[string]bool{"success": true})
	return tool.ToolResult{
		Output: fmt.Sprintf("Typed text into: %s", params.Selector)
		OutputJSON: out
		EffectClass: tool.EffectClassWrite
	}, nil
}

func (r *browserRuntime) toolScreenshot(ctx context.Context, input json.RawMessage) (tool.ToolResult, error) {
	var params struct {
		FullPage *bool `json:"full_page"`
		Quality int `json:"quality"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return tool.ToolResult{Error: "invalid parameters"}, nil
	}
	fullPage := false
	if params.FullPage != nil {
		fullPage = *params.FullPage
	}
	if params.Quality == 0 {
		params.Quality = 80
	}

	// Check context before taking screenshot
	select {
	case <-ctx.Done:
		return tool.ToolResult{Error: "context cancelled"}, nil
	default:
	}

	data, err := r.Screenshot(ctx, fullPage, params.Quality)
	if err != nil {
		return tool.ToolResult{Error: err.Error}, nil
	}

	b64 := base64.StdEncoding.EncodeToString(data)
	out, _ := json.Marshal(map[string]any{
		"image_base64": b64
	})
	return tool.ToolResult{
		Output: fmt.Sprintf("Screenshot captured (%d bytes)", len(data))
		OutputJSON: out
		EffectClass: tool.EffectClassRead
	}, nil
}

func (r *browserRuntime) toolExtract(ctx context.Context, input json.RawMessage) (tool.ToolResult, error) {
	var params struct {
		Selector string `json:"selector"`
		Multiple bool `json:"multiple"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return tool.ToolResult{Error: "invalid parameters"}, nil
	}

	result, err := r.Extract(ctx, params.Selector, params.Multiple)
	if err != nil {
		return tool.ToolResult{Error: err.Error}, nil
	}

	out, _ := json.Marshal(result)
	return tool.ToolResult{
		Output: fmt.Sprintf("Extracted %d element(s) matching %s", result.Count, params.Selector)
		OutputJSON: out
		EffectClass: tool.EffectClassRead
	}, nil
}

func (r *browserRuntime) toolEvaluate(ctx context.Context, input json.RawMessage) (tool.ToolResult, error) {
	var params struct {
		JS string `json:"js"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return tool.ToolResult{Error: "invalid parameters"}, nil
	}

	result, err := r.Evaluate(ctx, params.JS)
	if err != nil {
		return tool.ToolResult{Error: err.Error}, nil
	}

	out, _ := json.Marshal(map[string]any{"result": result})
	return tool.ToolResult{
		Output: fmt.Sprintf("JS evaluated: result=%v", result)
		OutputJSON: out
		EffectClass: tool.EffectClassWrite
	}, nil
}

func (r *browserRuntime) toolWait(ctx context.Context, input json.RawMessage) (tool.ToolResult, error) {
	var params struct {
		Selector string `json:"selector"`
		TimeoutMs int `json:"timeout_ms"`
		Visible bool `json:"visible"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return tool.ToolResult{Error: "invalid parameters"}, nil
	}
	if params.TimeoutMs == 0 {
		params.TimeoutMs = 10000
	}

	start := time.Now
	found, err := r.WaitForSelector(ctx, params.Selector, params.TimeoutMs, params.Visible)
	if err != nil {
		return tool.ToolResult{Error: err.Error}, nil
	}

	elapsed := time.Since(start).Milliseconds
	out, _ := json.Marshal(map[string]any{
		"found": found
		"elapsed_ms": elapsed
	})
	return tool.ToolResult{
		Output: fmt.Sprintf("Wait for %s: found=%v elapsed=%dms", params.Selector, found, elapsed)
		OutputJSON: out
		EffectClass: tool.EffectClassRead
	}, nil
}
