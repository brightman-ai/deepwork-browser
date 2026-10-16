package testing

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
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
//   - captureErrors: optional screenshot capture failure; takes precedence over decoding bytes.
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
	captureErrors ...error,
) *Observation {
	obs := &Observation{
		Schema:    "dw.observe.v2",
		State:     "ready",
		ID:        NewObservationID(),
		Channels:  map[string]ChannelState{"structural": {Status: "not-requested"}, "visual": {Status: "not-requested"}, "behavior": {Status: "not-requested"}, "console": {Status: "not-requested"}, "network": {Status: "not-requested"}},
		SessionID: sessionID,
		Timestamp: time.Now(),
	}

	if snap != nil {
		if snap.ObservationID != "" {
			obs.ID = snap.ObservationID
			obs.runtimeCapture = true
		}
		obs.RuntimeRunID = snap.RuntimeRunID
		obs.TargetID = snap.TargetID
		obs.DocumentID = snap.DocumentID
	}
	obs.OperationID = obs.ID

	// — structural 层 —
	if snap != nil {
		obs.Structural = structuralFromSnap(snap)
		obs.Channels["structural"] = ChannelState{Status: "captured"}
		if snap.Progressive {
			obs.State = "pending"
			obs.Channels["structural"] = ChannelState{Status: "unavailable", Reason: snap.ProgressReason}
		}
		obs.Page = PageState{
			URL:       snap.URL,
			Title:     snap.PageTitle,
			ViewportW: int(snap.Viewport.Width),
			ViewportH: int(snap.Viewport.Height),
			Geometry:  snap.Viewport,
		}
	}

	// — visual 层 —
	if screenshotPath != "" {
		obs.Channels["visual"] = ChannelState{Status: "unavailable", Reason: "screenshot missing or write failed"}
		obs.State = "unavailable"
		if len(captureErrors) > 0 && captureErrors[0] != nil {
			obs.Channels["visual"] = ChannelState{Status: "unavailable", Reason: captureErrors[0].Error()}
		} else if decoded, _, err := image.Decode(bytes.NewReader(screenshotData)); err == nil {
			var encoded bytes.Buffer
			if err := png.Encode(&encoded, decoded); err != nil {
				obs.Channels["visual"] = ChannelState{Status: "unavailable", Reason: err.Error()}
				return obs
			}
			if err := writeScreenshot(screenshotPath, encoded.Bytes()); err == nil {
				obs.Visual = &VisualState{
					ScreenshotPath: screenshotPath,
				}
				obs.Channels["visual"] = ChannelState{Status: "captured"}
				if snap == nil || !snap.Progressive {
					obs.State = "ready"
				} else {
					obs.State = "pending"
				}
			} else {
				obs.Channels["visual"] = ChannelState{Status: "unavailable", Reason: err.Error()}
			}
		} else {
			obs.Channels["visual"] = ChannelState{Status: "unavailable", Reason: err.Error()}
		}
	}

	// — behavior 层 —
	if behavior != nil {
		obs.Behavior = behavior
		if snap != nil {
			obs.Behavior.URL = snap.URL
			obs.Behavior.Title = snap.PageTitle
			obs.Behavior.LoadState = snap.LoadState
		}
		obs.Channels["behavior"] = ChannelState{Status: "captured"}
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
		if telemetry.ConsoleErrors != nil {
			obs.Channels["console"] = ChannelState{Status: "captured"}
		} else {
			obs.Channels["console"] = ChannelState{Status: "unavailable", Reason: "collector did not supply console evidence"}
		}
		if telemetry.NetworkFailures != nil {
			obs.Channels["network"] = ChannelState{Status: "captured"}
		} else {
			obs.Channels["network"] = ChannelState{Status: "unavailable", Reason: "collector did not supply network evidence"}
		}
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
			Ref:            r.Ref,
			AllowedActions: browser.AllowedActionsForElement(r, browser.Scenario("")),
			Role:           r.Role,
			Name:           r.NameShort,
			Value:          r.Value,
			TestID:         r.TestID,
			NameFull:       r.NameFull, Locator: r.RecommendedLocator, BBox: r.BBox, VisibilityKnown: r.VisibilityKnown, Visible: r.VisibleInViewport, Observed: r.Observed, ModalRank: r.ModalRank, Blocked: r.BlockedByModal,
		})
	}
	return &StructuralState{
		SnapshotType:         snap.SnapshotType,
		RefsCount:            len(snap.Refs),
		TextHash:             textHash(snap.Text),
		LoadState:            snap.LoadState,
		ReadyState:           snap.ReadyState,
		Text:                 snap.Text,
		Refs:                 refs,
		DocumentTestIDs:      snap.DocumentTestIDs,
		DocumentText:         snap.DocumentText,
		DocumentTextComplete: snap.DocumentTextComplete,
	}
}

// BindActionHints projects core-captured target facts under the owning scenario.
func (obs *Observation) BindActionHints(snap *browser.Snapshot, scenario browser.Scenario) {
	if obs == nil || obs.Structural == nil || snap == nil || len(obs.Structural.Refs) != len(snap.Refs) {
		return
	}
	for index, ref := range snap.Refs {
		obs.Structural.Refs[index].AllowedActions = browser.AllowedActionsForElement(ref, scenario)
		obs.Structural.Refs[index].Select = browser.SelectCapabilityForElement(ref, scenario)
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

func NewObservationID() string { return browser.NewObservationID() }
