// Package browser — P1：Chrome 的正常退出。
//
// 为什么需要它：Chrome 约每 30s 才把 Cookie 库批量提交一次，对进程组 SIGKILL 会丢掉未提交的部分，
// Preferences 的 exit_type 也被记成异常退出。所以"close"必须先让 Chrome 自己退出，再考虑强杀。
//
// 三步，每一步都有各自的等待上限，回执如实写明最后是哪一步让进程消失的：
//
//  1. CDP Browser.close   → 等进程自己退出（≤ ChromeShutdownGracefulWait）  → mode=graceful
//  2. 对进程组 SIGTERM     → 等进程退出（≤ ChromeShutdownTermWait）           → mode=terminated
//  3. 对进程组 SIGKILL     → 等进程消失                                       → mode=killed
//
// 另有 mode=gone：close 到达时进程已经不在了（崩溃或被外部终止）。我们没有做任何退出动作，
// 也就不承诺任何落盘——它不是 graceful。只有 mode=graceful 才承诺 profile 状态已完整落盘。
//
// 策略：需要保留状态的（持久 profile）走三步；没有要保留的状态的（临时 profile，会随会话删除）
// 与 --force 直接走第 3 步。本文件只负责"让进程消失"；封存（SealProfile）在进程消失之后由调用方做。
package browser

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/chromedp/cdproto"
	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
)

// 回执里的 shutdown.mode 取值。
const (
	ShutdownGraceful   = "graceful"   // Browser.close 之后进程自己退出，状态完整落盘
	ShutdownTerminated = "terminated" // Browser.close 没能让它退出，SIGTERM 之后退出了
	ShutdownKilled     = "killed"     // 直接强杀，或前两步都没能让它退出
	ShutdownGone       = "gone"       // close 到达时进程已不在；没做任何退出动作
)

// 各步等待上限。等待是"免费"的：Chrome 健康时几百毫秒就退出了，用不满这段时间，只有挂死的 Chrome 才会等满；
// 而持久 profile 的落盘完整性（Cookie 等状态能不能保住）优先于关闭耗时。
//
//   - Browser.close 之后等 8s：原设计值是 3s，但在满载机器上（全量测试与别的进程同时跑 Chrome）实测过
//     Chrome 要 4.1s 才退出，3s 会把一次正常但稍慢的退出误杀成 killed，状态就丢了。8s 约留 2 倍余量。
//   - SIGTERM 之后再等 2s：SIGTERM 对 Chrome 同样是"正常退出"，给它收尾的时间；原设计值 1s。
//   - 再不退出才 SIGKILL，并等进程消失。
const (
	// ChromeShutdownGracefulWait 是发出 Browser.close 之后等 Chrome 自己退出的上限。
	ChromeShutdownGracefulWait = 8 * time.Second
	// ChromeShutdownTermWait 是发出 SIGTERM 之后等 Chrome 退出的上限。
	ChromeShutdownTermWait = 2 * time.Second
	// ChromeShutdownKillWait 是强杀之后等进程消失的上限（目标没有自带 Kill 时使用；ChromeHandle.Kill 自带 5s）。
	ChromeShutdownKillWait = 5 * time.Second
	// chromeShutdownSealBudget 是关闭之后封存持久 profile 允许占用的时间：取封存锁最长等 30s，加上打包、加密、
	// 整体解密自校验（与 profile 体积成正比）。
	chromeShutdownSealBudget = 2 * time.Minute

	// BrowserMuxHostShutdownRequestTimeout 是等 mux host 同步完成"正常退出 → SIGTERM → 强杀 → 封存"的上限，
	// 由各步上限相加得出——外层超时必须装得下内层全部步骤，否则会出现"CLI 已经超时报错，host 里还在封存"。
	BrowserMuxHostShutdownRequestTimeout = ChromeShutdownGracefulWait + ChromeShutdownTermWait + ChromeShutdownKillWait + chromeShutdownSealBudget
	// BrowserMuxHostShutdownCallerTimeout 是 CLI 为这一次请求设的 context 超时：比 HTTP 客户端超时再多一点余量，
	// 让客户端自己的超时错误（带原因）先于 context 取消出现。
	BrowserMuxHostShutdownCallerTimeout = BrowserMuxHostShutdownRequestTimeout + 10*time.Second
)

