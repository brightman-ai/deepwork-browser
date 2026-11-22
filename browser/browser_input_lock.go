package browser

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
//
// # 持有凭证(输入窗口所有权显式化)
//
// *BrowserInputLock 既是锁,也是"我正在窗口内"的凭证。规则只有一条:
//
//	凡是在窗口内执行的函数,签名必须带 w *BrowserInputLock,先 w.Covers(自己的 key),
//	绝不调用 Acquire;窗口外入口 = Acquire + 调窗口内版本,两者成对出现。
//
// 为什么不靠 xxxLocked 命名约定:"自己取锁还是要求调用方已持锁"一旦经过闭包/钩子
// 跨组件(engine → tracker),命名携带的信息就丢了 —— headed 附着激活闭包正是这样
// 在窗口内调了"窗口外入口",同进程第二次 open 同一个锁文件,flock 按打开的文件描述
// 判冲突,自己等自己 8s。凭证作为参数传递后,这类调用在类型上就写不出来。
//
// 为什么不做"同进程立即失败"的隐式重入检测:tracker 的事件 worker、关闭 target 的
// goroutine 与动作本身在同一进程里是合法的并发等待,Go 没有 goroutine 身份,进程级
// 判定会误伤它们。结构性防线交给凭证,超时诊断(见 describeInputLockTimeout)只负责
// 说真话:是本进程占着、是别的进程占着、还是查不到。

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

func browserInputOwnerPath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(browserInputLocksDir, "input-"+hex.EncodeToString(sum[:8])+".owner")
}

// ErrInputWindowNotHeld:窗口内函数要求的凭证缺失、已释放,或属于另一个浏览器实例。
// 这是调用方的缺陷(把窗口内函数用在了窗口外,或 engine/tracker 绑错了 key),不是并发争用。
var ErrInputWindowNotHeld = errors.New("browser input window not held")

// ErrInputWindowReentrant:取锁等待耗尽,而持有者就是本进程——疑似同进程重入(同一
// goroutine 等自己持有的窗口永远不会成功),也可能只是本进程内的另一个持有者卡住、
// 超过了等待上限。Go 没有 goroutine 身份,两者无法区分;错误体见 InputWindowReentrantError。
var ErrInputWindowReentrant = errors.New("browser input window re-entered by the holding process")

// InputWindowOwner 是输入窗口旁 .owner 文件的内容 —— 只用于把"谁占着"讲清楚。
// Command 来自 SetInputWindowCommand,只含命令名与 --id,绝不含用户文本参数
// (如 act fill --text);.owner 会原样出现在另一个进程的报错里。
type InputWindowOwner struct {
	PID     int       `json:"pid"`
	Command string    `json:"cmd,omitempty"`
	Op      string    `json:"op"`
	Since   time.Time `json:"since"`
}

// BrowserInputBusyError:有界等待耗尽,且持有者是另一个进程(或查不到持有者)。
type BrowserInputBusyError struct {
	Op         string // 申请方的 op
	Owner      InputWindowOwner
	OwnerKnown bool
	Waited     time.Duration
}

func (e *BrowserInputBusyError) Error() string {
	if e == nil {
		return "browser input lock: busy"
	}
	if !e.OwnerKnown {
		return fmt.Sprintf("browser input lock: 另一个操作者已持有该浏览器实例的输入临界区超过 %s"+
			"(共享浏览器的输入必须串行;占有者未留下身份记录);%s 放弃,请重试",
			e.Waited.Round(time.Millisecond), e.Op)
	}
	cmd := ""
	if e.Owner.Command != "" {
		cmd = "," + e.Owner.Command
	}
	return fmt.Sprintf("browser input lock: 该浏览器实例的输入临界区被 pid %d(op=%s%s)持有,自 %s,已等 %s"+
		"(共享浏览器的输入必须串行);%s 放弃,请重试",
		e.Owner.PID, e.Owner.Op, cmd, e.Owner.Since.Format("15:04:05.000"),
		e.Waited.Round(time.Millisecond), e.Op)
}

// InputWindowReentrantError 同时满足 errors.Is(err, ErrInputWindowReentrant)。
type InputWindowReentrantError struct {
	HolderOp    string
	HeldFor     time.Duration
	RequesterOp string
	Waited      time.Duration
}

