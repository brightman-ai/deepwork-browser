package main

import (
	"context"
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
	request := browser.DocumentRequest{URL: positional[0]}
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
		result := CommandResult{}
		if unsupported, ok := err.(*browser.UnsupportedDocumentContentError); ok && unsupported.Reroutable {
			result.Payload = map[string]any{"schema": browser.DocumentSchema, "tool_version": version, "reason": "unsupported_content_type", "requested_url": request.URL, "url": unsupported.URL, "content_type": unsupported.ContentType, "policy": unsupported.Policy, "cleanup": unsupported.Cleanup}
		}
		return result, &CommandError{Phase: "document", DispatchState: "unknown", Code: exitRunErr, Cause: err, Recovery: "Inspect the acquisition error; this job used no existing browser session"}
	}
	doc.ToolVersion = version
	return commandSuccess(doc), nil
}
