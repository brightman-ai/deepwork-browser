package browser

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCoordinateAction_MissingCSSFailsImmediately(t *testing.T) {
	requireChromeForPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	core, err := NewBrowserCore(ctx, fmt.Sprintf("coordinate-miss-%d", time.Now().UnixNano()),
		WithMode(BrowserModeHeadless))
	if err != nil {
		t.Fatalf("NewBrowserCore: %v", err)
	}
	defer core.Close(context.Background())
	core.SetPolicy(SessionPolicy{RemoteWrites: RemoteWriteAllow}, "")

	started := time.Now()
	_, err = core.Act(ctx, "dragat css=#definitely-missing 20% 20% 80% 80%", false)
	if err == nil {
		t.Fatal("missing coordinate target should fail")
	}
	if !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("error = %v, want ErrRefNotFound", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("missing coordinate target failed after %s, want immediate failure", elapsed)
	}
}

func TestSetLiveViewport_PostconditionIsSynchronous(t *testing.T) {
	requireChromeForPool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	core, err := NewBrowserCore(ctx, fmt.Sprintf("viewport-contract-%d", time.Now().UnixNano()),
		WithFingerprintPreset(PresetIPhone15Pro), WithTouchEmulation(true), WithMode(BrowserModeHeadless))
	if err != nil {
		t.Fatalf("NewBrowserCore: %v", err)
	}
	defer core.Close(context.Background())
	page := `<meta name="viewport" content="width=device-width,initial-scale=1"><title>viewport contract</title>`
	dataURL := "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte(page))
	if _, err := core.Navigate(ctx, dataURL); err != nil {
		t.Fatalf("Navigate viewport fixture: %v", err)
	}

	syncer, ok := core.(LiveViewportSyncer)
	if !ok {
		t.Fatal("BrowserCore does not implement LiveViewportSyncer")
	}
	if err := syncer.SetLiveViewport(393, 659, 3, true); err != nil {
		t.Fatalf("SetLiveViewport: %v", err)
	}

	var got struct {
		Width  int     `json:"width"`
		Height int     `json:"height"`
		DPR    float64 `json:"dpr"`
		Touch  int     `json:"touch"`
		Coarse bool    `json:"coarse"`
	}
	if err := core.EvalJS(ctx, `({
		width: innerWidth,
		height: innerHeight,
		dpr: devicePixelRatio,
		touch: navigator.maxTouchPoints,
		coarse: matchMedia('(pointer: coarse)').matches
	})`, &got); err != nil {
		t.Fatalf("EvalJS: %v", err)
	}
	if got.Width != 393 || got.Height != 659 || got.DPR != 3 || got.Touch < 1 || !got.Coarse {
		t.Fatalf("viewport postcondition = %+v, want 393x659 dpr=3 touch/coarse", got)
	}
}
