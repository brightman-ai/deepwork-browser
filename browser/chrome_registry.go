// Package browser — chrome process registry.
//
// Why a file on disk and not just a defer:
//
// Every in-process cleanup path (defer, context cancel, chromedp's own
// SIGKILL-on-exit, signal handlers) shares one precondition: this process gets
// to run code before it dies. SIGKILL, an OOM kill, `kill -9` from an agent
// harness, and a hard CI cancel all violate it. The kernel-level substitute,
// Pdeathsig, is cleared when snap-confine (setuid root) execs — measured, see
// ApplyOwnedChromeProcAttr. That leaves exactly one place to put the intent
// where it survives our own death: the filesystem.
//
// So: before we own a Chrome we write down who owns it and how to kill its
// whole tree; when we release it we delete the note; and any later dw-browser
// process (next launch, or `dw-browser gc`) reaps notes whose owner is gone.
//
// Ownership is proven by (pid, start-time) rather than pid alone — pids are
// recycled, and a recycled pid would make an abandoned Chrome look owned
// forever.
package browser

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// EnvChromeProcRegistryDir overrides where ownership records live. Set by the
// browser package's TestMain so a test run's registry is its own and a leak
// gate can attribute precisely; also useful for isolating concurrent agents.
const EnvChromeProcRegistryDir = "DW_BROWSER_PROCREG_DIR"

// ChromeOwnershipKind labels why a Chrome was started, so a reaper can apply
// the right policy — most importantly, whether it is *supposed* to outlive its
// launcher.
type ChromeOwnershipKind string

const (
	// ChromeOwnedByPool — BrowserPool headless Chrome (chromedp ExecAllocator).
	ChromeOwnedByPool ChromeOwnershipKind = "pool"
	// ChromeOwnedByCore — NewBrowserCore / BrowserMuxHost controlled Chrome.
	ChromeOwnedByCore ChromeOwnershipKind = "core"
	// ChromeOwnedByLauncher — legacy chromeLauncherImpl.Launch.
	ChromeOwnedByLauncher ChromeOwnershipKind = "launcher"
	// ChromeDetachedSession — dw-browser open/session: Chrome must outlive the
	// CLI on purpose. Never reaped on owner death; only `dw-browser gc` can
	// take it, and only once no live session references it.
	ChromeDetachedSession ChromeOwnershipKind = "detached-session"
)

// ChromeProcessRecord is one ownership note.
type ChromeProcessRecord struct {
	Token      string              `json:"token"`
	Kind       ChromeOwnershipKind `json:"kind"`
	OwnerPID   int                 `json:"owner_pid"`
	OwnerBoot  string              `json:"owner_boot,omitempty"`
	ChromePID  int                 `json:"chrome_pid"`
	PGID       int                 `json:"pgid"`
	ProfileDir string              `json:"profile_dir,omitempty"`
	TempDir    bool                `json:"temp_dir,omitempty"`
	StartedAt  time.Time           `json:"started_at"`
}

// Detached reports whether this Chrome is meant to outlive its launcher.
func (r ChromeProcessRecord) Detached() bool { return r.Kind == ChromeDetachedSession }

var chromeRegistryOnce sync.Once

// ChromeProcRegistryDir is where ownership records are written.
func ChromeProcRegistryDir() string {
	if dir := strings.TrimSpace(os.Getenv(EnvChromeProcRegistryDir)); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "dw-browser-procreg")
	}
	return filepath.Join(home, ".deepwork", "browser", "procreg")
}

// RegisterChromeProcess writes an ownership note for a Chrome this process just
// forked and returns its token. Call it as soon as the pid is known — before
// waiting for CDP readiness, because a launch that times out is exactly the
// case that leaks. Errors are returned but callers should treat them as
// non-fatal: an unregistered Chrome is a Chrome only its in-process owner can
// clean up, which is the pre-existing behaviour, not a regression.
func RegisterChromeProcess(kind ChromeOwnershipKind, chromePID int, profileDir string, tempDir bool) (string, error) {
	if chromePID <= 0 {
		return "", fmt.Errorf("register chrome process: invalid pid %d", chromePID)
	}
	dir := ChromeProcRegistryDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("register chrome process: mkdir %s: %w", dir, err)
	}
	rec := ChromeProcessRecord{
		Token:      fmt.Sprintf("%d-%d-%d", os.Getpid(), chromePID, time.Now().UnixNano()),
		Kind:       kind,
		OwnerPID:   os.Getpid(),
		OwnerBoot:  processStartToken(os.Getpid()),
		ChromePID:  chromePID,
		PGID:       ChromeProcessGroupID(chromePID),
		ProfileDir: filepath.Clean(profileDir),
		TempDir:    tempDir,
		StartedAt:  time.Now(),
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, rec.Token+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return "", fmt.Errorf("register chrome process: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("register chrome process: commit: %w", err)
	}
	return rec.Token, nil
}

