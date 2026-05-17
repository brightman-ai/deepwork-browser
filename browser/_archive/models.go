package browser

import "time"

// RuntimeState — BrowserRuntime 状态机状态.
type RuntimeState string

const (
	StateUninitialized RuntimeState = "uninitialized"
	StateInitializing RuntimeState = "initializing"
	StateRunning RuntimeState = "running"
	StateRecovering RuntimeState = "recovering"
	StateStopped RuntimeState = "stopped"
	StateUnavailable RuntimeState = "unavailable"
)

// Frame — LiveView CDP Screencast 帧.
type Frame struct {
	Data byte // JPEG 数据 (raw bytes)
	Timestamp time.Time
	SessionID int64
	FrameNo int64
	Width int
	Height int
}

// Cookie — 浏览器 Cookie，序列化存储到 TS-01 KV.
type Cookie struct {
	Name string `json:"name"`
	Value string `json:"value"`
	Domain string `json:"domain"`
	Path string `json:"path"`
	Expires time.Time `json:"expires"`
	HTTPOnly bool `json:"http_only"`
	Secure bool `json:"secure"`
	SameSite string `json:"same_site,omitempty"`
}

// NavigateResult — browser_navigate 返回结果.
type NavigateResult struct {
	Status int // HTTP 状态码
	Title string // 页面标题
	URL string // 最终 URL（含重定向）
}

// ExtractResult — browser_extract 返回结果.
type ExtractResult struct {
	Text string // 文本内容
	HTML string // 内部 HTML
	Count int // 匹配元素数
	Items string // multiple=true 时多个文本
}

// BrowserTask — browser_tasks 表实体，memoryDB 持久化.
type BrowserTask struct {
	ID int64
	SessionID int64
	WorkspaceID int64
	URL string
	Status string // pending|running|completed|failed
	Screenshot byte
	CreatedAt time.Time
	CompletedAt *time.Time
}

// ScreencastConfig — Screencast 配置参数.
type ScreencastConfig struct {
	Format string // "jpeg"
	Quality int // 1-100
	MaxFPS int // 最大帧率
	MaxWidth int
	MaxHeight int
}

// DefaultScreencastConfig 返回默认 Screencast 配置.
func DefaultScreencastConfig ScreencastConfig {
	return ScreencastConfig{
		Format: "jpeg"
		Quality: 80
		MaxFPS: 10
		MaxWidth: 1280
		MaxHeight: 720
	}
}

// EventBrowserCrashed — Chrome 崩溃事件.
type EventBrowserCrashed struct{}

// EventBrowserRestarted — Chrome 重启成功事件.
type EventBrowserRestarted struct{}

// EventBrowserDownloadProgress — Chromium 下载进度事件.
type EventBrowserDownloadProgress struct {
	Percent float64
	BytesDownloaded int64
	TotalBytes int64
}

// EventTakeoverStateChanged — Takeover 状态变更事件 (browser 订阅).
type EventTakeoverStateChanged struct {
	Active bool
	SessionID int64
}

// EventSecureInputRequired — 密码字段安全输入事件 (前端弹窗触发).
type EventSecureInputRequired struct {
	TaskID int64
	Selector string
	Label string
}
