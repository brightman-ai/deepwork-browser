package testing

// Status 表示断言或旅程步骤的执行结果状态。
type Status string

const (
	StatusPass          Status = "PASS"
	StatusFail          Status = "FAIL"
	StatusBug           Status = "BUG"
	StatusVisualSuspect Status = "VISUAL_SUSPECT"
	StatusFlaky         Status = "FLAKY"
	StatusBlocked       Status = "BLOCKED"
)

// AssertionResult — 单条断言的执行结果
type AssertionResult struct {
	Schema     string   `json:"schema"` // "dw.check.v1"
	ID         string   `json:"id"`
	Assertion  string   `json:"assertion"` // 原始表达式
	Using      []string `json:"using"`     // ["structural", "behavior"]
	Status     Status   `json:"status"`
	Confidence float64  `json:"confidence"` // 1.0 for deterministic, <1.0 for VLM
	Reason     string   `json:"reason"`
	Evidence   []string `json:"evidence,omitempty"` // 相关证据文件路径
	// OracleWarning 标注 oracle-class 不一致（调用者 using 与 inferUsing 内在类冲突）。
	// 默认即硬 REJECT（Status=BLOCKED）；DW_BROWSER_ORACLE_WARN_ONLY=1 时降级为仅告警。
	OracleWarning string `json:"oracle_warning,omitempty"`
}

// StepResult — 一步的执行结果
type StepResult struct {
	StepID    string            `json:"step_id"`
	Action    string            `json:"action"`
	Status    Status            `json:"status"`
	Checks    []AssertionResult `json:"checks"`
	LatencyMs int64             `json:"latency_ms"`
	Error     string            `json:"error,omitempty"`
}

// JourneyResult — 完整旅程的执行结果
type JourneyResult struct {
	Schema     string       `json:"schema"` // "dw.journey.v1"
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Status     Status       `json:"status"`
	Steps      []StepResult `json:"steps"`
	Recovery   []StepResult `json:"recovery,omitempty"`
	DurationMs int64        `json:"duration_ms"`
	Evidence   string       `json:"evidence_path"`
}
