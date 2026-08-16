package browser

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// A locator miss has two completely different causes, and conflating them sent
// agents chasing ghosts: the tool used to answer "当前页面无 button 元素" while
// the DOM held 427 buttons. The engine can only ever speak about the current
// observation, so these diagnostics say exactly that, and split:
//
//	(a) 没有有效 observation —— refs 已作废或还没 observe 过 → 引导 observe
//	(b) observation 有效但该 role 不在可见集 → 报可见集的 role 分布
//	(c) role 在, name 不匹配 → 给最近似候选, 逐条可读, 不再糊成一坨
//
// [BUG-FALSE-NEGATIVE-ROLE-CLAIM]
const (
	locatorCandidateLimit    = 8
	locatorCandidateNameRune = 60
)

// locatorCandidate is one addressable thing the current observation knows
// about. Ref is empty for element-mode candidates, which carry no handle.
type locatorCandidate struct {
	Ref  string
	Role string
	Name string
}

func truncateRunes(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit]) + "…"
}

// locatorSimilarity scores how close a candidate name is to what the caller
// asked for. Deterministic and cheap: exact > prefix > substring > token
// overlap > character overlap.
func locatorSimilarity(want, got string) float64 {
	w := strings.ToLower(strings.Join(strings.Fields(want), " "))
	g := strings.ToLower(strings.Join(strings.Fields(got), " "))
	if w == "" || g == "" {
		return 0
	}
	if w == g {
		return 1000
	}
	lengthRatio := float64(len(w)) / float64(len(g))
	if lengthRatio > 1 {
		lengthRatio = 1 / lengthRatio
	}
	if strings.HasPrefix(g, w) || strings.HasPrefix(w, g) {
		return 800 + 100*lengthRatio
	}
	if strings.Contains(g, w) || strings.Contains(w, g) {
		return 600 + 100*lengthRatio
	}
	wantTokens := strings.Fields(w)
	gotTokens := strings.Fields(g)
	if len(wantTokens) > 0 && len(gotTokens) > 0 {
		gotSet := make(map[string]bool, len(gotTokens))
		for _, tok := range gotTokens {
			gotSet[tok] = true
		}
		shared := 0
		for _, tok := range wantTokens {
			if gotSet[tok] {
				shared++
			}
		}
		if shared > 0 {
			return 300 + 100*float64(shared)/float64(len(wantTokens))
		}
	}
	// Character overlap catches CJK names, which carry no whitespace tokens.
	gotRunes := make(map[rune]bool)
	for _, r := range g {
		gotRunes[r] = true
	}
	shared := 0
	total := 0
	for _, r := range w {
		total++
		if gotRunes[r] {
			shared++
		}
	}
	if total == 0 || shared == 0 {
		return 0
	}
	return 100 * float64(shared) / float64(total)
}

// rankLocatorCandidates returns the closest candidates to want, best first,
// deduplicated by role+name and capped so a miss cannot dump the whole page.
func rankLocatorCandidates(candidates []locatorCandidate, want string, limit int) []locatorCandidate {
	seen := make(map[string]bool, len(candidates))
	unique := make([]locatorCandidate, 0, len(candidates))
	for _, c := range candidates {
		key := c.Role + "\x00" + c.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, c)
	}
	scores := make(map[int]float64, len(unique))
	for i, c := range unique {
		scores[i] = locatorSimilarity(want, c.Name)
	}
	order := make([]int, len(unique))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return scores[order[a]] > scores[order[b]]
	})
	if limit > 0 && len(order) > limit {
		order = order[:limit]
	}
	out := make([]locatorCandidate, 0, len(order))
	for _, idx := range order {
		out = append(out, unique[idx])
	}
	return out
}

// formatLocatorCandidates renders candidates one per line so an agent can read
// (and copy) them, instead of the previous single unsplittable blob.
func formatLocatorCandidates(candidates []locatorCandidate) string {
	lines := make([]string, 0, len(candidates))
	for _, c := range candidates {
		name := truncateRunes(c.Name, locatorCandidateNameRune)
		switch {
		case c.Ref != "" && c.Role != "":
			lines = append(lines, fmt.Sprintf("  %s %s '%s'", c.Ref, c.Role, name))
		case c.Ref != "":
			lines = append(lines, fmt.Sprintf("  %s '%s'", c.Ref, name))
		case c.Role != "":
			lines = append(lines, fmt.Sprintf("  %s '%s'", c.Role, name))
		default:
			lines = append(lines, "  '"+name+"'")
		}
	}
	return strings.Join(lines, "\n")
}

func formatRoleHistogram(counts map[string]int) string {
	if len(counts) == 0 {
		return "(空)"
	}
	roles := make([]string, 0, len(counts))
	for role := range counts {
		roles = append(roles, role)
	}
	sort.Slice(roles, func(i, j int) bool {
		if counts[roles[i]] != counts[roles[j]] {
			return counts[roles[i]] > counts[roles[j]]
		}
		return roles[i] < roles[j]
	})
	parts := make([]string, 0, len(roles))
	for _, role := range roles {
		parts = append(parts, fmt.Sprintf("%s %d", role, counts[role]))
	}
	return strings.Join(parts, ", ")
}

// errNoObservation is the honest reading of "the ref table is empty": this
// process holds no observation, so it knows nothing about the page — which is
// NOT the same as the page holding no such element.
func errNoObservation(role, name, setLabel string) error {
	target := role
	if name != "" {
		target = fmt.Sprintf("%s:'%s'", role, truncateRunes(name, locatorCandidateNameRune))
	}
	return fmt.Errorf("%w: 元素 %s 未找到: 当前没有有效的%s —— 尚未 observe, 或上一步动作后 refs 已作废。\n"+
		"先 `dw-browser observe --id <id>` 再定位。本条只说明本工具此刻没有观察结果, 不代表页面上没有 %s 元素",
		ErrRefNotFound, target, setLabel, role)
}

// errRoleAbsentFromObservation is scoped to the observation and reports what
// the observation actually holds, so the agent can re-aim instead of concluding
// the page is broken.
func errRoleAbsentFromObservation(role, name, setLabel string, size int, histogram map[string]int) error {
	target := role
	if name != "" {
		target = fmt.Sprintf("%s:'%s'", role, truncateRunes(name, locatorCandidateNameRune))
	}
	return fmt.Errorf("%w: 元素 %s 未找到: 最近一次 observe 的%s(%d 个)里没有 %s 元素。\n"+
		"该%s现有 role 分布: %s\n"+
		"页面上可能存在但当前不可及(屏外/被浮层遮挡/未渲染) —— 滚动或关闭浮层后重新 observe",
		ErrRefNotFound, target, setLabel, size, role, setLabel, formatRoleHistogram(histogram))
}

// errNameNoMatch lists the nearest candidates instead of dumping every name.
func errNameNoMatch(role, name, setLabel string, candidates []locatorCandidate) error {
	ranked := rankLocatorCandidates(candidates, name, locatorCandidateLimit)
	more := ""
	if len(candidates) > len(ranked) {
		more = fmt.Sprintf("\n(%s共 %d 个 %s, 上面是最接近的 %d 个)", setLabel, len(candidates), role, len(ranked))
	}
	return fmt.Errorf("%w: 元素 %s:'%s' 未找到: %s里有 %s 元素, 但没有一个的 name 匹配。最近似候选:\n%s%s",
		ErrRefNotFound, role, truncateRunes(name, locatorCandidateNameRune), setLabel, role,
		formatLocatorCandidates(ranked), more)
}
