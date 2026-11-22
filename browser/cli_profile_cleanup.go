package browser

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	BrowserCLIEphemeralProfilePrefix = "ephemeral-"
	DefaultCLIEphemeralPruneMinAge   = 30 * time.Minute

	// EnvBrowserCLIProfileRoot 把 CLI profile 根整体改到另一个目录（唯一的一个根）。
	// 测试靠它不去碰真实的 ~/.deepwork/browser-cli；运维也可以用它把 profile 放到别的盘。
	// 它同时是 profile 删除的边界（RemoveCLIProfileDir 只认这些根的直接子级）。
	EnvBrowserCLIProfileRoot = "DW_BROWSER_CLI_PROFILE_ROOT"
)

// browserCLIProfileRootOverride 返回 EnvBrowserCLIProfileRoot 指定的根（已清理）；未设置返回空串。
func browserCLIProfileRootOverride() string {
	raw := strings.TrimSpace(os.Getenv(EnvBrowserCLIProfileRoot))
	if raw == "" {
		return ""
	}
	return filepath.Clean(raw)
}

type BrowserCLIEphemeralPruneOptions struct {
	ChromePath string
	Roots      []string
	MinAge     time.Duration
	DryRun     bool
	Now        time.Time
	// ReapOrphaned additionally kills any still-alive Chrome process backing
	// an ephemeral-kind (task-/test-/ephemeral-) profile (only a process whose
	// command line carries --user-data-dir=<that profile> — see
	// reapBrowserCLIProfileLiveProcesses) once it's past
	// MinAge, instead of leaving the profile protected forever. Safe because
	// eligibility here already means SessionIsolationEphemeral
	// (session_contract.go): these are agent-owned, single-run profiles by
	// policy, so "alive past MinAge" is not legitimate ongoing use — it's the
	// launching CLI having died (crash/SIGKILL/Ctrl-C) before it could clean
	// up. Chrome is deliberately Setpgid-detached from that death
	// (chrome_handle_unix.go), so nothing else was ever going to reap it.
	// No effect when DryRun is set. Off by default: existing callers that
	// rely on "never touch anything alive" keep that behavior unchanged.
	ReapOrphaned bool
}

type BrowserCLIEphemeralPruneEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	AgeSec    int64  `json:"age_sec,omitempty"`
	// Notes lists pids that --reap-orphaned deliberately did not signal because
	// they are not this profile's Chrome (stale marker, pid reused by another program).
	Notes []string `json:"notes,omitempty"`
}

type BrowserCLIEphemeralPruneResult struct {
	Roots       []string                        `json:"roots"`
	Scanned     int                             `json:"scanned"`
	Removed     int                             `json:"removed"`
	WouldRemove int                             `json:"would_remove,omitempty"`
	Protected   int                             `json:"protected"`
	Skipped     int                             `json:"skipped"`
	Bytes       int64                           `json:"bytes"`
	Entries     []BrowserCLIEphemeralPruneEntry `json:"entries,omitempty"`
}

func NewBrowserCLIEphemeralProfileID() string {
	return fmt.Sprintf("%s%d-%d", BrowserCLIEphemeralProfilePrefix, os.Getpid(), time.Now().UnixNano())
}

// browserCLIDefaultBase 是 NewBrowserCore / document 落 profile 的目录：~/.deepwork/browser-cli，
// 或 EnvBrowserCLIProfileRoot 指定的根。不含 BrowserCLIProfileRoots 里的 snap 特判。
func browserCLIDefaultBase() string {
	if override := browserCLIProfileRootOverride(); override != "" {
		return override
	}
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".deepwork", "browser-cli")
}

func BrowserCLIProfileDir(chromePath string, profileID string) string {
	return filepath.Join(BrowserCLIProfileRoot(chromePath), profileID)
}

func BrowserCLIProfileRoot(chromePath string) string {
	roots := BrowserCLIProfileRoots(chromePath)
	if len(roots) == 0 {
		return ""
	}
	return roots[0]
}

func BrowserCLIProfileRoots(chromePath string) []string {
	if override := browserCLIProfileRootOverride(); override != "" {
		return []string{override}
	}
	homeDir, _ := os.UserHomeDir()
	var roots []string
	if runtime.GOOS == "linux" && UsesSnapSandboxChrome(chromePath) && homeDir != "" {
		roots = append(roots, filepath.Join(homeDir, "snap", "chromium", "common", "dw-browser-cli"))
	}
	if homeDir != "" {
		roots = append(roots, filepath.Join(homeDir, ".deepwork", "browser-cli"))
		if runtime.GOOS == "linux" {
			roots = append(roots, filepath.Join(homeDir, "snap", "chromium", "common", "dw-browser-cli"))
		}
	}
	if runtime.GOOS == "linux" {
		cacheDir := os.Getenv("XDG_CACHE_HOME")
		if cacheDir == "" {
			cacheDir = "/tmp"
		}
		roots = append(roots, filepath.Join(cacheDir, "dw-browser-cli"))
	}
	return dedupeCleanPaths(roots)
}

