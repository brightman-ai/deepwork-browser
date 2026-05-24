package audit

import (
	"encoding/json"
	"fmt"
)

// Report 审计报告。
type Report struct {
	URL       string        `json:"url"`
	Engine    string        `json:"engine"`
	Device    string        `json:"device,omitempty"`
	Timestamp string        `json:"timestamp"`
	Score     int           `json:"score"`
	Summary   Summary       `json:"summary"`
	Checks    []CheckResult `json:"checks"`
}

// Summary 各严重度计数。
type Summary struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
	Pass     int `json:"pass"`
}

// computeScore 从 100 分起扣，critical-15 high-8 medium-3 low-1，最低 0。
func computeScore(s Summary) int {
	score := 100
	score -= s.Critical * 15
	score -= s.High * 8
	score -= s.Medium * 3
	score -= s.Low * 1
	if score < 0 {
		score = 0
	}
	return score
}

// FormatJSON 输出缩进 JSON 格式。
func (r *Report) FormatJSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// FormatCompact 输出 CI 友好单行。
// 示例: "AUDIT 72/100 | critical:2 high:3 medium:1 pass:11 | http://localhost:18082"
func (r *Report) FormatCompact() string {
	return fmt.Sprintf(
		"AUDIT %d/100 | critical:%d high:%d medium:%d low:%d pass:%d | %s",
		r.Score,
		r.Summary.Critical,
		r.Summary.High,
		r.Summary.Medium,
		r.Summary.Low,
		r.Summary.Pass,
		r.URL,
	)
}
