package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

type documentLaunchOwner struct {
	pid    atomic.Int64
	handle atomic.Pointer[chromeHandleImpl]
}

type documentRuntimePolicy struct {
	mu          sync.Mutex
	err         error
	blocked     int64
	contentType string
}

func (p *documentRuntimePolicy) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		p.err = err
	}
}
func (p *documentRuntimePolicy) Err() error { p.mu.Lock(); defer p.mu.Unlock(); return p.err }
func documentLaunchContext(ctx context.Context, proxy string) context.Context {
	if proxy == "" {
		return nil
	}
	return ctx
}

func prepareDocumentLaunch(profilePath, proxyURL string, debugPort, width, height int) ([]string, error) {
	u, err := url.Parse(proxyURL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("document requires a dedicated loopback HTTP proxy")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid document proxy port")
	}
	if err := os.Chmod(profilePath, 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(profilePath, "Default"), 0700); err != nil {
		return nil, err
	}
	prefs := map[string]interface{}{"webrtc": map[string]string{"ip_handling_policy": "disable_non_proxied_udp"}, "net": map[string]int{"network_prediction_options": 2}, "search": map[string]bool{"suggest_enabled": false}, "dns_prefetching": map[string]bool{"enabled": false}, "signin": map[string]bool{"allowed": false}, "default_search_provider": map[string]interface{}{"enabled": false}, "profile": map[string]interface{}{"default_content_setting_values": map[string]int{"popups": 2, "media_stream_mic": 2, "media_stream_camera": 2, "automatic_downloads": 2, "notifications": 2, "geolocation": 2, "clipboard": 2}}, "download": map[string]int{"restrictions": 3}}
	data, err := json.Marshal(prefs)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(profilePath, "Default", "Preferences"), data, 0600); err != nil {
		return nil, err
	}
	// This is a distinct policy in the shared launch path. In particular it does
	// not inherit legacy stealth, proxy environment, popup or sandbox flags.
	args := []string{
		"--remote-debugging-port=0", "--remote-debugging-address=127.0.0.1", "--user-data-dir=" + profilePath,
		"--headless=new", "--enable-automation", "--no-first-run", "--no-default-browser-check", "--disable-session-crashed-bubble",
		fmt.Sprintf("--window-size=%d,%d", width, height), "--proxy-server=" + proxyURL, "--proxy-bypass-list=<-loopback>",
		"--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1", "--disable-quic",
		"--disable-background-networking", "--disable-background-mode", "--disable-domain-reliability", "--disable-search-engine-choice-screen", "--disable-component-update", "--disable-default-apps", "--disable-sync", "--disable-extensions", "--disable-breakpad", "--disable-crash-reporter", "--noerrdialogs",
		"--mute-audio", "--autoplay-policy=user-gesture-required", "--block-new-web-contents", "--deny-permission-prompts",
		"--blink-settings=imagesEnabled=false", "--disable-features=MediaRouter,OptimizationHints,AutofillServerCommunication,CertificateTransparencyComponentUpdater",
		"--password-store=basic", "--use-mock-keychain", ChromeInitialPageURL,
	}
	return args, nil
}

