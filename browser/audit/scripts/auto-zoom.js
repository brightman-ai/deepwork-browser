(function () {
  var minFontSize = (typeof __params !== 'undefined' && __params.minFontSize != null) ? __params.minFontSize : 16;

  var elements = document.querySelectorAll('input, textarea, select');
  var violations = [];

  elements.forEach(function (el) {
    // 排除不可见元素
    var style = window.getComputedStyle(el);
    if (style.visibility === 'hidden' || style.display === 'none') return;
    if (el.offsetParent === null && style.position !== 'fixed') return;

    var fontSize = parseFloat(style.fontSize);
    if (isNaN(fontSize)) return;

    if (fontSize < minFontSize) {
      var testid = el.getAttribute('data-testid') || '';
      var selector = testid
        ? '[data-testid="' + testid + '"]'
        : el.id
        ? '#' + el.id
        : el.name
        ? el.tagName.toLowerCase() + '[name="' + el.name + '"]'
        : el.tagName.toLowerCase() + (el.className && typeof el.className === 'string' ? '.' + el.className.trim().split(/\s+/)[0] : '');

      violations.push({
        selector: selector,
        role: el.tagName.toLowerCase(),
        name: el.getAttribute('aria-label') || el.getAttribute('name') || el.getAttribute('placeholder') || '',
        testid: testid,
        actual: { fontSize: fontSize },
        expected: { minFontSize: minFontSize },
        fix: 'Set font-size >= ' + minFontSize + 'px to prevent iOS Safari auto-zoom on focus',
      });
    }
  });

  if (violations.length === 0) {
    return { status: 'pass', message: 'All input elements have font-size >= ' + minFontSize + 'px', violations: [] };
  }
  return {
    status: 'fail',
    message: violations.length + ' input element(s) have font-size < ' + minFontSize + 'px, may trigger iOS Safari auto-zoom',
    violations: violations,
  };
})()
