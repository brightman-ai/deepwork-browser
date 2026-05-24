// explore.go — Explore 引擎：自动探索页面并生成候选 baseline
package testing

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// CandidateBaseline 是 explore 产出的候选基线。
// Human review 之前 review_status 始终为 "candidate"。
type CandidateBaseline struct {
	Schema       string          `yaml:"schema" json:"schema"`               // "dw.baseline.v1"
	ReviewStatus string          `yaml:"review_status" json:"review_status"` // "candidate"
	ID           string          `yaml:"id" json:"id"`
	URL          string          `yaml:"url" json:"url"`
	Regions      []RegionDef     `yaml:"regions" json:"regions"`
	Invariants   []AssertionSpec `yaml:"invariants" json:"invariants"`
	Metadata     ExploreMetadata `yaml:"metadata" json:"metadata"`
}

// RegionDef 描述页面中的一个语义区域。
type RegionDef struct {
	ID      string `yaml:"id" json:"id"`
	Locator string `yaml:"locator" json:"locator"`
	Role    string `yaml:"role" json:"role"`
}

// ExploreMetadata 记录探索过程的元信息。
type ExploreMetadata struct {
	URL          string    `yaml:"url" json:"url"`
	Timestamp    time.Time `yaml:"timestamp" json:"timestamp"`
	ActionsTried int       `yaml:"actions_tried" json:"actions_tried"`
	Duration     string    `yaml:"duration" json:"duration"`
}

// Explorer 自动探索页面生成候选 baseline。
type Explorer struct {
	executor ActionExecutor
	engine   AssertionEngine
}

// NewExplorer 创建 Explorer。
func NewExplorer(executor ActionExecutor) *Explorer {
	return &Explorer{executor: executor}
}

// LearnBaseline 执行 4 步探索流程，生成候选 baseline。
//
// 流程：
//  1. 采集稳定初态 observation
//  2. 识别 regions（landmark roles）
//  3. 探索低风险动作 + 收敛 stable refs
//  4. 从 stable refs 生成 invariant assertions
func (e *Explorer) LearnBaseline(ctx context.Context, goal string) (*CandidateBaseline, error) {
	start := time.Now()
	cb := &CandidateBaseline{
		Schema:       "dw.baseline.v1",
		ReviewStatus: "candidate",
		ID:           fmt.Sprintf("explore-%d", time.Now().Unix()),
	}

	// Step 1: 采集稳定初态 observation
	initObs := e.observe(ctx)
	if initObs == nil {
		return nil, fmt.Errorf("cannot observe initial state")
	}
	cb.URL = initObs.Page.URL

	// Step 2: 识别 regions（通过 A11y refs 中的 landmark roles）
	cb.Regions = identifyRegions(initObs)

	// Step 3: 探索 + 发现不变量
	// 3a. 记录初态中始终存在的元素（作为 invariant 候选）
	stableRefs := findStableElements(initObs)

	// 3b. 执行低风险动作（click navigation/tab/button，back 恢复）
	actionsTried := 0
	for _, action := range lowRiskActions(initObs) {
		if err := e.executor.Execute(ctx, action); err != nil {
			// 执行失败不中断，继续下一个
			continue
		}
		time.Sleep(500 * time.Millisecond) // 等 settle

		afterObs := e.observe(ctx)
		if afterObs == nil {
			// 恢复并继续
			_ = e.executor.Execute(ctx, "back")
			time.Sleep(300 * time.Millisecond)
			continue
		}
		actionsTried++

		// 检查哪些初态元素仍然存在 → 更强的 invariant 候选
		stableRefs = intersect(stableRefs, afterObs)

		// 尝试恢复（back）
		_ = e.executor.Execute(ctx, "back")
		time.Sleep(300 * time.Millisecond)
	}

	// Step 4: 从 stable refs 生成 invariant assertions
	cb.Invariants = generateInvariants(stableRefs, initObs)

	// 始终添加的通用 invariants
	cb.Invariants = append(cb.Invariants,
		AssertionSpec{
			ID:     "no-console-error",
			Assert: "console_errors_count == 0",
			Using:  []string{"telemetry"},
		},
		AssertionSpec{
			ID:     "no-network-failure",
			Assert: "network_failures_count == 0",
			Using:  []string{"telemetry"},
		},
	)

	cb.Metadata = ExploreMetadata{
		URL:          cb.URL,
		Timestamp:    time.Now(),
		ActionsTried: actionsTried,
		Duration:     time.Since(start).String(),
	}

	return cb, nil
}

