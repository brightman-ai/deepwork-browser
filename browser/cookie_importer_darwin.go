//go:build darwin

package browser

import (
	"crypto/sha1" //nolint:gosec
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/crypto/pbkdf2"
)

// platformDecryptV10 macOS 实现：从 Keychain 取密钥后 AES-128-CBC 解密 [TC-C4-07]。
func platformDecryptV10(data byte) (string, error) {
	key, err := getMacOSChromeKey
	if err != nil {
		return "", fmt.Errorf("%w: keychain access failed: %v", ErrCookieDecryptFailed, err)
	}
	return aes128CBCDecrypt(key, data)
}

// getMacOSChromeKey 从 macOS Keychain 读取 "Chrome Safe Storage" 密钥。
// 使用 security 命令行工具（避免 CGO Keychain API 依赖）。
func getMacOSChromeKey (byte, error) {
	cmd := exec.Command("security", "find-generic-password"
		"-ga", "Chrome Safe Storage"
		"-w") // -w: 只输出 password
	out, err := cmd.Output
	if err != nil {
		// 尝试不带 "Safe Storage" 的旧版条目
		cmd2 := exec.Command("security", "find-generic-password"
			"-ga", "Chrome", "-w")
		out2, err2 := cmd2.Output
		if err2 != nil {
			return nil, ErrCookieDecryptFailed
		}
		out = out2
	}

	password := strings.TrimSpace(string(out))
	if password == "" {
		return nil, ErrCookieDecryptFailed
	}

	// PBKDF2-SHA1: key = PBKDF2(password, "saltysalt", 1003, 16)
	key := pbkdf2.Key(byte(password), byte("saltysalt"), 1003, 16, sha1.New) //nolint:gosec
	return key, nil
}
