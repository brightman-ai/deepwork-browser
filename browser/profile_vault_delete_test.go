package browser

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// realTempDir 返回已解析 symlink 的临时目录（macOS 的 /var -> /private/var）。
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// outsideFixture 造一个"profile 之外"的目录，里面的东西无论如何都不能被删或被改。
func outsideFixture(t *testing.T) (dir, file string) {
	t.Helper()
	dir = realTempDir(t)
	writeFileMode(t, filepath.Join(dir, "inner", "deep.txt"), []byte("OUTSIDE-DEEP"), 0o600)
	file = filepath.Join(dir, "outside.txt")
	writeFileMode(t, file, []byte("OUTSIDE-FILE"), 0o600)
	return dir, file
}

func requireOutsideIntact(t *testing.T, dir string) {
	t.Helper()
	for rel, want := range map[string]string{"inner/deep.txt": "OUTSIDE-DEEP", "outside.txt": "OUTSIDE-FILE"} {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Fatalf("outside file %s was touched: %q %v", rel, got, err)
		}
	}
}

func requireDeleteRefused(t *testing.T, err error) *VaultDeleteRefusedError {
	t.Helper()
	if !errors.Is(err, ErrVaultDeleteRefused) {
		t.Fatalf("want ErrVaultDeleteRefused, got %v", err)
	}
	var refused *VaultDeleteRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("want *VaultDeleteRefusedError, got %T", err)
	}
	return refused
}

func filterTree(tree map[string]string, dropPrefix string) map[string]string {
	out := map[string]string{}
	for k, v := range tree {
		if k == dropPrefix || strings.HasPrefix(k, dropPrefix+string(filepath.Separator)) {
			continue
		}
		out[k] = v
	}
	return out
}

// ------------------------------------------------------------
// 清单校验：越界路径、被篡改的清单
// ------------------------------------------------------------

func TestDeletePlanRefusesForgedItemsAndDeletesNothing(t *testing.T) {
	skipOnWindows(t)
	outDir, outFile := outsideFixture(t)
	outInfo, err := os.Lstat(outFile)
	if err != nil {
		t.Fatal(err)
	}

	forged := map[string]string{
		"parent escape":           "../outside.txt",
		"parent escape in middle": "sub/../../outside.txt",
		"absolute path":           outFile,
		"profile root itself":     ".",
		"empty":                   "",
		"manifest is protected":   ProfileManifestFile,
		"vault file is protected": ProfileVaultFile,
		"unclean path":            "sub//b.txt",
		"dot segment":             "./a.txt",
	}
	for name, rel := range forged {
		for _, position := range []string{"first", "last"} {
			t.Run(name+"/"+position, func(t *testing.T) {
				root := realTempDir(t)
				writeFileMode(t, filepath.Join(root, "a.txt"), []byte("A"), 0o600)
				writeFileMode(t, filepath.Join(root, "sub", "b.txt"), []byte("B"), 0o600)
				writeFileMode(t, filepath.Join(root, ProfileManifestFile), []byte("{}"), 0o600)
				// 真实清单（不含 manifest/vault）+ 一条伪造项（用越界目标的真实 Lstat 信息，让它看起来"合法"）。
				items, err := planSubtrees(root, func(top string) bool { return top != ProfileManifestFile }, vaultItemLeftover)
				if err != nil {
					t.Fatal(err)
				}
				bad := vaultItem{rel: rel, info: outInfo, kind: vaultItemLeftover}
				if position == "first" {
					items = append([]vaultItem{bad}, items...)
				} else {
					items = append(items, bad)
				}
				plan := &vaultDeletePlan{root: root, keep: newVaultKeep(ProfileManifestFile, ProfileVaultFile), items: items}

				requireDeleteRefused(t, plan.validate(nil))
				requireDeleteRefused(t, plan.execute())

				// 整体中止：连清单里本来合法的条目也一个都没删。
				for _, p := range []string{"a.txt", "sub/b.txt", ProfileManifestFile} {
					if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
						t.Fatalf("%s was deleted although the plan was refused: %v", p, err)
					}
				}
				requireOutsideIntact(t, outDir)
			})
		}
	}
}

