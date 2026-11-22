package browser

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// useTestKeyFile 让本测试的封存/解封走文件密钥库（回执的 protection 为 file）：绝不触碰登录钥匙串。
func useTestKeyFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys", "master.key")
	t.Setenv(EnvProfileKeyFile, path)
	return path
}

func mockArgs() []string { return []string{"--use-mock-keychain", "--password-store=basic"} }

func TestLaunchProfileKeyStoreHonoursEnvOverride(t *testing.T) {
	path := useTestKeyFile(t)
	ks := LaunchProfileKeyStore()
	if ks.Protection() != KeyProtectionFile {
		t.Fatalf("protection %q, want file", ks.Protection())
	}
	if _, err := LoadOrCreateMasterKey(ks); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the env-selected key file was not the one used: %v", err)
	}
}

func TestPrepareNewPersistentProfileIsMockAndVaulted(t *testing.T) {
	useTestKeyFile(t)
	dir := filepath.Join(t.TempDir(), "p")
	l, err := PrepareProfileForLaunch(dir, "headed", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l.Manifest.OSCrypt != ProfileOSCryptMock || l.Manifest.Vault != ProfileVaultOpen {
		t.Fatalf("manifest %+v", l.Manifest)
	}
	if !reflect.DeepEqual(l.CryptoArgs(), mockArgs()) {
		t.Fatalf("crypto args %v", l.CryptoArgs())
	}
	r := l.Receipt
	if !r.Managed || !r.Persistent || r.OSCrypt != "mock" || r.Vault != "open" || r.Protection != KeyProtectionFile || r.ClassifiedBy != ClassifyBasisNewProfile {
		t.Fatalf("receipt %+v", r)
	}
	// 回执要能直接进 JSON：字段名是对外契约。
	b, _ := json.Marshal(r)
	for _, want := range []string{`"os_crypt":"mock"`, `"vault":"open"`, `"protection":"file"`, `"persistent":true`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("receipt JSON lacks %s: %s", want, b)
		}
	}
}

