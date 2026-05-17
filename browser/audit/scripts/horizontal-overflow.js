(function () {
  var vpWidth = window.innerWidth;
  var docScrollWidth = document.documentElement.scrollWidth;

  if (docScrollWidth <= vpWidth) {
    return { status: 'pass', message: 'No horizontal overflow detected', violations: [] };
  }

  var violations = [];
  var visited = new Set();

  function buildSelector(el) {
    var testid = el.getAttribute('data-testid') || '';
    if (testid) return '[data-testid="' + testid + '"]';
    if (el.id) return '#' + el.id;
    return el.tagName.toLowerCase() + (el.className && typeof el.className === 'string' ? '.' + el.className.trim().split(/\s+/)[0] : '');
  }

  function scan(el) {
    if (!el || visited.has(el) || violations.length >= 20) return;
    visited.add(el);

    var rect = el.getBoundingClientRect();
    var sw = el.scrollWidth;

    // 判断是否溢出：右边界超出 viewport 或 scrollWidth > clientWidth
    var overflowsByRect = rect.right > vpWidth + 1;
    var overflowsByScroll = sw > el.clientWidth && el.clientWidth > 0;

    if (overflowsByRect || overflowsByScroll) {
      violations.push({
        selector: buildSelector(el),
        role: el.getAttribute('role') || el.tagName.toLowerCase(),
        name: el.getAttribute('aria-label') || '',
        testid: el.getAttribute('data-testid') || '',
        actual: { scrollWidth: sw, rectRight: Math.round(rect.right) },
        expected: { maxWidth: vpWidth },
        fix: 'Add max-width:100% or overflow-x:hidden to prevent horizontal overflow',
      });
    }

    // 递归子元素
    Array.from(el.children).forEach(scan);
  }

  Array.from(document.body.children).forEach(scan);

  // 如果没找到具体元素（罕见），上报文档层
  if (violations.length === 0) {
    violations.push({
      selector: 'html',
      role: 'document',
      name: '',
      testid: '',
      actual: { scrollWidth: docScrollWidth },
      expected: { maxWidth: vpWidth },
      fix: 'Identify and constrain elements causing horizontal overflow',
    });
  }

  return {
    status: 'fail',
    message: 'Page has horizontal overflow: document scrollWidth ' + docScrollWidth + 'px > viewport ' + vpWidth + 'px',
    violations: violations,
  };
})()