func TestDeletePlanRefusesEntryReplacedByOutOfRootSymlinkAfterPlanning(t *testing.T) {
	skipOnWindows(t)
	outDir, outFile := outsideFixture(t)

	t.Run("file swapped for a symlink to an outside file", func(t *testing.T) {
		root := realTempDir(t)
		writeFileMode(t, filepath.Join(root, "a.txt"), []byte("A"), 0o600)
		writeFileMode(t, filepath.Join(root, "z.txt"), []byte("Z"), 0o600)
		items, err := planSubtrees(root, func(string) bool { return true }, vaultItemLeftover)
		if err != nil {
			t.Fatal(err)
		}
		plan := &vaultDeletePlan{root: root, keep: newVaultKeep(ProfileManifestFile, ProfileVaultFile), items: items, coversRoot: true}
		if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outFile, filepath.Join(root, "a.txt")); err != nil {
			t.Fatal(err)
		}
		r := requireDeleteRefused(t, plan.execute())
		if !strings.Contains(r.Reason, "changed since planned") {
			t.Fatalf("reason: %v", r)
		}
		if _, err := os.Lstat(filepath.Join(root, "z.txt")); err != nil {
			t.Fatalf("z.txt must survive a refused plan: %v", err)
		}
		requireOutsideIntact(t, outDir)
	})

	t.Run("directory swapped for a symlink to an outside directory", func(t *testing.T) {
		root := realTempDir(t)
		writeFileMode(t, filepath.Join(root, "sub", "inner", "deep.txt"), []byte("mine"), 0o600)
		items, err := planSubtrees(root, func(string) bool { return true }, vaultItemLeftover)
		if err != nil {
			t.Fatal(err)
		}
		plan := &vaultDeletePlan{root: root, keep: newVaultKeep(ProfileManifestFile, ProfileVaultFile), items: items, coversRoot: true}
		// 真目录挪走，原位放一个指向"外部目录"的 symlink：外部目录里恰好有同名的 inner/deep.txt。
		if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(realTempDir(t), "moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outDir, filepath.Join(root, "sub")); err != nil {
			t.Fatal(err)
		}
		requireDeleteRefused(t, plan.execute())
		requireOutsideIntact(t, outDir)
	})

	t.Run("ancestor swapped but the item type is unchanged", func(t *testing.T) {
		// 只有祖先被换成 symlink、叶子路径照样能 Lstat 成"同类型"：必须靠祖先检查拦住。
		root := realTempDir(t)
		writeFileMode(t, filepath.Join(root, "sub", "outside.txt"), []byte("OUTSIDE-FILE"), 0o600)
		items, err := planSubtrees(root, func(string) bool { return true }, vaultItemLeftover)
		if err != nil {
			t.Fatal(err)
		}
		var leaf []vaultItem
		for _, it := range items {
			if it.rel == "sub/outside.txt" {
				// 把规划信息替换成外部文件的 Lstat：SameFile 会通过，只剩祖先检查能拦。
				info, err := os.Lstat(outFile)
				if err != nil {
					t.Fatal(err)
				}
				it.info = info
				leaf = append(leaf, it)
			}
		}
		if len(leaf) != 1 {
			t.Fatalf("fixture: %d leaf items", len(leaf))
		}
		if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(realTempDir(t), "moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outDir, filepath.Join(root, "sub")); err != nil {
			t.Fatal(err)
		}
		plan := &vaultDeletePlan{root: root, keep: newVaultKeep(ProfileManifestFile, ProfileVaultFile), items: leaf}
		r := requireDeleteRefused(t, plan.execute())
		if !strings.Contains(r.Reason, "symlink") {
			t.Fatalf("want the ancestor-symlink reason, got %v", r)
		}
		requireOutsideIntact(t, outDir)
	})
}

