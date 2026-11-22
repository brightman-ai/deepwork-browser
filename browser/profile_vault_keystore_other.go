//go:build !darwin

package browser

// defaultProfileKeyStore 非 macOS：profile 根目录下 0600 的密钥文件，
// 回执里如实标注 protection=file。
func defaultProfileKeyStore() KeyStore {
	return defaultFileKeyStore()
}
