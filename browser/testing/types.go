package testing

import "time"

// Observation 是一次多通道终态快照，由 observe 采集。
type Observation struct {
	Schema     string           `json:"schema"`     // "dw.observe.v1"
	SessionID  string           `json:"session_id"`
	Timestamp  time.Time        `json:"timestamp"`
	Page       PageState        `json:"page"`
	Structural *StructuralState `json:"structural,omitempty"`
	Visual     *VisualState     `json:"visual,omitempty"`
	Behavior   *BehaviorState   `json:"behavior,omitempty"`
	Telemetry  *TelemetryState  `json:"telemetry,omitempty"`
}

// PageState — 页面基本信息
type PageState struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	ViewportW int    `json:"viewport_w"`
	ViewportH int    `json:"viewport_h"`
	Engine    string `json:"engine"` // "chrome" | "safari"
}

// StructuralState — A11y 树 + refs
type StructuralState struct {
	SnapshotType string       `json:"snapshot_type"` // "a11y" | "dom_fallback"
	RefsCount    int          `json:"refs_count"`
	TextHash     string       `json:"text_hash"`  // sha256 of compact text
	LoadState    string       `json:"load_state"`
	ReadyState   string       `json:"ready_state"`
	Text         string       `json:"text"`           // compact A11y text
	Refs         []RefSummary `json:"refs,omitempty"`
}

// RefSummary — 精简的元素引用
type RefSummary struct {
	Ref    string `json:"ref"`
	Role   string `json:"role"`
	Name   string `json:"name"`
	TestID string `json:"testid,omitempty"`
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
	ScreenshotPath string      `json:"screenshot_path"`
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
}

// ConsoleEntry — 单条控制台日志
type ConsoleEntry struct {
	Level  string `json:"level"`          // "error" | "warning"
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
