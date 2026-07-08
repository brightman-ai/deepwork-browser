package browser

import _ "embed"

// PersonaSelfCheckHTML 是 persona 自检夹具页(REQ-10)。
// 加载后把运行时"实现事实"信号 dump 成 JSON 到 <pre id="dump">,供:
//   - selfcheck 断言(人格声明值 == 实测值);
//   - Witness 目视验收。
//
//go:embed devicedata/persona-selfcheck.html
var PersonaSelfCheckHTML string

// PersonaSelfCheckJS 返回一个 IIFE,求值得到与夹具页同一份信号对象的 JSON 串。
// 供测试/探针在任意页面上直接 Evaluate 取信号(无需先导航到夹具页)。
// 信号定义须与 persona-selfcheck.html 的 dwPersonaSignals() 保持一致。
const PersonaSelfCheckJS = `(function(){
  var uad = navigator.userAgentData;
  return JSON.stringify({
    userAgent: navigator.userAgent,
    uaData: uad ? { mobile: uad.mobile, platform: uad.platform,
      brands: (uad.brands||[]).map(function(b){return b.brand;}) } : null,
    screen: { w: screen.width, h: screen.height, dpr: window.devicePixelRatio },
    media: {
      hoverNone: matchMedia('(hover: none)').matches,
      pointerCoarse: matchMedia('(pointer: coarse)').matches,
      dark: matchMedia('(prefers-color-scheme: dark)').matches
    },
    serviceWorker: ('serviceWorker' in navigator),
    clipboard: !!(navigator.clipboard && navigator.clipboard.writeText),
    notification: (typeof Notification !== 'undefined') ? Notification.permission : 'absent',
    tz: Intl.DateTimeFormat().resolvedOptions().timeZone,
    locale: Intl.DateTimeFormat().resolvedOptions().locale,
    maxTouchPoints: navigator.maxTouchPoints,
    weixinJSBridge: (typeof WeixinJSBridge !== 'undefined'),
    wxEnv: (typeof __wxjs_environment !== 'undefined') ? __wxjs_environment : null
  });
})()`
