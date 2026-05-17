package browser

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
)

func seedTrackerPrimary(t *testing.T, tracker *TargetTracker, id, url, title string) {
	t.Helper
	tid := target.ID(id)
	tracker.primaryID = tid
	tracker.activeID = tid
	tracker.targets[tid] = &trackedTarget{
		ID: tid
		URL: url
		Title: title
		Ctx: tracker.browserCtx
		Cancel: func {}
		Created: time.Now
		Closable: false
	}
	tracker.order = removeTargetFromOrder(tracker.order, tid)
	tracker.addTargetOrderLocked(tid, true)
}

func TestTargetTracker_HandleTargetCreated_PendingTargetBroadcastsTabList(t *testing.T) {
	tracker := NewTargetTracker(context.Background)
	seedTrackerPrimary(t, tracker, "root-target", "https://www.baidu.com/", "Baidu")

	type switchState struct {
		url string
		title string
		count int
	}
	stateCh := make(chan switchState, 1)
	tracker.SetOnSwitch(func(url, title string, targetCount int) {
		stateCh <- switchState{url: url, title: title, count: targetCount}
	})

	tracker.HandleTargetCreated(&target.Info{
		TargetID: "pending-tab"
		Type: "page"
		URL: ""
		OpenerID: "root-target"
	})

	select {
	case state := <-stateCh:
		if state.url != "https://www.baidu.com/" {
			t.Fatalf("broadcast URL = %q, want main tab URL", state.url)
		}
		if state.title != "Baidu" {
			t.Fatalf("broadcast Title = %q, want main tab title", state.title)
		}
		if state.count != 2 {
			t.Fatalf("broadcast targetCount = %d, want 2", state.count)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("pending target registration must broadcast updated tab state")
	}

	tabs := tracker.ListTargets
	if len(tabs) != 2 {
		t.Fatalf("ListTargets len = %d, want 2", len(tabs))
	}
	if tabs[1].ID != "pending-tab" {
		t.Fatalf("secondary tab id = %q, want pending-tab", tabs[1].ID)
	}
	if tabs[1].Active {
		t.Fatal("pending blank target must not become active before URL resolves")
	}
}

func TestLiveViewEngine_HandleFrameFrom_IgnoresStaleTargetFrames(t *testing.T) {
	hub := NewFrameBroadcastHub
	frameCh := hub.Subscribe("test-conn")
	defer hub.Unsubscribe("test-conn", frameCh)

	engine := newLiveViewEngine(1280, 720)
	oldCtx := context.WithValue(context.Background, "ctx", "old")
	newCtx := context.WithValue(context.Background, "ctx", "new")

	engine.mu.Lock
	engine.hub = hub
	engine.ctx = newCtx
	engine.activeTargetID = "new-target"
	engine.mu.Unlock

	engine.handleFrameFrom(oldCtx, &page.EventScreencastFrame{
		Data: base64.StdEncoding.EncodeToString(byte("old-frame"))
		SessionID: 1
	})

	select {
	case frame := <-frameCh:
		t.Fatalf("stale target frame should not be published, got %q", string(frame.Data))
	case <-time.After(150 * time.Millisecond):
	}

	engine.handleFrameFrom(newCtx, &page.EventScreencastFrame{
		Data: base64.StdEncoding.EncodeToString(byte("new-frame"))
		SessionID: 2
	})

	select {
	case frame := <-frameCh:
		if string(frame.Data) != "new-frame" {
			t.Fatalf("published frame = %q, want new-frame", string(frame.Data))
		}
		if frame.TargetID != "new-target" {
			t.Fatalf("published frame TargetID = %q, want new-target", frame.TargetID)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("active target frame was not published")
	}
}

func TestTargetTracker_HandleTargetCreated_DoesNotAutoFollowOpenerlessTargetWithoutClaim(t *testing.T) {
	tracker := NewTargetTracker(context.Background)
	seedTrackerPrimary(t, tracker, "root-target", "https://source.example/", "Source")
	tracker.SetLiveEngine(newLiveViewEngine(1280, 720), NewFrameBroadcastHub)

	tracker.HandleTargetCreated(&target.Info{
		TargetID: "foreign-tab"
		Type: "page"
		URL: "https://tieba.baidu.com/"
	})

	if got := tracker.ActiveTargetID; got != "root-target" {
		t.Fatalf("ActiveTargetID = %s, want root-target for opener-less target without claim", got)
	}

	tabs := tracker.ListTargets
	if len(tabs) != 2 {
		t.Fatalf("ListTargets len = %d, want 2", len(tabs))
	}
	if tabs[1].Active {
		t.Fatal("opener-less target without claim must not become active")
	}
}

func TestTargetTracker_SwitchToTargetUpdatesStateWithoutWarmup(t *testing.T) {
	tracker := NewTargetTracker(context.Background)
	seedTrackerPrimary(t, tracker, "root-target", "https://first.example/", "First")
	tracker.targets["second"] = &trackedTarget{
		ID: "second"
		URL: "https://second.example/"
		Title: "Second"
		Ctx: context.Background
		Closable: true
	}
	tracker.addTargetOrderLocked("second", false)

	stateCh := make(chan string, 1)
	tracker.SetOnSwitch(func(url, title string, targetCount int) {
		stateCh <- url
	})

	start := time.Now
	if err := tracker.SwitchToTarget("second"); err != nil {
		t.Fatalf("SwitchToTarget: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("SwitchToTarget blocked for %s; tab switching must not wait for CDP warmup", elapsed)
	}
	if got := tracker.ActiveTargetID; got != "second" {
		t.Fatalf("ActiveTargetID = %s, want second", got)
	}
	select {
	case got := <-stateCh:
		if got != "https://second.example/" {
			t.Fatalf("onSwitch url = %q, want second tab URL", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("SwitchToTarget must broadcast active tab state immediately")
	}
}

func TestTargetTracker_HandleTargetCreated_OpenerMustMatchCurrentSource(t *testing.T) {
	tracker := NewTargetTracker(context.Background)
	seedTrackerPrimary(t, tracker, "root-target", "https://source.example/", "Source")
	tracker.SetLiveEngine(newLiveViewEngine(1280, 720), NewFrameBroadcastHub)

	tracker.HandleTargetCreated(&target.Info{
		TargetID: "foreign-opener"
		Type: "page"
		URL: "https://www.bilibili.com/"
		OpenerID: "other-target"
	})

	if got := tracker.ActiveTargetID; got != "root-target" {
		t.Fatalf("ActiveTargetID = %s, want root-target for foreign opener", got)
	}

	tracker.HandleTargetCreated(&target.Info{
		TargetID: "current-opener"
		Type: "page"
		URL: "https://www.bilibili.com/"
		OpenerID: "root-target"
	})

	if got := tracker.ActiveTargetID; got != "current-opener" {
		t.Fatalf("ActiveTargetID = %s, want current-opener", got)
	}
}

func TestTargetTracker_HandleTargetInfoChanged_WindowOpenHintFollowsBlankPopup(t *testing.T) {
	tracker := NewTargetTracker(context.Background)
	seedTrackerPrimary(t, tracker, "root-target", "https://source.example/", "Source")
	tracker.SetLiveEngine(newLiveViewEngine(1280, 720), NewFrameBroadcastHub)
	tracker.recordWindowOpenHint("root-target", &page.EventWindowOpen{
		URL: "https://tieba.baidu.com/"
		UserGesture: true
	})

	tracker.HandleTargetCreated(&target.Info{
		TargetID: "pending-window-open"
		Type: "page"
		URL: ""
	})

	if got := tracker.ActiveTargetID; got != "root-target" {
		t.Fatalf("ActiveTargetID after blank create = %s, want root-target", got)
	}

	tracker.HandleTargetInfoChanged(&target.Info{
		TargetID: "pending-window-open"
		Type: "page"
		URL: "https://tieba.baidu.com/"
	})

	if got := tracker.ActiveTargetID; got != "pending-window-open" {
		t.Fatalf("ActiveTargetID after URL resolve = %s, want pending-window-open", got)
	}
}

func TestTargetTracker_RecordUserGesture_ClaimsOpenerlessTarget(t *testing.T) {
	tracker := NewTargetTracker(context.Background)
	seedTrackerPrimary(t, tracker, "root-target", "https://source.example/", "Source")
	tracker.SetLiveEngine(newLiveViewEngine(1280, 720), NewFrameBroadcastHub)
	tracker.RecordUserGesture(&InputEvent{
		Type: "mouse"
		Event: "mouseReleased"
		Button: "left"
	})

	tracker.HandleTargetCreated(&target.Info{
		TargetID: "claimed-tab"
		Type: "page"
		URL: "https://tieba.baidu.com/"
	})

	if got := tracker.ActiveTargetID; got != "claimed-tab" {
		t.Fatalf("ActiveTargetID = %s, want claimed-tab", got)
	}
}

func TestTargetTracker_HandleTargetCreated_NonGestureWindowOpenDoesNotAutoFollow(t *testing.T) {
	tracker := NewTargetTracker(context.Background)
	seedTrackerPrimary(t, tracker, "root-target", "https://source.example/", "Source")
	tracker.SetLiveEngine(newLiveViewEngine(1280, 720), NewFrameBroadcastHub)
	tracker.recordWindowOpenHint("root-target", &page.EventWindowOpen{
		URL: "https://tieba.baidu.com/"
		UserGesture: false
	})

	tracker.HandleTargetCreated(&target.Info{
		TargetID: "non-gesture-window-open"
		Type: "page"
		URL: "https://tieba.baidu.com/"
	})

	if got := tracker.ActiveTargetID; got != "root-target" {
		t.Fatalf("ActiveTargetID = %s, want root-target for non-gesture windowOpen", got)
	}
}
