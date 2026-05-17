# Architecture

## Overview

```
┌─────────────────────────────────────────┐
│             dw-browser CLI              │
│        cmd/dw-browser/main.go           │
└──────────────┬──────────────────────────┘
               │
┌──────────────▼──────────────────────────┐
│             HTTP Server                 │
│             server.go                   │
│   Handler() mounts all API routes       │
└──────────────┬──────────────────────────┘
               │
┌──────────────▼──────────────────────────┐
│           browser/ package              │
│                                         │
│  BrowserPool   — Chrome process pool    │
│  Session       — per-tab lifecycle      │
│  TargetTracker — CDP target events      │
│  LiveView      — DOM/screenshot stream  │
│  Snapshot      — full-page capture      │
│  InputGateway  — click/type/scroll      │
└──────────────┬──────────────────────────┘
               │
┌──────────────▼──────────────────────────┐
│        chromedp / CDP                   │
│        Chrome DevTools Protocol         │
└─────────────────────────────────────────┘
```

## Key Components

### BrowserPool
Manages a pool of Chrome processes. Limits concurrent browser instances. Handles process crash recovery.

### Session
Represents a single browser tab. Owns the CDP connection lifecycle. Provides `Navigate`, `Snapshot`, `LiveView`, `Execute` methods.

### LiveView
Streams real-time page state (DOM mutations + periodic screenshots) over HTTP using SSE or WebSocket. Used for monitoring automation progress without a local display.

### Snapshot
Captures the current page as a structured object: full HTML, a PNG screenshot, and extracted text. Useful for archiving and analysis.

### InputGateway
Translates high-level actions (`Click`, `Type`, `Scroll`, `KeyPress`) into CDP commands. Includes coordinate mapping for element-relative actions.

## Data Persistence

Session metadata is stored in SQLite (`~/.dw/browser.db` by default). This allows sessions to survive process restarts and enables multi-process coordination.
