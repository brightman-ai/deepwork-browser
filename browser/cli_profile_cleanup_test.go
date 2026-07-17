package browser

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneBrowserCLIEphemeralProfilesRemovesOnlyStaleEphemeralDirs(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	young := now.Add(-5 * time.Minute)

	oldEphemeral := filepath.Join(root, "ephemeral-old")
	youngEphemeral := filepath.Join(root, "ephemeral-young")
	// interactive-/service-/debug- are policy-dedicated (session_contract.go),
	// never prune-eligible regardless of age — unlike task-/test-, see
	// TestPruneBrowserCLIEphemeralProfilesReachesOrphanedTaskAndTestDirs below.
	dedicated := filepath.Join(root, "interactive-kept")
	writeProfileFile(t, oldEphemeral, "payload")
	writeProfileFile(t, youngEphemeral, "payload")
	writeProfileFile(t, dedicated, "payload")
	mustChtimes(t, oldEphemeral, old)
	mustChtimes(t, youngEphemeral, young)
	mustChtimes(t, dedicated, old)

	result, err := PruneBrowserCLIEphemeralProfiles(BrowserCLIEphemeralPruneOptions{
		Roots:  []string{root},
		MinAge: 30 * time.Minute,
		Now:    now,
	})
	if err != nil {
		t.Fatalf("PruneBrowserCLIEphemeralProfiles() error = %v", err)
	}
	if result.Scanned != 2 || result.Removed != 1 || result.Protected != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, err := os.Stat(oldEphemeral); !os.IsNotExist(err) {
		t.Fatalf("old ephemeral should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(youngEphemeral); err != nil {
		t.Fatalf("young ephemeral should be protected: %v", err)
	}
	if _, err := os.Stat(dedicated); err != nil {
		t.Fatalf("dedicated-kind profile should not be scanned: %v", err)
	}
}

// TestPruneBrowserCLIEphemeralProfilesReachesOrphanedTaskAndTestDirs closes
// the actual incident: task-*/test-* are the default session kinds and are
// policy-classified SessionIsolationEphemeral (session_contract.go), but
// their directories were never named "ephemeral-*" — so the old
// strings.HasPrefix(name, "ephemeral-") gate meant an abandoned task-*/test-*
// profile (crashed, killed, or just never explicitly closed) had no cleanup
// path at all, ever. This accumulated 19GB/475 dirs on a real machine before
// browserCLIProfileNameEligibleForPrune replaced the name check with one
// derived from the same policy table close() already uses.
func TestPruneBrowserCLIEphemeralProfilesReachesOrphanedTaskAndTestDirs(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := now.Add(-2 * time.Hour)

	orphanTask := filepath.Join(root, "task-1783852512539722848")
	orphanTest := filepath.Join(root, "test-dw-test-1783852512539722848")
	writeProfileFile(t, orphanTask, "payload")
	writeProfileFile(t, orphanTest, "payload")
	mustChtimes(t, orphanTask, old)
	mustChtimes(t, orphanTest, old)

	result, err := PruneBrowserCLIEphemeralProfiles(BrowserCLIEphemeralPruneOptions{
		Roots:  []string{root},
		MinAge: 30 * time.Minute,
		Now:    now,
	})
	if err != nil {
		t.Fatalf("PruneBrowserCLIEphemeralProfiles() error = %v", err)
	}
	if result.Scanned != 2 || result.Removed != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, err := os.Stat(orphanTask); !os.IsNotExist(err) {
		t.Fatalf("orphaned task- dir should be removed, stat err=%v", err)
	}
	if _, err := os.Stat(orphanTest); !os.IsNotExist(err) {
		t.Fatalf("orphaned test- dir should be removed, stat err=%v", err)
	}
}

// TestPruneBrowserCLIEphemeralProfilesProtectsLiveTaskSession proves the
// widened scan doesn't blindly nuke an in-use task- session: liveness
// protection (browserCLIProfileProtected) still applies exactly as it does
// for ephemeral-* today.
func TestPruneBrowserCLIEphemeralProfilesProtectsLiveTaskSession(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	dir := filepath.Join(root, "task-live")
	writeProfileFile(t, dir, "payload")
	mustChtimes(t, dir, now.Add(-2*time.Hour))

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	body := fmt.Sprintf(`{"chrome_pid":%d,"identity_key":"test-live-task"}`, cmd.Process.Pid)
	if err := os.WriteFile(filepath.Join(dir, deepworkProfileOwnerFile), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := PruneBrowserCLIEphemeralProfiles(BrowserCLIEphemeralPruneOptions{
		Roots: []string{root},
		Now:   now,
	})
	if err != nil {
		t.Fatalf("PruneBrowserCLIEphemeralProfiles() error = %v", err)
	}
	if result.Removed != 0 || result.Protected != 1 {
		t.Fatalf("live task- session should be protected: %+v", result)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("live task- profile dir should remain: %v", err)
	}
}

func TestPruneBrowserCLIEphemeralProfilesProtectsLiveOwnerMarker(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	dir := filepath.Join(root, "ephemeral-live")
	writeProfileFile(t, dir, "payload")
	mustChtimes(t, dir, now.Add(-2*time.Hour))

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	body := fmt.Sprintf(`{"chrome_pid":%d,"identity_key":"test-live"}`, cmd.Process.Pid)
	if err := os.WriteFile(filepath.Join(dir, deepworkProfileOwnerFile), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}

	result, err := PruneBrowserCLIEphemeralProfiles(BrowserCLIEphemeralPruneOptions{
		Roots: []string{root},
		Now:   now,
	})
	if err != nil {
		t.Fatalf("PruneBrowserCLIEphemeralProfiles() error = %v", err)
	}
	if result.Removed != 0 || result.Protected != 1 {
		t.Fatalf("live owner should be protected: %+v", result)
	}
	if got := result.Entries[0].Reason; got != "live_owner_marker" {
		t.Fatalf("reason = %q, want live_owner_marker", got)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("live profile dir should remain: %v", err)
	}
}

// TestPruneBrowserCLIEphemeralProfilesReapsOrphanedLiveSession closes the
// real-world incident (2026-07-15): task-/test- Chrome instances left running
// for hours after their launching CLI died (crash/SIGKILL/Ctrl-C) — Setpgid
// deliberately decouples Chrome from that death (chrome_handle_unix.go), and
// the default prune protects anything alive forever, so nothing ever reaped
// them. 9 such orphans (57 processes) were found driving load average to
// 76+ on a 16-core box. ReapOrphaned closes that gap: an ephemeral-kind
// profile alive past MinAge with no legitimate reason to still be running
// gets its Chrome process group killed, then removed like any stale dir.
func TestPruneBrowserCLIEphemeralProfilesReapsOrphanedLiveSession(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	dir := filepath.Join(root, "task-orphaned")
	writeProfileFile(t, dir, "payload")

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	pid := cmd.Process.Pid
	// Reap promptly in the background: isPIDAlive's kill(pid, 0) still
	// reports a killed-but-unwaited child as alive (zombie) until something
	// calls Wait() on it — unlike a real orphan (reparented to init, which
	// reaps automatically), this test's child is ours until we let it go.
	waited := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(waited)
	}()
	defer func() {
		_ = cmd.Process.Kill()
		<-waited
	}()
	body := fmt.Sprintf(`{"chrome_pid":%d,"identity_key":"test-orphan"}`, pid)
	if err := os.WriteFile(filepath.Join(dir, deepworkProfileOwnerFile), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	// Writing the marker above just bumped dir's mtime — set the "abandoned
	// 2 hours ago" age last so it sticks.
	mustChtimes(t, dir, now.Add(-2*time.Hour))

	result, err := PruneBrowserCLIEphemeralProfiles(BrowserCLIEphemeralPruneOptions{
		Roots:        []string{root},
		MinAge:       30 * time.Minute,
		ReapOrphaned: true,
		Now:          now,
	})
	if err != nil {
		t.Fatalf("PruneBrowserCLIEphemeralProfiles() error = %v", err)
	}
	if result.Removed != 1 || result.Protected != 0 {
		t.Fatalf("orphaned live task- session should be reaped and removed: %+v", result)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("reaped profile dir should be removed, stat err=%v", err)
	}
	if isPIDAlive(pid) {
		t.Fatalf("orphaned process pid=%d should have been killed", pid)
	}
}

// TestPruneBrowserCLIEphemeralProfilesReapOrphanedRespectsDryRun proves
// --dry-run stays side-effect-free even with ReapOrphaned set: DryRun must
// never kill a live process, only report what would happen.
func TestPruneBrowserCLIEphemeralProfilesReapOrphanedRespectsDryRun(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	dir := filepath.Join(root, "task-dryrun-live")
	writeProfileFile(t, dir, "payload")

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	body := fmt.Sprintf(`{"chrome_pid":%d,"identity_key":"test-dryrun-live"}`, pid)
	if err := os.WriteFile(filepath.Join(dir, deepworkProfileOwnerFile), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	mustChtimes(t, dir, now.Add(-2*time.Hour))

	result, err := PruneBrowserCLIEphemeralProfiles(BrowserCLIEphemeralPruneOptions{
		Roots:        []string{root},
		MinAge:       30 * time.Minute,
		ReapOrphaned: true,
		DryRun:       true,
		Now:          now,
	})
	if err != nil {
		t.Fatalf("PruneBrowserCLIEphemeralProfiles() error = %v", err)
	}
	if result.Removed != 0 || result.Protected != 1 {
		t.Fatalf("dry-run must not reap: %+v", result)
	}
	if got := result.Entries[0].Reason; got != "live_owner_marker" {
		t.Fatalf("reason = %q, want live_owner_marker (age gate should not be why it's protected)", got)
	}
	if !isPIDAlive(pid) {
		t.Fatalf("dry-run must not kill live process pid=%d", pid)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dry-run profile dir should remain: %v", err)
	}
}

func TestPruneBrowserCLIEphemeralProfilesDryRunDoesNotDelete(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	dir := filepath.Join(root, "ephemeral-dry")
	writeProfileFile(t, dir, "payload")
	mustChtimes(t, dir, now.Add(-2*time.Hour))

	result, err := PruneBrowserCLIEphemeralProfiles(BrowserCLIEphemeralPruneOptions{
		Roots:  []string{root},
		DryRun: true,
		Now:    now,
	})
	if err != nil {
		t.Fatalf("PruneBrowserCLIEphemeralProfiles() error = %v", err)
	}
	if result.Removed != 0 || result.WouldRemove != 1 || result.Entries[0].Action != "would_remove" {
		t.Fatalf("dry run should report would_remove: %+v", result)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dry-run profile dir should remain: %v", err)
	}
}

func writeProfileFile(t *testing.T, dir, data string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func mustChtimes(t *testing.T, path string, ts time.Time) {
	t.Helper()
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatal(err)
	}
}
