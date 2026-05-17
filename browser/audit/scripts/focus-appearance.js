(function () {
  var minOutlineWidth = (typeof __params !== 'undefined' && __params.minOutlineWidth != null) ? __params.minOutlineWidth : 2;

  var violations = [];

  // 遍历所有 StyleSheet，查找 :focus 规则中是否有 outline:none/0 且无补偿样式
  var sheets;
  try {
    sheets = Array.prototype.slice.call(document.styleSheets);
  } catch (e) {
    return { status: 'error', message: 'Cannot access styleSheets: ' + e.message, violations: [] };
  }

  sheets.forEach(function (sheet) {
    var rules;
    try {
      rules = sheet.cssRules || sheet.rules;
    } catch (e) {
      // 跨域 stylesheet 无法读取，跳过
      return;
    }
    if (!rules) return;

    Array.prototype.slice.call(rules).forEach(function (rule) {
      if (!rule.selectorText || !rule.style) return;

      // 匹配含 :focus 的选择器（包括 :focus-visible, :focus-within）
      if (!/:focus/.test(rule.selectorText)) return;

      var outline = rule.style.outline || '';
      var outlineWidth = rule.style.outlineWidth || '';
      var outlineStyle = rule.style.outlineStyle || '';

      var hasOutlineNone = outline === 'none' || outline === '0' ||
        outlineStyle === 'none' ||
        outlineWidth === '0' || outlineWidth === '0px';

      if (!hasOutlineNone) return;

      // 检查是否有补偿样式
      var boxShadow = rule.style.boxShadow || '';
      var border = rule.style.border || '';
      var borderWidth = rule.style.borderWidth || '';
      var backgroundColor = rule.style.backgroundColor || '';

      var hasCompensation = (boxShadow && boxShadow !== 'none') ||
        (border && border !== 'none') ||
        (borderWidth && borderWidth !== '0' && borderWidth !== '0px');

      if (!hasCompensation) {
        violations.push({
          selector: rule.selectorText,
          actual: { outline: 'none' },
          expected: { minOutlineWidth: minOutlineWidth },
          fix: 'Remove "outline: none" or add compensating focus indicator via box-shadow or border (WCAG 2.4.11)',
        });
      }
    });
  });

  if (violations.length === 0) {
    return {
      status: 'pass',
      message: 'No :focus rules suppress outline without compensation',
      violations: [],
    };
  }
  return {
    status: 'fail',
    message: violations.length + ' CSS rule(s) suppress focus outline without visible compensation',
    violations: violations,
  };
})()
