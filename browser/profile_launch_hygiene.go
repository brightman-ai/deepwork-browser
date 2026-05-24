package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// PrepareProfileForControlledLaunch removes Chrome session-restore artifacts
// without touching cookies, local storage, IndexedDB, passwords, or permissions.
//
// This matters for invisible headed mode: a restored Chrome window can ignore
// the requested --window-position and briefly appear on the Human's main Space.
func PrepareProfileForControlledLaunch(profileDir string) error {
	if profileDir == "" {
		return nil
	}
	// Modern Chrome stores tab/session restore state under Default/Sessions.
	_ = os.RemoveAll(filepath.Join(profileDir, "Default", "Sessions"))
	// Older Chrome variants used these files.
	for _, rel := range []string{
		filepath.Join("Default", "Current Session"),
		filepath.Join("Default", "Current Tabs"),
		filepath.Join("Default", "Last Session"),
		filepath.Join("Default", "Last Tabs"),
	} {
		_ = os.Remove(filepath.Join(profileDir, rel))
	}
	return markChromeProfileExitedCleanly(filepath.Join(profileDir, "Default", "Preferences"))
}

func markChromeProfileExitedCleanly(prefPath string) error {
	raw, err := os.ReadFile(prefPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var prefs map[string]interface{}
	if err := json.Unmarshal(raw, &prefs); err != nil {
		// Preferences corruption is handled by startup recovery. Do not block a
		// launch merely because this best-effort hygiene step cannot parse JSON.
		return nil
	}
	profile, _ := prefs["profile"].(map[string]interface{})
	if profile == nil {
		profile = map[string]interface{}{}
		prefs["profile"] = profile
	}
	profile["exited_cleanly"] = true
	profile["exit_type"] = "Normal"

	session, _ := prefs["session"].(map[string]interface{})
	if session == nil {
		session = map[string]interface{}{}
		prefs["session"] = session
	}
	// 5 = open New Tab page. It prevents "continue where you left off" from
	// resurrecting old window bounds while preserving site data.
	session["restore_on_startup"] = float64(5)

	updated, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(prefPath, updated, 0644)
}
