// Package browser — profile 封存的主密钥存储。
//
// 每台机器一个 32 字节主密钥；每次封存用 HKDF 从它派生出独立的数据密钥（见 profile_vault.go）。
// 主密钥放哪里由 KeyStore 决定：macOS 登录钥匙串（profile_vault_keystore_darwin.go），
// 其他平台 profile 根目录下 0600 的密钥文件（本文件的 fileKeyStore）。
//
// 铁律：密钥字节绝不进入进程命令行、日志、错误文本。
package browser

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	profileVaultMasterKeyLen = 32
	profileVaultKeyFileName  = "vault-master.key"
)

// 回执里的 protection 取值。
const (
	KeyProtectionKeychain = "keychain" // 系统钥匙串
	KeyProtectionFile     = "file"     // 0600 密钥文件（与封存文件同机同用户，防护弱于钥匙串）
)

var (
	// ErrVaultKeyUnavailable 主密钥取不到（不存在且不允许创建 / 钥匙串拒绝 / 超时 / 内容损坏）。
	ErrVaultKeyUnavailable = errors.New("browser: profile vault key unavailable")
	// ErrVaultKeyNotFound KeyStore 层：条目不存在。LoadOrCreateMasterKey 之外的调用方通常看到的是
	// 包了 ErrVaultKeyUnavailable 的错误。
	ErrVaultKeyNotFound = errors.New("browser: profile vault key not found")
	// ErrVaultKeyExists KeyStore.Create 层：条目已存在（并发创建的败者）。
	ErrVaultKeyExists = errors.New("browser: profile vault key already exists")
)

// KeyStore 存取机器主密钥。实现必须保证 Create 是"不覆盖"的原子创建。
type KeyStore interface {
	// Protection 返回回执用的防护级别（KeyProtection*）。
	Protection() string
	// Load 读取主密钥。条目不存在 → ErrVaultKeyNotFound；其他失败 → 任意错误（不得含密钥）。
	Load() ([]byte, error)
	// Create 原子创建主密钥。已存在 → ErrVaultKeyExists，且不得改动已有条目。
	Create(key []byte) error
}

// DefaultProfileKeyStore 返回当前平台的默认 KeyStore。
func DefaultProfileKeyStore() KeyStore {
	return defaultProfileKeyStore()
}

// LoadOrCreateMasterKey 读取主密钥，不存在则生成并创建（并发创建时认领先到者的值）。
// 仅封存路径使用；解封路径只应 Load，绝不凭空创建——新密钥解不开旧封存。
func LoadOrCreateMasterKey(ks KeyStore) ([]byte, error) {
	key, err := ks.Load()
	if err == nil {
		return checkMasterKey(ks, key)
	}
	if !errors.Is(err, ErrVaultKeyNotFound) {
		return nil, keyUnavailable(ks, err)
	}
	fresh := make([]byte, profileVaultMasterKeyLen)
	if _, err := rand.Read(fresh); err != nil {
		return nil, fmt.Errorf("%w: entropy: %v", ErrVaultKeyUnavailable, err)
	}
	if err := ks.Create(fresh); err != nil {
		clear(fresh)
		if !errors.Is(err, ErrVaultKeyExists) {
			return nil, keyUnavailable(ks, err)
		}
		key, err := ks.Load()
		if err != nil {
			return nil, keyUnavailable(ks, err)
		}
		return checkMasterKey(ks, key)
	}
	return fresh, nil
}

// LoadMasterKey 读取已有主密钥；不存在同样视为不可用。
func LoadMasterKey(ks KeyStore) ([]byte, error) {
	key, err := ks.Load()
	if err != nil {
		return nil, keyUnavailable(ks, err)
	}
	return checkMasterKey(ks, key)
}

func checkMasterKey(ks KeyStore, key []byte) ([]byte, error) {
	if len(key) != profileVaultMasterKeyLen {
		clear(key)
		return nil, fmt.Errorf("%w: %s entry has wrong length", ErrVaultKeyUnavailable, ks.Protection())
	}
	return key, nil
}

