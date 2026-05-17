# deepwork-browser

A browser automation engine built on Chrome DevTools Protocol (CDP). Provides a managed Chrome pool, session lifecycle, LiveView HTTP streaming, and a CLI for scripted browser tasks.

## Features

- Chrome pool with configurable concurrency
- Session-based tab management with target tracking
- LiveView: real-time DOM/screenshot streaming over HTTP
- Snapshot: full-page capture (HTML + screenshot)
- Action execution: click, type, scroll, evaluate
- SQLite-backed session persistence
- CLI tool (`dw-browser`) for scripted automation

## Quick Start

```bash
go get github.com/brightman-ai/deepwork-browser
```

Or run the CLI:

```bash
go run ./cmd/dw-browser --help
```

```bash
# Open a URL and take a screenshot
dw-browser open https://example.com
dw-browser snap --out screenshot.png
```

Embed the HTTP server:

```go
import browser "github.com/brightman-ai/deepwork-browser"

srv := browser.New(browser.DefaultConfig())
http.Handle("/browser/", srv.Handler())
```

See [guide/](guide/) for full documentation.

## License

[MIT](LICENSE)
