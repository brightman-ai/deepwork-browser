package browser

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ============================================================
// § Session Manager [Ref: T5A-BS-09]
// ============================================================

// sessionsDir はセッションファイルを保存するディレクトリ。
const sessionsDir = "/tmp/dw-browser-sessions"

// SessionInfo Chrome セッション情報（ファイルに永続化）。
type SessionInfo struct {
	SessionID        string             `json:"session_id"`
	BrowserSessionID string             `json:"browser_session_id,omitempty"`
	SessionKind      BrowserSessionKind `json:"session_kind,omitempty"`
	Goal             string             `json:"goal,omitempty"`
	Owner            string             `json:"owner,omitempty"`
	AuthorityState   string             `json:"authority_state,omitempty"`
	ProfileID        string             `json:"profile_id,omitempty"`
	Isolation        string             `json:"isolation,omitempty"`
	ServiceName      string             `json:"service,omitempty"`
	AccountID        string             `json:"account_id,omitempty"`
	ChromePID        int                `json:"chrome_pid"`
	WSURL            string             `json:"ws_url"` // CDP WebSocket URL
	DebugPort        int                `json:"debug_port"`
	TargetID         string             `json:"target_id"` // CDP target ID for the active page
	Mode             BrowserMode        `json:"mode,omitempty"`
	PresetID         string             `json:"preset_id,omitempty"`
	ProfileDir       string             `json:"profile_dir"`
	PageURL          string             `json:"page_url"`
	CreatedAt        string             `json:"created_at"`
	ViewportW        int                `json:"viewport_w"`
	ViewportH        int                `json:"viewport_h"`
	UserAgent        string             `json:"user_agent"`
	Touch            bool               `json:"touch"`
	SnapEpoch        int                `json:"snap_epoch"` // Incremented on each snap
	Refs             []SessionRef       `json:"refs"`       // Ref table from last snap
	Ephemeral        bool               `json:"ephemeral"`  // true if --ephemeral was used
	XvfbPID          int                `json:"xvfb_pid"`   // Xvfb process PID (headed mode, Linux)

	BrowserMuxHostID      string `json:"browser_mux_host_id,omitempty"`
	BrowserMuxHostPID     int    `json:"browser_mux_host_pid,omitempty"`
	RuntimeID             string `json:"runtime_id,omitempty"`
	BrowserRunID          string `json:"browser_run_id,omitempty"`
	DisplayBackend        string `json:"display_backend,omitempty"`
	DisplayID             uint32 `json:"display_id,omitempty"`
	DisplayVerified       bool   `json:"display_verified,omitempty"`
	ChromeWindowContained bool   `json:"chrome_window_contained,omitempty"`

	Engine     BrowserEngine `json:"engine,omitempty"`      // 空值 = chrome（向后兼容）
	DeviceUDID string        `json:"device_udid,omitempty"` // Safari Simulator UDID
	DeviceName string        `json:"device_name,omitempty"` // Safari 设备名
}

// SessionRef セッション内の要素 ref エントリ。
type SessionRef struct {
	Ref           string      `json:"ref"` // "@r1", "@r2", ...
	BackendNodeID int64       `json:"backend_node_id"`
	Role          string      `json:"role"`
	Name          string      `json:"name"`
	TestID        string      `json:"testid,omitempty"`
	Placeholder   string      `json:"placeholder,omitempty"`
	Locator       NodeLocator `json:"locator,omitempty"`
	AXPath        string      `json:"ax_path,omitempty"`
	StableKey     string      `json:"stable_key,omitempty"`
}

// sessionFilePath セッションIDからファイルパスを取得。
func sessionFilePath(sessionID string) string {
	return filepath.Join(sessionsDir, sessionID+".json")
}

// SaveSession セッション情報をファイルに書き込む。
func SaveSession(info *SessionInfo) error {
	NormalizeSessionInfo(info)
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		return fmt.Errorf("session_manager: mkdir %s: %w", sessionsDir, err)
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("session_manager: marshal: %w", err)
	}
	path := sessionFilePath(info.SessionID)
	if err := writeFileAtomic(path, data, 0644); err != nil {
		return fmt.Errorf("session_manager: write %s: %w", path, err)
	}
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// LoadSession セッションファイルを読み込む。
func LoadSession(sessionID string) (*SessionInfo, error) {
	path := sessionFilePath(sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: session %q", ErrSessionNotFound, sessionID)
		}
		return nil, fmt.Errorf("session_manager: read %s: %w", path, err)
	}
	var info SessionInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("session_manager: parse %s: %w", path, err)
	}
	NormalizeSessionInfo(&info)
	return &info, nil
}

// DeleteSession セッションファイルを削除する。
func DeleteSession(sessionID string) error {
	path := sessionFilePath(sessionID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("session_manager: delete %s: %w", path, err)
	}
	return nil
}

// ListSessions 全アクティブセッションの一覧を返す。
func ListSessions() ([]SessionInfo, error) {
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil, fmt.Errorf("session_manager: list dir: %w", err)
	}
	var sessions []SessionInfo
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		sessionID := entry.Name()[:len(entry.Name())-5] // strip .json
		info, err := LoadSession(sessionID)
		if err != nil {
			continue // skip corrupt files
		}
		sessions = append(sessions, *info)
	}
	return sessions, nil
}

