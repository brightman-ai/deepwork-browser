package safari

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// Safari engine timing constants.
const (
	wdSettleAfterAction = 300 * time.Millisecond // Act 后等待 DOM 稳定
	wdSettleAfterFocus  = 150 * time.Millisecond // type 前等待焦点切换
	wdNavigateTimeout   = 15 * time.Second       // Navigate 最大等待

	// P2: AX bridge timing (保留，iOS 模式备用)
	axPollInterval = 400 * time.Millisecond
	axPollTimeout  = 10 * time.Second
)

// SafariOptions 创建 SafariBrowserCore 的选项。
type SafariOptions struct {
	// DeviceQuery 指定 iOS Simulator 设备（族类/名称/UDID）。
	// 空值 = macOS Safari 桌面模式（不需要 simctl）。
	DeviceQuery string
	// UDID 直接指定设备 UDID（优先于 DeviceQuery）。
	// 用于已知 UDID 的场景（如 session 恢复）。
	UDID string
}

// SafariBrowserCore 实现 browser.BrowserCore 和 browser.SessionCore。
// 主通道：WebDriver（safaridriver）；P2 备用：AX bridge（iOS 模式）。
type SafariBrowserCore struct {
	mu     sync.Mutex
	wdc    *WebDriverClient          // 主通道：WebDriver 客户端
	wdSnap *WebDriverSnapshotBuilder // WebDriver snapshot builder

	// iOS Simulator 支持
	simctl *SimctlManager // iOS Simulator 设备管理（仅 isIOS=true 时非 nil）
	ax     *AXBridge      // P2: AX bridge fallback（仅 iOS 模式初始化）

	deviceUDID string
	deviceName string
	isIOS      bool // true=iOS Simulator, false=macOS Safari

	// session ref 存储
	refs      map[string]browser.ElementRef // ref string → ElementRef
	snapEpoch int
}

// NewSafariBrowserCore 创建 SafariBrowserCore。
//
// DeviceQuery/UDID 非空 → iOS Simulator 模式（SmartResolveDevice + safaridriver iOS 会话）。
// 两者均为空 → macOS Safari 桌面模式（safaridriver mac 会话，无 simctl）。
func NewSafariBrowserCore(ctx context.Context, opts SafariOptions) (*SafariBrowserCore, error) {
	c := &SafariBrowserCore{
		wdSnap: &WebDriverSnapshotBuilder{},
		refs:   make(map[string]browser.ElementRef),
	}

	// iOS Simulator 模式：DeviceQuery 非空或直接指定了 UDID
	iosQuery := opts.DeviceQuery
	if opts.UDID != "" {
		iosQuery = opts.UDID // UDID 优先，SmartResolveDevice 支持 UUID 格式直接解析
	}

	if iosQuery != "" {
		// iOS Simulator 模式
		simctl := NewSimctlManager()
		c.simctl = simctl

		device, err := SmartResolveDevice(ctx, simctl, iosQuery)
		if err != nil {
			return nil, fmt.Errorf("safari: %w", err)
		}
		c.deviceUDID = device.UDID
		c.deviceName = device.Name
		c.isIOS = true

		// P2: AX bridge 初始化（iOS 模式备用）
		ax := NewAXBridge()
		if _, err := ax.EnsureCompiled(); err != nil {
			// AX bridge 编译失败不阻断启动，仅记录
			ax = nil
		}
		c.ax = ax

		wdc, err := NewWebDriverClient(ctx, WebDriverOpts{
			Platform:   "iOS",
			DeviceUDID: device.UDID,
		})
		if err != nil {
			return nil, fmt.Errorf("safari: webdriver iOS session: %w", err)
		}
		c.wdc = wdc
	} else {
		// macOS Safari 桌面模式
		c.isIOS = false

		wdc, err := NewWebDriverClient(ctx, WebDriverOpts{
			Platform: "mac",
		})
		if err != nil {
			return nil, fmt.Errorf("safari: webdriver mac session: %w", err)
		}
		c.wdc = wdc
	}

	return c, nil
}

