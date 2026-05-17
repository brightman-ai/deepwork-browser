# Getting Started

## Requirements

- Go 1.26+
- Chrome or Chromium installed (detected automatically)

## Installation

```bash
go get github.com/brightman-ai/deepwork-browser
```

## Embed in your application

```go
package main

import (
    "net/http"
    browser "github.com/brightman-ai/deepwork-browser"
)

func main() {
    cfg := browser.DefaultConfig()
    srv := browser.New(cfg)
    http.Handle("/browser/", srv.Handler())
    http.ListenAndServe(":8090", nil)
}
```

## Run as CLI

```bash
go install github.com/brightman-ai/deepwork-browser/cmd/dw-browser@latest

# Open a URL in managed Chrome
dw-browser open https://example.com

# Take a full-page screenshot
dw-browser snap --out page.png

# Execute a script
dw-browser act --script ./my-script.js

# List open tabs
dw-browser tabs
```

## Build from source

```bash
git clone https://github.com/brightman-ai/deepwork-browser
cd deepwork-browser
go build ./cmd/dw-browser
./dw-browser --help
```