func TestDeletePlanRefusesInconsistentKindsAndSealListMismatch(t *testing.T) {
	skipOnWindows(t)
	root := realTempDir(t)
	writeFileMode(t, filepath.Join(root, "Default", "Preferences"), []byte("12345"), 0o600)
	writeFileMode(t, filepath.Join(root, "Default", "Cache", "f"), []byte("c"), 0o600)
	items, err := planSubtrees(root, func(string) bool { return true }, vaultItemLeftover)
	if err != nil {
		t.Fatal(err)
	}
	byRel := map[string]vaultItem{}
	for _, it := range items {
		byRel[it.rel] = it
	}
	pref, cacheFile, cacheDir, defaultDir := byRel["Default/Preferences"], byRel["Default/Cache/f"], byRel["Default/Cache"], byRel["Default"]
	keep := newVaultKeep(ProfileManifestFile, ProfileVaultFile)

	check := func(name string, item vaultItem, archived map[string]int64, wantReason string) {
		t.Run(name, func(t *testing.T) {
			plan := &vaultDeletePlan{root: root, keep: keep, items: []vaultItem{item}}
			r := requireDeleteRefused(t, plan.validate(archived))
			if !strings.Contains(r.Reason, wantReason) {
				t.Fatalf("reason %q does not mention %q", r.Reason, wantReason)
			}
			requireDeleteRefused(t, plan.execute())
			if _, err := os.Lstat(filepath.Join(root, "Default", "Preferences")); err != nil {
				t.Fatalf("file deleted by a refused plan: %v", err)
			}
		})
	}

	// 缓存项必须命中允许列表。
	notCache := cacheFile
	notCache.kind, notCache.cacheRoot = vaultItemCache, "Default" // "Default" 不是缓存目录名
	check("cache root not on allow list", notCache, nil, "allow list")
	outsideCacheRoot := cacheFile
	outsideCacheRoot.kind, outsideCacheRoot.cacheRoot = vaultItemCache, "Default/Cache/other"
	check("cache item not under its cache root", outsideCacheRoot, nil, "allow list")
	prefAsCache := pref
	prefAsCache.kind, prefAsCache.cacheRoot = vaultItemCache, ""
	check("plaintext passed off as cache", prefAsCache, nil, "allow list")
	dirAsCache := defaultDir
	dirAsCache.kind, dirAsCache.cacheRoot = vaultItemCache, "Default"
	check("whole Default dir passed off as cache", dirAsCache, nil, "allow list")

	// 封存类条目必须与已验证的 vault 逐项一致。
	arch := pref
	arch.kind = vaultItemArchive
	check("archive item without a verified listing", arch, nil, "no verified vault listing")
	check("archive item missing from vault", arch, map[string]int64{"Default/other": 5}, "not in the verified vault")
	check("archive item size differs", arch, map[string]int64{"Default/Preferences": 999}, "size differs")
	archDir := cacheDir
	archDir.kind = vaultItemArchive
	check("directory recorded as a file", archDir, map[string]int64{"Default/Cache": 7}, "different type")

	t.Run("vault holds entries the deletion list does not know", func(t *testing.T) {
		plan := &vaultDeletePlan{root: root, keep: keep, items: []vaultItem{arch}}
		r := requireDeleteRefused(t, plan.validate(map[string]int64{"Default/Preferences": 5, "Default/extra": 1}))
		if !strings.Contains(r.Reason, "vault holds") {
			t.Fatalf("reason: %v", r)
		}
	})

	// 其他类别的名单校验。
	rt := pref
	rt.kind = vaultItemRuntime
	check("not a runtime artifact", rt, nil, "runtime artifact")
	sp := pref
	sp.kind = vaultItemSpecial
	check("regular file passed off as special", sp, nil, "special file")
	tmp := pref
	tmp.kind = vaultItemTemp
	check("not an unseal temp entry", tmp, nil, "unseal temp")
	unknown := pref
	unknown.kind = 0
	check("unknown kind", unknown, nil, "unknown entry kind")

	t.Run("duplicate entries", func(t *testing.T) {
		plan := &vaultDeletePlan{root: root, keep: keep, items: []vaultItem{pref, pref}}
		requireDeleteRefused(t, plan.validate(nil))
	})
}

// ------------------------------------------------------------
// SealProfile：在提交之前整体拒绝
// ------------------------------------------------------------

// sealWithHook 在"封存已写完、校验已开始之前"执行 hook，然后封存，并断言：被 ErrVaultDeleteRefused 拒绝，
// 清单仍为 open、没有 vault/临时件，且 hook 之外的明文一个字节没少。
func sealRefusedAfter(t *testing.T, dir string, ks KeyStore, hook func()) error {
	t.Helper()
	vaultSealHookAfterWrite = func(string, []byte, string) { hook() }
	defer func() { vaultSealHookAfterWrite = nil }()
	_, err := SealProfile(dir, ks)
	requireDeleteRefused(t, err)
	if !strings.Contains(err.Error(), "nothing deleted") {
		t.Fatalf("error should state that nothing was deleted: %v", err)
	}
	requireManifestVault(t, dir, ProfileVaultOpen)
	for _, n := range dirNames(t, dir) {
		if n == ProfileVaultFile || strings.Contains(n, ".tmp-") {
			t.Fatalf("refused seal left %s behind", n)
		}
	}
	if _, lerr := os.Lstat(filepath.Join(dir, "SingletonLock")); lerr != nil && !os.IsNotExist(lerr) {
		t.Fatal(lerr)
	}
	return err
}