// Navigate 在 Safari 中打开 URL 并等待页面加载。
func (c *SafariBrowserCore) Navigate(ctx context.Context, url string) (*browser.Snapshot, error) {
	if c.isIOS {
		// iOS: simctl 触发 URL，WebDriver 确认加载
		if err := c.simctl.OpenURL(ctx, c.deviceUDID, url); err != nil {
			return nil, fmt.Errorf("safari navigate: %w", err)
		}
		// 等待 WebDriver 会话中 URL 变更
		if err := waitForURL(ctx, c.wdc, url, wdNavigateTimeout); err != nil {
			// 降级：直接让 WebDriver 也导航一次
			_ = c.wdc.Navigate(ctx, url)
		}
	} else {
		if err := c.wdc.Navigate(ctx, url); err != nil {
			return nil, fmt.Errorf("safari navigate: %w", err)
		}
	}
	time.Sleep(wdSettleAfterAction)
	snap, err := c.snap(ctx, false)
	if err == nil {
		return snap, nil
	}
	pageURL, _ := c.wdc.CurrentURL(ctx)
	pageTitle, _ := c.wdc.Title(ctx)
	return &browser.Snapshot{
		PageTitle:    pageTitle,
		URL:          pageURL,
		SnapshotType: "webdriver_empty",
		LoadState:    "loading",
	}, nil
}

// Snap 获取当前页面快照。
func (c *SafariBrowserCore) Snap(ctx context.Context) (*browser.Snapshot, error) {
	return c.snap(ctx, false)
}

// Act 执行操作。
func (c *SafariBrowserCore) Act(ctx context.Context, action string, observe bool) (*browser.Snapshot, error) {
	if err := c.executeAction(ctx, action); err != nil {
		return nil, err
	}
	if !observe {
		return nil, nil
	}
	time.Sleep(wdSettleAfterAction)
	return c.snap(ctx, false)
}

// Text 提取页面文本（通过 JS，无 AX 依赖）。
func (c *SafariBrowserCore) Text(ctx context.Context, focus *string) (string, error) {
	script := "return document.body ? document.body.innerText : ''"
	if focus != nil && *focus != "" {
		script = fmt.Sprintf(
			`var el = document.querySelector(%q); return el ? el.innerText : document.body.innerText`,
			*focus,
		)
	}
	raw, err := c.wdc.ExecuteScript(ctx, script)
	if err != nil {
		return "", fmt.Errorf("safari text: %w", err)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		// raw 可能不是字符串（如 null），返回空
		return "", nil
	}
	return text, nil
}

// Screenshot 截图。
// iOS 模式优先用 simctl（保留设备级截图语义）；macOS 模式用 WebDriver。
func (c *SafariBrowserCore) Screenshot(ctx context.Context, annotate bool) ([]byte, error) {
	if c.isIOS && c.simctl != nil {
		return c.simctl.Screenshot(ctx, c.deviceUDID)
	}
	return c.wdc.Screenshot(ctx)
}

