//go:build !windows

package browser

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// 这些"长得像"的进程让身份核对既有正例也有反例：被核对的 PID 必须真的是 dw-browser 自己的 host / 这个 profile 的 Chrome，
// 别的任何进程（包括测试自己起的 sleep）一概不被当作它们、不被发信号。

func spawnLongSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("no sleep binary: %v", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // 回收僵尸，isPIDAlive 才可信
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return pid
}

// spawnMuxHostLookalike 起一个命令行形如 `<…/dw-browser> muxhost serve --muxhost-id <id>` 的进程，扮演
// "dw-browser 自己的 BrowserMuxHost"：身份核对要认得出它，别的进程一概认不出。做法是一个名叫 dw-browser 的 shell 脚本
// （脚本不 exec，所以命令行里一直是它自己），脚本里真正干活的是 sleep。
func spawnMuxHostLookalike(t *testing.T, hostID string) int {
	t.Helper()
	script := filepath.Join(t.TempDir(), "dw-browser")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 300\ntrue\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script, "muxhost", "serve", "--muxhost-id", hostID)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start the look-alike: %v", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return pid
}

// spawnChromeLookalike 起一个命令行带 `--user-data-dir=<profileDir>` 的进程（sh，真正干活的是 sleep），
// 扮演"这个 profile 自己的 Chrome"：启动恢复只对认得出是这种进程的 PID 发信号。
func spawnChromeLookalike(t *testing.T, profileDir string) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "sleep 300; true", "--", "--remote-debugging-port=0", "--user-data-dir="+profileDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start the look-alike: %v", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	return pid
}
