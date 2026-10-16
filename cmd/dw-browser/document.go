package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// A separate request type deliberately exposes no session, caller JavaScript,
// user profile or write-grant options. FetchDocument owns the entire read job.
func parseDocumentRequest(args []string) (browser.DocumentRequest, error) {
	options, positional, err := parseDeclaredArguments(args, "document")
	if err != nil {
		return browser.DocumentRequest{}, err
	}
	request := browser.DocumentRequest{URL: positional[0], NetworkRoute: browser.DocumentNetworkSystem, Mode: browser.ModeHeadless}
	if raw, ok := options["--mode"]; ok {
		request.Mode = browser.BrowserMode(raw)
		if request.Mode != browser.ModeHeadless && request.Mode != browser.ModeHeaded {
			return request, fmt.Errorf("--mode requires headless or headed")
		}
	}
	if raw, ok := options["--network-route"]; ok {
		request.NetworkRoute = browser.DocumentNetworkRoute(raw)
		if request.NetworkRoute != browser.DocumentNetworkSystem && request.NetworkRoute != browser.DocumentNetworkDirect {
			return request, fmt.Errorf("--network-route requires system or direct")
		}
	}
	if _, err := browser.ValidatePublicDocumentURL(request.URL, nil); err != nil {
		return request, err
	}
	if raw, ok := options["--timeout"]; ok {
		request.Timeout, err = time.ParseDuration(raw)
		if err != nil || request.Timeout <= 0 {
			return request, fmt.Errorf("--timeout requires a positive duration")
		}
	}
	for _, field := range []struct {
		name string
		dest *int64
	}{{"--max-html-bytes", &request.MaxHTMLBytes}, {"--max-network-bytes", &request.MaxNetworkBytes}} {
		if raw, ok := options[field.name]; ok {
			*field.dest, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || *field.dest <= 0 {
				return request, fmt.Errorf("%s requires a positive byte count", field.name)
			}
		}
	}
	return request, nil
}

func runDocumentResult(args []string) (CommandResult, error) {
	request, err := parseDocumentRequest(args)
	if err != nil {
		return CommandResult{}, commandError("validation", exitRunErr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	doc, err := browser.FetchDocument(ctx, request)
	if err != nil {
		return documentFailureResult(request, err)
	}
	doc.ToolVersion = version
	return commandSuccess(doc), nil
}

// Only a direct settled unsupported error authorizes transport fallback. Typed
// acquisition failures carry diagnostics without exposing a partial DOM.
func documentFailureResult(request browser.DocumentRequest, err error) (CommandResult, error) {
	result := CommandResult{}
	var acquisition *browser.DocumentAcquisitionError
	if errors.As(err, &acquisition) {
		diagnostic := acquisition.Diagnostic
		diagnostic.ToolVersion = version
		result.Payload = diagnostic
	}
	if unsupported, ok := err.(*browser.UnsupportedDocumentContentError); ok && unsupported.Reroutable {
		result.Payload = map[string]any{"schema": browser.DocumentSchema, "tool_version": version, "reason": "unsupported_content_type", "requested_url": request.URL, "url": unsupported.URL, "content_type": unsupported.ContentType, "policy": unsupported.Policy, "cleanup": unsupported.Cleanup, "rendering": unsupported.Rendering}
	}
	return result, &CommandError{Phase: "document", DispatchState: "unknown", Code: exitRunErr, Cause: err, Recovery: "Inspect the acquisition error; this job used no existing browser session"}
}