func keyUnavailable(ks KeyStore, err error) error {
	if errors.Is(err, ErrVaultKeyNotFound) {
		return fmt.Errorf("%w: master key not found in %s store", ErrVaultKeyUnavailable, ks.Protection())
	}
	return fmt.Errorf("%w: %s store: %v", ErrVaultKeyUnavailable, ks.Protection(), err)
}

// ============================================================
// § 文件密钥库
// ============================================================

type fileKeyStore struct {
	path string
}

// NewFileKeyStore 返回把主密钥存成 0600 文件（hex）的 KeyStore。
func NewFileKeyStore(path string) KeyStore {
	return &fileKeyStore{path: path}
}

// DefaultProfileKeyFilePath 是非 macOS 平台默认密钥文件位置：os.UserConfigDir()/dw-browser/vault-master.key。
// 刻意不放在 profile 根目录里：profile 根目录是会被备份、同步、打包的对象，密钥不能跟被保护的东西躺在一起。
func DefaultProfileKeyFilePath() string {
	cfg, err := os.UserConfigDir()
	if err != nil || cfg == "" {
		return ""
	}
	return filepath.Join(cfg, "dw-browser", profileVaultKeyFileName)
}

// mkdirAllPrivate 创建 dir 及缺失的上级目录，并把"这次新建出来的"每一级收紧到 0700。
// 已存在的目录不动（不替用户改别人的目录权限）。
//
// 必须逐级"建一级、立刻 chmod、再建下一级"：umask 会从 0700 里再去掉位（umask 0277 → 0500），
// 如果等 MkdirAll 全部建完再 chmod，下一级目录在一个不可写的父目录里就建不出来了。
func mkdirAllPrivate(dir string) error {
	var missing []string // 由深到浅
	for d := dir; ; d = filepath.Dir(d) {
		fi, err := os.Lstat(d)
		if err == nil {
			if !fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("browser: %s exists and is not a directory", d)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		d := missing[i]
		if err := os.Mkdir(d, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue // 并发创建的赢家；它的权限不归我们管
			}
			return err
		}
		if runtime.GOOS != "windows" {
			if err := os.Chmod(d, 0o700); err != nil {
				return err
			}
		}
	}
	return nil
}

// unavailableKeyStore 在无法确定默认位置时占位，让调用方得到类型化错误而不是 panic。
type unavailableKeyStore struct{ reason string }

func (u unavailableKeyStore) Protection() string { return KeyProtectionFile }
func (u unavailableKeyStore) Load() ([]byte, error) {
	return nil, errors.New(u.reason)
}
func (u unavailableKeyStore) Create([]byte) error { return errors.New(u.reason) }

func defaultFileKeyStore() KeyStore {
	path := DefaultProfileKeyFilePath()
	if path == "" {
		return unavailableKeyStore{reason: "cannot determine home directory for key file"}
	}
	return NewFileKeyStore(path)
}

func (s *fileKeyStore) Protection() string { return KeyProtectionFile }

func (s *fileKeyStore) Load() ([]byte, error) {
	fi, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrVaultKeyNotFound
		}
		return nil, err
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("key file permissions %04o too open (want 0600)", fi.Mode().Perm())
	}
	body, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	defer clear(body)
	key, err := hex.DecodeString(strings.TrimSpace(string(body)))
	if err != nil {
		return nil, errors.New("key file is not valid hex")
	}
	return key, nil
}

// Create 先写完整的 tmp 再 link 到目标：目标要么不存在要么内容完整，且 link 不会覆盖已有文件。
func (s *fileKeyStore) Create(key []byte) error {
	dir := filepath.Dir(s.path)
	if err := mkdirAllPrivate(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".dw-vault-key-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer removeFileInRoot(dir, tmpPath)
	line := []byte(hex.EncodeToString(key) + "\n")
	_, werr := tmp.Write(line)
	clear(line)
	if werr != nil {
		_ = tmp.Close()
		return werr
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpPath, s.path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrVaultKeyExists
		}
		return err
	}
	syncDir(dir)
	return nil
}
