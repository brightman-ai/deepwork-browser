//go:build darwin

package browser

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	keychainServiceName = "dw-browser.profile-vault"
	keychainAccountName = "master-key-v1"

	// 只经 Apple 签名的 /usr/bin/security 存取：条目的访问控制列表只信任创建它的 security，
	// 之后同一个二进制读取无弹窗（实测 0.02s）。不经 cgo、不引入 Security.framework 绑定。
	securityBinary = "/usr/bin/security"

	// security 的退出码：errSecItemNotFound / errSecDuplicateItem。
	securityExitNotFound  = 44
	securityExitDuplicate = 45

	keychainCallTimeout = 15 * time.Second
)

// 服务名/账户名会被拼进 `security -i` 的 stdin 命令行，只允许保守字符集，杜绝命令注入。
var keychainNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type keychainKeyStore struct {
	service string
	account string
}

func newKeychainKeyStore(service, account string) *keychainKeyStore {
	return &keychainKeyStore{service: service, account: account}
}

func defaultProfileKeyStore() KeyStore {
	return newKeychainKeyStore(keychainServiceName, keychainAccountName)
}

func (k *keychainKeyStore) Protection() string { return KeyProtectionKeychain }

func (k *keychainKeyStore) validNames() error {
	if !keychainNameRE.MatchString(k.service) || !keychainNameRE.MatchString(k.account) {
		return errors.New("invalid keychain service/account name")
	}
	return nil
}

// Load：security find-generic-password -w。密钥走 stdout 管道，不出现在任何命令行里。
func (k *keychainKeyStore) Load() ([]byte, error) {
	if err := k.validNames(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), keychainCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, securityBinary, "find-generic-password", "-s", k.service, "-a", k.account, "-w")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	defer wipeBuffer(&stdout)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("keychain read timed out")
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == securityExitNotFound {
			return nil, ErrVaultKeyNotFound
		}
		return nil, fmt.Errorf("keychain read failed: %s", securityFailure(err, stderr.String(), nil))
	}
	key, err := hex.DecodeString(strings.TrimSpace(stdout.String()))
	if err != nil {
		return nil, errors.New("keychain entry is not valid hex")
	}
	return key, nil
}

// Create：写入走 `security -i` 的 stdin（密钥不出现在进程命令行里），不带 -U 所以不覆盖；
// 已存在 → 退出码 45。写完立刻读回比对，防止"写成功但读不出"的静默失败。
func (k *keychainKeyStore) Create(key []byte) error {
	if err := k.validNames(); err != nil {
		return err
	}
	hexKey := []byte(hex.EncodeToString(key))
	defer clear(hexKey)

	cmdline := append([]byte("add-generic-password -s "+k.service+" -a "+k.account+" -w "), hexKey...)
	cmdline = append(cmdline, '\n')
	defer clear(cmdline)

	ctx, cancel := context.WithTimeout(context.Background(), keychainCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, securityBinary, "-i")
	cmd.Stdin = bytes.NewReader(cmdline)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("keychain write timed out")
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == securityExitDuplicate {
			return ErrVaultKeyExists
		}
		return fmt.Errorf("keychain write failed: %s", securityFailure(err, stderr.String(), hexKey))
	}

	got, err := k.Load()
	if err != nil {
		return fmt.Errorf("keychain write could not be read back: %w", err)
	}
	defer clear(got)
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return errors.New("keychain read-back mismatch")
	}
	return nil
}

// delete 仅供测试清理自己的条目。
func (k *keychainKeyStore) delete() error {
	if err := k.validNames(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), keychainCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, securityBinary, "delete-generic-password", "-s", k.service, "-a", k.account)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == securityExitNotFound {
			return ErrVaultKeyNotFound
		}
		return err
	}
	return nil
}

// securityFailure 把 security 的失败整理成可进错误文本的一行：先抹掉密钥字节，再截断。
func securityFailure(err error, stderr string, secret []byte) string {
	msg := strings.TrimSpace(stderr)
	if len(secret) > 0 {
		msg = strings.ReplaceAll(msg, string(secret), "<redacted>")
	}
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 160 {
		msg = msg[:160]
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("exit %d: %s", ee.ExitCode(), msg)
	}
	return msg
}

func wipeBuffer(b *bytes.Buffer) {
	raw := b.Bytes()
	clear(raw)
	b.Reset()
}
