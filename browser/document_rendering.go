package browser

import (
	"context"
	"errors"
	"fmt"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

func normalizeDocumentMode(mode BrowserMode) (BrowserMode, error) {
	if mode == "" {
		mode = ModeHeadless
	}
	if mode != ModeHeadless && mode != ModeHeaded {
		return mode, fmt.Errorf("document mode must be headless or headed, got %q", mode)
	}
	return mode, nil
}

func documentProfilePolicy(mode BrowserMode) string {
	if mode == ModeHeaded {
		return "ephemeral-headed-hidden"
	}
	return "ephemeral-headless"
}

func documentTargetSurface(mode BrowserMode) string {
	if mode == ModeHeaded {
		return "hidden-target"
	}
	return "headless-target"
}

// Do not use generated CreateTargetParams here: Chrome 155 rejects hidden
// targets when newWindow is present, including false. Omit that parameter and
// forTab entirely. There is deliberately no ordinary-target fallback.
type documentHiddenTargetParams struct {
	URL        string `json:"url"`
	Hidden     bool   `json:"hidden"`
	Background bool   `json:"background"`
}

// Connect without Run: chromedp.Run on a fresh remote context creates an
// ordinary page. NewBrowser uses the job context even during the initial dial.
// Seed the allocator root before NewContext, so the child inherits this live
// browser without allocating another connection or an ordinary target. Keep
// this browser-level session alive for the hidden target's entire lifetime.
func newDocumentHiddenBrowserContext(allocCtx context.Context, wsURL string) (context.Context, context.CancelFunc, error) {
	bootstrap, bootstrapCancel := context.WithCancel(allocCtx)
	b, err := chromedp.NewBrowser(bootstrap, wsURL, chromedp.WithBrowserErrorf(chromedpErrorf))
	if err != nil {
		bootstrapCancel()
		return nil, nil, fmt.Errorf("document hidden browser connection: %w", err)
	}
	chromedp.FromContext(allocCtx).Browser = b
	var result struct {
		ID target.ID `json:"targetId"`
	}
	params := documentHiddenTargetParams{URL: "about:blank", Hidden: true, Background: true}
	err = b.Execute(cdp.WithExecutor(bootstrap, b), target.CommandCreateTarget, &params, &result)
	if err != nil || result.ID == "" {
		bootstrapCancel()
		if err == nil {
			err = errors.New("empty hidden target identity")
		}
		return nil, nil, fmt.Errorf("document hidden target unavailable (no window fallback): %w", err)
	}
	ctx, cancel := newDocumentHiddenTargetContext(bootstrap, result.ID)
	return ctx, func() { cancel(); bootstrapCancel() }, nil
}

func newDocumentHiddenTargetContext(bootstrap context.Context, id target.ID) (context.Context, context.CancelFunc) {
	// The browser already exists. WithErrorf is a browser allocation option
	// and chromedp rejects it here; NewBrowser configured the error logger.
	return chromedp.NewContext(bootstrap, chromedp.WithTargetID(id))
}