// UnregisterChromeProcess drops an ownership note after its Chrome has been
// killed (or deliberately handed off). Idempotent.
func UnregisterChromeProcess(token string) {
	if strings.TrimSpace(token) == "" {
		return
	}
	_ = os.Remove(filepath.Join(ChromeProcRegistryDir(), token+".json"))
}

// PromoteChromeProcessToDetached rewrites a note's kind so a Chrome that was
// launched under this process's ownership becomes an intentionally-detached
// session Chrome (dw-browser open's success path). Without this, the very next
// dw-browser invocation would reap a session the user just asked to keep.
func PromoteChromeProcessToDetached(token string) {
	if strings.TrimSpace(token) == "" {
		return
	}
	path := filepath.Join(ChromeProcRegistryDir(), token+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var rec ChromeProcessRecord
	if json.Unmarshal(body, &rec) != nil {
		return
	}
	rec.Kind = ChromeDetachedSession
	if out, err := json.Marshal(rec); err == nil {
		_ = os.WriteFile(path, out, 0o600)
	}
}

// ListChromeProcessRecords returns every ownership note currently on disk,
// oldest first. Unparseable files are skipped, not fatal — a half-written note
// from a process killed mid-write must not break the reaper.
func ListChromeProcessRecords() []ChromeProcessRecord {
	dir := ChromeProcRegistryDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	records := make([]ChromeProcessRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		var rec ChromeProcessRecord
		if json.Unmarshal(body, &rec) != nil || rec.ChromePID <= 0 {
			continue
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].StartedAt.Before(records[j].StartedAt) })
	return records
}

// ChromeReapAction is one line of "what gc / startup recovery did".
type ChromeReapAction struct {
	Token      string              `json:"token"`
	Kind       ChromeOwnershipKind `json:"kind"`
	ChromePID  int                 `json:"chrome_pid"`
	PGID       int                 `json:"pgid,omitempty"`
	ProfileDir string              `json:"profile_dir,omitempty"`
	AgeSec     int64               `json:"age_sec"`
	Action     string              `json:"action"`
	Reason     string              `json:"reason"`
}

// ChromeReapOptions tunes ReapAbandonedChromeProcesses.
type ChromeReapOptions struct {
	// DryRun reports what would be done without killing or deleting anything.
	DryRun bool
	// IncludeDetached also considers intentionally-detached session Chromes
	// (dw-browser open). Only `dw-browser gc` sets it, and even then a live
	// session file still protects them.
	IncludeDetached bool
	// MinAge protects notes younger than this (0 = no age floor). Guards
	// against reaping a Chrome another process is in the middle of launching.
	MinAge time.Duration
	Now    time.Time
}

// ReapAbandonedChromeProcesses kills the process group of every registered
// Chrome whose owner is gone, and deletes the note. This is the fallback that
// makes `kill -9 <dw-browser>` survivable: the note outlives the owner, and the
// next dw-browser process (or gc) settles the debt.
func ReapAbandonedChromeProcesses(opts ChromeReapOptions) []ChromeReapAction {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	self := os.Getpid()
	var actions []ChromeReapAction

	for _, rec := range ListChromeProcessRecords() {
		age := now.Sub(rec.StartedAt)
		if age < 0 {
			age = 0
		}
		act := ChromeReapAction{
			Token:      rec.Token,
			Kind:       rec.Kind,
			ChromePID:  rec.ChromePID,
			PGID:       rec.PGID,
			ProfileDir: rec.ProfileDir,
			AgeSec:     int64(age.Seconds()),
		}

		// A note whose Chrome is already gone is pure bookkeeping debris.
		if !isPIDAlive(rec.ChromePID) {
			act.Action, act.Reason = "record_removed", "chrome_already_exited"
			if !opts.DryRun {
				UnregisterChromeProcess(rec.Token)
				removeReapedTempProfile(rec)
			}
			actions = append(actions, act)
			continue
		}
		if opts.MinAge > 0 && age < opts.MinAge {
			act.Action, act.Reason = "protected", "younger_than_min_age"
			actions = append(actions, act)
			continue
		}
		if rec.OwnerPID == self {
			act.Action, act.Reason = "protected", "owned_by_this_process"
			actions = append(actions, act)
			continue
		}
		if rec.Detached() && !opts.IncludeDetached {
			act.Action, act.Reason = "protected", "detached_session_by_design"
			actions = append(actions, act)
			continue
		}
		if ownerStillAlive(rec) {
			act.Action, act.Reason = "protected", "owner_alive"
			actions = append(actions, act)
			continue
		}
		// A live session file outranks the note's kind. The note records who
		// forked the process; the session file records who is still using it,
		// and only the second is a reason to keep it alive.
		if chromeRecordReferencedByLiveSession(rec) {
			act.Action, act.Reason = "protected", "live_session"
			actions = append(actions, act)
			continue
		}

		act.Action, act.Reason = "killed", "owner_gone"
		if opts.DryRun {
			act.Action = "would_kill"
			actions = append(actions, act)
			continue
		}
		killChromeRecord(rec)
		UnregisterChromeProcess(rec.Token)
		removeReapedTempProfile(rec)
		actions = append(actions, act)
	}
	return actions
}

