package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chromedp/cdproto/accessibility"
	cdp "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// ============================================================
// § SnapshotEngine [Ref: CAP-BS09-C2, T5-B4]
// ============================================================

// snapshotEngine 实现 A11y 快照获取与 Fallback 链 [IR-07]。
type snapshotEngine struct {
	// refTable 当前快照的 Ref → BackendNodeID 映射（每次 snap 后重建）
	refTable map[string]int64
	// refMeta Ref → ElementRef 完整信息（DOM 发现的 clickable 用 name 做 CSS 选择器回退）
	refMeta map[string]*ElementRef
}

const (
	snapshotTypeA11y               = "a11y"
	snapshotTypeDOMFallback        = "dom_fallback"
	snapshotTypeScreenshotFallback = "screenshot_fallback"
	snapshotTypeProgressiveLoading = "progressive_loading"

	loadStateActionable     = "actionable"
	loadStateReadable       = "readable"
	loadStateVisual         = "visual"
	loadStateLoading        = "loading"
	loadStateWaitingForApp  = "waiting_for_app"
	loadStateUnavailable    = "unavailable"
	progressiveRetryDefault = 1000
)

const (
	a11yProbeTimeout       = 8 * time.Second
	snapshotBasicTimeout   = 2 * time.Second
	domFallbackTimeout     = 2500 * time.Millisecond
	screenshotProbeTimeout = 1800 * time.Millisecond
	pageStateProbeTimeout  = 1200 * time.Millisecond
	jsEnrichmentTimeout    = 1200 * time.Millisecond
	selectorResolveTimeout = 3 * time.Second
	partialAXProbeTimeout  = 5 * time.Second
)

type pageLoadProbe struct {
	URL             string `json:"url"`
	Title           string `json:"title"`
	ReadyState      string `json:"readyState"`
	VisibilityState string `json:"visibilityState"`
	HasBody         bool   `json:"hasBody"`
	BodyTextLength  int    `json:"bodyTextLength"`
	BodyChildCount  int    `json:"bodyChildCount"`
	ViewportWidth   int    `json:"viewportWidth"`
	ViewportHeight  int    `json:"viewportHeight"`
}

// newSnapshotEngine 创建 SnapshotEngine 实例。
func newSnapshotEngine() *snapshotEngine {
	return &snapshotEngine{
		refTable: make(map[string]int64),
		refMeta:  make(map[string]*ElementRef),
	}
}

func (e *snapshotEngine) clearRefs() {
	e.refTable = make(map[string]int64)
	e.refMeta = make(map[string]*ElementRef)
}

// LookupRefMeta 获取 ref 的完整元数据（用于 clickable 类型的 CSS 选择器回退）。
func (e *snapshotEngine) LookupRefMeta(ref string) (*ElementRef, bool) {
	if len(e.refMeta) == 0 {
		return nil, false
	}
	meta, ok := e.refMeta[ref]
	return meta, ok
}

// GetSnapshot 获取当前页面 A11y 快照，带 Fallback 链 [IR-07, TC-09-U-03, TC-09-U-04]。
//
// Fallback 链:
//  1. CDP Accessibility.getFullAXTree → 过滤 → DFS Refs 分配 → compact 格式化
//  2. A11y Refs < 3 → DOM fallback，SnapshotType="dom_fallback" [TC-09-U-03]
//  3. DOM 也空 → screenshot fallback，SnapshotType="screenshot_fallback" [TC-09-U-04]
//  4. 页面正在水合/挑战/启动且 fallback 暂不可用 → progressive_loading，调用方稍后重试
func (e *snapshotEngine) GetSnapshot(ctx context.Context) (*Snapshot, error) {
	// 步骤 1: 获取当前 URL 和 Title
	var currentURL, title string
	basicCtx, basicCancel := context.WithTimeout(ctx, snapshotBasicTimeout)
	err := chromedp.Run(basicCtx,
		chromedp.Location(&currentURL),
		chromedp.Title(&title),
	)
	basicCancel()
	if err != nil {
		return e.progressiveFallback(ctx, currentURL, title, "basic_info_unavailable", err), nil
	}

	// 步骤 2: 获取 A11y Tree
	var axNodes []*accessibility.Node
	a11yCtx, a11yCancel := context.WithTimeout(ctx, a11yProbeTimeout)
	err = chromedp.Run(a11yCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		var fetchErr error
		axNodes, fetchErr = accessibility.GetFullAXTree().Do(ctx)
		return fetchErr
	}))
	a11yCancel()
	if err != nil {
		// A11y 获取失败，走 DOM fallback
		return e.domFallback(ctx, currentURL, title, "a11y_unavailable", err)
	}

	// 步骤 3: 过滤 + DFS Refs 分配
	refs := extractInteractableRefs(axNodes)

	// 步骤 4: 检查 Refs 数量，< 3 时走 fallback [IR-07, TC-09-U-03]
	if len(refs) < 3 {
		return e.domFallback(ctx, currentURL, title, "a11y_insufficient_refs", nil)
	}

	// 步骤 4.5: 纯 JS enrichment + DOM-only 元素发现
	// TH-0405-p7c 验证结论: CDP DOM 域 AND DOMSnapshot 域 均破坏 chromedp Click 状态。
	// 唯一安全路径: Accessibility.getFullAXTree + Runtime.evaluate（JS 执行）。
	// 文本匹配是该约束下的最优解，非降级（业界 Playwright 也用 JS 计算 role/name）。
	enrichTestIDsViaJS(ctx, refs)
	domRefs := discoverClickableDOMViaJS(ctx, refs)
	if len(domRefs) > 0 {
		refs = append(refs, domRefs...)
	}

	// 步骤 5: 构建 compact 文本 + TokenEst
	text := buildCompactText(refs)
	tokenEst := estimateTokens(text)

	// 步骤 6: 计算 MatchCount（同 role+name 的元素数量）
	type roleNameKey struct{ role, name string }
	roleNameCount := make(map[roleNameKey]int, len(refs))
	for i := range refs {
		k := roleNameKey{refs[i].Role, refs[i].Name}
		roleNameCount[k]++
	}
	for i := range refs {
		k := roleNameKey{refs[i].Role, refs[i].Name}
		refs[i].MatchCount = roleNameCount[k]
	}

	// 步骤 6.1: 更新 refTable + refMeta（每次 snap 重建）
	e.refTable = make(map[string]int64, len(refs))
	e.refMeta = make(map[string]*ElementRef, len(refs)*2)
	for i := range refs {
		e.refTable[refs[i].Ref] = refs[i].BackendNodeID
		e.refMeta[refs[i].Ref] = &refs[i]
		// 同时按 TestID 索引，供 resolveSelector "#testid" 查找
		if refs[i].TestID != "" {
			e.refMeta["#"+refs[i].TestID] = &refs[i]
		}
	}

	// 步骤 6.2: 计算 RecommendedLocator
	for i := range refs {
		refs[i].RecommendedLocator = computeRecommendedLocator(&refs[i])
	}

	return &Snapshot{
		PageTitle:    title,
		URL:          currentURL,
		Text:         text,
		Refs:         refs,
		SnapshotType: snapshotTypeA11y,
		TokenEst:     tokenEst,
		LoadState:    loadStateActionable,
	}, nil
}

