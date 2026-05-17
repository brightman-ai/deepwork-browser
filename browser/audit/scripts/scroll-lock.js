(function () {
  function isOverflowLocked(val) {
    return val === 'hidden' || val === 'clip';
  }

  var bodyStyle = window.getComputedStyle(document.body);
  var htmlStyle = window.getComputedStyle(document.documentElement);

  var bodyLocked = isOverflowLocked(bodyStyle.overflow) || isOverflowLocked(bodyStyle.overflowY);
  var htmlLocked = isOverflowLocked(htmlStyle.overflow) || isOverflowLocked(htmlStyle.overflowY);

  if (!bodyLocked && !htmlLocked) {
    return { status: 'pass', message: 'No scroll lock detected', violations: [] };
  }

  var hasContainedAppShell =
    !!document.querySelector('.dw-app-viewport-frame') &&
    !!document.querySelector('[data-portal-scroll-root]');

  if (hasContainedAppShell) {
    return { status: 'pass', message: 'Scroll lock present for contained app shell viewport', violations: [] };
  }

  // Check for visible modal/dialog/overlay that would justify the scroll lock
  var modalSelectors = [
    '[role="dialog"]',
    '[role="alertdialog"]',
    '[aria-modal="true"]',
    '.modal.show',
    'dialog[open]',
  ];

  var hasVisibleModal = modalSelectors.some(function (sel) {
    try {
      var els = document.querySelectorAll(sel);
      for (var i = 0; i < els.length; i++) {
        var el = els[i];
        var style = window.getComputedStyle(el);
        if (style.display !== 'none' && style.visibility !== 'hidden' && parseFloat(style.opacity) > 0) {
          return true;
        }
      }
    } catch (e) {}
    return false;
  });

  if (hasVisibleModal) {
    return { status: 'pass', message: 'Scroll lock present but a visible modal/dialog justifies it', violations: [] };
  }

  var violations = [];

  if (bodyLocked) {
    violations.push({
      selector: 'body',
      role: 'body',
      name: '',
      testid: '',
      actual: { overflow: bodyStyle.overflow, overflowY: bodyStyle.overflowY },
      expected: { overflow: 'auto or visible (no modal present)' },
      fix: 'Remove overflow:hidden / overflow:clip from <body> when no modal is open',
    });
  }

  if (htmlLocked) {
    violations.push({
      selector: 'html',
      role: 'document',
      name: '',
      testid: '',
      actual: { overflow: htmlStyle.overflow, overflowY: htmlStyle.overflowY },
      expected: { overflow: 'auto or visible (no modal present)' },
      fix: 'Remove overflow:hidden / overflow:clip from <html> when no modal is open',
    });
  }

  return {
    status: 'fail',
    message: 'Scroll lock detected on ' + violations.map(function (v) { return v.selector; }).join(', ') + ' with no visible modal',
    violations: violations,
  };
})()
