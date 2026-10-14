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

Protocol syntax migrated to v2; the prior verified_at date is historical site evidence, not current UI validation. Adapt visible names/selectors before accepting a baseline. Publishing/sending requires the user task to authorize that operation. Plans do not execute themselves.

## Site
- React SPA, contenteditable input
- Anti-bot: active — use verified typed physical input
- Auth: requires login, inspect observe for conversation sidebar

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
          "locator": "css=[contenteditable=\"true\"]"
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
        "assert": "gone(role='button', name='Stop response')",
        "timeout_ms": 120000
      }
    }
  ]
}
```

- input 是 contenteditable div (类似 chatgpt)
- press Enter 发送，比 click Send 按钮更可靠

## wait-response
intent: 等待回复

```dw-browser-plan
{
  "steps": [
    {
      "wait": {
        "assert": "gone(role='button', name='Stop response')",
        "timeout_ms": 120000
      }
    }
  ]
}
```

- Claude 回复可能较长，timeout 建议 120s
- streaming 完成后 aria-busy 消失

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
