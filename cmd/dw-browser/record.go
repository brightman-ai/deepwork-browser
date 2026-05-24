// record.go — dw-browser record {start|stop|export} 子命令
// Implements: CAP-BS09-C5 v3 §3.5 Record Mode CLI fallback
//
// 架构说明:
//
//	dw-browser CLI 每条命令是独立进程，无法共享内存中的 RecordBuffer。
//	录制状态通过文件持久化: /tmp/dw-browser-sessions/<id>-record.json
//	record start → 写状态文件（标记录制开始 + domain/URL）
//	dw-browser act  → 若状态文件存在，追加 step（实现 act-tap，见 appendRecordStep）
//	record stop  → 读状态文件 → 输出 trace JSON → 删除文件
//	record export → 读状态文件 → 输出 trace JSON（不删除，允许继续录制）
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// ============================================================
// § Record State 文件结构
// ============================================================

const recordSessionsDir = "/tmp/dw-browser-sessions"

// CLIRecordState 录制状态文件，持久化在 /tmp/dw-browser-sessions/<id>-record.json
type CLIRecordState struct {
	SessionID string          `json:"session_id"`
	Recording bool            `json:"recording"`
	Domain    string          `json:"domain"`
	StartURL  string          `json:"start_url"`
	StartTime time.Time       `json:"start_time"`
	Steps     []CLIRecordStep `json:"steps"`
}

// CLIRecordStep 录制的单个操作步骤（CLI 级别 — 记录 act 动作字符串）
type CLIRecordStep struct {
	Seq         int    `json:"seq"`
	Action      string `json:"action"`        // 原始 act 动作字符串（如 "click link:'More information'"）
	URL         string `json:"url,omitempty"` // 操作时的页面 URL
	TimestampMs int64  `json:"timestamp_ms"`
}

// CLIRecordTrace 最终输出的 trace 格式（兼容 RecordTrace JSON 结构）
type CLIRecordTrace struct {
	Domain     string          `json:"domain"`
	StartURL   string          `json:"start_url"`
	StartTime  time.Time       `json:"start_time"`
	DurationMs int64           `json:"duration_ms"`
	Steps      []CLIRecordStep `json:"steps"`
}

// recordStateFilePath 返回录制状态文件路径。
func recordStateFilePath(sessionID string) string {
	return filepath.Join(recordSessionsDir, sessionID+"-record.json")
}

// loadRecordState 读取录制状态文件。
func loadRecordState(sessionID string) (*CLIRecordState, error) {
	path := recordStateFilePath(sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no active recording for session %q (use 'record start' first)", sessionID)
		}
		return nil, fmt.Errorf("record: read state file: %w", err)
	}
	var state CLIRecordState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("record: parse state file: %w", err)
	}
	return &state, nil
}

