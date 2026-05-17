// Package safari provides the Safari engine for dw-browser.
//
// Architecture (v4, MERGED):
//
//	SafariBrowserCore
//	├── WebDriverClient → W3C WebDriver protocol (safaridriver)
//	├── SimctlManager → xcrun simctl (iOS Simulator only)
//	└── AXBridge → macOS AX API (P2 optional, native UI only)
//
// safaridriver supports both macOS Safari (platformName=mac) and
// iOS Simulator Safari (platformName=iOS). The --device flag routes
// between these two modes.
package safari

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// WebDriverClient is a minimal W3C WebDriver protocol client for safaridriver.
// It covers the subset needed by SafariBrowserCore: session management
// navigation, element finding, JS execution, screenshots, and element interaction.
type WebDriverClient struct {
	mu sync.Mutex
	baseURL string
	sessionID string
	http *http.Client
	driver *exec.Cmd // safaridriver process (owned lifecycle)
	port int
}

// WebDriverOpts configures a WebDriver session.
type WebDriverOpts struct {
	// Platform: "mac" for macOS Safari, "iOS" for Simulator Safari.
	Platform string
	// DeviceUDID: iOS Simulator UDID (required when Platform="iOS").
	DeviceUDID string
	// Port: safaridriver listen port. 0 = auto-assign.
	Port int
}

// Element is a W3C WebDriver element reference.
type Element struct {
	ID string // W3C element identifier
}

// ElementInfo holds element metadata retrieved via WebDriver.
type ElementInfo struct {
	ID string
	TagName string
	Text string
	Role string
	AriaLabel string
	TestID string
	Placeholder string
	Href string
	Visible bool
	Enabled bool
}

// w3c element identifier key per spec.
const w3cElementKey = "element-6066-11e4-a52e-4f735466cecf"

// NewWebDriverClient starts safaridriver and creates a session.
func NewWebDriverClient(ctx context.Context, opts WebDriverOpts) (*WebDriverClient, error) {
	port := opts.Port
	if port == 0 {
		p, err := freePort
		if err != nil {
			return nil, fmt.Errorf("webdriver: find free port: %w", err)
		}
		port = p
	}

	// Check safaridriver --enable status by attempting session.
	// If it fails with specific error, guide user.
	driver := exec.CommandContext(ctx, "safaridriver", "-p", strconv.Itoa(port))
	driver.Stdout = nil
	driver.Stderr = nil
	if err := driver.Start; err != nil {
		return nil, fmt.Errorf("webdriver: start safaridriver: %w\nRun 'sudo safaridriver --enable' first", err)
	}

	c := &WebDriverClient{
		baseURL: fmt.Sprintf("http://localhost:%d", port)
		http: &http.Client{Timeout: 30 * time.Second}
		driver: driver
		port: port
	}

	// Wait for safaridriver to be ready.
	if err := c.waitReady(ctx); err != nil {
		driver.Process.Kill
		return nil, err
	}

	// Create session.
	if err := c.createSession(ctx, opts); err != nil {
		driver.Process.Kill
		return nil, err
	}

	return c, nil
}

