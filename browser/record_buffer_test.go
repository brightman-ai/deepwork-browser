package browser

// record_buffer_test.go — RecordBuffer 单元测试 + InputGateway tap 集成测试
//
// TC 覆盖:
//   TC-RB-01: Start/Stop 生命周期
//   TC-RB-02: 未录制时 Append 不 panic
//   TC-RB-03: mousePressed → "click" step
//   TC-RB-04: 连续可打印字符合并为 "type"
//   TC-RB-05: 特殊键 → "keypress"
//   TC-RB-06: 混合事件 → 正确 seq
//   TC-RB-07: ExportJSON 可反序列化
//   TC-RB-08: SetSnapshotFn 填充 SnapshotBefore
//   TC-RB-09: 连续两次 Start 重置状态
//   TC-RB-10: Stop 未录制时不 panic，返回空 trace
//   TC-RB-11: 并发 Append 不 race
//   TC-IG-01: InputGateway.HandleInput → RecordBuffer tap 集成

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// ============================================================
// § TC-RB-01: Start/Stop 生命周期
// ============================================================

func Test_TC_RB_01_StartStop(t *testing.T) {
	rb := NewRecordBuffer()

	if rb.IsRecording() {
		t.Fatal("IsRecording() = true before Start, want false")
	}

	rb.Start("example.com", "https://example.com/page")

	if !rb.IsRecording() {
		t.Fatal("IsRecording() = false after Start, want true")
	}

	trace := rb.Stop()

	if rb.IsRecording() {
		t.Fatal("IsRecording() = true after Stop, want false")
	}
	if trace.Domain != "example.com" {
		t.Errorf("trace.Domain = %q, want %q", trace.Domain, "example.com")
	}
	if trace.StartURL != "https://example.com/page" {
		t.Errorf("trace.StartURL = %q, want %q", trace.StartURL, "https://example.com/page")
	}
	if trace.DurationMs < 0 {
		t.Errorf("trace.DurationMs = %d, want >= 0", trace.DurationMs)
	}
	if trace.StartTime.IsZero() {
		t.Error("trace.StartTime is zero, want non-zero")
	}
}

// ============================================================
// § TC-RB-02: 未录制时 Append 不 panic
// ============================================================

func Test_TC_RB_02_NotRecording_NoPanic(t *testing.T) {
	rb := NewRecordBuffer()

	// 未 Start 时调用不应 panic
	rb.AppendMouseEvent(&InputEvent{
		Type:  "mouse",
		Event: "mousePressed",
		X:     100,
		Y:     200,
	})
	rb.AppendKeyEvent(&InputEvent{
		Type:  "keyboard",
		Event: "keyDown",
		Key:   "a",
	})

	if rb.StepCount() != 0 {
		t.Errorf("StepCount() = %d, want 0 (not recording)", rb.StepCount())
	}
}

// ============================================================
// § TC-RB-03: mousePressed → "click" step
// ============================================================

func Test_TC_RB_03_AppendMouseClick(t *testing.T) {
	rb := NewRecordBuffer()
	rb.Start("test.com", "https://test.com")

	rb.AppendMouseEvent(&InputEvent{
		Type:   "mouse",
		Event:  "mousePressed",
		X:      150.0,
		Y:      300.0,
		Button: "left",
	})

	trace := rb.Stop()

	if len(trace.Steps) != 1 {
		t.Fatalf("len(trace.Steps) = %d, want 1", len(trace.Steps))
	}
	step := trace.Steps[0]
	if step.Action != "click" {
		t.Errorf("step.Action = %q, want %q", step.Action, "click")
	}
	if step.Seq != 1 {
		t.Errorf("step.Seq = %d, want 1", step.Seq)
	}
	if step.Coordinates == nil {
		t.Fatal("step.Coordinates is nil, want non-nil")
	}
	if step.Coordinates.X != 150.0 || step.Coordinates.Y != 300.0 {
		t.Errorf("step.Coordinates = {%.1f, %.1f}, want {150.0, 300.0}",
			step.Coordinates.X, step.Coordinates.Y)
	}
	if step.TimestampMs <= 0 {
		t.Errorf("step.TimestampMs = %d, want > 0", step.TimestampMs)
	}
}

// ============================================================
// § TC-RB-04: 连续可打印字符合并为 "type"
// ============================================================

