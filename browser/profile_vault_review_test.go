package browser

// 独立审查（review）发现的问题对应的回归测试。每个用例先在旧实现上变红，再修复变绿。

import (
	"archive/tar"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1" //nolint:gosec
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ------------------------------------------------------------
// 通用夹具
// ------------------------------------------------------------

// spawnSleeper 起一个活着的子进程，命令行带上 extraArgs（用来冒充"正在用这个 profile 的 Chrome"）。
func spawnSleeper(t *testing.T, extraArgs ...string) *exec.Cmd {
	t.Helper()
	args := append([]string{"-c", "sleep 60; :", "dw-fake-chrome"}, extraArgs...)
	cmd := exec.Command("/bin/sh", args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		})
	}
	t.Cleanup(stop)
	return cmd
}

func stopProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

func writeOwnerMarker(t *testing.T, dir string, owner, chrome, mux int) {
	t.Helper()
	body := fmt.Sprintf(`{"owner_pid":%d,"chrome_pid":%d,"browser_mux_host_pid":%d}`, owner, chrome, mux)
	writeFileMode(t, filepath.Join(dir, deepworkProfileOwnerFile), []byte(body), 0o600)
}

func setManifestVault(t *testing.T, dir, state string) {
	t.Helper()
	m, err := ReadProfileManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	m.Vault = state
	if err := WriteProfileManifest(dir, m); err != nil {
		t.Fatal(err)
	}
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// rewriteVault 把 vaultPath 里的封存解出来，按 rename 改条目名后用同一把密钥重新封存（认证有效、数量字节不变）。
func rewriteVault(t *testing.T, vaultPath string, master []byte, profileID string, rename func(string) string) {
	t.Helper()
	f, err := os.Open(vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	vr, _, err := openVaultStream(f, master, profileID)
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		h    tar.Header
		body []byte
	}
	var entries []entry
	tr := tar.NewReader(vr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry{h: *h, body: body})
	}
	_ = f.Close()
	fill := func(tw *tar.Writer) {
		for _, e := range entries {
			h := e.h
			h.PAXRecords, h.Format = nil, tar.FormatUnknown
			isDir := strings.HasSuffix(h.Name, "/")
			h.Name = rename(strings.TrimSuffix(h.Name, "/"))
			if isDir {
				h.Name += "/"
			}
			if err := tw.WriteHeader(&h); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	out, err := os.OpenFile(vaultPath, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := writeVaultStream(out, master, profileID, vaultChunkSize, tarTotals(t, fill), func(tw *tar.Writer) error {
		fill(tw)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// buildFixtureWithTotals 与 buildSealedFixture 相同，但头部声明的总量由调用方指定。
func buildFixtureWithTotals(t *testing.T, dir string, ks KeyStore, totals vaultTotals, build func(tw *tar.Writer)) {
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
	if err := writeVaultStream(f, master, m.ProfileID, vaultChunkSize, totals, func(tw *tar.Writer) error {
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

// ------------------------------------------------------------
// 1. 封存接受集合 ⊆ 解封接受集合
// ------------------------------------------------------------

func TestSealRefusesEntriesUnsealWouldRefuse(t *testing.T) {
	skipOnWindows(t)
	cases := map[string]func(t *testing.T, dir string) (offender string){
		"backslash in file name": func(t *testing.T, dir string) string {
			writeFileMode(t, filepath.Join(dir, "Default", "Extensions", `a\b`), []byte("x"), 0o600)
			return "Default/Extensions/" + `a\b`
		},
		"backslash in directory name": func(t *testing.T, dir string) string {
			writeFileMode(t, filepath.Join(dir, "Default", `ext\dir`, "inner.txt"), []byte("x"), 0o600)
			return "Default/" + `ext\dir`
		},
		"invalid UTF-8 name": func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "Default", "bad-\xff.txt")
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Skipf("filesystem refuses non-UTF-8 names: %v", err)
			}
			return "Default/bad-\xff.txt"
		},
		"path too long once the unseal temp dir prefixes it": func(t *testing.T, dir string) string {
			real, err := resolveProfileDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			// 现在的绝对路径放得下（比上限短 8 字节），但解封要先解到 <dir>/.dw-unseal-XXXXXXXXXX/ 下面，会超限。
			want := vaultPathLimit() - 8 - len(real) - 1
			rel := "Default"
			for len(rel)+1+200 < want {
				rel += "/" + strings.Repeat("d", 200)
			}
			if rest := want - len(rel) - 1; rest > 0 {
				rel += "/" + strings.Repeat("e", rest)
			}
			writeFileMode(t, filepath.Join(real, filepath.FromSlash(rel)), []byte("x"), 0o600)
			return rel[:40]
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			dir, _ := newTestProfile(t)
			ks := testKeyStore(t)
			mk(t, dir)
			before := snapshotTree(t, dir)

			_, err := SealProfile(dir, ks)
			if !errors.Is(err, ErrVaultUnsupportedEntry) {
				t.Fatalf("want ErrVaultUnsupportedEntry (seal must not accept what unseal would refuse), got %v", err)
			}
			requireManifestVault(t, dir, ProfileVaultOpen)
			for _, n := range dirNames(t, dir) {
				if n == ProfileVaultFile || strings.Contains(n, ".tmp-") {
					t.Fatalf("aborted seal left %s behind", n)
				}
			}
			diffTrees(t, before, snapshotTree(t, dir))
		})
	}
}

// 文件系统层面造不出的名字（macOS 拒绝非 UTF-8），在规则函数层直接验证：封存规划和解封用的是同一份规则。
func TestVaultNameRulesAreSharedBetweenPlanAndUnseal(t *testing.T) {
	root := "/profiles/p"
	long := strings.Repeat("x", vaultMaxNameLen+1)
	for name, rel := range map[string]string{
		"invalid UTF-8":             "Default/bad-\xff.txt",
		"backslash":                 `Default/a\b`,
		"element over 255 bytes":    "Default/" + long,
		"dot element":               "Default/./x",
		"parent element":            "Default/../x",
		"absolute":                  "/etc/passwd",
		"empty element":             "Default//x",
		"reserved top-level":        ProfileManifestFile,
		"reserved unseal temp name": vaultUnsealTmpPrefix + "123/x",
	} {
		t.Run(name, func(t *testing.T) {
			if err := vaultArchivableName(root, rel); !errors.Is(err, ErrVaultUnsupportedEntry) {
				t.Fatalf("plan side: want ErrVaultUnsupportedEntry, got %v", err)
			}
			if _, err := vaultNameRules(rel, vaultFitsUnderProfile(root)); !errors.Is(err, ErrVaultUnsafeEntry) {
				t.Fatalf("unseal side: want ErrVaultUnsafeEntry, got %v", err)
			}
		})
	}
	for _, rel := range []string{"Default/Cookies", "Default/Local Storage/leveldb/000003.log", "Local State", "Default/\u4e2d\u6587.txt"} {
		if err := vaultArchivableName(root, rel); err != nil {
			t.Errorf("%q should be archivable: %v", rel, err)
		}
	}
	// 路径长度：临时目录前缀一加就超限的边界。
	limit := vaultPathLimit()
	fits := vaultFitsUnderProfile(root)
	edge := limit - len(root) - 1 - vaultUnsealTmpNameLen - 1 // 刚好放得下的 rel 长度上限 + 1
	if !fits(strings.Repeat("a", edge-1)) || fits(strings.Repeat("a", edge)) {
		t.Fatalf("path-length boundary is off by one (limit %d, edge %d)", limit, edge)
	}
}

func TestVaultEntryCheckerRejectsConflictsAndCountsTotals(t *testing.T) {
	hdr := func(name string, typ byte, size int64) *tar.Header {
		return &tar.Header{Name: name, Typeflag: typ, Size: size, Mode: 0o600, Format: tar.FormatUSTAR}
	}
	newChk := func(limits vaultTotals) *vaultEntryChecker { return newVaultEntryChecker(limits, nil) }
	big := vaultTotals{Entries: 100, Files: 100, Bytes: 1 << 20}

	c := newChk(big)
	if _, err := c.check(hdr("a", tar.TypeReg, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.check(hdr("a/b", tar.TypeReg, 1)); !errors.Is(err, ErrVaultMalformed) {
		t.Fatalf("entry under a file: %v", err)
	}
	c = newChk(big)
	_, _ = c.check(hdr("a", tar.TypeReg, 1))
	if _, err := c.check(hdr("a", tar.TypeReg, 1)); !errors.Is(err, ErrVaultMalformed) {
		t.Fatalf("duplicate file: %v", err)
	}
	c = newChk(big)
	_, _ = c.check(hdr("d/x", tar.TypeReg, 1)) // d 是隐含目录
	if _, err := c.check(hdr("d", tar.TypeReg, 1)); !errors.Is(err, ErrVaultMalformed) {
		t.Fatalf("file replacing an implied directory: %v", err)
	}
	if _, err := c.check(hdr("d/", tar.TypeDir, 0)); err != nil {
		t.Fatalf("explicit directory after an implied one is fine: %v", err)
	}
	c = newChk(big)
	if _, err := c.check(hdr("sym", tar.TypeSymlink, 0)); !errors.Is(err, ErrVaultUnsafeEntry) {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := c.check(&tar.Header{Name: "neg", Typeflag: tar.TypeReg, Size: -1, Format: tar.FormatUSTAR}); !errors.Is(err, ErrVaultMalformed) {
		t.Fatalf("negative size: %v", err)
	}
	// 总量：恰好等于声明可以，差一个就不行；finish 要求实际等于声明。
	c = newChk(vaultTotals{Entries: 2, Files: 1, Bytes: 5})
	if _, err := c.check(hdr("d/", tar.TypeDir, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.check(hdr("d/f", tar.TypeReg, 6)); !errors.Is(err, ErrVaultMalformed) {
		t.Fatalf("6 bytes against a declared 5: %v", err)
	}
	// 超出条数 / 文件数必须在读取内容之前就被 check 拒绝（而不是等到流结束后 finish 才对账）。
	c = newChk(vaultTotals{Entries: 3, Files: 1, Bytes: 100})
	_, _ = c.check(hdr("a", tar.TypeReg, 1))
	if _, err := c.check(hdr("b", tar.TypeReg, 1)); !errors.Is(err, ErrVaultMalformed) || !strings.Contains(err.Error(), "more files") {
		t.Fatalf("second file against a declared 1: %v", err)
	}
	c = newChk(vaultTotals{Entries: 2, Files: 2, Bytes: 100})
	_, _ = c.check(hdr("a", tar.TypeReg, 1))
	_, _ = c.check(hdr("b", tar.TypeReg, 1))
	if _, err := c.check(hdr("d/", tar.TypeDir, 0)); !errors.Is(err, ErrVaultMalformed) || !strings.Contains(err.Error(), "more entries") {
		t.Fatalf("third entry against a declared 2: %v", err)
	}
	c = newChk(vaultTotals{Entries: 2, Files: 1, Bytes: 5})
	_, _ = c.check(hdr("d/", tar.TypeDir, 0))
	_, _ = c.check(hdr("d/f", tar.TypeReg, 5))
	if err := c.finish(); err != nil {
		t.Fatalf("exact totals: %v", err)
	}
	c = newChk(vaultTotals{Entries: 2, Files: 1, Bytes: 5})
	_, _ = c.check(hdr("d/", tar.TypeDir, 0))
	if err := c.finish(); !errors.Is(err, ErrVaultMalformed) {
		t.Fatalf("fewer than declared: %v", err)
	}
}

func TestSealSelfVerificationDryRunsTheUnsealRules(t *testing.T) {
	skipOnWindows(t)
	cases := map[string]func(dirLen int) func(string) string{
		"entry name unseal would refuse": func(int) func(string) string {
			return func(n string) string {
				if n == "Default/Preferences" {
					return `Default/Pre\ferences`
				}
				return n
			}
		},
		"entry path too long for the unseal temp dir": func(dirLen int) func(string) string {
			return func(n string) string {
				if n != "Default/Preferences" {
					return n
				}
				rel := "Default"
				want := vaultPathLimit() - 8 - dirLen - 1
				for len(rel)+1+200 < want {
					rel += "/" + strings.Repeat("d", 200)
				}
				return rel + "/" + strings.Repeat("e", want-len(rel)-1)
			}
		},
	}
	for name, mkRename := range cases {
		t.Run(name, func(t *testing.T) {
			dir, _ := newTestProfile(t)
			real, _ := resolveProfileDir(dir)
			before := snapshotTree(t, dir)
			vaultSealHookAfterWrite = func(tmpPath string, master []byte, profileID string) {
				rewriteVault(t, tmpPath, master, profileID, mkRename(len(real)))
			}
			defer func() { vaultSealHookAfterWrite = nil }()

			_, err := SealProfile(dir, testKeyStore(t))
			if !errors.Is(err, ErrVaultUnsafeEntry) || !strings.Contains(err.Error(), "self-verification") {
				t.Fatalf("want self-verification failure wrapping ErrVaultUnsafeEntry, got %v", err)
			}
			requireManifestVault(t, dir, ProfileVaultOpen)
			diffTrees(t, before, snapshotTree(t, dir))
			if got := dirNames(t, dir); contains(got, ProfileVaultFile) {
				t.Fatalf("failed seal left a vault: %v", got)
			}
		})
	}
}

// ------------------------------------------------------------
// 2. sealed + 残留明文：封存侧验证后清理
// ------------------------------------------------------------

func addLeftovers(t *testing.T, dir, outDir string) {
	t.Helper()
	writeFileMode(t, filepath.Join(dir, "stray.txt"), []byte("stray"), 0o600)
	writeFileMode(t, filepath.Join(dir, "left-dir", "inner.txt"), []byte("inner"), 0o600)
	if err := os.Symlink(outDir, filepath.Join(dir, "evil-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".dw-unseal-crashed", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestSealCleansLeftoversWhenManifestIsSealedAndVaultVerifies(t *testing.T) {
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	outDir, _ := outsideFixture(t)
	want := snapshotTree(t, dir)
	mustSeal(t, dir, ks)
	vaultBefore := readBytes(t, filepath.Join(dir, ProfileVaultFile))
	addLeftovers(t, dir, outDir)

	rc := mustSeal(t, dir, ks)
	if rc.Action != "seal" || rc.Vault != ProfileVaultSealed || !rc.Verified || !strings.Contains(rc.Note, "leftover") {
		t.Fatalf("receipt: %+v", rc)
	}
	requireOnlyManifestAndVault(t, dir)
	if !bytes.Equal(vaultBefore, readBytes(t, filepath.Join(dir, ProfileVaultFile))) {
		t.Fatal("cleaning leftovers must not rewrite the vault")
	}
	requireOutsideIntact(t, outDir)
	mustUnseal(t, dir, ks)
	diffTrees(t, want, snapshotTree(t, dir))
}

func TestSealLeftoverCleanupNeedsAVerifiedVaultAndNoRunningChrome(t *testing.T) {
	skipOnWindows(t)
	type setup struct {
		prep    func(t *testing.T, dir string, ks KeyStore) KeyStore
		wantErr error
	}
	cases := map[string]setup{
		"tampered vault": {
			prep: func(t *testing.T, dir string, ks KeyStore) KeyStore {
				p := filepath.Join(dir, ProfileVaultFile)
				raw := readBytes(t, p)
				raw[len(raw)-1] ^= 0xff
				if err := os.WriteFile(p, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				return ks
			},
			wantErr: ErrVaultAuthFailed,
		},
		"master key unavailable": {
			prep:    func(t *testing.T, dir string, ks KeyStore) KeyStore { return testKeyStore(t) },
			wantErr: ErrVaultKeyUnavailable,
		},
		"vault file missing": {
			prep: func(t *testing.T, dir string, ks KeyStore) KeyStore {
				if err := os.Remove(filepath.Join(dir, ProfileVaultFile)); err != nil {
					t.Fatal(err)
				}
				return ks
			},
			wantErr: ErrVaultMissing,
		},
		"chrome still running": {
			prep: func(t *testing.T, dir string, ks KeyStore) KeyStore {
				if err := os.Symlink(fmt.Sprintf("host-%d", os.Getpid()), filepath.Join(dir, "SingletonLock")); err != nil {
					t.Fatal(err)
				}
				return ks
			},
			wantErr: ErrProfileInUse,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir, _ := newTestProfile(t)
			ks := testKeyStore(t)
			outDir, _ := outsideFixture(t)
			mustSeal(t, dir, ks)
			addLeftovers(t, dir, outDir)
			useKS := tc.prep(t, dir, ks)
			before := snapshotTree(t, dir)

			_, err := SealProfile(dir, useKS)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("want %v, got %v", tc.wantErr, err)
			}
			// 任何一步不过：残留一个都没动，也没有凭空创建主密钥。
			diffTrees(t, before, snapshotTree(t, dir))
			if _, err := os.Stat(filepath.Join(dir, "stray.txt")); err != nil {
				t.Fatalf("leftover was deleted: %v", err)
			}
			requireOutsideIntact(t, outDir)
			if name == "master key unavailable" {
				if _, lerr := useKS.Load(); !errors.Is(lerr, ErrVaultKeyNotFound) {
					t.Fatalf("cleanup path must not create a master key: %v", lerr)
				}
			}
		})
	}
}

// ------------------------------------------------------------
// 3. 启动租约 + 更完整的"是否在运行"检测
// ------------------------------------------------------------

func TestUnsealForLaunchHoldsTheVaultLockUntilReleased(t *testing.T) {
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	want := snapshotTree(t, dir)
	mustSeal(t, dir, ks)

	old := vaultLockWait
	vaultLockWait = 150 * time.Millisecond
	defer func() { vaultLockWait = old }()

	rc, release, err := UnsealProfileForLaunch(dir, ks)
	if err != nil || rc.Action != "unseal" || release == nil {
		t.Fatalf("UnsealProfileForLaunch: %+v release=%v err=%v", rc, release != nil, err)
	}
	// 租约期间：Chrome 还没建好 SingletonLock，别的进程的封存/解封都必须被挡在锁外。
	if _, err := SealProfile(dir, ks); !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("Seal during the launch lease: want ErrProfileInUse, got %v", err)
	}
	if _, err := UnsealProfile(dir, ks); !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("Unseal during the launch lease: want ErrProfileInUse, got %v", err)
	}
	diffTrees(t, want, snapshotTree(t, dir))
	requireManifestVault(t, dir, ProfileVaultOpen)

	release()
	release() // 幂等
	mustSeal(t, dir, ks)
	requireOnlyManifestAndVault(t, dir)
}

func TestUnsealForLaunchReleasesTheLockWhenUnsealFails(t *testing.T) {
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	mustSeal(t, dir, ks)
	p := filepath.Join(dir, ProfileVaultFile)
	raw := readBytes(t, p)
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	_, release, err := UnsealProfileForLaunch(dir, ks)
	if !errors.Is(err, ErrVaultAuthFailed) {
		t.Fatalf("want ErrVaultAuthFailed, got %v", err)
	}
	if release == nil {
		t.Fatal("release must be non-nil even on error")
	}
	release()
	real, _ := resolveProfileDir(dir)
	old := vaultLockWait
	vaultLockWait = 100 * time.Millisecond
	defer func() { vaultLockWait = old }()
	lock, err := acquireProfileVaultLock(real)
	if err != nil {
		t.Fatalf("lock still held after a failed UnsealProfileForLaunch: %v", err)
	}
	_ = lock.Close()
}

func TestSealRefusedWhileOwnerMarkerNamesALiveProcess(t *testing.T) {
	skipOnWindows(t)
	for name, mk := range map[string]func(live int) (owner, chrome, mux int){
		"chrome_pid alive":           func(live int) (int, int, int) { return 0, live, 0 },
		"owner_pid alive":            func(live int) (int, int, int) { return live, 0, 0 },
		"browser_mux_host_pid alive": func(live int) (int, int, int) { return 0, 0, live },
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := newTestProfile(t)
			ks := testKeyStore(t)
			sleeper := spawnSleeper(t)
			o, c, m := mk(sleeper.Process.Pid)
			writeOwnerMarker(t, dir, o, c, m)
			before := snapshotTree(t, dir)

			_, err := SealProfile(dir, ks)
			if !errors.Is(err, ErrProfileInUse) {
				t.Fatalf("want ErrProfileInUse, got %v", err)
			}
			requireManifestVault(t, dir, ProfileVaultOpen)
			diffTrees(t, before, snapshotTree(t, dir))

			// 进程退出之后同一份 marker 不再挡路。
			stopProcess(t, sleeper)
			mustSeal(t, dir, ks)
			requireOnlyManifestAndVault(t, dir)
		})
	}

	t.Run("pids that are dead or are this very process do not block", func(t *testing.T) {
		dir, _ := newTestProfile(t)
		dead := deadPID(t)
		// mux host 在自己进程里封存：marker 里的 owner/mux pid 就是本进程，不能把自己挡在外面。
		writeOwnerMarker(t, dir, os.Getpid(), dead, os.Getpid())
		mustSeal(t, dir, testKeyStore(t))
		requireOnlyManifestAndVault(t, dir)
	})
}

func TestSealRefusedWhileAProcessRunsWithTheProfileDir(t *testing.T) {
	skipOnWindows(t)
	dir, _ := newTestProfile(t)
	real, err := resolveProfileDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ks := testKeyStore(t)
	chrome := spawnSleeper(t, "--user-data-dir="+real)
	before := snapshotTree(t, dir)

	_, err = SealProfile(dir, ks)
	if !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("want ErrProfileInUse while a process has --user-data-dir=%s, got %v", real, err)
	}
	var inUse *ProfileInUseError
	if !errors.As(err, &inUse) || inUse.PID != chrome.Process.Pid {
		t.Fatalf("error should name the process: %#v", err)
	}
	diffTrees(t, before, snapshotTree(t, dir))

	stopProcess(t, chrome)
	mustSeal(t, dir, ks)
}

// ------------------------------------------------------------
// 4. open 清单 + vault + 没有明文：按 sealed 处理，绝不删 vault
// ------------------------------------------------------------

func TestUnsealTreatsOpenManifestWithVaultAndNoPlaintextAsSealed(t *testing.T) {
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	want := snapshotTree(t, dir)
	mustSeal(t, dir, ks)
	setManifestVault(t, dir, ProfileVaultOpen) // 清单被回滚 / 写坏成 open，目录里只有 vault

	rc := mustUnseal(t, dir, ks)
	if rc.Action != "unseal" || rc.Vault != ProfileVaultOpen {
		t.Fatalf("receipt: %+v", rc)
	}
	diffTrees(t, want, snapshotTree(t, dir))
	if _, err := os.Stat(filepath.Join(dir, ProfileVaultFile)); !os.IsNotExist(err) {
		t.Fatalf("vault should be consumed only after a successful unseal: %v", err)
	}
}

func TestUnsealOfOpenManifestWithOnlyAVaultKeepsTheVaultOnFailure(t *testing.T) {
	dir, _ := newTestProfile(t)
	mustSeal(t, dir, testKeyStore(t))
	setManifestVault(t, dir, ProfileVaultOpen)
	vaultPath := filepath.Join(dir, ProfileVaultFile)
	before := readBytes(t, vaultPath)

	if _, err := UnsealProfile(dir, testKeyStore(t)); !errors.Is(err, ErrVaultKeyUnavailable) {
		t.Fatalf("want ErrVaultKeyUnavailable, got %v", err)
	}
	other := testKeyStore(t)
	if _, err := LoadOrCreateMasterKey(other); err != nil {
		t.Fatal(err)
	}
	if _, err := UnsealProfile(dir, other); !errors.Is(err, ErrVaultAuthFailed) {
		t.Fatalf("want ErrVaultAuthFailed, got %v", err)
	}
	if !bytes.Equal(before, readBytes(t, vaultPath)) {
		t.Fatal("the vault must survive every failed unseal untouched")
	}
}

func TestSealNeverOverwritesAVaultWhenThereIsNoPlaintext(t *testing.T) {
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	want := snapshotTree(t, dir)
	mustSeal(t, dir, ks)
	vaultPath := filepath.Join(dir, ProfileVaultFile)
	before := readBytes(t, vaultPath)
	setManifestVault(t, dir, ProfileVaultOpen)
	// 只有运行期垃圾（不算明文）。
	if err := os.Symlink(fmt.Sprintf("host-%d", deadPID(t)), filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatal(err)
	}

	rc := mustSeal(t, dir, ks)
	if rc.Action != "noop" || rc.Vault != ProfileVaultSealed {
		t.Fatalf("receipt: %+v", rc)
	}
	if !bytes.Equal(before, readBytes(t, vaultPath)) {
		t.Fatal("sealing an empty directory must not replace an existing vault with an empty one")
	}
	requireManifestVault(t, dir, ProfileVaultSealed)
	mustUnseal(t, dir, ks)
	diffTrees(t, want, snapshotTree(t, dir))
}

// ------------------------------------------------------------
// 5. 解封落盘：置 open 之前逐文件 fsync + 目录 fsync
// ------------------------------------------------------------

func TestUnsealSyncsEverythingBeforeTheManifestSaysOpen(t *testing.T) {
	skipOnWindows(t)
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)
	want := snapshotTree(t, dir)
	mustSeal(t, dir, ks)
	real, err := resolveProfileDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	tmpRE := regexp.MustCompile("^" + regexp.QuoteMeta(real) + `/\.dw-unseal-[^/]+(?:/(.*))?$`)
	var mu sync.Mutex
	files, dirs := map[string]bool{}, map[string]bool{}
	var violations []string
	vaultSyncObserver = func(kind, path string) {
		mu.Lock()
		defer mu.Unlock()
		if m, err := ReadProfileManifest(dir); err != nil || m.Vault != ProfileVaultSealed {
			violations = append(violations, kind+" "+path)
		}
		rel := "<root>"
		if path != real {
			sub := tmpRE.FindStringSubmatch(path)
			if sub == nil {
				violations = append(violations, "unexpected path "+path)
				return
			}
			rel = sub[1]
		}
		if kind == "file" {
			files[rel] = true
		} else {
			dirs[rel] = true
		}
	}
	defer func() { vaultSyncObserver = nil }()

	mustUnseal(t, dir, ks)

	if len(violations) > 0 {
		t.Fatalf("fsync must happen while the manifest still says sealed: %v", violations)
	}
	for rel, desc := range want {
		if strings.HasPrefix(desc, "file") && !files[filepath.ToSlash(rel)] {
			t.Errorf("extracted file %s was not fsynced", rel)
		}
		if strings.HasPrefix(desc, "dir") && !dirs[filepath.ToSlash(rel)] {
			t.Errorf("extracted directory %s was not fsynced", rel)
		}
	}
	if !dirs[""] {
		t.Error("the extraction temp dir itself was not fsynced")
	}
	if !dirs["<root>"] {
		t.Error("the profile root was not fsynced after the entries were moved in")
	}
}

// ------------------------------------------------------------
// 6. 头部总量 + 非常规 tar 条目
// ------------------------------------------------------------

func TestUnsealAbortsWhenContentDisagreesWithTheHeaderTotals(t *testing.T) {
	skipOnWindows(t)
	threeFiles := func(t *testing.T, tw *tar.Writer) {
		tarFile(t, tw, "a", "1234567890")
		tarFile(t, tw, "b", "1234567890")
		tarFile(t, tw, "c", "1234567890")
	}
	filesAndDirs := func(t *testing.T, tw *tar.Writer) {
		for _, d := range []string{"d1/", "d2/", "d3/"} {
			if err := tw.WriteHeader(&tar.Header{Name: d, Typeflag: tar.TypeDir, Mode: 0o700}); err != nil {
				t.Fatal(err)
			}
		}
		tarFile(t, tw, "a", "1")
		tarFile(t, tw, "b", "2")
	}
	cases := map[string]struct {
		build   func(t *testing.T, tw *tar.Writer)
		totals  vaultTotals
		wantErr bool
	}{
		"exact totals are accepted":     {threeFiles, vaultTotals{Entries: 3, Files: 3, Bytes: 30}, false},
		"more files than declared":      {threeFiles, vaultTotals{Entries: 3, Files: 2, Bytes: 30}, true},
		"more entries than declared":    {filesAndDirs, vaultTotals{Entries: 4, Files: 2, Bytes: 3}, true},
		"more bytes than declared":      {threeFiles, vaultTotals{Entries: 3, Files: 3, Bytes: 29}, true},
		"fewer files than declared":     {threeFiles, vaultTotals{Entries: 9, Files: 9, Bytes: 30}, true},
		"fewer bytes than declared":     {threeFiles, vaultTotals{Entries: 3, Files: 3, Bytes: 31}, true},
		"declares nothing but has data": {threeFiles, vaultTotals{}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "profile")
			ks := testKeyStore(t)
			buildFixtureWithTotals(t, dir, ks, tc.totals, func(tw *tar.Writer) { tc.build(t, tw) })
			_, err := UnsealProfile(dir, ks)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("exact totals must unseal: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrVaultMalformed) {
				t.Fatalf("want ErrVaultMalformed, got %v", err)
			}
			requireOnlyManifestAndVault(t, dir)
			requireManifestVault(t, dir, ProfileVaultSealed)
		})
	}
}

func TestUnsealRejectsUnusualTarEntries(t *testing.T) {
	skipOnWindows(t)
	cases := map[string]*tar.Header{
		"pax xattr record":  {Name: "x", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1, PAXRecords: map[string]string{"SCHILY.xattr.user.evil": "1"}},
		"pax unknown key":   {Name: "x", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1, PAXRecords: map[string]string{"comment": "hi"}},
		"gnu format header": {Name: "x", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1, Format: tar.FormatGNU},
	}
	for name, hdr := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "profile")
			ks := testKeyStore(t)
			build := func(tw *tar.Writer) {
				if err := tw.WriteHeader(hdr); err != nil {
					t.Skipf("tar writer refuses to produce this entry: %v", err)
				}
				if _, err := tw.Write([]byte("x")); err != nil {
					t.Skipf("tar writer: %v", err)
				}
			}
			buildFixtureWithTotals(t, dir, ks, vaultTotals{Entries: 1, Files: 1, Bytes: 1}, build)
			_, err := UnsealProfile(dir, ks)
			if !errors.Is(err, ErrVaultUnsafeEntry) {
				t.Fatalf("want ErrVaultUnsafeEntry, got %v", err)
			}
			requireOnlyManifestAndVault(t, dir)
		})
	}
}

