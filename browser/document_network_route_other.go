//go:build !darwin

package browser

func readDocumentSystemProxySettings() (documentSystemProxySettings, string, error) {
	return documentSystemProxySettings{}, "system-no-proxy", nil
}