func (e *InputWindowReentrantError) Error() string {
	if e == nil {
		return ErrInputWindowReentrant.Error()
	}
	// 措辞刻意不比证据更强:Go 没有 goroutine 身份,我们只知道"持有者是本进程",
	// 不知道申请者是不是持有者自己。两种情形都落在这条消息里,所以两种都要说。
	return fmt.Sprintf("browser input lock: 本进程内已持有该浏览器实例的输入临界区(op=%q,已持有 %s),"+
		"%q 又来申请并等了 %s 未果 —— 疑似同进程重入(窗口内的代码应当接收 *BrowserInputLock 凭证,而不是再次取锁),"+
		"也可能是本进程内的持有者卡住、超过了等待上限;此处无法区分这两种情形(但不是另一个操作者占着浏览器)",
		e.HolderOp, e.HeldFor.Round(time.Millisecond), e.RequesterOp, e.Waited.Round(time.Millisecond))
}

func (e *InputWindowReentrantError) Is(target error) bool { return target == ErrInputWindowReentrant }

// BrowserInputLock 是一次输入派发窗口对某个浏览器实例的排他持有,同时是窗口内代码
// 必须显式接收的"持有凭证"(见文件头)。
type BrowserInputLock struct {
	key       string
	op        string
	since     time.Time
	ownerPath string

	mu    sync.Mutex
	lease exclusiveFileLock
}

// 本进程当前持有的窗口(key → 凭证)。只为超时诊断服务:flock 才是权威。
var inProcessInputWindows = struct {
	sync.Mutex
	held map[string]*BrowserInputLock
}{held: map[string]*BrowserInputLock{}}

// inProcessInputWindowContention 统计"申请开始时本进程已持有同一 key"的次数。
// 生产里它可能是合法的并发等待(事件 worker 等动作窗口);受控测试里(单 goroutine
// 持窗口、逐个执行窗口内钩子)它必须为 0 —— 那就是"同进程重入次数"。
var inProcessInputWindowContention atomic.Int64

// 本进程的命令摘要,由 CLI 入口设置一次,随 .owner 一起写出。
var inputWindowCommand atomic.Value // string

// SetInputWindowCommand 登记本进程的命令摘要(如 "act click --id s1")。
// 调用方负责保证摘要不含用户文本(只许命令名与 --id);这里再按 96 字符截断。
func SetInputWindowCommand(summary string) {
	inputWindowCommand.Store(clampOperatorSummary(strings.TrimSpace(summary), 96))
}

func currentInputWindowCommand() string {
	if v, ok := inputWindowCommand.Load().(string); ok {
		return v
	}
	return ""
}

// AcquireBrowserInputLock 进入输入临界区(窗口外入口)。key 为空(拿不到 ws_url 也没有
// pid)时返回 nil 锁并放行 —— 键都构造不出来的场合谈不上"共享同一浏览器",不该因为
// 记账问题挡住真实输入。
//
// op 标明申请方(如 action.dispatch、tracker.activate、cli.tabs.select),只用于
// .owner 与超时报错的诊断,不参与互斥。
//
// 窗口内的代码不得调用本函数:它们接收返回的 *BrowserInputLock 并 Covers。
func AcquireBrowserInputLock(key, op string, wait time.Duration) (*BrowserInputLock, error) {
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	if strings.TrimSpace(op) == "" {
		op = "unspecified"
	}
	if err := os.MkdirAll(browserInputLocksDir, 0755); err != nil {
		return nil, fmt.Errorf("browser input lock: mkdir %s: %w", browserInputLocksDir, err)
	}
	path := browserInputLockPath(key)
	if inProcessHolder(key) != nil {
		inProcessInputWindowContention.Add(1)
	}
	started := time.Now()
	lease, acquired, err := acquireExclusiveFileLock(path, wait)
	if err != nil {
		return nil, fmt.Errorf("browser input lock: %s: %w", path, err)
	}
	if !acquired {
		return nil, describeInputLockTimeout(key, op, time.Since(started))
	}
	l := &BrowserInputLock{key: key, op: op, since: time.Now(), ownerPath: browserInputOwnerPath(key), lease: lease}
	inProcessInputWindows.Lock()
	inProcessInputWindows.held[key] = l
	inProcessInputWindows.Unlock()
	l.writeOwner()
	return l, nil
}

func inProcessHolder(key string) *BrowserInputLock {
	inProcessInputWindows.Lock()
	defer inProcessInputWindows.Unlock()
	return inProcessInputWindows.held[key]
}