// EvalJS 通过 WebDriver execute/sync 执行 JavaScript。
func (c *SafariBrowserCore) EvalJS(ctx context.Context, expr string, result interface{}) error {
	script := "return (" + expr + ")"
	trimmed := strings.TrimSpace(expr)
	if strings.HasPrefix(trimmed, "const __params = ") || strings.HasPrefix(trimmed, "var __params = ") {
		if idx := strings.Index(trimmed, "\n"); idx >= 0 {
			paramsLine := trimmed[:idx]
			body := strings.TrimSpace(trimmed[idx+1:])
			script = paramsLine + "\nreturn (" + body + ")"
		}
	}
	raw, err := c.wdc.ExecuteScript(ctx, script)
	if err != nil && script != expr {
		raw, err = c.wdc.ExecuteScript(ctx, expr)
	}
	if err != nil {
		return fmt.Errorf("safari evaljs: %w", err)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(raw, result); err != nil {
		return fmt.Errorf("safari evaljs: unmarshal result: %w", err)
	}
	return nil
}

// StartLiveView 不支持。
func (c *SafariBrowserCore) StartLiveView(ctx context.Context) (*browser.FrameBroadcastHub, error) {
	return nil, ErrSafariLiveViewNotSupported
}

// StopLiveView no-op。
func (c *SafariBrowserCore) StopLiveView(ctx context.Context) error {
	return nil
}

// EnableTakeover 不支持。
func (c *SafariBrowserCore) EnableTakeover(ctx context.Context) error {
	return ErrSafariTakeoverNotSupported
}

// DisableTakeover no-op。
func (c *SafariBrowserCore) DisableTakeover(ctx context.Context) error {
	return nil
}

// DispatchInput 不支持。
func (c *SafariBrowserCore) DispatchInput(ctx context.Context, event *browser.InputEvent) error {
	return ErrSafariTakeoverNotSupported
}

// Close 关闭 WebDriver 会话。iOS 模式会结束 MobileSafari，以释放 WebDriver pairing。
func (c *SafariBrowserCore) Close(ctx context.Context) error {
	var firstErr error
	if c.isIOS && c.simctl != nil && c.deviceUDID != "" {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.simctl.TerminateApp(cleanupCtx, c.deviceUDID, "com.apple.mobilesafari"); err != nil {
			firstErr = err
		}
		cleanupCancel()
	}
	if err := c.wdc.Close(ctx); err != nil {
		if firstErr == nil {
			firstErr = err
		}
	}
	if c.isIOS && c.simctl != nil && c.deviceUDID != "" {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.simctl.TerminateApp(cleanupCtx, c.deviceUDID, "com.apple.mobilesafari"); err != nil && firstErr == nil {
			firstErr = err
		}
		cleanupCancel()
	}
	return firstErr
}

// GetTargetTracker Safari 不支持 target tracking。
func (c *SafariBrowserCore) GetTargetTracker() *browser.TargetTracker {
	return nil
}

// ============================================================
// SessionCore 接口实现
// ============================================================

// SnapWithSessionMode 获取 session 模式快照（@rN ref 格式）。
func (c *SafariBrowserCore) SnapWithSessionMode(ctx context.Context, snapEpoch int) (*browser.Snapshot, error) {
	c.snapEpoch++
	return c.snap(ctx, true)
}

// SnapWithOptions 获取带选项的快照。
func (c *SafariBrowserCore) SnapWithOptions(ctx context.Context, opts browser.SnapOptions) (*browser.Snapshot, error) {
	return c.SnapWithSessionMode(ctx, 0)
}

// ActWithSessionMode 执行 session 模式操作。
func (c *SafariBrowserCore) ActWithSessionMode(ctx context.Context, action string, observe bool) (*browser.Snapshot, error) {
	if err := c.executeAction(ctx, action); err != nil {
		return nil, err
	}
	if !observe {
		return nil, nil
	}
	time.Sleep(wdSettleAfterAction)
	return c.snap(ctx, true)
}

// RestoreRefsFromSession 从 session 恢复 ref 表。
func (c *SafariBrowserCore) RestoreRefsFromSession(refs []browser.SessionRef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refs = make(map[string]browser.ElementRef, len(refs))
	for _, r := range refs {
		loc := r.Locator
		if loc.Engine == "" {
			loc = browser.NodeLocator{
				Engine:    browser.EngineSafari,
				AXPath:    r.AXPath,
				StableKey: r.StableKey,
			}
		}
		c.refs[r.Ref] = browser.ElementRef{
			Ref:          r.Ref,
			Locator:      loc,
			AXPath:       loc.AXPath,
			Role:         r.Role,
			Name:         r.Name,
			NameFull:     r.Name,
			NameShort:    truncateNameStr(r.Name, 50),
			TestID:       r.TestID,
			Placeholder:  r.Placeholder,
			Interactable: true,
		}
	}
}

// DeviceUDID 返回当前设备 UDID（iOS 模式）。
func (c *SafariBrowserCore) DeviceUDID() string { return c.deviceUDID }

// DeviceName 返回当前设备名称（iOS 模式）。
func (c *SafariBrowserCore) DeviceName() string { return c.deviceName }

// ============================================================
// 内部方法
// ============================================================

// snap 内部 snapshot 实现，sessionMode 控制 ref 格式。
func (c *SafariBrowserCore) snap(ctx context.Context, sessionMode bool) (*browser.Snapshot, error) {
	snap, err := c.wdSnap.Build(ctx, c.wdc, sessionMode)
	if err != nil {
		return nil, fmt.Errorf("safari snap: %w", err)
	}
	c.updateRefs(snap)
	return snap, nil
}

// updateRefs 更新内部 ref 表。
func (c *SafariBrowserCore) updateRefs(snap *browser.Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refs = make(map[string]browser.ElementRef, len(snap.Refs))
	for _, ref := range snap.Refs {
		c.refs[ref.Ref] = ref
	}
}

// executeAction 解析并执行 action。
func (c *SafariBrowserCore) executeAction(ctx context.Context, action string) error {
	parts := strings.Fields(action)
	if len(parts) == 0 {
		return fmt.Errorf("%w: empty action", browser.ErrActFailed)
	}
	op := strings.ToLower(parts[0])

	switch op {
	case "click":
		if len(parts) < 2 {
			return fmt.Errorf("%w: click requires target", browser.ErrActFailed)
		}
		return c.executeClick(ctx, parts[1])

	case "type", "fill":
		if len(parts) < 3 {
			return fmt.Errorf("%w: type requires target and value", browser.ErrActFailed)
		}
		target := parts[1]
		value := extractValue(action, target)
		return c.executeType(ctx, target, value)

	case "scroll":
		if len(parts) < 2 {
			return fmt.Errorf("%w: scroll requires direction", browser.ErrActFailed)
		}
		return c.executeScroll(ctx, parts[1])

	case "back":
		_, err := c.wdc.ExecuteScript(ctx, "window.history.back()")
		return err

	case "forward":
		_, err := c.wdc.ExecuteScript(ctx, "window.history.forward()")
		return err

	default:
		return fmt.Errorf("%w: unsupported action %q in Safari engine", browser.ErrActFailed, op)
	}
}

// executeClick 执行点击操作。
// 解析 target → WebDriver element → ClickElement。
// 失败时 fallback 到 re-snap + 通过 stableKey/testid 重定位。
func (c *SafariBrowserCore) executeClick(ctx context.Context, target string) error {
	el, err := c.resolveElement(ctx, target)
	if err != nil {
		return err
	}
	if err := c.wdc.ClickElement(ctx, *el); err != nil {
		// Stale element: re-snap 一次后重试
		snap, snapErr := c.snap(ctx, false)
		if snapErr != nil {
			return fmt.Errorf("%w: click failed and re-snap also failed: %v", browser.ErrActFailed, err)
		}
		_ = snap
		el2, err2 := c.resolveElement(ctx, target)
		if err2 != nil {
			return fmt.Errorf("%w: element became stale after snap: %v", browser.ErrStaleRef, err)
		}
		return c.wdc.ClickElement(ctx, *el2)
	}
	return nil
}

// executeType 执行输入操作。
func (c *SafariBrowserCore) executeType(ctx context.Context, target, value string) error {
	el, err := c.resolveElement(ctx, target)
	if err != nil {
		return fmt.Errorf("safari type: focus failed: %w", err)
	}
	if err := c.wdc.ClearElement(ctx, *el); err != nil {
		// 忽略 clear 失败（非 input 元素可能不支持）
		_ = err
	}
	time.Sleep(wdSettleAfterFocus)
	return c.wdc.SendKeys(ctx, *el, value)
}

// executeScroll 执行滚动（JS window.scrollBy）。
func (c *SafariBrowserCore) executeScroll(ctx context.Context, direction string) error {
	var script string
	switch strings.ToLower(direction) {
	case "down":
		script = "window.scrollBy(0, 300)"
	case "up":
		script = "window.scrollBy(0, -300)"
	case "left":
		script = "window.scrollBy(-300, 0)"
	case "right":
		script = "window.scrollBy(300, 0)"
	default:
		return fmt.Errorf("%w: unknown scroll direction %q", browser.ErrActFailed, direction)
	}
	_, err := c.wdc.ExecuteScript(ctx, script)
	return err
}

// resolveElement 解析 target 到 WebDriver Element。
//
// 解析优先级:
//  1. @rN / eN — 从 refs 表取 stableKey/testid，通过 WebDriver 重新定位
//  2. #testid — css [data-testid='...']
//  3. role:'name' — ARIA 语义选择器（JS 查询）
//  4. bare role — 取第一个匹配的 interactable ref
func (c *SafariBrowserCore) resolveElement(ctx context.Context, target string) (*Element, error) {
	c.mu.Lock()
	refs := c.refs
	c.mu.Unlock()

	// @rN / eN — ref 表查找 → 通过 stableKey 定位
	isRef := strings.HasPrefix(target, "@r") ||
		(len(target) > 1 && target[0] == 'e' && target[1] >= '0' && target[1] <= '9')
	if isRef {
		ref, ok := refs[target]
		if !ok {
			return nil, fmt.Errorf("%w: ref %q not found", browser.ErrRefNotFound, target)
		}
		return c.elementFromRef(ctx, ref)
	}

	// #testid
	if strings.HasPrefix(target, "#") {
		testID := target[1:]
		return c.wdc.FindElement(ctx, "css selector", fmt.Sprintf("[data-testid='%s']", testID))
	}

	// role:'name' 语义选择器
	if idx := strings.IndexByte(target, ':'); idx > 0 {
		role := target[:idx]
		name := strings.Trim(target[idx+1:], "'\"")
		// 先从 refs 表匹配
		for _, ref := range refs {
			if ref.Role == role && strings.Contains(ref.Name, name) {
				el, err := c.elementFromRef(ctx, ref)
				if err == nil {
					return el, nil
				}
			}
		}
		return nil, fmt.Errorf("%w: %s with name containing %q not found", browser.ErrRefNotFound, role, name)
	}

	// bare role — 取第一个 interactable 匹配
	for _, ref := range refs {
		if ref.Role == target && ref.Interactable {
			el, err := c.elementFromRef(ctx, ref)
			if err == nil {
				return el, nil
			}
		}
	}

	return nil, fmt.Errorf("%w: target %q not found in current snapshot", browser.ErrRefNotFound, target)
}

// elementFromRef 通过 ElementRef 定位 WebDriver Element。
// 优先用 testid；fallback 用 stableKey 中的 role+name；最后用 ordinal。
func (c *SafariBrowserCore) elementFromRef(ctx context.Context, ref browser.ElementRef) (*Element, error) {
	// 快速路径：testid
	if ref.TestID != "" {
		el, err := c.wdc.FindElement(ctx, "css selector", fmt.Sprintf("[data-testid='%s']", ref.TestID))
		if err == nil {
			return el, nil
		}
	}

	// 中速路径：role + name（ARIA 语义）
	if ref.Role != "" && ref.Name != "" {
		el, err := c.findByRoleAndName(ctx, ref.Role, ref.Name)
		if err == nil {
			return el, nil
		}
	}

	// 慢路径：ordinal（按序号取第 N 个交互元素，最脆弱）
	if ref.Locator.Ordinal > 0 {
		elements, err := c.wdc.FindElements(ctx, "css selector",
			"button,a,input,textarea,select,[role],[tabindex]")
		if err == nil {
			visible := 0
			for _, el := range elements {
				displayed, _ := c.wdc.ElementDisplayed(ctx, el)
				enabled, _ := c.wdc.ElementEnabled(ctx, el)
				if displayed && enabled {
					visible++
					if visible == ref.Locator.Ordinal {
						return &el, nil
					}
				}
			}
		}
	}

	return nil, fmt.Errorf("%w: cannot locate element for ref %q", browser.ErrStaleRef, ref.Ref)
}

// findByRoleAndName 通过 ARIA role + name 在 DOM 中定位元素。
func (c *SafariBrowserCore) findByRoleAndName(ctx context.Context, role, name string) (*Element, error) {
	script := `
var role = arguments[0], name = arguments[1];
var candidates = document.querySelectorAll('button,a,input,textarea,select,[role]');
for (var i = 0; i < candidates.length; i++) {
	var el = candidates[i];
	var elRole = el.getAttribute('role') || el.tagName.toLowerCase();
	var elName = (el.getAttribute('aria-label') || el.textContent || '').trim();
	if (elRole === role && elName.indexOf(name) >= 0) {
		return el;
	}
}
return null;`
	raw, err := c.wdc.ExecuteScript(ctx, script, role, name)
	if err != nil {
		return nil, err
	}
	// WebDriver 返回 element 引用时格式为 {"element-6066-...": "id"}
	var ref map[string]string
	if err := json.Unmarshal(raw, &ref); err != nil || len(ref) == 0 {
		return nil, fmt.Errorf("webdriver: role+name not found: %s %q", role, name)
	}
	for _, id := range ref {
		return &Element{ID: id}, nil
	}
	return nil, fmt.Errorf("webdriver: role+name not found: %s %q", role, name)
}

// waitForURL 等待 WebDriver 会话中当前 URL 包含 target（用于 iOS simctl 触发后确认）。
func waitForURL(ctx context.Context, wdc *WebDriverClient, target string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cur, err := wdc.CurrentURL(ctx)
		if err == nil && strings.Contains(cur, stripScheme(target)) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(axPollInterval):
		}
	}
	return fmt.Errorf("safari: URL did not change to %q within %s", target, timeout)
}

