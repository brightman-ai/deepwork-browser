package testing

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// BuildObservation 从已采集的原始数据组装一次 Observation。
//
// 参数:
//   - sessionID:      会话 ID
//   - snap:           来自 browser.BrowserCore.Snap() 的页面快照（nil 时跳过 structural）
//   - screenshotData: PNG/JPEG 字节（nil 时跳过 visual）
//   - screenshotPath: 截图保存路径（screenshotData 非 nil 时必填）
//   - behavior:       行为状态（nil 时跳过 behavior）
//   - telemetry:      遥测状态（nil 时跳过 telemetry）
//
// 调用方负责在调用前完成截图写盘（或传入 screenshotPath 后由本函数写盘）。
// 为保持简洁，本函数接管写盘职责：若 screenshotData 非空且 screenshotPath 非空，
// 则写入文件，写失败时 visual 层 ScreenshotPath 留空但不 panic。
func BuildObservation(
	sessionID string,
	snap *browser.Snapshot,
	screenshotData []byte,
	screenshotPath string,
	behavior *BehaviorState,
	telemetry *TelemetryState,
) *Observation {
	obs := &Observation{
		Schema:    "dw.observe.v1",
		SessionID: sessionID,
		Timestamp: time.Now(),
	}

	// — structural 层 —
	if snap != nil {
		obs.Structural = structuralFromSnap(snap)
		obs.Page = PageState{
			URL:   snap.URL,
			Title: snap.PageTitle,
			// ViewportW/ViewportH: browser.Snapshot does not carry viewport dimensions.
			// Callers that need viewport info should obtain it from browser.SessionInfo
			// (ViewportW/ViewportH fields) and set obs.Page.ViewportW/H directly after
			// calling BuildObservation.
		}
	}

	// — visual 层 —
	if len(screenshotData) > 0 && screenshotPath != "" {
		if err := writeScreenshot(screenshotPath, screenshotData); err == nil {
			obs.Visual = &VisualState{
				ScreenshotPath: screenshotPath,
			}
		}
	}

	// — behavior 层 —
	if behavior != nil {
		obs.Behavior = behavior
		// behavior 提供的 URL/Title 比 snap 更实时（来自 SessionAuthority）
		if obs.Page.URL == "" {
			obs.Page.URL = behavior.URL
		}
		if obs.Page.Title == "" {
			obs.Page.Title = behavior.Title
		}
	}

	// — telemetry 层 —
	if telemetry != nil {
		obs.Telemetry = telemetry
	}

	return obs
}

// BehaviorFromSessionState 将 browser.SessionState 转换为 BehaviorState。
//
// 用法：
//
//	state := session.Authority.GetState()
//	behavior := testing.BehaviorFromSessionState(state)
func BehaviorFromSessionState(state browser.SessionState) *BehaviorState {
	tabs := make([]TabState, 0, len(state.Tabs))
	activeID := ""
	for i, t := range state.Tabs {
		tabs = append(tabs, TabState{
			ID:     t.ID,
			Index:  i,
			URL:    t.URL,
			Title:  t.Title,
			Active: t.Active,
		})
		if t.Active {
			activeID = t.ID
		}
	}
	return &BehaviorState{
		URL:         state.URL,
		Title:       state.Title,
		Tabs:        tabs,
		ActiveTabID: activeID,
		TabCount:    len(tabs),
		LoadState:   state.Mode,
	}
}

// ScreenshotPath 生成截图文件路径：{dir}/{stepID}.png。
// stepID 为空时使用时间戳命名。
func ScreenshotPath(dir, stepID string) string {
	name := stepID
	if name == "" {
		name = fmt.Sprintf("obs-%d", time.Now().UnixMilli())
	}
	return filepath.Join(dir, name+".png")
}

// — 内部 helpers —

// structuralFromSnap 将 browser.Snapshot 转换为 StructuralState。
func structuralFromSnap(snap *browser.Snapshot) *StructuralState {
	refs := make([]RefSummary, 0, len(snap.Refs))
	for _, r := range snap.Refs {
		refs = append(refs, RefSummary{
			Ref:    r.Ref,
			Role:   r.Role,
			Name:   r.NameShort,
			TestID: r.TestID,
		})
	}
	return &StructuralState{
		SnapshotType:    snap.SnapshotType,
		RefsCount:       len(snap.Refs),
		TextHash:        textHash(snap.Text),
		LoadState:       snap.LoadState,
		ReadyState:      snap.ReadyState,
		Text:            snap.Text,
		Refs:            refs,
		DocumentTestIDs: snap.DocumentTestIDs,
		DocumentText:    snap.DocumentText,
	}
}

// textHash 计算字符串的 sha256 十六进制摘要（前 16 字节，32 hex chars）。
func textHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", sum[:16])
}

// writeScreenshot 将字节写入路径，自动创建父目录。
func writeScreenshot(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
