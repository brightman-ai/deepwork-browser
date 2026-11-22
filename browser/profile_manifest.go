// Package browser — profile 清单（.dw-profile.json）与旧 profile 归类。
//
// 清单把"profile 的加密身份"固定在 profile 自己身上，而不是随启动模式漂移：
// 启动参数只看清单（mock → 任何模式都加 --use-mock-keychain --password-store=basic；
// system → 任何模式都不加）。封存格式与状态机见 profile_vault.go。
package browser

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // Chromium OSCrypt 的 KDF 就是 PBKDF2-SHA1，这里只是复现它去验证
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// ProfileManifestFile 是 profile 目录下的清单文件名。
	ProfileManifestFile = ".dw-profile.json"
	// ProfileVaultFile 是封存文件名。
	ProfileVaultFile = "profile.vault"

	profileManifestSchema = 1
)

// os_crypt 取值。
const (
	ProfileOSCryptMock   = "mock"   // Chromium 公开的固定口令（--use-mock-keychain / --password-store=basic）
	ProfileOSCryptSystem = "system" // 系统钥匙串；不封存，保住已有登录态
)

// vault 取值。
const (
	ProfileVaultNone   = "none"   // 不封存（临时 profile / system profile）
	ProfileVaultOpen   = "open"   // 明文，运行中或崩溃遗留
	ProfileVaultSealed = "sealed" // 已封存，目录里只剩清单和 profile.vault
)

// 旧 profile 归类依据（ProfileClassification.Basis）。
const (
	ClassifyBasisNewProfile    = "new-profile"                                 // 目录不存在或为空
	ClassifyBasisNoCookies     = "no-cookies"                                  // 已有 Chrome 状态，但没有加密 Cookie
	ClassifyBasisMockDecrypts  = "cookie-decrypts-with-mock"                   // 模拟口令解开了 Cookie，且域绑定校验通过
	ClassifyBasisMockLenient   = "cookie-decrypts-with-mock-no-domain-binding" // 无域绑定的旧版 Cookie：宽松档（填充合法 + UTF-8）判 mock
	ClassifyBasisSystemCookies = "cookie-not-decryptable-with-mock"            // 有加密 Cookie，但模拟口令解不开
)

var (
	// ErrProfileManifestNotFound 目录里没有 .dw-profile.json。
	ErrProfileManifestNotFound = errors.New("browser: profile manifest not found")
	// ErrProfileManifestInvalid 清单存在但内容不合法（含未知 schema）。
	ErrProfileManifestInvalid = errors.New("browser: profile manifest invalid")
)

// ProfileManifest 是 <profile>/.dw-profile.json 的内容。
type ProfileManifest struct {
	Schema      int    `json:"schema"`
	ProfileID   string `json:"profile_id"`
	OSCrypt     string `json:"os_crypt"`
	Vault       string `json:"vault"`
	CreatedMode string `json:"created_mode"`
	CreatedAt   string `json:"created_at"`
}

// NewProfileManifest 构造一份新清单（profile_id 随机 128 位）。
// createdMode 仅作记录（如 "headless" / "headed" / "visible"），不参与任何决策。
func NewProfileManifest(osCrypt, vault, createdMode string) (*ProfileManifest, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("browser: profile id: %w", err)
	}
	m := &ProfileManifest{
		Schema:      profileManifestSchema,
		ProfileID:   hex.EncodeToString(raw[:]),
		OSCrypt:     osCrypt,
		Vault:       vault,
		CreatedMode: createdMode,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *ProfileManifest) validate() error {
	if m == nil {
		return fmt.Errorf("%w: nil manifest", ErrProfileManifestInvalid)
	}
	if m.Schema != profileManifestSchema {
		return fmt.Errorf("%w: unsupported schema %d", ErrProfileManifestInvalid, m.Schema)
	}
	if m.ProfileID == "" {
		return fmt.Errorf("%w: empty profile_id", ErrProfileManifestInvalid)
	}
	switch m.OSCrypt {
	case ProfileOSCryptMock, ProfileOSCryptSystem:
	default:
		return fmt.Errorf("%w: os_crypt %q", ErrProfileManifestInvalid, m.OSCrypt)
	}
	switch m.Vault {
	case ProfileVaultNone, ProfileVaultOpen, ProfileVaultSealed:
	default:
		return fmt.Errorf("%w: vault %q", ErrProfileManifestInvalid, m.Vault)
	}
	if m.OSCrypt == ProfileOSCryptSystem && m.Vault != ProfileVaultNone {
		// system profile 的 Cookie 由系统钥匙串加密，封存没有意义且会破坏"保住登录态"的承诺。
		return fmt.Errorf("%w: os_crypt=system cannot be vaulted", ErrProfileManifestInvalid)
	}
	return nil
}