// domFallback 当 A11y Refs < 3 时的 DOM fallback [TC-09-U-03]。
func (e *snapshotEngine) domFallback(ctx context.Context, url, title, reason string, cause error) (*Snapshot, error) {
	var rawText string
	domCtx, domCancel := context.WithTimeout(ctx, domFallbackTimeout)
	err := chromedp.Run(domCtx, chromedp.Evaluate(`(() => {
		const body = document.body;
		if (!body) return "";
		const text = (body.innerText || body.textContent || "").trim();
		return text.slice(0, 12000);
	})()`, &rawText))
	domCancel()
	text := normalizeVisibleText(rawText)
	if err != nil || text == "" {
		// DOM 也空，走 screenshot fallback [TC-09-U-04]
		if err != nil {
			return e.screenshotFallback(ctx, url, title, reason+":dom_unavailable", err)
		}
		return e.screenshotFallback(ctx, url, title, reason+":dom_empty", cause)
	}

	if len(strings.TrimSpace(text)) < 10 {
		return e.screenshotFallback(ctx, url, title, reason+":dom_text_short", cause)
	}

	tokenEst := estimateTokens(text)
	e.clearRefs()

	return &Snapshot{
		PageTitle:    title,
		URL:          url,
		Text:         text,
		Refs:         nil,
		SnapshotType: snapshotTypeDOMFallback,
		TokenEst:     tokenEst,
		LoadState:    loadStateReadable,
	}, nil
}

// screenshotFallback 当 A11y + DOM 均空时的截图 fallback [TC-09-U-04]。
// 使用 CaptureScreenshot 仅截取当前 viewport，不修改 DeviceMetrics。
func (e *snapshotEngine) screenshotFallback(ctx context.Context, url, title, reason string, cause error) (*Snapshot, error) {
	// 仅验证页面可截图（数据不使用），显式 JPEG 格式匹配前端 (Codex #3)
	shotCtx, shotCancel := context.WithTimeout(ctx, screenshotProbeTimeout)
	err := chromedp.Run(shotCtx, chromedp.ActionFunc(func(actCtx context.Context) error {
		_, e := page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatJpeg).WithQuality(80).Do(actCtx)
		return e
	}))
	shotCancel()
	if err != nil {
		if cause != nil {
			return e.progressiveFallback(ctx, url, title, reason+":screenshot_unavailable", fmt.Errorf("%v; screenshot fallback failed: %w", cause, err)), nil
		}
		return e.progressiveFallback(ctx, url, title, reason+":screenshot_unavailable", err), nil
	}

	e.clearRefs()
	return &Snapshot{
		PageTitle:    title,
		URL:          url,
		Text:         "[screenshot]",
		Refs:         nil,
		SnapshotType: snapshotTypeScreenshotFallback,
		TokenEst:     estimateTokens("[screenshot]"),
		LoadState:    loadStateVisual,
	}, nil
}

func (e *snapshotEngine) progressiveFallback(ctx context.Context, url, title, reason string, cause error) *Snapshot {
	probe, _ := probePageLoadState(ctx)
	if probe.URL != "" {
		url = probe.URL
	}
	if probe.Title != "" {
		title = probe.Title
	}
	loadState := deriveProgressiveLoadState(probe)
	diagnostics := map[string]interface{}{
		"reason":           reason,
		"document_ready":   probe.ReadyState,
		"body_text_length": probe.BodyTextLength,
		"body_child_count": probe.BodyChildCount,
		"has_body":         probe.HasBody,
		"viewport_width":   probe.ViewportWidth,
		"viewport_height":  probe.ViewportHeight,
	}
	if probe.VisibilityState != "" {
		diagnostics["visibility_state"] = probe.VisibilityState
	}
	if cause != nil {
		diagnostics["cause"] = cause.Error()
	}
	text := fmt.Sprintf(
		"[progressive_loading ready_state=%s load_state=%s refs=0 body_text=%d retry_after_ms=%d reason=%s]",
		valueOrUnknown(probe.ReadyState),
		loadState,
		probe.BodyTextLength,
		progressiveRetryDefault,
		reason,
	)
	e.clearRefs()
	return &Snapshot{
		PageTitle:        title,
		URL:              url,
		Text:             text,
		Refs:             nil,
		SnapshotType:     snapshotTypeProgressiveLoading,
		TokenEst:         estimateTokens(text),
		Progressive:      true,
		LoadState:        loadState,
		ReadyState:       probe.ReadyState,
		RetryAfterMillis: progressiveRetryDefault,
		ProgressReason:   reason,
		Diagnostics:      diagnostics,
	}
}

