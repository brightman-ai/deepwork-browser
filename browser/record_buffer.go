package browser

// record_buffer.go — Recording 缓冲区 
//
// RecordBuffer 接收来自 InputGateway 的事件 tap，组装为 RecordTrace。
// 零侵入设计: 仅在 recording 为 true 时执行，不影响 InputGateway 主路径。
// 键盘输入合并: 连续可打印字符 keyDown 合并为一个 "type" step（2s 内）。

import (
	"encoding/json"
	"sync"
	"time"
	"unicode/utf8"
)

// ============================================================
// § 数据类型
// ============================================================

// RecordTarget 描述操作目标的 A11y 元数据。
type RecordTarget struct {
	Selector string `json:"selector"`
	Role string `json:"role"`
	Name string `json:"name"`
	Tag string `json:"tag"`
}

// RecordXY 记录操作坐标。
type RecordXY struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// RecordStep 是 trace 中的单个操作步骤。
type RecordStep struct {
	Seq int `json:"seq"`
	Action string `json:"action"` // "click" | "type" | "keypress" | "scroll"
	Target *RecordTarget `json:"target,omitempty"`
	Text string `json:"text,omitempty"`
	Key string `json:"key,omitempty"`
	Coordinates *RecordXY `json:"coordinates,omitempty"`
	TimestampMs int64 `json:"timestamp_ms"`
	SnapshotBefore string `json:"snapshot_before,omitempty"`
}

// RecordTrace 是完整的操作录制记录。
type RecordTrace struct {
	Domain string `json:"domain"`
	StartURL string `json:"start_url"`
	StartTime time.Time `json:"start_time"`
	DurationMs int64 `json:"duration_ms"`
	Steps RecordStep `json:"steps"`
}

// ============================================================
// § RecordBuffer
// ============================================================

// RecordBuffer 接收 InputGateway 事件 tap，按步骤组装录制 trace。
// 线程安全: 所有公开方法均加锁。
type RecordBuffer struct {
	mu sync.Mutex
	recording bool
	trace RecordTrace
	startTime time.Time
	seq int
	snapshotFn func(x, y float64) string // 获取操作 target A11y 子树的回调 
}

// NewRecordBuffer 创建 RecordBuffer 实例（初始非录制状态）。
func NewRecordBuffer *RecordBuffer {
	return &RecordBuffer{}
}

// Start 开始录制，重置状态。连续两次 Start 会重置为新录制。
func (rb *RecordBuffer) Start(domain, url string) {
	rb.mu.Lock
	defer rb.mu.Unlock

	now := time.Now
	rb.recording = true
	rb.startTime = now
	rb.seq = 0
	rb.trace = RecordTrace{
		Domain: domain
		StartURL: url
		StartTime: now
		Steps: RecordStep{}
	}
}

// Stop 停止录制，计算 duration，返回 trace 快照。
// 未录制时返回空 trace，不 panic。
func (rb *RecordBuffer) Stop RecordTrace {
	rb.mu.Lock
	defer rb.mu.Unlock

	if !rb.recording {
		return RecordTrace{}
	}

	rb.recording = false
	rb.trace.DurationMs = time.Since(rb.startTime).Milliseconds

	// 返回 trace 副本
	snapshot := rb.trace
	snapshot.Steps = make(RecordStep, len(rb.trace.Steps))
	copy(snapshot.Steps, rb.trace.Steps)
	return snapshot
}

// IsRecording 返回当前是否正在录制。
func (rb *RecordBuffer) IsRecording bool {
	rb.mu.Lock
	defer rb.mu.Unlock
	return rb.recording
}

// SetSnapshotFn 注入 A11y 快照回调（: 操作 target 最小子树）。
func (rb *RecordBuffer) SetSnapshotFn(fn func(x, y float64) string) {
	rb.mu.Lock
	defer rb.mu.Unlock
	rb.snapshotFn = fn
}

