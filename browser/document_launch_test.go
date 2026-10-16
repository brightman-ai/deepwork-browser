package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocumentLaunchEnforcesPolicy(t *testing.T) {
	dir := t.TempDir()
	args, err := prepareDocumentLaunch(dir, "http://127.0.0.1:32123", 0, 1024, 768)
	if err != nil {
		t.Fatal(err)
	}
	flags := map[string]bool{}
	for _, arg := range args {
		flags[arg] = true
	}
	for _, required := range []string{"--headless=new", "--enable-automation", "--remote-debugging-port=0", "--proxy-server=http://127.0.0.1:32123", "--proxy-bypass-list=<-loopback>", "--mute-audio", "--block-new-web-contents", "--deny-permission-prompts", "--disable-quic"} {
		if !flags[required] {
			t.Errorf("missing policy %s", required)
		}
	}
	for _, arg := range args {
		for _, unsafe := range []string{"--no-sandbox", "--disable-web-security", "--ignore-certificate-errors", "--disable-gpu-sandbox", "--disable-popup-blocking", "--disable-blink-features=AutomationControlled", "--user-agent="} {
			if strings.HasPrefix(arg, unsafe) {
				t.Errorf("unsafe policy argument %s", arg)
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "Default", "Preferences"))
	if err != nil {
		t.Fatal(err)
	}
	var prefs struct {
		WebRTC struct {
			IPHandlingPolicy string `json:"ip_handling_policy"`
		} `json:"webrtc"`
	}
	if err := json.Unmarshal(data, &prefs); err != nil {
		t.Fatal(err)
	}
	if prefs.WebRTC.IPHandlingPolicy != "disable_non_proxied_udp" {
		t.Fatalf("WebRTC can bypass proxy: %s", data)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("profile exposed mode %v", info.Mode())
	}
}

func TestDocumentLaunchDisablesUpdaterSchedulerInBothModes(t *testing.T) {
	for _, mode := range []BrowserMode{ModeHeadless, ModeHeaded} {
		t.Run(string(mode), func(t *testing.T) {
			args, err := prepareDocumentLaunchMode(t.TempDir(), "http://127.0.0.1:32123", 0, 1024, 768, mode)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, arg := range args {
				if arg == "--disable-updater-scheduler" {
					found = true
				}
				if arg == "--no-sandbox" || arg == "--disable-web-security" {
					t.Fatalf("document launch relaxed security: %s", arg)
				}
			}
			if !found {
				t.Fatal("document job may start the browser's delayed global updater scheduler")
			}
		})
	}
}
