// Package browser — BrowserMuxHost 与 CLI 的版本一致性。
//
// 全局 mux host（browser-mux-host-global）是一个长寿进程，会被后来的每一个 CLI 调用复用。升级 dw-browser 之后，
// 旧版 host 可能还活着；新 CLI 若悄悄复用它，新版的承诺就静默失效：旧 host 启动 Chrome 时不带清单要求的模拟钥匙串参数，
// 不封存、也不同步关闭，登录态会在下次打开时悄悄丢失。
//
// 一条规则，不做能力协商：host 在自己的状态里带上它的二进制版本；CLI（BinaryVersion 非空）在 ensure/attach 时，
// 只要 host 版本 ≠ 自己的版本——
//   - host 上没有存活的 runtime：请它关闭，等它退出，再用当前二进制重新拉起；
//   - 还有存活的 runtime：拒绝本次操作，错误里写明 host 的 pid、版本、占用中的会话以及恢复办法；
//   - 绝不悄悄复用。
//
// 旧版 host 的状态里没有版本字段，读出来是空串——同样算"不一致"。
package browser

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// BinaryVersion 是本进程所运行的 dw-browser 可执行文件的版本，由 main 在启动时设置一次。
// 空串表示这不是 dw-browser CLI（库调用方，如 BrowserPool）：没有可比较的版本身份，不做版本检查。
var BinaryVersion string

// muxHostVersionRetireWait 是请旧 host 关闭之后等它退出的上限，超时则强制终止（与陈旧 host 的处理一致）。
// 是 var 只为测试能缩短它。
var muxHostVersionRetireWait = 10 * time.Second

// MuxHostVersionMismatchError：host 版本与本 CLI 不一致，且 host 上还有存活的会话，拒绝复用。
type MuxHostVersionMismatchError struct {
	HostID      string
	HostPID     int
	HostVersion string // 空 = 旧版 host，状态里没有版本字段
	WantVersion string
	Sessions    []string // 占用中的会话（browser session id）
}

func (e *MuxHostVersionMismatchError) Error() string {
	have := e.HostVersion
	if have == "" {
		have = "unknown (it predates version reporting)"
	}
	return fmt.Sprintf("browser_mux_host: refusing to reuse BrowserMuxHost %s (pid %d, version %s): this dw-browser is %s, "+
		"and that host still holds %d live session(s): %s. A host from another version would start Chrome without this "+
		"version's profile encryption and shutdown guarantees. Recovery: close those sessions (dw-browser close --id <id>) "+
		"or, if they are abandoned, run `dw-browser muxhost shutdown --muxhost-id %s`, then retry",
		e.HostID, e.HostPID, have, e.WantVersion, len(e.Sessions), strings.Join(e.Sessions, ", "), e.HostID)
}

// liveMuxHostSessions 列出 host 上仍然存活的 runtime 对应的会话。以本进程自己的 PID 探测为准，不信 host 自报的 chrome_alive。
func liveMuxHostSessions(health *BrowserMuxHostState) []string {
	var live []string
	for _, rt := range health.Runtimes {
		if rt.ChromePID > 0 && isPIDAlive(rt.ChromePID) {
			id := rt.BrowserSessionID
			if id == "" {
				id = rt.RuntimeID
			}
			live = append(live, id)
		}
	}
	return live
}

// checkMuxHostVersion 在复用一个已存在的 host 之前强制版本一致。
// 返回 (true, nil) 表示可以复用；(false, nil) 表示 host 已被关闭、调用方应当拉起新的；(false, err) 表示拒绝。
func checkMuxHostVersion(ctx context.Context, state, health *BrowserMuxHostState) (reusable bool, err error) {
	if BinaryVersion == "" || health.BinaryVersion == BinaryVersion {
		return true, nil
	}
	pid := health.MuxHostPID
	if pid <= 0 {
		pid = state.MuxHostPID
	}
	if sessions := liveMuxHostSessions(health); len(sessions) > 0 {
		return false, &MuxHostVersionMismatchError{HostID: state.MuxHostID, HostPID: pid, HostVersion: health.BinaryVersion, WantVersion: BinaryVersion, Sessions: sessions}
	}
	log.Printf("[BROWSER-MUX-HOST] AUDIT: stale_version_muxhost_retire muxhost_id=%s pid=%d host_version=%q want=%q",
		state.MuxHostID, pid, health.BinaryVersion, BinaryVersion)
	host := *state
	host.RuntimeID = "" // 整个 host，不是某个 runtime
	if _, err := ShutdownBrowserMuxHost(ctx, &host); err != nil {
		log.Printf("[BROWSER-MUX-HOST] AUDIT: stale_version_muxhost_shutdown_request_failed pid=%d err=%v", pid, err)
	}
	deadline := time.Now().Add(muxHostVersionRetireWait)
	for isPIDAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(ProcessExitPollInterval)
	}
	if isPIDAlive(pid) {
		// 请不动：与陈旧 host 同样处理——但发信号之前先核对这个 PID 的命令行确实是这个 host（见 process_identity.go）；
		// 等待的这几秒里 PID 也可能已被复用，所以在这里核对，不是在更早的时候。
		if err := requireBrowserMuxHostPID(pid, state.MuxHostID); err != nil {
			return false, fmt.Errorf("could not retire BrowserMuxHost %s (version %q) that does not match this dw-browser (%s): %w; "+
				"stop it with `dw-browser muxhost shutdown --muxhost-id %s` and retry", state.MuxHostID, health.BinaryVersion, BinaryVersion, err, state.MuxHostID)
		}
		killAndWait(pid, ProfileOwnerMuxHostKillGrace)
	}
	if isPIDAlive(pid) {
		return false, fmt.Errorf("browser_mux_host: could not retire BrowserMuxHost %s (pid %d, version %q) that does not match this dw-browser (%s)",
			state.MuxHostID, pid, health.BinaryVersion, BinaryVersion)
	}
	return false, nil
}
