package browser

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brightman-ai/kit/obs"
)

func TestInputGateway_RejectionLoggedToObs(t *testing.T) {
	since := time.Now().Add(-1 * time.Second)

	gw := NewInputGateway(nil, nil)
	ack := gw.HandleInput("conn-test", &InputMessage{
		Type:  "input",
		Seq:   7,
		Lease: "bad",
		Event: InputEvent{Type: "keyboard", Event: "keyDown", Key: "a", Code: "KeyA"},
	})
	if ack == nil || ack.Status != "rejected" {
		t.Fatalf("HandleInput() ack = %+v, want rejected", ack)
	}

	entries := obs.RecentEntries("WARN", 20, since)
	for _, entry := range entries {
		if entry.Mod == "browser" && entry.Msg == "input dispatch rejected" {
			if got := entry.Ext["reason"]; got != "not_in_takeover" {
				t.Fatalf("reason = %v, want not_in_takeover", got)
			}
			return
		}
	}
	t.Fatalf("obs recent entries missing browser rejection log: %+v", entries)
}

func TestWarmTargetContext_LogsToObs(t *testing.T) {
	since := time.Now().Add(-1 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := warmTargetContext(ctx); err == nil {
		t.Fatal("warmTargetContext() err = nil, want error")
	}

	entries := obs.RecentEntries("WARN", 20, since)
	for _, entry := range entries {
		if entry.Mod == "browser" && entry.Msg == "warm target context failed" {
			if !strings.Contains(entry.STG, STGLiveView) {
				t.Fatalf("stage = %q, want %q", entry.STG, STGLiveView)
			}
			return
		}
	}
	t.Fatalf("obs recent entries missing warm target log: %+v", entries)
}
