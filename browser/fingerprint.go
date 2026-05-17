// Package browser — Browser Fingerprint Presets.
// 消除 headless Chrome 指纹，模拟真实浏览器环境。
// 设计依据: + Profile Management
package browser

import (
	"fmt"
	"runtime"
	"strings"
)

const (
	PresetWindowsChrome = "windows-chrome"
	PresetLinuxChrome = "linux-chrome"
	PresetMacOSChrome = "macos-chrome"
	PresetAndroidChrome = "android-chrome"
	PresetMacOSSafariUA = "macos-safari-ua"
	PresetIPhoneSafariUA = "iphone-safari-ua"
)

// FingerprintPreset 浏览器指纹预设。
type FingerprintPreset struct {
	ID string `json:"id"`
	Name string `json:"name"`
	UserAgent string `json:"user_agent"`
	Platform string `json:"platform"` // navigator.platform
	Vendor string `json:"vendor"` // navigator.vendor
	Languages string `json:"languages"` // JS array literal
	WebGLVendor string `json:"webgl_vendor"`
	WebGLRenderer string `json:"webgl_renderer"`
	ViewportW int `json:"viewport_w"`
	ViewportH int `json:"viewport_h"`
	DeviceScaleFactor float64 `json:"device_scale_factor"`
	Mobile bool `json:"mobile"`
	Touch bool `json:"touch"`
	MaxTouchPoints int `json:"max_touch_points"`
}

// BuiltinPresets 内置浏览器指纹预设 (2026-04 主流版本 + 主流硬件)。
//
// Languages 选择: 仅 en-US/en — 中文 IP + Chromium + zh-CN Accept-Language 是
// Cloudflare 反爬高危组合 (中国区 ASN 已被标记),反而拉低 Turnstile 通过率。
// 真实用户可以随时在 Chrome 设置切换语言,fingerprint 层不暴露 zh 即可。
var BuiltinPresets = map[string]*FingerprintPreset{
	PresetWindowsChrome: {
		ID: PresetWindowsChrome
		Name: "Windows 11 · Chrome 133"
		UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
		Platform: "Win32"
		Vendor: "Google Inc."
		Languages: "['en-US','en']"
		WebGLVendor: "Google Inc. (NVIDIA)"
		WebGLRenderer: "ANGLE (NVIDIA, NVIDIA GeForce RTX 4070 Direct3D11 vs_5_0 ps_5_0, D3D11)"
		ViewportW: 1920, ViewportH: 1080
		DeviceScaleFactor: 1.0
	}
	PresetLinuxChrome: {
		ID: PresetLinuxChrome
		Name: "Ubuntu 24.04 · Chrome 133"
		UserAgent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
		Platform: "Linux x86_64"
		Vendor: "Google Inc."
		Languages: "['en-US','en']"
		WebGLVendor: "Google Inc. (NVIDIA)"
		WebGLRenderer: "ANGLE (NVIDIA, NVIDIA GeForce RTX 4070 OpenGL ES 3.2, OpenGL 4.6.0)"
		ViewportW: 1920, ViewportH: 1080
		DeviceScaleFactor: 1.0
	}
	PresetMacOSChrome: {
		ID: PresetMacOSChrome
		Name: "macOS Sequoia · Chrome 133"
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
		Platform: "MacIntel"
		Vendor: "Google Inc."
		Languages: "['en-US','en']"
		WebGLVendor: "Google Inc. (Apple)"
		WebGLRenderer: "ANGLE (Apple, ANGLE Metal Renderer: Apple M4 Pro, Unspecified Version)"
		ViewportW: 1512, ViewportH: 982, // MacBook Pro 14" 默认缩放 (主流开发机)
		DeviceScaleFactor: 2.0
	}
	PresetAndroidChrome: {
		ID: PresetAndroidChrome
		Name: "Android · Chrome 133"
		UserAgent: "Mozilla/5.0 (Linux; Android 15; Pixel 9) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Mobile Safari/537.36"
		Platform: "Linux armv8l"
		Vendor: "Google Inc."
		Languages: "['en-US','en']"
		WebGLVendor: "Google Inc. (Qualcomm)"
		WebGLRenderer: "ANGLE (Qualcomm, Adreno 750, OpenGL ES 3.2)"
		ViewportW: 412, ViewportH: 923
		DeviceScaleFactor: 2.625
		Mobile: true
		Touch: true
		MaxTouchPoints: 5
	}
	PresetMacOSSafariUA: {
		ID: PresetMacOSSafariUA
		Name: "macOS Sequoia · Safari UA 模拟"
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.3 Safari/605.1.15"
		Platform: "MacIntel"
		Vendor: "Apple Computer, Inc."
		Languages: "['en-US','en']"
		WebGLVendor: "Apple"
		WebGLRenderer: "Apple M4 Pro"
		ViewportW: 1512, ViewportH: 982, // MacBook Pro 14" 默认缩放 (主流开发机)
		DeviceScaleFactor: 2.0
	}
	PresetIPhoneSafariUA: {
		ID: PresetIPhoneSafariUA
		Name: "iPhone · Safari UA 模拟"
		UserAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_3 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.3 Mobile/15E148 Safari/604.1"
		Platform: "iPhone"
		Vendor: "Apple Computer, Inc."
		Languages: "['en-US','en']"
		WebGLVendor: "Apple Inc."
		WebGLRenderer: "Apple GPU"
		// iPhone 16 Pro Max 实际 CSS viewport 是 430x932 (Apple 官方 HIG 规格)
		// 此前注释"430x932 是 iPhone 15 Pro Max"属错误记载,二者同尺寸。
		ViewportW: 430, ViewportH: 932
		DeviceScaleFactor: 3.0
		Mobile: true
		Touch: true
		MaxTouchPoints: 5
	}
}

