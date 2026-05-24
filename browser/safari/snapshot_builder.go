package safari

import (
	"fmt"
	"strings"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// SafariSnapshotBuilder 将 AXNode 树转换为 browser.Snapshot。
// 核心职责：WebContent 过滤 + AX→ARIA 映射 + Chrome 格式对齐的 compact text。
type SafariSnapshotBuilder struct{}

// NewSnapshotBuilder 创建 SafariSnapshotBuilder。
func NewSnapshotBuilder() *SafariSnapshotBuilder {
	return &SafariSnapshotBuilder{}
}

// Build 将 AXNode 树构建为 browser.Snapshot。
// sessionMode=true 时使用 @rN ref 格式，否则使用位置序号。
func (b *SafariSnapshotBuilder) Build(tree *AXNode, sessionMode bool) *browser.Snapshot {
	if tree == nil {
		return &browser.Snapshot{
			SnapshotType: "ax_empty",
			LoadState:    "unavailable",
		}
	}

	// 1. 过滤到 WebContent 子树（排除 Safari 地址栏/工具栏/状态栏）
	webArea, pageURL, pageTitle := findWebArea(tree)
	walkRoot := webArea
	if walkRoot == nil {
		walkRoot = tree // fallback: 用整棵树
	}

	// 2. DFS 遍历，提取 ElementRef
	var refs []browser.ElementRef
	counter := 0

	var walk func(n *AXNode)
	walk = func(n *AXNode) {
		if n == nil {
			return
		}

		ariaRole := mapAXRoleToARIA(n.Role)

		// 跳过 generic 节点但继续遍历子节点
		if genericARIARoles[ariaRole] {
			for i := range n.Children {
				walk(&n.Children[i])
			}
			return
		}

		name := accessibleNameFromAX(n)
		placeholder := placeholderFromAX(n)
		testID := ""
		if n.Identifier != nil {
			testID = *n.Identifier
		}

		isInteractable := interactableARIARoles[ariaRole] && n.Enabled && n.Visible

		// 有名字或可交互的元素才纳入 ref 表
		if isInteractable || (name != "" && ariaRole != "") {
			counter++
			refStr := fmt.Sprintf("e%d", counter)
			if sessionMode {
				refStr = fmt.Sprintf("@r%d", counter)
			}

			stableKey := buildStableKey(ariaRole, name, testID, placeholder, n.Frame)
			nameShort := truncateNameStr(name, 50)

			refs = append(refs, browser.ElementRef{
				Ref: refStr,
				Locator: browser.NodeLocator{
					Engine:    browser.EngineSafari,
					AXPath:    n.Path,
					StableKey: stableKey,
					Frame: browser.Rect{
						X: n.Frame.X, Y: n.Frame.Y,
						Width: n.Frame.Width, Height: n.Frame.Height,
					},
				},
				AXPath:       n.Path,
				Role:         ariaRole,
				Name:         name,
				NameFull:     name,
				NameShort:    nameShort,
				Placeholder:  placeholder,
				TestID:       testID,
				Interactable: isInteractable,
			})
		}

		for i := range n.Children {
			walk(&n.Children[i])
		}
	}
	walk(walkRoot)

	// 3. 构建 compact text（与 Chrome buildCompactTextWithMode 格式完全对齐）
	text := buildCompactText(refs, sessionMode)

	snapshotType := "ax"
	loadState := "actionable"
	if len(refs) == 0 {
		loadState = "loading"
		snapshotType = "ax_empty"
	}

	return &browser.Snapshot{
		PageTitle:    pageTitle,
		URL:          pageURL,
		Text:         text,
		Refs:         refs,
		SnapshotType: snapshotType,
		TokenEst:     len(text) / 4,
		LoadState:    loadState,
		ReadyState:   "", // Safari 无 JS eval，不可获取 document.readyState
	}
}

// WebAreaBounds 返回 WebContent 区域的 bounds（用于 scroll 计算）。
func (b *SafariSnapshotBuilder) WebAreaBounds(tree *AXNode) browser.Rect {
	wa, _, _ := findWebArea(tree)
	if wa == nil {
		// 默认 iPhone 尺寸
		return browser.Rect{X: 0, Y: 100, Width: 390, Height: 744}
	}
	return browser.Rect{X: wa.Frame.X, Y: wa.Frame.Y, Width: wa.Frame.Width, Height: wa.Frame.Height}
}

// findWebArea 在 AX 树中找到 Safari 的 WebContent 根节点。
// 返回 WebArea 节点指针、页面 URL、页面标题。
func findWebArea(root *AXNode) (webArea *AXNode, url, title string) {
	if root == nil {
		return nil, "", ""
	}
	var dfs func(n *AXNode)
	dfs = func(n *AXNode) {
		if webArea != nil {
			return
		}
		if n.Role == "AXWebArea" {
			webArea = n
			if n.Value != nil {
				url = *n.Value
			}
			if n.Label != nil {
				title = *n.Label
			}
			return
		}
		for i := range n.Children {
			dfs(&n.Children[i])
		}
	}
	dfs(root)

	// 从工具栏 TextField 提取 URL（WebArea.Value 可能为空）
	if url == "" {
		url = extractURLFromToolbar(root)
	}
	return
}

// extractURLFromToolbar 从 Safari 地址栏提取 URL。
func extractURLFromToolbar(root *AXNode) string {
	var found string
	var dfs func(n *AXNode)
	dfs = func(n *AXNode) {
		if found != "" {
			return
		}
		if n.Role == "AXTextField" && n.Value != nil {
			v := *n.Value
			if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") || strings.Contains(v, ".") {
				found = v
				return
			}
		}
		for i := range n.Children {
			dfs(&n.Children[i])
		}
	}
	dfs(root)
	return found
}

