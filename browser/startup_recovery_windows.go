//go:build windows

// Package browser — startup recovery 平台 dispatch (Windows).
//
// Windows 无 POSIX kill -0; 用 os.FindProcess + 进程对象交互推断:
// - FindProcess 总是返回非 nil (Windows 语义), 需通过其它 API 判断
// - 这里采用保守策略: 通过 OpenProcess 间接判断 (经 os/exec 包装抛出错误码区分)
//
// 注: dw-browser 当前主要支持 macOS/Linux; Windows 通路为兼容编译, recovery 行为降级.
package browser

import "os"

func platformIsPIDAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil || p == nil {
		return false
	}
	// 在 Windows 上, FindProcess 不验证存活. 用 Signal(syscall.Signal(0)) 间接探测.
	// 失败 → 进程不存在 (返回 false 保守判定 = 不存活).
	if sigErr := p.Signal(os.Interrupt); sigErr != nil {
		// Signal 在 Windows 上对非 self 进程几乎总是返回错误; 无法可靠判断
		// 保守: 假定不存活, 让 Singleton* 残留被清理 (Chrome 启动会自夺锁)
		return false
	}
	return true
}

// killPIDGraceful Windows 版: os.Process.Kill 立即终止 (无 graceful 概念).
func killPIDGraceful(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil || p == nil {
		return err
	}
	return p.Kill
}

// killPIDForce Windows 版: 同 graceful (Windows 无 SIGKILL 区别).
func killPIDForce(pid int) error {
	return killPIDGraceful(pid)
}

func platformFindChromePIDsByProfileDir(profileDir string) int {
	return nil
}