// StepCount 返回当前已记录的 step 数量。
func (rb *RecordBuffer) StepCount int {
	rb.mu.Lock
	defer rb.mu.Unlock
	return len(rb.trace.Steps)
}

// ExportJSON 将当前 trace 序列化为 JSON。
func (rb *RecordBuffer) ExportJSON (byte, error) {
	rb.mu.Lock
	snapshot := rb.trace
	snapshot.Steps = make(RecordStep, len(rb.trace.Steps))
	copy(snapshot.Steps, rb.trace.Steps)
	rb.mu.Unlock

	return json.MarshalIndent(snapshot, "", " ")
}

// ============================================================
// § 事件 Tap（由 InputGateway dispatch 成功后调用）
// ============================================================

// AppendMouseEvent 处理鼠标事件 tap。
// 仅 mousePressed 转为 "click" step（mouseReleased/mouseMoved 忽略）。
// 调用前不需要持锁（内部加锁）。
func (rb *RecordBuffer) AppendMouseEvent(event *InputEvent) {
	rb.mu.Lock
	defer rb.mu.Unlock

	if !rb.recording {
		return
	}

	// 仅处理 mousePressed → "click"
	if event.Event != "mousePressed" {
		return
	}

	now := time.Now
	rb.seq++

	var snapshotBefore string
	if rb.snapshotFn != nil {
		snapshotBefore = rb.snapshotFn(event.X, event.Y)
	}

	step := RecordStep{
		Seq: rb.seq
		Action: "click"
		Coordinates: &RecordXY{
			X: event.X
			Y: event.Y
		}
		TimestampMs: now.UnixMilli
		SnapshotBefore: snapshotBefore
	}

	rb.trace.Steps = append(rb.trace.Steps, step)
}

// AppendKeyEvent 处理键盘事件 tap。
// keyDown 事件: 可打印字符 → 合并 "type"；特殊键 → "keypress"。
// 其他事件 (keyUp/char) 忽略。
// 调用前不需要持锁（内部加锁）。
func (rb *RecordBuffer) AppendKeyEvent(event *InputEvent) {
	rb.mu.Lock
	defer rb.mu.Unlock

	if !rb.recording {
		return
	}

	// 仅处理 keyDown
	if event.Event != "keyDown" {
		return
	}

	now := time.Now

	if isPrintableKey(event.Key) {
		// 可打印字符: 尝试合并到上一个 "type" step（2s 内）
		if len(rb.trace.Steps) > 0 {
			last := &rb.trace.Steps[len(rb.trace.Steps)-1]
			if last.Action == "type" && (now.UnixMilli-last.TimestampMs) < 2000 {
				last.Text += event.Key
				return
			}
		}
		// 新建 "type" step
		rb.seq++
		rb.trace.Steps = append(rb.trace.Steps, RecordStep{
			Seq: rb.seq
			Action: "type"
			Text: event.Key
			TimestampMs: now.UnixMilli
		})
	} else {
		// 特殊键: "keypress"
		rb.seq++
		rb.trace.Steps = append(rb.trace.Steps, RecordStep{
			Seq: rb.seq
			Action: "keypress"
			Key: event.Key
			TimestampMs: now.UnixMilli
		})
	}
}

// ============================================================
// § 内部工具
// ============================================================

// isPrintableKey 判断键是否为可打印字符（单个 Unicode 字符，可见非空白）。
// 特殊键 (Enter, Tab, Escape, Backspace, ArrowUp 等) 返回 false。
func isPrintableKey(key string) bool {
	if key == "" {
		return false
	}
	r, size := utf8.DecodeRuneInString(key)
	if r == utf8.RuneError {
		return false
	}
	// 必须恰好是一个 rune（多字符的 key 名如 "Enter" 包含多个字节）
	if size != len(key) {
		return false
	}
	// 可见字符: 排除控制字符和空白
	return r >= 0x20 && r != 0x7F
}
