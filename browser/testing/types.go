package testing

import (
	"encoding/json"
	"github.com/brightman-ai/deepwork-browser/browser"
	"time"
)

// Observation 是一次多通道终态快照，由 observe 采集。
type Observation struct {
	runtimeCapture bool                    // Only a fresh core snapshot can bind coordinate dispatch.
	RuntimeRunID   string                  `json:"runtime_run_id,omitempty"`
	DocumentID     int64                   `json:"document_id,omitempty"`
	TargetID       string                  `json:"target_id,omitempty"`
	OperationID    string                  `json:"operation_id,omitempty"`
	Scenario       string                  `json:"scenario,omitempty"`
	Deterministic  bool                    `json:"deterministic"`
	State          string                  `json:"observation_state,omitempty"`
	ID             string                  `json:"observation_id,omitempty"`
	Transcript     *TranscriptEvidence     `json:"transcript,omitempty"`
	Files          map[string]FileEvidence `json:"files,omitempty"`
	WorkspaceDir   string                  `json:"workspace_dir,omitempty"`
	Content        *ContentState           `json:"content,omitempty"`
	Probes         map[string]ProbeFact    `json:"probes,omitempty"`
	Channels       map[string]ChannelState `json:"channels,omitempty"`
	Schema         string                  `json:"schema"` // "dw.observe.v2"
	SessionID      string                  `json:"session_id"`
	Timestamp      time.Time               `json:"timestamp"`
	Page           PageState               `json:"page"`
	Structural     *StructuralState        `json:"structural,omitempty"`
	Visual         *VisualState            `json:"visual,omitempty"`
	Behavior       *BehaviorState          `json:"behavior,omitempty"`
	Telemetry      *TelemetryState         `json:"telemetry,omitempty"`
}

// PageState — 页面基本信息
type PageState struct {
	URL       string                `json:"url"`
	Title     string                `json:"title"`
	ViewportW int                   `json:"viewport_w"`
	ViewportH int                   `json:"viewport_h"`
	Geometry  browser.ViewportFacts `json:"geometry"`
	Engine    string                `json:"engine"` // "chrome" | "safari"
}

// StructuralState — A11y 树 + refs
type StructuralState struct {
	SnapshotType string       `json:"snapshot_type"` // "a11y" | "dom_fallback"
	RefsCount    int          `json:"refs_count"`
	TextHash     string       `json:"text_hash"` // sha256 of compact text
	LoadState    string       `json:"load_state"`
	ReadyState   string       `json:"ready_state"`
	Text         string       `json:"text"` // compact A11y text
	Refs         []RefSummary `json:"refs,omitempty"`
	// DocumentTestIDs: document-wide testid presence (assertion evidence; not
	// an action capability). Nil = unproven, never "proven absent".
	DocumentTestIDs []string `json:"document_testids,omitempty"`
	// DocumentText: rendered visible document text (innerText + visible form
	// values). text_contains falls back to it for static content (headings,
	// paragraphs). Absence is proven only when DocumentTextComplete is true.
	DocumentText string `json:"document_text,omitempty"`
	// False includes redaction, truncation and unavailable completeness facts.
	DocumentTextComplete bool `json:"document_text_complete"`
}

// RefSummary — 精简的元素引用
type RefSummary struct {
	Select          *browser.SelectCapability `json:"select,omitempty"`
	AllowedActions  []string                  `json:"allowed_actions"`
	Ref             string                    `json:"ref"`
	Role            string                    `json:"role"`
	Name            string                    `json:"name"`
	Value           string                    `json:"value,omitempty"`
	NameFull        string                    `json:"name_full,omitempty"`
	TestID          string                    `json:"testid,omitempty"`
	Locator         string                    `json:"locator,omitempty"`
	BBox            browser.Rect              `json:"bbox"`
	VisibilityKnown bool                      `json:"visibility_known"`
	Visible         bool                      `json:"visible"`
	Observed        bool                      `json:"observed"`
	ModalRank       int                       `json:"modal_rank,omitempty"`
	Blocked         bool                      `json:"blocked,omitempty"`
}

// BehaviorState — 用户可感知的行为状态
type BehaviorState struct {
	URL          string     `json:"url"`
	Title        string     `json:"title"`
	Tabs         []TabState `json:"tabs"`
	ActiveTabID  string     `json:"active_tab_id"`
	TabCount     int        `json:"tab_count"`
	HistoryIndex int        `json:"history_index"`
	LoadState    string     `json:"load_state"`
	LatencyMs    int64      `json:"latency_ms,omitempty"`
}

// TabState — 单个 tab 状态
type TabState struct {
	ID     string `json:"id"`
	Index  int    `json:"index"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Active bool   `json:"active"`
}

// VisualState — 截图和区域
type VisualState struct {
	ScreenshotPath string       `json:"screenshot_path"`
	Regions        []RegionSnap `json:"regions,omitempty"`
}

// RegionSnap — 页面区域截图
type RegionSnap struct {
	ID        string `json:"id"`
	Rect      Rect   `json:"rect"`
	ImagePath string `json:"image_path"`
}

// Rect — 矩形区域（像素坐标，整数）
type Rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

// TelemetryState — 遥测数据
type TelemetryState struct {
	ConsoleErrors   []ConsoleEntry `json:"console_errors"`
	NetworkFailures []NetworkEntry `json:"network_failures"`
	// VisibleErrors — on-screen error UI a human sees instantly but console/network checks
	// MISS (CHG-016 R3, closes the "console_errors==0 yet a red error banner is on screen"
	// silent-failure gap). Sourced by DOM scan: W3C role=alert / aria-invalid, error-styled
	// classes, red-colored text, and multilingual error keywords. Non-empty ⇒ the agent must
	// NOT report "healthy" — it has to inspect the screenshot and explain each signal.
	VisibleErrors []VisibleErrorEntry `json:"visible_errors,omitempty"`
}

// VisibleErrorEntry — one on-screen error signal.
type VisibleErrorEntry struct {
	Kind     string `json:"kind"`               // "aria" | "styled" | "color" | "keyword"
	Text     string `json:"text"`               // the visible message (trimmed)
	Selector string `json:"selector,omitempty"` // role / class / tag hint for locating it
}

// ConsoleEntry — 单条控制台日志
type ConsoleEntry struct {
	Level  string `json:"level"` // "error" | "warning"
	Text   string `json:"text"`
	Source string `json:"source,omitempty"`
	URL    string `json:"url,omitempty"`
	Line   int    `json:"line,omitempty"`
}

// NetworkEntry — 单条网络请求记录
type NetworkEntry struct {
	URL        string `json:"url"`
	Method     string `json:"method"`
	StatusCode int    `json:"status_code"`
	Error      string `json:"error,omitempty"`
}

// ChannelState distinguishes absent evidence from a measured empty result.
type ChannelState struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// ProbeFact is immutable captured SUT evidence, never an action capability.
type ProbeFact struct {
	ChannelState
	Count        *int            `json:"count,omitempty"`
	Text         *string         `json:"text,omitempty"`
	Value        *string         `json:"value,omitempty"`
	JSON         json.RawMessage `json:"json,omitempty"`
	HTTPStatus   int             `json:"http_status,omitempty"`
	Intervention bool            `json:"intervention,omitempty"`
}

// ObservationContext is provenance captured by a runtime adapter.
type ObservationContext struct {
	SessionID, RuntimeRunID, TargetID, Scenario string
	Deterministic                               bool
}
