package safari

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// WebDriverSnapshotBuilder 通过 WebDriver 构建 Snapshot。
type WebDriverSnapshotBuilder struct{}

// Build 通过 WebDriver 获取页面元素并构建 Snapshot。
// sessionMode=true 时使用 @rN ref 格式，否则使用 eN 格式。
func (b *WebDriverSnapshotBuilder) Build(ctx context.Context, wdc *WebDriverClient, sessionMode bool) (*browser.Snapshot, error) {
	// 1. 查找交互元素
	var elements Element
	var infos ElementInfo
	var err error

	for attempt := 0; attempt < 3; attempt++ {
		elements, err = wdc.FindElements(ctx, "css selector", "button,a,input,textarea,select,[role],[tabindex]")
		if err != nil {
			return nil, fmt.Errorf("snapshot_webdriver: find elements: %w", err)
		}

		// 2. 批量获取元素信息（单次 JS eval，避免 N+1）
		infos, err = wdc.BatchGetElementInfo(ctx, elements)
		if err == nil {
			break
		}
		if !strings.Contains(strings.ToLower(err.Error), "stale element reference") {
			return nil, fmt.Errorf("snapshot_webdriver: batch get element info: %w", err)
		}
		time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("snapshot_webdriver: batch get element info: %w", err)
	}

	// 3. 过滤 + 构建 ElementRef 列表
	refs := make(browser.ElementRef, 0, len(infos))
	counter := 0
	for _, info := range infos {
		if !info.Visible || !info.Enabled {
			continue
		}

		counter++
		refStr := fmt.Sprintf("e%d", counter)
		if sessionMode {
			refStr = fmt.Sprintf("@r%d", counter)
		}

		role := wdRole(info)
		name := wdName(info)
		nameShort := truncateNameStr(name, 50)

		stableKey := wdStableKey(role, name, info.TestID, info.Placeholder)

		refs = append(refs, browser.ElementRef{
			Ref: refStr
			Locator: browser.NodeLocator{
				Engine: browser.EngineSafari
				AXPath: "", // WebDriver 无 AX path
				StableKey: stableKey
				// 使用 Ordinal 存储序号以便定位回溯
				Ordinal: counter
			}
			Role: role
			Name: name
			NameFull: name
			NameShort: nameShort
			Placeholder: info.Placeholder
			TestID: info.TestID
			Interactable: true
		})
	}

	// 4. 获取页面 URL 和标题
	pageURL, err := wdc.CurrentURL(ctx)
	if err != nil {
		pageURL = ""
	}
	pageTitle, err := wdc.Title(ctx)
	if err != nil {
		pageTitle = ""
	}

	// 5. 构建 compact text（与 snapshot_builder.go buildCompactText 格式完全对齐）
	text := buildCompactText(refs, sessionMode)

	snapshotType := "webdriver"
	loadState := "actionable"
	if len(refs) == 0 {
		loadState = "loading"
		snapshotType = "webdriver_empty"
	}

	return &browser.Snapshot{
		PageTitle: pageTitle
		URL: pageURL
		Text: text
		Refs: refs
		SnapshotType: snapshotType
		TokenEst: len(text) / 4
		LoadState: loadState
	}, nil
}

// wdRole 从 ElementInfo 推导 ARIA role。
// 优先使用显式 role 属性；fallback 到 tag 名称映射。
func wdRole(info ElementInfo) string {
	explicit := strings.ToLower(info.Role)
	// 若 role 字段已经是 ARIA role（来自 getAttribute('role')），直接使用
	if explicit != "" && explicit != strings.ToLower(info.TagName) {
		return explicit
	}
	// 按 tag 名称映射到 ARIA role
	switch strings.ToLower(info.TagName) {
	case "button":
		return "button"
	case "a":
		return "link"
	case "input":
		return "textbox"
	case "textarea":
		return "textbox"
	case "select":
		return "combobox"
	case "img":
		return "img"
	case "h1", "h2", "h3", "h4", "h5", "h6":
		return "heading"
	default:
		if explicit != "" {
			return explicit
		}
		return strings.ToLower(info.TagName)
	}
}

// wdName 从 ElementInfo 提取 accessible name。
// 优先级: aria-label > text > href（link）。
func wdName(info ElementInfo) string {
	if info.AriaLabel != "" {
		return info.AriaLabel
	}
	if info.Text != "" {
		return info.Text
	}
	if info.Href != "" {
		return info.Href
	}
	return ""
}

// wdStableKey 构建 WebDriver 元素的 stable key（用于漂移重定位）。
func wdStableKey(role, name, testID, placeholder string) string {
	if testID != "" {
		return fmt.Sprintf("id:%s", testID)
	}
	return fmt.Sprintf("%s|%s|%s", role, name, placeholder)
}
