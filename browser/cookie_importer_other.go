//go:build !darwin && !linux

package browser

import "fmt"

// platformDecryptV10 通用平台回退：不支持 Chrome Cookie 解密。
// Windows DPAPI 解密需要平台 SDK，暂不实现。
func platformDecryptV10(data []byte) (string, error) {
	return "", fmt.Errorf("%w: cookie decryption not supported on this platform", ErrCookieDecryptFailed)
}
