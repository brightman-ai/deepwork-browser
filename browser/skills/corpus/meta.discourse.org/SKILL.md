---
name: meta.discourse.org
description: Discourse forum browsing and assisted reply actions
domain: meta.discourse.org
dependencies:
  - dw-browser
actions: [capture-thread, open-reply, quote-post, publish-reply]
verified_at: 2026-05-01
---

# Discourse

Protocol syntax migrated to v2; the prior verified_at date is historical site evidence, not current UI validation. Adapt visible names/selectors before accepting a baseline. Publishing/sending requires the user task to authorize that operation. Plans do not execute themselves.

## Site
- Server-rendered forum UI with progressive enhancement.
- Auth may be required for reply/publish actions.
- Standard Browser Sidebar mode must never silently submit a reply.

## capture-thread
intent: Capture current thread context as evidence

```sh
dw-browser observe --id NAME
```

- Fast mode is enough because this action only records visible browser context.
- Store source URL/title with the evidence record.

## open-reply
intent: Open the reply composer without submitting

```dw-browser-plan
{
  "steps": [
    {
      "action": {
        "kind": "click",
        "target": {
          "kind": "locator",
          "locator": "role=button[name=\"Reply\"]"
        }
      }
    },
    {
      "wait": {
        "assert": "exists(role='textbox')",
        "timeout_ms": 10000
      }
    }
  ]
}
```

- Opening the composer is reversible; publishing remains a separate confirmed action.
- If login is required, switch to webvisit with the explicit authorized --allow-host before retrying.

## quote-post
intent: Quote selected post text into a local draft

```dw-browser-plan
{
  "steps": [
    {
      "action": {
        "kind": "click",
        "target": {
          "kind": "locator",
          "locator": "role=button[name=\"Quote\"]"
        }
      }
    }
  ]
}
```

- Treat this as an assisted copy/insert step.
- Human confirmation is required before inserting into a remote composer.

## publish-reply
intent: Publish the prepared reply after Human confirmation

```dw-browser-plan
{
  "steps": [
    {
      "action": {
        "kind": "click",
        "target": {
          "kind": "locator",
          "locator": "role=button[name=\"Reply\"]"
        }
      }
    }
  ]
}
```

- Requires webvisit with the explicit authorized --allow-host and explicit Human confirmation.
- Browser Sidebar default origin-gated webvisit must block silent publish attempts.
