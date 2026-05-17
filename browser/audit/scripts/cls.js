(function () {
  var params = (typeof __params !== 'undefined' && __params) || {};
  var maxCLS = typeof params.maxCLS === 'number' ? params.maxCLS : 0.1;

  var entries = [];
  try {
    entries = performance.getEntriesByType('layout-shift');
  } catch (e) {
    return {
      status: 'error',
      message: 'PerformanceObserver layout-shift not supported: ' + e.message,
      violations: [],
    };
  }

  // CLS = sum of layout-shift entries that were NOT preceded by recent user input
  var cls = 0;
  entries.forEach(function (entry) {
    if (!entry.hadRecentInput) {
      cls += entry.value;
    }
  });

  // Round to 4 decimal places for readability
  cls = Math.round(cls * 10000) / 10000;

  if (cls <= maxCLS) {
    return {
      status: 'pass',
      message: 'CLS: ' + cls + ' (threshold: ' + maxCLS + ')',
      violations: [],
    };
  }

  return {
    status: 'fail',
    message: 'CLS: ' + cls + ' (threshold: ' + maxCLS + ')',
    violations: [
      {
        selector: 'page',
        role: 'document',
        name: '',
        testid: '',
        actual: { cls: cls },
        expected: { maxCLS: maxCLS },
        fix: 'Reduce layout shifts by reserving space for images/ads (width/height attributes), avoiding inserting content above existing content, and using CSS transform for animations instead of properties that trigger layout',
      },
    ],
  };
})()
