package browser

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

func TestDocumentHiddenTargetContextInheritsAllocatedBrowser(t *testing.T) {
	// Exercise the real chromedp constructor without a websocket or Run. Its
	// allocation-only options panic when a browser has already been inherited.
	root, rootCancel := chromedp.NewRemoteAllocator(context.Background(), "ws://must-not-connect.invalid")
	defer rootCancel()
	b := &chromedp.Browser{}
	chromedp.FromContext(root).Browser = b
	ctx, cancel := newDocumentHiddenTargetContext(root, target.ID("owned-hidden-target"))
	defer cancel()
	c := chromedp.FromContext(ctx)
	if c.Browser != b || c.Allocator != chromedp.FromContext(root).Allocator || c.Target != nil {
		t.Fatal("hidden target construction allocated a browser/target instead of inheriting the owned connection")
	}
}

func TestDocumentHeadedLaunchHasNoStartupWindowOrURL(t *testing.T) {
	args, err := prepareDocumentLaunchMode(t.TempDir(), "http://127.0.0.1:32123", 0, 1024, 768, ModeHeaded)
	if err != nil {
		t.Fatal(err)
	}
	flags := make(map[string]bool)
	for _, arg := range args {
		flags[arg] = true
		if !strings.HasPrefix(arg, "--") {
			t.Fatalf("headed startup contains initial URL/positional argument %q", arg)
		}
		for _, forbidden := range []string{"--headless", "--window-position", "--start-fullscreen", "--no-sandbox", "--disable-web-security", "--disable-blink-features=AutomationControlled", "--user-agent="} {
			if strings.HasPrefix(arg, forbidden) {
				t.Fatalf("headed document inherited unsafe/window argument %q", arg)
			}
		}
	}
	for _, flag := range []string{"--no-startup-window", "--enable-automation", "--mute-audio", "--block-new-web-contents", "--deny-permission-prompts", "--proxy-server=http://127.0.0.1:32123", "--proxy-bypass-list=<-loopback>", "--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1"} {
		if !flags[flag] {
			t.Errorf("missing isolation flag %s", flag)
		}
	}
}

func TestDocumentHiddenTargetWireOmitsWindowCreationParameters(t *testing.T) {
	data, err := json.Marshal(documentHiddenTargetParams{URL: "about:blank", Hidden: true, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire) != 3 || wire["url"] != "about:blank" || wire["hidden"] != true || wire["background"] != true {
		t.Fatalf("hidden target wire contract can create an ordinary window: %s", data)
	}
}

func TestDocumentRenderingValidationFailureIsNotStarted(t *testing.T) {
	for _, mode := range []BrowserMode{ModeHeadless, ModeHeaded, ModeVisible, "unknown"} {
		doc, err := FetchDocument(context.Background(), DocumentRequest{URL: "file:///must-not-launch", Mode: mode})
		var failure *DocumentAcquisitionError
		if doc != nil || !errors.As(err, &failure) {
			t.Fatalf("mode %q: %v %v", mode, doc, err)
		}
		if failure.Diagnostic.Rendering.Mode != mode || failure.Diagnostic.Rendering.Surface != "not-started" {
			t.Fatalf("failed validation claimed a running target: %+v", failure.Diagnostic.Rendering)
		}
	}
}

func TestDocumentControlledLaunchRejectsDisplayModesBeforeLaunch(t *testing.T) {
	for _, mode := range []BrowserMode{ModeVisible, "unknown"} {
		result, err := launchControlledBrowserCoreChrome(context.Background(), controlledBrowserCoreLaunchOptions{documentProxy: "http://127.0.0.1:32123", mode: mode, chromePath: "/must-not-execute"})
		if result != nil || err == nil || !strings.Contains(err.Error(), "document mode") {
			t.Fatalf("mode crossed launch boundary: %v %v", result, err)
		}
	}
}