func probePageLoadState(ctx context.Context) (pageLoadProbe, error) {
	var probe pageLoadProbe
	probeCtx, cancel := context.WithTimeout(ctx, pageStateProbeTimeout)
	defer cancel()
	js := `(() => {
		const body = document.body;
		const text = body ? ((body.innerText || body.textContent || '').trim()) : '';
		return {
			url: location.href || '',
			title: document.title || '',
			readyState: document.readyState || '',
			visibilityState: document.visibilityState || '',
			hasBody: !!body,
			bodyTextLength: text.length,
			bodyChildCount: body ? body.children.length : 0,
			viewportWidth: window.innerWidth || 0,
			viewportHeight: window.innerHeight || 0
		};
	})()`
	err := chromedp.Run(probeCtx, chromedp.Evaluate(js, &probe))
	return probe, err
}

func deriveProgressiveLoadState(probe pageLoadProbe) string {
	switch probe.ReadyState {
	case "loading", "":
		return loadStateLoading
	case "interactive", "complete":
		if !probe.HasBody || (probe.BodyTextLength == 0 && probe.BodyChildCount == 0) {
			return loadStateLoading
		}
		return loadStateWaitingForApp
	default:
		return loadStateUnavailable
	}
}

func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}

// GetText 提取当前页面纯文本（~500-800 tok）[Ref: T5-B4]。
func (e *snapshotEngine) GetText(ctx context.Context, focus *string) (string, error) {
	if focus != nil && *focus != "" {
		// focus 可以是 Ref（如 "e3"）或 CSS selector
		sel := *focus
		if strings.HasPrefix(sel, "e") && len(sel) <= 5 {
			// 可能是 Ref，忽略 focus 使用全页面文本
		}
		var text string
		err := chromedp.Run(ctx, chromedp.Text(sel, &text, chromedp.ByQuery))
		if err == nil && strings.TrimSpace(text) != "" {
			return text, nil
		}
	}

	// 全页面文本
	var bodyText string
	err := chromedp.Run(ctx, chromedp.Text("body", &bodyText, chromedp.ByQuery))
	if err != nil {
		return "", fmt.Errorf("browser: text extraction failed: %w", err)
	}
	return bodyText, nil
}

// Screenshot 截图，annotate=true 时叠加 Element Ref 标注（当前 Phase 不支持标注）。
// 使用 CaptureScreenshot 仅截取当前 viewport，不修改 DeviceMetrics。
// 不使用 FullScreenshot — 其内部调用 SetDeviceMetricsOverride 修改 viewport 为全文档高度，
// 导致 Screencast 帧变成全页面高度（BUG-1）、坐标映射错误（BUG-5）、帧过大 WS 失败（BUG-6）。
func (e *snapshotEngine) Screenshot(ctx context.Context, annotate bool) ([]byte, error) {
	var buf []byte
	// 显式指定 JPEG 格式 — chromedp.CaptureScreenshot 默认 PNG，前端按 JPEG 解码 (Codex #3)
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(actCtx context.Context) error {
		data, e := page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatJpeg).WithQuality(80).Do(actCtx)
		buf = data
		return e
	}))
	if err != nil {
		return nil, fmt.Errorf("browser: screenshot failed: %w", err)
	}
	return buf, nil
}

// mergeWithDOMSnapshot 已废弃。
// TH-0405-p7c 实测: DOMSnapshot.captureSnapshot 虽属独立 CDP 域，
// 仍然破坏 chromedp 的 Click 操作（与 DOM.describeNode 同样症状）。
// DDC-I-05: chromedp 中任何触发 CDP DOM 树遍历的 API（DOM / DOMSnapshot 域）
// 均会修改内部 NodeID 缓存，导致后续 Click 超时。
// 唯一安全路径: Accessibility.getFullAXTree + Runtime.evaluate。

// LookupRef 通过 Ref 字符串查找 BackendNodeID [TC-09-U-27]。
func (e *snapshotEngine) LookupRef(ref string) (int64, bool) {
	if len(e.refTable) == 0 {
		return 0, false
	}
	nodeID, ok := e.refTable[ref]
	return nodeID, ok
}

// LookupByTestID 通过 data-testid 查找 ElementRef。
func (e *snapshotEngine) LookupByTestID(testID string) (*ElementRef, bool) {
	if len(e.refMeta) == 0 {
		return nil, false
	}
	meta, ok := e.refMeta["#"+testID]
	return meta, ok
}

// LookupByRoleName 通过 role + name 查找第一个匹配的 ElementRef。
// nameContains 为空时只按 role 匹配。
func (e *snapshotEngine) LookupByRoleName(role, nameContains string) (*ElementRef, bool) {
	all := e.LookupAllByRoleName(role, nameContains, "*=")
	if len(all) == 0 {
		return nil, false
	}
	return all[0], true
}