func addCacheTo(t *testing.T, dir string) {
	t.Helper()
	writeFileMode(t, filepath.Join(dir, "Default", "Cache", "Cache_Data", "f_000001"), []byte("CACHE-1"), 0o600)
	writeFileMode(t, filepath.Join(dir, "Default", "Cache", "index"), []byte("CACHE-2"), 0o600)
}

func TestSealRefusesWhenCacheDirIsSwappedForSymlinkAfterPlanning(t *testing.T) {
	dir, _ := newTestProfile(t)
	addCacheTo(t, dir)
	outDir, _ := outsideFixture(t)
	writeFileMode(t, filepath.Join(outDir, "Cache_Data", "f_000001"), []byte("OUTSIDE-CACHE"), 0o600)
	before := filterTree(snapshotTree(t, dir), filepath.Join("Default", "Cache"))

	sealRefusedAfter(t, dir, testKeyStore(t), func() {
		if err := os.Rename(filepath.Join(dir, "Default", "Cache"), filepath.Join(realTempDir(t), "moved-cache")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outDir, filepath.Join(dir, "Default", "Cache")); err != nil {
			t.Fatal(err)
		}
	})

	requireOutsideIntact(t, outDir)
	if got, err := os.ReadFile(filepath.Join(outDir, "Cache_Data", "f_000001")); err != nil || string(got) != "OUTSIDE-CACHE" {
		t.Fatalf("outside cache file touched: %q %v", got, err)
	}
	diffTrees(t, before, filterTree(snapshotTree(t, dir), filepath.Join("Default", "Cache")))
}

func TestSealRefusesWhenArchivedFileChangesAfterPlanning(t *testing.T) {
	for name, mutate := range map[string]func(t *testing.T, path string){
		"appended": func(t *testing.T, path string) {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString("-written-after-the-vault-was-made"); err != nil {
				t.Fatal(err)
			}
		},
		"same size, newer mtime": func(t *testing.T, path string) {
			if err := os.Chtimes(path, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := newTestProfile(t)
			target := filepath.Join(dir, "Default", "Preferences")
			sealRefusedAfter(t, dir, testKeyStore(t), func() { mutate(t, target) })
			// 被改动的文件还在，且没有被 vault 里的旧版本替换。
			if _, err := os.Stat(target); err != nil {
				t.Fatalf("modified file was deleted: %v", err)
			}
		})
	}
}

func TestSealRefusesUnplannedEntriesThatAppearAfterPlanning(t *testing.T) {
	for name, rel := range map[string]string{
		"new top-level file": "late-top.txt",
		"new nested file":    filepath.Join("Default", "Local Storage", "late-nested.txt"),
		"new file in cache":  filepath.Join("Default", "Cache", "late-cache.bin"),
	} {
		t.Run(name, func(t *testing.T) {
			dir, _ := newTestProfile(t)
			addCacheTo(t, dir)
			late := filepath.Join(dir, rel)
			sealRefusedAfter(t, dir, testKeyStore(t), func() { writeFileMode(t, late, []byte("LATE"), 0o600) })
			if got, err := os.ReadFile(late); err != nil || string(got) != "LATE" {
				t.Fatalf("unplanned file was deleted: %q %v", got, err)
			}
			// 清掉计划外文件后，同一个 profile 可以正常封存——说明被拒的只是那次，不是 profile 坏了。
			if err := os.Remove(late); err != nil {
				t.Fatal(err)
			}
			vaultSealHookAfterWrite = nil
			mustSeal(t, dir, testKeyStore(t))
			requireOnlyManifestAndVault(t, dir)
		})
	}
}

// ------------------------------------------------------------
// symlink 只删链接本身
// ------------------------------------------------------------

