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

## Site
- Server-rendered forum UI with progressive enhancement.
- Auth may be required for reply/publish actions.
- Standard Browser Sidebar mode must never silently submit a reply.

## capture-thread
intent: Capture current thread context as evidence

```dw-browser
snap
```

- Fast mode is enough because this action only records visible browser context.
- Store source URL/title with the evidence record.

## open-reply
intent: Open the reply composer without submitting

```dw-browser
click button:'Reply'
wait "textarea, [contenteditable=true]" --timeout 10
```

- Opening the composer is reversible; publishing remains a separate confirmed action.
- If login is required, switch to Trusted mode before retrying.

## quote-post
intent: Quote selected post text into a local draft

```dw-browser
click button:'Quote'
```

- Treat this as an assisted copy/insert step.
- Human confirmation is required before inserting into a remote composer.

## publish-reply
intent: Publish the prepared reply after Human confirmation

```dw-browser
click button:'Reply'
```

- Requires Trusted mode and explicit Human confirmation.
- Browser Sidebar Standard mode must block silent publish attempts.