// CryptoLaunchArgs 返回清单要求的 Chrome 加密相关启动参数。
func (m *ProfileManifest) CryptoLaunchArgs() []string {
	if m != nil && m.OSCrypt == ProfileOSCryptMock {
		return []string{"--use-mock-keychain", "--password-store=basic"}
	}
	return nil
}

// ReadProfileManifest 读取并校验清单。不存在 → ErrProfileManifestNotFound。
func ReadProfileManifest(profileDir string) (*ProfileManifest, error) {
	path := filepath.Join(profileDir, ProfileManifestFile)
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrProfileManifestNotFound, path)
		}
		return nil, fmt.Errorf("browser: read profile manifest: %w", err)
	}
	var m ProfileManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProfileManifestInvalid, err)
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// WriteProfileManifest 原子写入清单（tmp → fsync → rename）。
func WriteProfileManifest(profileDir string, m *ProfileManifest) error {
	if err := m.validate(); err != nil {
		return err
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return atomicWriteFile(filepath.Join(profileDir, ProfileManifestFile), body, 0o600)
}

// atomicWriteFile 在同目录写 tmp、fsync、rename，最后 fsync 目录。
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	fail := func(err error) error {
		_ = tmp.Close()
		_ = removeFileInRoot(dir, tmpPath)
		return err
	}
	if perm != 0o600 { // CreateTemp 默认就是 0600
		if err := tmp.Chmod(perm); err != nil {
			return fail(err)
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = removeFileInRoot(dir, tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = removeFileInRoot(dir, tmpPath)
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir 尽力 fsync 目录（让 rename / unlink 落盘）。部分平台不支持，忽略错误。
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// ============================================================
// § 旧 profile 凭证据归类
// ============================================================

// ProfileClassification 是对一个"没有清单的 profile 目录"的归类结论。
type ProfileClassification struct {
	OSCrypt       string `json:"os_crypt"`       // mock | system
	Vault         string `json:"vault"`          // 新策略下的初始 vault 状态：open（关闭时封存）| none
	Basis         string `json:"basis"`          // ClassifyBasis*
	CookiesProbed int    `json:"cookies_probed"` // 实际试解的加密 Cookie 条数
	MockOK        int    `json:"mock_ok"`        // 其中能被模拟口令解开的条数（严格档或宽松档）
	StrictOK      int    `json:"strict_ok"`      // 其中通过严格档（域绑定校验）的条数
}

// ClassifyProfile 凭证据归类，不凭假设：
//   - 用模拟口令试解 Cookies 库中的加密记录，并校验明文前 32 字节 == SHA256(host_key)
//     （Chrome 新版 Cookie 自带域绑定，随机错误口令几乎不可能通过）；
//   - 能解开 → mock；有加密 Cookie 但解不开 → system；没有 Cookie → 采用新策略 mock + 封存。
//
// Cookie 库存在但打不开/读不出时返回错误，不猜。
func ClassifyProfile(profileDir string) (*ProfileClassification, error) {
	st, err := os.Stat(profileDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newPolicyClassification(ClassifyBasisNewProfile, 0), nil
		}
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("browser: profile path is not a directory: %s", profileDir)
	}
	entries, err := os.ReadDir(profileDir)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return newPolicyClassification(ClassifyBasisNewProfile, 0), nil
	}

	// 新版 Chrome 把 Cookie 库放在 Default/Network/Cookies，旧布局是 Default/Cookies。两处都看。
	var rows []cookieRow
	for _, rel := range []string{
		filepath.Join("Default", "Network", "Cookies"),
		filepath.Join("Default", "Cookies"),
	} {
		path := filepath.Join(profileDir, rel)
		fi, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if fi.Size() == 0 {
			continue
		}
		got, err := readCookieRows(path)
		if err != nil {
			return nil, fmt.Errorf("browser: classify profile cookies %s: %w", rel, err)
		}
		rows = append(rows, got...)
	}
	return classifyCookieRows(rows), nil
}

// 宽松档最多看这么多条，且每一条都必须满足。
const lenientSampleLimit = 20

// classifyCookieRows 按证据归类：
//   - 严格档：明文前 32 字节 == SHA256(host_key)（Chrome 新版域绑定）。有任意一条通过就进入"按多数判定"：
//     能被模拟口令解开的条数 >= 解不开的条数 → mock（平局归 mock），否则 system。
//   - 严格档一条都没过：宽松档（兼容没有域绑定的旧版 Cookie）——取前 20 条，每一条都必须同一把模拟密钥解开、
//     PKCS7 填充合法、明文是合法 UTF-8，才判 mock；否则 system。
func classifyCookieRows(rows []cookieRow) *ProfileClassification {
	cls := &ProfileClassification{CookiesProbed: len(rows)}
	if len(rows) == 0 {
		return newPolicyClassification(ClassifyBasisNoCookies, 0)
	}
	for _, r := range rows {
		if mockCookieDecrypts(r.host, r.enc) {
			cls.StrictOK++
		}
	}
	if cls.StrictOK > 0 {
		cls.MockOK = cls.StrictOK
		if cls.StrictOK >= len(rows)-cls.StrictOK {
			cls.OSCrypt, cls.Vault, cls.Basis = ProfileOSCryptMock, ProfileVaultOpen, ClassifyBasisMockDecrypts
		} else {
			cls.OSCrypt, cls.Vault, cls.Basis = ProfileOSCryptSystem, ProfileVaultNone, ClassifyBasisSystemCookies
		}
		return cls
	}

	sample := rows
	if len(sample) > lenientSampleLimit {
		sample = sample[:lenientSampleLimit]
	}
	encs := make([][]byte, len(sample))
	for i, r := range sample {
		encs[i] = r.enc
	}
	// MockOK 只用于回执：任何一把模拟密钥能（宽松地）解开的记录数，取最大。
	for _, key := range mockKeys() {
		n := 0
		for _, r := range rows {
			if mockCookieDecryptsLenient(key, r.enc) {
				n++
			}
		}
		if n > cls.MockOK {
			cls.MockOK = n
		}
	}
	if mockCookiesDecryptLenient(encs) {
		cls.OSCrypt, cls.Vault, cls.Basis = ProfileOSCryptMock, ProfileVaultOpen, ClassifyBasisMockLenient
		return cls
	}
	cls.OSCrypt, cls.Vault, cls.Basis = ProfileOSCryptSystem, ProfileVaultNone, ClassifyBasisSystemCookies
	return cls
}

func newPolicyClassification(basis string, probed int) *ProfileClassification {
	return &ProfileClassification{
		OSCrypt: ProfileOSCryptMock, Vault: ProfileVaultOpen,
		Basis: basis, CookiesProbed: probed,
	}
}

// EnsureProfileManifest 保证 profileDir 有清单：已有 → 原样返回（不重新归类）；
// 没有 → 凭证据归类后写入。返回的 classification 仅在这次真的归类时非 nil。
//
// 目录里有 profile.vault 却没有清单时拒绝（ErrVaultOrphaned）：此时应先 UnsealProfile，
// 否则新建清单后下一次封存会把旧的封存文件覆盖掉。
func EnsureProfileManifest(profileDir, createdMode string) (*ProfileManifest, *ProfileClassification, error) {
	m, err := ReadProfileManifest(profileDir)
	if err == nil {
		return m, nil, nil
	}
	if !errors.Is(err, ErrProfileManifestNotFound) {
		return nil, nil, err
	}
	if _, serr := os.Lstat(filepath.Join(profileDir, ProfileVaultFile)); serr == nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrVaultOrphaned, profileDir)
	}
	cls, err := ClassifyProfile(profileDir)
	if err != nil {
		return nil, nil, err
	}
	m, err = NewProfileManifest(cls.OSCrypt, cls.Vault, createdMode)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(profileDir, 0o700); err != nil {
		return nil, nil, err
	}
	if err := WriteProfileManifest(profileDir, m); err != nil {
		return nil, nil, err
	}
	return m, cls, nil
}

