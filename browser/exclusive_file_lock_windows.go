//go:build windows

package browser

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

// exclusiveFileLock 是一次内核级排他持有。Close 放锁;进程死亡时内核关掉句柄
// 也等于放锁 —— Windows 的 LockFileEx 与 Unix flock 在这一点上语义一致,所以
// 这里是等价实现而不是降级实现。
type exclusiveFileLock interface {
	Close() error
}

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	errorLockViolation      = syscall.Errno(33)
)

var (
	kernel32DLL      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32DLL.NewProc("LockFileEx")
	procUnlockFileEx = kernel32DLL.NewProc("UnlockFileEx")
)

type windowsFileLock struct {
	file *os.File
}

func (l *windowsFileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	var overlapped syscall.Overlapped
	ret, _, err := procUnlockFileEx.Call(
		uintptr(syscall.Handle(l.file.Fd())),
		0,
		1, 0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	closeErr := l.file.Close()
	l.file = nil
	if ret == 0 && err != nil {
		return err
	}
	return closeErr
}

// acquireExclusiveFileLock 取 path 的 LockFileEx 排他锁,最多等 wait。
// 与 unix 侧同构:FAIL_IMMEDIATELY + 轮询,保证等待有界。
func acquireExclusiveFileLock(path string, wait time.Duration) (exclusiveFileLock, bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	deadline := time.Now().Add(wait)
	for {
		var overlapped syscall.Overlapped
		ret, _, callErr := procLockFileEx.Call(
			uintptr(syscall.Handle(file.Fd())),
			uintptr(lockfileExclusiveLock|lockfileFailImmediately),
			0,
			1, 0,
			uintptr(unsafe.Pointer(&overlapped)),
		)
		if ret != 0 {
			return &windowsFileLock{file: file}, true, nil
		}
		if callErr != errorLockViolation {
			_ = file.Close()
			return nil, false, callErr
		}
		if !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, false, nil
		}
		remaining := time.Until(deadline)
		sleep := operatorLockPollInterval
		if remaining < sleep {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
}

// processAlive 只用来判断 .owner 里那条身份还值不值得打印。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const processQueryLimitedInformation = 0x1000
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	var code uint32
	if err := syscall.GetExitCodeProcess(handle, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
