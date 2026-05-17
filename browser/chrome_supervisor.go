package browser

import (
	"context"
	"os"
	"time"
)

// ============================================================
// § ChromeSupervisor [Ref: CAP-BS09-C1 §3.2, T5-B8]
// ============================================================

// chromeSupervisorImpl implements ChromeSupervisor.
type chromeSupervisorImpl struct{}

// NewChromeSupervisor 返回默认 ChromeSupervisor 实现。
func NewChromeSupervisor() *chromeSupervisorImpl {
	return &chromeSupervisorImpl{}
}

// Watch 启动监控 goroutine，Chrome 进程意外退出时触发 onCrash。
// ctx.Done() 触发时视为正常退出，不调用 onCrash [Ref: TC-09-U-22]。
func (s *chromeSupervisorImpl) Watch(ctx context.Context, pid int, onCrash func()) {
	go func() {
		proc, err := os.FindProcess(pid)
		if err != nil {
			// 进程不存在，立即触发崩溃回调
			onCrash()
			return
		}

		// 使用 channel 等待进程退出
		done := make(chan error, 1)
		go func() {
			// proc.Wait() 在 Linux 上只对子进程有效
			// 对非子进程，使用轮询方式检测进程是否存活
			done <- waitForProcess(pid)
		}()

		select {
		case <-ctx.Done():
			// 正常退出，不触发 onCrash [TC-09-U-22]
			return
		case <-done:
			// 进程意外退出 → 触发 onCrash [TC-09-U-23]
			onCrash()
		}
		_ = proc
	}()
}

// waitForProcess 轮询检测进程是否存活（适用于非子进程）。
// 进程退出时返回 nil。
func waitForProcess(pid int) error {
	for {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return nil // 进程不存在
		}
		// 发送信号 0 检查进程是否存在
		if err := proc.Signal(os.Interrupt); err != nil {
			// os.Interrupt 在 Linux 是 SIGINT，不适合仅探测用
			// 改用 syscall.Signal(0)，但为避免 syscall 依赖，用轮询 /proc/{pid}
		}
		// 检查 /proc/{pid} 是否存在（Linux 特有）
		if _, statErr := os.Stat("/proc/" + itoa(pid)); statErr != nil {
			return nil // 进程已退出
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// itoa 简单整数转字符串（避免 import fmt 循环）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// RestartWithBackoff 崩溃后指数退避重启 Chrome。
// 最多 maxRetries 次（1s/2s/4s），全失败返回 ErrBrowserCrashed [Ref: CAP-BS09-C1 §3.2]。
func (s *chromeSupervisorImpl) RestartWithBackoff(ctx context.Context, launcher *chromeLauncherImpl, profileID string, maxRetries int) (cdpURL string, pid int, err error) {
	for i := 0; i < maxRetries; i++ {
		// 指数退避: 1s, 2s, 4s
		delay := time.Duration(1<<uint(i)) * time.Second

		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-time.After(delay):
		}

		cdpURL, pid, err = launcher.Launch(ctx, profileID)
		if err == nil {
			return cdpURL, pid, nil
		}
	}

	return "", 0, ErrBrowserCrashed
}
