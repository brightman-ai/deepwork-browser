(function () {
  if (typeof window.axe === 'undefined') {
    return {
      status: 'error',
      message: 'axe-core not available — inject it first via: dw-browser audit --suite a11y --inject-axe',
      violations: [],
    };
  }

  // axe.run 是异步的，但 WebDriver executeScript 不支持 Promise 返回。
  // 此脚本在 axe-core 已注入的前提下同步返回元数据，实际 axe.run 结果
  // 通过 executeAsyncScript 由调用方获取。
  return {
    status: 'pass',
    message: 'axe-core ' + (window.axe.version || 'unknown') + ' is available — call executeAsyncScript to run axe.run()',
    violations: [],
    meta: { axeVersion: window.axe.version || null },
  };
})()
