package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func parseResourceRequest(args []string) (browser.ResourceRequest, string, error) {
	options, positional, err := parseDeclaredArguments(args, "fetch")
	if err != nil {
		return browser.ResourceRequest{}, "", err
	}
	request := browser.ResourceRequest{URL: positional[0], NetworkRoute: browser.NetworkRouteSystem}
	request.DNSResolver, err = browser.ValidateDNSResolver(options["--dns-resolver"])
	if err != nil {
		return request, "", fmt.Errorf("--dns-resolver: %w", err)
	}
	if _, err = browser.ValidatePublicURL(request.URL, nil); err != nil {
		return request, "", err
	}
	if raw, ok := options["--network-route"]; ok {
		request.NetworkRoute = browser.NetworkRoute(raw)
	}
	if raw, ok := options["--timeout"]; ok {
		request.Timeout, err = time.ParseDuration(raw)
		if err != nil || request.Timeout <= 0 {
			return request, "", fmt.Errorf("--timeout requires a positive duration")
		}
	}
	for _, field := range []struct {
		name string
		dest *int64
	}{{"--max-bytes", &request.MaxBytes}, {"--max-network-bytes", &request.MaxNetworkBytes}} {
		if raw, ok := options[field.name]; ok {
			*field.dest, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || *field.dest <= 0 {
				return request, "", fmt.Errorf("%s requires a positive byte count", field.name)
			}
		}
	}
	return request, options["--save"], nil
}

