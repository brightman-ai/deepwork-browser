package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

func activeHeadedBrowserRuntimePIDs() []int {
	root := BrowserMuxHostRootDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	seen := make(map[int]bool)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "muxhost.json")
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var state BrowserMuxHostState
		if err := json.Unmarshal(body, &state); err != nil {
			continue
		}
		normalizeBrowserMuxHostState(&state)
		collectBrowserRuntimePID(seen, browserRuntimeProcessPID(state.BrowserPID, state.ChromePID), state.Mode, state.DisplayBackend, state.DisplayID)
		for _, rt := range state.Runtimes {
			collectBrowserRuntimePID(seen, browserRuntimeProcessPID(rt.BrowserPID, rt.ChromePID), "", rt.DisplayBackend, rt.DisplayID)
		}
	}
	out := make([]int, 0, len(seen))
	for pid := range seen {
		out = append(out, pid)
	}
	return out
}

func browserRuntimeProcessPID(browserPID, legacyChromePID int) int {
	if browserPID > 0 {
		return browserPID
	}
	return legacyChromePID
}

func collectBrowserRuntimePID(seen map[int]bool, pid int, mode BrowserMode, backend string, displayID uint32) {
	if pid <= 0 || !isPIDAlive(pid) {
		return
	}
	backend = strings.TrimSpace(strings.ToLower(backend))
	if displayID == 0 && backend != "cgvirtualdisplay" {
		return
	}
	if mode != "" && mode != ModeHeaded {
		return
	}
	seen[pid] = true
}
