package browser

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestHumanInputRechecksLeaseAfterRuntimeLockWait(t *testing.T) {
	key := BrowserInputKey(fmt.Sprintf("human-lock-test-%d-%d", os.Getpid(), time.Now().UnixNano()), 0)
	held, err := AcquireBrowserInputLock(key, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	gw := NewInputGateway(nil, nil)
	lease, _, err := gw.RequestTakeover("human")
	if err != nil {
		t.Fatal(err)
	}
	reachedLock := make(chan struct{}, 1)
	gw.SetBrowserInputKeyProvider(func() string { reachedLock <- struct{}{}; return key })
	done := make(chan *InputAck, 1)
	go func() {
		done <- gw.HandleInput("human", &InputMessage{Type: "input", Seq: 1, Lease: lease, Event: InputEvent{Type: "mouse", Event: "mousePressed", Button: "left", X: 1, Y: 1}})
	}()
	select {
	case <-reachedLock:
	case <-time.After(time.Second):
		t.Fatal("human input did not reach runtime lock boundary")
	}
	select {
	case ack := <-done:
		t.Fatalf("input escaped held runtime lock: %+v", ack)
	case <-time.After(50 * time.Millisecond):
	}
	if err := gw.ReleaseTakeover("human"); err != nil {
		t.Fatal(err)
	}
	held.Release()
	select {
	case ack := <-done:
		if ack.Status != "rejected" || ack.Reason != "lease changed while waiting for runtime input lock" {
			t.Fatalf("stale human lease dispatched: %+v", ack)
		}
	case <-time.After(time.Second):
		t.Fatal("human input did not finish after runtime lock release")
	}
}
