package browser

import (
	"archive/tar"
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ------------------------------------------------------------
// 夹具
// ------------------------------------------------------------

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix-only fixture (symlinks, file modes)")
	}
}

func testKeyStore(t *testing.T) KeyStore {
	t.Helper()
	return NewFileKeyStore(filepath.Join(t.TempDir(), "keys", "master.key"))
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFileMode(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// newTestProfile 造一个 vault=open 的持久 profile，里面有多层目录、空文件、空目录、
// 二进制、>1MiB 跨块文件、恰好 1MiB 的文件、不同权限。
func newTestProfile(t *testing.T) (dir string, m *ProfileManifest) {
	t.Helper()
	skipOnWindows(t)
	dir = filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFileMode(t, filepath.Join(dir, "Local State"), []byte(`{"os_crypt":{"x":1}}`), 0o600)
	writeFileMode(t, filepath.Join(dir, "Default", "Preferences"), []byte(`{"a":"SECRET-MARKER-PREFS"}`), 0o644)
	writeFileMode(t, filepath.Join(dir, "Default", "Cookies"), append([]byte("SQLite format 3\x00SECRET-MARKER-COOKIES"), randBytes(t, 20000)...), 0o600)
	writeFileMode(t, filepath.Join(dir, "Default", "empty.bin"), nil, 0o600)
	writeFileMode(t, filepath.Join(dir, "Default", "Local Storage", "leveldb", "000003.log"), randBytes(t, 4096), 0o600)
	writeFileMode(t, filepath.Join(dir, "Default", "IndexedDB", "a", "b", "c", "deep.bin"), randBytes(t, 1<<20+37), 0o600)
	writeFileMode(t, filepath.Join(dir, "Default", "exactly-1MiB.bin"), randBytes(t, 1<<20), 0o600)
	writeFileMode(t, filepath.Join(dir, "big-3MiB.bin"), randBytes(t, 3<<20+17), 0o600)
	writeFileMode(t, filepath.Join(dir, "Default", "tool.sh"), []byte("#!/bin/sh\n"), 0o755)
	if err := os.MkdirAll(filepath.Join(dir, "Default", "empty-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	var err error
	m, err = NewProfileManifest(ProfileOSCryptMock, ProfileVaultOpen, "headed")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteProfileManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	return dir, m
}

// snapshotTree 记录目录树的 相对路径 → "类型 权限 大小 sha256"（不含清单和封存文件）。
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." || rel == ProfileManifestFile || rel == ProfileVaultFile {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = "link " + target
			return nil
		}
		if info.IsDir() {
			out[rel] = fmt.Sprintf("dir %04o", info.Mode().Perm())
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		out[rel] = fmt.Sprintf("file %04o %d %x", info.Mode().Perm(), info.Size(), sum[:8])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffTrees(t *testing.T, want, got map[string]string) {
	t.Helper()
	var diffs []string
	for k, v := range want {
		if g, ok := got[k]; !ok {
			diffs = append(diffs, "missing "+k)
		} else if g != v {
			diffs = append(diffs, fmt.Sprintf("changed %s: %s -> %s", k, v, g))
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			diffs = append(diffs, "unexpected "+k)
		}
	}
	if len(diffs) > 0 {
		sort.Strings(diffs)
		t.Fatalf("tree mismatch:\n  %s", strings.Join(diffs, "\n  "))
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func requireOnlyManifestAndVault(t *testing.T, dir string) {
	t.Helper()
	want := []string{ProfileManifestFile, ProfileVaultFile}
	sort.Strings(want)
	got := dirNames(t, dir)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("directory should hold exactly %v, got %v", want, got)
	}
}

func mustSeal(t *testing.T, dir string, ks KeyStore) VaultReceipt {
	t.Helper()
	rc, err := SealProfile(dir, ks)
	if err != nil {
		t.Fatalf("SealProfile: %v", err)
	}
	return rc
}

func mustUnseal(t *testing.T, dir string, ks KeyStore) VaultReceipt {
	t.Helper()
	rc, err := UnsealProfile(dir, ks)
	if err != nil {
		t.Fatalf("UnsealProfile: %v", err)
	}
	return rc
}

func requireManifestVault(t *testing.T, dir, want string) {
	t.Helper()
	m, err := ReadProfileManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Vault != want {
		t.Fatalf("manifest vault = %q, want %q", m.Vault, want)
	}
}

// deadPID 返回一个刚刚退出的进程的 pid。
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// ------------------------------------------------------------
// 往返
// ------------------------------------------------------------

func TestVaultRoundTrip(t *testing.T) {
	dir, m := newTestProfile(t)
	ks := testKeyStore(t)
	before := snapshotTree(t, dir)

	rc := mustSeal(t, dir, ks)
	if rc.Action != "seal" || rc.Vault != ProfileVaultSealed || rc.Protection != KeyProtectionFile || !rc.Verified {
		t.Fatalf("seal receipt: %+v", rc)
	}
	if rc.ProfileID != m.ProfileID || rc.OSCrypt != ProfileOSCryptMock {
		t.Fatalf("seal receipt identity: %+v", rc)
	}
	if rc.Files != 9 || rc.PlainBytes <= 4<<20 {
		t.Fatalf("seal receipt counts: %+v", rc)
	}
	requireOnlyManifestAndVault(t, dir)
	requireManifestVault(t, dir, ProfileVaultSealed)

	// 封存文件里不能出现明文标记，也不能出现主密钥。
	vaultBytes, err := os.ReadFile(filepath.Join(dir, ProfileVaultFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"SECRET-MARKER-PREFS", "SECRET-MARKER-COOKIES", "SQLite format 3", "os_crypt"} {
		if bytes.Contains(vaultBytes, []byte(marker)) {
			t.Fatalf("vault leaks plaintext marker %q", marker)
		}
	}
	master, err := ks.Load()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(vaultBytes, master) || bytes.Contains(vaultBytes, []byte(hex.EncodeToString(master))) {
		t.Fatal("vault contains master key material")
	}
	if !bytes.HasPrefix(vaultBytes, []byte(vaultMagic)) {
		t.Fatal("missing magic")
	}
	if fi, _ := os.Stat(filepath.Join(dir, ProfileVaultFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("vault perm %v", fi.Mode().Perm())
	}

	rc = mustUnseal(t, dir, ks)
	if rc.Action != "unseal" || rc.Vault != ProfileVaultOpen || rc.Files != 9 {
		t.Fatalf("unseal receipt: %+v", rc)
	}
	requireManifestVault(t, dir, ProfileVaultOpen)
	if _, err := os.Stat(filepath.Join(dir, ProfileVaultFile)); !os.IsNotExist(err) {
		t.Fatalf("vault file should be gone after unseal: %v", err)
	}
	diffTrees(t, before, snapshotTree(t, dir))

	// 同一份主密钥下可以反复封存/解封，期间改过内容也照常往返。
	writeFileMode(t, filepath.Join(dir, "Default", "new-after-unseal.txt"), []byte("second round"), 0o600)
	before = snapshotTree(t, dir)
	mustSeal(t, dir, ks)
	requireOnlyManifestAndVault(t, dir)
	mustUnseal(t, dir, ks)
	diffTrees(t, before, snapshotTree(t, dir))
	if got, _ := ReadProfileManifest(dir); got.ProfileID != m.ProfileID {
		t.Fatal("profile id must be stable across seal cycles")
	}
}

func TestVaultSealDropsCachesAndRuntimeArtifacts(t *testing.T) {
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)

	// 可再生缓存（任何深度）、Singleton*、RunningChromeVersion、owner 标记。
	for _, rel := range []string{
		"Default/Cache/Cache_Data/f_000001",
		"Default/Code Cache/js/index",
		"Default/GPUCache/data_0",
		"ShaderCache/data_0",
		"GrShaderCache/data_0",
		"DawnGraphiteCache/data_0",
		"DawnWebGPUCache/data_0",
		"component_crx_cache/x.crx",
		"extensions_crx_cache/y.crx",
	} {
		writeFileMode(t, filepath.Join(dir, filepath.FromSlash(rel)), []byte("CACHE-SECRET-RESPONSE"), 0o600)
	}
	// owner marker 里写的是已退出的 pid：活的 pid 现在会被判为"仍在使用"而拒绝封存。
	writeFileMode(t, filepath.Join(dir, deepworkProfileOwnerFile), []byte(fmt.Sprintf(`{"owner_pid":%d}`, deadPID(t))), 0o600)
	dead := fmt.Sprintf("somehost-%d", deadPID(t))
	for name, target := range map[string]string{
		"SingletonLock":        dead,
		"SingletonSocket":      "/tmp/does-not-matter/SingletonSocket",
		"SingletonCookie":      "1234567890",
		"RunningChromeVersion": "130.0.0.0",
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	// 名字像缓存的普通文件不受影响（只按目录匹配）。
	writeFileMode(t, filepath.Join(dir, "Default", "Cache"+".note"), []byte("keep"), 0o600)

	want := snapshotTree(t, dir)
	for k := range want {
		if strings.Contains(k, "Cache"+string(filepath.Separator)) || strings.HasSuffix(k, "Cache") && want[k][:3] == "dir" ||
			strings.HasPrefix(filepath.Base(k), "Singleton") || filepath.Base(k) == "RunningChromeVersion" || filepath.Base(k) == deepworkProfileOwnerFile ||
			strings.Contains(k, "ShaderCache") || strings.Contains(k, "_crx_cache") || strings.Contains(k, "GPUCache") {
			delete(want, k)
		}
	}

	rc := mustSeal(t, dir, ks)
	if rc.CachesPurged != 9 {
		t.Fatalf("caches purged = %d, want 9", rc.CachesPurged)
	}
	requireOnlyManifestAndVault(t, dir)
	vaultBytes, _ := os.ReadFile(filepath.Join(dir, ProfileVaultFile))
	if bytes.Contains(vaultBytes, []byte("CACHE-SECRET-RESPONSE")) {
		t.Fatal("cache content leaked into vault")
	}

	mustUnseal(t, dir, ks)
	got := snapshotTree(t, dir)
	diffTrees(t, want, got)
	for _, rel := range []string{"Default/Cache", "Default/Code Cache", "ShaderCache", "SingletonLock", "RunningChromeVersion", deepworkProfileOwnerFile} {
		if _, err := os.Lstat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
			t.Fatalf("%s should not survive a seal/unseal cycle: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "Default", "Cache.note")); err != nil {
		t.Fatalf("regular file named like a cache must survive: %v", err)
	}
}

func TestVaultNoopStates(t *testing.T) {
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)

	// vault=none：封存/解封都是 noop，不创建主密钥，不动目录。
	none, _ := NewProfileManifest(ProfileOSCryptSystem, ProfileVaultNone, "visible")
	noneDir := t.TempDir()
	writeFileMode(t, filepath.Join(noneDir, "Default", "Cookies"), []byte("system-encrypted"), 0o600)
	if err := WriteProfileManifest(noneDir, none); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, noneDir)
	for _, op := range []func(string, KeyStore) (VaultReceipt, error){SealProfile, UnsealProfile} {
		rc, err := op(noneDir, ks)
		if err != nil || rc.Action != "noop" || rc.Vault != ProfileVaultNone {
			t.Fatalf("none-state op: %+v %v", rc, err)
		}
	}
	diffTrees(t, before, snapshotTree(t, noneDir))
	if _, err := ks.Load(); !errors.Is(err, ErrVaultKeyNotFound) {
		t.Fatalf("noop must not create a master key: %v", err)
	}

	// 已封存再封存 = noop；已 open 再解封 = noop。
	mustSeal(t, dir, ks)
	if rc := mustSeal(t, dir, ks); rc.Action != "noop" {
		t.Fatalf("second seal: %+v", rc)
	}
	requireOnlyManifestAndVault(t, dir)
	mustUnseal(t, dir, ks)
	if rc := mustUnseal(t, dir, ks); rc.Action != "noop" || rc.Vault != ProfileVaultOpen {
		t.Fatalf("second unseal: %+v", rc)
	}
}

// ------------------------------------------------------------
// 分块流：边界、篡改、截断、重排
// ------------------------------------------------------------

func testAEADStream(t *testing.T, chunk int, plain []byte) (stream []byte, prefix []byte, digest [sha256.Size]byte, key []byte, salt []byte) {
	t.Helper()
	master := randBytes(t, 32)
	salt = randBytes(t, 32)
	prefix = randBytes(t, 8)
	digest = sha256.Sum256([]byte("fake header"))
	aead, err := deriveVaultAEAD(master, salt, "pid")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	vw := newVaultWriter(&buf, aead, prefix, digest, chunk)
	// 分几次写，覆盖"写入跨块边界"。
	for rest := plain; len(rest) > 0; {
		n := 7
		if n > len(rest) {
			n = len(rest)
		}
		if _, err := vw.Write(rest[:n]); err != nil {
			t.Fatal(err)
		}
		rest = rest[n:]
	}
	if err := vw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), prefix, digest, master, salt
}

func readAEADStream(t *testing.T, stream []byte, chunk int, prefix []byte, digest [sha256.Size]byte, master, salt []byte) ([]byte, error) {
	t.Helper()
	aead, err := deriveVaultAEAD(master, salt, "pid")
	if err != nil {
		t.Fatal(err)
	}
	vr := newVaultReader(bufio.NewReader(bytes.NewReader(stream)), aead, prefix, digest, chunk)
	out, err := io.ReadAll(vr)
	if err == nil && !vr.done {
		t.Fatal("ReadAll returned nil error but the final chunk was never authenticated")
	}
	return out, err
}

func TestVaultStreamChunkBoundaries(t *testing.T) {
	const chunk = 64
	for _, n := range []int{0, 1, chunk - 1, chunk, chunk + 1, 2 * chunk, 2*chunk + 5, 5 * chunk} {
		plain := randBytes(t, n)
		stream, prefix, digest, master, salt := testAEADStream(t, chunk, plain)
		got, err := readAEADStream(t, stream, chunk, prefix, digest, master, salt)
		if err != nil {
			t.Fatalf("len %d: %v", n, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("len %d: round trip mismatch", n)
		}
		// 恰好整块倍数时不额外写一个空末块：密文长度 = 明文 + 块数*16。
		chunks := (n + chunk - 1) / chunk
		if chunks == 0 {
			chunks = 1
		}
		if want := n + chunks*16; len(stream) != want {
			t.Fatalf("len %d: stream %d bytes, want %d", n, len(stream), want)
		}
	}
}

func TestVaultStreamDetectsEveryTamper(t *testing.T) {
	const chunk = 64
	plain := randBytes(t, 3*chunk+10) // 4 块，末块短
	stream, prefix, digest, master, salt := testAEADStream(t, chunk, plain)
	rec := chunk + 16

	// 任意一个字节翻转 → 认证失败。
	for i := range stream {
		mut := append([]byte{}, stream...)
		mut[i] ^= 0x01
		if _, err := readAEADStream(t, mut, chunk, prefix, digest, master, salt); !errors.Is(err, ErrVaultAuthFailed) {
			t.Fatalf("flip at byte %d: want ErrVaultAuthFailed, got %v", i, err)
		}
	}
	// 在每个长度上截断 → 认证失败（含恰好落在块边界上的截断）。
	for cut := 0; cut < len(stream); cut++ {
		if _, err := readAEADStream(t, stream[:cut], chunk, prefix, digest, master, salt); !errors.Is(err, ErrVaultAuthFailed) {
			t.Fatalf("truncate to %d bytes: want ErrVaultAuthFailed, got %v", cut, err)
		}
	}
	// 追加任意字节 → 认证失败。
	for _, extra := range [][]byte{{0}, bytes.Repeat([]byte{9}, 16), bytes.Repeat([]byte{9}, rec)} {
		if _, err := readAEADStream(t, append(append([]byte{}, stream...), extra...), chunk, prefix, digest, master, salt); !errors.Is(err, ErrVaultAuthFailed) {
			t.Fatalf("append %d bytes: want ErrVaultAuthFailed, got %v", len(extra), err)
		}
	}
	// 块重排 / 重复。
	swap := append([]byte{}, stream...)
	copy(swap[0:rec], stream[rec:2*rec])
	copy(swap[rec:2*rec], stream[0:rec])
	if _, err := readAEADStream(t, swap, chunk, prefix, digest, master, salt); !errors.Is(err, ErrVaultAuthFailed) {
		t.Fatalf("swap chunks: %v", err)
	}
	dup := append(append([]byte{}, stream[:rec]...), stream[:rec]...)
	dup = append(dup, stream[rec:]...)
	if _, err := readAEADStream(t, dup, chunk, prefix, digest, master, salt); !errors.Is(err, ErrVaultAuthFailed) {
		t.Fatalf("duplicate chunk: %v", err)
	}
	// 头部摘要不同（等价于头部被篡改）/ 错密钥。
	if _, err := readAEADStream(t, stream, chunk, prefix, sha256.Sum256([]byte("other header")), master, salt); !errors.Is(err, ErrVaultAuthFailed) {
		t.Fatalf("header digest: %v", err)
	}
	if _, err := readAEADStream(t, stream, chunk, prefix, digest, randBytes(t, 32), salt); !errors.Is(err, ErrVaultAuthFailed) {
		t.Fatalf("wrong key: %v", err)
	}
}

// vaultFileLayout 解析真实封存文件：头部结束位置与各块记录长度。
func vaultFileLayout(t *testing.T, path string) (hdrEnd int, recs []int, raw []byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hdr, _, err := readVaultHeader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	hdrEnd = len(vaultMagic) + 4 + int(raw[len(vaultMagic)+3]) + int(raw[len(vaultMagic)+2])<<8 + int(raw[len(vaultMagic)+1])<<16 + int(raw[len(vaultMagic)])<<24
	rest := len(raw) - hdrEnd
	full := hdr.ChunkSize + 16
	for rest > full {
		recs = append(recs, full)
		rest -= full
	}
	recs = append(recs, rest)
	return hdrEnd, recs, raw
}

// tamperedVaultCase 返回"封存后改动 profile.vault"的各种方式。
func tamperedVaultCases(hdrEnd int, recs []int, raw []byte) map[string][]byte {
	clone := func() []byte { return append([]byte{}, raw...) }
	flip := func(off int) []byte { b := clone(); b[off] ^= 0x80; return b }
	cases := map[string][]byte{
		"flip magic":          flip(1),
		"flip header byte":    flip(len(vaultMagic) + 4 + 3),
		"flip first chunk":    flip(hdrEnd + 5),
		"flip middle chunk":   flip(hdrEnd + recs[0] + recs[1]/2),
		"flip last chunk tag": flip(len(raw) - 1),
		"truncate one byte":   clone()[:len(raw)-1],
		"truncate to header":  clone()[:hdrEnd],
		"truncate last chunk": clone()[:len(raw)-recs[len(recs)-1]],
		"append garbage":      append(clone(), 0x00),
		"empty file":          {},
		"header only magic":   clone()[:len(vaultMagic)],
	}
	swapped := clone()
	copy(swapped[hdrEnd:hdrEnd+recs[0]], raw[hdrEnd+recs[0]:hdrEnd+recs[0]+recs[1]])
	copy(swapped[hdrEnd+recs[0]:hdrEnd+recs[0]+recs[1]], raw[hdrEnd:hdrEnd+recs[0]])
	cases["swap first two chunks"] = swapped
	return cases
}

func TestUnsealRejectsTamperedVaultAndLeavesDirectoryAlone(t *testing.T) {
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	mustSeal(t, dir, ks)
	vaultPath := filepath.Join(dir, ProfileVaultFile)
	hdrEnd, recs, raw := vaultFileLayout(t, vaultPath)
	if len(recs) < 4 {
		t.Fatalf("fixture too small to exercise multi-chunk tampering: %d chunks", len(recs))
	}

	for name, mutated := range tamperedVaultCases(hdrEnd, recs, raw) {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(vaultPath, mutated, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := UnsealProfile(dir, ks)
			if err == nil {
				t.Fatal("tampered vault was accepted")
			}
			if !errors.Is(err, ErrVaultAuthFailed) && !errors.Is(err, ErrVaultMalformed) {
				t.Fatalf("want auth/malformed error, got %v", err)
			}
			// 原样保留：清单仍 sealed、只有两个文件、没有 .dw-unseal-* 残留。
			requireOnlyManifestAndVault(t, dir)
			requireManifestVault(t, dir, ProfileVaultSealed)
		})
	}

	// 块内容类篡改必须明确报"认证失败"，而不是笼统的格式错误。
	for _, name := range []string{"flip first chunk", "flip middle chunk", "flip last chunk tag", "truncate one byte", "truncate last chunk", "swap first two chunks", "append garbage"} {
		if err := os.WriteFile(vaultPath, tamperedVaultCases(hdrEnd, recs, raw)[name], 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := UnsealProfile(dir, ks); !errors.Is(err, ErrVaultAuthFailed) {
			t.Fatalf("%s: want ErrVaultAuthFailed, got %v", name, err)
		}
	}

	// 还原后仍可正常解封。
	if err := os.WriteFile(vaultPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	mustUnseal(t, dir, ks)
}

// 删除明文不可逆，所以封存后的自校验是最后一道保险：它失败时必须保住明文、清掉 tmp、不留 vault。
func TestSealSelfVerificationFailureKeepsPlaintext(t *testing.T) {
	skipOnWindows(t)
	cases := map[string]struct {
		hook    func(t *testing.T, tmpPath string, master []byte, profileID string)
		wantErr error
	}{
		"ciphertext corrupted after write": {
			hook: func(t *testing.T, tmpPath string, _ []byte, _ string) {
				raw, err := os.ReadFile(tmpPath)
				if err != nil {
					t.Fatal(err)
				}
				raw[len(raw)-1] ^= 0xff
				if err := os.WriteFile(tmpPath, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: ErrVaultAuthFailed,
		},
		"authentic stream that does not match what was written": {
			hook: func(t *testing.T, tmpPath string, master []byte, profileID string) {
				f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_TRUNC, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				// 合法、可认证，但只有一个文件：文件数/字节数对不上。
				if err := writeVaultStream(f, master, profileID, vaultChunkSize, vaultTotals{Entries: 1, Files: 1, Bytes: 1}, func(tw *tar.Writer) error {
					tarFile(t, tw, "only-one.txt", "x")
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: ErrVaultMalformed,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir, _ := newTestProfile(t)
			ks := testKeyStore(t)
			before := snapshotTree(t, dir)
			vaultSealHookAfterWrite = func(tmpPath string, master []byte, profileID string) {
				tc.hook(t, tmpPath, master, profileID)
			}
			defer func() { vaultSealHookAfterWrite = nil }()

			_, err := SealProfile(dir, ks)
			if !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), "self-verification") {
				t.Fatalf("want self-verification failure wrapping %v, got %v", tc.wantErr, err)
			}
			requireManifestVault(t, dir, ProfileVaultOpen)
			for _, n := range dirNames(t, dir) {
				if n == ProfileVaultFile || strings.Contains(n, ".tmp-") {
					t.Fatalf("failed seal left %s behind", n)
				}
			}
			diffTrees(t, before, snapshotTree(t, dir))
		})
	}
}

// 解封在整条流（含末块）认证通过之前，不得动目录里的任何东西——包括"封存后残留的明文"。
func TestUnsealFailureDoesNotTouchLeftovers(t *testing.T) {
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	mustSeal(t, dir, ks)
	writeFileMode(t, filepath.Join(dir, "leftover.txt"), []byte("leftover"), 0o600)

	vaultPath := filepath.Join(dir, ProfileVaultFile)
	raw, err := os.ReadFile(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vaultPath, raw[:len(raw)-1], 0o600); err != nil { // 截断末块
		t.Fatal(err)
	}
	if _, err := UnsealProfile(dir, ks); !errors.Is(err, ErrVaultAuthFailed) {
		t.Fatalf("want ErrVaultAuthFailed, got %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(dir, "leftover.txt")); err != nil || string(body) != "leftover" {
		t.Fatalf("failed unseal must not touch leftovers: %v", err)
	}
	for _, n := range dirNames(t, dir) {
		if strings.HasPrefix(n, vaultUnsealTmpPrefix) {
			t.Fatalf("temp dir left behind: %s", n)
		}
	}
	requireManifestVault(t, dir, ProfileVaultSealed)
}

func TestUnsealWrongKey(t *testing.T) {
	dir, _ := newTestProfile(t)
	mustSeal(t, dir, testKeyStore(t))
	other := testKeyStore(t)
	if _, err := LoadOrCreateMasterKey(other); err != nil {
		t.Fatal(err)
	}
	if _, err := UnsealProfile(dir, other); !errors.Is(err, ErrVaultAuthFailed) {
		t.Fatalf("wrong key: want ErrVaultAuthFailed, got %v", err)
	}
	requireOnlyManifestAndVault(t, dir)
}

func TestUnsealKeyUnavailable(t *testing.T) {
	dir, _ := newTestProfile(t)
	mustSeal(t, dir, testKeyStore(t))
	empty := testKeyStore(t) // 没有主密钥
	_, err := UnsealProfile(dir, empty)
	if !errors.Is(err, ErrVaultKeyUnavailable) {
		t.Fatalf("want ErrVaultKeyUnavailable, got %v", err)
	}
	// 解封绝不凭空创建主密钥——新密钥解不开旧封存。
	if _, lerr := empty.Load(); !errors.Is(lerr, ErrVaultKeyNotFound) {
		t.Fatalf("unseal must not create a master key: %v", lerr)
	}
	requireOnlyManifestAndVault(t, dir)

	// 密钥库本身坏了（Load 返回非 NotFound 错误）同样归为不可用，且错误文本不含密钥。
	_, err = UnsealProfile(dir, brokenKeyStore{})
	if !errors.Is(err, ErrVaultKeyUnavailable) {
		t.Fatalf("broken store: %v", err)
	}
	_, err = SealProfile(mustOpenCopy(t), brokenKeyStore{})
	if !errors.Is(err, ErrVaultKeyUnavailable) {
		t.Fatalf("broken store (seal): %v", err)
	}
}

type brokenKeyStore struct{}

func (brokenKeyStore) Protection() string      { return KeyProtectionKeychain }
func (brokenKeyStore) Load() ([]byte, error)   { return nil, errors.New("keychain locked") }
func (brokenKeyStore) Create(key []byte) error { return errors.New("keychain locked") }

func mustOpenCopy(t *testing.T) string {
	dir, _ := newTestProfile(t)
	return dir
}

// ------------------------------------------------------------
// 恶意封存
// ------------------------------------------------------------

// buildSealedFixture 用真实密钥和真实格式造一份"清单 sealed + 自定义 tar 内容"的封存。
func buildSealedFixture(t *testing.T, dir string, ks KeyStore, build func(tw *tar.Writer)) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := NewProfileManifest(ProfileOSCryptMock, ProfileVaultSealed, "headed")
	if err != nil {
		t.Fatal(err)
	}
	master, err := LoadOrCreateMasterKey(ks)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, ProfileVaultFile), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeVaultStream(f, master, m.ProfileID, vaultChunkSize, tarTotals(t, build), func(tw *tar.Writer) error {
		build(tw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := WriteProfileManifest(dir, m); err != nil {
		t.Fatal(err)
	}
}

// tarTotals 把 build 跑一遍到内存，数出头部要声明的条目数 / 文件数 / 字节数。
func tarTotals(t *testing.T, build func(tw *tar.Writer)) vaultTotals {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	build(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var totals vaultTotals
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return totals
		}
		if err != nil {
			t.Fatal(err)
		}
		totals.Entries++
		if h.Typeflag == tar.TypeReg {
			totals.Files++
			totals.Bytes += h.Size
		}
	}
}

func tarFile(t *testing.T, tw *tar.Writer, name, body string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
}

func TestUnsealRejectsMaliciousEntries(t *testing.T) {
	skipOnWindows(t)
	outside := t.TempDir() // 绝对路径条目的攻击目标

	cases := map[string]func(t *testing.T, tw *tar.Writer){
		"absolute path": func(t *testing.T, tw *tar.Writer) {
			tarFile(t, tw, "Default/ok.txt", "fine")
			tarFile(t, tw, filepath.Join(outside, "pwned-abs"), "pwn")
		},
		"dotdot escape": func(t *testing.T, tw *tar.Writer) {
			tarFile(t, tw, "../pwned-parent", "pwn")
		},
		"dotdot in the middle": func(t *testing.T, tw *tar.Writer) {
			tarFile(t, tw, "Default/../../pwned-parent", "pwn")
		},
		"dot element": func(t *testing.T, tw *tar.Writer) {
			tarFile(t, tw, "./Default/x", "x")
		},
		"backslash": func(t *testing.T, tw *tar.Writer) {
			tarFile(t, tw, `Default\..\x`, "x")
		},
		"symlink entry": func(t *testing.T, tw *tar.Writer) {
			if err := tw.WriteHeader(&tar.Header{Name: "Default/link", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777}); err != nil {
				t.Fatal(err)
			}
		},
		"write through symlink": func(t *testing.T, tw *tar.Writer) {
			if err := tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777}); err != nil {
				t.Fatal(err)
			}
			tarFile(t, tw, "link/pwned-through-link", "pwn")
		},
		"hardlink entry": func(t *testing.T, tw *tar.Writer) {
			tarFile(t, tw, "a", "a")
			if err := tw.WriteHeader(&tar.Header{Name: "b", Typeflag: tar.TypeLink, Linkname: "a", Mode: 0o600}); err != nil {
				t.Fatal(err)
			}
		},
		"fifo entry": func(t *testing.T, tw *tar.Writer) {
			if err := tw.WriteHeader(&tar.Header{Name: "pipe", Typeflag: tar.TypeFifo, Mode: 0o600}); err != nil {
				t.Fatal(err)
			}
		},
		"overwrite manifest": func(t *testing.T, tw *tar.Writer) {
			tarFile(t, tw, ProfileManifestFile, `{"schema":1}`)
		},
		"overwrite vault": func(t *testing.T, tw *tar.Writer) {
			tarFile(t, tw, ProfileVaultFile, "x")
		},
		"smuggle singleton": func(t *testing.T, tw *tar.Writer) {
			tarFile(t, tw, "SingletonLock", "x")
		},
		"empty name": func(t *testing.T, tw *tar.Writer) {
			tarFile(t, tw, "", "x")
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "profile")
			ks := testKeyStore(t)
			buildSealedFixture(t, dir, ks, func(tw *tar.Writer) { build(t, tw) })

			_, err := UnsealProfile(dir, ks)
			if err == nil {
				t.Fatal("malicious vault was accepted")
			}
			if !errors.Is(err, ErrVaultUnsafeEntry) {
				t.Fatalf("want ErrVaultUnsafeEntry, got %v", err)
			}
			// 什么都没逃出去，也没留下半成品。
			requireOnlyManifestAndVault(t, dir)
			requireManifestVault(t, dir, ProfileVaultSealed)
			if got := dirNames(t, outside); len(got) != 0 {
				t.Fatalf("files escaped into outside dir: %v", got)
			}
			if got := dirNames(t, parent); len(got) != 1 || got[0] != "profile" {
				t.Fatalf("files escaped into parent dir: %v", got)
			}
		})
	}
}

func TestUnsealRejectsDuplicateAndConflictingEntries(t *testing.T) {
	skipOnWindows(t)
	for name, build := range map[string]func(tw *tar.Writer){
		"duplicate file": func(tw *tar.Writer) {
			tarFile(t, tw, "a", "1")
			tarFile(t, tw, "a", "2")
		},
		"file then dir with same name": func(tw *tar.Writer) {
			tarFile(t, tw, "a", "1")
			tarFile(t, tw, "a/b", "2")
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "profile")
			ks := testKeyStore(t)
			buildSealedFixture(t, dir, ks, build)
			if _, err := UnsealProfile(dir, ks); !errors.Is(err, ErrVaultMalformed) {
				t.Fatalf("want ErrVaultMalformed, got %v", err)
			}
			requireOnlyManifestAndVault(t, dir)
		})
	}
}

func TestUnsealRestoresDirectoryModesAfterExtraction(t *testing.T) {
	skipOnWindows(t)
	dir := filepath.Join(t.TempDir(), "profile")
	ks := testKeyStore(t)
	buildSealedFixture(t, dir, ks, func(tw *tar.Writer) {
		// 目录权限很紧（0500）：如果提取时就收紧，后面的子文件就写不进去。
		if err := tw.WriteHeader(&tar.Header{Name: "locked/", Typeflag: tar.TypeDir, Mode: 0o500}); err != nil {
			t.Fatal(err)
		}
		tarFile(t, tw, "locked/inside.txt", "hello")
	})
	mustUnseal(t, dir, ks)
	fi, err := os.Stat(filepath.Join(dir, "locked"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o500 {
		t.Fatalf("dir mode %v", fi.Mode().Perm())
	}
	_ = os.Chmod(filepath.Join(dir, "locked"), 0o700) // 让 t.TempDir 能清理
}

// ------------------------------------------------------------
// 崩溃恢复与运行态保护
// ------------------------------------------------------------

func TestCrashLeftoverOpenManifestWithVaultUsesPlaintext(t *testing.T) {
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	original := snapshotTree(t, dir)

	// 造"上次封存到一半崩溃"：vault 已在、明文完整、清单仍为 open。
	mustSeal(t, dir, ks)
	staleVault, err := os.ReadFile(filepath.Join(dir, ProfileVaultFile))
	if err != nil {
		t.Fatal(err)
	}
	mustUnseal(t, dir, ks)
	// 会话里又改了数据（明文比旧 vault 新）。
	writeFileMode(t, filepath.Join(dir, "Default", "newer-than-vault.txt"), []byte("fresh"), 0o600)
	if err := os.WriteFile(filepath.Join(dir, "Default", "Preferences"), []byte(`{"a":"edited-after-crash"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ProfileVaultFile), staleVault, 0o600); err != nil {
		t.Fatal(err)
	}
	requireManifestVault(t, dir, ProfileVaultOpen)
	live := snapshotTree(t, dir)
	if live["Default/newer-than-vault.txt"] == "" || live["Default/Preferences"] == original["Default/Preferences"] {
		t.Fatal("fixture should differ from the stale vault")
	}

	// 解封：以明文为准，旧 vault 作废，明文一个字节不动。
	rc := mustUnseal(t, dir, ks)
	if rc.Action != "noop" || !strings.Contains(rc.Note, "stale vault") {
		t.Fatalf("unseal on open+vault: %+v", rc)
	}
	if _, err := os.Stat(filepath.Join(dir, ProfileVaultFile)); !os.IsNotExist(err) {
		t.Fatalf("stale vault must be discarded: %v", err)
	}
	diffTrees(t, live, snapshotTree(t, dir))

	// 另一条路径：直接再封存，同样以明文为准，覆盖旧 vault。
	if err := os.WriteFile(filepath.Join(dir, ProfileVaultFile), staleVault, 0o600); err != nil {
		t.Fatal(err)
	}
	rc = mustSeal(t, dir, ks)
	if !strings.Contains(rc.Note, "stale vault") {
		t.Fatalf("seal over stale vault: %+v", rc)
	}
	mustUnseal(t, dir, ks)
	diffTrees(t, live, snapshotTree(t, dir))
}

func TestCrashSealedManifestWithPlaintextLeftoversUsesVault(t *testing.T) {
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	want := snapshotTree(t, dir)
	mustSeal(t, dir, ks)

	// 造"清单已 sealed、删除明文删到一半崩溃"：残留一部分明文。
	writeFileMode(t, filepath.Join(dir, "Default", "Cookies"), []byte("half-deleted leftover"), 0o600)
	writeFileMode(t, filepath.Join(dir, "stray.txt"), []byte("stray"), 0o600)
	if err := os.MkdirAll(filepath.Join(dir, ".dw-unseal-crashed", "x"), 0o700); err != nil {
		t.Fatal(err)
	}

	// 封存侧：清单为 sealed 且有残留明文时，先加载主密钥并整体验证 vault，通过后清掉残留（不是 noop）。
	rc := mustSeal(t, dir, ks)
	if rc.Action != "seal" || rc.Vault != ProfileVaultSealed || !strings.Contains(rc.Note, "leftover") {
		t.Fatalf("seal on sealed+leftovers: %+v", rc)
	}
	requireOnlyManifestAndVault(t, dir)

	// 内容回到封存时的样子。
	mustUnseal(t, dir, ks)
	diffTrees(t, want, snapshotTree(t, dir))
	if _, err := os.Stat(filepath.Join(dir, "stray.txt")); !os.IsNotExist(err) {
		t.Fatal("leftover must be gone")
	}
}

func TestSealedManifestWithoutVaultFile(t *testing.T) {
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	mustSeal(t, dir, ks)
	if err := os.Remove(filepath.Join(dir, ProfileVaultFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := UnsealProfile(dir, ks); !errors.Is(err, ErrVaultMissing) {
		t.Fatalf("want ErrVaultMissing, got %v", err)
	}
}

func TestUnsealRecoversMissingManifestFromVaultHeader(t *testing.T) {
	dir, m := newTestProfile(t)
	ks := testKeyStore(t)
	want := snapshotTree(t, dir)
	mustSeal(t, dir, ks)
	if err := os.Remove(filepath.Join(dir, ProfileManifestFile)); err != nil {
		t.Fatal(err)
	}
	rc := mustUnseal(t, dir, ks)
	if !strings.Contains(rc.Note, "recovered") {
		t.Fatalf("receipt: %+v", rc)
	}
	got, err := ReadProfileManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfileID != m.ProfileID || got.OSCrypt != ProfileOSCryptMock || got.Vault != ProfileVaultOpen {
		t.Fatalf("recovered manifest: %+v", got)
	}
	diffTrees(t, want, snapshotTree(t, dir))
}

func TestSealRefusedWhileChromeIsRunning(t *testing.T) {
	skipOnWindows(t)
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	before := snapshotTree(t, dir)

	// SingletonLock → 本测试进程（存活）。
	if err := os.Symlink(fmt.Sprintf("host-%d", os.Getpid()), filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	_, err := SealProfile(dir, ks)
	if !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("want ErrProfileInUse, got %v", err)
	}
	var inUse *ProfileInUseError
	if !errors.As(err, &inUse) || inUse.PID != os.Getpid() {
		t.Fatalf("want ProfileInUseError with pid, got %#v", err)
	}
	// 明文、清单完好；没有临时文件；也没有凭空创建主密钥。
	requireManifestVault(t, dir, ProfileVaultOpen)
	if got := dirNames(t, dir); contains(got, ProfileVaultFile) {
		t.Fatalf("refused seal left a vault: %v", got)
	}
	_ = os.Remove(filepath.Join(dir, "SingletonLock"))
	diffTrees(t, before, snapshotTree(t, dir))
	if _, err := ks.Load(); !errors.Is(err, ErrVaultKeyNotFound) {
		t.Fatalf("refused seal must not create a master key: %v", err)
	}

	// 陈旧的锁（进程已死）不挡路。
	if err := os.Symlink(fmt.Sprintf("host-%d", deadPID(t)), filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatal(err)
	}
	mustSeal(t, dir, ks)
	requireOnlyManifestAndVault(t, dir)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestSealAbortsOnUnsupportedSymlinkAndKeepsPlaintext(t *testing.T) {
	skipOnWindows(t)
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "Default", "stray-link")); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, dir)
	_, err := SealProfile(dir, ks)
	if !errors.Is(err, ErrVaultUnsupportedEntry) {
		t.Fatalf("want ErrVaultUnsupportedEntry, got %v", err)
	}
	if !strings.Contains(err.Error(), "Default/stray-link") {
		t.Fatalf("error should name the offending entry: %v", err)
	}
	requireManifestVault(t, dir, ProfileVaultOpen)
	if got := dirNames(t, dir); contains(got, ProfileVaultFile) {
		t.Fatalf("aborted seal left files behind: %v", got)
	}
	for _, n := range dirNames(t, dir) {
		if strings.Contains(n, ".tmp-") {
			t.Fatalf("temp file left behind: %s", n)
		}
	}
	diffTrees(t, before, snapshotTree(t, dir))
	if _, lerr := os.Lstat(filepath.Join(dir, "Default", "stray-link")); lerr != nil {
		t.Fatal("symlink was removed")
	}
}

func TestSealRequiresManifest(t *testing.T) {
	dir := t.TempDir()
	if _, err := SealProfile(dir, testKeyStore(t)); !errors.Is(err, ErrProfileManifestNotFound) {
		t.Fatalf("want ErrProfileManifestNotFound, got %v", err)
	}
	if _, err := UnsealProfile(dir, testKeyStore(t)); !errors.Is(err, ErrProfileManifestNotFound) {
		t.Fatalf("unseal: want ErrProfileManifestNotFound, got %v", err)
	}
}

func TestConcurrentSealsSerialize(t *testing.T) {
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	want := snapshotTree(t, dir)

	var wg sync.WaitGroup
	receipts := make([]VaultReceipt, 3)
	errs := make([]error, 3)
	for i := range receipts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			receipts[i], errs[i] = SealProfile(dir, ks)
		}()
	}
	wg.Wait()
	seals := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("seal %d: %v", i, err)
		}
		if receipts[i].Action == "seal" {
			seals++
		}
	}
	if seals != 1 {
		t.Fatalf("exactly one concurrent seal should do the work, got %d", seals)
	}
	requireOnlyManifestAndVault(t, dir)
	mustUnseal(t, dir, ks)
	diffTrees(t, want, snapshotTree(t, dir))
}

func TestVaultLockTimeoutReportsInUse(t *testing.T) {
	dir, _ := newTestProfile(t)
	real, err := resolveProfileDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	held, err := acquireProfileVaultLock(real)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	old := vaultLockWait
	vaultLockWait = 80 * time.Millisecond
	defer func() { vaultLockWait = old }()
	if _, err := SealProfile(dir, testKeyStore(t)); !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("want ErrProfileInUse while the vault lock is held, got %v", err)
	}
}

func TestVaultHeaderIsAuthenticatedAndBounded(t *testing.T) {
	// 头部里的 chunk_size 在认证之前就用来分配缓冲：越界必须在分配之前拒绝。
	for _, size := range []int{0, 1, vaultMinChunkSize - 1, vaultMaxChunkSize + 1, 1 << 30} {
		h := vaultHeader{
			Version: 1, Alg: vaultAlgName, KDF: vaultKDFName,
			Salt: make([]byte, 32), NoncePrefix: make([]byte, 8), ProfileID: "p", ChunkSize: size,
		}
		framed, _, err := encodeVaultHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := readVaultHeader(bytes.NewReader(framed)); !errors.Is(err, ErrVaultMalformed) {
			t.Fatalf("chunk_size %d: want ErrVaultMalformed, got %v", size, err)
		}
	}
	// 声明的头部长度过大。
	big := append([]byte(vaultMagic), 0x7f, 0xff, 0xff, 0xff)
	if _, _, err := readVaultHeader(bytes.NewReader(big)); !errors.Is(err, ErrVaultMalformed) {
		t.Fatalf("oversized header: %v", err)
	}
	// 头部字段在 JSON 里的样子：不含任何密钥材料。
	h := vaultHeader{
		Version: 1, Alg: vaultAlgName, KDF: vaultKDFName,
		Salt: make([]byte, 32), NoncePrefix: make([]byte, 8), ProfileID: "p", ChunkSize: vaultChunkSize,
	}
	framed, _, _ := encodeVaultHeader(h)
	var parsed map[string]any
	if err := json.Unmarshal(framed[len(vaultMagic)+4:], &parsed); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"v", "alg", "kdf", "salt", "nonce_prefix", "profile_id", "chunk_size", "created_at", "entries", "files", "bytes"} {
		if _, ok := parsed[k]; !ok {
			t.Fatalf("header missing %q", k)
		}
	}
}

// ------------------------------------------------------------
// 密钥库
// ------------------------------------------------------------

func TestFileKeyStore(t *testing.T) {
	skipOnWindows(t)
	path := filepath.Join(t.TempDir(), "nested", "master.key")
	ks := NewFileKeyStore(path)
	if ks.Protection() != KeyProtectionFile {
		t.Fatalf("protection %q", ks.Protection())
	}
	if _, err := ks.Load(); !errors.Is(err, ErrVaultKeyNotFound) {
		t.Fatalf("missing: %v", err)
	}
	key := randBytes(t, 32)
	if err := ks.Create(key); err != nil {
		t.Fatal(err)
	}
	if err := ks.Create(randBytes(t, 32)); !errors.Is(err, ErrVaultKeyExists) {
		t.Fatalf("second create: want ErrVaultKeyExists, got %v", err)
	}
	got, err := ks.Load()
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("load after duplicate create must still return the first key: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file perm %v", fi.Mode().Perm())
	}
	if names := dirNames(t, filepath.Dir(path)); len(names) != 1 {
		t.Fatalf("temp files left in key dir: %v", names)
	}

	// 权限过宽、内容损坏都是"不可用"，且错误文本不含密钥。
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadMasterKey(ks)
	if !errors.Is(err, ErrVaultKeyUnavailable) || strings.Contains(err.Error(), hex.EncodeToString(key)) {
		t.Fatalf("open perms: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("zz-not-hex"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(ks); !errors.Is(err, ErrVaultKeyUnavailable) {
		t.Fatalf("corrupt: %v", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key[:16])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMasterKey(ks); !errors.Is(err, ErrVaultKeyUnavailable) {
		t.Fatalf("short key: %v", err)
	}
}

func TestLoadOrCreateMasterKeyRacesToOneKey(t *testing.T) {
	skipOnWindows(t)
	ks := testKeyStore(t)
	const n = 16
	keys := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], errs[i] = LoadOrCreateMasterKey(ks)
		}()
	}
	wg.Wait()
	for i := range keys {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if !bytes.Equal(keys[i], keys[0]) {
			t.Fatalf("racers disagree on the master key")
		}
	}
	if len(keys[0]) != 32 {
		t.Fatalf("key length %d", len(keys[0]))
	}
}

func TestDefaultProfileKeyStoreProtection(t *testing.T) {
	ks := DefaultProfileKeyStore()
	want := KeyProtectionFile
	if runtime.GOOS == "darwin" {
		want = KeyProtectionKeychain
	}
	if ks.Protection() != want {
		t.Fatalf("default protection = %q, want %q", ks.Protection(), want)
	}
}