func UsesSnapSandboxChrome(chromePath string) bool {
	clean := filepath.Clean(strings.TrimSpace(chromePath))
	if strings.HasPrefix(clean, "/snap/") {
		return true
	}
	if runtime.GOOS == "linux" && filepath.Base(clean) == "chromium-browser" {
		if _, err := os.Stat("/snap/bin/chromium"); err == nil {
			return true
		}
	}
	return false
}

func RemoveBrowserCLIEphemeralProfile(profileID string) error {
	if !strings.HasPrefix(profileID, BrowserCLIEphemeralProfilePrefix) {
		return nil
	}
	return RemoveBrowserCLIProfileByID(profileID)
}

// RemoveBrowserCLIProfileByID removes a browser-cli profile directory across
// all profile roots by its profile ID, regardless of naming convention (no
// "ephemeral-" prefix requirement). Callers must already know from
// authoritative session metadata (SessionInfo.Ephemeral) that the profile is
// safe to delete — this is intentionally not a blind-sweep-safe primitive.
//
// The deletion itself goes through the list → validate → delete funnel
// (profile_vault_delete.go), bounded to the CLI profile root: an ID that is not
// a plain directory name is refused instead of being joined into a path.
func RemoveBrowserCLIProfileByID(profileID string) error {
	if strings.TrimSpace(profileID) == "" {
		return nil
	}
	var firstErr error
	for _, root := range BrowserCLIProfileRoots("") {
		if err := removeProfileDirUnderRoot(root, profileID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// browserCLIEphemeralKindPrefixes lists the default-profile-ID prefixes
// (see defaultProfileID in cmd/dw-browser/main.go) for every BrowserSessionKind
// whose policy default is SessionIsolationEphemeral. Derived from
// DefaultsForBrowserSessionKind — the single policy table in
// session_contract.go — instead of hand-duplicated, so a future change to a
// kind's default isolation automatically changes prune eligibility here too.
var browserCLIEphemeralKindPrefixes = ephemeralBrowserCLIKindPrefixes()

func ephemeralBrowserCLIKindPrefixes() []string {
	kinds := []BrowserSessionKind{
		SessionKindTask,
		SessionKindInteractive,
		SessionKindService,
		SessionKindDebug,
		SessionKindTest,
	}
	var prefixes []string
	for _, kind := range kinds {
		if DefaultsForBrowserSessionKind(kind).Isolation == SessionIsolationEphemeral {
			prefixes = append(prefixes, string(kind)+"-")
		}
	}
	return prefixes
}

// browserCLIProfileNameEligibleForPrune reports whether a profile directory
// name is even a *candidate* for PruneBrowserCLIEphemeralProfiles to
// consider — not whether it will actually be removed (age/liveness/owner
// checks still apply after this). This used to require a literal
// "ephemeral-" prefix, which meant task-*/test-* profiles — the default
// session kind, policy-classified ephemeral by session_contract.go — could
// never be reached by the one standalone cleanup command, even after crashing
// or being abandoned with no live session to protect them. Kind-default-
// dedicated names (interactive-/service-/debug-) and any custom/explicit
// --profile name (e.g. a hand-named debugging profile) are intentionally
// excluded: those persist by design and must be removed by a human, not a
// blind sweep.
func browserCLIProfileNameEligibleForPrune(name string) bool {
	if strings.HasPrefix(name, BrowserCLIEphemeralProfilePrefix) {
		return true
	}
	for _, prefix := range browserCLIEphemeralKindPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func PruneBrowserCLIEphemeralProfiles(opts BrowserCLIEphemeralPruneOptions) (BrowserCLIEphemeralPruneResult, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	roots := opts.Roots
	if len(roots) == 0 {
		roots = BrowserCLIProfileRoots(opts.ChromePath)
	}
	roots = dedupeCleanPaths(roots)

	result := BrowserCLIEphemeralPruneResult{Roots: roots}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return result, fmt.Errorf("prune browser cli profiles: read %s: %w", root, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() || !browserCLIProfileNameEligibleForPrune(entry.Name()) {
				continue
			}
			result.Scanned++
			path := filepath.Join(root, entry.Name())
			pruneEntry := BrowserCLIEphemeralPruneEntry{
				Name: entry.Name(),
				Path: path,
			}
			info, err := entry.Info()
			if err != nil {
				result.Skipped++
				pruneEntry.Action = "skipped"
				pruneEntry.Reason = "stat_failed"
				result.Entries = append(result.Entries, pruneEntry)
				continue
			}
			age := now.Sub(info.ModTime())
			if age < 0 {
				age = 0
			}
			pruneEntry.AgeSec = int64(age.Seconds())
			if opts.MinAge > 0 && age < opts.MinAge {
				result.Protected++
				pruneEntry.Action = "protected"
				pruneEntry.Reason = "younger_than_min_age"
				result.Entries = append(result.Entries, pruneEntry)
				continue
			}
			protected, reason, notes := browserCLIProfileProtected(path, opts.ReapOrphaned && !opts.DryRun)
			pruneEntry.Notes = notes
			if protected {
				result.Protected++
				pruneEntry.Action = "protected"
				pruneEntry.Reason = reason
				result.Entries = append(result.Entries, pruneEntry)
				continue
			}
			size := dirSizeBytes(path)
			pruneEntry.SizeBytes = size
			if opts.DryRun {
				pruneEntry.Action = "would_remove"
				result.WouldRemove++
				result.Bytes += size
				result.Entries = append(result.Entries, pruneEntry)
				continue
			} else if err := removeProfileDirUnderRoot(root, entry.Name()); err != nil {
				result.Skipped++
				pruneEntry.Action = "skipped"
				pruneEntry.Reason = "remove_failed"
				result.Entries = append(result.Entries, pruneEntry)
				continue
			}
			pruneEntry.Action = "removed"
			result.Removed++
			result.Bytes += size
			result.Entries = append(result.Entries, pruneEntry)
		}
	}
	sort.Slice(result.Entries, func(i, j int) bool {
		return result.Entries[i].Path < result.Entries[j].Path
	})
	return result, nil
}

func browserCLIProfileProtected(profileDir string, reapOrphaned bool) (bool, string, []string) {
	liveSession := browserCLIProfileReferencedByLiveSession(profileDir)
	liveOwner := browserCLIProfileHasLiveOwnerMarker(profileDir)
	livePIDs := platformFindChromePIDsByProfileDir(profileDir)
	if !liveSession && !liveOwner && len(livePIDs) == 0 {
		return false, "", nil
	}

	if reapOrphaned {
		reaped, notes := reapBrowserCLIProfileLiveProcesses(profileDir)
		if reaped {
			return false, "", notes
		}
		return true, browserCLIProfileProtectionReason(liveSession, liveOwner), notes
	}
	return true, browserCLIProfileProtectionReason(liveSession, liveOwner), nil
}

func browserCLIProfileProtectionReason(liveSession, liveOwner bool) string {
	switch {
	case liveSession:
		return "live_session"
	case liveOwner:
		return "live_owner_marker"
	default:
		return "live_chrome_user_data_dir"
	}
}

// browserCLIProfileLivePIDs collects every PID any tracking mechanism (a
// live session file, an owner marker, or a ps scan for --user-data-dir=)
// claims is still using profileDir. These are claims, not facts: a marker can
// outlive its process by days and the pid can since belong to any program. The
// reaper therefore verifies each one before signalling it
// (reapBrowserCLIProfileLiveProcesses).
//
// Reaping must cover all verified Chrome processes — a PID tracked by only one
// source would keep the directory non-removable (zygote/renderer/gpu-process/
// crashpad_handler children resurrect files RemoveAll already reported
// removing, same reason KillChromeProcessGroup targets the whole pgid instead
// of a single PID).
func browserCLIProfileLivePIDs(profileDir string) []int {
	self := os.Getpid()
	var pids []int
	seen := map[int]bool{}
	add := func(pid int) {
		if pid > 0 && pid != self && !seen[pid] {
			seen[pid] = true
			pids = append(pids, pid)
		}
	}

	if sessions, err := ListSessions(); err == nil {
		cleanDir := filepath.Clean(profileDir)
		for _, session := range sessions {
			if !session.Ephemeral && !strings.HasPrefix(session.ProfileID, BrowserCLIEphemeralProfilePrefix) {
				continue
			}
			if session.ProfileDir == "" || filepath.Clean(session.ProfileDir) != cleanDir {
				continue
			}
			add(session.ChromePID)
			add(session.BrowserMuxHostPID)
		}
	}

	if body, err := os.ReadFile(filepath.Join(profileDir, deepworkProfileOwnerFile)); err == nil {
		var marker deepworkProfileOwner
		if json.Unmarshal(body, &marker) == nil {
			add(marker.ChromePID)
			add(marker.BrowserMuxHostPID)
			add(marker.OwnerPID)
		}
	}

	for _, pid := range platformFindChromePIDsByProfileDir(profileDir) {
		add(pid)
	}

	return pids
}

// reapBrowserCLIProfileLiveProcesses kills every PID browserCLIProfileLivePIDs
// finds for profileDir that is really that profile's Chrome, and waits briefly
// for them to actually exit. Reports true only if nothing of ours is left alive
// — a genuinely stuck process (e.g. D state) must keep the profile protected,
// not be treated as reaped.
//
// Identity first: before any signal, the pid's command line must carry
// `--user-data-dir=<profileDir>` (requireChromeOfProfilePID). Everything else
// the tracking sources named — a recycled pid now running an unrelated program
// (other-muxd included), the owner CLI, the shared BrowserMuxHost, or the
// Chrome of a sibling profile whose directory name merely starts the same
// (task-1 vs task-10, which the ps scan's substring match cannot tell apart) —
// is not signalled and not counted as using the profile; each is returned as a
// note instead. The mux host in particular serves every session, so one orphaned
// profile must not take it down; it is closed through `muxhost shutdown`.
func reapBrowserCLIProfileLiveProcesses(profileDir string) (bool, []string) {
	var ours []int
	var notes []string
	for _, pid := range browserCLIProfileLivePIDs(profileDir) {
		if !isPIDAlive(pid) {
			continue
		}
		if err := requireChromeOfProfilePID(pid, profileDir); err != nil {
			note := fmt.Sprintf("pid %d not signalled: %v", pid, err)
			log.Printf("[PROFILE-PRUNE] profile %s: %s", profileDir, note)
			notes = append(notes, note)
			continue
		}
		ours = append(ours, pid)
	}
	anyAlive := func() bool {
		for _, pid := range ours {
			if isPIDAlive(pid) {
				return true
			}
		}
		return false
	}
	if !anyAlive() {
		return true, notes
	}
	for _, pid := range ours {
		KillChromeProcessGroup(pid)
	}
	deadline := time.Now().Add(ProfileOwnerChromeKillGrace)
	for time.Now().Before(deadline) {
		if !anyAlive() {
			return true, notes
		}
		time.Sleep(ProcessExitPollInterval)
	}
	return !anyAlive(), notes
}

func browserCLIProfileReferencedByLiveSession(profileDir string) bool {
	sessions, err := ListSessions()
	if err != nil {
		return false
	}
	cleanProfileDir := filepath.Clean(profileDir)
	for _, session := range sessions {
		if !session.Ephemeral && !strings.HasPrefix(session.ProfileID, BrowserCLIEphemeralProfilePrefix) {
			continue
		}
		if session.ProfileDir == "" || filepath.Clean(session.ProfileDir) != cleanProfileDir {
			continue
		}
		if session.ChromePID > 0 && isPIDAlive(session.ChromePID) {
			return true
		}
		if session.BrowserMuxHostPID > 0 && isPIDAlive(session.BrowserMuxHostPID) {
			return true
		}
		if session.DebugPort > 0 {
			if _, err := FetchChromeTargets(session.DebugPort); err == nil {
				return true
			}
		}
	}
	return false
}

func browserCLIProfileHasLiveOwnerMarker(profileDir string) bool {
	body, err := os.ReadFile(filepath.Join(profileDir, deepworkProfileOwnerFile))
	if err != nil || len(body) == 0 {
		return false
	}
	var marker deepworkProfileOwner
	if err := json.Unmarshal(body, &marker); err != nil {
		return false
	}
	if marker.ChromePID > 0 && marker.ChromePID != os.Getpid() && isPIDAlive(marker.ChromePID) {
		return true
	}
	if marker.BrowserMuxHostPID > 0 && marker.BrowserMuxHostPID != os.Getpid() && isPIDAlive(marker.BrowserMuxHostPID) {
		return true
	}
	if marker.OwnerPID > 0 && marker.OwnerPID != os.Getpid() && isPIDAlive(marker.OwnerPID) {
		return true
	}
	return false
}

func dedupeCleanPaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		clean := filepath.Clean(path)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	return out
}

func dirSizeBytes(root string) int64 {
	var size int64
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, infoErr := d.Info(); infoErr == nil {
			size += info.Size()
		}
		return nil
	})
	return size
}