// cookieRow 是 Cookies 库里的一条加密记录。
type cookieRow struct {
	host string
	enc  []byte
}

// readCookieRows 读出一个 Cookies 库里最多 cookieRowLimit 条加密记录（只读打开，库被锁时复制副本再读）。
func readCookieRows(dbPath string) ([]cookieRow, error) {
	const cookieRowLimit = 64

	db, tmpPath, err := openCookieDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if tmpPath != "" {
		defer removeFileInRoot(filepath.Dir(tmpPath), tmpPath)
	}

	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='cookies'`).Scan(&tables); err != nil {
		return nil, err
	}
	if tables == 0 {
		return nil, nil
	}
	rs, err := db.Query(`SELECT host_key, encrypted_value FROM cookies WHERE length(encrypted_value) > 3 LIMIT ?`, cookieRowLimit)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []cookieRow
	for rs.Next() {
		var r cookieRow
		if err := rs.Scan(&r.host, &r.enc); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

var (
	mockCookieKeysOnce sync.Once
	mockCookieKeys     [][]byte
)

// mockKeys 返回 Chromium 公开固定口令派生出的 AES-128 密钥：
//   - macOS --use-mock-keychain：PBKDF2-SHA1("mock_password", "saltysalt", 1003, 16)
//   - Linux --password-store=basic：PBKDF2-SHA1("peanuts", "saltysalt", 1, 16)
//
// 两者都算"mock"（清单 mock 同时下发两个启动参数），归类时都要认。
func mockKeys() [][]byte {
	mockCookieKeysOnce.Do(func() {
		for _, p := range []struct {
			password string
			iter     int
		}{{"mock_password", 1003}, {"peanuts", 1}} {
			k, err := pbkdf2.Key(sha1.New, p.password, []byte("saltysalt"), p.iter, 16)
			if err == nil {
				mockCookieKeys = append(mockCookieKeys, k)
			}
		}
	})
	return mockCookieKeys
}

// mockCookieDecrypts 判断 enc（Chrome encrypted_value）能否被模拟口令解开，
// 并且明文前 32 字节等于 SHA256(host_key)。严格 PKCS7，避免错误密钥的偶然通过。
func mockCookieDecrypts(hostKey string, enc []byte) bool {
	if len(enc) < 3+2*aes.BlockSize || string(enc[:3]) != "v10" {
		return false
	}
	ct := enc[3:]
	if len(ct)%aes.BlockSize != 0 {
		return false
	}
	want := sha256.Sum256([]byte(hostKey))
	iv := []byte("                ") // 16 个空格
	for _, key := range mockKeys() {
		block, err := aes.NewCipher(key)
		if err != nil {
			continue
		}
		pt := make([]byte, len(ct))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
		pad := int(pt[len(pt)-1])
		if pad < 1 || pad > aes.BlockSize || pad > len(pt) {
			continue
		}
		valid := true
		for _, b := range pt[len(pt)-pad:] {
			if int(b) != pad {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		pt = pt[:len(pt)-pad]
		if len(pt) >= sha256.Size && string(pt[:sha256.Size]) == string(want[:]) {
			return true
		}
	}
	return false
}

// mockCookiesDecryptLenient：同一把模拟密钥必须解开样本里的每一条（PKCS7 填充合法 + 明文是合法 UTF-8）。
// 样本为空不算证据；任何一条是 v11（Linux 钥匙串）、块长度不对齐、填充不合法或不是 UTF-8，都不能判 mock。
func mockCookiesDecryptLenient(samples [][]byte) bool {
	if len(samples) == 0 {
		return false
	}
	for _, key := range mockKeys() {
		all := true
		for _, enc := range samples {
			if !mockCookieDecryptsLenient(key, enc) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// mockCookieDecryptsLenient 是宽松档的单条判断：用给定模拟密钥解 v10 记录，只要求 PKCS7 填充合法且明文是合法 UTF-8。
// 只在严格档一条都没过时使用（旧版 Cookie 没有域绑定前缀）；调用方负责"每一条都满足"的要求。
func mockCookieDecryptsLenient(key, enc []byte) bool {
	if len(enc) < 3+aes.BlockSize || string(enc[:3]) != "v10" {
		return false
	}
	ct := enc[3:]
	if len(ct)%aes.BlockSize != 0 {
		return false
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return false
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, []byte("                ")).CryptBlocks(pt, ct)
	pad := int(pt[len(pt)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(pt) {
		return false
	}
	for _, b := range pt[len(pt)-pad:] {
		if int(b) != pad {
			return false
		}
	}
	return utf8.Valid(pt[:len(pt)-pad])
}
