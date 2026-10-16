//go:build !darwin

package browser

func readSystemProxySettings() (publicSystemProxySettings, string, error) {
	return publicSystemProxySettings{}, "system-no-proxy", nil
}