// describeInputLockTimeout 把"等满了仍被占"翻成真话:本进程占着 / 别的进程占着 / 查不到。
func describeInputLockTimeout(key, op string, waited time.Duration) error {
	if holder := inProcessHolder(key); holder != nil {
		return &InputWindowReentrantError{
			HolderOp: holder.op, HeldFor: time.Since(holder.since), RequesterOp: op, Waited: waited,
		}
	}
	owner, ok := readInputWindowOwner(browserInputOwnerPath(key))
	return &BrowserInputBusyError{Op: op, Owner: owner, OwnerKnown: ok, Waited: waited}
}

func (l *BrowserInputLock) writeOwner() {
	data, err := json.Marshal(InputWindowOwner{PID: os.Getpid(), Command: currentInputWindowCommand(), Op: l.op, Since: l.since})
	if err != nil {
		return
	}
	// 附注,写失败最多让对方的报错少一行身份,不影响锁的正确性。
	_ = os.WriteFile(l.ownerPath, data, 0600)
}

// removeInputWindowOwner 只删除"本 key 的 .owner 单文件":路径必须恰好等于
// browserInputOwnerPath(key)(即锁目录下的 input-<hash>.owner),否则什么都不删。
// os.Remove 不递归,最坏情况也只影响这一个文件。
func removeInputWindowOwner(key, path string) {
	if path == "" || path != browserInputOwnerPath(key) ||
		filepath.Dir(path) != browserInputLocksDir || !strings.HasSuffix(path, ".owner") {
		return
	}
	_ = os.Remove(path)
}

func readInputWindowOwner(path string) (InputWindowOwner, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return InputWindowOwner{}, false
	}
	var owner InputWindowOwner
	if err := json.Unmarshal(data, &owner); err != nil || owner.PID <= 0 {
		return InputWindowOwner{}, false
	}
	// 持锁才是权威。释放时我们在放锁前清掉 .owner,所以残留只来自被 SIGKILL 的持有者;
	// 进程已不在,这条身份就不该拿去误导人。
	if !processAlive(owner.PID) {
		return InputWindowOwner{}, false
	}
	return owner, true
}

// Covers 校验"这把凭证确实覆盖 key 对应的浏览器实例输入窗口"。窗口内函数的第一行。
//
// 凭证为 nil、已释放、或属于另一个 key → ErrInputWindowNotHeld。key 不同还意味着
// 调用链两端(如 engine 与 tracker)绑定了不同的运行时 key,等于互斥失效,同样要明确报错。
//
// 例外:key 为空 = 构造不出键,输入临界区本就退化为不加锁(见 AcquireBrowserInputLock
// 返回 nil 锁),此时没有窗口可言,nil 凭证视为覆盖。
func (l *BrowserInputLock) Covers(key string) error {
	if strings.TrimSpace(key) == "" {
		if l == nil {
			return nil
		}
		return fmt.Errorf("%w: credential is for %q but this function's runtime key is empty", ErrInputWindowNotHeld, l.key)
	}
	if l == nil {
		return fmt.Errorf("%w: no credential passed for %q (a function that runs inside the input window must receive it, never acquire)", ErrInputWindowNotHeld, key)
	}
	l.mu.Lock()
	held := l.lease != nil
	l.mu.Unlock()
	if !held {
		return fmt.Errorf("%w: credential for %q (op=%s) was already released", ErrInputWindowNotHeld, l.key, l.op)
	}
	if l.key != key {
		return fmt.Errorf("%w: credential is for %q (op=%s) but this function runs on %q (engine and tracker bound to different runtime keys)", ErrInputWindowNotHeld, l.key, l.op, key)
	}
	return nil
}

// Op 返回取得窗口时登记的 op(诊断用)。
func (l *BrowserInputLock) Op() string {
	if l == nil {
		return ""
	}
	return l.op
}

// Release 退出输入临界区。
func (l *BrowserInputLock) Release() {
	if l == nil {
		return
	}
	l.mu.Lock()
	lease := l.lease
	l.lease = nil
	l.mu.Unlock()
	if lease == nil {
		return
	}
	// 仍持有 flock 时先清登记与 .owner:此刻没有别的写者,清完再放锁,下一任看到的
	// 要么是空、要么是自己写的身份,不会读到上一任的残留。
	inProcessInputWindows.Lock()
	if inProcessInputWindows.held[l.key] == l {
		delete(inProcessInputWindows.held, l.key)
	}
	inProcessInputWindows.Unlock()
	removeInputWindowOwner(l.key, l.ownerPath)
	_ = lease.Close()
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