func (impl *browserCoreImpl) configureDocumentRuntime() error {
	policy := &documentRuntimePolicy{}
	impl.documentRuntime = policy
	ctx := impl.browserCtx
	browserExec := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)
	installDocumentTargetGuards(ctx, policy)

	mainID := chromedp.FromContext(ctx).Target.TargetID
	chromedp.ListenBrowser(ctx, func(event interface{}) {
		if created, ok := event.(*target.EventTargetCreated); ok && created.TargetInfo != nil && created.TargetInfo.Type == "page" && created.TargetInfo.TargetID != mainID {
			id := created.TargetInfo.TargetID
			go func() {
				if err := target.CloseTarget(id).Do(browserExec); err != nil && ctx.Err() == nil && !documentTargetGoneError(err) {
					policy.fail(fmt.Errorf("document popup control: %w", err))
				}
			}()
		}
	})

	return chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		tree, err := page.GetFrameTree().Do(c)
		if err != nil {
			return err
		}
		if tree == nil || tree.Frame == nil {
			return errors.New("document main frame unavailable during network setup")
		}
		mainFrameID := tree.Frame.ID
		chromedp.ListenTarget(ctx, func(event interface{}) {
			response, ok := event.(*network.EventResponseReceived)
			if !ok || response.Type != network.ResourceTypeDocument || response.FrameID != mainFrameID || response.Response == nil {
				return
			}
			contentType := response.Response.MimeType
			for key, value := range response.Response.Headers {
				if strings.EqualFold(key, "Content-Type") {
					if header, ok := value.(string); ok {
						contentType = header
					}
				}
				if strings.HasPrefix(response.Response.URL, "http://") && strings.EqualFold(key, PublicNetworkErrorHeader) {
					policy.fail(errors.New("main document rejected by network guard"))
					return
				}
			}
			policy.mu.Lock()
			policy.contentType = contentType
			policy.mu.Unlock()
		})
		if err := cdpbrowser.SetDownloadBehavior(cdpbrowser.SetDownloadBehaviorBehaviorDeny).Do(browserExec); err != nil {
			return fmt.Errorf("document deny downloads: %w", err)
		}
		for _, name := range []string{"camera", "microphone", "geolocation", "notifications", "clipboard-read", "clipboard-write"} {
			if err := cdpbrowser.SetPermission(&cdpbrowser.PermissionDescriptor{Name: name}, cdpbrowser.PermissionSettingDenied).Do(browserExec); err != nil {
				return fmt.Errorf("document deny %s: %w", name, err)
			}
		}
		if err := network.Enable().Do(c); err != nil {
			return err
		}
		if err := fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*", RequestStage: fetch.RequestStageRequest}}).Do(c); err != nil {
			return err
		}
		if err := target.SetDiscoverTargets(true).Do(browserExec); err != nil {
			return err
		}
		return target.SetAutoAttach(false, false).WithFlatten(true).Do(c)
	}))
}

func documentCoreContentType(core BrowserCore) string {
	if impl, ok := core.(*browserCoreImpl); ok && impl.documentRuntime != nil {
		impl.documentRuntime.mu.Lock()
		defer impl.documentRuntime.mu.Unlock()
		return impl.documentRuntime.contentType
	}
	return ""
}

func (impl *browserCoreImpl) closeDocumentRuntime(ctx context.Context) error {
	impl.mu.Lock()
	defer impl.mu.Unlock()
	var err error
	if impl.chromeHandle != nil {
		handle := impl.chromeHandle
		if concrete, ok := handle.(*chromeHandleImpl); ok {
			err = concrete.killWithContext(ctx)
		} else {
			err = handle.Kill()
		}
		if err == nil {
			err = waitDocumentProcessGroup(ctx, impl.chromePID)
		}
		if err == nil {
			impl.chromeHandle = nil
		}
	}
	// Close the protocol context after its owned process group has exited.
	if impl.browserCancel != nil {
		impl.browserCancel()
		impl.browserCancel = nil
	}
	if impl.allocCancel != nil {
		impl.allocCancel()
		impl.allocCancel = nil
	}
	if err == nil {
		RemoveProfileOwnerMarker(impl.profilePath, impl.ownerIdentityKey)
	}
	return err
}

func documentCorePolicyError(core BrowserCore) error {
	if impl, ok := core.(*browserCoreImpl); ok && impl.documentRuntime != nil {
		return impl.documentRuntime.Err()
	}
	return nil
}

// A short interval is used only for reaping this call's own process group.
const documentReapPoll = 20 * time.Millisecond