// LookupAllByRoleName 通过 role + name 查找所有匹配的 ElementRef。
// nameContains 为空时只按 role 匹配。
// op: "=" exact | "*=" contains | "^=" prefix (default "*=")
func (e *snapshotEngine) LookupAllByRoleName(role, name, op string) []*ElementRef {
	var results []*ElementRef
	seen := make(map[string]bool)
	for key, meta := range e.refMeta {
		// 跳过 testid 索引条目（key 以 # 开头）
		if strings.HasPrefix(key, "#") {
			continue
		}
		// 避免同一 ref 重复匹配（refMeta 中 eN 和 @rN 可能指向同一元素）
		if seen[meta.Ref] {
			continue
		}
		if !strings.EqualFold(meta.Role, role) {
			continue
		}
		if name == "" {
			seen[meta.Ref] = true
			results = append(results, meta)
			continue
		}
		var match bool
		switch op {
		case "=":
			match = meta.Name == name || meta.NameFull == name
		case "^=":
			match = strings.HasPrefix(meta.Name, name) || strings.HasPrefix(meta.NameFull, name)
		default: // "*=" or empty
			match = strings.Contains(meta.Name, name) || strings.Contains(meta.NameFull, name)
		}
		if match {
			seen[meta.Ref] = true
			results = append(results, meta)
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		return extractRefNth(results[i].Ref) < extractRefNth(results[j].Ref)
	})
	return results
}

// AllTestIDs 返回当前快照中所有有 TestID 的元素的 testid 列表（用于错误提示）。
func (e *snapshotEngine) AllTestIDs() []string {
	var ids []string
	for key := range e.refMeta {
		if strings.HasPrefix(key, "#") {
			ids = append(ids, key[1:])
		}
	}
	return ids
}

// AllByRole 返回当前快照中指定 role 的所有元素名称（用于错误提示）。
func (e *snapshotEngine) AllByRole(role string) []string {
	seen := make(map[string]bool)
	var names []string
	for key, meta := range e.refMeta {
		if strings.HasPrefix(key, "#") {
			continue
		}
		if strings.EqualFold(meta.Role, role) && !seen[meta.Name] {
			seen[meta.Name] = true
			names = append(names, meta.Name)
		}
	}
	return names
}

// ============================================================
// § A11y Tree 处理函数 [Ref: T5-B4, TC-09-U-01, TC-09-U-02, TC-09-U-26]
// ============================================================

// interactableRoles 是需要保留的 ARIA role 集合 [TC-09-U-01]。
// 覆盖 Vue/Quasar SPA 常见可交互角色 [HA-02 A11y SPA POC]。
var interactableRoles = map[string]bool{
	"button":     true,
	"link":       true,
	"textbox":    true,
	"searchbox":  true,
	"combobox":   true,
	"listbox":    true,
	"option":     true,
	"menuitem":   true,
	"checkbox":   true,
	"radio":      true,
	"switch":     true, // Quasar QToggle → role=switch [HA-02 SPA coverage]
	"slider":     true,
	"spinbutton": true,
	"tab":        true,
	"treeitem":   true,
}

// genericRoles 是需要过滤掉的 generic/noise role [TC-09-U-01]。
var genericRoles = map[string]bool{
	"generic":       true,
	"none":          true,
	"presentation":  true,
	"InlineTextBox": true,
}

// extractInteractableRefs 从 A11y Tree 中提取可交互元素，按 DFS 顺序分配 Refs [TC-09-U-26]。
func extractInteractableRefs(nodes []*accessibility.Node) []ElementRef {
	var refs []ElementRef
	counter := 1

	// 建立 nodeID → node 映射，用于 DFS 遍历
	nodeMap := make(map[accessibility.NodeID]*accessibility.Node, len(nodes))
	for _, n := range nodes {
		if n != nil {
			nodeMap[n.NodeID] = n
		}
	}

	// DFS 遍历（找根节点，通常是第一个没有父节点的节点）
	var dfs func(nodeID accessibility.NodeID)
	visited := make(map[accessibility.NodeID]bool)
	dfs = func(nodeID accessibility.NodeID) {
		if visited[nodeID] {
			return
		}
		visited[nodeID] = true

		node, ok := nodeMap[nodeID]
		if !ok {
			return
		}

		// 检查是否为可交互元素
		roleName := getRoleName(node)
		if !genericRoles[roleName] && (interactableRoles[roleName] || isInteractableByProperties(node)) {
			name := getAccessibleName(node)
			placeholder := getPlaceholder(node)
			if name != "" || placeholder != "" || interactableRoles[roleName] {
				ref := fmt.Sprintf("e%d", counter)
				counter++
				nameShort := name
				if len(nameShort) > 50 {
					// Truncate by runes, not bytes
					runes := []rune(nameShort)
					if len(runes) > 50 {
						nameShort = string(runes[:47]) + "..."
					}
				}
				refs = append(refs, ElementRef{
					Ref:           ref,
					BackendNodeID: int64(node.BackendDOMNodeID),
					Role:          roleName,
					Name:          name,
					NameFull:      name,
					NameShort:     nameShort,
					Placeholder:   placeholder,
					Interactable:  true,
				})
			}
		}

		// 递归处理子节点
		for _, childID := range node.ChildIDs {
			dfs(childID)
		}
	}

	// 找根节点并开始 DFS
	for _, node := range nodes {
		if node != nil && len(findParent(node, nodes)) == 0 {
			dfs(node.NodeID)
		}
	}

	// 如果 DFS 遍历没有找到，直接遍历所有节点
	if len(refs) == 0 {
		for _, node := range nodes {
			if node == nil {
				continue
			}
			roleName := getRoleName(node)
			if genericRoles[roleName] {
				continue
			}
			if !interactableRoles[roleName] {
				continue
			}
			name := getAccessibleName(node)
			placeholder := getPlaceholder(node)
			ref := fmt.Sprintf("e%d", counter)
			counter++
			nameShort := name
			if runes := []rune(nameShort); len(runes) > 50 {
				nameShort = string(runes[:47]) + "..."
			}
			refs = append(refs, ElementRef{
				Ref:           ref,
				BackendNodeID: int64(node.BackendDOMNodeID),
				Role:          roleName,
				Name:          name,
				NameFull:      name,
				NameShort:     nameShort,
				Placeholder:   placeholder,
				Interactable:  true,
			})
		}
	}

	return refs
}