// rawTarHeader 手工拼一个 ustar 头块。Go 的 tar.Writer 会悄悄丢弃 GNU.sparse.* 记录，
// 所以要验证"解封拒绝稀疏条目"，只能自己拼原始字节。
func rawTarHeader(name string, typeflag byte, size int64) []byte {
	b := make([]byte, 512)
	copy(b[0:], name)
	copy(b[100:], "0000600\x00")
	copy(b[108:], "0000000\x00")
	copy(b[116:], "0000000\x00")
	copy(b[124:], fmt.Sprintf("%011o\x00", size))
	copy(b[136:], "00000000000\x00")
	copy(b[148:], "        ")
	b[156] = typeflag
	copy(b[257:], "ustar\x00")
	copy(b[263:], "00")
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:], fmt.Sprintf("%06o\x00 ", sum))
	return b
}

func padTo512(data []byte) []byte {
	if rem := len(data) % 512; rem != 0 {
		data = append(data, make([]byte, 512-rem)...)
	}
	return data
}

func paxRecord(k, v string) string {
	body := " " + k + "=" + v + "\n"
	n := len(body)
	for {
		l := len(fmt.Sprintf("%d", n)) + len(body)
		if l == n {
			break
		}
		n = l
	}
	return fmt.Sprintf("%d%s", n, body)
}