// ShutdownReceipt 是一次关闭的回执。
type ShutdownReceipt struct {
	Mode   string `json:"mode"`             // graceful | terminated | killed | gone
	Ms     int64  `json:"ms"`               // 从开始关闭到进程消失
	Reason string `json:"reason,omitempty"` // 为什么不是 graceful：forced / ephemeral-profile / 具体失败
	Error  string `json:"error,omitempty"`  // 强杀自己报的错（进程没能确认消失）
}

// ChromeShutdownPolicy 决定走哪几步。
type ChromeShutdownPolicy struct {
	// Graceful=true：Browser.close → SIGTERM → SIGKILL；false：直接 SIGKILL。
	Graceful bool
	// Reason 在 Graceful=false 时写进回执，说明为什么没有尝试正常退出。
	Reason string

	// 各步等待上限；0 取默认值。
	GracefulWait time.Duration
	TermWait     time.Duration
	KillWait     time.Duration
}

// ChromeShutdownTarget 描述要关闭的那个 Chrome。
type ChromeShutdownTarget struct {
	PID int
	// PGID 是启动时记下的进程组 ID。0 表示现在读——前提是 leader 还活着；leader 退出后就读不到了。
	PGID int
	// WSURL 是浏览器级 CDP 端点（ws://…/devtools/browser/…）。空则跳过 Browser.close，直接 SIGTERM。
	WSURL string
	// Done 在 leader 退出时关闭（我们是它的父进程时才有）。nil 则按 PID 轮询存活。
	Done <-chan struct{}
	// Kill 是最后一步的强杀；它应当等进程退出后再返回（如 ChromeHandle.Kill）。nil 则用进程组 SIGKILL 并轮询。
	Kill func() error
}

func (t ChromeShutdownTarget) exited() bool {
	if t.Done != nil {
		select {
		case <-t.Done:
			return true
		default:
			return false
		}
	}
	return !isPIDAlive(t.PID)
}

