---
name: claude.ai
description: Claude.ai web automation — send messages, manage conversations
domain: claude.ai
dependencies:
  - dw-browser
actions: [send-message, wait-response, new-conversation]
verified_at: 2026-04-27
---

# claude.ai

## Site
- React SPA, contenteditable input
- Anti-bot: active — prefer clickat, real mouse dispatch
- Auth: requires login, check snap for conversation sidebar

## send-message
intent: 发送消息

```dw-browser
fill textbox:'Reply to Claude...' '{message}'
press Enter
wait "not [aria-busy]" --timeout 60
```

- input 是 contenteditable div (类似 chatgpt)
- press Enter 发送，比 click Send 按钮更可靠

## wait-response
intent: 等待回复

```dw-browser
wait "not [aria-busy]" --timeout 120
```

- Claude 回复可能较长，timeout 建议 120s
- streaming 完成后 aria-busy 消失

## new-conversation
intent: 新建对话

```dw-browser
click button:'New chat'
```
