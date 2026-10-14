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

Protocol syntax migrated to v2; the prior verified_at date is historical site evidence, not current UI validation. Adapt visible names/selectors before accepting a baseline. Publishing/sending requires the user task to authorize that operation. Plans do not execute themselves.

## Site
- React SPA, contenteditable block editor
- Anti-bot: active — use verified typed physical input
- Auth: requires login (Google/Apple/Email), inspect observe for "New chat" sidebar
- ProviderDriver 已验证: browser-level trusted mouse + send-accepted retry

## send-message
intent: 发送消息

```dw-browser-plan
{
  "steps": [
    {
      "action": {
        "kind": "fill",
        "text": "{message}",
        "target": {
          "kind": "locator",
          "locator": "css=#prompt-textarea"
        }
      }
    },
    {
      "action": {
        "kind": "press",
        "key": "Enter"
      }
    },
    {
      "wait": {
        "assert": "gone(role='button', name='Stop generating')",
        "timeout_ms": 60000
      }
    }
  ]
}
```

- input 是 contenteditable div，不是 textarea
- press Enter 比 click send 按钮更可靠 (anti-bot)
- thinking block 是临时元素，observe 中出现但很快消失

## wait-response
intent: 等待 AI 回复完成

```dw-browser-plan
{
  "steps": [
    {
      "wait": {
        "assert": "gone(role='button', name='Stop generating')",
        "timeout_ms": 60000
      }
    }
  ]
}
```

- response 增量出现 (streaming)，aria-busy 消失 = 完成
- thinking/placeholder divs 是临时的，忽略

## new-conversation
intent: 新建对话

```dw-browser-plan
{
  "steps": [
    {
      "action": {
        "kind": "click",
        "target": {
          "kind": "locator",
          "locator": "role=button[name=\"New chat\"]"
        }
      }
    }
  ]
}
```

- sidebar 中的按钮，不是 top bar

## switch-model
intent: 切换 AI 模型

```dw-browser-plan
{
  "steps": [
    {
      "action": {
        "kind": "click",
        "target": {
          "kind": "locator",
          "locator": "role=button[name=\"ChatGPT\"]"
        }
      }
    },
    {
      "action": {
        "kind": "click",
        "target": {
          "kind": "locator",
          "locator": "role=menuitem[name=\"{model}\"]"
        }
      }
    }
  ]
}
```

- model 名在 dropdown menu 中，需先点击当前 model 按钮打开
- 可用 model 取决于账号 tier (Free/Plus/Team)

For an actual aria-busy marker, preserve selector waiting through an explicit journey probe, recollected by the shared wait kernel:

```yaml
- id: await-stream
  kind: wait
  probes:
    - id: busy
      kind: css
      selector: '[aria-busy="true"]'
  wait:
    assert: "probe_count(id='busy') == 0"
    timeout_ms: 120000
```

An absent/unavailable probe is BLOCKED, not a zero count. Confirm the site's actual completion marker; delay alone does not prove completion.
