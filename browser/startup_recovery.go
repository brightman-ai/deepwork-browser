// Package browser — startup recovery: 启动期 Chrome profile 健康检查 [v2 ]
//
// 来源:
// - (启动期 Recovery 4 步协议)
// - ( 实施范围)
//
// 协议 (4 步, 在 startChromeLocked 启动 Chrome 之前执行):
// 1. Singleton lock 残留检测: 解析 SingletonLock symlink 拿 PID;
// PID 已死 → 删除残留; PID 仍活 → 强杀 (orphan, 上次崩溃未清理)
// 2. profile health check: stat user-data-dir/Cookies + 校验 SQLite header (前 16 字节)
// 3. 损坏 → 整体重命名为 user-data-dir.broken/{timestamp}/ (隔离不污染下次)
// ProfileManager 会在下次自动重建 (空 profile_dir → Chrome 首次启动行为)
// 4. 返回 nil (健康) 或 error (隔离/清理失败)
//
// 不在范围:
// - .dw-pid 文件机制 — chromedp 不暴露 Chrome 进程 PID; 当前用 SingletonLock 反查代替
// - ProfileManager.Repair — 隔离后重建逻辑由 Pool 自然 lazy launch 接管
//
// audit 事件 (log only, 与 GracefulShutdown 风格对齐):
// - "startup_recovery_lock_cleaned" — Singleton 残留已删除
// - "startup_recovery_orphan_killed" — orphan Chrome 强杀
// - "startup_recovery_quarantined" — profile 损坏隔离
package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SQLite 文件 magic header (前 16 字节, 含 NUL 结尾).
// Cookies / Local Storage 等核心 profile 数据库均以此开头.
var sqliteMagicHeader = byte("SQLite format 3\x00")

// Chrome SingletonLock symlink target 格式: "{hostname}-{PID}".
// 例: "MacBook-Pro-12345" → PID=12345.
var singletonLockTargetRE = regexp.MustCompile(`-(\d+)$`)

const deepworkProfileOwnerFile = ".deepwork-browser-owner.json"

