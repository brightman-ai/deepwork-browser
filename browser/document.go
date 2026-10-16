package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"time"

	"github.com/chromedp/cdproto/page"
	cdpruntime "github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

const DocumentSchema = "dw.document.v1"

// DocumentRequest is a public URL read, never a script or a session attachment.
// Zero budgets select the defaults. Timeout covers queueing, startup and reading.
type DocumentRequest struct {
	URL             string
	Timeout         time.Duration
	MaxHTMLBytes    int64
	MaxNetworkBytes int64
}

type DocumentReadiness struct {
	DocumentReadyState string `json:"document_ready_state"`
	DOMReady           bool   `json:"dom_ready"`
}

type DocumentPolicy struct {
	Network         string `json:"network"`
	Profile         string `json:"profile"`
	JavaScript      string `json:"javascript"`
	NetworkRequests int64  `json:"network_requests"`
	BlockedRequests int64  `json:"blocked_requests"`
	NetworkBytes    int64  `json:"network_bytes"`
	MaxHTMLBytes    int64  `json:"max_html_bytes"`
	MaxNetworkBytes int64  `json:"max_network_bytes"`
}

type DocumentCleanup struct {
	BrowserClosed  bool `json:"browser_closed"`
	ProfileRemoved bool `json:"profile_removed"`
	NetworkClosed  bool `json:"network_closed"`
}

// Document contains the main frame DOM, not an extraction or a screenshot.
// DeclaredCanonicalURL is an untrusted DOM hint, never a navigated/verified URL.
type Document struct {
	Schema               string            `json:"schema"`
	ToolVersion          string            `json:"tool_version,omitempty"`
	RequestedURL         string            `json:"requested_url"`
	URL                  string            `json:"url"`
	DeclaredCanonicalURL string            `json:"declared_canonical_url,omitempty"`
	ContentType          string            `json:"content_type"`
	Readiness            DocumentReadiness `json:"readiness"`
	Content              string            `json:"content"`
	Representation       string            `json:"representation"`
	Policy               DocumentPolicy    `json:"policy"`
	Cleanup              DocumentCleanup   `json:"cleanup"`
}

// UnsupportedDocumentContentError preserves actual response metadata without
// returning a browser-generated XML/PDF viewer as the original document.
type UnsupportedDocumentContentError struct {
	URL         string
	ContentType string
	Reroutable  bool
	Policy      DocumentPolicy
	Cleanup     DocumentCleanup
}

func (e *UnsupportedDocumentContentError) Error() string {
	return fmt.Sprintf("unsupported document content type %q: this endpoint returns HTML DOM", e.ContentType)
}

func documentIsHTML(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && (mediaType == "text/html" || mediaType == "application/xhtml+xml")
}

func normalizeDocumentRequest(r DocumentRequest) (DocumentRequest, error) {
	if r.Timeout < 0 || r.MaxHTMLBytes < 0 || r.MaxNetworkBytes < 0 {
		return r, errors.New("document budgets must not be negative")
	}
	if r.Timeout == 0 {
		r.Timeout = 30 * time.Second
	}
	if r.MaxHTMLBytes == 0 {
		r.MaxHTMLBytes = 8 << 20
	}
	if r.MaxNetworkBytes == 0 {
		r.MaxNetworkBytes = 32 << 20
	}
	if _, err := ValidatePublicDocumentURL(r.URL, nil); err != nil {
		return r, fmt.Errorf("document URL: %w", err)
	}
	return r, nil
}

// FetchDocument is the single typed read kernel for CLI and Go consumers. Each
// call owns a fresh headless browser, a private proxy and a disposable profile.
// A non-nil error means no successful document, including cleanup/policy errors.
func FetchDocument(parent context.Context, request DocumentRequest) (doc *Document, resultErr error) {
	return fetchDocumentWithNetwork(parent, request, PublicNetworkOptions{})
}

