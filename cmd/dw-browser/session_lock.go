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
// runCLI 持有锁直到 typed handler 与结果交付完成，并通过 defer 释放。
// flock 同时保证异常进程退出（包括 SIGKILL）不会留下脏锁。

// heldSessionLock 是包级引用,唯一目的是防止 GC 回收 *os.File 触发 finalizer
// 提前 close 掉持锁的 fd。runCLI 完成时显式释放。
var heldSessionLock *browser.OperatorLock

// Only the declared request enters the session critical section. Diagnostics
// and infrastructure identifiers never pretend to be a task session id.
func acquireSessionOperatorLockE(argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	def, _, err := lookupCommand(argv)
	if err == nil && def.Diagnostic {
		return nil
	}
	sid := scanSessionIDArg(argv)
	if sid == "" {
		return nil
	}
	if argv[0] == "close" && scanBoolFlag(argv, "--force") {
		return nil
	}
	lock, err := browser.AcquireSessionOperatorLock(sid, browser.SummarizeOperatorCommand(redactCLIArguments(argv)), scanLockWait(argv))
	if err != nil {
		return err
	}
	heldSessionLock = lock
	return nil
}
func redactCLIArguments(args []string) []string {
	result := append([]string(nil), args...)
	for i := 0; i < len(result); i++ {
		name, _, inline := strings.Cut(result[i], "=")
		if name == "--text" || name == "--text-file" || strings.HasSuffix(name, "-api-key") {
			if inline {
				result[i] = name + "=***"
			} else if i+1 < len(result) {
				i++
				result[i] = "***"
			}
		}
	}
	return result
}

func isSessionBusy(err error) bool {
	var busy *browser.SessionBusyError
	return errors.As(err, &busy)
}

// scanSessionIDArg 只认 --id,与 parseCommonFlags 保持一致。
// 只提取会话身份；完整声明验证和 typed parser 负责参数错误。
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
