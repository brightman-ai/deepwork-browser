package browser

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// StartScreencast begins capturing CDP Screencast frames.
// Returns a channel that receives Frame values.
// The channel is closed when StopScreencast is called or ctx is cancelled.
func (r *browserRuntime) StartScreencast(ctx context.Context, quality int, fps int) (<-chan Frame, error) {
	if r.State() != StateRunning {
		return nil, ErrBrowserUnavailable
	}

	page, err := r.ensureLiveViewPage(ctx)
	if err != nil {
		return nil, fmt.Errorf("liveview page: %w", err)
	}

	if quality <= 0 {
		quality = 80
	}
	if fps <= 0 {
		fps = 10
	}

	ch := make(chan Frame, 10) // buffered to prevent blocking on slow consumers

	screenCtx, screenCancel := context.WithCancel(ctx)

	r.liveviewMu.Lock()
	if r.screencastCancel != nil {
		r.screencastCancel() // stop any existing screencast
	}
	r.screencastCancel = screenCancel
	r.screencastCh = ch
	r.liveviewMu.Unlock()

	go r.screencastLoop(screenCtx, page, quality, fps, ch)

	return ch, nil
}

// StopScreencast stops the CDP Screencast and closes the frame channel.
func (r *browserRuntime) StopScreencast(ctx context.Context) error {
	r.liveviewMu.Lock()
	cancel := r.screencastCancel
	r.screencastCancel = nil
	r.liveviewMu.Unlock()

	if cancel != nil {
		cancel()
	}
	return nil
}

// ensureLiveViewPage returns the persistent liveview page, creating it if needed.
func (r *browserRuntime) ensureLiveViewPage(ctx context.Context) (*rod.Page, error) {
	r.liveviewMu.Lock()
	defer r.liveviewMu.Unlock()

	if r.liveviewPage != nil {
		return r.liveviewPage, nil
	}

	r.browserMu.Lock()
	b := r.browser
	r.browserMu.Unlock()

	if b == nil {
		return nil, ErrBrowserUnavailable
	}

	page, err := b.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return nil, err
	}
	r.liveviewPage = page
	return page, nil
}

// screencastLoop runs CDP Page.startScreencast and pushes frames to ch.
func (r *browserRuntime) screencastLoop(ctx context.Context, page *rod.Page, quality, fps int, ch chan<- Frame) {
	defer close(ch)

	var frameNo int64

	q := quality
	w := 1280
	h := 720
	nth := 1

	// Start CDP screencast
	startEvt := proto.PageStartScreencast{
		Format:        proto.PageStartScreencastFormatJpeg,
		Quality:       &q,
		MaxWidth:      &w,
		MaxHeight:     &h,
		EveryNthFrame: &nth,
	}
	err := startEvt.Call(page)
	if err != nil {
		logger.Warn("start screencast failed", "err", err)
		return
	}

	defer func() {
		stopEvt := proto.PageStopScreencast{}
		_ = stopEvt.Call(page)
	}()

	// EachEvent registers event handlers and returns a wait function.
	// The wait function blocks until the context is done.
	wait := page.EachEvent(func(e *proto.PageScreencastFrame) {
		select {
		case <-ctx.Done():
			return
		default:
		}

		no := atomic.AddInt64(&frameNo, 1)
		frame := Frame{
			Data:      e.Data,
			Timestamp: time.Now(),
			FrameNo:   no,
		}
		if e.Metadata != nil {
			frame.Width = int(e.Metadata.DeviceWidth)
			frame.Height = int(e.Metadata.DeviceHeight)
		}

		select {
		case ch <- frame:
		case <-ctx.Done():
			return
		default:
			// Drop frame if channel full (non-blocking)
		}

		// Acknowledge frame to continue screencast
		ack := proto.PageScreencastFrameAck{SessionID: e.SessionID}
		_ = ack.Call(page)
	})

	// Keep running until context cancelled
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait()
	}()

	select {
	case <-ctx.Done():
	case <-done:
	}
}

// RequestTakeover acquires the TakeoverLock and publishes TakeoverStateChanged.
func (r *browserRuntime) RequestTakeover(ctx context.Context) error {
	if err := r.takeoverLock.Acquire("user"); err != nil {
		return err
	}
	r.bus.Publish(EventTakeoverStateChanged{Active: true})
	logger.Info("takeover acquired")
	return nil
}

// ReleaseTakeover releases the TakeoverLock and publishes TakeoverStateChanged.
func (r *browserRuntime) ReleaseTakeover(ctx context.Context) error {
	r.takeoverLock.Release()
	r.bus.Publish(EventTakeoverStateChanged{Active: false})
	logger.Info("takeover released")
	return nil
}