// Private network seams let acceptance fixtures exercise real Chrome and proxy
// traffic. Production callers always enter through FetchDocument's fixed policy.
func fetchDocumentWithNetwork(parent context.Context, request DocumentRequest, networkOptions PublicNetworkOptions) (doc *Document, resultErr error) {
	r, err := normalizeDocumentRequest(request)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, r.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("document deadline: %w", err)
	}
	if err := documentRuntimeAvailable(); err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	slot, err := acquireDocumentSlot(ctx, filepath.Join(home, ".deepwork", "browser-document-slots"))
	if err != nil {
		return nil, err
	}
	cleanup := DocumentCleanup{}
	var policy DocumentPolicy
	defer func() {
		if err := slot.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("document slot cleanup: %w", err))
			doc = nil
		}
		doc, resultErr = settleDocumentReceipt(doc, resultErr, policy, cleanup)
	}()
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	profileID := "document-" + hex.EncodeToString(token[:])
	profilePath := filepath.Join(home, ".deepwork", "browser-cli", profileID)
	networkOptions.Timeout = r.Timeout
	networkOptions.MaxBytes = r.MaxNetworkBytes
	networkOptions.MaxRequests = 128
	networkOptions.AllowedPorts = nil
	guard, err := StartPublicNetworkGuard(ctx, networkOptions)
	if err != nil {
		return nil, err
	}
	// A global budget/control failure stops startup or an in-flight CDP read
	// immediately; a successful per-resource denial does not cancel this context.
	operationCtx, cancelOperation := context.WithCancel(ctx)
	stopNetworkCancel := context.AfterFunc(guard.ctx, cancelOperation)
	defer stopNetworkCancel()
	defer cancelOperation()
	var core BrowserCore
	owner := &documentLaunchOwner{}
	defer func() {
		// Cleanup has its own deadline: a timed-out read must still reap its browser.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		policyErr := errors.Join(guard.Err(), documentCorePolicyError(core))
		if closeErr := guard.CloseContext(cleanupCtx); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("document network cleanup: %w", closeErr))
		} else {
			cleanup.NetworkClosed = true
		}
		if core != nil {
			closeErr := core.Close(cleanupCtx)
			cleanup.BrowserClosed = closeErr == nil
			if closeErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("document browser cleanup: %w", closeErr))
			}
		} else if owner.pid.Load() > 0 {
			if err := owner.cleanup(cleanupCtx); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("document failed startup cleanup: %w", err))
			} else {
				cleanup.BrowserClosed = true
			}
		} else {
			// No child was started; only this job's random profile may exist.
			cleanup.BrowserClosed = true
		}
		if finalPolicyErr := guard.PolicyError(); finalPolicyErr != nil {
			policyErr = finalPolicyErr
		}
		if policyErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("document network policy: %w", policyErr))
		}
		if cleanup.BrowserClosed {
			if removeErr := os.RemoveAll(profilePath); removeErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("document profile cleanup: %w", removeErr))
			} else {
				cleanup.ProfileRemoved = true
			}
		}
		stats := guard.Stats()
		policy = DocumentPolicy{Network: "page-requests-public-only", Profile: "ephemeral-headless", JavaScript: "fixed-isolated-dom-read", MaxHTMLBytes: r.MaxHTMLBytes, MaxNetworkBytes: r.MaxNetworkBytes, NetworkRequests: stats.Requests, NetworkBytes: stats.Bytes, BlockedRequests: stats.DeniedRequests + documentCoreBlockedRequests(core)}
	}()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("document deadline: %w", err)
	}
	core, err = NewBrowserCore(operationCtx, profileID, WithMode(ModeHeadless), withDocumentNetwork(guard.ProxyURL(), owner))
	if err != nil {
		return nil, fmt.Errorf("document browser startup: %w", err)
	}
	provider, ok := core.(CDPContextProvider)
	if !ok {
		return nil, errors.New("document browser has no CDP context")
	}
	runCtx, cancelRun := deriveTargetContext(operationCtx, provider.CDPContext())
	defer cancelRun()
	var finalURL string
	var payload documentDOMPayload
	err = chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
		_, _, text, download, err := page.Navigate(r.URL).Do(c)
		if err != nil {
			return err
		}
		if download {
			return errors.New("document navigation attempted a download")
		}
		if text != "" {
			return fmt.Errorf("document navigation: %s", text)
		}
		return nil
	}))
	if err != nil {
		return nil, documentReadError(ctx, guard, err)
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = chromedp.Run(runCtx, chromedp.ActionFunc(func(c context.Context) error {
			tree, err := page.GetFrameTree().Do(c)
			if err != nil {
				return err
			}
			if tree == nil || tree.Frame == nil {
				return errors.New("document main frame unavailable")
			}
			before := tree.Frame
			world, err := page.CreateIsolatedWorld(before.ID).WithWorldName("dw.document.v1.reader").Do(c)
			if err != nil {
				return err
			}
			value, exception, err := cdpruntime.Evaluate(documentDOMExpression(r.MaxHTMLBytes)).WithContextID(world).WithReturnByValue(true).WithSilent(true).Do(c)
			if err != nil {
				return err
			}
			if exception != nil {
				return errors.New("document isolated DOM read failed")
			}
			if value == nil {
				return errors.New("document DOM result unavailable")
			}
			if err := json.Unmarshal(value.Value, &payload); err != nil {
				return err
			}
			after, err := page.GetFrameTree().Do(c)
			if err != nil {
				return err
			}
			if after == nil || after.Frame == nil || after.Frame.ID != before.ID || after.Frame.LoaderID != before.LoaderID || after.Frame.URL != before.URL || after.Frame.URLFragment != before.URLFragment {
				return errors.New("document changed during DOM read")
			}
			finalURL = after.Frame.URL + after.Frame.URLFragment
			return nil
		}))
		if err != nil {
			return nil, documentReadError(ctx, guard, err)
		}
		if payload.ReadyState == "interactive" || payload.ReadyState == "complete" {
			break
		}
		select {
		case <-ctx.Done():
			return nil, documentReadError(ctx, guard, ctx.Err())
		case <-ticker.C:
		}
	}
	if payload.TooLarge || int64(len(payload.HTML)) > r.MaxHTMLBytes {
		return nil, errors.New("document HTML byte budget exceeded")
	}
	if _, err := ValidatePublicDocumentURL(finalURL, nil); err != nil {
		return nil, fmt.Errorf("document final URL: %w", err)
	}
	if err := errors.Join(guard.Err(), documentCorePolicyError(core)); err != nil {
		return nil, fmt.Errorf("document network policy: %w", err)
	}
	contentType := documentCoreContentType(core)
	if contentType == "" {
		contentType = payload.ContentType
	}
	if !documentIsHTML(contentType) {
		return nil, &UnsupportedDocumentContentError{URL: finalURL, ContentType: contentType}
	}
	if payload.ContentType == "" || payload.HTML == "" {
		return nil, errors.New("document DOM content unavailable")
	}
	return &Document{Schema: DocumentSchema, RequestedURL: r.URL, URL: finalURL, DeclaredCanonicalURL: payload.Canonical, ContentType: contentType, Readiness: DocumentReadiness{DocumentReadyState: payload.ReadyState, DOMReady: true}, Content: payload.HTML, Representation: "dom-html"}, nil
}

