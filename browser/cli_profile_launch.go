// Package browser — CLI profile 的启动前准备与关闭后封存（P2 接入层）。
//
// profile_manifest.go / profile_vault.go（P2 模块，独占维护，这里只经 API 使用） 是纯模块：给一个目录，它们能分类、封存、解封。
// 这里把它们接到"启动 Chrome"和"关闭 Chrome"这两个时刻上，并统一产出 open / close / profile list 的回执：
//
//	启动前  PrepareProfileForLaunch：封存着就先解封 → 启动恢复 → 保证清单存在 → 启动参数只从清单得出
//	关闭后  SealPersistentProfile：   Chrome 已经消失之后，按需归类旧 profile，再封存；失败不吞，如实写进回执
//
// 持久 profile = 会话结束后保留的 profile（非 ephemeral）：关闭时正常退出并封存。
// 临时 profile = 会随会话删除的 profile：mock、不封存。
package browser

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvProfileKeyFile 指向一个 0600 的密钥文件时，封存/解封使用它而不是平台默认的密钥库（钥匙串）。
// 用途：无钥匙串的环境，以及测试——测试必须靠它避免创建生产钥匙串条目。回执的 protection 如实写 file。
const EnvProfileKeyFile = "DW_BROWSER_PROFILE_KEY_FILE"

// LaunchProfileKeyStore 返回本进程封存/解封该用的 KeyStore。
func LaunchProfileKeyStore() KeyStore {
	if p := strings.TrimSpace(os.Getenv(EnvProfileKeyFile)); p != "" {
		return NewFileKeyStore(p)
	}
	return DefaultProfileKeyStore()
}

// 回执里 profile.error_kind 的取值：让调用方不必解析错误文本。
const (
	ProfileErrInUse            = "profile_in_use"    // SingletonLock 指向存活进程，或另一个封存操作持有锁
	ProfileErrUnsupportedEntry = "unsupported_entry" // 遇到普通 symlink 等无法安全封存的条目：封存中止，明文保留
	ProfileErrDeleteRefused    = "delete_refused"    // 删除清单校验没通过：未删除任何东西
	ProfileErrKeyUnavailable   = "key_unavailable"   // 主密钥取不到（钥匙串拒绝 / 超时 / 损坏）
	ProfileErrAuthFailed       = "vault_auth_failed" // 认证失败：错误密钥、篡改、截断
	ProfileErrChromeNotExited  = "chrome_not_exited" // Chrome 没能确认退出：封存根本没有尝试，明文原样保留
	ProfileErrManifest         = "manifest"          // 清单不合法或归类失败
	ProfileErrOther            = "seal_failed"
)

func profileErrorKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrProfileInUse):
		return ProfileErrInUse
	case errors.Is(err, ErrVaultUnsupportedEntry):
		return ProfileErrUnsupportedEntry
	case errors.Is(err, ErrVaultDeleteRefused):
		return ProfileErrDeleteRefused
	case errors.Is(err, ErrVaultKeyUnavailable):
		return ProfileErrKeyUnavailable
	case errors.Is(err, ErrVaultAuthFailed), errors.Is(err, ErrVaultMalformed), errors.Is(err, ErrVaultUnsafeEntry):
		return ProfileErrAuthFailed
	case errors.Is(err, ErrProfileManifestInvalid), errors.Is(err, ErrProfileManifestNotFound), errors.Is(err, ErrVaultOrphaned):
		return ProfileErrManifest
	}
	return ProfileErrOther
}

// ProfileState 是一个 profile 的加密身份与封存状态——只描述事实（profile list 直接用它）。
// Error 非空时 Vault 仍是清单里的真实状态（以清单为准，不是以期望为准）。
type ProfileState struct {
	Managed    bool   `json:"managed"`              // 清单是否已接管（false = 还没有清单，下次启动时归类）
	OSCrypt    string `json:"os_crypt,omitempty"`   // mock | system | unclassified
	Vault      string `json:"vault,omitempty"`      // none | open | sealed
	Protection string `json:"protection,omitempty"` // keychain | file；仅 vault != none 时给
	Error      string `json:"error,omitempty"`
	ErrorKind  string `json:"error_kind,omitempty"`
	Note       string `json:"note,omitempty"`
}

// ProfileCryptoReceipt 是 open / close 回执里 "profile" 一节：ProfileState 加上这一次操作做了什么。
type ProfileCryptoReceipt struct {
	ProfileState
	Persistent   bool          `json:"persistent"`              // 关闭时是否保留并封存
	ClassifiedBy string        `json:"classified_by,omitempty"` // 这次新建清单时的归类依据
	Unseal       *VaultReceipt `json:"unseal,omitempty"`
	Seal         *VaultReceipt `json:"seal,omitempty"`
}

