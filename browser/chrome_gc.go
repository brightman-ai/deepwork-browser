// Package browser — ownerless-Chrome garbage collection.
//
// The registry (chrome_registry.go) settles debts we wrote down. This file
// settles the ones nobody wrote down: a Chrome forked by a build of dw-browser
// that predates the registry, by a raw chromedp.NewContext somewhere, or by a
// process killed in the millisecond between fork and register. Their common
// signature on Linux is PPID==1 — reparented to init, so no live process is
// waiting on them — plus a profile directory we recognise as ours.
//
// The age threshold is the safety interlock: a Chrome that is only seconds old
// may simply be mid-launch in another dw-browser process that has not written
// its note yet.
package browser

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultChromeGCMinAge is how long an ownerless Chrome or a stale chromedp
// temp profile must have existed before gc will touch it.
const DefaultChromeGCMinAge = 10 * time.Minute

// ChromeGCOptions tunes RunChromeGC.
type ChromeGCOptions struct {
	// DryRun reports without killing or deleting.
	DryRun bool
	// MinAge protects anything younger. Zero means DefaultChromeGCMinAge; use a
	// negative value to disable the age floor entirely (tests, forced sweeps).
	MinAge time.Duration
	Now    time.Time
}

// ChromeGCResult is the report `dw-browser gc` prints.
type ChromeGCResult struct {
	DryRun          bool               `json:"dry_run"`
	MinAgeSec       int64              `json:"min_age_sec"`
	RegistryActions []ChromeReapAction `json:"registry,omitempty"`
	Processes       []ChromeGCProcess  `json:"processes,omitempty"`
	ProfileDirs     []ChromeGCDir      `json:"profile_dirs,omitempty"`
	KilledProcesses int                `json:"killed_processes"`
	RemovedDirs     int                `json:"removed_dirs"`
	FreedBytes      int64              `json:"freed_bytes"`
}

// ChromeGCProcess is one ownerless Chrome gc found.
type ChromeGCProcess struct {
	PID        int    `json:"pid"`
	PGID       int    `json:"pgid,omitempty"`
	ProfileDir string `json:"profile_dir"`
	AgeSec     int64  `json:"age_sec"`
	Action     string `json:"action"`
	Reason     string `json:"reason"`
}

