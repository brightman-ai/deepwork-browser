package testing

// Diff — 两次 Observation 的结构化对比
type Diff struct {
	Schema     string         `json:"schema"` // "dw.diff.v1"
	URL        URLDiff        `json:"url"`
	Tabs       TabsDiff       `json:"tabs"`
	Structural StructuralDiff `json:"structural"`
	Telemetry  TelemetryDiff  `json:"telemetry"`
	LatencyMs  int64          `json:"latency_ms"`
}

// URLDiff — URL 变化
type URLDiff struct {
	Before  string `json:"before"`
	After   string `json:"after"`
	Changed bool   `json:"changed"`
}

// TabsDiff — 标签页数量和活跃 tab 变化
type TabsDiff struct {
	BeforeCount   int  `json:"before_count"`
	AfterCount    int  `json:"after_count"`
	ActiveChanged bool `json:"active_changed"`
}

// StructuralDiff — A11y 结构变化
type StructuralDiff struct {
	AddedText   []string `json:"added_text,omitempty"`
	RemovedText []string `json:"removed_text,omitempty"`
	AddedRefs   int      `json:"added_refs"`
	RemovedRefs int      `json:"removed_refs"`
}

// TelemetryDiff — 新增遥测事件
type TelemetryDiff struct {
	NewConsoleErrors   []ConsoleEntry `json:"new_console_errors,omitempty"`
	NewNetworkFailures []NetworkEntry `json:"new_network_failures,omitempty"`
}

// ComputeDiff 对比两次 Observation 的变化。before/after 及其子字段均可为 nil。
func ComputeDiff(before, after *Observation) *Diff {
	d := &Diff{Schema: "dw.diff.v1"}

	// URL diff
	var beforeURL, afterURL string
	if before != nil {
		beforeURL = before.Page.URL
	}
	if after != nil {
		afterURL = after.Page.URL
	}
	d.URL = URLDiff{Before: beforeURL, After: afterURL, Changed: beforeURL != afterURL}

	// Tabs diff
	var beforeCount, afterCount int
	var beforeActiveID, afterActiveID string
	if before != nil && before.Behavior != nil {
		beforeCount = before.Behavior.TabCount
		beforeActiveID = before.Behavior.ActiveTabID
	}
	if after != nil && after.Behavior != nil {
		afterCount = after.Behavior.TabCount
		afterActiveID = after.Behavior.ActiveTabID
		d.LatencyMs = after.Behavior.LatencyMs
	}
	d.Tabs = TabsDiff{
		BeforeCount:   beforeCount,
		AfterCount:    afterCount,
		ActiveChanged: beforeActiveID != afterActiveID,
	}

	// Structural diff
	var beforeText, afterText string
	var beforeRefs, afterRefs int
	if before != nil && before.Structural != nil {
		beforeText = before.Structural.Text
		beforeRefs = before.Structural.RefsCount
	}
	if after != nil && after.Structural != nil {
		afterText = after.Structural.Text
		afterRefs = after.Structural.RefsCount
	}
	added, removed := textDiff(beforeText, afterText)
	refDelta := afterRefs - beforeRefs
	addedRefs, removedRefs := 0, 0
	if refDelta > 0 {
		addedRefs = refDelta
	} else if refDelta < 0 {
		removedRefs = -refDelta
	}
	d.Structural = StructuralDiff{
		AddedText:   added,
		RemovedText: removed,
		AddedRefs:   addedRefs,
		RemovedRefs: removedRefs,
	}

	// Telemetry diff
	var beforeTel, afterTel *TelemetryState
	if before != nil {
		beforeTel = before.Telemetry
	}
	if after != nil {
		afterTel = after.Telemetry
	}
	d.Telemetry = computeTelemetryDiff(beforeTel, afterTel)

	return d
}

// textDiff 返回 after 中新增的行和 before 中删除的行（基于行集合差异）。
func textDiff(before, after string) (added, removed []string) {
	beforeSet := toLineSet(before)
	afterSet := toLineSet(after)

	for line := range afterSet {
		if !beforeSet[line] {
			added = append(added, line)
		}
	}
	for line := range beforeSet {
		if !afterSet[line] {
			removed = append(removed, line)
		}
	}
	return
}

// toLineSet 将多行字符串拆分为非空行的集合。
func toLineSet(s string) map[string]bool {
	set := make(map[string]bool)
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			line := s[start:i]
			if line != "" {
				set[line] = true
			}
			start = i + 1
		}
	}
	return set
}

// computeTelemetryDiff 找出 after 中新出现的 console errors 和 network failures。
func computeTelemetryDiff(before, after *TelemetryState) TelemetryDiff {
	var td TelemetryDiff
	if after == nil {
		return td
	}

	// New console errors: entries in after not present in before
	for _, ae := range after.ConsoleErrors {
		if before == nil || !containsConsoleEntry(before.ConsoleErrors, ae) {
			td.NewConsoleErrors = append(td.NewConsoleErrors, ae)
		}
	}

	// New network failures: entries in after not present in before
	for _, ae := range after.NetworkFailures {
		if before == nil || !containsNetworkEntry(before.NetworkFailures, ae) {
			td.NewNetworkFailures = append(td.NewNetworkFailures, ae)
		}
	}

	return td
}

func containsConsoleEntry(entries []ConsoleEntry, target ConsoleEntry) bool {
	for _, e := range entries {
		if e.Level == target.Level && e.Text == target.Text && e.Source == target.Source {
			return true
		}
	}
	return false
}

func containsNetworkEntry(entries []NetworkEntry, target NetworkEntry) bool {
	for _, e := range entries {
		if e.URL == target.URL && e.Method == target.Method && e.StatusCode == target.StatusCode && e.Error == target.Error {
			return true
		}
	}
	return false
}