// killChromeRecord kills the recorded Chrome's whole process group. Both the
// recorded pgid and a live re-read are tried: the recorded one still reaches
// surviving children after the leader has exited, the live one covers a note
// written before Setpgid could be observed.
func killChromeRecord(rec ChromeProcessRecord) {
	if rec.PGID > 0 {
		KillChromeProcessGroupID(rec.PGID)
	}
	KillChromeProcessTree(rec.ChromePID)
	deadline := time.Now().Add(ProfileOwnerChromeKillGrace)
	for time.Now().Before(deadline) {
		if !isPIDAlive(rec.ChromePID) {
			return
		}
		time.Sleep(ProcessExitPollInterval)
	}
}

// removeReapedTempProfile deletes the profile directory of a reaped Chrome, but
// only when the note says it was a throwaway (chromedp's own temp dir, or an
// ephemeral CLI profile). Persistent profiles are the Human's data.
func removeReapedTempProfile(rec ChromeProcessRecord) {
	if !rec.TempDir || rec.ProfileDir == "" {
		return
	}
	if !isDisposableChromeProfileDir(rec.ProfileDir) {
		return
	}
	_ = os.RemoveAll(rec.ProfileDir)
}

// isDisposableChromeProfileDir is a deliberately narrow allowlist: a reaper
// that can be talked into RemoveAll on an arbitrary path is a worse bug than
// the leak it fixes.
func isDisposableChromeProfileDir(dir string) bool {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "" || dir == "/" || dir == "." {
		return false
	}
	base := filepath.Base(dir)
	if strings.HasPrefix(base, "chromedp-runner") {
		return true
	}
	if strings.HasPrefix(base, BrowserCLIEphemeralProfilePrefix) {
		return true
	}
	return false
}

func ownerStillAlive(rec ChromeProcessRecord) bool {
	if rec.OwnerPID <= 0 {
		return false
	}
	if !isPIDAlive(rec.OwnerPID) {
		return false
	}
	// pid recycled: same number, different process. Treat as gone.
	if rec.OwnerBoot != "" {
		if current := processStartToken(rec.OwnerPID); current != "" && current != rec.OwnerBoot {
			return false
		}
	}
	return true
}

func chromeRecordReferencedByLiveSession(rec ChromeProcessRecord) bool {
	sessions, err := ListSessions()
	if err != nil {
		return false
	}
	for _, session := range sessions {
		if session.ChromePID > 0 && session.ChromePID == rec.ChromePID {
			return true
		}
		if rec.ProfileDir != "" && session.ProfileDir != "" &&
			filepath.Clean(session.ProfileDir) == rec.ProfileDir {
			return true
		}
	}
	return false
}

// ChromeProfileDirFromArgs pulls --user-data-dir out of a Chrome argv so an
// ownership note can record which profile a pid is holding. That is what lets a
// reaper in another process cross-check "is this pid still using that dir"
// without carrying any in-memory state. Exported for the CLI, which builds its
// own argv rather than going through ChromeLaunchSpec.
func ChromeProfileDirFromArgs(args []string) string {
	for _, arg := range args {
		if value, ok := strings.CutPrefix(arg, "--user-data-dir="); ok {
			return value
		}
	}
	return ""
}

func logChromeRegistryReap(act ChromeReapAction) {
	log.Printf("[CHROME-REAP] reaped abandoned chrome pid=%d pgid=%d kind=%s age_sec=%d profile=%s reason=%s",
		act.ChromePID, act.PGID, act.Kind, act.AgeSec, act.ProfileDir, act.Reason)
}

// EnsureChromeRegistryReaped runs the owner-death reaper once per process, as
// close to "whenever this binary is about to own a Chrome" as possible. Cheap
// (one small directory read) and idempotent, so every launch path can call it
// without coordinating.
func EnsureChromeRegistryReaped() {
	chromeRegistryOnce.Do(func() {
		actions := ReapAbandonedChromeProcesses(ChromeReapOptions{})
		for _, act := range actions {
			if act.Action == "killed" {
				logChromeRegistryReap(act)
			}
		}
	})
}