// PresetOrder 前端展示顺序。
var PresetOrder = string{
	PresetWindowsChrome
	PresetLinuxChrome
	PresetMacOSChrome
	PresetAndroidChrome
	PresetMacOSSafariUA
	PresetIPhoneSafariUA
}

// DefaultPresetID 返回当前平台的默认桌面浏览器指纹。
// 目的: 避免 macOS/Windows 本地调试时仍落回 windows-chrome 的硬编码默认。
func DefaultPresetID string {
	switch runtime.GOOS {
	case "darwin":
		return PresetMacOSChrome
	case "windows":
		return PresetWindowsChrome
	default:
		return PresetLinuxChrome
	}
}

// NormalizePresetID 只处理空值与空白。
// 未知 ID 会原样返回，必须由 ValidatePresetID 或调用方报错，避免旧契约静默回退。
func NormalizePresetID(presetID string) string {
	presetID = strings.TrimSpace(presetID)
	if presetID == "" {
		return DefaultPresetID
	}
	return presetID
}

// ValidatePresetID 返回已知 preset；未知值必须显式失败。
func ValidatePresetID(presetID string) (string, error) {
	presetID = NormalizePresetID(presetID)
	if _, ok := BuiltinPresets[presetID]; !ok {
		return "", fmt.Errorf("unknown preset_id %q (valid: %s)", presetID, strings.Join(PresetOrder, ", "))
	}
	return presetID, nil
}

// MinimalWebdriverStealthScript is used only for real headed Chrome
// (headed/visible modes). In those modes the browser should expose its native
// WebGL, screen, fonts, canvas, audio, languages, and hardware surfaces. The
// only automation residue we remove is navigator.webdriver, because some bot
// tests fail on property presence even when the value is not truthy.
const MinimalWebdriverStealthScript = `
(function {
 try {
 var proto = Navigator.prototype;
 if (proto && Object.prototype.hasOwnProperty.call(proto, 'webdriver')) {
 delete proto.webdriver;
 }
 } catch (e) {}
 try {
 if (Object.prototype.hasOwnProperty.call(navigator, 'webdriver')) {
 delete navigator.webdriver;
 }
 } catch (e) {}
 try {
 var desc = Object.getOwnPropertyDescriptor(Navigator.prototype, 'webdriver');
 if (desc) {
 Object.defineProperty(Navigator.prototype, 'webdriver', {
 get: function { return undefined; }
 configurable: true
 });
 }
 } catch (e) {}
});
`

