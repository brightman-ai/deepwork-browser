package browser

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// removeTreeFixture 造一个 profiles 根目录：里面有要删的 p1、同前缀的兄弟目录、无关目录和文件，
// 另外有一个"根目录之外"的目录（含一个与根同级的 victim）。
type removeTreeFixture struct {
	parent  string // root 的父目录（里面还有 victim）
	root    string // 已解析 symlink 的 profiles 根
	outDir  string // 完全在外面的目录
	outFile string
}

func newRemoveTreeFixture(t *testing.T) removeTreeFixture {
	t.Helper()
	skipOnWindows(t)
	f := removeTreeFixture{parent: realTempDir(t)}
	f.root = filepath.Join(f.parent, "profiles")
	if err := os.MkdirAll(f.root, 0o700); err != nil {
		t.Fatal(err)
	}
	f.outDir, f.outFile = outsideFixture(t)

	// 要删的 p1：多层目录、空目录、很多文件、指向外部的 symlink（目录 / 文件 / 悬空）。
	for i := 0; i < 300; i++ {
		writeFileMode(t, filepath.Join(f.root, "p1", "Default", "Cache", "Cache_Data", "f_"+strings.Repeat("0", 3)+string(rune('a'+i%26))+string(rune('a'+i/26%26))), []byte("c"), 0o600)
	}
	writeFileMode(t, filepath.Join(f.root, "p1", "Default", "Cookies"), []byte("cookies"), 0o600)
	writeFileMode(t, filepath.Join(f.root, "p1", "Local State"), []byte("{}"), 0o600)
	if err := os.MkdirAll(filepath.Join(f.root, "p1", "Default", "empty-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		filepath.Join("p1", "Default", "link-to-outside-dir"): f.outDir,
		filepath.Join("p1", "link-to-outside-file"):           f.outFile,
		filepath.Join("p1", "dangling"):                       filepath.Join(f.outDir, "no-such-thing"),
		filepath.Join("p1", "SingletonLock"):                  "host-1",
		filepath.Join("p1", "Default", "link-to-root-parent"): f.parent,
	} {
		if err := os.Symlink(target, filepath.Join(f.root, link)); err != nil {
			t.Fatal(err)
		}
	}

	// 邻居：绝不能被碰。
	writeFileMode(t, filepath.Join(f.root, "p2", "Default", "Cookies"), []byte("p2-cookies"), 0o600)
	writeFileMode(t, filepath.Join(f.root, "p1-sibling", "keep.txt"), []byte("sibling"), 0o600)
	writeFileMode(t, filepath.Join(f.root, "p10", "keep.txt"), []byte("p10"), 0o600)
	writeFileMode(t, filepath.Join(f.root, "x.txt"), []byte("x"), 0o600)
	writeFileMode(t, filepath.Join(f.parent, "victim", "precious.txt"), []byte("PRECIOUS"), 0o600)
	return f
}

func (f removeTreeFixture) requireUntouched(t *testing.T) {
	t.Helper()
	for rel, want := range map[string]string{
		"profiles/p2/Default/Cookies":  "p2-cookies",
		"profiles/p1-sibling/keep.txt": "sibling",
		"profiles/p10/keep.txt":        "p10",
		"profiles/x.txt":               "x",
		"victim/precious.txt":          "PRECIOUS",
	} {
		got, err := os.ReadFile(filepath.Join(f.parent, filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("%s was touched: %q %v", rel, got, err)
		}
	}
	requireOutsideIntact(t, f.outDir)
	if _, err := os.Stat(f.root); err != nil {
		t.Errorf("root itself disappeared: %v", err)
	}
}

func TestRemoveProfileTreeDeletesOnlyTheNamedDirectory(t *testing.T) {
	f := newRemoveTreeFixture(t)
	if err := RemoveProfileTree(f.root, "p1"); err != nil {
		t.Fatalf("RemoveProfileTree: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "p1")); !os.IsNotExist(err) {
		t.Fatalf("p1 should be gone: %v", err)
	}
	f.requireUntouched(t)
	if names := dirNames(t, f.root); strings.Join(names, ",") != "p1-sibling,p10,p2,x.txt" {
		t.Fatalf("root entries: %v", names)
	}
	// 幂等：已经不在了不是错误。
	if err := RemoveProfileTree(f.root, "p1"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	// 通过 symlink 指向 root 也行：边界按解析后的真实路径算。
	link := filepath.Join(realTempDir(t), "root-link")
	if err := os.Symlink(f.root, link); err != nil {
		t.Fatal(err)
	}
	if err := RemoveProfileTree(link, "p2"); err != nil {
		t.Fatalf("via a symlinked root: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.root, "p2")); !os.IsNotExist(err) {
		t.Fatalf("p2 should be gone via the symlinked root: %v", err)
	}
}

func TestRemoveProfileTreeRefusesNamesThatAreNotASingleSegment(t *testing.T) {
	f := newRemoveTreeFixture(t)
	for name, n := range map[string]string{
		"empty":                    "",
		"dot":                      ".",
		"dotdot":                   "..",
		"leading dot-slash":        "./p1",
		"trailing slash":           "p1/",
		"trailing dot":             "p1/.",
		"nested":                   "p1/Default",
		"absolute":                 filepath.Join(f.root, "p1"),
		"absolute outside":         filepath.Join(f.parent, "victim"),
		"parent escape":            "../victim",
		"parent escape via p1":     "p1/../../victim",
		"backslash":                `p1\Default`,
		"NUL byte":                 "p1\x00evil",
		"longer than NAME_MAX":     strings.Repeat("x", 256),
		"root-relative parent dir": "../profiles",
	} {
		t.Run(name, func(t *testing.T) {
			requireDeleteRefused(t, RemoveProfileTree(f.root, n))
			f.requireUntouched(t)
			if _, err := os.Stat(filepath.Join(f.root, "p1", "Default", "Cookies")); err != nil {
				t.Fatalf("p1 must be intact after a refused call: %v", err)
			}
		})
	}
}

func TestRemoveProfileTreeOnlyDeletesRealDirectories(t *testing.T) {
	f := newRemoveTreeFixture(t)
	if err := os.Symlink(f.outDir, filepath.Join(f.root, "link-to-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.outFile, filepath.Join(f.root, "link-to-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.root, filepath.Join(f.root, "link-to-root")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"link-to-dir", "link-to-file", "link-to-root", "x.txt"} {
		t.Run(name, func(t *testing.T) {
			requireDeleteRefused(t, RemoveProfileTree(f.root, name))
			if _, err := os.Lstat(filepath.Join(f.root, name)); err != nil {
				t.Fatalf("%s must still exist: %v", name, err)
			}
			f.requireUntouched(t)
		})
	}
}

func TestRemoveProfileTreeRefusesDangerousOrBrokenRoots(t *testing.T) {
	skipOnWindows(t)
	f := newRemoveTreeFixture(t)

	// 文件系统根：任何子目录都不许碰。名字特意取一个不存在的，万一防线失效也不会误删任何东西。
	requireDeleteRefused(t, RemoveProfileTree("/", "dw-browser-test-no-such-directory"))
	requireDeleteRefused(t, RemoveProfileTree("", "p1"))
	requireDeleteRefused(t, RemoveProfileTree("   ", "p1"))
	requireDeleteRefused(t, RemoveProfileTree(filepath.Join(f.root, "x.txt"), "p1")) // root 是文件

	if err := RemoveProfileTree(filepath.Join(f.parent, "no-such-root"), "p1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing root is a caller bug and must not be reported as success: %v", err)
	}
	f.requireUntouched(t)
	if _, err := os.Stat(filepath.Join(f.root, "p1", "Local State")); err != nil {
		t.Fatalf("p1 must be intact: %v", err)
	}
}

// 公开入口永远只会生成"都在指定目录之下"的清单，所以这条校验只能直接对清单测：
// 篡改后的清单里混进别的顶层目录下的项，或者没有指定目录名，整份清单都必须被拒绝，什么都不删。
func TestDeletePlanRefusesTreeItemsOutsideTheDesignatedDirectory(t *testing.T) {
	f := newRemoveTreeFixture(t)
	items, err := planSubtrees(f.root, func(top string) bool { return top == "p1" || top == "p2" }, vaultItemTree)
	if err != nil {
		t.Fatal(err)
	}
	for name, treeName := range map[string]string{
		"plan designates p1 but also lists p2": "p1",
		"plan designates nothing":              "",
		"plan designates a different name":     "p1-sibling",
	} {
		t.Run(name, func(t *testing.T) {
			plan := &vaultDeletePlan{root: f.root, keep: newVaultKeep(), items: items, treeName: treeName}
			requireDeleteRefused(t, plan.execute())
			f.requireUntouched(t)
			if _, err := os.Stat(filepath.Join(f.root, "p2", "Default", "Cookies")); err != nil {
				t.Fatalf("p2 was deleted by a refused plan: %v", err)
			}
			if _, err := os.Stat(filepath.Join(f.root, "p1", "Local State")); err != nil {
				t.Fatalf("p1 was deleted by a refused plan: %v", err)
			}
		})
	}
}