// waitReady polls safaridriver until it responds.
func (c *WebDriverClient) waitReady(ctx context.Context) error {
	deadline := time.Now.Add(10 * time.Second)
	for time.Now.Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/status", nil)
		resp, err := c.http.Do(req)
		if err == nil {
			resp.Body.Close
			return nil
		}
		select {
		case <-ctx.Done:
			return ctx.Err
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("webdriver: safaridriver not ready after 10s")
}

// createSession sends NewSession to safaridriver.
func (c *WebDriverClient) createSession(ctx context.Context, opts WebDriverOpts) error {
	platform := opts.Platform
	if platform == "" {
		platform = "mac"
	}

	caps := map[string]any{
		"browserName": "safari"
		"platformName": platform
	}
	if opts.DeviceUDID != "" {
		caps["safari:deviceUDID"] = opts.DeviceUDID
		caps["safari:useSimulator"] = true
	}

	body := map[string]any{
		"capabilities": map[string]any{
			"alwaysMatch": caps
		}
	}

	var result struct {
		Value struct {
			SessionID string `json:"sessionId"`
			Capabilities map[string]any `json:"capabilities"`
			Error string `json:"error"`
			Message string `json:"message"`
		} `json:"value"`
	}

	if err := c.post(ctx, "/session", body, &result); err != nil {
		return fmt.Errorf("webdriver: create session: %w", err)
	}
	if result.Value.Error != "" {
		msg := result.Value.Message
		if result.Value.Error == "session not created" {
			msg += "\nHint: run 'sudo safaridriver --enable' if not done yet"
		}
		return fmt.Errorf("webdriver: %s: %s", result.Value.Error, msg)
	}
	if result.Value.SessionID == "" {
		return fmt.Errorf("webdriver: session created but no sessionId returned")
	}

	c.sessionID = result.Value.SessionID
	return nil
}

// SessionID returns the active session identifier.
func (c *WebDriverClient) SessionID string { return c.sessionID }

// Navigate loads a URL and waits for page load.
func (c *WebDriverClient) Navigate(ctx context.Context, url string) error {
	return c.sessionPost(ctx, "/url", map[string]any{"url": url}, nil)
}

// CurrentURL returns the current page URL.
func (c *WebDriverClient) CurrentURL(ctx context.Context) (string, error) {
	var result struct {
		Value string `json:"value"`
	}
	if err := c.sessionGet(ctx, "/url", &result); err != nil {
		return "", err
	}
	return result.Value, nil
}

// Title returns the page title.
func (c *WebDriverClient) Title(ctx context.Context) (string, error) {
	var result struct {
		Value string `json:"value"`
	}
	if err := c.sessionGet(ctx, "/title", &result); err != nil {
		return "", err
	}
	return result.Value, nil
}

// FindElements finds elements by CSS selector.
func (c *WebDriverClient) FindElements(ctx context.Context, using, value string) (Element, error) {
	var result struct {
		Value map[string]string `json:"value"`
	}
	body := map[string]any{"using": using, "value": value}
	if err := c.sessionPost(ctx, "/elements", body, &result); err != nil {
		return nil, err
	}
	elements := make(Element, 0, len(result.Value))
	for _, m := range result.Value {
		if id, ok := m[w3cElementKey]; ok {
			elements = append(elements, Element{ID: id})
		}
	}
	return elements, nil
}

// FindElement finds a single element by CSS selector.
func (c *WebDriverClient) FindElement(ctx context.Context, using, value string) (*Element, error) {
	var result struct {
		Value map[string]string `json:"value"`
	}
	body := map[string]any{"using": using, "value": value}
	if err := c.sessionPost(ctx, "/element", body, &result); err != nil {
		return nil, err
	}
	id, ok := result.Value[w3cElementKey]
	if !ok {
		return nil, fmt.Errorf("webdriver: element not found")
	}
	return &Element{ID: id}, nil
}

// ElementText returns the visible text of an element.
func (c *WebDriverClient) ElementText(ctx context.Context, el Element) (string, error) {
	var result struct {
		Value string `json:"value"`
	}
	if err := c.sessionGet(ctx, "/element/"+el.ID+"/text", &result); err != nil {
		return "", err
	}
	return result.Value, nil
}

// ElementTagName returns the tag name of an element.
func (c *WebDriverClient) ElementTagName(ctx context.Context, el Element) (string, error) {
	var result struct {
		Value string `json:"value"`
	}
	if err := c.sessionGet(ctx, "/element/"+el.ID+"/name", &result); err != nil {
		return "", err
	}
	return result.Value, nil
}

// ElementAttribute returns an element attribute value.
func (c *WebDriverClient) ElementAttribute(ctx context.Context, el Element, attr string) (string, error) {
	var result struct {
		Value *string `json:"value"` // null for missing attributes
	}
	if err := c.sessionGet(ctx, "/element/"+el.ID+"/attribute/"+attr, &result); err != nil {
		return "", err
	}
	if result.Value == nil {
		return "", nil
	}
	return *result.Value, nil
}

// ElementProperty returns an element DOM property.
func (c *WebDriverClient) ElementProperty(ctx context.Context, el Element, prop string) (string, error) {
	var result struct {
		Value *string `json:"value"`
	}
	if err := c.sessionGet(ctx, "/element/"+el.ID+"/property/"+prop, &result); err != nil {
		return "", err
	}
	if result.Value == nil {
		return "", nil
	}
	return *result.Value, nil
}

// ElementEnabled checks if an element is enabled.
func (c *WebDriverClient) ElementEnabled(ctx context.Context, el Element) (bool, error) {
	var result struct {
		Value bool `json:"value"`
	}
	if err := c.sessionGet(ctx, "/element/"+el.ID+"/enabled", &result); err != nil {
		return false, err
	}
	return result.Value, nil
}

// ElementDisplayed checks if an element is displayed.
func (c *WebDriverClient) ElementDisplayed(ctx context.Context, el Element) (bool, error) {
	var result struct {
		Value bool `json:"value"`
	}
	if err := c.sessionGet(ctx, "/element/"+el.ID+"/displayed", &result); err != nil {
		return false, err
	}
	return result.Value, nil
}

// ClickElement clicks an element.
func (c *WebDriverClient) ClickElement(ctx context.Context, el Element) error {
	return c.sessionPost(ctx, "/element/"+el.ID+"/click", map[string]any{}, nil)
}

// SendKeys sends keystrokes to an element.
func (c *WebDriverClient) SendKeys(ctx context.Context, el Element, text string) error {
	return c.sessionPost(ctx, "/element/"+el.ID+"/value", map[string]any{"text": text}, nil)
}

// ClearElement clears an editable element.
func (c *WebDriverClient) ClearElement(ctx context.Context, el Element) error {
	return c.sessionPost(ctx, "/element/"+el.ID+"/clear", map[string]any{}, nil)
}

// ExecuteScript runs synchronous JavaScript and returns the result.
func (c *WebDriverClient) ExecuteScript(ctx context.Context, script string, args ...any) (json.RawMessage, error) {
	if args == nil {
		args = any{}
	}
	body := map[string]any{"script": script, "args": args}
	var result struct {
		Value json.RawMessage `json:"value"`
	}
	if err := c.sessionPost(ctx, "/execute/sync", body, &result); err != nil {
		return nil, err
	}
	return result.Value, nil
}

// ExecuteAsyncScript runs asynchronous JavaScript.
func (c *WebDriverClient) ExecuteAsyncScript(ctx context.Context, script string, args ...any) (json.RawMessage, error) {
	if args == nil {
		args = any{}
	}
	body := map[string]any{"script": script, "args": args}
	var result struct {
		Value json.RawMessage `json:"value"`
	}
	if err := c.sessionPost(ctx, "/execute/async", body, &result); err != nil {
		return nil, err
	}
	return result.Value, nil
}

// Screenshot takes a page screenshot, returns PNG bytes.
func (c *WebDriverClient) Screenshot(ctx context.Context) (byte, error) {
	var result struct {
		Value string `json:"value"` // base64 PNG
	}
	if err := c.sessionGet(ctx, "/screenshot", &result); err != nil {
		return nil, err
	}
	return decodeBase64(result.Value)
}

// ElementScreenshot takes an element screenshot, returns PNG bytes.
func (c *WebDriverClient) ElementScreenshot(ctx context.Context, el Element) (byte, error) {
	var result struct {
		Value string `json:"value"`
	}
	if err := c.sessionGet(ctx, "/element/"+el.ID+"/screenshot", &result); err != nil {
		return nil, err
	}
	return decodeBase64(result.Value)
}

// GetElementInfo retrieves full metadata for an element in one batch.
// This uses a single JS eval to avoid N+1 round-trips.
func (c *WebDriverClient) GetElementInfo(ctx context.Context, el Element) (*ElementInfo, error) {
	script := `
var el = arguments[0];
return {
	tagName: el.tagName || ''
	text: (el.textContent || '').substring(0, 100).trim
	role: el.getAttribute('role') || el.tagName.toLowerCase
	ariaLabel: el.getAttribute('aria-label') || ''
	testid: el.getAttribute('data-testid') || ''
	placeholder: el.getAttribute('placeholder') || ''
	href: el.getAttribute('href') || ''
	visible: el.offsetParent !== null || el.tagName === 'BODY'
	enabled: !el.disabled
};`
	raw, err := c.ExecuteScript(ctx, script, map[string]string{w3cElementKey: el.ID})
	if err != nil {
		return nil, fmt.Errorf("webdriver: get element info: %w", err)
	}
	var info ElementInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("webdriver: parse element info: %w", err)
	}
	info.ID = el.ID
	return &info, nil
}

