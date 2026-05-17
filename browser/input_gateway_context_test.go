package browser

import (
	"context"
	"testing"
	"time"
)

func TestInputGateway_ResolveCDPContextPrefersLiveProvider(t *testing.T) {
	fallbackCtx, fallbackCancel := context.WithCancel(context.Background)
	fallbackCancel
	liveCtx := context.WithValue(context.Background, struct{ k string }{"ctx"}, "live")

	gw := NewInputGateway(fallbackCtx, nil)
	gw.SetCDPContextProvider(func context.Context {
		return liveCtx
	})

	if got := gw.resolveCDPContext; got != liveCtx {
		t.Fatalf("resolveCDPContext = %v, want live provider ctx", got)
	}
}

func TestInputGateway_HandleInputRetriesAfterContextCanceled(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background)
	cancel
	liveCtx := context.WithValue(context.Background, struct{ k string }{"ctx"}, "live")

	gw := NewInputGateway(canceledCtx, nil)
	providerCalls := 0
	gw.SetCDPContextProvider(func context.Context {
		providerCalls++
		if providerCalls <= 2 {
			return canceledCtx
		}
		return liveCtx
	})

	dispatchCalls := 0
	gw.dispatchMouse = func(ctx context.Context, event *InputEvent) error {
		dispatchCalls++
		return ctx.Err
	}

	gw.mu.Lock
	gw.mode = TakeoverModeTakeover
	gw.owner = "conn-test"
	gw.leaseToken = "lease-token"
	gw.leaseExpiry = time.Now.Add(time.Minute)
	gw.mu.Unlock

	ack := gw.HandleInput("conn-test", &InputMessage{
		Type: "input"
		Seq: 1
		Lease: "lease-token"
		Event: InputEvent{
			Type: "mouse"
			Event: "mousePressed"
			Button: "left"
			X: 10
			Y: 20
		}
	})
	if ack == nil || ack.Status != "accepted" {
		t.Fatalf("HandleInput ack = %+v, want accepted", ack)
	}
	if dispatchCalls != 2 {
		t.Fatalf("dispatchMouse calls = %d, want 2 (initial + retry)", dispatchCalls)
	}
	if providerCalls < 3 {
		t.Fatalf("provider calls = %d, want at least 3 (resolve + retry resolve)", providerCalls)
	}
}
