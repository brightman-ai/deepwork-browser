//go:build !windows

package browser

import (
	"os"
	"syscall"
	"time"
)

// exclusiveFileLock 是一次内核级排他持有。Close 放锁;进程死亡(含 SIGKILL)时
// 内核关掉 fd 也等于放锁 —— 这正是选 flock 的理由:脏锁在设计上不存在。
type exclusiveFileLock interface {
	Close() error
}

type unixFlock struct {
	file *os.File
}

func (l *unixFlock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// acquireExclusiveFileLock 取 path 的 flock 排他锁,最多等 wait。
//
// 用 LOCK_NB + 轮询而不是阻塞式 flock:阻塞式没有超时参数,要做有界等待只能靠
// 信号打断,那比重试脆弱得多(还会和 Go runtime 的信号处理打架)。轮询间隔 20ms,
// 10s 的默认等待也就 500 次 syscall,代价可以忽略。
//
// 返回 (lock, true, nil) = 拿到;(nil, false, nil) = 等满了仍被占;err = 真出错。
func acquireExclusiveFileLock(path string, wait time.Duration) (exclusiveFileLock, bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &unixFlock{file: file}, true, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			_ = file.Close()
			return nil, false, err
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
// 信号 0 不投递,只做权限/存在性检查。
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
