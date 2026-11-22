//go:build !windows

package browser

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// openFDsOnFile 数本进程里有多少个打开的 fd 指向 path 这个文件（按 dev+inode 比对）。
func openFDsOnFile(t *testing.T, path string) int {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	want, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no stat_t")
	}
	n := 0
	for fd := 3; fd < 2048; fd++ {
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			continue
		}
		if st.Ino == want.Ino && st.Dev == want.Dev {
			n++
		}
	}
	return n
}

// umask 会从 MkdirAll 的 0700 里再去掉位；极端 umask 下新建的目录会变成 0500 之类，之后连文件都建不进去。
// mkdirAllPrivate 必须把"自己新建的"每一级收紧回 0700，且不动已存在的上级目录。
func TestMkdirAllPrivateOverridesARestrictiveUmask(t *testing.T) {
	base := realTempDir(t)
	existing := filepath.Join(base, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0o755); err != nil {
		t.Fatal(err)
	}

	old := syscall.Umask(0o277)
	defer syscall.Umask(old)
	target := filepath.Join(existing, "a", "b", "c")
	if err := mkdirAllPrivate(target); err != nil {
		t.Fatal(err)
	}
	syscall.Umask(old)

	for _, d := range []string{filepath.Join(existing, "a"), filepath.Join(existing, "a", "b"), target} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s created with mode %v, want 0700", d, fi.Mode().Perm())
		}
	}
	if fi, _ := os.Stat(existing); fi.Mode().Perm() != 0o755 {
		t.Errorf("pre-existing parent was re-permissioned: %v", fi.Mode().Perm())
	}
}

// 8. 删除 vault 之前必须先关掉句柄（Windows 不允许删除打开着的文件）。
func TestUnsealClosesTheVaultHandleBeforeRemovingIt(t *testing.T) {
	skipOnWindows(t)
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	mustSeal(t, dir, ks)

	open := -1
	vaultBeforeVaultRemoveHook = func(path string) { open = openFDsOnFile(t, path) }
	defer func() { vaultBeforeVaultRemoveHook = nil }()
	mustUnseal(t, dir, ks)
	if open != 0 {
		t.Fatalf("open handles on the vault when it was removed: %d (-1 means the hook never ran)", open)
	}
}
