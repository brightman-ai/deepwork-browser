package testing

import "github.com/brightman-ai/deepwork-browser/browser"

// Status 表示断言或旅程步骤的执行结果状态。
type Status string

const (
	StatusPass          Status = "PASS"
	StatusDelivered     Status = "DELIVERED"
	StatusFail          Status = "FAIL"
	StatusBug           Status = "BUG"
	StatusVisualSuspect Status = "VISUAL_SUSPECT"
	StatusFlaky         Status = "FLAKY"
	StatusBlocked       Status = "BLOCKED"
)

// AssertionResult — 单条断言的执行结果
type AssertionResult struct {
	Schema         string   `json:"schema"` // "dw.check.v1"
	ID             string   `json:"id"`
	Assertion      string   `json:"assertion"` // 原始表达式
	Required       bool     `json:"required"`
	RequestedUsing []string `json:"requested_using,omitempty"`
	Using          []string `json:"using"` // ["structural", "behavior"]
	Status         Status   `json:"status"`
	Confidence     float64  `json:"confidence"` // hard deterministic confidence; VLM confidence is in VisualAdvice
	Reason         string   `json:"reason"`
	Evidence       []string `json:"evidence,omitempty"` // 相关证据文件路径
	// OracleWarning 标注 oracle-class 不一致（调用者 using 与 inferUsing 内在类冲突）。
	// Oracle-class mismatch is a hard BLOCKED rejection; no downgrade switch.
	OracleWarning     string        `json:"oracle_warning,omitempty"`
	VisualAdvice      *VisionResult `json:"visual_advice,omitempty"`
	VisualAdviceError string        `json:"visual_advice_error,omitempty"`
}

// StepResult — 一步的执行结果
type StepResult struct {
	StepID         string                        `json:"step_id"`
	Action         *RecordedAction               `json:"action,omitempty"`
	Outcome        *browser.Outcome              `json:"outcome,omitempty"`
	ActionFidelity *browser.ActionFidelityReport `json:"action_fidelity,omitempty"`
	Status         Status                        `json:"status"`
	Checks         []AssertionResult             `json:"checks"`
	ReferenceDiffs []ReferenceDiff               `json:"reference_diffs,omitempty"`
	LatencyMs      int64                         `json:"latency_ms"`
	Error          string                        `json:"error,omitempty"`
}

// JourneyResult — 完整旅程的执行结果
type JourneyResult struct {
	Schema             string       `json:"schema"` // "dw.journey.v2"
	ID                 string       `json:"id"`
	Name               string       `json:"name"`
	Status             Status       `json:"status"`
	Steps              []StepResult `json:"steps"`
	Recovery           []StepResult `json:"recovery,omitempty"`
	DurationMs         int64        `json:"duration_ms"`
	Evidence           string       `json:"evidence_path"`
	Assurance          string       `json:"assurance"`
	VerifiedChecks     []string     `json:"verified_checks,omitempty"`
	ExecutionOnlySteps []string     `json:"execution_only_steps,omitempty"`
}

// CheckExitCode permits success only when every required check passed.
func CheckExitCode(results []AssertionResult) int {
	if len(results) == 0 {
		return 2
	}
	code := 0
	for _, r := range results {
		switch r.Status {
		case StatusPass:
		case StatusFail:
			if code == 0 {
				code = 1
			}
		default:
			code = 2
		}
	}
	return code
}

// RecordedAction is an evidence envelope; a redacted secret is never a replay request.
type RecordedAction struct {
	Kind       string          `json:"kind"`
	Target     browser.Target  `json:"target,omitempty"`
	Request    *browser.Action `json:"request,omitempty"`
	Sensitive  bool            `json:"sensitive,omitempty"`
	Redacted   bool            `json:"redacted,omitempty"`
	Replayable bool            `json:"replayable"`
}

// ReferenceDiff delivers comparisons; existence does not grant accepted-baseline identity.
type ReferenceDiff struct {
	ReferencePath string `json:"reference_path"`
	ObservationID string `json:"reference_observation_id"`
	Diff          *Diff  `json:"diff"`
	Acceptance    string `json:"acceptance"`
}
