//go:build linux

package browser

import (
	"crypto/sha1" //nolint:gosec
	"fmt"

	"golang.org/x/crypto/pbkdf2"
)

// platformDecryptV10 Linux 实现：使用固定密钥 "peanuts" + PBKDF2 + AES-128-CBC 解密 [TC-C4-07]。
// Linux Chrome 默认使用已知密钥（无需 SecretService），适合 CI 环境。
func platformDecryptV10(data byte) (string, error) {
	// Linux Chrome 默认密钥: "peanuts" (Chromium 源码中的默认值)
	password := "peanuts"
	key := pbkdf2.Key(byte(password), byte("saltysalt"), 1, 16, sha1.New) //nolint:gosec
	result, err := aes128CBCDecrypt(key, data)
	if err != nil {
		return "", fmt.Errorf("%w: linux decrypt failed", ErrCookieDecryptFailed)
	}
	return result, nil
}
