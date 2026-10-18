package browser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupFailedBrowserMuxProfileReapsEarlyMarkerWithoutManifest(t *testing.T) {
	profile := t.TempDir()
	if err := WriteProfileOwnerMarkerWithMetadata(profile, "test-failed-runtime", 999999, 0, ProfileOwnerMetadata{RuntimeID: "rt-failed", DisplayBackend: "cgvirtualdisplay"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(profile, deepworkProfileOwnerFile)); err != nil {
		t.Fatal(err)
	}
	cleanupFailedBrowserMuxProfile(profile)
	if _, err := os.Stat(filepath.Join(profile, deepworkProfileOwnerFile)); !os.IsNotExist(err) {
		t.Fatalf("early owner marker remains after failed-runtime cleanup: %v", err)
	}
}

func TestCleanupFailedBrowserMuxProfileIgnoresMalformedMarker(t *testing.T) {
	profile := t.TempDir()
	path := filepath.Join(profile, deepworkProfileOwnerFile)
	if err := os.WriteFile(path, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanupFailedBrowserMuxProfile(profile)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("malformed marker should remain for diagnosis: %v", err)
	}
}
