package browser

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================
// § 浏览器级输入临界区 — 共享同一个浏览器实例的两个会话之间
// ============================================================
//
// 会话锁保证"一个会话只有一个操作者",但两个会话可能坐在同一个浏览器实例上
// (mux-host 多 tab)。此时输入是有全局状态的:bringToFront 改的是"哪个 target 收
// 输入",鼠标坐标落在哪个 tab 上也由它决定。A 把自己的 tab 提到前台、还没把
// click 发出去,B 就把前台抢走 —— A 的 click 就打在 B 的页面上了。
//
// 所以在浏览器实例这一层再加一把锁,但只在输入派发窗口
// (bringToFront → dispatch → 等 ack)持有,毫秒级。它不是"会话串行化"(那是第一把
// 锁的事),只保证前台归属在一次输入的生命周期内不被抢走。

// browserInputLocksDir 是浏览器级锁与前台账本的存放处。
// 用 /tmp 而不是 os.TempDir():TMPDIR 因人而异,不是跨进程的同一命名空间。
const browserInputLocksDir = "/tmp/dw-browser-input-locks"

const (
	// DefaultBrowserInputLockWait 是输入临界区的等待上界。临界区本身是毫秒级,
	// 等这么久基本只会发生在对方卡住的时候 —— 那时响亮失败比闷头派发更有用。
	DefaultBrowserInputLockWait = 8 * time.Second

	// frontFlipWindow / frontFlipThreshold 定义"前台目标反复切换"。
	// 阈值 4 次 / 5 秒:正常单操作者在一个 tab 上连续动作时根本不会触发
	// (已经在前台就不切),只有两个操作者互相把对方的 tab 顶下去才会。
	frontFlipWindow    = 5 * time.Second
	frontFlipThreshold = 4
)

// BrowserInputKey 把"哪个浏览器实例"压成一个跨进程稳定的键。
// 优先 ws_url(同一浏览器的所有连接串一致);拿不到就退到 chrome pid。
func BrowserInputKey(wsURL string, chromePID int) string {
	if s := strings.TrimSpace(wsURL); s != "" {
		return "ws:" + s
	}
	if chromePID > 0 {
		return fmt.Sprintf("pid:%d", chromePID)
	}
	return ""
}

func browserInputLockPath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(browserInputLocksDir, "input-"+hex.EncodeToString(sum[:8])+".lock")
}

func browserFrontLedgerPath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(browserInputLocksDir, "input-"+hex.EncodeToString(sum[:8])+".front.json")
}

// BrowserInputLock 是一次输入派发窗口对某个浏览器实例的排他持有。
type BrowserInputLock struct {
	key   string
	lease exclusiveFileLock
}

// AcquireBrowserInputLock 进入输入临界区。key 为空(拿不到 ws_url 也没有 pid)时
// 返回 nil 锁并放行 —— 键都构造不出来的场合谈不上"共享同一浏览器",不该因为
// 记账问题挡住真实输入。
func AcquireBrowserInputLock(key string, wait time.Duration) (*BrowserInputLock, error) {
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	if err := os.MkdirAll(browserInputLocksDir, 0755); err != nil {
		return nil, fmt.Errorf("browser input lock: mkdir %s: %w", browserInputLocksDir, err)
	}
	path := browserInputLockPath(key)
	lease, acquired, err := acquireExclusiveFileLock(path, wait)
	if err != nil {
		return nil, fmt.Errorf("browser input lock: %s: %w", path, err)
	}
	if !acquired {
		return nil, fmt.Errorf("browser input lock: 另一个操作者已持有该浏览器实例的输入临界区超过 %s"+
			"(共享浏览器的输入必须串行);本次派发放弃,请重试", wait.Round(time.Millisecond))
	}
	return &BrowserInputLock{key: key, lease: lease}, nil
}

// Release 退出输入临界区。
func (l *BrowserInputLock) Release() {
	if l == nil || l.lease == nil {
		return
	}
	_ = l.lease.Close()
	l.lease = nil
}