// buildCompactText 构建与 Chrome buildCompactTextWithMode 完全对齐的紧凑文本。
// 格式: `[@r1 button 'name' #testid] [@r2 link 'href'] ...`
func buildCompactText(refs []browser.ElementRef, sessionMode bool) string {
	if len(refs) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, ref := range refs {
		sb.WriteByte('[')
		if sessionMode && strings.HasPrefix(ref.Ref, "@r") {
			sb.WriteString(ref.Ref)
		} else {
			sb.WriteString(fmt.Sprintf("%d.", i+1))
		}
		sb.WriteByte(' ')
		sb.WriteString(ref.Role)
		if ref.NameShort != "" {
			sb.WriteString(" '")
			sb.WriteString(ref.NameShort)
			sb.WriteByte('\'')
		} else if ref.Name != "" {
			name := ref.Name
			if runes := []rune(name); len(runes) > 50 {
				name = string(runes[:47]) + "..."
			}
			sb.WriteString(" '")
			sb.WriteString(name)
			sb.WriteByte('\'')
		} else if ref.Placeholder != "" {
			sb.WriteString(" placeholder='")
			sb.WriteString(ref.Placeholder)
			sb.WriteByte('\'')
		}
		if ref.TestID != "" {
			sb.WriteString(" #")
			sb.WriteString(ref.TestID)
		}
		sb.WriteByte(']')
		sb.WriteByte(' ')
	}
	return strings.TrimSpace(sb.String())
}

// SnapshotSignature 生成快照内容签名，用于页面加载稳定性检测。
// 两次 sig 相同 → AX 树未变化 → 页面加载完成。
func SnapshotSignature(snap *browser.Snapshot) string {
	if snap == nil || len(snap.Refs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("n=%d|", len(snap.Refs)))
	for _, ref := range snap.Refs {
		sb.WriteString(ref.Role)
		sb.WriteByte('|')
		sb.WriteString(ref.Locator.StableKey)
		sb.WriteByte(';')
	}
	return sb.String()
}

// accessibleNameFromAX 从 AXNode 提取 accessible name。
func accessibleNameFromAX(n *AXNode) string {
	if n.Label != nil && *n.Label != "" {
		return *n.Label
	}
	if n.Value != nil && *n.Value != "" {
		return *n.Value
	}
	return ""
}

// placeholderFromAX 从 AXNode 提取 placeholder。
// AXTextField/AXTextArea 且没有实际值时，label 可能是 placeholder。
func placeholderFromAX(n *AXNode) string {
	if (n.Role == "AXTextField" || n.Role == "AXSearchField" || n.Role == "AXTextArea") &&
		(n.Value == nil || *n.Value == "") &&
		n.Label != nil {
		return *n.Label
	}
	return ""
}

// buildStableKey 构建用于漂移重定位的 stable key。
func buildStableKey(role, name, testID, placeholder string, frame AXFrame) string {
	// testID 最稳定
	if testID != "" {
		return fmt.Sprintf("id:%s", testID)
	}
	return fmt.Sprintf("%s|%s|%s|%.0fx%.0f",
		role, name, placeholder, frame.Width, frame.Height)
}

// truncateNameStr 截断名称到指定 rune 数。
func truncateNameStr(name string, maxRunes int) string {
	runes := []rune(name)
	if len(runes) <= maxRunes {
		return name
	}
	return string(runes[:maxRunes-3]) + "..."
}