func Test_TC_RB_04_AppendKeyType(t *testing.T) {
	rb := NewRecordBuffer()
	rb.Start("test.com", "https://test.com")

	keys := []string{"H", "e", "l", "l", "o"}
	for _, k := range keys {
		rb.AppendKeyEvent(&InputEvent{
			Type:  "keyboard",
			Event: "keyDown",
			Key:   k,
		})
	}

	trace := rb.Stop()

	if len(trace.Steps) != 1 {
		t.Fatalf("len(trace.Steps) = %d, want 1 (merged type step)", len(trace.Steps))
	}
	step := trace.Steps[0]
	if step.Action != "type" {
		t.Errorf("step.Action = %q, want %q", step.Action, "type")
	}
	if step.Text != "Hello" {
		t.Errorf("step.Text = %q, want %q", step.Text, "Hello")
	}
	if step.Seq != 1 {
		t.Errorf("step.Seq = %d, want 1", step.Seq)
	}
}

// ============================================================
// § TC-RB-05: 特殊键 → "keypress"
// ============================================================

func Test_TC_RB_05_AppendKeySpecial(t *testing.T) {
	rb := NewRecordBuffer()
	rb.Start("test.com", "https://test.com")

	rb.AppendKeyEvent(&InputEvent{
		Type:  "keyboard",
		Event: "keyDown",
		Key:   "Enter",
	})

	trace := rb.Stop()

	if len(trace.Steps) != 1 {
		t.Fatalf("len(trace.Steps) = %d, want 1", len(trace.Steps))
	}
	step := trace.Steps[0]
	if step.Action != "keypress" {
		t.Errorf("step.Action = %q, want %q", step.Action, "keypress")
	}
	if step.Key != "Enter" {
		t.Errorf("step.Key = %q, want %q", step.Key, "Enter")
	}
	if step.Seq != 1 {
		t.Errorf("step.Seq = %d, want 1", step.Seq)
	}
}

// ============================================================
// § TC-RB-06: 混合事件 → 正确 seq
// ============================================================

func Test_TC_RB_06_MixedEvents(t *testing.T) {
	rb := NewRecordBuffer()
	rb.Start("test.com", "https://test.com")

	// click
	rb.AppendMouseEvent(&InputEvent{Type: "mouse", Event: "mousePressed", X: 10, Y: 20})
	// type "hi"
	rb.AppendKeyEvent(&InputEvent{Type: "keyboard", Event: "keyDown", Key: "h"})
	rb.AppendKeyEvent(&InputEvent{Type: "keyboard", Event: "keyDown", Key: "i"})
	// Enter
	rb.AppendKeyEvent(&InputEvent{Type: "keyboard", Event: "keyDown", Key: "Enter"})

	trace := rb.Stop()

	if len(trace.Steps) != 3 {
		t.Fatalf("len(trace.Steps) = %d, want 3 (click + type + keypress)", len(trace.Steps))
	}

	// 验证 seq 和 action
	expectations := []struct {
		action string
		seq    int
	}{
		{"click", 1},
		{"type", 2},
		{"keypress", 3},
	}
	for i, exp := range expectations {
		s := trace.Steps[i]
		if s.Action != exp.action {
			t.Errorf("Steps[%d].Action = %q, want %q", i, s.Action, exp.action)
		}
		if s.Seq != exp.seq {
			t.Errorf("Steps[%d].Seq = %d, want %d", i, s.Seq, exp.seq)
		}
	}

	// type step 文本验证
	if trace.Steps[1].Text != "hi" {
		t.Errorf("Steps[1].Text = %q, want %q", trace.Steps[1].Text, "hi")
	}
}

// ============================================================
// § TC-RB-07: ExportJSON 可反序列化
// ============================================================

func Test_TC_RB_07_ExportJSON(t *testing.T) {
	rb := NewRecordBuffer()
	rb.Start("example.com", "https://example.com")
	rb.AppendMouseEvent(&InputEvent{Type: "mouse", Event: "mousePressed", X: 5, Y: 10})

	data, err := rb.ExportJSON()
	if err != nil {
		t.Fatalf("ExportJSON() error = %v", err)
	}
	if len(data) == 0 {
		t.Fatal("ExportJSON() returned empty bytes")
	}

	var parsed RecordTrace
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal(ExportJSON()) error = %v\nJSON: %s", err, data)
	}
	if parsed.Domain != "example.com" {
		t.Errorf("parsed.Domain = %q, want %q", parsed.Domain, "example.com")
	}
	if len(parsed.Steps) != 1 {
		t.Errorf("len(parsed.Steps) = %d, want 1", len(parsed.Steps))
	}
}

