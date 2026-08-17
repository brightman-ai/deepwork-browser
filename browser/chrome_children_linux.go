//go:build linux

package browser

import (
	"os"
	"strconv"
	"strings"
)

// childPIDsOf returns the direct children of pid, read from
// /proc/<pid>/task/*/children — the kernel's own answer, so no ps parsing and
// no race window wider than the read itself.
//
// Used only by KillChromeProcessTree, for the case where a Chrome cannot be
// group-killed because it shares our process group (a chromedp allocator that
// did not Setpgid). Falls back to scanning every /proc entry's PPID if the
// children file is unavailable (CONFIG_PROC_CHILDREN off, or a hidepid mount).
func childPIDsOf(pid int) []int {
	if children := childPIDsFromProcChildren(pid); len(children) > 0 {
		return children
	}
	return childPIDsFromPPIDScan(pid)
}

func childPIDsFromProcChildren(pid int) []int {
	base := "/proc/" + strconv.Itoa(pid) + "/task"
	tasks, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []int
	for _, task := range tasks {
		body, err := os.ReadFile(base + "/" + task.Name() + "/children")
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(body)) {
			if child, err := strconv.Atoi(field); err == nil && child > 1 {
				out = append(out, child)
			}
		}
	}
	return out
}

func childPIDsFromPPIDScan(pid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate, err := strconv.Atoi(entry.Name())
		if err != nil || candidate <= 1 {
			continue
		}
		if pidPPIDFromProc(candidate) == pid {
			out = append(out, candidate)
		}
	}
	return out
}
