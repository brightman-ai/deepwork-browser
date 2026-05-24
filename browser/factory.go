package browser

// CoreFactoryRequest 统一 BrowserCore 创建请求。
// 实际的 factory 函数（switch-case）放在 CLI 层（cmd/dw-browser/main.go），
// 避免 internal/browser 导入子包 internal/browser/safari 造成循环依赖。
type CoreFactoryRequest struct {
	Engine      BrowserEngine   // EngineChrome | EngineSafari
	DeviceQuery string          // Safari: 设备名/预设名
	DeviceUDID  string          // Safari: 直接指定 UDID
	ProfileID   string          // Chrome: profile ID
	Options     []BrowserOption // Chrome: 浏览器选项
}
