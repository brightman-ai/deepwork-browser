package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
)

func TestHeadlessTabsForegroundNeedsNoMuxHost(t *testing.T) {
	if err := ensureForegroundAllowedE(context.Background(), &browser.SessionInfo{Mode: browser.ModeHeadless, RuntimeID: "missing-runtime-headless"}, "tabs select"); err != nil {
		t.Fatal(err)
	}
}

func TestVisibleNativeTabsForegroundNeedsNoMuxHost(t *testing.T) {
	if err := ensureForegroundAllowedE(context.Background(), &browser.SessionInfo{Mode: browser.ModeVisible, RuntimeID: "missing-runtime-visible"}, "tabs select"); err != nil {
		t.Fatal(err)
	}
}

func TestHeadedTabsForegroundStillFailsClosed(t *testing.T) {
	if os.Getenv("DW_TEST_HEADED_GUARD") == "1" {
		if err := ensureForegroundAllowedE(context.Background(), &browser.SessionInfo{Mode: browser.ModeHeaded, RuntimeID: "missing-runtime-headed-test"}, "tabs select"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(exitRunErr)
		}
		t.Fatal("missing headed runtime was accepted")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHeadedTabsForegroundStillFailsClosed$")
	cmd.Env = append(os.Environ(), "DW_TEST_HEADED_GUARD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("headed foreground guard succeeded without runtime evidence")
	}
	if !strings.Contains(string(out), "BrowserMuxHost state unavailable") {
		t.Fatalf("unexpected guard failure: %s", out)
	}
}
