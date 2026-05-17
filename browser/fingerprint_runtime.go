package browser

import (
	"context"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"github.com/chromedp/cdproto/emulation"
)

var (
	chromeVersionPattern = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)\.(\d+)`)
	chromeUAFieldPattern = regexp.MustCompile(`Chrome/\d+\.\d+\.\d+\.\d+`)
	chromeNamePattern = regexp.MustCompile(`Chrome \d+`)
	darwinVersionOnce sync.Once
	darwinVersionValue string
	chromeVersionCache sync.Map
)

type chromeVersionInfo struct {
	Full string
	Major string
}

func detectChromeVersion(chromePath string) chromeVersionInfo {
	if chromePath == "" {
		if path, err := NewChromeLauncher.FindChrome; err == nil {
			chromePath = path
		}
	}
	if chromePath == "" {
		return chromeVersionInfo{}
	}
	if cached, ok := chromeVersionCache.Load(chromePath); ok {
		return cached.(chromeVersionInfo)
	}

	out, err := exec.Command(chromePath, "--version").Output
	if err != nil {
		chromeVersionCache.Store(chromePath, chromeVersionInfo{})
		return chromeVersionInfo{}
	}
	match := chromeVersionPattern.FindStringSubmatch(strings.TrimSpace(string(out)))
	if len(match) < 2 {
		chromeVersionCache.Store(chromePath, chromeVersionInfo{})
		return chromeVersionInfo{}
	}

	info := chromeVersionInfo{
		Full: match[0]
		Major: match[1]
	}
	chromeVersionCache.Store(chromePath, info)
	return info
}

func isChromePreset(preset *FingerprintPreset) bool {
	return preset != nil && strings.Contains(preset.UserAgent, "Chrome/")
}

func rewriteChromeUserAgent(userAgent string, major string) string {
	if major == "" {
		return userAgent
	}
	return chromeUAFieldPattern.ReplaceAllString(userAgent, "Chrome/"+major+".0.0.0")
}

func rewriteChromePresetName(name string, major string) string {
	if major == "" {
		return name
	}
	return chromeNamePattern.ReplaceAllString(name, "Chrome "+major)
}

// ResolveRuntimeFingerprintPreset returns a runtime copy of the preset with the
// local Chrome major version injected into UA/name for Chrome-based presets.
func ResolveRuntimeFingerprintPreset(presetID string, chromePath string) *FingerprintPreset {
	presetID = NormalizePresetID(presetID)
	base, ok := BuiltinPresets[presetID]
	if !ok || base == nil {
		return nil
	}

	resolved := *base
	if !isChromePreset(base) {
		return &resolved
	}

	version := detectChromeVersion(chromePath)
	if version.Major == "" {
		return &resolved
	}

	resolved.UserAgent = rewriteChromeUserAgent(base.UserAgent, version.Major)
	resolved.Name = rewriteChromePresetName(base.Name, version.Major)
	return &resolved
}

func darwinPlatformVersion string {
	darwinVersionOnce.Do(func {
		out, err := exec.Command("sw_vers", "-productVersion").Output
		if err == nil {
			darwinVersionValue = strings.TrimSpace(string(out))
		}
		if darwinVersionValue == "" {
			darwinVersionValue = "15.0.0"
		}
	})
	return darwinVersionValue
}

func userAgentMetadataForPreset(presetID string, chromePath string) *emulation.UserAgentMetadata {
	presetID = NormalizePresetID(presetID)
	preset := BuiltinPresets[presetID]
	if !isChromePreset(preset) {
		return nil
	}

	meta := &emulation.UserAgentMetadata{
		Mobile: preset.Mobile
		Wow64: false
	}

	switch presetID {
	case "windows-chrome":
		meta.Platform = "Windows"
		meta.PlatformVersion = "15.0.0"
		meta.Architecture = "x86"
		meta.Bitness = "64"
	case "linux-chrome":
		meta.Platform = "Linux"
		meta.PlatformVersion = "6.8.0"
		meta.Architecture = "x86"
		meta.Bitness = "64"
	case "macos-chrome":
		meta.Platform = "macOS"
		meta.PlatformVersion = darwinPlatformVersion
		meta.Architecture = "x86"
		if runtime.GOARCH == "arm64" {
			meta.Architecture = "arm"
		}
		meta.Bitness = "64"
	default:
		return nil
	}

	version := detectChromeVersion(chromePath)
	if version.Full != "" && version.Major != "" {
		meta.Brands = *emulation.UserAgentBrandVersion{
			{Brand: "Chromium", Version: version.Major}
			{Brand: "Google Chrome", Version: version.Major}
		}
		meta.FullVersionList = *emulation.UserAgentBrandVersion{
			{Brand: "Chromium", Version: version.Full}
			{Brand: "Google Chrome", Version: version.Full}
		}
	}

	return meta
}

func applyFingerprintEmulation(ctx context.Context, chromePath string, presetID string) error {
	resolved := ResolveRuntimeFingerprintPreset(presetID, chromePath)
	if !isChromePreset(resolved) {
		return nil
	}

	params := emulation.SetUserAgentOverride(resolved.UserAgent).
		WithAcceptLanguage("en-US,en").
		WithPlatform(resolved.Platform)

	if meta := userAgentMetadataForPreset(presetID, chromePath); meta != nil {
		params = params.WithUserAgentMetadata(meta)
	}

	return params.Do(ctx)
}
