(function () {
  var minSize = (typeof __params !== 'undefined' && __params.minSize != null) ? __params.minSize : 44;

  var selectors = [
    'a[href]',
    'button',
    'input',
    'select',
    'textarea',
    '[role="button"]',
    '[role="link"]',
    '[onclick]',
    '[tabindex]:not([tabindex="-1"])',
  ].join(', ');

  var elements = document.querySelectorAll(selectors);
  var violations = [];

  elements.forEach(function (el) {
    // 排除不可见元素
    var style = window.getComputedStyle(el);
    if (style.visibility === 'hidden' || style.display === 'none') return;
    if (el.offsetParent === null && style.position !== 'fixed') return;

    var rect = el.getBoundingClientRect();
    // 零尺寸元素跳过（未渲染）
    if (rect.width === 0 && rect.height === 0) return;

    if (rect.width < minSize || rect.height < minSize) {
      // 构建 selector
      var testid = el.getAttribute('data-testid') || '';
      var selector = testid
        ? '[data-testid="' + testid + '"]'
        : el.id
        ? '#' + el.id
        : el.tagName.toLowerCase() + (el.className && typeof el.className === 'string' ? '.' + el.className.trim().split(/\s+/)[0] : '');

      violations.push({
        selector: selector,
        role: el.getAttribute('role') || el.tagName.toLowerCase(),
        name: el.getAttribute('aria-label') || el.getAttribute('name') || el.textContent.trim().slice(0, 40) || '',
        testid: testid,
        actual: { width: Math.round(rect.width), height: Math.round(rect.height) },
        expected: { minWidth: minSize, minHeight: minSize },
        fix: 'Increase element to at least ' + minSize + 'x' + minSize + 'px, or add padding/min-width/min-height',
      });
    }
  });

  if (violations.length === 0) {
    return { status: 'pass', message: 'All interactive elements meet the ' + minSize + 'px touch target size requirement', violations: [] };
  }
  return {
    status: 'fail',
    message: violations.length + ' element(s) smaller than ' + minSize + 'px touch target size',
    violations: violations,
  };
})()
