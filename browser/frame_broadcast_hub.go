package browser

import "sync"

// ============================================================
// § FrameBroadcastHub [Ref: CAP-BS09-C3 r2]
// ============================================================

// FrameBroadcastHub 将 Screencast 帧 fan-out 给所有 WS subscriber。
//
// 设计要点:
//   - 每个 subscriber (WS 连接) 持有独立的 1-slot channel，避免单 channel 竞争消费 [CAP-BS09-C3]
//   - 保新丢旧: Publish 时 channel 满则替换为最新帧（非阻塞生产者）[TC-09-U-09]
//   - fan-out: 一帧广播给所有 subscriber，互不干扰
//   - close 安全: Unsubscribe 时安全关闭 channel，不会 panic
type FrameBroadcastHub struct {
	mu          sync.RWMutex
	subscribers map[string]chan *ScreencastFrame // connID → 1-slot channel
}

// NewFrameBroadcastHub 创建 FrameBroadcastHub 实例。
func NewFrameBroadcastHub() *FrameBroadcastHub {
	return &FrameBroadcastHub{
		subscribers: make(map[string]chan *ScreencastFrame),
	}
}

// Subscribe 为 connID 注册独立的 1-slot 帧 channel，返回 channel 本身（用于 Unsubscribe 匹配）。
// 重复注册同一 connID 会先关闭旧 channel 再创建新的（幂等注册）。
//
// 返回值是双向 channel，用于传给 Unsubscribe 做代次匹配。
// 读取端请用 <-chan *ScreencastFrame 类型接收。
func (h *FrameBroadcastHub) Subscribe(connID string) chan *ScreencastFrame {
	h.mu.Lock()
	defer h.mu.Unlock()

	// 旧 channel 存在则先关闭（避免旧连接的 defer Unsubscribe 关掉新 channel）
	if old, ok := h.subscribers[connID]; ok {
		close(old)
	}

	ch := make(chan *ScreencastFrame, 1)
	h.subscribers[connID] = ch
	return ch
}

// Unsubscribe 用 channel 指针做代次匹配，只关闭与传入 ch 相同的 channel。
// 若 connID 已被新连接替换（ch 不匹配），静默忽略，不影响新连接。
// 这是 SEV-H2 的关键修复：防止旧连接的 defer 关掉新连接的 channel。
func (h *FrameBroadcastHub) Unsubscribe(connID string, ch chan *ScreencastFrame) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// 代次匹配: 只有 channel 指针完全相同时才关闭
	if current, ok := h.subscribers[connID]; ok && current == ch {
		close(current)
		delete(h.subscribers, connID)
	}
	// 不匹配说明已被新连接替换，静默忽略
}

// Publish 将帧广播给所有 subscriber（非阻塞，保新丢旧）[TC-09-U-09]。
//
// 保新丢旧逻辑:
//  1. 尝试直接写入 channel
//  2. channel 满时先取出旧帧，再写入新帧
//     （内层 default 防止并发 Unsubscribe 导致 channel 被关闭后二次操作）
func (h *FrameBroadcastHub) Publish(frame *ScreencastFrame) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, ch := range h.subscribers {
		select {
		case ch <- frame:
		default:
			// channel 满 → 丢旧保新
			select {
			case <-ch: // 取出旧帧
			default:
			}
			// 再次尝试写入（此时 channel 应有空位）
			select {
			case ch <- frame:
			default:
			}
		}
	}
}

// Count 返回当前 subscriber 数量。
func (h *FrameBroadcastHub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers)
}