// ProfileLaunch 是 PrepareProfileForLaunch 的结果。
type ProfileLaunch struct {
	Manifest *ProfileManifest
	Receipt  ProfileCryptoReceipt

	// release 是"启动租约"（UnsealProfileForLaunch 的返回）：解封之后一直持有封存/解封锁，直到 Chrome 已经建好
	// SingletonLock 或已就绪。这段时间里别的进程不能对同一个目录封存或再解封，否则它看不到任何"在运行"的证据，
	// 会把刚解封的 profile 又封回去。没有解封动作时为 nil。
	release func()
}

// Release 声明 Chrome 已经就绪（或这次启动被放弃）：放掉启动租约。幂等，nil 安全。
// 调用方必须在对同一个目录做 SealPersistentProfile 之前调用它，否则封存会等自己持有的锁。
func (l *ProfileLaunch) Release() {
	if l == nil || l.release == nil {
		return
	}
	release := l.release
	l.release = nil
	release()
}

// CryptoArgs 返回启动参数里与加密身份相关的部分——只来自清单，与启动模式无关。
func (l *ProfileLaunch) CryptoArgs() []string {
	if l == nil {
		return nil
	}
	return l.Manifest.CryptoLaunchArgs()
}

// needsUnseal：清单说 sealed，或目录里有 profile.vault（含"有 vault 没清单"与"open + 陈旧 vault"）。
func needsUnseal(dir string) (bool, error) {
	m, err := ReadProfileManifest(dir)
	vaultPresent := false
	if _, serr := os.Lstat(filepath.Join(dir, ProfileVaultFile)); serr == nil {
		vaultPresent = true
	}
	switch {
	case err == nil:
		return m.Vault == ProfileVaultSealed || vaultPresent, nil
	case errors.Is(err, ErrProfileManifestNotFound):
		return vaultPresent, nil
	default:
		return false, err
	}
}

