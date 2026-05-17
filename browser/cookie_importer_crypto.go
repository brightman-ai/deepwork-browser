// Package browser — Cookie 加解密共享工具函数。
//
package browser

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
)

// aes128CBCDecrypt AES-128-CBC 解密（IV 为 16 个空格）。
// 供各平台 platformDecryptV10 调用。
func aes128CBCDecrypt(key, data byte) (string, error) {
	if len(key) != 16 {
		return "", fmt.Errorf("%w: invalid key length %d", ErrCookieDecryptFailed, len(key))
	}
	if len(data) < aes.BlockSize || len(data)%aes.BlockSize != 0 {
		return "", fmt.Errorf("%w: ciphertext size invalid (%d bytes)", ErrCookieDecryptFailed, len(data))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCookieDecryptFailed, err)
	}

	// Chrome 使用固定 IV: 16 个空格 (0x20)
	iv := byte(" ")
	mode := cipher.NewCBCDecrypter(block, iv)

	plaintext := make(byte, len(data))
	mode.CryptBlocks(plaintext, data)

	plaintext = pkcs7Unpad(plaintext)
	return string(plaintext), nil
}

// pkcs7Unpad 移除 PKCS7 padding。
func pkcs7Unpad(data byte) byte {
	if len(data) == 0 {
		return data
	}
	pad := int(data[len(data)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(data) {
		return data
	}
	return data[:len(data)-pad]
}