// findParent 检查节点是否有父节点（简单判断：是否出现在其他节点的 ChildIDs 中）。
func findParent(target *accessibility.Node, allNodes []*accessibility.Node) []*accessibility.Node {
	var parents []*accessibility.Node
	for _, n := range allNodes {
		if n == nil {
			continue
		}
		for _, childID := range n.ChildIDs {
			if childID == target.NodeID {
				parents = append(parents, n)
				break
			}
		}
	}
	return parents
}

// getRoleName 获取 A11y 节点的 role 名称。
func getRoleName(node *accessibility.Node) string {
	if node.Role == nil {
		return ""
	}
	return unquoteJSONString(node.Role.Value.String())
}

// getAccessibleName 获取 A11y 节点的 accessible name。
func getAccessibleName(node *accessibility.Node) string {
	if node.Name == nil {
		return ""
	}
	return unquoteJSONString(node.Name.Value.String())
}

// getPlaceholder 获取输入框的 placeholder。
func getPlaceholder(node *accessibility.Node) string {
	for _, prop := range node.Properties {
		if prop != nil && prop.Name == "placeholder" && prop.Value != nil {
			return unquoteJSONString(prop.Value.Value.String())
		}
	}
	return ""
}

// isInteractableByProperties 通过 A11y 属性判断是否可交互。
func isInteractableByProperties(node *accessibility.Node) bool {
	for _, prop := range node.Properties {
		if prop == nil {
			continue
		}
		if prop.Name == "focusable" || prop.Name == "clickable" {
			// jsontext.Value 是 []byte，true 为 "true"
			val := prop.Value.Value.String()
			if val == "true" {
				return true
			}
		}
	}
	return false
}

// unquoteJSONString 去除 JSON 字符串的引号并解码 \uXXXX Unicode 转义（"button" → button）。
// A11y tree 节点名称由 cdproto 以 JSON 编码形式返回，包含 \uXXXX 转义序列。
// 使用 json.Unmarshal 正确解码所有 JSON 转义（含中文 Unicode）[BUG-04 fix]。
func unquoteJSONString(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var decoded string
		if err := json.Unmarshal([]byte(s), &decoded); err == nil {
			return decoded
		}
		// fallback: 仅去外引号（兼容非标准格式）
		return s[1 : len(s)-1]
	}
	return s
}

// enrichTestIDsViaJS 用纯 JS 获取 {text→testid} 映射，通过 A11y ref.Name 文本匹配关联。
// TH-0405-p7c 验证: CDP DOM 域(describeNode) 和 DOMSnapshot 域(captureSnapshot) 均破坏状态。
// Runtime.evaluate 是唯一安全的 DOM 属性获取路径。Playwright 同理用 JS 计算 accessible name。
func enrichTestIDsViaJS(ctx context.Context, refs []ElementRef) {
	type tidEntry struct {
		TestID string `json:"testid"`
		Text   string `json:"text"`
	}
	var entries []tidEntry
	js := `(() => {
		const r = [];
		document.querySelectorAll('[data-testid]').forEach(el => {
			const text = (
				(el.textContent||'').trim() ||
				(el.getAttribute('aria-label')||'').trim() ||
				(el.getAttribute('placeholder')||'').trim() ||
				(el.getAttribute('title')||'').trim() ||
				(el.getAttribute('name')||'').trim()
			);
			r.push({testid: el.getAttribute('data-testid'), text: text.substring(0,60)});
		});
		return r;
	})()`
	jsCtx, cancel := context.WithTimeout(ctx, jsEnrichmentTimeout)
	defer cancel()
	if err := chromedp.Run(jsCtx, chromedp.Evaluate(js, &entries)); err != nil || len(entries) == 0 {
		return
	}
	textToTID := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Text != "" && e.TestID != "" {
			if _, exists := textToTID[e.Text]; !exists {
				textToTID[e.Text] = e.TestID
			}
		}
	}
	for i := range refs {
		if refs[i].TestID != "" || refs[i].Name == "" {
			continue
		}
		name := refs[i].Name
		if tid, ok := textToTID[name]; ok {
			refs[i].TestID = tid
			delete(textToTID, name)
		}
	}
}