// writeVaultRawPlaintext 用真实密钥和格式封一段"原始明文字节"（不经过 tar.Writer）。
func writeVaultRawPlaintext(t *testing.T, dir string, ks KeyStore, totals vaultTotals, plain []byte) {
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
	salt, prefix := randBytes(t, vaultSaltLen), randBytes(t, vaultNoncePrefix)
	framed, digest, err := encodeVaultHeader(vaultHeader{
		Version: 1, Alg: vaultAlgName, KDF: vaultKDFName, Salt: salt, NoncePrefix: prefix,
		ProfileID: m.ProfileID, ChunkSize: vaultChunkSize,
		Entries: totals.Entries, Files: totals.Files, Bytes: totals.Bytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	aead, err := deriveVaultAEAD(master, salt, m.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	out.Write(framed)
	vw := newVaultWriter(&out, aead, prefix, digest, vaultChunkSize)
	if _, err := vw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := vw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ProfileVaultFile), out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteProfileManifest(dir, m); err != nil {
		t.Fatal(err)
	}
}

func TestUnsealRejectsPAXSparseEntries(t *testing.T) {
	skipOnWindows(t)
	// PAX 0.1 稀疏文件：磁盘上只有 1 字节数据，却声明 1 TB 的真实大小——照做会把磁盘写满。
	records := paxRecord("GNU.sparse.major", "0") + paxRecord("GNU.sparse.minor", "1") +
		paxRecord("GNU.sparse.numblocks", "1") + paxRecord("GNU.sparse.map", "0,1") +
		paxRecord("GNU.sparse.size", "1000000000000") + paxRecord("GNU.sparse.name", "x")
	var raw []byte
	raw = append(raw, rawTarHeader("PaxHeaders/x", 'x', int64(len(records)))...)
	raw = append(raw, padTo512([]byte(records))...)
	raw = append(raw, rawTarHeader("x", '0', 1)...)
	raw = append(raw, padTo512([]byte("x"))...)
	raw = append(raw, make([]byte, 1024)...)

	// 先确认夹具本身：Go 的 reader 确实把它读成了带 GNU.sparse 记录的条目，而不是普通文件。
	h, err := tar.NewReader(bytes.NewReader(raw)).Next()
	if err != nil {
		t.Fatalf("fixture is not a readable tar: %v", err)
	}
	var sparseKeys int
	for k := range h.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			sparseKeys++
		}
	}
	if sparseKeys == 0 {
		t.Fatalf("fixture lost its GNU.sparse records: %+v", h.PAXRecords)
	}

	dir := filepath.Join(t.TempDir(), "profile")
	ks := testKeyStore(t)
	// 头部声明只有 1 字节：即使稀疏检查失效，总量上限也会在写盘之前拦住 1 TB 的声明（变异验证时不会真的写满磁盘）。
	writeVaultRawPlaintext(t, dir, ks, vaultTotals{Entries: 1, Files: 1, Bytes: 1}, raw)
	_, err = UnsealProfile(dir, ks)
	if !errors.Is(err, ErrVaultUnsafeEntry) {
		t.Fatalf("want ErrVaultUnsafeEntry for a sparse entry, got %v", err)
	}
	requireOnlyManifestAndVault(t, dir)
	requireManifestVault(t, dir, ProfileVaultSealed)
}