// frontEvent 是一次真实的前台切换。
type frontEvent struct {
	Target string    `json:"target"`
	PID    int       `json:"pid"`
	At     time.Time `json:"at"`
}

// RecordBringToFront 把一次真实的前台切换记进账本,并回答"是不是在互抢前台"。
//
// 只在持有输入临界区锁时调用 —— 读-改-写因此天然互斥,不需要第二把锁。
// 返回非空字符串 = 检测到多操作者互扰,内容是给人看的 WARN 正文。
func RecordBringToFront(key, targetID string) string {
	if strings.TrimSpace(key) == "" {
		return ""
	}
	path := browserFrontLedgerPath(key)
	events := readFrontLedger(path)
	now := time.Now()
	events = append(pruneFrontEvents(events, now), frontEvent{Target: targetID, PID: os.Getpid(), At: now})
	writeFrontLedger(path, events)
	return frontFlipWarning(events)
}

// frontFlipWarning 判定"前台目标反复切换"。
//
// 三个条件:窗口内切换够密(≥ 阈值)、跨度在窗口内、且切换涉及不止一方
// (不止一个 target 或不止一个进程)。
//
// "不止一方"这一条是防误报的关键,而它必须同时认 target 和 pid 两种形态:
//   - 两个操作者各开各的 tab ⇒ target 不同(最直观的形态);
//   - 两个操作者盯着同一个 tab,或者一方开的是我们看不到 target id 的窗口 ⇒
//     target 相同但 pid 不同。实测就是后一种:同一个 tab 在 0.7s 内被四个不同的
//     CLI 进程各自重新提了一次前台。只认 target 会把这种最常见的形态漏掉。
//
// 为什么不会误报单操作者:一次 bringToFront 成功之后 target 就在前台了,同一个人
// 的后续动作探到 visible 直接跳过,根本不记账。要在 5s 内攒够 4 条记录,前台必须
// 被别人抢走 4 次 —— 那正是我们要说的事。
func frontFlipWarning(events []frontEvent) string {
	if len(events) < frontFlipThreshold {
		return ""
	}
	recent := events[len(events)-frontFlipThreshold:]
	distinctTargets := map[string]bool{}
	distinctPIDs := map[int]bool{}
	for _, e := range recent {
		distinctTargets[e.Target] = true
		distinctPIDs[e.PID] = true
	}
	if len(distinctTargets) < 2 && len(distinctPIDs) < 2 {
		return ""
	}
	span := recent[len(recent)-1].At.Sub(recent[0].At)
	if span > frontFlipWindow {
		return ""
	}
	return fmt.Sprintf("多操作者共享浏览器,输入已串行化但节奏互扰:%s 内前台被重新抢回 %d 次"+
		"(%d 个 target / %d 个进程) —— 每次输入前都要把自己的 tab 重新提到前台,吞吐和时序都会变差",
		span.Round(time.Millisecond), len(recent), len(distinctTargets), len(distinctPIDs))
}

func pruneFrontEvents(events []frontEvent, now time.Time) []frontEvent {
	cutoff := now.Add(-frontFlipWindow)
	kept := events[:0]
	for _, e := range events {
		if e.At.After(cutoff) {
			kept = append(kept, e)
		}
	}
	// 账本只服务一个短窗口判定,不该无限长。
	if len(kept) > 32 {
		kept = kept[len(kept)-32:]
	}
	return kept
}

func readFrontLedger(path string) []frontEvent {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var events []frontEvent
	if err := json.Unmarshal(data, &events); err != nil {
		return nil
	}
	return events
}

func writeFrontLedger(path string, events []frontEvent) {
	data, err := json.Marshal(events)
	if err != nil {
		return
	}
	// 账本是诊断附注,写失败只让 WARN 少一次,不该影响输入本身。
	_ = os.WriteFile(path, data, 0600)
}
