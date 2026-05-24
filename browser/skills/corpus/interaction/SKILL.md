---
name: interaction
description: Generic browser interaction techniques for dw-browser — shadow DOM, iframes, scrolling, anti-bot, cookie consent
dependencies:
  - dw-browser
actions: [shadow-dom, iframe, cross-origin-iframe, virtual-scroll, cookie-consent, anti-bot]
---

# Browser Interaction Techniques

通用交互技能，适用于所有站点。遇到特定交互难题时参考。

## shadow-dom
intent: 穿透 Shadow DOM 操作内部元素

**策略 1 — 坐标点击 (推荐)**:
```dw-browser
clickat {host-element} 0.5 0.5
```
- compositor 层不受 shadow boundary 限制
- 需先通过 snap 或 screenshot 确定目标位置

**策略 2 — JS 穿透**:
```dw-browser
eval "document.querySelector('{host}').shadowRoot.querySelector('{target}').click()"
```
- 需要知道 host element 和 shadow 内部 selector
- 只适用于 open shadow root

## iframe
intent: 操作同源 iframe 内元素

```dw-browser
eval "document.querySelector('iframe').contentDocument.querySelector('{selector}').click()"
```

- 同源 iframe 可通过 contentDocument 直接访问
- 跨域 iframe 需用 cross-origin-iframe 策略

## cross-origin-iframe
intent: 操作跨域 iframe 内元素

**策略 — 坐标点击**:
```dw-browser
clickat iframe 0.5 0.5
```

- Input.dispatchMouseEvent 走 compositor，穿透跨域 iframe
- 无需额外 DOM 访问
- 通过 screenshot 确定 iframe 内目标坐标

## virtual-scroll
intent: 处理虚拟列表/无限滚动

```dw-browser
scroll down
wait "text='{target}'" --timeout 5
```

- 虚拟列表只渲染可见区域的 DOM 节点
- 需要滚动到目标位置才能看到/操作目标元素
- scrollIntoView 对虚拟列表无效 — 需用 scroll 命令逐步滚动
- 某些列表有自己的 scroll container，需确认滚动目标

## cookie-consent
intent: 处理 GDPR/Cookie 同意弹窗

```dw-browser
click button:'Accept'
```

- 常见变体: "Accept all", "Accept cookies", "I agree", "Got it"
- 部分站点使用 shadow-dom overlay — 用 clickat 穿透
- 某些站点 banner 出现有延迟 — snap 前先 wait 1-2s

## anti-bot
intent: 绕过 anti-bot 检测的通用策略

**原则**: compositor 层交互比 DOM 交互更难被检测

- 优先 `clickat` (Input.dispatchMouseEvent) 而非 `click` (可能降级为 JS el.click())
- 操作间加随机间隔 (Human jitter 已内置于 InputGateway)
- 使用 fingerprint presets (dw-browser --device / --preset) 模拟真实设备
- 避免: document.querySelector().click() / element.dispatchEvent() 等纯 JS 交互
- 验证: 使用 screenshot 确认操作生效，不依赖 DOM 状态推断
