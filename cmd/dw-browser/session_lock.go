package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// ============================================================
// § 单操作者门 — 每次会话作用域的 CLI 调用都从这里进
// ============================================================
//
// observe→act 契约本来就隐含串行:agent 先 observe 拿到 @rN,再据此下手。两个操作
// 者同时对同一个会话下手时,后手手里的 @rN 指的已经不是他看到的那个元素 —— 这不是
// "偶尔打架",是契约被悄悄破坏。所以这里把串行从"隐含约定"变成"机制":
// 凡带 --id 的会话作用域调用,全程持有该会话的 flock 排他锁。
//
// 取锁点刻意放在 main() 分派之前而不是每个子命令内部:一个入口 = 一条不变量,
// 不会出现"新加的子命令忘了取锁"这种按文件数量增长的漏洞。
//
// 持锁 goroutine-safe 说明:锁在进程生命周期内一直持有,没有 defer Release ——
// 各子命令都以 os.Exit 结束(defer 本来就不跑),而 flock 由内核在进程退出时释放。
// 这正是选 flock 的理由:SIGKILL 也不会留下脏锁。

// heldSessionLock 是包级引用,唯一目的是防止 GC 回收 *os.File 触发 finalizer
// 提前 close 掉持锁的 fd。它必须活到进程结束。
var heldSessionLock *browser.OperatorLock

// operatorLockExemptCommands 是不取会话锁的命令。
//
// 每一条都有具体理由,不是"顺手放行":
//   - muxhost: 浏览器宿主基础设施。它由持锁的会话命令 fork 出来(muxhost serve
//     --id <同一个 session>),自己再取同一把锁必然自锁死。
//   - session status / session list / gc: 只读诊断。占有者是谁恰恰要靠它们查,
//     把诊断也堵在锁后面等于"出问题时没法看"。
//   - profile / skills / persona / version / help: 根本不碰会话。
var operatorLockExemptCommands = map[string]bool{
	"muxhost": true,
	"gc":      true,
	"profile": true,
	"skills":  true,
	"persona": true, "personas": true,
	"--version": true, "version": true,
	"--help": true, "-h": true, "help": true,
}

// acquireSessionOperatorLockOrExit 在命令分派前取会话锁。
//
// 无 --id(一次性会话 / 纯本地命令)= 无会话可争,直接放行。
// close --force 是运维逃生口:会话已经被卡住的操作者占着时,总得有办法把它收掉。
func acquireSessionOperatorLockOrExit(argv []string) {
	if len(argv) == 0 {
		return
	}
	cmd := argv[0]
	if operatorLockExemptCommands[cmd] {
		return
	}
	if cmd == "session" && len(argv) > 1 && (argv[1] == "status" || argv[1] == "list") {
		return
	}
	sessionID := scanSessionIDArg(argv)
	if sessionID == "" {
		return
	}
	if cmd == "close" || (cmd == "session" && len(argv) > 1 && argv[1] == "close") {
		if scanBoolFlag(argv, "--force") {
			fmt.Fprintf(os.Stderr, "dw-browser %s: --force 跳过会话锁(运维逃生口):"+
				"如果另一个操作者正在这个会话上动作,它的下一步可能落空\n", cmd)
			return
		}
	}

	wait := scanLockWait(argv)
	lock, err := browser.AcquireSessionOperatorLock(sessionID, browser.SummarizeOperatorCommand(argv), wait)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser %s: %v\n", cmd, err)
		if isSessionBusy(err) {
			// 逃生口一并说清,否则被拒的人只知道"不让我干",不知道下一步怎么办。
			fmt.Fprintf(os.Stderr, "  等久一点: --lock-wait 30s   |   查占有者: dw-browser session status --id %s"+
				"   |   强制收掉会话: dw-browser close --id %s --force\n", sessionID, sessionID)
		}
		os.Exit(exitRunErr)
	}
	heldSessionLock = lock
}

func isSessionBusy(err error) bool {
	var busy *browser.SessionBusyError
	return errors.As(err, &busy)
}

// scanSessionIDArg 只认 --id,与 parseCommonFlags 保持一致。
// 单独扫一遍 argv 而不复用 parseCommonFlags:后者会对未知 flag / 缺失 --scenario
// 直接 os.Exit,取锁这一步不该背上那些语义。
func scanSessionIDArg(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--id" && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(args[i], "--id="):
			return args[i][len("--id="):]
		}
	}
	return ""
}

func scanBoolFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

// scanLockWait 解析 --lock-wait。接受 Go duration("10s"/"500ms")也接受裸秒数("30")。
// 解析不了就用默认值并说一声 —— 一个打错的等待时长不值得让整条命令失败。
func scanLockWait(args []string) time.Duration {
	raw := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--lock-wait" && i+1 < len(args):
			raw = args[i+1]
		case strings.HasPrefix(args[i], "--lock-wait="):
			raw = args[i][len("--lock-wait="):]
		}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return browser.DefaultOperatorLockWait
	}
	if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
		return d
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	fmt.Fprintf(os.Stderr, "dw-browser: --lock-wait %q 无法解析(用 10s / 500ms / 30),回退默认 %s\n",
		raw, browser.DefaultOperatorLockWait)
	return browser.DefaultOperatorLockWait
}