// discoverClickableDOMViaJS 补充发现有 data-testid 但不在 A11y 树的元素。纯 JS，零 CDP DOM API。
func discoverClickableDOMViaJS(ctx context.Context, existingRefs []ElementRef) []ElementRef {
	counter := len(existingRefs) + 1
	type domItem struct {
		TestID string `json:"testid"`
		Text   string `json:"text"`
	}
	var items []domItem
	js := `(() => {
		const skipNative = new Set(['INPUT','SELECT','TEXTAREA','LABEL']);
		const results = [];
		const isBlocked = (el) => {
			for (let n = el; n && n.nodeType === 1; n = n.parentElement) {
				if (n.hasAttribute('inert')) return true;
				if (n.getAttribute('aria-hidden') === 'true') return true;
			}
			return false;
		};
		const isTopmost = (el) => {
			const r = el.getBoundingClientRect();
			if (r.width <= 0 || r.height <= 0) return false;
			const style = window.getComputedStyle(el);
			if (style.display === 'none' || style.visibility === 'hidden' || style.pointerEvents === 'none') return false;
			const x = Math.min(Math.max(r.left + r.width / 2, 0), window.innerWidth - 1);
			const y = Math.min(Math.max(r.top + r.height / 2, 0), window.innerHeight - 1);
			const top = document.elementFromPoint(x, y);
			return !!top && (el === top || el.contains(top));
		};
		document.querySelectorAll('[data-testid]').forEach(el => {
			if (skipNative.has(el.tagName)) return;
			if (isBlocked(el) || !isTopmost(el)) return;
			const text = (
				(el.textContent||'').trim() ||
				(el.getAttribute('aria-label')||'').trim() ||
				(el.getAttribute('title')||'').trim()
			);
			results.push({testid: el.getAttribute('data-testid'), text: text.substring(0,50)});
		});
		return results;
	})()`
	jsCtx, cancel := context.WithTimeout(ctx, jsEnrichmentTimeout)
	defer cancel()
	if err := chromedp.Run(jsCtx, chromedp.Evaluate(js, &items)); err != nil || len(items) == 0 {
		return nil
	}
	existing := make(map[string]bool, len(existingRefs))
	for _, r := range existingRefs {
		if r.TestID != "" {
			existing[r.TestID] = true
		}
		existing[r.Name] = true
	}
	var supplementary []ElementRef
	for _, item := range items {
		if item.TestID == "" || existing[item.TestID] {
			continue
		}
		existing[item.TestID] = true
		name := item.TestID
		if len(name) > 40 {
			name = name[:37] + "..."
		}
		ref := fmt.Sprintf("e%d", counter)
		counter++
		supplementary = append(supplementary, ElementRef{
			Ref: ref, BackendNodeID: 0, Role: "clickable",
			Name: name, TestID: item.TestID, Interactable: true,
		})
	}
	return supplementary
}

// computeRecommendedLocator 计算元素的推荐定位器。
func computeRecommendedLocator(ref *ElementRef) string {
	// 优先: testid
	if ref.TestID != "" {
		return "#" + ref.TestID
	}
	// 唯一 role+name: 简写
	if ref.MatchCount == 1 && ref.Name != "" {
		return ref.Role + ":'" + ref.Name + "'"
	}
	// 歧义 role+name: canonical + nth
	if ref.MatchCount > 1 && ref.Name != "" {
		// We'll use the ref number to determine nth
		// Extract numeric suffix from ref
		nth := extractRefNth(ref.Ref)
		if nth > 0 {
			return fmt.Sprintf("role=%s[name=\"%s\"][nth=%d]", ref.Role, ref.Name, nth)
		}
		return fmt.Sprintf("role=%s[name*=\"%s\"]", ref.Role, ref.Name)
	}
	// Bare role fallback
	if ref.Role != "" {
		return ref.Role
	}
	return ref.Ref
}

// extractRefNth extracts the numeric index from a ref string like "e3" or "@r3".
func extractRefNth(ref string) int {
	if strings.HasPrefix(ref, "@r") {
		n := 0
		for _, c := range ref[2:] {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
			} else {
				return 0
			}
		}
		return n
	}
	if strings.HasPrefix(ref, "e") {
		n := 0
		for _, c := range ref[1:] {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
			} else {
				return 0
			}
		}
		return n
	}
	return 0
}

// buildCompactText 构建 compact A11y 文本。
// 在 session 模式下，使用 @rN 格式；否则使用序号。
func buildCompactText(refs []ElementRef) string {
	return buildCompactTextWithMode(refs, false)
}

// buildCompactTextWithMode 构建 compact A11y 文本，格式 "[{ref} role 'name' #testid]"。
// sessionMode=true 时用 @rN ref，否则用位置序号。
func buildCompactTextWithMode(refs []ElementRef, sessionMode bool) string {
	var sb strings.Builder
	for i, ref := range refs {
		sb.WriteString("[")
		if sessionMode && strings.HasPrefix(ref.Ref, "@r") {
			// Session mode: use @rN ref
			sb.WriteString(ref.Ref)
		} else {
			// One-shot mode: use position number
			sb.WriteString(fmt.Sprintf("%d.", i+1))
		}
		sb.WriteString(" ")
		sb.WriteString(ref.Role)
		if ref.NameShort != "" {
			sb.WriteString(" '")
			sb.WriteString(ref.NameShort)
			sb.WriteString("'")
		} else if ref.Name != "" {
			sb.WriteString(" '")
			name := ref.Name
			if runes := []rune(name); len(runes) > 50 {
				name = string(runes[:47]) + "..."
			}
			sb.WriteString(name)
			sb.WriteString("'")
		} else if ref.Placeholder != "" {
			sb.WriteString(" placeholder='")
			sb.WriteString(ref.Placeholder)
			sb.WriteString("'")
		}
		// 有 data-testid 时追加 #testid
		if ref.TestID != "" {
			sb.WriteString(" #")
			sb.WriteString(ref.TestID)
		}
		sb.WriteString("]")
		sb.WriteString(" ")
	}
	return strings.TrimSpace(sb.String())
}

// GetSnapshotWithSessionMode 获取快照，session 模式下使用 @rN ref。
func (e *snapshotEngine) GetSnapshotWithSessionMode(ctx context.Context, snapEpoch int) (*Snapshot, error) {
	snap, err := e.GetSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	if len(snap.Refs) == 0 {
		return snap, nil
	}
	// Remap refs to @rN format and rebuild tables
	for i := range snap.Refs {
		oldRef := snap.Refs[i].Ref
		newRef := fmt.Sprintf("@r%d", i+1)
		snap.Refs[i].Ref = newRef
		// Update refTable and refMeta
		if nodeID, ok := e.refTable[oldRef]; ok {
			e.refTable[newRef] = nodeID
			delete(e.refTable, oldRef)
		}
		if meta, ok := e.refMeta[oldRef]; ok {
			meta.Ref = newRef
			e.refMeta[newRef] = meta
			delete(e.refMeta, oldRef)
		}
		// Recompute recommended locator with new ref
		snap.Refs[i].RecommendedLocator = computeRecommendedLocator(&snap.Refs[i])
	}
	// Rebuild compact text in session mode
	snap.Text = buildCompactTextWithMode(snap.Refs, true)
	return snap, nil
}

