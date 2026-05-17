(function () {
  var params = (typeof __params !== 'undefined' && __params) || {};

  function buildSelector(el) {
    var testid = el.getAttribute('data-testid') || '';
    if (testid) return '[data-testid="' + testid + '"]';
    if (el.id) return '#' + el.id;
    return el.tagName.toLowerCase() + (el.className && typeof el.className === 'string' && el.className.trim() ? '.' + el.className.trim().split(/\s+/)[0] : '');
  }

  function getZIndex(el) {
    var z = window.getComputedStyle(el).zIndex;
    return z === 'auto' ? null : parseInt(z, 10);
  }

  function rectsOverlap(a, b) {
    return !(a.right <= b.left || b.right <= a.left || a.bottom <= b.top || b.bottom <= a.top);
  }

  function hasImplicitStackingContext(el) {
    var style = window.getComputedStyle(el);
    if (style.transform && style.transform !== 'none') return 'transform';
    if (style.opacity && parseFloat(style.opacity) < 1) return 'opacity<1';
    if (style.filter && style.filter !== 'none') return 'filter';
    if (style.isolation === 'isolate') return 'isolation:isolate';
    if (style.willChange && /transform|opacity|filter/.test(style.willChange)) return 'will-change:' + style.willChange;
    return null;
  }

  // Collect all fixed/sticky elements
  var all = Array.from(document.querySelectorAll('*'));
  var positioned = [];
  all.forEach(function (el) {
    var pos = window.getComputedStyle(el).position;
    if (pos === 'fixed' || pos === 'sticky') {
      positioned.push({ el: el, pos: pos });
    }
  });

  var violations = [];

  // Check z-index conflicts: pairs with same z-index and overlapping bounding rects
  for (var i = 0; i < positioned.length && violations.length < 20; i++) {
    var a = positioned[i];
    var zA = getZIndex(a.el);
    if (zA === null) continue;
    var rectA = a.el.getBoundingClientRect();

    for (var j = i + 1; j < positioned.length && violations.length < 20; j++) {
      var b = positioned[j];
      var zB = getZIndex(b.el);
      if (zB === null) continue;
      if (zA !== zB) continue;

      var rectB = b.el.getBoundingClientRect();
      if (!rectsOverlap(rectA, rectB)) continue;

      violations.push({
        selector: buildSelector(a.el) + ' vs ' + buildSelector(b.el),
        role: a.el.getAttribute('role') || a.el.tagName.toLowerCase(),
        name: a.el.getAttribute('aria-label') || '',
        testid: a.el.getAttribute('data-testid') || '',
        actual: { zIndex: zA, position: a.pos },
        expected: { uniqueZIndex: true },
        fix: 'Assign distinct z-index values to overlapping fixed/sticky elements',
      });
    }
  }

  // Check implicit stacking contexts on fixed/sticky elements
  positioned.forEach(function (item) {
    var reason = hasImplicitStackingContext(item.el);
    if (!reason) return;
    if (violations.length >= 20) return;
    violations.push({
      selector: buildSelector(item.el),
      role: item.el.getAttribute('role') || item.el.tagName.toLowerCase(),
      name: item.el.getAttribute('aria-label') || '',
      testid: item.el.getAttribute('data-testid') || '',
      actual: { position: item.pos, implicitStackingContext: reason },
      expected: { uniqueZIndex: true },
      fix: 'Implicit stacking context created by ' + reason + '; ensure z-index layering is intentional',
    });
  });

  if (violations.length === 0) {
    return { status: 'pass', message: 'No stacking context conflicts detected', violations: [] };
  }

  return {
    status: 'fail',
    message: 'Detected ' + violations.length + ' stacking context conflict(s) among fixed/sticky elements',
    violations: violations,
  };
})()