// ------------------------------------------------------------
// 7. vault 锁目录：每用户私有
// ------------------------------------------------------------

func withLockBase(t *testing.T) string {
	t.Helper()
	base := realTempDir(t)
	old := vaultLockBaseDir
	vaultLockBaseDir = func() (string, error) { return base, nil }
	t.Cleanup(func() { vaultLockBaseDir = old })
	return base
}

func noneStateProfile(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := NewProfileManifest(ProfileOSCryptSystem, ProfileVaultNone, "visible")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteProfileManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestVaultLockLivesInAPerUserPrivateDirectory(t *testing.T) {
	skipOnWindows(t)
	base := withLockBase(t)
	if _, err := SealProfile(noneStateProfile(t), testKeyStore(t)); err != nil {
		t.Fatal(err)
	}
	lockDir := filepath.Join(base, "dw-browser", "vault-locks")
	fi, err := os.Lstat(lockDir)
	if err != nil {
		t.Fatalf("lock directory not created under the per-user base dir: %v", err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("lock dir mode %v", fi.Mode())
	}
	if names := dirNames(t, lockDir); len(names) != 1 || !strings.HasSuffix(names[0], ".lock") {
		t.Fatalf("lock files: %v", names)
	}
}

func TestVaultLockDirectoryIsValidated(t *testing.T) {
	skipOnWindows(t)
	for name, prep := range map[string]func(t *testing.T, lockDir string){
		"permissions too open": func(t *testing.T, lockDir string) {
			if err := os.MkdirAll(lockDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(lockDir, 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"is a symlink": func(t *testing.T, lockDir string) {
			if err := os.MkdirAll(filepath.Dir(lockDir), 0o700); err != nil {
				t.Fatal(err)
			}
			target := realTempDir(t)
			if err := os.Chmod(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, lockDir); err != nil {
				t.Fatal(err)
			}
		},
		"owned by someone else": func(t *testing.T, lockDir string) {
			if err := os.MkdirAll(lockDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(lockDir, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			base := withLockBase(t)
			lockDir := filepath.Join(base, "dw-browser", "vault-locks")
			prep(t, lockDir)
			if name == "owned by someone else" {
				old := vaultFileOwner
				vaultFileOwner = func(fs.FileInfo) (int, bool) { return os.Getuid() + 1, true }
				defer func() { vaultFileOwner = old }()
			}
			_, err := SealProfile(noneStateProfile(t), testKeyStore(t))
			if !errors.Is(err, ErrVaultLockDirUnsafe) {
				t.Fatalf("want ErrVaultLockDirUnsafe, got %v", err)
			}
		})
	}

	t.Run("a correct pre-existing directory is accepted", func(t *testing.T) {
		base := withLockBase(t)
		lockDir := filepath.Join(base, "dw-browser", "vault-locks")
		if err := os.MkdirAll(lockDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(lockDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := SealProfile(noneStateProfile(t), testKeyStore(t)); err != nil {
			t.Fatal(err)
		}
	})
}

// ------------------------------------------------------------
// 8. 删除 vault 之前必须先关掉句柄
// ------------------------------------------------------------

// ------------------------------------------------------------
// 9. 归类：宽松档、混合库、缓存允许列表位置
// ------------------------------------------------------------

func legacyRows(t *testing.T, n int) []testCookie {
	var rows []testCookie
	for i := 0; i < n; i++ {
		rows = append(rows, testCookie{fmt.Sprintf(".legacy%d.example.com", i), oldFormatCookie(t, []byte(fmt.Sprintf("session-%d-value", i)), "mock_password", 1003)})
	}
	return rows
}

func systemRows(t *testing.T, n int) []testCookie {
	var rows []testCookie
	for i := 0; i < n; i++ {
		rows = append(rows, testCookie{fmt.Sprintf(".sys%d.example.com", i), mockEncryptCookie(t, fmt.Sprintf(".sys%d.example.com", i), "secret", "real-keychain-password", 1003)})
	}
	return rows
}

func strictRows(t *testing.T, n int) []testCookie {
	var rows []testCookie
	for i := 0; i < n; i++ {
		rows = append(rows, testCookie{fmt.Sprintf(".ok%d.example.com", i), mockEncryptCookie(t, fmt.Sprintf(".ok%d.example.com", i), "v", "mock_password", 1003)})
	}
	return rows
}

func TestClassifyLegacyCookiesWithoutDomainBindingUseTheLenientTier(t *testing.T) {
	cls := classifyCookies(t, legacyRows(t, 3))
	if cls.OSCrypt != ProfileOSCryptMock || cls.Vault != ProfileVaultOpen || cls.Basis != ClassifyBasisMockLenient {
		t.Fatalf("%+v", cls)
	}
	if cls.CookiesProbed != 3 || cls.MockOK != 3 || cls.StrictOK != 0 {
		t.Fatalf("counters: %+v", cls)
	}
}

func TestClassifyLenientTierNeedsEverySampledRowToPass(t *testing.T) {
	// 两条旧版 mock Cookie + 一条钥匙串加密的：不是"每条都满足"，不能判 mock。
	rows := append(legacyRows(t, 2), systemRows(t, 1)...)
	cls := classifyCookies(t, rows)
	if cls.OSCrypt != ProfileOSCryptSystem || cls.Basis != ClassifyBasisSystemCookies {
		t.Fatalf("%+v", cls)
	}
	// 解出来的明文不是合法 UTF-8 也不行。
	bin := []testCookie{{".bin.example.com", oldFormatCookie(t, []byte{0xff, 0xfe, 0xfd, 0x80, 0x81}, "mock_password", 1003)}}
	if cls := classifyCookies(t, bin); cls.OSCrypt != ProfileOSCryptSystem {
		t.Fatalf("non-UTF-8 plaintext must not pass the lenient tier: %+v", cls)
	}
}

func TestClassifyLenientTierSamplesAtMostTwentyRows(t *testing.T) {
	rows := append(legacyRows(t, 20), systemRows(t, 5)...)
	cls := classifyCookies(t, rows)
	if cls.OSCrypt != ProfileOSCryptMock || cls.Basis != ClassifyBasisMockLenient {
		t.Fatalf("only the first 20 rows are sampled: %+v", cls)
	}
	if cls.CookiesProbed != 25 || cls.MockOK != 20 || cls.StrictOK != 0 {
		t.Fatalf("counters: %+v", cls)
	}
}

func TestClassifyMixedLibraryFollowsTheMajority(t *testing.T) {
	for name, tc := range map[string]struct {
		strict, system int
		want           string
	}{
		"3 mock vs 2 system": {3, 2, ProfileOSCryptMock},
		"1 mock vs 3 system": {1, 3, ProfileOSCryptSystem},
		"tie goes to mock":   {2, 2, ProfileOSCryptMock},
		"all system":         {0, 4, ProfileOSCryptSystem},
		"mock only":          {4, 0, ProfileOSCryptMock},
	} {
		t.Run(name, func(t *testing.T) {
			cls := classifyCookies(t, append(strictRows(t, tc.strict), systemRows(t, tc.system)...))
			if cls.OSCrypt != tc.want {
				t.Fatalf("os_crypt %s, want %s: %+v", cls.OSCrypt, tc.want, cls)
			}
			if cls.CookiesProbed != tc.strict+tc.system || cls.StrictOK != tc.strict || cls.MockOK != tc.strict {
				t.Fatalf("counters: %+v", cls)
			}
			if tc.want == ProfileOSCryptSystem && cls.Vault != ProfileVaultNone {
				t.Fatalf("system must not be vaulted: %+v", cls)
			}
		})
	}
}

func TestSealCacheAllowListOnlyMatchesKnownLocations(t *testing.T) {
	skipOnWindows(t)
	dir, _ := newTestProfile(t)
	ks := testKeyStore(t)

	purged := []string{
		"Cache", "ShaderCache", "component_crx_cache", // profile 根目录第一层
		"Default/Cache", "Default/Code Cache", "Default/GPUCache", // Default/ 第一层
		"Profile 1/GPUCache", "Profile 12/Cache", "Guest Profile/Cache", "System Profile/Code Cache",
	}
	kept := []string{
		"Default/Extensions/abcdef/Cache/data", // 扩展自己的数据目录恰好叫 Cache：是用户数据
		"Default/Service Worker/Cache/index",
		"Default/Foo/GPUCache/x",
		"Profile X/Cache/x", // 不是 "Profile <数字>"
		"Profile/Cache/x",
		"Other/Cache/x", // 顶层但不是 profile 目录
		"Default/Extensions/ext/ShaderCache/x",
	}
	for _, rel := range purged {
		writeFileMode(t, filepath.Join(dir, filepath.FromSlash(rel), "f"), []byte("CACHE-"+rel), 0o600)
	}
	for _, rel := range kept {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if filepath.Base(p) == "data" || filepath.Base(p) == "index" || filepath.Base(p) == "x" {
			writeFileMode(t, p, []byte("KEEP-"+rel), 0o600)
		}
	}
	want := snapshotTree(t, dir)
	for _, rel := range purged {
		for k := range want {
			if k == filepath.FromSlash(rel) || strings.HasPrefix(k, filepath.FromSlash(rel)+string(filepath.Separator)) {
				delete(want, k)
			}
		}
	}

	rc := mustSeal(t, dir, ks)
	if rc.CachesPurged != len(purged) {
		t.Fatalf("caches purged = %d, want %d", rc.CachesPurged, len(purged))
	}
	mustUnseal(t, dir, ks)
	diffTrees(t, want, snapshotTree(t, dir))
	for _, rel := range kept {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("user data %s was purged as if it were a cache: %v", rel, err)
		}
	}
}

// ------------------------------------------------------------
// 10. 非 darwin 密钥文件放到用户配置目录
// ------------------------------------------------------------

func TestDefaultKeyFileLivesInTheUserConfigDirNotInTheProfileRoot(t *testing.T) {
	skipOnWindows(t)
	home := realTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg-config"))
	cfg, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cfg, "dw-browser", "vault-master.key")
	if got := DefaultProfileKeyFilePath(); got != want {
		t.Fatalf("DefaultProfileKeyFilePath = %q, want %q", got, want)
	}
	if root := BrowserCLIProfileRoot(""); root != "" && strings.HasPrefix(want, root+string(filepath.Separator)) {
		t.Fatalf("key file must not live under the profile root %s", root)
	}

	ks := defaultFileKeyStore()
	if _, err := LoadOrCreateMasterKey(ks); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(filepath.Dir(want))
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("key dir mode %v, want 0700", di.Mode().Perm())
	}
	fi, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v, want 0600", fi.Mode().Perm())
	}
}

// rawMockCBC 用模拟口令 + 固定 IV 加密"已经填充好"的明文，不替调用方补 PKCS7——
// 用来造"其余都合法、只有填充（或前缀、对齐）这一处违规"的样本，逐个验证宽松档的每道检查都在起作用。
func rawMockCBC(t *testing.T, padded []byte, prefix string) []byte {
	t.Helper()
	if len(padded)%16 != 0 {
		t.Fatal("rawMockCBC needs block-aligned input")
	}
	key, err := pbkdf2.Key(sha1.New, "mock_password", []byte("saltysalt"), 1003, 16)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte("                ")).CryptBlocks(ct, padded)
	return append([]byte(prefix), ct...)
}

func TestClassifyLenientTierChecksEveryShapeRule(t *testing.T) {
	goodRow := testCookie{".ok.example", oldFormatCookie(t, []byte("fine"), "mock_password", 1003)}
	block := func(tail ...byte) []byte { // 前面补 ASCII，保证只有 tail 描述的部分有问题
		b := bytes.Repeat([]byte("a"), 16-len(tail))
		return append(b, tail...)
	}
	valid := rawMockCBC(t, block(0x03, 0x03, 0x03), "v10")

	// 对照：自己拼的合法样本（含"整块填充 = 空值"）能通过，说明下面各例的失败确实来自被点名的那一处。
	for name, enc := range map[string][]byte{
		"valid padding 3":       valid,
		"full block of padding": rawMockCBC(t, bytes.Repeat([]byte{0x10}, 16), "v10"),
	} {
		if cls := classifyCookies(t, []testCookie{goodRow, {".c.example", enc}}); cls.OSCrypt != ProfileOSCryptMock {
			t.Fatalf("control %q should pass the lenient tier: %+v", name, cls)
		}
	}

	for name, enc := range map[string][]byte{
		"v11 prefix on ciphertext that does decrypt": append([]byte("v11"), valid[3:]...),
		"v20 prefix on ciphertext that does decrypt": append([]byte("v20"), valid[3:]...),
		"valid ciphertext plus trailing bytes":       append(append([]byte{}, valid...), 1, 2, 3, 4, 5),
		"padding byte is 0":                          rawMockCBC(t, block(0x00), "v10"),
		"padding byte exceeds the block size":        rawMockCBC(t, block(0x11), "v10"),
		"padding bytes are inconsistent":             rawMockCBC(t, block(0x01, 0x02, 0x03), "v10"),
		"padding claims more than the data holds":    rawMockCBC(t, block(0x10), "v10"),
		"valid padding but not UTF-8":                rawMockCBC(t, append(bytes.Repeat([]byte{0xff}, 13), 0x03, 0x03, 0x03), "v10"),
		"shorter than one block":                     append([]byte("v10"), 1, 2, 3, 4),
	} {
		t.Run(name, func(t *testing.T) {
			cls := classifyCookies(t, []testCookie{goodRow, {".bad.example", enc}})
			if cls.OSCrypt != ProfileOSCryptSystem || cls.Basis != ClassifyBasisSystemCookies {
				t.Fatalf("one malformed sample must keep the profile on the system keychain: %+v", cls)
			}
		})
	}
}

// 边界用字面量 20，不用生产常量：测试若引用被测常量，常量被改时测试的边界会一起漂走。
func TestClassifyLenientSampleBoundaryIsExactlyTwenty(t *testing.T) {
	good := func() testCookie {
		return testCookie{".ok.example", oldFormatCookie(t, []byte("v"), "mock_password", 1003)}
	}
	bad := func() testCookie {
		return testCookie{".bad.example", oldFormatCookie(t, []byte{0xff, 0xfe}, "mock_password", 1003)}
	}
	build := func(badAt int) []testCookie {
		rows := make([]testCookie, 25)
		for i := range rows {
			rows[i] = good()
		}
		rows[badAt] = bad()
		return rows
	}
	if got := lenientSampleLimit; got != 20 {
		t.Fatalf("the documented sample limit is 20, got %d", got)
	}
	for badAt, want := range map[int]string{18: ProfileOSCryptSystem, 19: ProfileOSCryptSystem, 20: ProfileOSCryptMock, 21: ProfileOSCryptMock, 24: ProfileOSCryptMock} {
		if cls := classifyCookies(t, build(badAt)); cls.OSCrypt != want {
			t.Errorf("bad cookie at index %d (row %d): os_crypt=%s, want %s (%+v)", badAt, badAt+1, cls.OSCrypt, want, cls)
		}
	}
}

// 宽松档的判断函数本身对"空密文 / 过短 / 空切片"必须稳妥地返回 false，不能越界 panic
// （实际流程里 SQL 的 length>3 和块对齐检查会先挡住，但函数可以被直接调用）。
func TestMockCookiesDecryptLenientNeverPanicsOnDegenerateInput(t *testing.T) {
	for name, enc := range map[string][]byte{
		"nil":               nil,
		"empty":             {},
		"prefix only":       []byte("v10"),
		"prefix + 1 byte":   []byte("v10x"),
		"prefix + 15 bytes": append([]byte("v10"), make([]byte, 15)...),
		"prefix + 17 bytes": append([]byte("v10"), make([]byte, 17)...),
		"too short, no pfx": []byte("v"),
	} {
		for _, key := range mockKeys() {
			if mockCookieDecryptsLenient(key, enc) && name != "prefix + 17 bytes" {
				t.Errorf("%s: must not be accepted", name)
			}
		}
		if mockCookiesDecryptLenient([][]byte{enc}) && name != "prefix + 17 bytes" {
			t.Errorf("%s as a sample: must not be accepted", name)
		}
	}
}
