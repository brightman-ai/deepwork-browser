(function () {
  // zones: 屏幕(视觉视口)CSS 坐标系的 chrome 遮挡矩形（Go 侧从设备几何 SSOT 注入）。
  // 空 = 本会话未启用 browser chrome 仿真 → 检查不适用（pass, 不造假阳）。
  var zones = (typeof __params !== 'undefined' && __params.zones) || [];
  if (!zones.length) {
    return { status: 'pass', message: 'browser chrome simulation not enabled for this session — check not applicable', violations: [] };
  }

  // 屏幕坐标 → 页面坐标（当前缩放/平移态下比较元素 rect；audit 在调用方设定的
  // 视口状态下运行：配合 act "zoom" 可扫缩放态）。
  var vv = window.visualViewport || { scale: 1, offsetTop: 0, offsetLeft: 0 };
  var scale = vv.scale || 1;
  function zoneInPageCoords(z) {
    return {
      left: z.x / scale + (vv.offsetLeft || 0),
      top: z.y / scale + (vv.offsetTop || 0),
      right: (z.x + z.w) / scale + (vv.offsetLeft || 0),
      bottom: (z.y + z.h) / scale + (vv.offsetTop || 0),
    };
  }
  var pageZones = zones.map(zoneInPageCoords);

  // 页面防护声明（dvh/svh、safe-area-inset）：命中则本页"看似被遮"多半是仿真
  // 双视口模型的已知假阳（Chrome 单视口分不出 dvh/lvh）→ 降级不判 fail。
  var css = '';
  try {
    for (var i = 0; i < document.styleSheets.length; i++) {
      try {
        var rules = document.styleSheets[i].cssRules || [];
        for (var j = 0; j < rules.length; j++) css += rules[j].cssText;
      } catch (e) { /* cross-origin sheet: skip */ }
    }
  } catch (e) {}
  css += (document.documentElement.getAttribute('style') || '');
  if (document.body) css += (document.body.getAttribute('style') || '');
  var protections = {
    dvh: /\b\d+(\.\d+)?(dvh|svh)\b/.test(css),
    safeArea: /safe-area-inset/.test(css),
  };

  var INTERACTIVE = 'a,button,input,select,textarea,[role="button"],[role="link"],[role="tab"],[role="menuitem"],[onclick],[tabindex]';
  function isInteractive(el) {
    try { return el.matches(INTERACTIVE) || !!el.closest(INTERACTIVE); } catch (e) { return false; }
  }
  function selectorFor(el) {
    var testid = el.getAttribute && el.getAttribute('data-testid');
    if (testid) return '[data-testid="' + testid + '"]';
    if (el.id) return '#' + el.id;
    var cls = (typeof el.className === 'string' && el.className.trim()) ? '.' + el.className.trim().split(/\s+/)[0] : '';
    return el.tagName.toLowerCase() + cls;
  }

  var violations = [];
  var MAX_VIOLATIONS = 20;
  var all = document.body ? document.body.querySelectorAll('*') : [];
  for (var k = 0; k < all.length && violations.length < MAX_VIOLATIONS; k++) {
    var el = all[k];
    var st = window.getComputedStyle(el);
    if (st.visibility === 'hidden' || st.display === 'none' || parseFloat(st.opacity) === 0) continue;
    var r = el.getBoundingClientRect();
    if (r.width <= 0 || r.height <= 0) continue;
    for (var z = 0; z < pageZones.length; z++) {
      var pz = pageZones[z];
      if (r.bottom <= pz.top || r.top >= pz.bottom || r.right <= pz.left || r.left >= pz.right) continue;
      var iy = Math.min(r.bottom, pz.bottom) - Math.max(r.top, pz.top);
      // 贴线 1-2px 的相交是布局噪音；遮挡判定要求纵向侵入 ≥ 4px。
      if (iy < 4) continue;
      var interactive = isInteractive(el);
      // 只报叶子/交互元素，避免"整个滚动容器都相交"的容器噪音刷屏。
      if (!interactive && el.children.length > 0) continue;
      violations.push({
        selector: selectorFor(el),
        role: el.tagName.toLowerCase(),
        name: (el.getAttribute && (el.getAttribute('aria-label') || '')) || (el.textContent || '').trim().slice(0, 40),
        testid: (el.getAttribute && el.getAttribute('data-testid')) || '',
        actual: {
          kind: interactive ? 'interactive-occluded' : 'content-occluded',
          rect: { left: Math.round(r.left), top: Math.round(r.top), width: Math.round(r.width), height: Math.round(r.height) },
          occludedPx: Math.round(iy),
          zoneState: zones[z].state || 'expanded',
          pageScale: scale,
          pageProtections: protections,
        },
        expected: { clearOfChromeZone: true },
        fix: 'Size layout with 100dvh (not 100vh) and pad bottom UI with env(safe-area-inset-bottom); keep interactive elements above the browser chrome band',
      });
      break;
    }
  }

  var interactiveCount = violations.filter(function (v) { return v.actual.kind === 'interactive-occluded'; }).length;

  if (!violations.length) {
    return { status: 'pass', message: 'no elements intersect the simulated browser chrome occlusion zone (scale=' + scale + ')', violations: [] };
  }
  if (protections.dvh || protections.safeArea) {
    // protected：页面已声明 dvh/safe-area 适配 —— 双视口仿真下的相交大概率为
    // 模型假阳（真机上该页会正确避开 chrome）。压误报：pass + protected 标记，
    // 终判交真机对标（--engine safari）。
    return {
      status: 'pass',
      message: violations.length + ' element(s) intersect the chrome zone BUT page declares dvh/safe-area protections (likely correct on a real device) — marked protected; verify with --engine safari if in doubt',
      violations: violations.map(function (v) { v.actual.kind = 'protected'; return v; }),
    };
  }
  return {
    status: 'fail',
    message: interactiveCount + ' interactive / ' + (violations.length - interactiveCount) + ' content element(s) occluded by simulated browser chrome (safari bottom bar) — a real user cannot see/tap these; page lacks dvh/safe-area handling',
    violations: violations,
  };
})()