func TestPrepareNewTemporaryProfileIsMockAndNotVaulted(t *testing.T) {
	useTestKeyFile(t)
	dir := filepath.Join(t.TempDir(), "p")
	l, err := PrepareProfileForLaunch(dir, "headless", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l.Manifest.OSCrypt != ProfileOSCryptMock || l.Manifest.Vault != ProfileVaultNone {
		t.Fatalf("a temporary profile is mock and never sealed: %+v", l.Manifest)
	}
	if !reflect.DeepEqual(l.CryptoArgs(), mockArgs()) {
		t.Fatalf("crypto args %v", l.CryptoArgs())
	}
	if l.Receipt.Protection != "" || l.Receipt.Persistent {
		t.Fatalf("receipt %+v", l.Receipt)
	}
	// 磁盘上的清单也是 none（不是只改了回执）。
	requireManifestVault(t, dir, ProfileVaultNone)
}

// 启动参数只看清单：同一个 profile 在任何启动模式下参数一致（E5 的根因是它随模式漂移）。
func TestCryptoArgsDoNotDependOnLaunchMode(t *testing.T) {
	useTestKeyFile(t)
	mock, err := NewProfileManifest(ProfileOSCryptMock, ProfileVaultOpen, "x")
	if err != nil {
		t.Fatal(err)
	}
	system, err := NewProfileManifest(ProfileOSCryptSystem, ProfileVaultNone, "x")
	if err != nil {
		t.Fatal(err)
	}
	has := func(args []string, flag string) bool {
		for _, a := range args {
			if a == flag {
				return true
			}
		}
		return false
	}
	for _, mode := range []BrowserMode{ModeHeadless, ModeHeaded, ModeVisible} {
		for _, tc := range []struct {
			name string
			m    *ProfileManifest
			want bool
		}{{"mock", mock, true}, {"system", system, false}} {
			args := BuildDetachedChromeArgs(DetachedChromeLaunchOptions{DebugPort: 1, ProfileDir: "/p", Mode: mode, UserAgent: "test-ua", ProfileCrypto: tc.m})
			if got := has(args, "--use-mock-keychain") && has(args, "--password-store=basic"); got != tc.want {
				t.Errorf("os_crypt=%s mode=%s: mock keychain flags present=%v, want %v (%v)", tc.name, mode, got, tc.want, args)
			}
			if !tc.want && (has(args, "--use-mock-keychain") || has(args, "--password-store=basic")) {
				t.Errorf("os_crypt=%s mode=%s: a system profile must never get mock flags: %v", tc.name, mode, args)
			}
		}
	}
	// 没有清单（BrowserPool 等非 CLI profile）保持旧行为：只有 headless 用模拟钥匙串。
	if !has(BuildDetachedChromeArgs(DetachedChromeLaunchOptions{DebugPort: 1, ProfileDir: "/p", Mode: ModeHeadless, UserAgent: "test-ua"}), "--use-mock-keychain") {
		t.Error("legacy headless must keep the mock keychain")
	}
	if has(BuildDetachedChromeArgs(DetachedChromeLaunchOptions{DebugPort: 1, ProfileDir: "/p", Mode: ModeHeaded, UserAgent: "test-ua"}), "--use-mock-keychain") {
		t.Error("legacy headed must stay without the mock keychain")
	}
}

func TestPrepareExistingSystemProfileKeepsLoginState(t *testing.T) {
	useTestKeyFile(t)
	dir := existingChromeProfile(t)
	writeCookiesDB(t, filepath.Join(dir, "Default", "Cookies"), []testCookie{
		{".example.com", mockEncryptCookie(t, ".example.com", "secret", "some-real-keychain-password", 1003)},
	})
	l, err := PrepareProfileForLaunch(dir, "headed", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l.Manifest.OSCrypt != ProfileOSCryptSystem || l.Manifest.Vault != ProfileVaultNone {
		t.Fatalf("manifest %+v", l.Manifest)
	}
	if len(l.CryptoArgs()) != 0 {
		t.Fatalf("a system profile gets no mock flags in any mode, got %v", l.CryptoArgs())
	}
	if l.Receipt.Protection != "" || l.Receipt.ClassifiedBy != ClassifyBasisSystemCookies || !strings.Contains(l.Receipt.Note, "system keychain") {
		t.Fatalf("receipt %+v", l.Receipt)
	}
}

func TestPreparePersistentUpgradesTemporaryMockProfile(t *testing.T) {
	useTestKeyFile(t)
	dir := filepath.Join(t.TempDir(), "p")
	if _, err := PrepareProfileForLaunch(dir, "headless", false, nil); err != nil {
		t.Fatal(err)
	}
	l, err := PrepareProfileForLaunch(dir, "headless", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l.Manifest.Vault != ProfileVaultOpen || !strings.Contains(l.Receipt.Note, "persistent now") {
		t.Fatalf("manifest %+v receipt %+v", l.Manifest, l.Receipt)
	}
	requireManifestVault(t, dir, ProfileVaultOpen)
}

func TestPrepareUnsealsASealedProfileBeforeLaunch(t *testing.T) {
	useTestKeyFile(t)
	dir, _ := newTestProfile(t)
	want := snapshotTree(t, dir)
	rc := SealPersistentProfile(dir)
	if rc.Error != "" || rc.Vault != ProfileVaultSealed {
		t.Fatalf("seal: %+v", rc)
	}
	requireOnlyManifestAndVault(t, dir)

	recovered := false
	l, err := PrepareProfileForLaunch(dir, "headed", true, func() error {
		// 启动恢复看到的必须已经是明文（它可能据此隔离损坏的 Cookie 库）。
		if _, err := os.Stat(filepath.Join(dir, "Default", "Cookies")); err != nil {
			t.Errorf("startup recovery ran before the profile was unsealed: %v", err)
		}
		recovered = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !recovered || l.Receipt.Unseal == nil || l.Receipt.Unseal.Action != "unseal" || l.Receipt.Vault != ProfileVaultOpen {
		t.Fatalf("recovered=%v receipt %+v", recovered, l.Receipt)
	}
	got := snapshotTree(t, dir)
	delete(got, ProfileManifestFile)
	delete(want, ProfileManifestFile)
	diffTrees(t, want, got)
}

// 封存着却解不开：绝不在一个封存的目录上启动出一份空 profile。
func TestPrepareRefusesToLaunchWhenUnsealFails(t *testing.T) {
	keyPath := useTestKeyFile(t)
	dir, _ := newTestProfile(t)
	if rc := SealPersistentProfile(dir); rc.Error != "" {
		t.Fatalf("seal: %+v", rc)
	}
	if err := os.Remove(keyPath); err != nil { // 主密钥丢了
		t.Fatal(err)
	}
	l, err := PrepareProfileForLaunch(dir, "headed", true, func() error { t.Error("recovery must not run on a profile that could not be unsealed"); return nil })
	if err == nil || !errors.Is(err, ErrVaultKeyUnavailable) {
		t.Fatalf("want ErrVaultKeyUnavailable, got %v", err)
	}
	if l == nil || l.Receipt.ErrorKind != ProfileErrKeyUnavailable {
		t.Fatalf("receipt %+v", l)
	}
	requireOnlyManifestAndVault(t, dir) // 目录原样，没有生出空 profile
}

func TestSealPersistentProfileSealsAndReports(t *testing.T) {
	useTestKeyFile(t)
	dir, _ := newTestProfile(t)
	rc := SealPersistentProfile(dir)
	if rc.Error != "" || rc.Vault != ProfileVaultSealed || rc.Seal == nil || !rc.Seal.Verified || rc.Seal.Action != "seal" || rc.Protection != KeyProtectionFile {
		t.Fatalf("receipt %+v", rc)
	}
	requireOnlyManifestAndVault(t, dir)
	// 再封一次是 noop，不是错误。
	if again := SealPersistentProfile(dir); again.Error != "" || again.Vault != ProfileVaultSealed || again.Seal.Action != "noop" {
		t.Fatalf("second seal %+v", again)
	}
}

// D5：遇到普通 symlink 就中止封存，明文保留；回执如实说"没封存"，并给出稳定的 error_kind。
func TestSealPersistentProfileReportsUnsupportedSymlinkAndKeepsPlaintext(t *testing.T) {
	skipOnWindows(t)
	useTestKeyFile(t)
	dir, _ := newTestProfile(t)
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "Default", "sneaky-link")); err != nil {
		t.Fatal(err)
	}
	want := snapshotTree(t, dir)
	rc := SealPersistentProfile(dir)
	if rc.ErrorKind != ProfileErrUnsupportedEntry || rc.Error == "" {
		t.Fatalf("want unsupported_entry, got %+v", rc)
	}
	if rc.Vault != ProfileVaultOpen || !strings.Contains(rc.Note, "NOT sealed") {
		t.Fatalf("the receipt must say the profile is still plaintext: %+v", rc)
	}
	diffTrees(t, want, snapshotTree(t, dir))
	if names := dirNames(t, dir); contains(names, ProfileVaultFile) {
		t.Fatalf("an aborted seal must not leave a vault behind: %v", names)
	}
}

// D5：ErrVaultDeleteRefused 视为"未封存、明文完好"，不能吞。
func TestSealPersistentProfileReportsDeleteRefusedAsNotSealed(t *testing.T) {
	skipOnWindows(t)
	useTestKeyFile(t)
	dir, _ := newTestProfile(t)
	// 自校验之后、提交之前让一个文件"自规划后被改动"：删除清单校验必须拒绝。
	vaultSealHookAfterWrite = func(string, []byte, string) {
		_ = os.WriteFile(filepath.Join(dir, "Default", "Preferences"), []byte(`{"changed":"after planning, longer than before"}`), 0o644)
	}
	defer func() { vaultSealHookAfterWrite = nil }()
	rc := SealPersistentProfile(dir)
	if rc.ErrorKind != ProfileErrDeleteRefused || !strings.Contains(rc.Error, "nothing deleted") || rc.Vault != ProfileVaultOpen {
		t.Fatalf("a refused deletion list means not sealed, plaintext intact, and the receipt says so: %+v", rc)
	}
	if _, err := os.Stat(filepath.Join(dir, "Default", "Cookies")); err != nil {
		t.Fatalf("plaintext must be intact: %v", err)
	}
	if contains(dirNames(t, dir), ProfileVaultFile) {
		t.Fatal("no vault may be left behind by an aborted seal")
	}
}

// 旧版本启动、没有清单的 profile：关闭时凭证据归类再封存。
func TestSealPersistentProfileClassifiesLegacyMockProfile(t *testing.T) {
	useTestKeyFile(t)
	dir := existingChromeProfile(t)
	writeCookiesDB(t, filepath.Join(dir, "Default", "Cookies"), []testCookie{
		{".example.com", mockEncryptCookie(t, ".example.com", "token", "mock_password", 1003)},
	})
	rc := SealPersistentProfile(dir)
	if rc.Error != "" || rc.OSCrypt != ProfileOSCryptMock || rc.Vault != ProfileVaultSealed {
		t.Fatalf("receipt %+v", rc)
	}
	requireOnlyManifestAndVault(t, dir)
}

func TestSealPersistentProfileLeavesSystemProfileAlone(t *testing.T) {
	useTestKeyFile(t)
	dir := existingChromeProfile(t)
	writeCookiesDB(t, filepath.Join(dir, "Default", "Cookies"), []testCookie{
		{".example.com", mockEncryptCookie(t, ".example.com", "secret", "keychain-password", 1003)},
	})
	want := snapshotTree(t, dir)
	rc := SealPersistentProfile(dir)
	if rc.Error != "" || rc.OSCrypt != ProfileOSCryptSystem || rc.Vault != ProfileVaultNone || rc.Seal != nil {
		t.Fatalf("receipt %+v", rc)
	}
	got := snapshotTree(t, dir)
	delete(got, ProfileManifestFile) // 唯一的新增是清单
	diffTrees(t, want, got)
}

func TestDescribeProfileIsReadOnly(t *testing.T) {
	useTestKeyFile(t)
	plain := existingChromeProfile(t)
	if st := DescribeProfile(plain); st.Managed || st.OSCrypt != "unclassified" || st.Error != "" {
		t.Fatalf("unmanaged: %+v", st)
	}
	if _, err := os.Stat(filepath.Join(plain, ProfileManifestFile)); err == nil {
		t.Fatal("listing must not write a manifest")
	}

	dir, _ := newTestProfile(t)
	if st := DescribeProfile(dir); !st.Managed || st.OSCrypt != "mock" || st.Vault != "open" || st.Protection != KeyProtectionFile {
		t.Fatalf("open: %+v", st)
	}
	SealPersistentProfile(dir)
	if st := DescribeProfile(dir); st.Vault != "sealed" {
		t.Fatalf("sealed: %+v", st)
	}
	if err := os.WriteFile(filepath.Join(dir, ProfileManifestFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := DescribeProfile(dir); st.Error == "" || st.ErrorKind != ProfileErrManifest {
		t.Fatalf("invalid manifest must be reported, not hidden: %+v", st)
	}
}

// 持久/托管策略必须跟着 serve 参数走到 mux host 进程里：host 是另一个进程，请求体之外只有命令行。
func TestMuxHostServeArgsCarryTheProfilePolicy(t *testing.T) {
	has := func(args []string, flag string) bool {
		for _, a := range args {
			if a == flag {
				return true
			}
		}
		return false
	}
	base := BrowserMuxHostRequest{MuxHostID: "h", RuntimeID: "r", BrowserSessionID: "browser-session-x", ProfileDir: "/p"}
	if a := browserMuxHostServeArgs(base); has(a, "--managed-profile") || has(a, "--persistent-profile") {
		t.Fatalf("an unmanaged request must not claim a policy: %v", a)
	}
	managed := base
	managed.ManagedProfile = true
	if a := browserMuxHostServeArgs(managed); !has(a, "--managed-profile") || has(a, "--persistent-profile") {
		t.Fatalf("managed only: %v", a)
	}
	persistent := base
	persistent.PersistentProfile = true
	if a := browserMuxHostServeArgs(persistent); !has(a, "--managed-profile") || !has(a, "--persistent-profile") {
		t.Fatalf("persistent implies managed: %v", a)
	}
}

// 启动租约：解封之后到 Release 之前，别的封存/解封都拿不到锁——否则它看不到任何"在运行"的证据，
// 会把刚解封的 profile 又封回去。Release 之后才能封存。
func TestPrepareHoldsTheLaunchLeaseUntilReleased(t *testing.T) {
	useTestKeyFile(t)
	dir, _ := newTestProfile(t)
	if rc := SealPersistentProfile(dir); rc.Error != "" {
		t.Fatalf("seal: %+v", rc)
	}
	l, err := PrepareProfileForLaunch(dir, "headed", true, nil)
	if err != nil {
		t.Fatal(err)
	}

	old := vaultLockWait
	vaultLockWait = 120 * time.Millisecond
	defer func() { vaultLockWait = old }()

	rc := SealPersistentProfile(dir) // 租约还在：另一个封存必须等不到锁，并且不能动明文
	if rc.ErrorKind != ProfileErrInUse || rc.Vault != ProfileVaultOpen {
		t.Fatalf("a seal during the launch window must be refused as in-use, plaintext untouched: %+v", rc)
	}
	if _, err := os.Stat(filepath.Join(dir, "Default", "Cookies")); err != nil {
		t.Fatalf("plaintext must be intact: %v", err)
	}

	l.Release()
	l.Release() // 幂等
	if rc := SealPersistentProfile(dir); rc.Error != "" || rc.Vault != ProfileVaultSealed {
		t.Fatalf("after Release the profile seals normally: %+v", rc)
	}
	(*ProfileLaunch)(nil).Release() // nil 安全
}

// 解封失败：租约已经释放，不会把锁留在一个启动不了的目录上。
func TestPrepareFailureDoesNotLeakTheLease(t *testing.T) {
	keyPath := useTestKeyFile(t)
	dir, _ := newTestProfile(t)
	if rc := SealPersistentProfile(dir); rc.Error != "" {
		t.Fatalf("seal: %+v", rc)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareProfileForLaunch(dir, "headed", true, nil); err == nil {
		t.Fatal("unseal without a key must fail")
	}
	old := vaultLockWait
	vaultLockWait = 120 * time.Millisecond
	defer func() { vaultLockWait = old }()
	if rc := SealPersistentProfile(dir); rc.ErrorKind == ProfileErrInUse {
		t.Fatalf("the failed launch kept the vault lock: %+v", rc)
	}
}

// mux host 启动在 Chrome 起来之前失败：已经解封成明文的持久 profile 必须先放租约、再封回去。
func TestMuxHostFailedStartReleasesTheLeaseAndReseals(t *testing.T) {
	useTestKeyFile(t)
	t.Setenv("TMPDIR", t.TempDir())
	dir, _ := newTestProfile(t)
	if rc := SealPersistentProfile(dir); rc.Error != "" {
		t.Fatalf("seal: %+v", rc)
	}
	srv := &browserMuxHostServer{hostID: "h", token: "t", runtimes: map[string]*browserMuxHostRuntime{}, shutdownCh: make(chan struct{})}
	req := BrowserMuxHostRequest{
		MuxHostID: "h", RuntimeID: "rt", BrowserSessionID: "browser-session-x", ProfileID: "p", ProfileDir: dir,
		ChromePath: filepath.Join(t.TempDir(), "no-such-chrome"), Mode: ModeHeadless, PersistentProfile: true,
	}
	if _, err := srv.startRuntime(context.Background(), normalizeBrowserMuxHostRequest(req)); err == nil {
		t.Fatal("launching a missing Chrome must fail")
	}
	requireOnlyManifestAndVault(t, dir) // 封回去了（而且没有死等自己持有的锁）
	requireManifestVault(t, dir, ProfileVaultSealed)
}
