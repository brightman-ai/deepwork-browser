//go:build !windows

// Package browser — startup recovery 平台 dispatch (Unix: macOS / Linux / *BSD).
//
// 用 syscall.Kill(pid, signal) 实现:
// - signal 0 → 仅校验 PID 是否合法 (kill -0 语义)
// - SIGTERM / SIGKILL → 双阶段终止
package browser

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func platformIsPIDAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	// EPERM = 进程存在但当前用户无权限发信号 (仍视为存活, 不误判)
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}

func killPIDGraceful(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

func killPIDForce(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}

func platformFindChromePIDsByProfileDir(profileDir string) int {
	profileDir = strings.TrimSpace(profileDir)
	if profileDir == "" {
		return nil
	}
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output
	if err != nil {
		return nil
	}
	needle := "--user-data-dir=" + profileDir
	lines := strings.Split(string(out), "\n")
	pids := make(int, 0, 2)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, needle) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}