func TestSealDeletesSymlinksInsideCacheAndRuntimeWithoutFollowing(t *testing.T) {
	dir, _ := newTestProfile(t)
	outDir, outFile := outsideFixture(t)
	ks := testKeyStore(t)

	addCacheTo(t, dir)
	for link, target := range map[string]string{
		filepath.Join("Default", "Cache", "Cache_Data", "escape-dir"): outDir,
		filepath.Join("Default", "Cache", "escape-file"):              outFile,
		filepath.Join("Default", "Code Cache", "escape-dir"):          outDir,
		"SingletonCookie":      outDir,
		"RunningChromeVersion": outFile,
		filepath.Join("ShaderCache", "nested", "escape-dangling"): filepath.Join(outDir, "does-not-exist"),
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, link)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	want := snapshotTree(t, dir)
	for k := range want {
		if strings.Contains(k, "Cache") || strings.Contains(k, "ShaderCache") || strings.HasPrefix(k, "Singleton") || k == "RunningChromeVersion" {
			delete(want, k)
		}
	}

	rc := mustSeal(t, dir, ks)
	if rc.CachesPurged != 3 { // Default/Cache、Default/Code Cache、ShaderCache
		t.Fatalf("caches purged = %d, want 3", rc.CachesPurged)
	}
	requireOnlyManifestAndVault(t, dir)
	requireOutsideIntact(t, outDir)

	mustUnseal(t, dir, ks)
	diffTrees(t, want, snapshotTree(t, dir))
	requireOutsideIntact(t, outDir)
}

func TestUnsealClearsLeftoverSymlinksWithoutFollowing(t *testing.T) {
	dir, _ := newTestProfile(t)
	outDir, outFile := outsideFixture(t)
	ks := testKeyStore(t)
	want := snapshotTree(t, dir)
	mustSeal(t, dir, ks)

	// 封存后删除到一半崩溃的残留里混着指向外部的 symlink。
	for link, target := range map[string]string{
		"evil-dir":                           outDir,
		"evil-file":                          outFile,
		filepath.Join("left-dir", "inner"):   outDir,
		filepath.Join("left-dir", "inner-f"): outFile,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, link)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
	}
	writeFileMode(t, filepath.Join(dir, "left-dir", "plain.txt"), []byte("x"), 0o600)

	mustUnseal(t, dir, ks)
	diffTrees(t, want, snapshotTree(t, dir))
	requireOutsideIntact(t, outDir)
}

func TestRemoveVaultTreeNeverFollowsSymlinksOrTouchesNeighbours(t *testing.T) {
	skipOnWindows(t)
	outDir, outFile := outsideFixture(t)
	root := realTempDir(t)
	tmp := vaultUnsealTmpPrefix + "unit"
	writeFileMode(t, filepath.Join(root, tmp, "a", "b", "c.txt"), []byte("c"), 0o600)
	writeFileMode(t, filepath.Join(root, tmp, "top.txt"), []byte("t"), 0o600)
	if err := os.Symlink(outDir, filepath.Join(root, tmp, "a", "link-dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outFile, filepath.Join(root, tmp, "link-file")); err != nil {
		t.Fatal(err)
	}
	// 邻居：同级的其他条目、清单、封存文件都不属于这次清单。
	writeFileMode(t, filepath.Join(root, "neighbour", "keep.txt"), []byte("n"), 0o600)
	writeFileMode(t, filepath.Join(root, ProfileManifestFile), []byte("{}"), 0o600)
	writeFileMode(t, filepath.Join(root, ProfileVaultFile), []byte("v"), 0o600)

	err := removeVaultTree(root, newVaultKeep(ProfileManifestFile, ProfileVaultFile),
		func(top string) bool { return top == tmp }, vaultItemTemp, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, tmp)); !os.IsNotExist(err) {
		t.Fatalf("temp tree should be gone: %v", err)
	}
	for _, p := range []string{"neighbour/keep.txt", ProfileManifestFile, ProfileVaultFile} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
			t.Fatalf("%s must not be touched: %v", p, err)
		}
	}
	requireOutsideIntact(t, outDir)

	// 清单之外长出来的东西 → 目录非空 → 拒绝而不是递归带走。
	writeFileMode(t, filepath.Join(root, tmp, "x", "one.txt"), []byte("1"), 0o600)
	items, err := planSubtrees(root, func(top string) bool { return top == tmp }, vaultItemTemp)
	if err != nil {
		t.Fatal(err)
	}
	plan := &vaultDeletePlan{root: root, keep: newVaultKeep(ProfileManifestFile, ProfileVaultFile), items: items}
	writeFileMode(t, filepath.Join(root, tmp, "x", "two-unplanned.txt"), []byte("2"), 0o600)
	requireDeleteRefused(t, plan.execute())
	if got, err := os.ReadFile(filepath.Join(root, tmp, "x", "two-unplanned.txt")); err != nil || string(got) != "2" {
		t.Fatalf("unplanned file was removed: %v", err)
	}
}

