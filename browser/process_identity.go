// Package browser — 发信号之前先确认"这个 PID 真的是我们要关的那个东西"。
//
// manifest 里记的 PID 可能已经陈旧：进程早就退出，PID 被系统分给了不相干的程序（例如用户正在用的别的守护进程）。
// 按陈旧 PID 发信号会误伤它。所以：定位目标只用 dw-browser 自己的 manifest 与控制接口；而在对任何 PID 发
// SIGTERM / SIGKILL 之前，再核对一次它的命令行确实是我们自己启动的那种进程，对不上就不碰。
// 这里从不按进程名搜索、枚举或匹配进程。无法获取命令行（如 Windows）时一律视为"对不上"，不发信号。
package browser

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// processCommandLine 返回 pid 的完整命令行；取不到返回空串。
func processCommandLine(pid int) string {
	if pid <= 1 {
		return ""
	}
	out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// pidIsBrowserMuxHost：pid 的命令行是 `<…/dw-browser> muxhost serve … --muxhost-id <hostID> …`，即 dw-browser 自己的 BrowserMuxHost。
func pidIsBrowserMuxHost(pid int, hostID string) bool {
	return commandLineIsBrowserMuxHost(processCommandLine(pid), hostID)
}

// commandLineIsBrowserMuxHost 三个条件缺一不可：可执行文件是 dw-browser、子命令恰好是 `muxhost serve`、
// `--muxhost-id` 的值恰好等于目标 host id。名字里带 mux 的别的程序（other-muxd、usbmuxd……）一个条件都过不了。
func commandLineIsBrowserMuxHost(cmdline, hostID string) bool {
	if cmdline == "" || hostID == "" {
		return false
	}
	pos := strings.Index(cmdline, " muxhost serve")
	if pos < 0 {
		return false
	}
	// 可执行文件路径可能含空格（如 .app 包里），所以取子命令之前的全部文本，再看文件名。
	switch filepath.Base(strings.TrimSpace(cmdline[:pos])) {
	case "dw-browser", "dw-browser.exe":
	default:
		return false
	}
	fields := strings.Fields(cmdline[pos:])
	for i, f := range fields {
		if f == "--muxhost-id" && i+1 < len(fields) && fields[i+1] == hostID {
			return true
		}
	}
	return false
}

// NotOurProcessError：对一个 PID 发信号之前的身份核对没通过。没有发出任何信号。
type NotOurProcessError struct {
	PID         int
	CommandLine string // 实际命令行（已截断）；读不到为空
	Want        string // 期望的身份描述
}

func (e *NotOurProcessError) Error() string {
	cmd := e.CommandLine
	if cmd == "" {
		cmd = "(unreadable, or the process is gone)"
	}
	return fmt.Sprintf("dw-browser: refusing to signal pid %d: its command line is %q, not %s — the recorded pid is stale or was reused by another program; nothing was sent",
		e.PID, cmd, e.Want)
}

// requireBrowserMuxHostPID：是我们的 host 返回 nil；否则返回 *NotOurProcessError（带 pid 与实际命令行摘要），调用方不得发信号。
func requireBrowserMuxHostPID(pid int, hostID string) error {
	cmdline := processCommandLine(pid)
	if commandLineIsBrowserMuxHost(cmdline, hostID) {
		return nil
	}
	return &NotOurProcessError{PID: pid, CommandLine: summarizeCommandLine(cmdline), Want: "a `dw-browser muxhost serve --muxhost-id " + hostID + "` process"}
}

func summarizeCommandLine(cmdline string) string {
	const max = 200
	if r := []rune(cmdline); len(r) > max {
		return string(r[:max]) + "…"
	}
	return cmdline
}

// pidIsChromeWithDebugPort：pid 的命令行带 --remote-debugging-port，即 dw-browser 以 CDP 方式启动的 Chrome。
func pidIsChromeWithDebugPort(pid int) bool {
	return strings.Contains(processCommandLine(pid), "--remote-debugging-port")
}

// PIDIsDebuggableChrome 是 pidIsChromeWithDebugPort 的导出版本，给 CLI 在对会话文件里记的 Chrome PID 发信号之前核对用。
func PIDIsDebuggableChrome(pid int) bool { return pidIsChromeWithDebugPort(pid) }

// pidIsChromeOfProfile：pid 的命令行带 `--user-data-dir=<profileDir>`（后面紧跟空白或结束），
// 即这个 profile 目录自己的 Chrome。判定方式与 platformFindChromePIDsByProfileDir 一致，只是多要求边界：
// 目录 /x/p 不会匹配 /x/p2 的 Chrome。
func pidIsChromeOfProfile(pid int, profileDir string) bool {
	return commandLineIsChromeOfProfile(processCommandLine(pid), profileDir)
}

func commandLineIsChromeOfProfile(cmdline, profileDir string) bool {
	profileDir = strings.TrimRight(strings.TrimSpace(profileDir), "/")
	if cmdline == "" || profileDir == "" {
		return false
	}
	needle := "--user-data-dir=" + profileDir
	for from := 0; ; {
		i := strings.Index(cmdline[from:], needle)
		if i < 0 {
			return false
		}
		end := from + i + len(needle)
		for end < len(cmdline) && cmdline[end] == '/' { // 目录写成 /x/p/ 也是同一个目录
			end++
		}
		if end == len(cmdline) || cmdline[end] == ' ' || cmdline[end] == '\t' {
			return true
		}
		from = from + i + 1
	}
}

// requireChromeOfProfilePID：pid 是这个 profile 目录自己的 Chrome 返回 nil；否则返回 *NotOurProcessError
// （带 pid 与实际命令行摘要），调用方不得发信号。
func requireChromeOfProfilePID(pid int, profileDir string) error {
	cmdline := processCommandLine(pid)
	if commandLineIsChromeOfProfile(cmdline, profileDir) {
		return nil
	}
	return &NotOurProcessError{PID: pid, CommandLine: summarizeCommandLine(cmdline), Want: "a Chrome started with --user-data-dir=" + profileDir}
}

// requireDebuggableChromePID：登记里没有 profile 目录可对时的退而求其次——pid 至少得是带 --remote-debugging-port 的 Chrome
// （dw-browser 以 CDP 方式启动的那种）。
func requireDebuggableChromePID(pid int) error {
	cmdline := processCommandLine(pid)
	if strings.Contains(cmdline, "--remote-debugging-port") {
		return nil
	}
	return &NotOurProcessError{PID: pid, CommandLine: summarizeCommandLine(cmdline), Want: "a Chrome started with --remote-debugging-port"}
}