// ============================================================
// § TC-RB-08: SetSnapshotFn 填充 SnapshotBefore
// ============================================================

func Test_TC_RB_08_SnapshotFn(t *testing.T) {
	rb := NewRecordBuffer()

	called := false
	rb.SetSnapshotFn(func(x, y float64) string {
		called = true
		return "a11y-subtree-stub"
	})

	rb.Start("test.com", "https://test.com")
	rb.AppendMouseEvent(&InputEvent{Type: "mouse", Event: "mousePressed", X: 50, Y: 60})
	trace := rb.Stop()

	if !called {
		t.Error("snapshotFn was not called during mousePressed")
	}
	if len(trace.Steps) == 0 {
		t.Fatal("no steps recorded")
	}
	if trace.Steps[0].SnapshotBefore != "a11y-subtree-stub" {
		t.Errorf("SnapshotBefore = %q, want %q", trace.Steps[0].SnapshotBefore, "a11y-subtree-stub")
	}
}

// ============================================================
// § TC-RB-09: 连续两次 Start 重置状态
// ============================================================

func Test_TC_RB_09_DoubleStart(t *testing.T) {
	rb := NewRecordBuffer()

	rb.Start("first.com", "https://first.com")
	rb.AppendMouseEvent(&InputEvent{Type: "mouse", Event: "mousePressed", X: 1, Y: 1})

	// 第二次 Start 应重置
	rb.Start("second.com", "https://second.com")

	if rb.StepCount() != 0 {
		t.Errorf("StepCount() = %d after second Start, want 0", rb.StepCount())
	}

	trace := rb.Stop()
	if trace.Domain != "second.com" {
		t.Errorf("trace.Domain = %q, want %q", trace.Domain, "second.com")
	}
	if len(trace.Steps) != 0 {
		t.Errorf("len(trace.Steps) = %d, want 0 (reset by second Start)", len(trace.Steps))
	}
}

// ============================================================
// § TC-RB-10: Stop 未录制时不 panic，返回空 trace
// ============================================================

func Test_TC_RB_10_StopWhenNotRecording(t *testing.T) {
	rb := NewRecordBuffer()

	// 未 Start，直接 Stop — 不应 panic
	trace := rb.Stop()

	if trace.Domain != "" || trace.StartURL != "" {
		t.Errorf("Stop() on non-recording returned non-empty trace: %+v", trace)
	}
}

// ============================================================
// § TC-RB-11: 并发 Append 不 race (需 -race 标志)
// ============================================================

func Test_TC_RB_11_Concurrent(t *testing.T) {
	rb := NewRecordBuffer()
	rb.Start("concurrent.com", "https://concurrent.com")

	const goroutines = 20
	const eventsPerGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < eventsPerGoroutine; j++ {
				if j%2 == 0 {
					rb.AppendMouseEvent(&InputEvent{
						Type:  "mouse",
						Event: "mousePressed",
						X:     float64(id),
						Y:     float64(j),
					})
				} else {
					rb.AppendKeyEvent(&InputEvent{
						Type:  "keyboard",
						Event: "keyDown",
						Key:   "a",
					})
				}
			}
		}(i)
	}

	wg.Wait()
	trace := rb.Stop()

	// 总 step 数 > 0 即可（合并逻辑使精确数不确定）
	if len(trace.Steps) == 0 {
		t.Error("no steps recorded after concurrent append")
	}
	// 验证 seq 单调递增
	for i := 1; i < len(trace.Steps); i++ {
		if trace.Steps[i].Seq <= trace.Steps[i-1].Seq {
			t.Errorf("Steps[%d].Seq=%d not > Steps[%d].Seq=%d — seq not monotonic",
				i, trace.Steps[i].Seq, i-1, trace.Steps[i-1].Seq)
			break
		}
	}
}

// ============================================================
// § TC-IG-01: InputGateway.HandleInput → RecordBuffer tap 集成
// ============================================================

