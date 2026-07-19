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

  // fixed-bottom 锚定检测（REQ-BC-05 MODIFIED(2)）：元素链上存在 position:fixed
  // 且底边锚定布局视口底 —— 真机 bars-expanded 时 layout viewport=svh，此类元素
  // 在底栏上方可见 = 双视口模型残留假阳（引擎级不可修）→ 降级注记不判 fail。
  // 键盘区(zone.state='keyboard')不适用：键盘在真机上同样盖住 fixed 元素。
  // 基准 = zones 推导的 lvh（zone 底边即布局视口底，Go 侧 SSOT 注入）：svh 单位
  // override 会把 documentElement.clientHeight 改成 svh，而 fixed 元素实际锚定
  // ICB=lvh —— 用 clientHeight 判定恒不命中 = 豁免死代码（评审实测抓获）。
  var lvh = 0;
  for (var zi = 0; zi < zones.length; zi++) lvh = Math.max(lvh, zones[zi].y + zones[zi].h);
  function isFixedBottomAnchored(el) {
    try {
      for (var n = el; n && n !== document.documentElement; n = n.parentElement) {
        if (window.getComputedStyle(n).position === 'fixed') {
          var fr = n.getBoundingClientRect();
          if (Math.abs(fr.bottom - lvh) <= 2) return true;
        }
      }
    } catch (e) {}
    return false;
  }

  var violations = [];
  var MAX_VIOLATIONS = 20;
  // 计数器扫全量（详情列表才截断到 MAX）：fixedOnly 全豁免判定不能只看被截断的
  // 前 20 条 —— 第 21+ 条若是真遮挡会被 every() 漏过 = 假绿（评审抓获，推断级）。
  var totalHits = 0, nonExemptHits = 0, kbHits = 0;
  var all = document.body ? document.body.querySelectorAll('*') : [];
  for (var k = 0; k < all.length; k++) {
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
      var isKb = zones[z].state === 'keyboard';
      var fixedAnchored = !isKb && isFixedBottomAnchored(el);
      totalHits++;
      if (isKb) kbHits++;
      if (!isKb && !fixedAnchored) nonExemptHits++;
      if (violations.length >= MAX_VIOLATIONS) break;
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
          fixedBottomAnchored: fixedAnchored,
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

  if (!totalHits) {
    return { status: 'pass', message: 'no elements intersect the simulated browser chrome occlusion zone (scale=' + scale + ')', violations: [] };
  }

  // 键盘区命中（REQ-BC-12）：真遮挡，无 protected/fixed-bottom 豁免——真机上
  // 键盘对所有页面（含 dvh/safe-area/fixed 模范页）都是覆盖层；自适应页应消费
  // visualViewport.height 把布局收上来（仿真已让 vv 真实收窄）。
  var kbViolations = violations.filter(function (v) { return v.actual.zoneState === 'keyboard'; });
  if (kbHits > 0) {
    var kbInteractive = kbViolations.filter(function (v) { return v.actual.kind === 'interactive-occluded'; }).length;
    return {
      status: 'fail',
      message: kbInteractive + ' interactive / ' + (kbHits - kbInteractive) + ' content element(s) occluded by the soft keyboard — layout does not consume visualViewport.height (keyboard-resize bug class); fix: pin layout height to visualViewport.height and listen to its resize event',
      violations: kbViolations.map(function (v) { v.fix = 'Consume visualViewport.height (+resize listener) so the composer/toolbar rides above the keyboard'; return v; }),
    };
  }

  if (protections.dvh || protections.safeArea) {
    // protected：页面已声明 dvh/safe-area 适配 —— 双视口仿真下的相交大概率为
    // 模型假阳（真机上该页会正确避开 chrome）。压误报：pass + protected 标记，
    // 终判交真机对标（--engine safari）。
    return {
      status: 'pass',
      message: totalHits + ' element(s) intersect the chrome zone BUT page declares dvh/safe-area protections (likely correct on a real device) — marked protected; verify with --engine safari if in doubt',
      violations: violations.map(function (v) { v.actual.kind = 'protected'; return v; }),
    };
  }

  // fixed-bottom 全豁免（REQ-BC-05 MODIFIED(2)）：命中元素全部锚定 fixed 视口底
  // → 真机 bars-expanded 时可见（layout viewport=svh），残留假阳降级注记。
  // 框架状态词表无 warn：同 protected 先例用 pass + 标记表达。
  // 判定用全量计数器 nonExemptHits（详情列表截断到 MAX，不可作全豁免依据）。
  if (nonExemptHits === 0) {
    return {
      status: 'pass',
      message: totalHits + ' element(s) in the chrome band are anchored to a position:fixed bottom bar — on a real device (bars expanded, layout viewport=svh) these sit above the toolbar and are likely visible; residual dual-viewport false positive, verify with --engine safari if in doubt',
      violations: violations.map(function (v) { v.actual.kind = 'fixed-anchored'; return v; }),
    };
  }

  return {
    status: 'fail',
    message: interactiveCount + ' interactive / ' + (violations.length - interactiveCount) + ' content element(s) (of ' + totalHits + ' total) occluded by simulated browser chrome (safari bottom bar) — a real user cannot see/tap these; page lacks dvh/safe-area handling',
    violations: violations,
  };
})()
