package audit

// Severity 检测严重度。
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
)

// Check 单个检测规则。
type Check struct {
	ID          string
	Category    string         // "compat" | "a11y" | "layout" | "perf"
	Tags        []string       // ["touch", "viewport", "ios"]
	Severity    Severity
	Description string
	Script      string         // JS 脚本内容，执行后返回 CheckResult JSON
	Params      map[string]any // 默认参数，可被 AuditContext 覆盖
}

// CheckResult 单个检测结果。
type CheckResult struct {
	ID         string      `json:"id"`
	Category   string      `json:"category"`
	Status     string      `json:"status"` // "pass" | "fail" | "error"
	Severity   Severity    `json:"severity"`
	Message    string      `json:"message"`
	Violations []Violation `json:"violations,omitempty"`
}

// Violation 单个违规项。
type Violation struct {
	Selector string         `json:"selector"`
	Role     string         `json:"role,omitempty"`
	Name     string         `json:"name,omitempty"`
	TestID   string         `json:"testid,omitempty"`
	Actual   map[string]any `json:"actual,omitempty"`
	Expected map[string]any `json:"expected,omitempty"`
	Fix      string         `json:"fix,omitempty"`
}

// Registry check 注册表（线程不安全，注册期在程序初始化时完成）。
type Registry struct {
	checks []Check
	byID   map[string]*Check
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{
		byID: make(map[string]*Check),
	}
}

// Register 注册一个 check。ID 重复时覆盖。
func (r *Registry) Register(check Check) {
	r.checks = append(r.checks, check)
	r.byID[check.ID] = &r.checks[len(r.checks)-1]
}

// ByCategory 返回指定 category 的所有 checks（副本切片）。
func (r *Registry) ByCategory(category string) []Check {
	var out []Check
	for _, c := range r.checks {
		if c.Category == category {
			out = append(out, c)
		}
	}
	return out
}

// ByTag 返回含指定 tag 的所有 checks（副本切片）。
func (r *Registry) ByTag(tag string) []Check {
	var out []Check
	for _, c := range r.checks {
		for _, t := range c.Tags {
			if t == tag {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

// ByID 按 ID 查找 check，不存在返回 nil。
func (r *Registry) ByID(id string) *Check {
	c, ok := r.byID[id]
	if !ok {
		return nil
	}
	return c
}

// All 返回所有已注册 checks（副本切片）。
func (r *Registry) All() []Check {
	out := make([]Check, len(r.checks))
	copy(out, r.checks)
	return out
}