// PrepareProfileForLaunch 在启动 Chrome 之前把 profile 带到"可以启动"的状态：
//
//  1. 目录不存在：什么都不用解封；
//  2. 已封存（或残留 vault）：UnsealProfile。解不开就失败——绝不在一个封存着的目录上启动出一份空 profile；
//  3. recoverFn（启动恢复：孤儿进程、Singleton 残留、损坏隔离），可为 nil；它可能把整个目录改名隔离，所以排在清单之前；
//  4. 创建目录，保证清单存在（没有就凭证据归类，新建 profile → mock）；
//  5. persistent 时把"之前当临时 profile 建的 mock profile"升级成可封存。
//
// persistent=false（临时 profile）新建的清单是 mock + vault=none：不封存。
func PrepareProfileForLaunch(profileDir, createdMode string, persistent bool, recoverFn func() error) (*ProfileLaunch, error) {
	l := &ProfileLaunch{}
	l.Receipt.Persistent = persistent
	fail := func(err error) (*ProfileLaunch, error) {
		l.Release() // 启动被放弃：不能带着租约返回
		l.Receipt.Error, l.Receipt.ErrorKind = err.Error(), profileErrorKind(err)
		return l, err
	}

	if _, err := os.Lstat(profileDir); err == nil {
		need, err := needsUnseal(profileDir)
		if err != nil {
			return fail(fmt.Errorf("browser: profile state unreadable, refusing to launch on it: %w", err))
		}
		if need {
			// 解封并持有启动租约：调用方在 Chrome 就绪（或放弃启动）之后调 Release。出错时租约已经释放，
			// 返回的 release 仍然可以无条件调用。
			rc, release, err := UnsealProfileForLaunch(profileDir, LaunchProfileKeyStore())
			l.release = release
			l.Receipt.Unseal = &rc
			if err != nil {
				return fail(fmt.Errorf("browser: profile is sealed and could not be unsealed: %w", err))
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}

	if recoverFn != nil {
		if err := recoverFn(); err != nil {
			return fail(err)
		}
	}
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return fail(fmt.Errorf("browser: create profile dir: %w", err))
	}

	m, cls, err := EnsureProfileManifest(profileDir, createdMode)
	if err != nil {
		return fail(fmt.Errorf("browser: profile manifest: %w", err))
	}
	switch {
	case cls != nil && !persistent && m.Vault == ProfileVaultOpen:
		// 刚为一个临时 profile 归类出的清单：它会随会话删除，没有封存的意义。
		m.Vault = ProfileVaultNone
		if err := WriteProfileManifest(profileDir, m); err != nil {
			return fail(fmt.Errorf("browser: profile manifest: %w", err))
		}
	case persistent && m.OSCrypt == ProfileOSCryptMock && m.Vault == ProfileVaultNone:
		m.Vault = ProfileVaultOpen
		if err := WriteProfileManifest(profileDir, m); err != nil {
			return fail(fmt.Errorf("browser: profile manifest: %w", err))
		}
		l.Receipt.Note = "profile was created as temporary; it is persistent now, so it will be sealed when the session closes"
	}

	l.Manifest = m
	l.Receipt.Managed = true
	l.Receipt.OSCrypt, l.Receipt.Vault = m.OSCrypt, m.Vault
	if m.Vault != ProfileVaultNone {
		l.Receipt.Protection = LaunchProfileKeyStore().Protection()
	}
	if cls != nil {
		l.Receipt.ClassifiedBy = cls.Basis
	}
	if m.OSCrypt == ProfileOSCryptSystem && l.Receipt.Note == "" {
		l.Receipt.Note = "existing profile whose cookies are encrypted by the system keychain: keeps its login state, is not sealed"
	}
	return l, nil
}

// SealPersistentProfile 在 Chrome 已经消失之后封存一个持久 profile，并返回如实的回执：
//
//   - 旧 profile（没有清单，由旧版本启动）先凭证据归类；归类为 system 的不封存；
//   - 封存失败（正在使用、遇到 symlink、删除清单校验被拒、密钥取不到……）不吞：
//     回执写明 Error / ErrorKind，Vault 以封存之后清单里的真实状态为准——明文还在就是 open，
//     封存已提交但明文没清干净就是 sealed 并带 Note。下次启动解封、下次关闭再试。
func SealPersistentProfile(profileDir string) ProfileCryptoReceipt {
	rc := ProfileCryptoReceipt{Persistent: true}
	if _, err := os.Stat(profileDir); err != nil {
		rc.Note = "profile directory does not exist; nothing to seal"
		return rc
	}
	m, _, err := EnsureProfileManifest(profileDir, "close")
	if err != nil {
		rc.Error, rc.ErrorKind = err.Error(), profileErrorKind(err)
		return rc
	}
	rc.Managed, rc.OSCrypt, rc.Vault = true, m.OSCrypt, m.Vault
	if m.Vault == ProfileVaultNone {
		if m.OSCrypt == ProfileOSCryptSystem {
			rc.Note = "system-keychain profile: not sealed"
		}
		return rc
	}
	ks := LaunchProfileKeyStore()
	rc.Protection = ks.Protection()
	vr, err := SealProfile(profileDir, ks)
	rc.Seal = &vr
	// 以清单为准报告最终状态。
	if after, rerr := ReadProfileManifest(profileDir); rerr == nil {
		rc.Vault = after.Vault
		if err != nil && after.Vault == ProfileVaultSealed {
			rc.Note = "sealed, but plaintext leftovers remain; they are discarded the next time the profile is opened"
		}
	}
	if err != nil {
		rc.Error, rc.ErrorKind = err.Error(), profileErrorKind(err)
		if rc.Vault == ProfileVaultOpen {
			rc.Note = "NOT sealed: plaintext profile is intact; sealing is retried the next time this profile is closed"
		}
	}
	return rc
}

// DescribeProfile 只读地描述一个 profile 目录的加密身份（profile list 用）：不取密钥、不归类、不改动任何东西。
func DescribeProfile(profileDir string) ProfileState {
	m, err := ReadProfileManifest(profileDir)
	switch {
	case err == nil:
		st := ProfileState{Managed: true, OSCrypt: m.OSCrypt, Vault: m.Vault}
		if m.Vault != ProfileVaultNone {
			st.Protection = LaunchProfileKeyStore().Protection()
		}
		return st
	case errors.Is(err, ErrProfileManifestNotFound):
		return ProfileState{OSCrypt: "unclassified", Note: "no manifest yet; classified from evidence the first time it is opened"}
	default:
		return ProfileState{Error: err.Error(), ErrorKind: profileErrorKind(err)}
	}
}

// ProfileNotSealedChromeAlive 是"Chrome 没有被确认退出"时的封存回执：封存只能在 Chrome 进程确认消失之后做
// （一个仍在写盘的进程会让封存读到不一致的内容，或把正在用的目录删掉），所以这里根本不尝试，
// 明文原样保留，下次关闭时再试。detail 是 ShutdownChrome 给出的原因。
func ProfileNotSealedChromeAlive(profileDir, detail string) ProfileCryptoReceipt {
	rc := ProfileCryptoReceipt{Persistent: true}
	rc.ProfileState = DescribeProfile(profileDir)
	rc.Error = "not sealed because chrome was not confirmed to have exited: " + detail
	rc.ErrorKind = ProfileErrChromeNotExited
	rc.Note = "NOT sealed: plaintext profile is intact; sealing is retried the next time this profile is closed"
	return rc
}