// Canonical paths catch relative and directory-symlink aliases, while SameFile
// additionally rejects existing hard links. No output is opened through a link.
func outputPathsAlias(a, b string) (bool, error) {
	canonical := func(path string) (string, error) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err == nil {
			return resolved, nil
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
		if err != nil {
			return "", err
		}
		return filepath.Join(parent, filepath.Base(abs)), nil
	}
	ca, err := canonical(a)
	if err != nil {
		return false, err
	}
	cb, err := canonical(b)
	if err != nil {
		return false, err
	}
	if ca == cb {
		return true, nil
	}
	sa, ea := os.Stat(a)
	sb, eb := os.Stat(b)
	if ea == nil && eb == nil && os.SameFile(sa, sb) {
		return true, nil
	}
	// Nonexistent names may alias on case-insensitive or normalization-aware
	// filesystems. Ask that filesystem inside an owned private directory,
	// without ever publishing a probe at either requested output path.
	pa, err := os.Stat(filepath.Dir(ca))
	if err != nil {
		return false, err
	}
	pb, err := os.Stat(filepath.Dir(cb))
	if err != nil {
		return false, err
	}
	if !os.SameFile(pa, pb) {
		return false, nil
	}
	probeDir, err := os.MkdirTemp(filepath.Dir(ca), ".dw-output-alias-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(probeDir)
	probePath := filepath.Join(probeDir, filepath.Base(ca))
	file, err := os.OpenFile(probePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return false, err
	}
	if err = file.Close(); err != nil {
		return false, err
	}
	one, err := os.Stat(probePath)
	if err != nil {
		return false, err
	}
	other, err := os.Stat(filepath.Join(probeDir, filepath.Base(cb)))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(one, other), nil
}

func prepareResourceFile(path string) (*os.File, error) {
	if path == "" {
		return nil, fmt.Errorf("--save requires a nonempty file path")
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return nil, fmt.Errorf("--save destination is a directory; choose a file in an existing writable parent directory")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect --save destination: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".dw-fetch-*")
	if err != nil {
		return nil, fmt.Errorf("--save parent directory must already exist and be writable; choose a valid file path: %w", err)
	}
	return file, nil
}

func validateFetchOutputs(args []string, metadata string) error {
	_, path, err := parseResourceRequest(args)
	if err != nil {
		return err
	}
	if metadata != "" {
		if info, err := os.Stat(metadata); err == nil && info.IsDir() {
			return fmt.Errorf("--output destination is a directory; choose a JSON metadata file")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect --output metadata destination: %w", err)
		}
		same, err := outputPathsAlias(path, metadata)
		if err != nil {
			return fmt.Errorf("validate fetch output paths: %w", err)
		}
		if same {
			return fmt.Errorf("--save body file and --output JSON metadata must use different paths and must not alias")
		}
	}
	probe, err := prepareResourceFile(path)
	if err != nil {
		return err
	}
	name := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(name)
	return errors.Join(closeErr, removeErr)
}

func runFetchResult(args []string) (CommandResult, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runFetchWith(ctx, args, browser.FetchResource)
}

// This adapter publishes only settled kernel results. The dependency parameter
// lets artifact tests exercise disk failures without replacing network tests.
func runFetchWith(ctx context.Context, args []string, acquire func(context.Context, browser.ResourceRequest) (*browser.Resource, error)) (CommandResult, error) {
	request, path, err := parseResourceRequest(args)
	if err != nil {
		return CommandResult{}, commandError("validation", exitRunErr, err)
	}
	file, err := prepareResourceFile(path)
	if err != nil {
		return CommandResult{}, commandError("validation", exitRunErr, err)
	}
	defer os.Remove(file.Name())
	defer file.Close()
	resource, err := acquire(ctx, request)
	if err != nil {
		return resourceFailureResult(err)
	}
	if resource == nil || !resource.Cleanup.NetworkClosed {
		return resourceFailureResult(&browser.ResourceAcquisitionError{Diagnostic: browser.ResourceFailureDiagnostic{Schema: browser.ResourceFailureSchema, RequestedURL: request.URL, Stage: "cleanup", Reason: "cleanup_failed"}, Cause: fmt.Errorf("resource acquisition did not settle network cleanup")})
	}
	persist := func() error {
		if _, err := file.Write(resource.Body); err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return os.Rename(file.Name(), path)
	}
	if err = persist(); err != nil {
		return resourceFailureResult(&browser.ResourceAcquisitionError{Diagnostic: browser.ResourceFailureDiagnostic{Schema: browser.ResourceFailureSchema, RequestedURL: resource.RequestedURL, URL: resource.URL, Stage: "persist", Reason: "persist_failed", ContentType: resource.ContentType, StatusCode: resource.StatusCode, BodyBytesRead: resource.Bytes, Policy: resource.Policy, Cleanup: resource.Cleanup}, Cause: fmt.Errorf("save complete resource body: %w", err)})
	}
	resource.ToolVersion = version
	result := commandSuccess(struct {
		*browser.Resource
		BodyFile string `json:"body_file"`
	}{resource, path})
	result.DeliveryFailurePayload = resourceDeliveryFailurePayload(browser.ResourceFailureDiagnostic{Schema: browser.ResourceFailureSchema, ToolVersion: version, RequestedURL: resource.RequestedURL, URL: resource.URL, ContentType: resource.ContentType, StatusCode: resource.StatusCode, BodyBytesRead: resource.Bytes, Policy: resource.Policy, Cleanup: resource.Cleanup})
	return result, nil
}

func resourceFailureResult(err error) (CommandResult, error) {
	result := CommandResult{}
	var acquisition *browser.ResourceAcquisitionError
	if errors.As(err, &acquisition) {
		diagnostic := acquisition.Diagnostic
		diagnostic.ToolVersion = version
		result.Payload = diagnostic
		result.DeliveryFailurePayload = resourceDeliveryFailurePayload(diagnostic)
	}
	return result, &CommandError{Phase: "fetch", DispatchState: "unknown", Code: exitRunErr, Cause: err, Recovery: "Inspect the failure diagnostic and retry explicitly; no body file from this attempt is usable"}
}

// The body rename is the commit point. A later JSON delivery failure cannot
// roll back two files or a pipe; preserve accounting without advertising body_file.
func resourceDeliveryFailurePayload(diagnostic browser.ResourceFailureDiagnostic) func(error) any {
	return func(error) any {
		diagnostic.Usable = false
		diagnostic.Stage = "delivery"
		diagnostic.Reason = "delivery_failed"
		return diagnostic
	}
}