func waitDocumentChromeReady(ctx context.Context, profilePath string, done <-chan struct{}) (string, error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		data, err := os.ReadFile(filepath.Join(profilePath, "DevToolsActivePort"))
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			// Chrome creates this file before its two lines are fully written.
			// A snapshot without a complete endpoint is pending, not a launch error.
			var port int
			complete := len(lines) == 2
			if complete {
				var parseErr error
				port, parseErr = strconv.Atoi(lines[0])
				complete = parseErr == nil && port > 0 && port <= 65535 && strings.HasPrefix(lines[1], "/devtools/browser/") && len(lines[1]) > len("/devtools/browser/")
			}
			if complete {
				endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
				req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
				if err != nil {
					return "", err
				}
				resp, err := client.Do(req)
				if err == nil {
					var version cdpVersionResponse
					decodeErr := json.NewDecoder(resp.Body).Decode(&version)
					resp.Body.Close()
					expected := fmt.Sprintf("ws://127.0.0.1:%d%s", port, lines[1])
					if decodeErr == nil && version.WebSocketDebuggerURL == expected {
						return expected, nil
					}
					if decodeErr == nil {
						return "", errors.New("document owned CDP identity mismatch")
					}
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-done:
			return "", errors.New("document Chrome exited before CDP ready")
		case <-ticker.C:
		}
	}
}

func documentDebugPort(port int, proxy string) int {
	if proxy != "" {
		return 0
	}
	return port
}

func documentChromeExitDiagnostics(handle ChromeHandle) string {
	if h, ok := handle.(*chromeHandleImpl); ok {
		select {
		case <-h.doneCh:
			if h.stderr != nil {
				s := h.stderr.String()
				if len(s) > 3000 {
					s = s[len(s)-3000:]
				}
				return s
			}
		default:
		}
	}
	return "Chrome did not provide exit diagnostics"
}

func installDocumentTargetGuards(ctx context.Context, policy *documentRuntimePolicy) {
	// Fetch listeners never run a synchronous command in chromedp's event loop.
	chromedp.ListenTarget(ctx, func(event interface{}) {
		switch event := event.(type) {
		case *fetch.EventRequestPaused:
			go func() {
				execCtx := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Target)
				reject := event.ResourceType == network.ResourceTypeImage || event.ResourceType == network.ResourceTypeMedia || event.ResourceType == network.ResourceTypeWebSocket
				// Disabled resource classes are deliberately not visited. data
				// and blob URLs have no network destination to public-validate.
				if !reject {
					if event.Request == nil {
						policy.fail(errors.New("document missing intercepted request"))
						reject = true
					} else {
						u, err := url.Parse(event.Request.URL)
						if err != nil {
							reject = true
						} else if u.Scheme != "data" && u.Scheme != "blob" {
							if _, err := ValidatePublicDocumentURL(event.Request.URL, nil); err != nil {
								reject = true
							}
						}
					}
				}
				var err error
				if reject {
					policy.mu.Lock()
					policy.blocked++
					policy.mu.Unlock()
					err = fetch.FailRequest(event.RequestID, network.ErrorReasonBlockedByClient).Do(execCtx)
				} else {
					err = fetch.ContinueRequest(event.RequestID).Do(execCtx)
				}
				if err != nil && ctx.Err() == nil && !documentTargetGoneError(err) {
					policy.fail(fmt.Errorf("document request control: %w", err))
				}
			}()
		}
	})
}

func documentRuntimeAvailable() error {
	switch runtime.GOOS {
	case "darwin", "linux":
		return nil
	default:
		return fmt.Errorf("document native backend unsupported on %s", runtime.GOOS)
	}
}

func documentCoreBlockedRequests(core BrowserCore) int64 {
	if impl, ok := core.(*browserCoreImpl); ok && impl.documentRuntime != nil {
		impl.documentRuntime.mu.Lock()
		defer impl.documentRuntime.mu.Unlock()
		return impl.documentRuntime.blocked
	}
	return 0
}

func documentTargetGoneError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	for _, known := range []string{"No target with given id", "No session with given id", "Session with given id not found", "Target closed", "Invalid InterceptionId", "Invalid interceptionId"} {
		if strings.Contains(message, known) {
			return true
		}
	}
	return false
}
func (owner *documentLaunchOwner) cleanup(ctx context.Context) error {
	if owner == nil || owner.pid.Load() == 0 {
		return nil
	}
	if handle := owner.handle.Load(); handle != nil {
		if err := handle.killWithContext(ctx); err != nil {
			return err
		}
	}
	return waitDocumentProcessGroup(ctx, int(owner.pid.Load()))
}