// estimateTokens 估算文本的 token 数（约 4 字符/token）。
func estimateTokens(text string) int {
	runes := utf8.RuneCountInString(text)
	// 英文约 4 chars/token，中文约 1.5 chars/token（折中用 3）
	return runes/3 + 1
}

func normalizeVisibleText(raw string) string {
	text := strings.Join(strings.Fields(raw), " ")
	if len(text) > 3000 {
		text = text[:3000]
	}
	return text
}

// navigateTo 导航到指定 URL 并等待加载完成。
func navigateTo(url string) chromedp.Action {
	return chromedp.Navigate(url)
}

// waitForLoad 等待页面 load 事件。
func waitForLoad() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.WaitReady("body", chromedp.ByQuery))
	})
}

// getDocumentHTML 获取 HTML 文档 outerHTML。
func getDocumentHTML(result *string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		node, err := dom.GetDocument().Do(ctx)
		if err != nil {
			return err
		}
		html, err := dom.GetOuterHTML().WithNodeID(node.NodeID).Do(ctx)
		if err != nil {
			return err
		}
		*result = html
		return nil
	})
}

// getPageTitle 获取页面 Title。
func getPageTitle(result *string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		return chromedp.Run(ctx, chromedp.Title(result))
	})
}

// ============================================================
// § SnapWithOptions — r2 Delta-REQ (TH-0418-c9x)
// [Ref: CAP-BS09-C5 §3.2b, SC-20, SC-21, TC-C5-13~16]
// ============================================================

// SnapOptions 控制快照过滤和格式化行为。
type SnapOptions struct {
	// Selector: CSS 选择器，仅返回该元素的后代节点 (SC-20)。空字符串=全页面。
	Selector string
	// Compact: 仅保留可交互 role 的元素 (SC-21)。
	Compact bool
	// MaxDepth: A11y 树 DFS 最大深度 (SC-21)，0=不限。
	MaxDepth int
	// SessionMode: true 时使用 @rN ref（session 模式）。
	SessionMode bool
	// SnapEpoch: session 模式快照序号（仅 SessionMode=true 时有效）。
	SnapEpoch int
}

// compactInteractableRoles 是 --compact 模式保留的可交互 role 集合 [SC-21]。
var compactInteractableRoles = map[string]bool{
	"button":    true,
	"input":     true,
	"link":      true,
	"textbox":   true,
	"checkbox":  true,
	"radio":     true,
	"combobox":  true,
	"slider":    true,
	"tab":       true,
	"searchbox": true,
	"menuitem":  true,
	"switch":    true,
}

