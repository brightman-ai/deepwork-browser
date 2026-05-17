package browser

import "testing"

func TestValidatePresetIDRejectsRetiredIDs(t *testing.T) {
	for _, id := range []string{"ios-safari", "macos-safari"} {
		if _, err := ValidatePresetID(id); err == nil {
			t.Fatalf("ValidatePresetID(%q) succeeded; retired preset IDs must fail", id)
		}
	}
}

func TestResolveRuntimeFingerprintPreset_RewritesChromeVersion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chromeVersionCache.Store("/tmp/fake-chrome", chromeVersionInfo{
		Full:  "147.0.7727.101",
		Major: "147",
	})
	t.Cleanup(func() {
		chromeVersionCache.Delete("/tmp/fake-chrome")
	})

	preset := ResolveRuntimeFingerprintPreset("macos-chrome", "/tmp/fake-chrome")
	if preset == nil {
		t.Fatal("preset should not be nil")
	}
	if got, want := preset.UserAgent, "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36"; got != want {
		t.Fatalf("user agent = %q, want %q", got, want)
	}
	if got, want := preset.Name, "macOS Sequoia · Chrome 147"; got != want {
		t.Fatalf("preset name = %q, want %q", got, want)
	}
}

func TestUserAgentMetadataForPreset_ChromeCarriesVersionAndPlatform(t *testing.T) {
	chromeVersionCache.Store("/tmp/fake-chrome", chromeVersionInfo{
		Full:  "147.0.7727.101",
		Major: "147",
	})
	t.Cleanup(func() {
		chromeVersionCache.Delete("/tmp/fake-chrome")
	})

	meta := userAgentMetadataForPreset("macos-chrome", "/tmp/fake-chrome")
	if meta == nil {
		t.Fatal("metadata should not be nil")
	}
	if meta.Platform != "macOS" {
		t.Fatalf("platform = %q, want macOS", meta.Platform)
	}
	if meta.Mobile {
		t.Fatal("macos-chrome metadata should not be mobile")
	}
	if len(meta.Brands) != 2 || meta.Brands[1].Version != "147" {
		t.Fatalf("brands = %#v, want Chrome major 147", meta.Brands)
	}
	if len(meta.FullVersionList) != 2 || meta.FullVersionList[1].Version != "147.0.7727.101" {
		t.Fatalf("fullVersionList = %#v, want full version 147.0.7727.101", meta.FullVersionList)
	}
}
