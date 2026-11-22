package browser

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// D8：整个 CLI profile 目录的删除统一走"清单 → 校验 → 逐项删除"，以 CLI profile 根为边界。

func hermeticRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "browser-cli")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvBrowserCLIProfileRoot, root)
	return root
}

func makeTree(t *testing.T, dir string) {
	t.Helper()
	writeFileMode(t, filepath.Join(dir, "Default", "Cookies"), []byte("x"), 0o600)
	writeFileMode(t, filepath.Join(dir, "Default", "Cache", "a"), []byte("y"), 0o600)
	writeFileMode(t, filepath.Join(dir, "Local State"), []byte("{}"), 0o600)
}

func TestEnvOverrideIsTheOnlyProfileRoot(t *testing.T) {
	root := hermeticRoot(t)
	if got := BrowserCLIProfileRoots(""); len(got) != 1 || got[0] != root {
		t.Fatalf("roots %v", got)
	}
	if got := BrowserCLIProfileDir("", "x"); got != filepath.Join(root, "x") {
		t.Fatalf("dir %s", got)
	}
	if got := browserCLIDefaultBase(); got != root {
		t.Fatalf("default base %s", got)
	}
}

func TestRemoveCLIProfileDirRemovesTheWholeTreeAndOnlyThat(t *testing.T) {
	skipOnWindows(t)
	root := hermeticRoot(t)
	victim, bystander := filepath.Join(root, "task-1"), filepath.Join(root, "task-10")
	makeTree(t, victim)
	makeTree(t, bystander)
	outside := t.TempDir()
	writeFileMode(t, filepath.Join(outside, "precious"), []byte("keep"), 0o600)
	// profile 里的 symlink 指向根外：只能删链接本身，绝不跟随。
	if err := os.Symlink(outside, filepath.Join(victim, "Default", "link-to-outside")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCLIProfileDir(victim); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(victim); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("victim still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bystander, "Default", "Cookies")); err != nil {
		t.Fatalf("a sibling whose name merely shares a prefix was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "precious")); err != nil {
		t.Fatalf("a symlink was followed out of the profile: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("the root itself must never be removed: %v", err)
	}
}

func TestRemoveCLIProfileDirRefusesAnythingOutsideTheRoot(t *testing.T) {
	root := hermeticRoot(t)
	elsewhere := filepath.Join(t.TempDir(), "task-1")
	makeTree(t, elsewhere)
	nested := filepath.Join(root, "task-1", "Default")
	makeTree(t, filepath.Join(root, "task-1"))

	for name, path := range map[string]string{
		"outside any root":       elsewhere,
		"nested under a profile": nested,
		"the root itself":        root,
		"the root's parent":      filepath.Dir(root),
		"dot-dot escape":         filepath.Join(root, "task-1", "..", "..", filepath.Base(filepath.Dir(elsewhere))),
		"hidden name":            filepath.Join(root, ".dw-vault-master.key"),
	} {
		err := RemoveCLIProfileDir(path)
		if !errors.Is(err, ErrVaultDeleteRefused) {
			t.Errorf("%s: want ErrVaultDeleteRefused, got %v", name, err)
		}
	}
	for _, p := range []string{elsewhere, filepath.Join(root, "task-1", "Default", "Cookies")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("a refused removal must delete nothing, %s: %v", p, err)
		}
	}
}

func TestRemoveCLIProfileDirMissingIsSuccess(t *testing.T) {
	root := hermeticRoot(t)
	if err := RemoveCLIProfileDir(filepath.Join(root, "gone")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveBrowserCLIProfileByID("gone"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveBrowserCLIProfileByIDRefusesPathsDisguisedAsIDs(t *testing.T) {
	root := hermeticRoot(t)
	makeTree(t, filepath.Join(root, "task-1"))
	for _, id := range []string{"..", ".", "../task-1", "task-1/Default", ".hidden", "a/../task-1"} {
		if err := RemoveBrowserCLIProfileByID(id); err == nil {
			t.Errorf("id %q was accepted", id)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "task-1", "Default", "Cookies")); err != nil {
		t.Fatalf("a refused id must delete nothing: %v", err)
	}
	if err := RemoveBrowserCLIProfileByID("task-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "task-1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile still there: %v", err)
	}
}

// profile prune 也走同一个入口，并且仍然保护"在用"的 profile、尊重年龄线。
func TestPruneRemovesThroughTheFunnel(t *testing.T) {
	skipOnWindows(t)
	root := hermeticRoot(t)
	old, young, custom := filepath.Join(root, "task-old"), filepath.Join(root, "task-young"), filepath.Join(root, "my-login")
	for _, d := range []string{old, young, custom} {
		makeTree(t, d)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	res, err := PruneBrowserCLIEphemeralProfiles(BrowserCLIEphemeralPruneOptions{MinAge: time.Hour, Roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 || res.Skipped != 0 {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Lstat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old ephemeral profile not removed: %v", err)
	}
	for _, p := range []string{young, custom} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s must survive prune: %v", p, err)
		}
	}
}

// 新增文件必须纳入"删除必须经过 funnel"的守卫：这几个文件不许自己删东西。
func TestNewLaunchAndShutdownFilesNeverDeleteDirectly(t *testing.T) {
	for _, name := range []string{"cli_profile_launch.go", "cli_profile_remove.go", "chrome_shutdown.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if m := codeLineRE.FindString(codeOnly(string(src))); m != "" {
			t.Fatalf("%s deletes directly (%q); route it through profile_vault_delete.go", name, m)
		}
	}
}