// SnapWithOptions 获取快照并应用 SnapOptions 过滤/格式化 [SC-20, SC-21]。
// 若 opts.Selector != ""，使用 CDP DOM.querySelector 定位根节点，再用
// accessibility.GetPartialAXTree 获取该子树的 A11y 数据。
func (e *snapshotEngine) SnapWithOptions(ctx context.Context, opts SnapOptions) (*Snapshot, error) {
	// 步骤 1: 获取 URL / Title
	var currentURL, title string
	basicCtx, basicCancel := context.WithTimeout(ctx, snapshotBasicTimeout)
	err := chromedp.Run(basicCtx,
		chromedp.Location(&currentURL),
		chromedp.Title(&title),
	)
	basicCancel()
	if err != nil {
		return e.progressiveFallback(ctx, currentURL, title, "basic_info_unavailable", err), nil
	}

	var refs []ElementRef

	if opts.Selector != "" {
		// SC-20: --selector 范围快照
		// 1. DOM.querySelector → backendNodeId
		var rootNodeID cdp.BackendNodeID
		selectorCtx, selectorCancel := context.WithTimeout(ctx, selectorResolveTimeout)
		err := chromedp.Run(selectorCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			doc, err := dom.GetDocument().WithDepth(-1).Do(ctx)
			if err != nil {
				return err
			}
			nodeID, err := dom.QuerySelector(doc.NodeID, opts.Selector).Do(ctx)
			if err != nil || nodeID == 0 {
				return ErrSelectorNotFound
			}
			// Convert NodeID → BackendNodeID
			nodes, err := dom.PushNodesByBackendIDsToFrontend([]cdp.BackendNodeID{}).Do(ctx)
			_ = nodes
			// Use DescribeNode to get backendNodeId
			described, err := dom.DescribeNode().WithNodeID(nodeID).WithDepth(0).Do(ctx)
			if err != nil {
				return ErrSelectorNotFound
			}
			rootNodeID = described.BackendNodeID
			return nil
		}))
		selectorCancel()
		if err != nil {
			return nil, err
		}
		if rootNodeID == 0 {
			return nil, ErrSelectorNotFound
		}

		// 2. GetPartialAXTree for root subtree
		var axNodes []*accessibility.Node
		partialCtx, partialCancel := context.WithTimeout(ctx, partialAXProbeTimeout)
		err = chromedp.Run(partialCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			var fetchErr error
			axNodes, fetchErr = accessibility.GetPartialAXTree().
				WithBackendNodeID(rootNodeID).
				WithFetchRelatives(false).
				Do(ctx)
			return fetchErr
		}))
		partialCancel()
		if err != nil || len(axNodes) == 0 {
			// Fallback: full tree and filter manually
			a11yCtx, a11yCancel := context.WithTimeout(ctx, a11yProbeTimeout)
			err = chromedp.Run(a11yCtx, chromedp.ActionFunc(func(ctx context.Context) error {
				var fetchErr error
				axNodes, fetchErr = accessibility.GetFullAXTree().Do(ctx)
				return fetchErr
			}))
			a11yCancel()
			if err != nil {
				return e.progressiveFallback(ctx, currentURL, title, "selector_a11y_unavailable", err), nil
			}
		}

		rawRefs := extractInteractableRefs(axNodes)

		// Apply --compact filter if requested
		if opts.Compact {
			rawRefs = filterCompactRefs(rawRefs)
		}

		// Renumber refs in session mode (@rN) or positional (eN)
		refs = renumberRefs(rawRefs, opts.SessionMode)

		// Rebuild refTable/refMeta
		e.refTable = make(map[string]int64, len(refs))
		e.refMeta = make(map[string]*ElementRef, len(refs)*2)
		for i := range refs {
			e.refTable[refs[i].Ref] = refs[i].BackendNodeID
			e.refMeta[refs[i].Ref] = &refs[i]
			if refs[i].TestID != "" {
				e.refMeta["#"+refs[i].TestID] = &refs[i]
			}
			refs[i].RecommendedLocator = computeRecommendedLocator(&refs[i])
		}

		textMode := opts.SessionMode
		text := buildCompactTextWithMode(refs, textMode)
		tokenEst := estimateTokens(text)

		snap := &Snapshot{
			PageTitle:    title,
			URL:          currentURL,
			Text:         text,
			Refs:         refs,
			SnapshotType: snapshotTypeA11y,
			TokenEst:     tokenEst,
			LoadState:    loadStateActionable,
		}
		return snap, nil
	}

	// No selector: full-page snap with optional compact/max-depth
	snap, err := e.GetSnapshot(ctx)
	if err != nil {
		return nil, err
	}

	// Apply compact filter
	if opts.Compact {
		snap.Refs = filterCompactRefs(snap.Refs)
	}

	// Apply max-depth truncation
	if opts.MaxDepth > 0 {
		snap.Text = applyMaxDepthText(snap.Refs, opts.MaxDepth, opts.SessionMode)
	}

	// Session mode remapping
	if opts.SessionMode {
		if len(snap.Refs) > 0 {
			for i := range snap.Refs {
				oldRef := snap.Refs[i].Ref
				newRef := fmt.Sprintf("@r%d", i+1)
				snap.Refs[i].Ref = newRef
				if nodeID, ok := e.refTable[oldRef]; ok {
					e.refTable[newRef] = nodeID
					delete(e.refTable, oldRef)
				}
				if meta, ok := e.refMeta[oldRef]; ok {
					meta.Ref = newRef
					e.refMeta[newRef] = meta
					delete(e.refMeta, oldRef)
				}
				snap.Refs[i].RecommendedLocator = computeRecommendedLocator(&snap.Refs[i])
			}
			if opts.MaxDepth > 0 {
				snap.Text = applyMaxDepthText(snap.Refs, opts.MaxDepth, true)
			} else {
				snap.Text = buildCompactTextWithMode(snap.Refs, true)
			}
		}
	} else if opts.Compact && len(snap.Refs) > 0 {
		snap.Text = buildCompactTextWithMode(snap.Refs, false)
	}

	snap.TokenEst = estimateTokens(snap.Text)
	return snap, nil
}

// filterCompactRefs 过滤只保留可交互 role 的 refs [SC-21, TC-C5-15]。
func filterCompactRefs(refs []ElementRef) []ElementRef {
	filtered := refs[:0:0]
	for i := range refs {
		if compactInteractableRoles[refs[i].Role] {
			filtered = append(filtered, refs[i])
		}
	}
	return filtered
}

// renumberRefs 重新编号 refs（session 模式 @rN，one-shot eN）。
func renumberRefs(refs []ElementRef, sessionMode bool) []ElementRef {
	result := make([]ElementRef, len(refs))
	for i := range refs {
		result[i] = refs[i]
		if sessionMode {
			result[i].Ref = fmt.Sprintf("@r%d", i+1)
		} else {
			result[i].Ref = fmt.Sprintf("e%d", i+1)
		}
	}
	return result
}

// applyMaxDepthText 对 refs 进行最大深度折叠，生成带折叠标记的文本 [SC-21, TC-C5-16]。
// 由于 refs 是扁平列表（DFS 顺序），此实现通过估算元素间的深度层级进行折叠。
// 超过 maxDepth 的连续元素折叠为 "[... N children]"。
// 注：A11y 树已经被展平为 refs，真实深度信息不在 ElementRef 中，
// 此处用启发式方式：将 refs 分段，每段超过阈值时折叠尾部元素。
func applyMaxDepthText(refs []ElementRef, maxDepth int, sessionMode bool) string {
	// 启发式实现: maxDepth 控制每组 top-level 元素下允许展开的子元素数。
	// 对于扁平化后的 refs 列表，超过 maxDepth 个连续 non-button/link 元素视为深层子树。
	// 实际效果: 当 maxDepth=1 时仅输出顶层可交互元素；maxDepth=3 输出前3层。
	if maxDepth <= 0 || len(refs) == 0 {
		return buildCompactTextWithMode(refs, sessionMode)
	}

	// 按 maxDepth 每组最多 maxDepth 个元素，超出折叠
	// 由于我们没有树深度信息，退化为：取前 maxDepth * 5 个元素，剩余折叠
	threshold := maxDepth * 5
	if threshold >= len(refs) {
		return buildCompactTextWithMode(refs, sessionMode)
	}

	visible := refs[:threshold]
	hidden := len(refs) - threshold
	text := buildCompactTextWithMode(visible, sessionMode)
	return text + fmt.Sprintf(" [... %d children]", hidden)
}
