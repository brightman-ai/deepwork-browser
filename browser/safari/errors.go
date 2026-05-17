package safari

import "errors"

var (
	// ErrSafariLiveViewNotSupported — Safari engine 不支持 LiveView (CLI-only)。
	ErrSafariLiveViewNotSupported = errors.New("browser: Safari engine does not support LiveView (CLI-only)")

	// ErrSafariEvalJSNotSupported — 保留兼容性，WebDriver 模式下 EvalJS 已通过 execute/sync 实现。
	// 此错误不再主动返回，仅在 JSON 反序列化失败时作为包装目标使用。
	ErrSafariEvalJSNotSupported = errors.New("browser: Safari engine EvalJS not available")

	// ErrSafariTakeoverNotSupported — Safari engine 不支持 Takeover 模式。
	ErrSafariTakeoverNotSupported = errors.New("browser: Safari engine does not support Takeover mode (CLI-only)")

	// ErrSafariNotBooted — Simulator 未启动。
	ErrSafariNotBooted = errors.New("browser: Safari Simulator not booted")

	// ErrSafariAXPermission — macOS Accessibility 权限未授予。
	ErrSafariAXPermission = errors.New("browser: macOS Accessibility permission not granted — open System Settings > Privacy & Security > Accessibility and add this app")
)
