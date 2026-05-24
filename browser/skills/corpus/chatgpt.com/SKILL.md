---
name: chatgpt.com
description: ChatGPT web automation — send messages, manage conversations, switch models
domain: chatgpt.com
dependencies:
  - dw-browser
actions: [send-message, wait-response, new-conversation, switch-model]
verified_at: 2026-04-27
---

# chatgpt.com

## Site
- React SPA, contenteditable block editor
- Anti-bot: active — prefer clickat over click, real mouse dispatch
- Auth: requires login (Google/Apple/Email), check snap for "New chat" sidebar
- ProviderDriver 已验证: browser-level trusted mouse + send-accepted retry

## send-message
intent: 发送消息

```dw-browser
fill #prompt-textarea '{message}'
press Enter
wait "not [aria-busy]" --timeout 30
```

- input 是 contenteditable div，不是 textarea
- press Enter 比 click send 按钮更可靠 (anti-bot)
- thinking block 是临时元素，snap 中出现但很快消失

## wait-response
intent: 等待 AI 回复完成

```dw-browser
wait "not [aria-busy]" --timeout 60
```

- response 增量出现 (streaming)，aria-busy 消失 = 完成
- thinking/placeholder divs 是临时的，忽略

## new-conversation
intent: 新建对话

```dw-browser
click button:'New chat'
```

- sidebar 中的按钮，不是 top bar

## switch-model
intent: 切换 AI 模型

```dw-browser
click button:'ChatGPT 4o'
click menuitem:'{model}'
```

- model 名在 dropdown menu 中，需先点击当前 model 按钮打开
- 可用 model 取决于账号 tier (Free/Plus/Team)
