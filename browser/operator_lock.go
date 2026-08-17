package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================
// § 单操作者 / 会话锁 — "一个会话同时只接受一个操作者"
// ============================================================
//
// observe→act 契约本来就隐含串行:agent 先看一眼拿到 @rN,再据此下手。两个操作者
// 同时对一个会话下手时,后手看到的页面已经不是他 observe 到的那一份 —— @rN 指向
// 的元素可能已被前手点没了。这不是"偶尔打架",而是契约被破坏。
//
// 机制选 flock 而不是 pidfile/lockdir:
//   - flock 由内核挂在打开的文件描述符上,进程一死(含 SIGKILL / OOM kill /断电重启后
//     的残留文件)锁自动释放。脏锁在设计上就不存在,不需要"锁过期"这类会自己出错的
//     补丁逻辑。
//   - 占有者身份写在锁旁边的 .owner 文件里,只为把错误信息说清楚(点名 pid)。它是
//     附注不是权威:锁的权威永远是"谁真的持着 flock"。所以释放时不删 owner 文件也
//     不影响正确性 —— 下一个人取到锁会立刻覆盖它。

// DefaultOperatorLockWait 是第二操作者的默认有界等待时长。
// 有界 = 要么拿到锁,要么响亮失败;绝不无限期挂着让调用方以为自己在正常工作。
const DefaultOperatorLockWait = 10 * time.Second

// operatorLockPollInterval 是等待期间的重试间隔。轮询而非阻塞 flock:阻塞式
// flock 没有超时参数,要做有界等待就得靠信号打断,那比重试脆弱得多。
const operatorLockPollInterval = 20 * time.Millisecond

// OperatorOwner 是锁旁 .owner 文件的内容 —— 只用于把"谁在操作"讲清楚。
type OperatorOwner struct {
	PID     int       `json:"pid"`
	Command string    `json:"cmd"`
	Since   time.Time `json:"since"`
}

// SessionBusyError 是有界等待耗尽后的响亮失败。它点名占有者,让第二操作者
// 一眼看出"不是工具坏了,是有人正在操作这个会话"。
type SessionBusyError struct {
	SessionID  string
	Owner      OperatorOwner
	OwnerKnown bool
	Waited     time.Duration
}

func (e *SessionBusyError) Error() string {
	if e == nil {
		return "session busy"
	}
	if !e.OwnerKnown {
		return fmt.Sprintf("session %s 正被另一个进程操作(占有者未留下身份记录);一个会话同时只接受一个操作者"+
			" —— 已等 %s(--lock-wait 可调)", e.SessionID, e.Waited.Round(time.Millisecond))
	}
	return fmt.Sprintf("session %s 正被 pid %d(%s,自 %s)操作;一个会话同时只接受一个操作者"+
		" —— 已等 %s(--lock-wait 可调)",
		e.SessionID, e.Owner.PID, e.Owner.Command, e.Owner.Since.Format("15:04:05"),
		e.Waited.Round(time.Millisecond))
}

// OperatorLock 是一次 CLI 调用对某个会话的排他持有。
type OperatorLock struct {
	sessionID string
	lease     exclusiveFileLock
	ownerPath string
}

// SessionOperatorLockPath 是会话锁文件路径(与 session json 同目录,便于 gc 一起看)。
func SessionOperatorLockPath(sessionID string) string {
	return filepath.Join(sessionsDir, sessionID+".oplock")
}

func sessionOperatorOwnerPath(sessionID string) string {
	return filepath.Join(sessionsDir, sessionID+".owner")
}

// AcquireSessionOperatorLock 取会话排他锁,最多等 wait。
//
// 拿到锁后立刻把占有者身份写进 .owner;拿不到就读对方的 .owner 组织错误信息。
// 返回的 *OperatorLock 必须被调用方持有到命令结束 —— 不是为了 Release(进程退出
// 内核就放锁),而是防止 GC 回收 *os.File 触发 finalizer 提前关掉 fd。
func AcquireSessionOperatorLock(sessionID, commandSummary string, wait time.Duration) (*OperatorLock, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("operator lock: empty session id")
	}
	if wait < 0 {
		wait = 0
	}
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		return nil, fmt.Errorf("operator lock: mkdir %s: %w", sessionsDir, err)
	}
	path := SessionOperatorLockPath(sessionID)

	started := time.Now()
	lease, acquired, err := acquireExclusiveFileLock(path, wait)
	if err != nil {
		return nil, fmt.Errorf("operator lock: %s: %w", path, err)
	}
	if !acquired {
		owner, ok := readOperatorOwner(sessionOperatorOwnerPath(sessionID))
		return nil, &SessionBusyError{
			SessionID:  sessionID,
			Owner:      owner,
			OwnerKnown: ok,
			Waited:     time.Since(started),
		}
	}

	lock := &OperatorLock{sessionID: sessionID, lease: lease, ownerPath: sessionOperatorOwnerPath(sessionID)}
	lock.writeOwner(OperatorOwner{PID: os.Getpid(), Command: commandSummary, Since: time.Now()})
	return lock, nil
}

// Release 主动放锁。进程退出时内核也会放,所以这里的失败不值得升级为错误 ——
// 唯一的作用是让同一进程内的后续调用(测试)能立刻再取到。
func (l *OperatorLock) Release() {
	if l == nil || l.lease == nil {
		return
	}
	_ = l.lease.Close()
	l.lease = nil
}

// SessionID 让调用方在日志里说清自己锁的是谁。
func (l *OperatorLock) SessionID() string {
	if l == nil {
		return ""
	}
	return l.sessionID
}

func (l *OperatorLock) writeOwner(owner OperatorOwner) {
	data, err := json.Marshal(owner)
	if err != nil {
		return
	}
	// owner 只是附注,写失败不影响锁的正确性 —— 最坏是对方的错误信息少一行身份。
	_ = os.WriteFile(l.ownerPath, data, 0600)
}

func readOperatorOwner(path string) (OperatorOwner, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return OperatorOwner{}, false
	}
	var owner OperatorOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return OperatorOwner{}, false
	}
	if owner.PID <= 0 {
		return OperatorOwner{}, false
	}
	// owner 文件是上一任留下的也有可能(释放不删)。持锁才是权威,所以这里只做一层
	// 廉价校验:进程都不在了,这条身份就不该拿去误导人。
	if !processAlive(owner.PID) {
		return OperatorOwner{}, false
	}
	return owner, true
}

// SummarizeOperatorCommand 把 argv 压成一行可读摘要放进 .owner。
//
// 刻意做两件事:截断(错误信息里不该出现半屏 JS),以及给 fillsecret 的值打码 ——
// owner 文件的内容会原样打印到另一个操作者的终端上,密码不该走这条路。
func SummarizeOperatorCommand(args []string) string {
	parts := make([]string, 0, len(args))
	maskNext := false
	for _, arg := range args {
		if maskNext {
			parts = append(parts, "***")
			maskNext = false
			continue
		}
		low := strings.ToLower(strings.TrimSpace(arg))
		if strings.HasPrefix(low, "fillsecret") {
			// 动作串本身就形如 `fillsecret @r3 'pw'` —— 整条打码只留动词。
			parts = append(parts, "fillsecret ***")
			continue
		}
		if low == "--password" || low == "--secret" || low == "--token" {
			parts = append(parts, arg)
			maskNext = true
			continue
		}
		parts = append(parts, arg)
	}
	summary := strings.Join(parts, " ")
	return clampOperatorSummary(summary, 96)
}

func clampOperatorSummary(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}