type deepworkProfileOwner struct {
	OwnerPID int `json:"owner_pid"`
	ChromePID int `json:"chrome_pid"`
	CDPPort int `json:"cdp_port,omitempty"`
	IdentityKey string `json:"identity_key,omitempty"`
	BrowserSessionID string `json:"browser_session_id,omitempty"`
	SessionKind string `json:"session_kind,omitempty"`
	BrowserMuxHostID string `json:"browser_mux_host_id,omitempty"`
	BrowserMuxHostPID int `json:"browser_mux_host_pid,omitempty"`
	RuntimeID string `json:"runtime_id,omitempty"`
	BrowserRunID string `json:"browser_run_id,omitempty"`
	ProfileID string `json:"profile_id,omitempty"`
	DisplayBackend string `json:"display_backend,omitempty"`
	DisplayID uint32 `json:"display_id,omitempty"`
	DisplayVerified bool `json:"display_verified,omitempty"`
	ChromeWindowContained bool `json:"chrome_window_contained,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

type ProfileOwnerMetadata struct {
	BrowserSessionID string
	SessionKind BrowserSessionKind
	BrowserMuxHostID string
	BrowserMuxHostPID int
	RuntimeID string
	BrowserRunID string
	ProfileID string
	DisplayBackend string
	DisplayID uint32
	DisplayVerified bool
	ChromeWindowContained bool
}

// RunStartupRecovery 在 Chrome 启动前对 profileDir 做 4 步健康检查 (CAP §3.5).
//
// 调用方:
// - BrowserPool.startChromeLocked (进程内 service 模式, IdentityKey 来自 Registry)
// - dw-browser open (CLI 短命模式, IdentityKey 用 "dw-cli-{sessionID}" 仅作 audit 标签)
//
// 返回 nil → profile 可直接启动 Chrome.
// 返回 err → 上层决定是否致命 (隔离失败应 abort, 清理失败可继续).
//
// identityKey 仅用于 audit log (定位故障 Chrome 实例).
func RunStartupRecovery(profileDir string, identityKey IdentityKey) error {
	// Step 0: Deepwork-owned profile owner marker.
	//
	// Chrome may exit cleanly enough to remove SingletonLock while the process
	// itself remains alive after a service crash/interruption. The marker is
	// Deepwork's own ownership fact, so startup recovery is not dependent on
	// Chrome's lock-file timing.
	if err := cleanDeepworkProfileOwner(profileDir, identityKey); err != nil {
		return err
	}

	// Step 1: Singleton lock 残留检测
	if err := cleanSingletonLocks(profileDir, identityKey); err != nil {
		// 非致命: 即使清理失败, Chrome 启动也会自己尝试夺锁
		log.Printf("[BROWSER-RECOVERY] identity=%s singleton cleanup partial failure: %v (continuing)", identityKey, err)
	}

	// Step 2-3: profile health check + 损坏隔离
	if err := checkAndQuarantineProfile(profileDir, identityKey); err != nil {
		return fmt.Errorf("profile health check: %w", err)
	}

	clearChromeCrashRestoreState(profileDir, identityKey)

	return nil
}

// RecoverBrowserRuntimeState performs a startup-wide owner-marker sweep for all
// BrowserPool profile directories under dataDir. It is intentionally separate
// from RunStartupRecovery: the app calls this at process startup, before any
// Browser Portal is opened, so a SIGKILLed BrowserMuxHost cannot leave headed
// Chrome visible on the user's primary Space until the next lazy acquire.
func RecoverBrowserRuntimeState(dataDir string) error {
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return nil
	}
	root := filepath.Join(dataDir, "browser-data", "profiles")
	if st, err := os.Stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat browser profile root: %w", err)
	} else if !st.IsDir {
		return nil
	}

	scanned := 0
	recovered := 0
	skippedLive := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir || entry.Name != deepworkProfileOwnerFile {
			return nil
		}
		scanned++
		profileDir := filepath.Dir(path)
		identityKey := recoverOwnerMarkerIdentity(path, profileDir)
		if err := cleanDeepworkProfileOwner(profileDir, identityKey); err != nil {
			if isLiveProfileOwnerError(err) {
				skippedLive++
				log.Printf("[BROWSER-RECOVERY] AUDIT: startup_recovery_sweep_live_owner_skipped identity=%s profile_dir=%s err=%v"
					identityKey, profileDir, err)
				return nil
			}
			return fmt.Errorf("recover profile owner marker %s: %w", profileDir, err)
		}
		recovered++
		return nil
	})
	if err != nil {
		return err
	}
	if scanned > 0 {
		log.Printf("[BROWSER-RECOVERY] AUDIT: startup_recovery_sweep_done root=%s scanned=%d recovered=%d skipped_live=%d"
			root, scanned, recovered, skippedLive)
	}
	return nil
}

func recoverOwnerMarkerIdentity(markerPath, profileDir string) IdentityKey {
	body, err := os.ReadFile(markerPath)
	if err == nil && len(body) > 0 {
		var marker deepworkProfileOwner
		if json.Unmarshal(body, &marker) == nil {
			if key := strings.TrimSpace(marker.IdentityKey); key != "" {
				return IdentityKey(key)
			}
			if hostID := strings.TrimSpace(marker.BrowserMuxHostID); hostID != "" {
				return IdentityKey("browser-mux-host-" + hostID)
			}
		}
	}
	return IdentityKey("browser-runtime-sweep-" + filepath.Base(profileDir))
}

func isLiveProfileOwnerError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error
	return strings.Contains(msg, "profile_owned_by_live_browser_mux_host") ||
		strings.Contains(msg, "profile_owned_by_live_deepwork")
}

func cleanDeepworkProfileOwner(profileDir string, identityKey IdentityKey) error {
	markerPath := filepath.Join(profileDir, deepworkProfileOwnerFile)
	marker := deepworkProfileOwner{}
	if body, err := os.ReadFile(markerPath); err == nil && len(body) > 0 {
		if jsonErr := json.Unmarshal(body, &marker); jsonErr != nil {
			log.Printf("[BROWSER-RECOVERY] AUDIT: startup_recovery_owner_marker_invalid identity=%s path=%s err=%v", identityKey, markerPath, jsonErr)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read profile owner marker: %w", err)
	}

	chromeAlive := marker.ChromePID > 0 && marker.ChromePID != os.Getpid && isPIDAlive(marker.ChromePID)
	hostMarker := strings.TrimSpace(marker.BrowserMuxHostID) != "" || marker.BrowserMuxHostPID > 0
	hostAlive := marker.BrowserMuxHostPID > 0 && marker.BrowserMuxHostPID != os.Getpid && isPIDAlive(marker.BrowserMuxHostPID)
	if hostMarker && hostAlive && !chromeAlive {
		log.Printf("[BROWSER-RECOVERY] AUDIT: startup_recovery_stale_browser_mux_host_killed identity=%s browser_mux_host_id=%s browser_mux_host_pid=%d chrome_pid=%d profile_dir=%s"
			identityKey, marker.BrowserMuxHostID, marker.BrowserMuxHostPID, marker.ChromePID, profileDir)
		killAndWait(marker.BrowserMuxHostPID, ProfileOwnerMuxHostKillGrace)
		hostAlive = marker.BrowserMuxHostPID > 0 && marker.BrowserMuxHostPID != os.Getpid && isPIDAlive(marker.BrowserMuxHostPID)
	}
	if hostMarker && hostAlive && chromeAlive {
		return fmt.Errorf("profile_owned_by_live_browser_mux_host: profile=%s browser_mux_host_id=%s browser_mux_host_pid=%d chrome_pid=%d chrome_alive=%t"
			profileDir, marker.BrowserMuxHostID, marker.BrowserMuxHostPID, marker.ChromePID, chromeAlive)
	}

	ownerAlive := marker.OwnerPID > 0 && marker.OwnerPID != os.Getpid && isPIDAlive(marker.OwnerPID)
	if !hostMarker && ownerAlive && chromeAlive {
		return fmt.Errorf("profile_owned_by_live_deepwork: profile=%s owner_pid=%d chrome_pid=%d", profileDir, marker.OwnerPID, marker.ChromePID)
	}

	if marker.OwnerPID > 0 && ownerAlive && !chromeAlive {
		log.Printf("[BROWSER-RECOVERY] AUDIT: startup_recovery_legacy_owner_marker_cleaned identity=%s owner_pid=%d chrome_pid=%d profile_dir=%s"
			identityKey, marker.OwnerPID, marker.ChromePID, profileDir)
	}

	if chromeAlive {
		log.Printf("[BROWSER-RECOVERY] AUDIT: startup_recovery_owner_orphan_killed identity=%s owner_pid=%d chrome_pid=%d"
			identityKey, marker.OwnerPID, marker.ChromePID)
		killAndWait(marker.ChromePID, ProfileOwnerChromeKillGrace)
	}

	for _, pid := range platformFindChromePIDsByProfileDir(profileDir) {
		if pid <= 0 || pid == os.Getpid || pid == marker.ChromePID {
			continue
		}
		if !isPIDAlive(pid) {
			continue
		}
		log.Printf("[BROWSER-RECOVERY] AUDIT: startup_recovery_profile_process_killed identity=%s pid=%d profile_dir=%s"
			identityKey, pid, profileDir)
		killAndWait(pid, ProfileOwnerChromeKillGrace)
	}

	if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove profile owner marker: %w", err)
	}
	return nil
}

func WriteProfileOwnerMarker(profileDir string, identityKey IdentityKey, chromePID, cdpPort int) error {
	return WriteProfileOwnerMarkerWithMetadata(profileDir, identityKey, chromePID, cdpPort, ProfileOwnerMetadata{})
}

func WriteProfileOwnerMarkerWithMetadata(profileDir string, identityKey IdentityKey, chromePID, cdpPort int, meta ProfileOwnerMetadata) error {
	if strings.TrimSpace(profileDir) == "" || chromePID <= 0 {
		return nil
	}
	if err := os.MkdirAll(profileDir, 0755); err != nil {
		return err
	}
	ownerPID := os.Getpid
	if meta.BrowserMuxHostPID > 0 {
		ownerPID = meta.BrowserMuxHostPID
	}
	body, err := json.MarshalIndent(deepworkProfileOwner{
		OwnerPID: ownerPID
		ChromePID: chromePID
		CDPPort: cdpPort
		IdentityKey: string(identityKey)
		BrowserSessionID: meta.BrowserSessionID
		SessionKind: string(meta.SessionKind)
		BrowserMuxHostID: meta.BrowserMuxHostID
		BrowserMuxHostPID: meta.BrowserMuxHostPID
		RuntimeID: meta.RuntimeID
		BrowserRunID: meta.BrowserRunID
		ProfileID: meta.ProfileID
		DisplayBackend: meta.DisplayBackend
		DisplayID: meta.DisplayID
		DisplayVerified: meta.DisplayVerified
		ChromeWindowContained: meta.ChromeWindowContained
		CreatedAt: time.Now.UTC.Format(time.RFC3339Nano)
	}, "", " ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(filepath.Join(profileDir, deepworkProfileOwnerFile), body, 0644)
}

func RemoveProfileOwnerMarker(profileDir string, identityKey IdentityKey) {
	if strings.TrimSpace(profileDir) == "" {
		return
	}
	path := filepath.Join(profileDir, deepworkProfileOwnerFile)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[BROWSER-RECOVERY] identity=%s owner marker remove failed path=%s err=%v", identityKey, path, err)
	}
}

func killAndWait(pid int, grace time.Duration) {
	_ = killPIDGraceful(pid)
	deadline := time.Now.Add(grace)
	for time.Now.Before(deadline) {
		if !isPIDAlive(pid) {
			return
		}
		time.Sleep(ProcessExitPollInterval)
	}
	if isPIDAlive(pid) {
		_ = killPIDForce(pid)
	}
}

// cleanSingletonLocks 处理 user-data-dir/Singleton* 残留.
//
// Chrome 在 user-data-dir 下创建 3 个 singleton 文件:
// - SingletonLock (symlink → "hostname-PID", 用于跨进程互斥)
// - SingletonCookie (类似)
// - SingletonSocket (Unix socket)
//
// 协议:
// - SingletonLock 可解析 PID:
// PID 已死 (kill -0 失败) → 直接删除残留 (audit: lock_cleaned)
// PID 仍活 → orphan Chrome (上次 Pool 强杀失败) → 强杀 + 删除 (audit: orphan_killed)
// - 其他 Singleton* → 直接删除 (无独立 PID 信息, 与 Lock 同进程绑定)
func cleanSingletonLocks(profileDir string, identityKey IdentityKey) error {
	lockPath := filepath.Join(profileDir, "SingletonLock")

	// 仅当 SingletonLock 存在时尝试解析 PID
	if pid, err := readSingletonLockPID(lockPath); err == nil {
		if isPIDAlive(pid) {
			log.Printf("[BROWSER-RECOVERY] AUDIT: startup_recovery_orphan_killed identity=%s pid=%d", identityKey, pid)
			// SIGTERM → 短等 → SIGKILL (与 GracefulShutdown 双阶段对齐, 但 grace 短: 1s)
			killAndWait(pid, ProfileOwnerChromeKillGrace)
		} else {
			log.Printf("[BROWSER-RECOVERY] AUDIT: startup_recovery_lock_cleaned identity=%s stale_pid=%d", identityKey, pid)
		}
	}

	// 删除所有 Singleton* 残留 (不论 PID 解析成功与否)
	matches, err := filepath.Glob(filepath.Join(profileDir, "Singleton*"))
	if err != nil {
		return fmt.Errorf("glob Singleton*: %w", err)
	}
	var firstErr error
	for _, p := range matches {
		if rmErr := os.Remove(p); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && firstErr == nil {
			firstErr = rmErr
		}
	}
	return firstErr
}

// readSingletonLockPID 读取 SingletonLock symlink 的 target, 解析末尾 PID.
//
// Chrome SingletonLock target 格式: "{hostname}-{PID}".
// 不是 symlink (常规文件) 或解析失败 → 返回 (0, error).
func readSingletonLockPID(lockPath string) (int, error) {
	target, err := os.Readlink(lockPath)
	if err != nil {
		return 0, err
	}
	m := singletonLockTargetRE.FindStringSubmatch(target)
	if len(m) < 2 {
		return 0, fmt.Errorf("singleton lock target %q: no trailing PID", target)
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("singleton lock target %q: invalid PID", target)
	}
	return pid, nil
}

// isPIDAlive 检测 PID 进程是否存活 (跨平台 dispatch 见 startup_recovery_{unix,windows}.go).
func isPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return platformIsPIDAlive(pid)
}

// checkAndQuarantineProfile 校验 profileDir 健康度, 损坏 → 隔离.
//
// 健康判定 (任一不通过 → 损坏):
// - profileDir 存在但非目录 → 损坏 (异常状态)
// - Cookies 文件存在但 SQLite header 损坏 → 损坏
// - profileDir 不存在 / 完全空 → 健康 (首次启动语义, Chrome 自建)
// - Cookies 不存在但 profileDir 有其他文件 → 健康 (Chrome 首次未访问站点的正常状态)
//
// 隔离动作: rename profileDir → {profileDir}.broken/{timestamp}/
// 隔离后原 profileDir 不存在, Chrome 启动会自动重建 (相当于全新 profile, Human 需重新登录).
func checkAndQuarantineProfile(profileDir string, identityKey IdentityKey) error {
	st, err := os.Stat(profileDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil // 首次启动语义
	}
	if err != nil {
		return fmt.Errorf("stat profile_dir: %w", err)
	}
	if !st.IsDir {
		return quarantineProfile(profileDir, identityKey, "profile_dir is not a directory")
	}

	// Cookies SQLite header 校验
	cookiesPath := filepath.Join(profileDir, "Cookies")
	if cookiesSt, statErr := os.Stat(cookiesPath); statErr == nil && !cookiesSt.IsDir {
		if cookiesSt.Size == 0 {
			// 空 Cookies 文件 = Chrome 启动中崩溃, 视为损坏
			return quarantineProfile(profileDir, identityKey, "Cookies file is empty (likely crashed during init)")
		}
		f, openErr := os.Open(cookiesPath)
		if openErr != nil {
			// 读不了 Cookies 不一定致命 (权限问题), 仅 log warning
			log.Printf("[BROWSER-RECOVERY] identity=%s cannot open Cookies for header check: %v", identityKey, openErr)
			return nil
		}
		header := make(byte, len(sqliteMagicHeader))
		n, readErr := f.Read(header)
		_ = f.Close
		if readErr != nil || n != len(sqliteMagicHeader) {
			return quarantineProfile(profileDir, identityKey, fmt.Sprintf("Cookies header read short (n=%d, err=%v)", n, readErr))
		}
		if string(header) != string(sqliteMagicHeader) {
			return quarantineProfile(profileDir, identityKey, "Cookies header mismatch (corrupted SQLite)")
		}
	}

	return nil
}

func clearChromeCrashRestoreState(profileDir string, identityKey IdentityKey) {
	for _, path := range string{
		filepath.Join(profileDir, "Local State")
		filepath.Join(profileDir, "Default", "Preferences")
	} {
		changed, err := rewriteChromeCleanExitJSON(path)
		if err != nil {
			log.Printf("[BROWSER-RECOVERY] identity=%s crash restore cleanup failed path=%s err=%v", identityKey, path, err)
			continue
		}
		if changed {
			log.Printf("[BROWSER-RECOVERY] AUDIT: startup_recovery_crash_restore_state_cleaned identity=%s path=%s", identityKey, path)
		}
	}
}

func rewriteChromeCleanExitJSON(path string) (bool, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(body) == 0 {
		return false, nil
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return false, err
	}
	profile, _ := data["profile"].(map[string]any)
	if profile == nil {
		profile = map[string]any{}
		data["profile"] = profile
	}
	changed := false
	if profile["exit_type"] != "Normal" {
		profile["exit_type"] = "Normal"
		changed = true
	}
	if profile["exited_cleanly"] != true {
		profile["exited_cleanly"] = true
		changed = true
	}
	if !changed {
		return false, nil
	}
	next, err := json.MarshalIndent(data, "", " ")
	if err != nil {
		return false, err
	}
	next = append(next, '\n')
	return true, os.WriteFile(path, next, 0644)
}

// quarantineProfile 将损坏的 profileDir 重命名到 {profileDir}.broken/{timestamp}/.
//
// 注:
// - .broken/ 目录不自动清理 (CAP §3.5 约束: Human 决定何时删除以避免误删可恢复数据)
// - rename 失败 → 致命错误 (Chrome 启动会撞同名 dir, 必须 abort)
func quarantineProfile(profileDir string, identityKey IdentityKey, reason string) error {
	parent := filepath.Dir(profileDir)
	base := filepath.Base(profileDir)
	brokenRoot := filepath.Join(parent, base+".broken")
	if err := os.MkdirAll(brokenRoot, 0755); err != nil {
		return fmt.Errorf("create broken root: %w", err)
	}
	target := filepath.Join(brokenRoot, time.Now.UTC.Format("20060102T150405Z"))
	if err := os.Rename(profileDir, target); err != nil {
		return fmt.Errorf("quarantine rename %s → %s: %w", profileDir, target, err)
	}
	log.Printf("[BROWSER-RECOVERY] AUDIT: startup_recovery_quarantined identity=%s reason=%q moved_to=%s", identityKey, reason, target)
	return nil
}