// FindFreePort 利用可能な TCP ポートを探す。
func FindFreePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("session_manager: find free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}

// WaitForChromeReady Chrome の CDP エンドポイントが準備完了するまでポーリング。
// 成功時に WebSocket URL を返す。
func WaitForChromeReady(port int, timeout time.Duration) (string, error) {
	startedAt := time.Now()
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	attempts := 0
	var lastErr error
	for time.Now().Before(deadline) {
		attempts++
		wsURL, err := fetchChromeWSURL(url)
		if err == nil && wsURL != "" {
			log.Printf("[CHROME-LAUNCH] cdp_ready port=%d attempts=%d elapsed_ms=%d request_timeout_ms=%d",
				port, attempts, time.Since(startedAt).Milliseconds(), ChromeVersionRequestTimeout.Milliseconds())
			return wsURL, nil
		}
		if err != nil {
			lastErr = err
		}
		time.Sleep(ChromeReadyPollInterval)
	}
	log.Printf("[CHROME-LAUNCH] cdp_ready_timeout port=%d attempts=%d elapsed_ms=%d last_err=%v",
		port, attempts, time.Since(startedAt).Milliseconds(), lastErr)
	return "", fmt.Errorf("session_manager: Chrome on port %d not ready after %s", port, timeout)
}

// fetchChromeWSURL /json/version エンドポイントから webSocketDebuggerUrl を取得。
func fetchChromeWSURL(versionURL string) (string, error) {
	client := &http.Client{Timeout: ChromeVersionRequestTimeout}
	resp, err := client.Get(versionURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.WebSocketDebuggerURL, nil
}

// ============================================================
// § Target Resolution — session target 自愈机制
// ============================================================
//
// 设计原则 (TH-0412-v4n):
//   全页面导航(非 SPA)、页面崩溃、Chrome 内部 target 重建等场景下，
//   session 文件中存储的 target_id 可能 stale。ResolveSessionTarget
//   提供透明的自愈: 先尝试存储的 target → 失败时查询 Chrome 找最佳匹配 → 更新 session。
//
// 所有 session 命令 (act/snap/get/wait/screenshot/explore) 在连接前调用此函数，
// 而非直接使用 session.TargetID。

// ResolveSessionTarget 解析有效的 target ID。
// 快速路径: 存储的 target_id 仍有效 → 直接返回。
// 恢复路径: target 不存在 → 查询 Chrome /json → 选最佳 page target → 更新 session 文件。
func ResolveSessionTarget(session *SessionInfo) (string, error) {
	if session.DebugPort <= 0 {
		return session.TargetID, nil // 无 debug port，无法验证，原样返回
	}

	targets, err := FetchChromeTargets(session.DebugPort)
	if err != nil {
		return session.TargetID, nil // Chrome 不可达时保留原 target（可能是网络瞬断）
	}

	// 快速路径: 存储的 target 仍存在且是用户页。
	// 若它仍停在 ChromeInitialPageURL，但 Chrome 中同时存在更合适的用户页 target，
	// 则仍需进入自愈路径，避免 session 永远绑在空白标签页上。
	storedTargetExists := false
	storedTargetUsable := false
	for _, t := range targets {
		tID := ExtractDevToolsTargetID(t)
		if tID == session.TargetID {
			storedTargetExists = true
			tURL, _ := t["url"].(string)
			if IsUserPageTargetURL(tURL) {
				storedTargetUsable = true
			}
			break
		}
	}
	if storedTargetUsable {
		return session.TargetID, nil
	}

	// 恢复路径:
	//   1. target 已不存在
	//   2. target 仍存在，但它是 Chrome 初始化页 / 空 URL
	// 两种情况都需要找最佳 page target。
	best := SelectAttachablePageTarget(targets, session.PageURL, "")
	bestID, bestURL := best.ID, best.URL
	if bestID == "" {
		if storedTargetExists && session.TargetID != "" {
			return session.TargetID, nil
		}
		return "", fmt.Errorf("session_manager: target %s stale and no suitable page target found (Chrome has %d targets)", session.TargetID, len(targets))
	}

	if bestID == session.TargetID {
		if bestURL != "" && session.PageURL != bestURL {
			session.PageURL = bestURL
			_ = SaveSession(session)
		}
		return session.TargetID, nil
	}

	// 自愈: 更新 session 文件
	oldTarget := session.TargetID
	session.TargetID = bestID
	if bestURL != "" {
		session.PageURL = bestURL
	}
	session.Refs = nil // target 变了，旧 refs 失效
	_ = SaveSession(session)

	fmt.Fprintf(os.Stderr, "[session-recovery] target %s → %s (url: %s)\n", oldTarget[:min(len(oldTarget), 8)], bestID[:min(len(bestID), 8)], bestURL)
	return bestID, nil
}

// min returns the smaller of a and b.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// FetchChromeTargets Chrome の /json エンドポイントからターゲット一覧を取得。
func FetchChromeTargets(port int) ([]map[string]interface{}, error) {
	client := &http.Client{Timeout: ChromeTargetsRequestTimeout}
	url := fmt.Sprintf("http://127.0.0.1:%d/json", port)
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("session_manager: fetch targets: %w", err)
	}
	defer resp.Body.Close()
	var targets []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, fmt.Errorf("session_manager: decode targets: %w", err)
	}
	return targets, nil
}
