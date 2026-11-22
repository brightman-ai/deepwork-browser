package browser

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1" //nolint:gosec
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// mockEncryptCookie 按 Chromium 的 v10 格式造一条加密 Cookie：
// "v10" ‖ AES-128-CBC(IV=16 个空格, PKCS7, SHA256(host_key) ‖ value)。
func mockEncryptCookie(t *testing.T, host, value, password string, iter int) []byte {
	t.Helper()
	key, err := pbkdf2.Key(sha1.New, password, []byte("saltysalt"), iter, 16)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(host))
	plain := append(append([]byte{}, sum[:]...), value...)
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(plain, bytes.Repeat([]byte{byte(pad)}, pad)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	ct := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, []byte("                ")).CryptBlocks(ct, plain)
	return append([]byte("v10"), ct...)
}

type testCookie struct {
	host string
	enc  []byte
}

func writeCookiesDB(t *testing.T, path string, cookies []testCookie) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE cookies (
		creation_utc INTEGER NOT NULL, host_key TEXT NOT NULL, name TEXT NOT NULL,
		value TEXT NOT NULL, encrypted_value BLOB NOT NULL, path TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i, c := range cookies {
		if _, err := db.Exec(`INSERT INTO cookies VALUES (?, ?, ?, '', ?, '/')`, i+1, c.host, "n", c.enc); err != nil {
			t.Fatal(err)
		}
	}
}

