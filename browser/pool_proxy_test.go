package browser

import "testing"

func TestResolveBrowserPoolProxy_DarwinDefaultIgnoresAmbientProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1080")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1080")
	t.Setenv(browserPoolProxyEnv, "")
	t.Setenv(browserPoolInheritProxyEnv, "")

	proxy, source := resolveBrowserPoolProxyForGOOS("darwin")
	if proxy != "" || source != "" {
		t.Fatalf("expected darwin default to ignore ambient proxy, got proxy=%q source=%q", proxy, source)
	}
}

func TestResolveBrowserPoolProxy_ExplicitProxyWins(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1080")
	t.Setenv(browserPoolProxyEnv, "socks5://127.0.0.1:7890")
	t.Setenv(browserPoolInheritProxyEnv, "")

	proxy, source := resolveBrowserPoolProxyForGOOS("darwin")
	if proxy != "socks5://127.0.0.1:7890" || source != browserPoolProxyEnv {
		t.Fatalf("expected explicit proxy, got proxy=%q source=%q", proxy, source)
	}
}

func TestResolveBrowserPoolProxy_DarwinOptInCanInheritAmbientProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1080")
	t.Setenv(browserPoolProxyEnv, "")
	t.Setenv(browserPoolInheritProxyEnv, "true")

	proxy, source := resolveBrowserPoolProxyForGOOS("darwin")
	if proxy != "http://127.0.0.1:1080" || source != "HTTPS_PROXY" {
		t.Fatalf("expected inherited HTTPS proxy, got proxy=%q source=%q", proxy, source)
	}
}

func TestResolveBrowserPoolProxy_LinuxDefaultKeepsAmbientProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1080")
	t.Setenv(browserPoolProxyEnv, "")
	t.Setenv(browserPoolInheritProxyEnv, "")

	proxy, source := resolveBrowserPoolProxyForGOOS("linux")
	if proxy != "http://127.0.0.1:1080" || source != "HTTPS_PROXY" {
		t.Fatalf("expected linux default to keep ambient proxy, got proxy=%q source=%q", proxy, source)
	}
}

func TestProfileSchemaVersionForPreset_MacOnlyBumpsToV6(t *testing.T) {
	if got := profileSchemaVersionForPreset("macos-chrome"); got != "v6" {
		t.Fatalf("macos-chrome schema version = %q, want v6", got)
	}
	if got := profileSchemaVersionForPreset("linux-chrome"); got != defaultProfileSchemaVersion {
		t.Fatalf("linux-chrome schema version = %q, want %q", got, defaultProfileSchemaVersion)
	}
}