// GenerateStealthScript 根据 Preset 生成定制化 Stealth 脚本。
func GenerateStealthScript(p *FingerprintPreset) string {
	return fmt.Sprintf(`
(function {
 // === navigator.webdriver (终局: 完全移除属性) ===
 // bot.sannysoft.com 检测 'webdriver' in navigator — 属性必须不存在
 // 1. 删除 prototype 上的 getter
 var proto = Object.getPrototypeOf(navigator);
 if ('webdriver' in proto) {
 delete proto.webdriver;
 }
 // 2. 删除实例属性
 if ('webdriver' in navigator) {
 Object.defineProperty(navigator, 'webdriver', { get: => undefined, configurable: true });
 delete navigator.webdriver;
 }

 // === chrome.runtime ===
 if (!window.chrome) window.chrome = {};
 if (!window.chrome.runtime) {
 window.chrome.runtime = {
 id: undefined
 connect: function { return { onMessage: { addListener: function{} }, postMessage: function{} }; }
 sendMessage: function {}
 onConnect: { addListener: function{} }
 onMessage: { addListener: function{} }
 };
 }

 // === User-Agent Client Hints ===
 Object.defineProperty(navigator, 'platform', { get: => %q, configurable: true });
 Object.defineProperty(navigator, 'vendor', { get: => %q, configurable: true });
 Object.defineProperty(navigator, 'languages', { get: => %s, configurable: true });

 // === plugins (每个元素必须 instanceof Plugin) ===
 function makePlugin(name, filename, desc, mimes) {
 var p = {};
 Object.setPrototypeOf(p, Plugin.prototype);
 Object.defineProperties(p, {
 name: {value:name}, filename: {value:filename}, description: {value:desc}, length: {value:mimes.length}
 });
 for (var i=0; i<mimes.length; i++) {
 var m = {};
 Object.setPrototypeOf(m, MimeType.prototype);
 Object.defineProperties(m, {
 type: {value:mimes[i].type}, suffixes: {value:mimes[i].suffixes}
 description: {value:mimes[i].desc}, enabledPlugin: {value:p}
 });
 Object.defineProperty(p, i, {value:m});
 }
 return p;
 }
 var fakePlugins = [
 makePlugin('Chrome PDF Plugin','internal-pdf-viewer','Portable Document Format',[{type:'application/x-google-chrome-pdf',suffixes:'pdf',desc:'PDF'}])
 makePlugin('Chrome PDF Viewer','mhjfbmdgcfjbbpaeojofohoefgiehjai','',[{type:'application/pdf',suffixes:'pdf',desc:''}])
 makePlugin('Native Client','internal-nacl-plugin','',[{type:'application/x-nacl',suffixes:'',desc:'NaCl'},{type:'application/x-pnacl',suffixes:'',desc:'PNaCl'}])
 ];
 fakePlugins.item = function(i) { return this[i] || null; };
 fakePlugins.namedItem = function(n) { for(var i=0;i<this.length;i++) if(this[i].name===n) return this[i]; return null; };
 fakePlugins.refresh = function {};
 Object.setPrototypeOf(fakePlugins, PluginArray.prototype);
 Object.defineProperty(navigator, 'plugins', { get: => fakePlugins, configurable: true });

 // === WebGL vendor/renderer (关键反检测) ===
 var origGetParam = WebGLRenderingContext.prototype.getParameter;
 WebGLRenderingContext.prototype.getParameter = function(p) {
 if (p === 37445) return %q;
 if (p === 37446) return %q;
 return origGetParam.call(this, p);
 };
 if (typeof WebGL2RenderingContext !== 'undefined') {
 var origGetParam2 = WebGL2RenderingContext.prototype.getParameter;
 WebGL2RenderingContext.prototype.getParameter = function(p) {
 if (p === 37445) return %q;
 if (p === 37446) return %q;
 return origGetParam2.call(this, p);
 };
 }

 // === Permissions ===
 if (navigator.permissions) {
 var origQ = navigator.permissions.query;
 navigator.permissions.query = function(params) {
 if (params.name === 'notifications') return Promise.resolve({state: Notification.permission});
 return origQ.call(this, params);
 };
 }

 // === window dimensions (headless = 0) ===
 if (window.outerWidth === 0) {
 Object.defineProperty(window, 'outerWidth', { get: => window.innerWidth });
 Object.defineProperty(window, 'outerHeight', { get: => window.innerHeight + 85 });
 }

 // === navigator.connection (NetworkInformation API) ===
 if (!navigator.connection) {
 Object.defineProperty(navigator, 'connection', {
 get: => ({
 effectiveType: '4g', rtt: 50, downlink: 10, saveData: false
 onchange: null, addEventListener: function{}, removeEventListener: function{}
 })
 configurable: true
 });
 }

 // === navigator.maxTouchPoints ===
 Object.defineProperty(navigator, 'maxTouchPoints', {
 get: => %d
 configurable: true
 });

 // === navigator.hardwareConcurrency (合理值) ===
 Object.defineProperty(navigator, 'hardwareConcurrency', {
 get: => 8
 configurable: true
 });

 // === navigator.deviceMemory (合理值) ===
 Object.defineProperty(navigator, 'deviceMemory', {
 get: => 8
 configurable: true
 });

 // === Notification.permission (headless = 'denied') ===
 try {
 Object.defineProperty(Notification, 'permission', {
 get: => 'default'
 configurable: true
 });
 } catch(e) {}

 // === MediaDevices (模拟摄像头/麦克风) ===
 if (navigator.mediaDevices && navigator.mediaDevices.enumerateDevices) {
 var origEnum = navigator.mediaDevices.enumerateDevices.bind(navigator.mediaDevices);
 navigator.mediaDevices.enumerateDevices = function {
 return origEnum.then(function(devices) {
 if (devices.length === 0) {
 return [
 {deviceId:'',kind:'audioinput',label:'',groupId:''}
 {deviceId:'',kind:'videoinput',label:'',groupId:''}
 {deviceId:'',kind:'audiooutput',label:'',groupId:''}
 ];
 }
 return devices;
 });
 };
 }

 // === iframe contentWindow.chrome 递归注入 ===
 var origCreate = document.createElement.bind(document);
 document.createElement = function(tag) {
 var el = origCreate(tag);
 if (tag.toLowerCase === 'iframe') {
 el.addEventListener('load', function {
 try {
 if (el.contentWindow && !el.contentWindow.chrome) {
 el.contentWindow.chrome = window.chrome;
 }
 } catch(e) {} // cross-origin iframe will throw
 });
 }
 return el;
 };
});
`, p.Platform, p.Vendor, p.Languages
		p.WebGLVendor, p.WebGLRenderer
		p.WebGLVendor, p.WebGLRenderer
		p.MaxTouchPoints)
}
