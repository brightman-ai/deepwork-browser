package browser

import "fmt"

// GenerateShellScript 返回 in-app webview 壳的 JS shim(经 AddScriptToEvaluateOnNewDocument
// 在每个新 document 注入)。无壳返回 ""。
//
// 两件事(REQ-05 + L5):
//  1. bridge 对象:注入 WeixinJSBridge / window.wx(壳身份;供更深探测的 app 识别 —— deepwork
//     自身检测虽纯 UA,但完全保真需要,举一反三到企业微信)。
//  2. 能力破损:忠实微信/企业微信 webview 会让 Service Worker 注册失败、Web Push /
//     Notification / Clipboard 不可用 —— 正是 InAppBrowserGuide 存在的根因。破损后被测 app
//     的降级路径才会被触发(这与 stealth 的"健全化"相反,故 fidelity persona 不叠 stealth)。
func GenerateShellScript(p *Persona) string {
	if p == nil || p.Shell.Kind == ShellNone {
		return ""
	}

	// 企业微信额外挂 wxwork 命名空间;两者都提供 WeixinJSBridge。
	var extraBridge string
	switch p.Shell.Kind {
	case ShellWeChat:
		extraBridge = `try { if (typeof window.wx === 'undefined') window.wx = { config: function(){}, ready: function(){}, error: function(){} }; } catch(e){}`
	case ShellWeCom:
		extraBridge = `try { if (typeof window.wx === 'undefined') window.wx = { config: function(){}, ready: function(){}, error: function(){}, agentConfig: function(){}, qy: {} }; } catch(e){}
		try { window.__wxjs_environment = window.__wxjs_environment; } catch(e){}`
	}

	return fmt.Sprintf(`
(function(){
  // === in-app bridge 对象(%s) ===
  try { if (typeof window.WeixinJSBridge === 'undefined') { window.WeixinJSBridge = { invoke: function(){}, on: function(){}, call: function(){} }; } } catch(e){}
  %s

  // === 能力破损:Service Worker 注册失败(webview 无 SW) ===
  // 只从 prototype 删除 → 'serviceWorker' in navigator 变 false(REQ-05 信号)。
  // 注:不能 defineProperty 到实例上,否则实例多一个 own 属性反而使 'in' 判 true。
  try { delete Navigator.prototype.serviceWorker; } catch(e){}

  // === 能力破损:Clipboard 不可用 ===
  try { Object.defineProperty(navigator, 'clipboard', { get: function(){ return undefined; }, configurable: true }); } catch(e){}

  // === 能力破损:Web Push / Notification 拒绝 ===
  try {
    if (typeof Notification !== 'undefined') {
      Object.defineProperty(Notification, 'permission', { get: function(){ return 'denied'; }, configurable: true });
      Notification.requestPermission = function(){ return Promise.resolve('denied'); };
    }
  } catch(e){}
})();
`, p.Shell.Kind, extraBridge)
}