// ChromeGCDir is one leftover profile directory gc found.
type ChromeGCDir struct {
	Path      string `json:"path"`
	AgeSec    int64  `json:"age_sec"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	Action    string `json:"action"`
	Reason    string `json:"reason"`
}

// RunChromeGC reclaims Chrome processes and profile directories that no live
// owner accounts for.
//
// Three passes, narrowest evidence first:
//  1. registry notes whose owner process is gone (including detached sessions
//     that no session file references any more);
//  2. Chrome processes with PPID==1 whose --user-data-dir is a chromedp temp
//     dir or a dw-browser profile root, past MinAge, not backed by a live
//     session;
//  3. chromedp-runner directories in TMPDIR with no process left using them.
func RunChromeGC(opts ChromeGCOptions) ChromeGCResult {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	minAge := opts.MinAge
	if minAge == 0 {
		minAge = DefaultChromeGCMinAge
	}
	if minAge < 0 {
		minAge = 0
	}

	result := ChromeGCResult{DryRun: opts.DryRun, MinAgeSec: int64(minAge.Seconds())}

	// Pass 1 — registry notes. IncludeDetached: gc is the one caller allowed to
	// take a `dw-browser open` session, and only after the live-session check
	// inside the reaper has cleared it.
	result.RegistryActions = ReapAbandonedChromeProcesses(ChromeReapOptions{
		DryRun:          opts.DryRun,
		IncludeDetached: true,
		MinAge:          minAge,
		Now:             now,
	})
	// Count would_kill too: a dry run that reports "0 would be cleaned" while
	// listing things it would kill is worse than no report at all.
	handled := map[int]bool{}
	for _, act := range result.RegistryActions {
		if act.Action == "killed" || act.Action == "would_kill" {
			result.KilledProcesses++
			handled[act.ChromePID] = true
		}
	}

	// Pass 2 — ownerless Chrome processes nobody registered. Anything pass 1
	// already accounted for is skipped rather than listed twice: in a real run
	// it is already dead by now, and in a dry run double-listing it would
	// inflate the count.
	live := liveSessionChromePIDs()
	for _, proc := range findOwnerlessChromeProcesses() {
		if handled[proc.pid] {
			continue
		}
		age := now.Sub(proc.startedAt)
		if age < 0 {
			age = 0
		}
		entry := ChromeGCProcess{
			PID:        proc.pid,
			PGID:       proc.pgid,
			ProfileDir: proc.profileDir,
			AgeSec:     int64(age.Seconds()),
		}
		switch {
		case live[proc.pid]:
			entry.Action, entry.Reason = "protected", "live_session"
		case age < minAge:
			entry.Action, entry.Reason = "protected", "younger_than_min_age"
		case opts.DryRun:
			entry.Action, entry.Reason = "would_kill", "ownerless_chrome"
			result.KilledProcesses++
			result.Processes = append(result.Processes, entry)
			continue
		default:
			KillChromeProcessTree(proc.pid)
			entry.Action, entry.Reason = "killed", "ownerless_chrome"
			result.KilledProcesses++
		}
		result.Processes = append(result.Processes, entry)
	}

	// Pass 3 — chromedp temp profiles with nothing using them. Runs last so a
	// directory freed by pass 1/2 is collected in the same sweep.
	for _, dir := range findStaleChromedpRunnerDirs(now, minAge) {
		entry := ChromeGCDir{Path: dir.path, AgeSec: dir.ageSec}
		if pids := platformFindChromePIDsByProfileDir(dir.path); len(pids) > 0 {
			entry.Action, entry.Reason = "protected", "chrome_still_using_dir"
			result.ProfileDirs = append(result.ProfileDirs, entry)
			continue
		}
		entry.SizeBytes = dirSizeBytes(dir.path)
		if opts.DryRun {
			entry.Action, entry.Reason = "would_remove", "stale_chromedp_profile"
			result.RemovedDirs++
			result.FreedBytes += entry.SizeBytes
			result.ProfileDirs = append(result.ProfileDirs, entry)
			continue
		}
		if err := os.RemoveAll(dir.path); err != nil {
			entry.Action, entry.Reason = "skipped", "remove_failed"
			result.ProfileDirs = append(result.ProfileDirs, entry)
			continue
		}
		entry.Action, entry.Reason = "removed", "stale_chromedp_profile"
		result.RemovedDirs++
		result.FreedBytes += entry.SizeBytes
		result.ProfileDirs = append(result.ProfileDirs, entry)
	}

	sort.Slice(result.Processes, func(i, j int) bool { return result.Processes[i].PID < result.Processes[j].PID })
	sort.Slice(result.ProfileDirs, func(i, j int) bool { return result.ProfileDirs[i].Path < result.ProfileDirs[j].Path })
	return result
}

type staleRunnerDir struct {
	path   string
	ageSec int64
}

func findStaleChromedpRunnerDirs(now time.Time, minAge time.Duration) []staleRunnerDir {
	tmp := os.TempDir()
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return nil
	}
	var dirs []staleRunnerDir
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "chromedp-runner") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		age := now.Sub(info.ModTime())
		if age < 0 {
			age = 0
		}
		if age < minAge {
			continue
		}
		dirs = append(dirs, staleRunnerDir{path: filepath.Join(tmp, entry.Name()), ageSec: int64(age.Seconds())})
	}
	return dirs
}

// liveSessionChromePIDs is the protection list: anything a session file still
// points at is somebody's live browser, not garbage — even if it is orphaned by
// PPID, which is exactly what `dw-browser open` produces on purpose.
func liveSessionChromePIDs() map[int]bool {
	pids := map[int]bool{}
	sessions, err := ListSessions()
	if err != nil {
		return pids
	}
	for _, session := range sessions {
		if session.ChromePID > 0 && isPIDAlive(session.ChromePID) {
			pids[session.ChromePID] = true
		}
		if session.BrowserMuxHostPID > 0 && isPIDAlive(session.BrowserMuxHostPID) {
			pids[session.BrowserMuxHostPID] = true
		}
	}
	return pids
}

// chromeGCProfileDirIsOurs keeps gc from ever touching the Human's own browser.
// A stray desktop Chrome has a profile under ~/.config/chromium or
// ~/snap/chromium/common/chromium — never a chromedp temp dir and never a
// dw-browser root — so the allowlist is what makes "kill anything with PPID==1"
// safe to say out loud.
func chromeGCProfileDirIsOurs(profileDir string) bool {
	profileDir = filepath.Clean(strings.TrimSpace(profileDir))
	if profileDir == "" || profileDir == "." || profileDir == "/" {
		return false
	}
	if strings.HasPrefix(filepath.Base(profileDir), "chromedp-runner") {
		return true
	}
	for _, root := range BrowserCLIProfileRoots("") {
		if profileDir == root {
			continue
		}
		if strings.HasPrefix(profileDir, root+string(filepath.Separator)) {
			return true
		}
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		poolRoot := filepath.Join(home, ".deepwork", "browser-cli")
		if strings.HasPrefix(profileDir, poolRoot+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