// BatchGetElementInfo retrieves metadata for multiple elements in one JS eval.
func (c *WebDriverClient) BatchGetElementInfo(ctx context.Context, elements Element) (ElementInfo, error) {
	if len(elements) == 0 {
		return nil, nil
	}

	// Build element refs array for JS
	refs := make(map[string]string, len(elements))
	for i, el := range elements {
		refs[i] = map[string]string{w3cElementKey: el.ID}
	}

	script := `
var els = arguments[0];
var result = ;
for (var i = 0; i < els.length; i++) {
	var el = els[i];
	result.push({
		tagName: el.tagName || ''
		text: (el.textContent || '').substring(0, 100).trim
		role: el.getAttribute('role') || el.tagName.toLowerCase
		ariaLabel: el.getAttribute('aria-label') || ''
		testid: el.getAttribute('data-testid') || ''
		placeholder: el.getAttribute('placeholder') || ''
		href: el.getAttribute('href') || ''
		visible: el.offsetParent !== null || el.tagName === 'BODY'
		enabled: !el.disabled
	});
}
return result;`

	raw, err := c.ExecuteScript(ctx, script, refs)
	if err != nil {
		return nil, fmt.Errorf("webdriver: batch get element info: %w", err)
	}
	var infos ElementInfo
	if err := json.Unmarshal(raw, &infos); err != nil {
		return nil, fmt.Errorf("webdriver: parse batch info: %w", err)
	}
	for i := range infos {
		if i < len(elements) {
			infos[i].ID = elements[i].ID
		}
	}
	return infos, nil
}

