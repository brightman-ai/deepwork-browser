package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/brightman-ai/kit/log"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
)

var _ = log.Module // suppress unused import

// chromeCandidates is the ordered list of Chrome executables to detect.
var chromeCandidates = func string {
	home, _ := os.UserHomeDir
	return string{
		"google-chrome"
		"google-chrome-stable"
		"chromium"
		"chromium-browser"
		filepath.Join(home, ".deepwork", "chromium", "chrome")
	}
}

// detectChrome returns the first available Chrome/Chromium executable path.
// Returns error if none found.
func (r *browserRuntime) detectChrome (string, error) {
	for _, candidate := range chromeCandidates {
		if filepath.IsAbs(candidate) {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		} else {
			if path, err := exec.LookPath(candidate); err == nil {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("chrome not found in PATH or cache")
}

// startChrome launches Chrome via Rod launcher and connects.
func (r *browserRuntime) startChrome(ctx context.Context, execPath string) error {
	profileDir := r.cfg.ProfileDir
	if profileDir == "" {
		home, _ := os.UserHomeDir
		profileDir = filepath.Join(home, ".deepwork", "browser-profile")
	}

	if err := os.MkdirAll(profileDir, 0755); err != nil {
		return fmt.Errorf("create profile dir: %w", err)
	}

	l := launcher.New.
		Bin(execPath).
		UserDataDir(profileDir).
		Headless(true)

	for _, flag := range r.cfg.ChromeFlags {
		l = l.Append(flags.Flag(flag))
	}

	controlURL, err := l.Launch
	if err != nil {
		return fmt.Errorf("launch chrome: %w", err)
	}

	browser := rod.New.ControlURL(controlURL)
	if err := browser.Connect; err != nil {
		return fmt.Errorf("connect to chrome: %w", err)
	}

	r.browserMu.Lock
	old := r.browser
	r.browser = browser
	r.browserMu.Unlock

	if old != nil {
		_ = old.Close
	}

	// Monitor for crash: Rod's internal context cancels when Chrome exits.
	go r.monitorChromeProcess(browser)

	return nil
}

// monitorChromeProcess waits for the Rod browser context to end (Chrome exit/crash).
func (r *browserRuntime) monitorChromeProcess(b *rod.Browser) {
	// Rod Browser has no direct "wait for process" API.
	// We monitor the browser's context: when Chrome exits, the WS connection
	// closes and Rod's context is cancelled.
	browserCtx := b.GetContext
	<-browserCtx.Done

	r.browserMu.Lock
	current := r.browser
	r.browserMu.Unlock

	// Only handle crash if this is still the active browser
	if current == b {
		state := r.State
		if state == StateRunning {
			logger.Warn("chrome process exited unexpectedly")
			go r.handleCrash(r.runCtx)
		}
	}
}

// downloadChromium downloads Chromium to ~/.deepwork/chromium/.
// Publishes BrowserDownloadProgress events during download.
func (r *browserRuntime) downloadChromium(ctx context.Context) error {
	if r.cfg.ChromiumURL == "" {
		return fmt.Errorf("%w: no chromium_url configured", ErrDownloadFailed)
	}

	home, _ := os.UserHomeDir
	destDir := filepath.Join(home, ".deepwork", "chromium")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("%w: mkdir %s: %v", ErrDownloadFailed, destDir, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.cfg.ChromiumURL, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDownloadFailed, err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDownloadFailed, err)
	}
	defer resp.Body.Close

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: HTTP %d", ErrDownloadFailed, resp.StatusCode)
	}

	totalBytes := resp.ContentLength
	destPath := filepath.Join(destDir, "chrome")
	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("%w: create file: %v", ErrDownloadFailed, err)
	}
	defer f.Close

	var downloaded int64
	buf := make(byte, 32*1024)
	for {
		select {
		case <-ctx.Done:
			return ctx.Err
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := f.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("%w: write: %v", ErrDownloadFailed, writeErr)
			}
			atomic.AddInt64(&downloaded, int64(n))
			var pct float64
			if totalBytes > 0 {
				pct = float64(downloaded) / float64(totalBytes) * 100
			}
			r.bus.Publish(EventBrowserDownloadProgress{
				Percent: pct
				BytesDownloaded: downloaded
				TotalBytes: totalBytes
			})
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("%w: read: %v", ErrDownloadFailed, readErr)
		}
	}

	// Make executable
	if err := os.Chmod(destPath, 0755); err != nil {
		return fmt.Errorf("%w: chmod: %v", ErrDownloadFailed, err)
	}

	logger.Info("chromium downloaded", "path", destPath, "bytes", downloaded)
	return nil
}

// reconnectCDP attempts to reconnect CDP with exponential backoff.
// On all failures, triggers crash recovery.
func (r *browserRuntime) reconnectCDP(ctx context.Context) {
	execPath, err := r.detectChrome
	if err != nil {
		go r.handleCrash(ctx)
		return
	}

	for i := 0; i < r.cfg.CDPReconnectAttempts; i++ {
		delay := time.Duration(1<<uint(i)) * time.Second // 1s/2s/4s
		select {
		case <-time.After(delay):
		case <-ctx.Done:
			return
		}

		if err := r.startChrome(ctx, execPath); err == nil {
			logger.Info("CDP reconnected", "attempt", i+1)
			return
		}
		logger.Warn("CDP reconnect failed", "attempt", i+1)
	}

	// All reconnect attempts failed — trigger crash recovery
	go r.handleCrash(ctx)
}

// downloadProgressReader wraps an io.Reader to track download progress.
// (helper kept for potential future use)
type downloadProgressReader struct {
	reader io.Reader
	total int64
	downloaded *int64
	bus EventBus
}

func (p *downloadProgressReader) Read(buf byte) (int, error) {
	n, err := p.reader.Read(buf)
	if n > 0 {
		current := atomic.AddInt64(p.downloaded, int64(n))
		var pct float64
		if p.total > 0 {
			pct = float64(current) / float64(p.total) * 100
		}
		p.bus.Publish(EventBrowserDownloadProgress{
			Percent: pct
			BytesDownloaded: current
			TotalBytes: p.total
		})
	}
	return n, err
}

// marshalJSON helper for EventBrowserDownloadProgress (used in event serialisation).
func marshalProgressEvent(e EventBrowserDownloadProgress) (byte, error) {
	return json.Marshal(map[string]any{
		"percent": e.Percent
		"bytes_downloaded": e.BytesDownloaded
		"total_bytes": e.TotalBytes
	})
}