func TestRemoveFileInRoot(t *testing.T) {
	skipOnWindows(t)
	outDir, outFile := outsideFixture(t)
	root := realTempDir(t)

	t.Run("regular file and idempotence", func(t *testing.T) {
		p := filepath.Join(root, "f.txt")
		writeFileMode(t, p, []byte("x"), 0o600)
		if err := removeFileInRoot(root, p); err != nil {
			t.Fatal(err)
		}
		if err := removeFileInRoot(root, p); err != nil {
			t.Fatalf("missing file must be a no-op: %v", err)
		}
	})
	t.Run("symlink: only the link goes", func(t *testing.T) {
		p := filepath.Join(root, "link")
		if err := os.Symlink(outFile, p); err != nil {
			t.Fatal(err)
		}
		if err := removeFileInRoot(root, p); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("link still there: %v", err)
		}
		requireOutsideIntact(t, outDir)
	})
	t.Run("directory refused", func(t *testing.T) {
		p := filepath.Join(root, "adir")
		writeFileMode(t, filepath.Join(p, "inside.txt"), []byte("x"), 0o600)
		requireDeleteRefused(t, removeFileInRoot(root, p))
		if _, err := os.Stat(filepath.Join(p, "inside.txt")); err != nil {
			t.Fatalf("directory contents touched: %v", err)
		}
	})
	t.Run("symlink to a directory is removed as a link, not as a directory", func(t *testing.T) {
		p := filepath.Join(root, "dirlink")
		if err := os.Symlink(outDir, p); err != nil {
			t.Fatal(err)
		}
		if err := removeFileInRoot(root, p); err != nil {
			t.Fatal(err)
		}
		requireOutsideIntact(t, outDir)
	})
	t.Run("outside root refused", func(t *testing.T) {
		requireDeleteRefused(t, removeFileInRoot(root, outFile))
		requireDeleteRefused(t, removeFileInRoot(root, filepath.Join(root, "..", "x")))
		requireDeleteRefused(t, removeFileInRoot(root, root))
		requireDeleteRefused(t, removeFileInRoot(root, filepath.Dir(root)))
		requireOutsideIntact(t, outDir)
	})
	t.Run("ancestor symlink refused", func(t *testing.T) {
		if err := os.Symlink(outDir, filepath.Join(root, "hop")); err != nil {
			t.Fatal(err)
		}
		requireDeleteRefused(t, removeFileInRoot(root, filepath.Join(root, "hop", "outside.txt")))
		requireOutsideIntact(t, outDir)
	})
}

// ------------------------------------------------------------
// 结构性守卫：所有删除都收口在 profile_vault_delete.go
// ------------------------------------------------------------

var codeLineRE = regexp.MustCompile(`(?m)^[^/\n]*?(os\.RemoveAll|os\.Remove|RemoveAll)\(`)

// codeOnly 去掉整行注释，避免文档里提到函数名触发误报。
func codeOnly(src string) string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestDeletionsAreFunnelledThroughOneFile(t *testing.T) {
	for _, name := range []string{
		"profile_manifest.go", "profile_vault.go", "profile_vault_keystore.go",
		"profile_vault_keystore_darwin.go", "profile_vault_keystore_other.go",
		"profile_vault_entries.go", "profile_vault_owner_unix.go", "profile_vault_owner_windows.go",
	} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if m := codeLineRE.FindString(codeOnly(string(src))); m != "" {
			t.Fatalf("%s deletes directly (%q); route it through profile_vault_delete.go", name, strings.TrimSpace(m))
		}
	}
	src, err := os.ReadFile("profile_vault_delete.go")
	if err != nil {
		t.Fatal(err)
	}
	code := codeOnly(string(src))
	if strings.Contains(code, "RemoveAll") {
		t.Fatal("profile_vault_delete.go must never call a recursive remove")
	}
	if n := strings.Count(code, "os.Remove("); n != 2 {
		t.Fatalf("profile_vault_delete.go should have exactly 2 os.Remove call sites (execute, removeFileInRoot), has %d", n)
	}
}