// existingChromeProfile 造一个"已有 Chrome 状态、没有清单"的 profile 目录。
func existingChromeProfile(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Default", "Preferences"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestProfileManifestRoundTripAndValidation(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadProfileManifest(dir); !errors.Is(err, ErrProfileManifestNotFound) {
		t.Fatalf("missing manifest: want ErrProfileManifestNotFound, got %v", err)
	}

	m, err := NewProfileManifest(ProfileOSCryptMock, ProfileVaultOpen, "headed")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.ProfileID) != 32 {
		t.Fatalf("profile id %q", m.ProfileID)
	}
	if err := WriteProfileManifest(dir, m); err != nil {
		t.Fatal(err)
	}
	got, err := ReadProfileManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, m)
	}
	fi, err := os.Stat(filepath.Join(dir, ProfileManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("manifest perm %v", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("atomic write left extra files: %v", entries)
	}
	if !reflect.DeepEqual(got.CryptoLaunchArgs(), []string{"--use-mock-keychain", "--password-store=basic"}) {
		t.Fatalf("mock launch args: %v", got.CryptoLaunchArgs())
	}

	sys, err := NewProfileManifest(ProfileOSCryptSystem, ProfileVaultNone, "visible")
	if err != nil {
		t.Fatal(err)
	}
	if args := sys.CryptoLaunchArgs(); len(args) != 0 {
		t.Fatalf("system profile must not add crypto flags: %v", args)
	}

	// 非法组合与未知值
	if _, err := NewProfileManifest(ProfileOSCryptSystem, ProfileVaultOpen, "x"); !errors.Is(err, ErrProfileManifestInvalid) {
		t.Fatalf("system+open must be invalid: %v", err)
	}
	if _, err := NewProfileManifest("keychain", ProfileVaultNone, "x"); !errors.Is(err, ErrProfileManifestInvalid) {
		t.Fatalf("unknown os_crypt: %v", err)
	}
	for _, body := range []string{
		`{not json`,
		`{"schema":2,"profile_id":"a","os_crypt":"mock","vault":"open"}`,
		`{"schema":1,"profile_id":"","os_crypt":"mock","vault":"open"}`,
		`{"schema":1,"profile_id":"a","os_crypt":"mock","vault":"weird"}`,
	} {
		bad := t.TempDir()
		if err := os.WriteFile(filepath.Join(bad, ProfileManifestFile), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadProfileManifest(bad); !errors.Is(err, ErrProfileManifestInvalid) {
			t.Fatalf("body %q: want ErrProfileManifestInvalid, got %v", body, err)
		}
	}
}

func TestClassifyProfileNewAndEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	for name, dir := range map[string]string{"missing": missing, "empty": t.TempDir()} {
		cls, err := ClassifyProfile(dir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cls.OSCrypt != ProfileOSCryptMock || cls.Vault != ProfileVaultOpen || cls.Basis != ClassifyBasisNewProfile {
			t.Fatalf("%s: %+v", name, cls)
		}
	}
}

func TestClassifyProfileMockDecryptable(t *testing.T) {
	cases := []struct {
		name     string
		password string
		iter     int
	}{
		{"macos mock_password", "mock_password", 1003},
		{"linux basic peanuts", "peanuts", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := existingChromeProfile(t)
			writeCookiesDB(t, filepath.Join(dir, "Default", "Cookies"), []testCookie{
				{".example.com", mockEncryptCookie(t, ".example.com", "token-123", tc.password, tc.iter)},
			})
			cls, err := ClassifyProfile(dir)
			if err != nil {
				t.Fatal(err)
			}
			if cls.OSCrypt != ProfileOSCryptMock || cls.Vault != ProfileVaultOpen || cls.Basis != ClassifyBasisMockDecrypts {
				t.Fatalf("%+v", cls)
			}
		})
	}
}

func TestClassifyProfileMockDecryptableViaNetworkCookiesPath(t *testing.T) {
	dir := existingChromeProfile(t)
	writeCookiesDB(t, filepath.Join(dir, "Default", "Network", "Cookies"), []testCookie{
		{".example.com", mockEncryptCookie(t, ".example.com", "v", "mock_password", 1003)},
	})
	cls, err := ClassifyProfile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cls.OSCrypt != ProfileOSCryptMock || cls.Basis != ClassifyBasisMockDecrypts {
		t.Fatalf("%+v", cls)
	}
}

func TestClassifyProfileCookiesNotDecryptableMeansSystem(t *testing.T) {
	dir := existingChromeProfile(t)
	writeCookiesDB(t, filepath.Join(dir, "Default", "Cookies"), []testCookie{
		// 用钥匙串里的随机口令加密：模拟口令解不开。
		{".example.com", mockEncryptCookie(t, ".example.com", "secret", "some-real-keychain-password", 1003)},
		// v11（Linux keyring）也解不开。
		{".example.org", append([]byte("v11"), bytes.Repeat([]byte{7}, 48)...)},
	})
	cls, err := ClassifyProfile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cls.OSCrypt != ProfileOSCryptSystem || cls.Vault != ProfileVaultNone || cls.Basis != ClassifyBasisSystemCookies || cls.CookiesProbed != 2 {
		t.Fatalf("%+v", cls)
	}
}

func TestClassifyProfileRequiresDomainBinding(t *testing.T) {
	// 口令对、PKCS7 也对，但明文前 32 字节不是 SHA256(host_key)：不能算"解开"。
	dir := existingChromeProfile(t)
	writeCookiesDB(t, filepath.Join(dir, "Default", "Cookies"), []testCookie{
		{".example.com", mockEncryptCookie(t, ".other-host.com", "value", "mock_password", 1003)},
	})
	cls, err := ClassifyProfile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cls.OSCrypt != ProfileOSCryptSystem {
		t.Fatalf("host-binding mismatch must not classify as mock: %+v", cls)
	}
}

func TestClassifyProfileNoCookies(t *testing.T) {
	t.Run("no cookie db", func(t *testing.T) {
		cls, err := ClassifyProfile(existingChromeProfile(t))
		if err != nil {
			t.Fatal(err)
		}
		if cls.OSCrypt != ProfileOSCryptMock || cls.Vault != ProfileVaultOpen || cls.Basis != ClassifyBasisNoCookies {
			t.Fatalf("%+v", cls)
		}
	})
	t.Run("cookie db without rows", func(t *testing.T) {
		dir := existingChromeProfile(t)
		writeCookiesDB(t, filepath.Join(dir, "Default", "Cookies"), nil)
		cls, err := ClassifyProfile(dir)
		if err != nil {
			t.Fatal(err)
		}
		if cls.OSCrypt != ProfileOSCryptMock || cls.Basis != ClassifyBasisNoCookies {
			t.Fatalf("%+v", cls)
		}
	})
	t.Run("zero-byte cookie file", func(t *testing.T) {
		dir := existingChromeProfile(t)
		if err := os.WriteFile(filepath.Join(dir, "Default", "Cookies"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		cls, err := ClassifyProfile(dir)
		if err != nil {
			t.Fatal(err)
		}
		if cls.Basis != ClassifyBasisNoCookies {
			t.Fatalf("%+v", cls)
		}
	})
}

func TestClassifyProfileCorruptCookieDBIsAnErrorNotAGuess(t *testing.T) {
	dir := existingChromeProfile(t)
	if err := os.WriteFile(filepath.Join(dir, "Default", "Cookies"), bytes.Repeat([]byte("not sqlite "), 200), 0o600); err != nil {
		t.Fatal(err)
	}
	if cls, err := ClassifyProfile(dir); err == nil {
		t.Fatalf("corrupt cookie db must error, got %+v", cls)
	}
}

func TestEnsureProfileManifest(t *testing.T) {
	t.Run("classifies once then keeps manifest", func(t *testing.T) {
		dir := existingChromeProfile(t)
		writeCookiesDB(t, filepath.Join(dir, "Default", "Cookies"), []testCookie{
			{".example.com", mockEncryptCookie(t, ".example.com", "x", "real-keychain", 1003)},
		})
		m, cls, err := EnsureProfileManifest(dir, "visible")
		if err != nil {
			t.Fatal(err)
		}
		if cls == nil || m.OSCrypt != ProfileOSCryptSystem || m.Vault != ProfileVaultNone || m.CreatedMode != "visible" {
			t.Fatalf("%+v / %+v", m, cls)
		}
		m2, cls2, err := EnsureProfileManifest(dir, "headless")
		if err != nil {
			t.Fatal(err)
		}
		if cls2 != nil || !reflect.DeepEqual(m, m2) {
			t.Fatalf("second call must return the stored manifest untouched: %+v / %+v", m2, cls2)
		}
	})
	t.Run("new profile directory is created", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "fresh", "profile")
		m, cls, err := EnsureProfileManifest(dir, "headed")
		if err != nil {
			t.Fatal(err)
		}
		if m.OSCrypt != ProfileOSCryptMock || m.Vault != ProfileVaultOpen || cls.Basis != ClassifyBasisNewProfile {
			t.Fatalf("%+v / %+v", m, cls)
		}
	})
	t.Run("orphan vault without manifest is refused", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ProfileVaultFile), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := EnsureProfileManifest(dir, "headed"); !errors.Is(err, ErrVaultOrphaned) {
			t.Fatalf("want ErrVaultOrphaned, got %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, ProfileManifestFile)); !os.IsNotExist(err) {
			t.Fatalf("must not create a manifest next to an orphan vault: %v", err)
		}
	})
	t.Run("invalid manifest is not overwritten", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ProfileManifestFile)
		if err := os.WriteFile(path, []byte(`{"schema":9}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := EnsureProfileManifest(dir, "headed"); !errors.Is(err, ErrProfileManifestInvalid) {
			t.Fatalf("want ErrProfileManifestInvalid, got %v", err)
		}
		if body, _ := os.ReadFile(path); string(body) != `{"schema":9}` {
			t.Fatalf("invalid manifest was overwritten: %s", body)
		}
	})
}