// stripScheme 去除 URL scheme 以便宽松匹配。
func stripScheme(url string) string {
	if i := strings.Index(url, "://"); i >= 0 {
		return url[i+3:]
	}
	return url
}

// extractValue 从 action 字符串中提取引号内的值。
func extractValue(action, afterTarget string) string {
	idx := strings.Index(action, afterTarget)
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(action[idx+len(afterTarget):])
	rest = strings.Trim(rest, "'\"")
	return rest
}

// ============================================================
// P2: AX bridge fallback（iOS 模式，备用）
// ============================================================

// revalidateRef 重新验证 ref 是否仍然有效（P2: AX bridge fallback）。
func (c *SafariBrowserCore) revalidateRef(ctx context.Context, ref browser.ElementRef) (browser.ElementRef, error) {
	if c.ax == nil || ref.AXPath == "" {
		return ref, nil
	}
	node, err := c.ax.Inspect(ctx, c.deviceUDID, ref.AXPath)
	if err != nil {
		return c.relocateByStableKey(ctx, ref)
	}
	role := mapAXRoleToARIA(node.Role)
	name := accessibleNameFromAX(node)
	testID := ""
	if node.Identifier != nil {
		testID = *node.Identifier
	}
	placeholder := placeholderFromAX(node)
	currentKey := buildStableKey(role, name, testID, placeholder, node.Frame)
	if currentKey == ref.Locator.StableKey {
		ref.Locator.Frame = browser.Rect{X: node.Frame.X, Y: node.Frame.Y, Width: node.Frame.Width, Height: node.Frame.Height}
		return ref, nil
	}
	return c.relocateByStableKey(ctx, ref)
}

// relocateByStableKey 用 StableKey 在当前 AX 树中重新定位元素（P2: AX bridge fallback）。
func (c *SafariBrowserCore) relocateByStableKey(ctx context.Context, oldRef browser.ElementRef) (browser.ElementRef, error) {
	if c.ax == nil {
		return browser.ElementRef{}, fmt.Errorf("%w: AX bridge not available", browser.ErrStaleRef)
	}
	builder := NewSnapshotBuilder()
	tree, err := c.ax.Dump(ctx, c.deviceUDID, 12)
	if err != nil {
		return browser.ElementRef{}, fmt.Errorf("safari relocate: %w", err)
	}
	snap := builder.Build(tree, true)
	for _, ref := range snap.Refs {
		if ref.Locator.StableKey == oldRef.Locator.StableKey {
			return ref, nil
		}
	}
	for _, ref := range snap.Refs {
		if ref.Role == oldRef.Role && ref.Name == oldRef.Name {
			return ref, nil
		}
	}
	return browser.ElementRef{}, fmt.Errorf("%w: element %q has moved; run snap again", browser.ErrStaleRef, oldRef.Ref)
}