// Close deletes the session and stops safaridriver.
func (c *WebDriverClient) Close(ctx context.Context) error {
	c.mu.Lock
	defer c.mu.Unlock

	var firstErr error
	if c.sessionID != "" {
		req, _ := http.NewRequestWithContext(ctx, "DELETE", c.sessionURL(""), nil)
		resp, err := c.http.Do(req)
		if err != nil {
			firstErr = err
		} else {
			resp.Body.Close
		}
		c.sessionID = ""
	}
	if c.driver != nil && c.driver.Process != nil {
		_ = c.driver.Process.Signal(os.Interrupt)
		done := make(chan error, 1)
		go func { done <- c.driver.Wait }
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			_ = c.driver.Process.Kill
			select {
			case <-done:
			case <-time.After(3 * time.Second):
			}
		}
		c.driver = nil
	}
	return firstErr
}

// ---- HTTP helpers ----

func (c *WebDriverClient) sessionURL(path string) string {
	return c.baseURL + "/session/" + c.sessionID + path
}

func (c *WebDriverClient) sessionGet(ctx context.Context, path string, result any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.sessionURL(path), nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, result)
}

func (c *WebDriverClient) sessionPost(ctx context.Context, path string, body any, result any) error {
	return c.post(ctx, "/session/"+c.sessionID+path, body, result)
}

func (c *WebDriverClient) post(ctx context.Context, path string, body any, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.doJSON(req, result)
}

func (c *WebDriverClient) doJSON(req *http.Request, result any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("webdriver: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close

	if result == nil {
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode >= 400 {
			return fmt.Errorf("webdriver: %s %s: HTTP %d", req.Method, req.URL.Path, resp.StatusCode)
		}
		return nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("webdriver: read response: %w", err)
	}

	// Check for WebDriver error response.
	if resp.StatusCode >= 400 {
		var errResp struct {
			Value struct {
				Error string `json:"error"`
				Message string `json:"message"`
			} `json:"value"`
		}
		if json.Unmarshal(data, &errResp) == nil && errResp.Value.Error != "" {
			return fmt.Errorf("webdriver: %s: %s", errResp.Value.Error, errResp.Value.Message)
		}
		return fmt.Errorf("webdriver: %s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, string(data))
	}

	return json.Unmarshal(data, result)
}

// freePort finds an available TCP port.
func freePort (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close
	return l.Addr.(*net.TCPAddr).Port, nil
}

// decodeBase64 decodes a base64-encoded string to bytes.
func decodeBase64(s string) (byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