// observe 采集当前页面 observation（最佳努力，失败返回 nil）。
func (e *Explorer) observe(ctx context.Context) *Observation {
	snap, _ := e.executor.Snapshot(ctx)
	behavior, _ := e.executor.GetSessionState(ctx)
	telemetry, _ := e.executor.GetTelemetry(ctx)
	return BuildObservation("explore", snap, nil, "", behavior, telemetry)
}

// SaveCandidate 将候选基线序列化为 YAML 写入 path。
func SaveCandidate(cb *CandidateBaseline, path string) error {
	data, err := yaml.Marshal(cb)
	if err != nil {
		return fmt.Errorf("marshal candidate baseline: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// ============================================================
// § 内部辅助函数
// ============================================================

// landmarkRoles 是 ARIA landmark roles 列表，用于 region 识别。
var landmarkRoles = map[string]bool{
	"main":          true,
	"navigation":    true,
	"complementary": true,
	"banner":        true,
	"contentinfo":   true,
	"region":        true,
	"search":        true,
	"form":          true,
}

// identifyRegions 从 A11y tree 中识别页面语义区域。
// 查找 landmark roles: main, navigation, complementary, banner, contentinfo 等。
func identifyRegions(obs *Observation) []RegionDef {
	if obs == nil || obs.Structural == nil {
		return []RegionDef{}
	}
	regions := make([]RegionDef, 0)
	seen := make(map[string]bool)

	for _, ref := range obs.Structural.Refs {
		role := strings.ToLower(ref.Role)
		if !landmarkRoles[role] {
			continue
		}
		// 生成可读 ID：role + name（截断）
		regionID := role
		if ref.Name != "" {
			safeName := sanitizeID(ref.Name)
			if safeName != "" {
				regionID = role + "-" + safeName
			}
		}
		// 去重
		if seen[regionID] {
			continue
		}
		seen[regionID] = true

		locator := buildLocator(ref)
		regions = append(regions, RegionDef{
			ID:      regionID,
			Locator: locator,
			Role:    role,
		})
	}
	return regions
}

// stableRoles 是值得追踪为 invariant 候选的元素 role。
var stableRoles = map[string]bool{
	"button":     true,
	"link":       true,
	"heading":    true,
	"navigation": true,
	"tab":        true,
	"menuitem":   true,
	"listitem":   true,
}

// findStableElements 提取初态中的关键元素作为 invariant 候选。
// 关注：buttons, links, headings, navigation items。
func findStableElements(obs *Observation) []RefSummary {
	if obs == nil || obs.Structural == nil {
		return []RefSummary{}
	}
	stable := make([]RefSummary, 0)
	for _, ref := range obs.Structural.Refs {
		role := strings.ToLower(ref.Role)
		if stableRoles[role] && ref.Name != "" {
			stable = append(stable, ref)
		}
	}
	return stable
}

// navigationRoles 是低风险探索优先点击的元素 role。
var navigationRoles = map[string]bool{
	"navigation": true,
	"tab":        true,
	"link":       true,
	"button":     true,
}

// lowRiskActions 生成低风险探索动作列表。
// 从 A11y refs 中提取可点击元素，优先 navigation/tab/link/button。
// 保守原则：不 fill 表单，不执行 delete/submit 类操作。
func lowRiskActions(obs *Observation) []string {
	if obs == nil || obs.Structural == nil {
		return []string{}
	}

	const maxActions = 8 // 限制探索动作数，避免过长

	// 先收集 navigation/tab/link，再收集 button
	var priority []RefSummary
	var fallback []RefSummary

	for _, ref := range obs.Structural.Refs {
		role := strings.ToLower(ref.Role)
		if !navigationRoles[role] || ref.Name == "" {
			continue
		}
		if isDangerousLabel(ref.Name) {
			continue
		}
		if role == "navigation" || role == "tab" || role == "link" {
			priority = append(priority, ref)
		} else {
			fallback = append(fallback, ref)
		}
	}

	combined := append(priority, fallback...)
	if len(combined) > maxActions {
		combined = combined[:maxActions]
	}

	actions := make([]string, 0, len(combined))
	for _, ref := range combined {
		actions = append(actions, buildClickAction(ref))
	}
	return actions
}

// intersect 保留在两个 observation 中都存在的 refs（按 Name + Role 匹配）。
func intersect(stableRefs []RefSummary, obs *Observation) []RefSummary {
	if obs == nil || obs.Structural == nil {
		return stableRefs
	}

	// 构建 after 的快速查找集合（role+name 组合）
	afterSet := make(map[string]bool, len(obs.Structural.Refs))
	for _, ref := range obs.Structural.Refs {
		key := strings.ToLower(ref.Role) + ":" + ref.Name
		afterSet[key] = true
	}

	result := stableRefs[:0:len(stableRefs)]
	for _, ref := range stableRefs {
		key := strings.ToLower(ref.Role) + ":" + ref.Name
		if afterSet[key] {
			result = append(result, ref)
		}
	}
	return result
}

// generateInvariants 从 stable refs 生成人可读的断言规格。
func generateInvariants(stableRefs []RefSummary, initObs *Observation) []AssertionSpec {
	var specs []AssertionSpec

	// URL 不变量（若能确定）
	if initObs != nil && initObs.Page.URL != "" {
		specs = append(specs, AssertionSpec{
			ID:     "url-stable",
			Assert: fmt.Sprintf("url_matches('%s')", initObs.Page.URL),
			Using:  []string{"behavior"},
		})
	}

	// 每个 stable ref 生成一条 exists 断言
	for _, ref := range stableRefs {
		if ref.Name == "" {
			continue
		}
		id := sanitizeID(strings.ToLower(ref.Role) + "-" + ref.Name)
		locator := buildLocator(ref)
		if locator == "" {
			continue
		}
		specs = append(specs, AssertionSpec{
			ID:     id,
			Assert: fmt.Sprintf("exists(%s)", locator),
			Using:  []string{"structural"},
		})
	}

	return specs
}

// ============================================================
// § 字符串工具
// ============================================================

// buildLocator 从 RefSummary 构建断言定位器。
// 优先 testid，其次 role+name，最后 ref。
func buildLocator(ref RefSummary) string {
	if ref.TestID != "" {
		return fmt.Sprintf("testid='%s'", ref.TestID)
	}
	if ref.Name != "" && ref.Role != "" {
		return fmt.Sprintf("role='%s' name='%s'", strings.ToLower(ref.Role), ref.Name)
	}
	if ref.Ref != "" {
		return fmt.Sprintf("ref='%s'", ref.Ref)
	}
	return ""
}

// buildClickAction 生成点击动作字符串（dw-browser act 语法）。
func buildClickAction(ref RefSummary) string {
	if ref.Name != "" && ref.Role != "" {
		return fmt.Sprintf("click %s:'%s'", strings.ToLower(ref.Role), ref.Name)
	}
	if ref.Ref != "" {
		return fmt.Sprintf("click @%s", ref.Ref)
	}
	return ""
}

// sanitizeID 将字符串转为合法 YAML ID（小写，非字母数字替换为 -，去重 -，截断 32）。
func sanitizeID(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash {
			b.WriteRune('-')
			prevDash = true
		}
	}
	result := strings.Trim(b.String(), "-")
	if len(result) > 32 {
		result = result[:32]
	}
	return result
}

// dangerousLabels 是不应探索点击的元素 name 关键词。
var dangerousKeywords = []string{
	"delete", "remove", "logout", "sign out", "log out",
	"clear", "reset", "cancel", "discard", "destroy",
	"submit", "confirm", "send", "buy", "purchase", "pay",
}

// isDangerousLabel 检查元素 name 是否包含危险操作关键词。
func isDangerousLabel(name string) bool {
	lower := strings.ToLower(name)
	for _, kw := range dangerousKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