// Only a pure unsupported-format outcome with settled, successful cleanup may
// be rerouted. errors.As alone must not turn a joined enforcement/cleanup error
// into permission to consume a feed through another transport.
func settleDocumentReceipt(doc *Document, err error, policy DocumentPolicy, cleanup DocumentCleanup) (*Document, error) {
	complete := cleanup.BrowserClosed && cleanup.ProfileRemoved && cleanup.NetworkClosed
	if unsupported, ok := err.(*UnsupportedDocumentContentError); ok && complete {
		unsupported.Reroutable = true
		unsupported.Policy = policy
		unsupported.Cleanup = cleanup
	}
	if err != nil {
		return nil, err
	}
	if !complete {
		return nil, errors.New("document cleanup incomplete")
	}
	if doc != nil {
		doc.Policy = policy
		doc.Cleanup = cleanup
	}
	return doc, nil
}

func documentReadError(ctx context.Context, guard *PublicNetworkGuard, err error) error {
	if policyErr := guard.Err(); policyErr != nil {
		return fmt.Errorf("document network policy: %w", policyErr)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("document deadline: %w", ctx.Err())
	}
	return fmt.Errorf("document read: %w", err)
}

type documentDOMPayload struct {
	ReadyState  string `json:"ready_state"`
	ContentType string `json:"content_type"`
	HTML        string `json:"html"`
	Canonical   string `json:"canonical"`
	TooLarge    bool   `json:"too_large"`
}

// Only a validated integer budget is interpolated. No caller expression, URL or
// selector enters the isolated-world program. Site-world prototype overrides
// cannot replace its DOM getters or TextEncoder.
func documentDOMExpression(maxBytes int64) string {
	return fmt.Sprintf(`(() => {
 const state = document.readyState;
 if (state !== "interactive" && state !== "complete") return {ready_state: state};
 const html = document.documentElement ? document.documentElement.outerHTML : "";
 if (new TextEncoder().encode(html).byteLength > %d) return {ready_state: state, too_large: true};
 const canonical = document.querySelector('link[rel~="canonical"]');
 return {ready_state: state, content_type: document.contentType, html: html, canonical: canonical ? canonical.href : ""};
})()`, maxBytes)
}

func acquireDocumentSlot(ctx context.Context, dir string) (exclusiveFileLock, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("document queue: %w", err)
		}
		for i := 0; i < 2; i++ {
			lock, ok, err := acquireExclusiveFileLock(filepath.Join(dir, fmt.Sprintf("slot-%d.lock", i)), 0)
			if err != nil {
				return nil, err
			}
			if ok {
				return lock, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("document queue: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
