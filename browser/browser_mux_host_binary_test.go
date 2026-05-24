package browser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveBrowserMuxHostBinary_AppBundlePrefersExternalPeer(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	appMacOS := filepath.Join(binDir, "Deepwork.app", "Contents", "MacOS")
	if err := os.MkdirAll(appMacOS, 0755); err != nil {
		t.Fatal(err)
	}
	appExe := filepath.Join(appMacOS, "Deepwork")
	bundled := filepath.Join(appMacOS, "dw-browser")
	external := filepath.Join(binDir, "dw-browser")
	for _, path := range []string{appExe, bundled, external} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	got, err := resolveBrowserMuxHostBinaryFrom("", appExe, root)
	if err != nil {
		t.Fatalf("resolve muxhost binary: %v", err)
	}
	if got != external {
		t.Fatalf("expected external peer %s, got %s", external, got)
	}
}

func TestResolveBrowserMuxHostBinary_AppBundleInstallsStableCopy(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	t.Setenv(browserMuxHostBinaryCacheEnv, cacheDir)

	appMacOS := filepath.Join(root, "Deepwork.app", "Contents", "MacOS")
	if err := os.MkdirAll(appMacOS, 0755); err != nil {
		t.Fatal(err)
	}
	appExe := filepath.Join(appMacOS, "Deepwork")
	bundled := filepath.Join(appMacOS, "dw-browser")
	if err := os.WriteFile(appExe, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundled, []byte("#!/bin/sh\necho muxhost\n"), 0755); err != nil {
		t.Fatal(err)
	}

	got, err := resolveBrowserMuxHostBinaryFrom("", appExe, root)
	if err != nil {
		t.Fatalf("resolve muxhost binary: %v", err)
	}
	want := filepath.Join(cacheDir, browserMuxHostBinaryName())
	if got != want {
		t.Fatalf("expected stable copied binary %s, got %s", want, got)
	}
	if pathLooksInsideMacAppBundle(got) {
		t.Fatalf("stable muxhost binary path must not be inside app bundle: %s", got)
	}
	body, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read copied binary: %v", err)
	}
	if string(body) != "#!/bin/sh\necho muxhost\n" {
		t.Fatalf("copied binary content mismatch: %q", string(body))
	}
}

func TestResolveBrowserMuxHostBinary_ExplicitEnvWins(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "custom-dw-browser")
	t.Setenv(browserMuxHostBinaryEnv, explicit)
	got, err := resolveBrowserMuxHostBinaryFrom("", "/tmp/Deepwork.app/Contents/MacOS/Deepwork", t.TempDir())
	if err != nil {
		t.Fatalf("resolve muxhost binary: %v", err)
	}
	if got != explicit {
		t.Fatalf("expected env binary %s, got %s", explicit, got)
	}
}
