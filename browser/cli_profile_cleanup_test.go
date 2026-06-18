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
	persistent := filepath.Join(root, "task-kept")
	writeProfileFile(t, oldEphemeral, "payload")
	writeProfileFile(t, youngEphemeral, "payload")
	writeProfileFile(t, persistent, "payload")
	mustChtimes(t, oldEphemeral, old)
	mustChtimes(t, youngEphemeral, young)
	mustChtimes(t, persistent, old)

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
	if _, err := os.Stat(persistent); err != nil {
		t.Fatalf("persistent profile should not be scanned: %v", err)
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