func Test_TC_IG_01_InputGateway_RecordTap(t *testing.T) {
	// 构造 InputGateway — 使用 test hook 避免真实 Chrome
	gw := NewInputGateway(context.Background(), nil)

	dispatchMouseCalls := 0
	dispatchKeyCalls := 0
	gw.dispatchMouse = func(_ context.Context, _ *InputEvent) error {
		dispatchMouseCalls++
		return nil
	}
	gw.dispatchKey = func(_ context.Context, _ *InputEvent) error {
		dispatchKeyCalls++
		return nil
	}

	// 设置 Takeover 模式（模拟已抢锁状态）
	gw.mu.Lock()
	gw.mode = TakeoverModeTakeover
	gw.owner = "conn-test"
	gw.leaseToken = "lease-abc"
	gw.leaseExpiry = time.Now().Add(time.Minute)
	gw.mu.Unlock()

	// 注入 RecordBuffer
	rb := NewRecordBuffer()
	rb.Start("gateway.test", "https://gateway.test")
	gw.SetRecordBuffer(rb)

	// 发送 mouse 事件
	ack := gw.HandleInput("conn-test", &InputMessage{
		Type:  "input",
		Seq:   1,
		Lease: "lease-abc",
		Event: InputEvent{
			Type:   "mouse",
			Event:  "mousePressed",
			Button: "left",
			X:      100,
			Y:      200,
		},
	})
	if ack.Status != "accepted" {
		t.Fatalf("HandleInput mouse: ack.Status = %q, want %q", ack.Status, "accepted")
	}

	// 发送 keyboard 事件
	ack = gw.HandleInput("conn-test", &InputMessage{
		Type:  "input",
		Seq:   2,
		Lease: "lease-abc",
		Event: InputEvent{
			Type:  "keyboard",
			Event: "keyDown",
			Key:   "Enter",
		},
	})
	if ack.Status != "accepted" {
		t.Fatalf("HandleInput keyboard: ack.Status = %q, want %q", ack.Status, "accepted")
	}

	// 验证 dispatch 调用次数
	if dispatchMouseCalls != 1 {
		t.Errorf("dispatchMouseCalls = %d, want 1", dispatchMouseCalls)
	}
	if dispatchKeyCalls != 1 {
		t.Errorf("dispatchKeyCalls = %d, want 1", dispatchKeyCalls)
	}

	// 验证 RecordBuffer 收到事件
	trace := rb.Stop()
	if len(trace.Steps) != 2 {
		t.Fatalf("len(trace.Steps) = %d, want 2 (click + keypress)", len(trace.Steps))
	}

	clickStep := trace.Steps[0]
	if clickStep.Action != "click" {
		t.Errorf("Steps[0].Action = %q, want %q", clickStep.Action, "click")
	}
	if clickStep.Seq != 1 {
		t.Errorf("Steps[0].Seq = %d, want 1", clickStep.Seq)
	}

	keypressStep := trace.Steps[1]
	if keypressStep.Action != "keypress" {
		t.Errorf("Steps[1].Action = %q, want %q", keypressStep.Action, "keypress")
	}
	if keypressStep.Key != "Enter" {
		t.Errorf("Steps[1].Key = %q, want %q", keypressStep.Key, "Enter")
	}

	// 负向断言: 无 RecordBuffer 时不崩溃
	gw.SetRecordBuffer(nil)
	ack = gw.HandleInput("conn-test", &InputMessage{
		Type:  "input",
		Seq:   3,
		Lease: "lease-abc",
		Event: InputEvent{Type: "mouse", Event: "mousePressed", X: 1, Y: 1},
	})
	if ack.Status != "accepted" {
		t.Errorf("HandleInput with nil RecordBuffer: ack.Status = %q, want accepted", ack.Status)
	}
}

// ============================================================
// § 补充: mouseReleased/mouseMoved 不产生 step
// ============================================================

func Test_TC_RB_MouseRelease_Ignored(t *testing.T) {
	rb := NewRecordBuffer()
	rb.Start("test.com", "https://test.com")

	rb.AppendMouseEvent(&InputEvent{Type: "mouse", Event: "mouseReleased", X: 10, Y: 20})
	rb.AppendMouseEvent(&InputEvent{Type: "mouse", Event: "mouseMoved", X: 30, Y: 40})

	if rb.StepCount() != 0 {
		t.Errorf("StepCount() = %d, want 0 (mouseReleased and mouseMoved should be ignored)", rb.StepCount())
	}
}

// ============================================================
// § 补充: keyUp/char 不产生 step
// ============================================================

func Test_TC_RB_KeyUp_Ignored(t *testing.T) {
	rb := NewRecordBuffer()
	rb.Start("test.com", "https://test.com")

	rb.AppendKeyEvent(&InputEvent{Type: "keyboard", Event: "keyUp", Key: "a"})
	rb.AppendKeyEvent(&InputEvent{Type: "keyboard", Event: "char", Key: "a"})

	if rb.StepCount() != 0 {
		t.Errorf("StepCount() = %d, want 0 (keyUp and char should be ignored)", rb.StepCount())
	}
}