func (t ChromeShutdownTarget) waitExit(d time.Duration) bool {
	if d <= 0 {
		return t.exited()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	if t.Done != nil {
		select {
		case <-t.Done:
			return true
		case <-timer.C:
			return false
		}
	}
	tick := time.NewTicker(ProcessExitPollInterval)
	defer tick.Stop()
	for {
		if !isPIDAlive(t.PID) {
			return true
		}
		select {
		case <-timer.C:
			return !isPIDAlive(t.PID)
		case <-tick.C:
		}
	}
}

// ShutdownChrome 按 policy 让 target 消失，并如实回报是怎么消失的。
func ShutdownChrome(t ChromeShutdownTarget, p ChromeShutdownPolicy) ShutdownReceipt {
	start := time.Now()
	if p.GracefulWait <= 0 {
		p.GracefulWait = ChromeShutdownGracefulWait
	}
	if p.TermWait <= 0 {
		p.TermWait = ChromeShutdownTermWait
	}
	if p.KillWait <= 0 {
		p.KillWait = ChromeShutdownKillWait
	}
	rc := ShutdownReceipt{}
	finish := func(mode string) ShutdownReceipt {
		rc.Mode = mode
		rc.Ms = time.Since(start).Milliseconds()
		return rc
	}

	// 进程组必须趁 leader 还活着读：leader 一退出 getpgid 就失败，幸存的 zygote/renderer 就够不着了。
	pgid := t.PGID
	if pgid <= 0 {
		pgid = ChromeProcessGroupID(t.PID)
	}
	// 只对"我们自己启动时建立的那个组"发信号：组长就是这个 PID。
	ownGroup := pgid > 0 && pgid == t.PID
	sweep := func() {
		if ownGroup {
			KillChromeProcessGroupID(pgid) // leader 已走，清掉可能还在写 profile 的帮手进程
		}
	}

	// 第 3 步（也是 !Graceful 时的唯一一步）：强杀。
	kill := func() ShutdownReceipt {
		var err error
		if t.Kill != nil {
			err = t.Kill()
		} else {
			KillChromeProcessGroup(t.PID)
			if !t.waitExit(p.KillWait) {
				err = fmt.Errorf("chrome pid=%d still alive %s after SIGKILL", t.PID, p.KillWait)
			}
		}
		sweep()
		out := finish(ShutdownKilled)
		if err != nil {
			out.Error = err.Error()
		}
		return out
	}

	if t.exited() {
		sweep()
		return finish(ShutdownGone)
	}
	if !p.Graceful {
		rc.Reason = p.Reason
		if rc.Reason == "" {
			rc.Reason = "forced"
		}
		return kill()
	}

	// 第 1 步：Browser.close。
	closeSent := false
	if t.WSURL == "" {
		rc.Reason = "no CDP endpoint to send Browser.close to"
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), p.GracefulWait)
		err := sendBrowserClose(ctx, t.WSURL)
		cancel()
		if err != nil {
			rc.Reason = "Browser.close failed: " + err.Error()
		} else {
			closeSent = true
		}
	}
	if closeSent {
		if t.waitExit(p.GracefulWait) {
			sweep()
			return finish(ShutdownGraceful)
		}
		rc.Reason = fmt.Sprintf("did not exit within %s of Browser.close", p.GracefulWait)
	}

	// 第 2 步：SIGTERM。Windows 没有 SIGTERM（killPIDGraceful 在那里就是强杀），所以跳过，
	// 不能把一次强杀记成"terminated"。
	if runtime.GOOS != "windows" {
		if !(ownGroup && TerminateChromeProcessGroupID(pgid)) {
			_ = killPIDGraceful(t.PID)
		}
		if t.waitExit(p.TermWait) {
			sweep()
			return finish(ShutdownTerminated)
		}
		if rc.Reason != "" {
			rc.Reason += "; "
		}
		rc.Reason += fmt.Sprintf("did not exit within %s of SIGTERM", p.TermWait)
	}

	// 第 3 步：强杀。
	return kill()
}

// WaitForProcessExit 等 pid 消失（不发任何信号）；返回是否在 timeout 内消失。
// 用于"别人已经在关它了，我们只需要知道它什么时候真的没了"的场合。
func WaitForProcessExit(pid int, timeout time.Duration) bool {
	return ChromeShutdownTarget{PID: pid}.waitExit(timeout)
}

// sendBrowserClose 在浏览器级 CDP 端点上发一条 Browser.close，并等到回应或连接被 Chrome 收掉。
// Chrome 退出时常常在回应之前就把 socket 关了——写成功之后连接断开算"已被接受"。
// chromedp 的 Transport.Read 不看 ctx，所以超时靠关连接来兑现。
func sendBrowserClose(ctx context.Context, wsURL string) error {
	conn, err := chromedp.DialContext(ctx, wsURL)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("dial: %w", err)
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	defer func() { close(stop); _ = conn.Close(); <-stopped }()

	const msgID = 1
	if err := conn.Write(ctx, &cdproto.Message{ID: msgID, Method: cdproto.MethodType(cdpbrowser.CommandClose)}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("write: %w", err)
	}
	for {
		var msg cdproto.Message
		if err := conn.Read(ctx, &msg); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// 请求已写出，随后连接被对端关闭：Chrome 正在退出。
			return nil
		}
		if msg.ID != msgID {
			continue // 事件或 ping 帧
		}
		if msg.Error != nil {
			return errors.New(msg.Error.Error())
		}
		return nil
	}
}