// saveRecordState 写录制状态文件（原子写）。
func saveRecordState(state *CLIRecordState) error {
	if err := os.MkdirAll(recordSessionsDir, 0755); err != nil {
		return fmt.Errorf("record: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("record: marshal state: %w", err)
	}
	path := recordStateFilePath(state.SessionID)
	// 原子写（tmp → rename）
	tmp, err := os.CreateTemp(recordSessionsDir, state.SessionID+"-record.*.tmp")
	if err != nil {
		return fmt.Errorf("record: create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// deleteRecordState 删除录制状态文件。
func deleteRecordState(sessionID string) {
	_ = os.Remove(recordStateFilePath(sessionID))
}

// extractDomainFromURL 从 URL 字符串提取 host（用于 domain 字段）。
func extractDomainFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Host
}

// ============================================================
// § appendRecordStep — act-tap 集成点（由 runActSession 调用）
// ============================================================

// appendRecordStep 若当前 session 有活跃录制，将 act 动作追加为一个 step。
// 无录制状态文件 → 静默返回（零副作用）。
// 设计原则: 不影响 act 主路径，错误静默忽略（录制是可选的）。
func appendRecordStep(sessionID, action, pageURL string) {
	if sessionID == "" {
		return
	}
	state, err := loadRecordState(sessionID)
	if err != nil || !state.Recording {
		return // 未录制 → 静默跳过
	}

	step := CLIRecordStep{
		Seq:         len(state.Steps) + 1,
		Action:      action,
		URL:         pageURL,
		TimestampMs: time.Now().UnixMilli(),
	}
	state.Steps = append(state.Steps, step)

	// 静默忽略写入错误，不影响 act 主路径
	_ = saveRecordState(state)
}

// ============================================================
// § runRecord dispatcher
// ============================================================

// runRecord — `dw-browser record {start|stop|export}` dispatcher
func runRecord(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "Usage: dw-browser record {start|stop|export} --session <id>\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "  record start   --session <id>   开始录制当前 session 的操作\n")
		fmt.Fprintf(os.Stderr, "  record stop    --session <id>   停止录制，输出 trace JSON → stdout\n")
		fmt.Fprintf(os.Stderr, "  record export  --session <id>   导出当前 trace（不停止录制）\n")
		os.Exit(exitRunErr)
	}
	switch args[0] {
	case "start":
		runRecordStart(args[1:])
	case "stop":
		runRecordStop(args[1:])
	case "export":
		runRecordExport(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "dw-browser record: unknown subcommand %q\n", args[0])
		fmt.Fprintf(os.Stderr, "  Available: start, stop, export\n")
		os.Exit(exitRunErr)
	}
}

// ============================================================
// § runRecordStart
// ============================================================

// runRecordStart — `dw-browser record start --session <id>`
// 1. 解析 --session flag
// 2. 加载 session 文件获取当前页面 URL + domain
// 3. 写录制状态文件
// 4. 输出: "Recording started for {domain} (session: {id})"
func runRecordStart(args []string) {
	_, flags := parseCommonFlags(args, "record start")

	if flags.sessionID == "" {
		fmt.Fprintf(os.Stderr, "dw-browser record start: --session <id> is required\n")
		os.Exit(exitRunErr)
	}

	// 加载 session 获取当前页面信息
	sessionInfo, err := loadSessionForRecord(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser record start: %v\n", err)
		os.Exit(exitRunErr)
	}

	pageURL := sessionInfo.PageURL
	domain := extractDomainFromURL(pageURL)
	if domain == "" {
		domain = flags.sessionID
	}

	// 检查是否已有活跃录制
	if existing, err := loadRecordState(flags.sessionID); err == nil && existing.Recording {
		fmt.Fprintf(os.Stderr, "dw-browser record start: recording already active for session %q (started at %s)\n",
			flags.sessionID, existing.StartTime.Format(time.RFC3339))
		fmt.Fprintf(os.Stderr, "  Use 'record stop --session %s' to stop it first.\n", flags.sessionID)
		os.Exit(exitRunErr)
	}

	state := &CLIRecordState{
		SessionID: flags.sessionID,
		Recording: true,
		Domain:    domain,
		StartURL:  pageURL,
		StartTime: time.Now(),
		Steps:     []CLIRecordStep{},
	}

	if err := saveRecordState(state); err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser record start: save state: %v\n", err)
		os.Exit(exitRunErr)
	}

	fmt.Printf("Recording started for %s (session: %s)\n", domain, flags.sessionID)
	fmt.Printf("  start_url: %s\n", pageURL)
	fmt.Printf("  Use 'dw-browser act --session %s ...' to perform actions.\n", flags.sessionID)
	fmt.Printf("  Use 'dw-browser record stop --session %s' to stop and export trace.\n", flags.sessionID)
}

// ============================================================
// § runRecordStop
// ============================================================

// runRecordStop — `dw-browser record stop --session <id>`
// 1. 解析 --session flag
// 2. 读录制状态文件 → 计算 duration → 序列化 trace JSON → stdout
// 3. 删除状态文件（结束录制）
func runRecordStop(args []string) {
	_, flags := parseCommonFlags(args, "record stop")

	if flags.sessionID == "" {
		fmt.Fprintf(os.Stderr, "dw-browser record stop: --session <id> is required\n")
		os.Exit(exitRunErr)
	}

	state, err := loadRecordState(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser record stop: %v\n", err)
		os.Exit(exitRunErr)
	}

	if !state.Recording {
		fmt.Fprintf(os.Stderr, "dw-browser record stop: session %q has a state file but recording=false\n", flags.sessionID)
		os.Exit(exitRunErr)
	}

	trace := buildCLIRecordTrace(state)

	data, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser record stop: marshal trace: %v\n", err)
		os.Exit(exitRunErr)
	}

	// 删除状态文件（结束录制）
	deleteRecordState(flags.sessionID)

	// 输出 trace JSON → stdout（管道友好）
	fmt.Println(string(data))
}

// ============================================================
// § runRecordExport
// ============================================================

// runRecordExport — `dw-browser record export --session <id>`
// 导出当前 trace 快照，不停止录制（录制继续进行）。
// 若需要"导出但不停止"，当前实现支持（直接读文件，不删除）。
func runRecordExport(args []string) {
	_, flags := parseCommonFlags(args, "record export")

	if flags.sessionID == "" {
		fmt.Fprintf(os.Stderr, "dw-browser record export: --session <id> is required\n")
		os.Exit(exitRunErr)
	}

	state, err := loadRecordState(flags.sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser record export: %v\n", err)
		os.Exit(exitRunErr)
	}

	if !state.Recording {
		fmt.Fprintf(os.Stderr, "dw-browser record export: session %q has a state file but recording=false\n", flags.sessionID)
		os.Exit(exitRunErr)
	}

	trace := buildCLIRecordTrace(state)

	data, err := json.MarshalIndent(trace, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser record export: marshal trace: %v\n", err)
		os.Exit(exitRunErr)
	}

	// 不删除状态文件 — 录制继续
	fmt.Println(string(data))
}

// ============================================================
// § 内部工具
// ============================================================

// buildCLIRecordTrace 从 CLIRecordState 构建最终 CLIRecordTrace（计算 duration）。
func buildCLIRecordTrace(state *CLIRecordState) CLIRecordTrace {
	return CLIRecordTrace{
		Domain:     state.Domain,
		StartURL:   state.StartURL,
		StartTime:  state.StartTime,
		DurationMs: time.Since(state.StartTime).Milliseconds(),
		Steps:      state.Steps,
	}
}

// loadSessionForRecord 加载 session 文件（record 命令专用错误消息）。
func loadSessionForRecord(sessionID string) (*sessionInfoForRecord, error) {
	path := filepath.Join(recordSessionsDir, sessionID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("session %q not found — use 'dw-browser open <url> --session %s' first", sessionID, sessionID)
		}
		return nil, fmt.Errorf("read session: %w", err)
	}
	var info sessionInfoForRecord
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("parse session: %w", err)
	}
	return &info, nil
}

// sessionInfoForRecord 仅提取 record 命令需要的 session 字段（避免导入 browser 包循环）。
// 实际使用 browser.SessionInfo（同一个 JSON 结构），此结构仅用于本文件内的轻量解析。
type sessionInfoForRecord struct {
	SessionID string `json:"session_id"`
	PageURL   string `json:"page_url"`
}
