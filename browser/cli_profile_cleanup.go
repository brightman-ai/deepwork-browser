package browser

import (
	"encoding/json"
	"fmt"
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
)

type BrowserCLIEphemeralPruneOptions struct {
	ChromePath string
	Roots      []string
	MinAge     time.Duration
	DryRun     bool
	Now        time.Time
}

type BrowserCLIEphemeralPruneEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	AgeSec    int64  `json:"age_sec,omitempty"`
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
	var firstErr error
	for _, root := range BrowserCLIProfileRoots("") {
		if err := os.RemoveAll(filepath.Join(root, profileID)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), BrowserCLIEphemeralProfilePrefix) {
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
			if protected, reason := browserCLIProfileProtected(path); protected {
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
			} else if err := os.RemoveAll(path); err != nil {
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

func browserCLIProfileProtected(profileDir string) (bool, string) {
	if browserCLIProfileReferencedByLiveSession(profileDir) {
		return true, "live_session"
	}
	if browserCLIProfileHasLiveOwnerMarker(profileDir) {
		return true, "live_owner_marker"
	}
	if len(platformFindChromePIDsByProfileDir(profileDir)) > 0 {
		return true, "live_chrome_user_data_dir"
	}
	return false, ""
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
