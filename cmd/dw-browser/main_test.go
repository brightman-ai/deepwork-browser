package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/brightman-ai/deepwork-browser/browser"
)

// ============================================================
// § isPointerAction 单元测试 [BUG-FIX: session new-tab tracking]
// ============================================================

// TestIsPointerAction 验证 isPointerAction 正确识别会触发新 tab 的 pointer/touch 操作。
//
// 根因: runActSession 在 click 触发 target=_blank 新标签后未更新 session TargetID，
// 导致后续 snap/act 仍连接到旧标签。
// 修复: click 前快照已知 targets，click 后扫描新 target → 更新 session 文件。
func TestIsPointerAction(t *testing.T) {
	tests := []struct {
		action string
		want   bool
	}{
		// pointer/touch 操作 → 需要 new-tab 检测
		{"click @r7", true},
		{"clickat #browser-liveview 92% 8%", true},
		{"click button:'确认'", true},
		{"click link:'Github'", true},
		{"click #btn", true},
		{"tap button:'接管'", true},
		{"tapat #browser-liveview 92% 8%", true},
		{"CLICK @r3", true}, // case-insensitive
		{"Click @r3", true}, // case-insensitive
		{"click", true},     // bare click

		// 非 pointer/touch 操作 → 不触发 new-tab 检测
		{"fill @r7 'hello'", false},
		{"type textbox:'name' 'value'", false},
		{"press Enter", false},
		{"scroll down", false},
		{"hover @r3", false},
		{"back", false},
		{"forward", false},
		{"focus @r5", false},
		{"", false},    // empty string
		{"   ", false}, // whitespace only
	}

	for _, tc := range tests {
		got := isPointerAction(tc.action)
		if got != tc.want {
			t.Errorf("isPointerAction(%q) = %v, want %v", tc.action, got, tc.want)
		}
	}
}

func TestActionFidelityOutputAndSessionFocusReconcile(t *testing.T) {
	output := map[string]interface{}{"success": true}
	report := browser.ActionFidelityReport{
		Fidelity:       browser.InteractionFidelityStrictHuman,
		Synthetic:      true,
		SyntheticNote:  "synthetic marker",
		HumanPath:      []string{"mouse_click", "active_element_verified"},
		AimSource:      browser.AimSourceContentQuadCentroid,
		HitCoverage:    "4/5",
		FocusUpdated:   true,
		FocusedBackend: 42,
	}
	injectActionFidelity(output, report)
	if output["fidelity"] != browser.InteractionFidelityStrictHuman || output["synthetic"] != true ||
		output["synthetic_note"] != "synthetic marker" || output["aim_source"] != browser.AimSourceContentQuadCentroid ||
		output["hit_coverage"] != "4/5" {
		t.Fatalf("fidelity output=%+v", output)
	}

	info := &browser.SessionInfo{PageURL: "https://example.test/", SnapEpoch: 7}
	reconcileSessionHumanFocus(info, report, false)
	if info.HumanFocusBackendNodeID != 42 || info.HumanFocusPageURL != info.PageURL || info.HumanFocusEpoch != 7 {
		t.Fatalf("persisted human focus=%+v", info)
	}
	reconcileSessionHumanFocus(info, browser.ActionFidelityReport{}, true)
	if info.HumanFocusBackendNodeID != 0 || info.HumanFocusPageURL != "" || info.HumanFocusEpoch != 0 {
		t.Fatalf("navigation retained human focus=%+v", info)
	}
}

func TestObserveHitAuditFlagIsAdditiveAndRemovedBeforeCommonParsing(t *testing.T) {
	clean, want, scope, err := stripObserveHitAuditFlag([]string{"--id", "s1", "--hit-audit", "--health", "--hit-audit"})
	if err != nil || !want || scope != hitAuditScopeVisible || strings.Join(clean, " ") != "--id s1 --health" {
		t.Fatalf("stripObserveHitAuditFlag=(%q,%t,%q,%v)", clean, want, scope, err)
	}
	clean, want, scope, err = stripObserveHitAuditFlag([]string{"--id", "s1"})
	if err != nil || want || scope != hitAuditScopeVisible || strings.Join(clean, " ") != "--id s1" {
		t.Fatalf("flag unexpectedly enabled=(%q,%t,%q,%v)", clean, want, scope, err)
	}
}

func TestSelectSessionTargetIDPrefersExactURLOverBlankTabs(t *testing.T) {
	targets := []map[string]interface{}{
		{
			"type":                 "page",
			"url":                  browser.ChromeInitialPageURL,
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/blank-1",
		},
		{
			"type":                 "page",
			"url":                  "http://127.0.0.1:8077/studio",
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/studio-1",
		},
	}

	got := selectSessionTargetID(targets, "http://127.0.0.1:8077/studio", "fallback-1")
	if got != "studio-1" {
		t.Fatalf("selectSessionTargetID() = %q, want studio-1", got)
	}
}

func TestSelectSessionTargetIDFallsBackToInitialTargetBeforeBlankTab(t *testing.T) {
	targets := []map[string]interface{}{
		{
			"type":                 "page",
			"url":                  browser.ChromeInitialPageURL,
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/blank-1",
		},
		{
			"type":                 "page",
			"url":                  browser.ChromeInitialPageURL,
			"webSocketDebuggerUrl": "ws://127.0.0.1/devtools/page/blank-2",
		},
	}

	got := selectSessionTargetID(targets, "http://127.0.0.1:8077/studio", "initial-123")
	if got != "initial-123" {
		t.Fatalf("selectSessionTargetID() = %q, want initial-123", got)
	}
}

func TestIsTransientBrowserStepError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "context canceled", err: errors.New("context canceled"), want: true},
		{name: "target closed", err: errors.New("target closed"), want: true},
		{name: "session closed", err: errors.New("session closed"), want: true},
		{name: "active target missing", err: errors.New("not attached to an active page target"), want: true},
		{name: "selector missing", err: errors.New("selector not found"), want: false},
	}

	for _, tc := range tests {
		if got := isTransientBrowserStepError(tc.err); got != tc.want {
			t.Fatalf("%s: isTransientBrowserStepError() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNeedsRefRefresh(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "backend node missing", err: errors.New("backend node not found: context canceled"), want: true},
		{name: "ref node missing", err: errors.New("node not found for ref \"e42\""), want: true},
		{name: "stale ref", err: errors.New("ref is stale"), want: true},
		{name: "selector missing", err: errors.New("selector not found"), want: true},
		{name: "plain error", err: errors.New("permission denied"), want: false},
	}

	for _, tc := range tests {
		if got := needsRefRefresh(tc.err); got != tc.want {
			t.Fatalf("%s: needsRefRefresh() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsNLGoalDistinguishesStructuredActions(t *testing.T) {
	tests := []struct {
		action string
		want   bool
	}{
		{action: "click #submit", want: false},
		{action: "fill textbox:'Search' 'deepwork'", want: false},
		{action: "press Enter", want: false},
		{action: "wait", want: false},
		{action: "noop", want: false},
		{action: "none", want: false},
		{action: "back", want: false},
		{action: "Open settings and inspect provider status", want: true},
		{action: "在浏览器中打开设置并检查 Provider", want: true},
	}

	for _, tc := range tests {
		if got := isNLGoal(tc.action); got != tc.want {
			t.Fatalf("isNLGoal(%q) = %v, want %v", tc.action, got, tc.want)
		}
	}
}

func TestActionMovesWitnessViewport(t *testing.T) {
	for _, action := range []string{"scroll down", "scroll @r2 up 3", "scrollto @r9", "scrollinto #panel"} {
		if !actionMovesWitnessViewport(action) {
			t.Errorf("actionMovesWitnessViewport(%q)=false, want true", action)
		}
	}
	for _, action := range []string{"click @r1", "hover @r2", "fill @r3 hi"} {
		if actionMovesWitnessViewport(action) {
			t.Errorf("actionMovesWitnessViewport(%q)=true, want false", action)
		}
	}
}

func TestScenarioUsesSeeToClickForEveryAgentScenario(t *testing.T) {
	for _, scenario := range []string{"app-test-explore", "app-test-baseline", "webvisit"} {
		if !scenarioUsesSeeToClick(scenario) {
			t.Fatalf("scenarioUsesSeeToClick(%q)=false", scenario)
		}
	}
	for _, scenario := range []string{"", "unknown"} {
		if scenarioUsesSeeToClick(scenario) {
			t.Fatalf("scenarioUsesSeeToClick(%q)=true", scenario)
		}
	}
}

func TestObserveAllListingIsRefLessAndKeepsSessionCapabilities(t *testing.T) {
	snap := &browser.Snapshot{
		SeeToClick:                true,
		URL:                       "http://127.0.0.1/page",
		DocumentInteractableCount: 2,
		Refs: []browser.ElementRef{{
			Ref:               "@r1",
			BackendNodeID:     11,
			Role:              "button",
			NameFull:          "Visible",
			VisibilityKnown:   true,
			VisibleInViewport: true,
		}},
		Census: []browser.CensusEntry{
			{Role: "button", Name: "Visible"},
			{Role: "button", Name: "Below fold"},
		},
	}

	listing, truncated := observeListing(snap, true, 20, 4096)
	if truncated {
		t.Fatal("census unexpectedly marked truncated")
	}
	if _, ok := listing["elements"]; ok {
		t.Fatalf("--all listing exposed actionable elements: %#v", listing)
	}
	if actionable, ok := listing["actionable"].(bool); !ok || actionable {
		t.Fatalf("actionable=%#v, want false", listing["actionable"])
	}
	encoded, err := json.Marshal(listing)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, forbidden := range []string{"\"ref\"", "@r1", "backend_node_id", "testid"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("--all JSON leaked %q: %s", forbidden, encoded)
		}
	}
	// [BUG-CENSUS-REVOKES-REFS] census 是只读普查: 它不铸新 @rN, 但也绝不
	// 撤销上一次 observe 已经发出的句柄。
	info := &browser.SessionInfo{
		PageURL: "http://127.0.0.1/page",
		Refs:    []browser.SessionRef{{Ref: "@r1", BackendNodeID: 11, Visible: true, Observed: true}},
	}
	kept := sessionRefsForObservation(info, snap, true)
	if len(kept) != 1 || kept[0].Ref != "@r1" || !kept[0].Observed {
		t.Fatalf("--all revoked existing refs: %+v", kept)
	}
	// 跨文档边界仍必须撤销: 上一页的句柄在新页上是死的。
	navigated := sessionRefsForObservation(info, &browser.Snapshot{SeeToClick: true, URL: "http://127.0.0.1/other"}, true)
	if len(navigated) != 0 {
		t.Fatalf("--all kept refs across a document boundary: %+v", navigated)
	}
}

func TestSessionRefsForStrictObservationDropsInvisibleEntries(t *testing.T) {
	snap := &browser.Snapshot{
		SeeToClick: true,
		Refs: []browser.ElementRef{
			{Ref: "@r1", BackendNodeID: 1, BBox: browser.Rect{X: 10, Y: 20, Width: 30, Height: 40}, VisibilityKnown: true, VisibleInViewport: true},
			{Ref: "@r2", BackendNodeID: 2, VisibilityKnown: true, VisibleInViewport: false},
			{Ref: "@r3", BackendNodeID: 3, VisibilityKnown: false, VisibleInViewport: true},
		},
	}
	refs := sessionRefsForObservation(&browser.SessionInfo{}, snap, false)
	if len(refs) != 1 || refs[0].Ref != "@r1" {
		t.Fatalf("persisted refs=%+v, want only @r1", refs)
	}
	if !refs[0].Observed || !refs[0].Visible || refs[0].BBox == nil {
		t.Fatalf("visible observation authority not persisted: %+v", refs[0])
	}
	openRefs := browser.SessionRefsFromSnapshot(snap, false)
	if len(openRefs) != 1 || openRefs[0].Observed {
		t.Fatalf("open refs must not grant observation authority: %+v", openRefs)
	}
	if got := browser.SessionRefsFromSnapshot(&browser.Snapshot{SeeToClick: true}, false); len(got) != 0 {
		t.Fatalf("empty snapshot retained stale refs: %+v", got)
	}
}

func TestSamePageFillThenClickKeepsObservedRefAuthority(t *testing.T) {
	const pageURL = "http://127.0.0.1/form"
	observed := &browser.Snapshot{
		URL:        pageURL,
		SeeToClick: true,
		Refs: []browser.ElementRef{
			{Ref: "@r1", BackendNodeID: 11, Role: "textbox", NameFull: "Name", VisibilityKnown: true, VisibleInViewport: true},
			{Ref: "@r2", BackendNodeID: 12, Role: "button", NameFull: "Submit", VisibilityKnown: true, VisibleInViewport: true},
		},
	}
	sessionInfo := &browser.SessionInfo{
		PageURL: pageURL,
		Refs:    browser.SessionRefsFromSnapshot(observed, true),
	}

	// A successful fill returns a same-document snapshot. It must not replace
	// the explicit observation with an internal, unobserved ref table.
	if navigated := applyPostActionSnapshot(sessionInfo, &browser.Snapshot{URL: pageURL}); navigated {
		t.Fatal("same-page fill was classified as navigation")
	}
	if len(sessionInfo.Refs) != 2 || sessionInfo.Refs[1].Ref != "@r2" || !sessionInfo.Refs[1].Observed {
		t.Fatalf("fill revoked the observed click ref: %+v", sessionInfo.Refs)
	}
}

func TestNavigationAfterActRevokesOldRefAuthority(t *testing.T) {
	sessionInfo := &browser.SessionInfo{
		PageURL: "http://127.0.0.1/form",
		Refs: []browser.SessionRef{{
			Ref:      "@r2",
			Visible:  true,
			Observed: true,
		}},
	}

	if navigated := applyPostActionSnapshot(sessionInfo, &browser.Snapshot{URL: "http://127.0.0.1/done"}); !navigated {
		t.Fatal("URL change was not classified as navigation")
	}
	if len(sessionInfo.Refs) != 0 {
		t.Fatalf("navigation retained old-page refs: %+v", sessionInfo.Refs)
	}
	if sessionInfo.PageURL != "http://127.0.0.1/done" {
		t.Fatalf("PageURL=%q, want destination URL", sessionInfo.PageURL)
	}
}

func TestWaitSelectorCountJSSupportsTestIDShorthand(t *testing.T) {
	js := waitSelectorCountJS("#card-remote-assist")
	for _, want := range []string{
		"document.querySelectorAll(selector)",
		"data-testid",
		"CSS.escape(testid)",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("wait selector JS missing %q: %s", want, js)
		}
	}
}

func TestURLSurfacesMatchTopLevelAndBrowserPortalURL(t *testing.T) {
	tests := []struct {
		name     string
		surfaces string
		pattern  string
		want     bool
	}{
		{
			name:     "top level url",
			surfaces: "https://example.com/",
			pattern:  "example.com",
			want:     true,
		},
		{
			name:     "portal chip without scheme",
			surfaces: "http://127.0.0.1:8080/portal/browser\n当前页面: iana.org/help/example-domains",
			pattern:  "https://www.iana.org/help/example-domains",
			want:     true,
		},
		{
			name:     "glob marker stripped",
			surfaces: "https://news.ycombinator.com/news",
			pattern:  "**news.ycombinator.com**",
			want:     true,
		},
		{
			name:     "unrelated",
			surfaces: "https://example.com/",
			pattern:  "iana.org",
			want:     false,
		},
	}

	for _, tc := range tests {
		if got := urlSurfacesMatch(tc.surfaces, tc.pattern); got != tc.want {
			t.Fatalf("%s: urlSurfacesMatch() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestParseLLMFlagsSetsEnvironmentAndLeavesCommandArgs(t *testing.T) {
	t.Setenv("DW_BROWSER_LLM_URL", "")
	t.Setenv("DW_BROWSER_LLM_MODEL", "")
	t.Setenv("DW_BROWSER_LLM_PROVIDER", "")
	t.Setenv("DW_BROWSER_LLM_API_KEY", "")
	t.Setenv("DW_BROWSER_VISION_URL", "")
	t.Setenv("DW_BROWSER_VISION_MODEL", "")
	t.Setenv("DW_BROWSER_VISION_PROVIDER", "")
	t.Setenv("DW_BROWSER_VISION_API_KEY", "")

	_, remaining := parseLLMFlags([]string{
		"--llm-url", "http://llm.local/v1",
		"--llm-provider=openai",
		"--llm-model=planner",
		"--llm-api-key", "planner-key",
		"--vlm-model", "vision",
		"--vlm-api-key=vision-key",
		"--id", "s1",
		"goal text",
	})

	if os.Getenv("DW_BROWSER_LLM_URL") != "http://llm.local/v1" {
		t.Fatalf("DW_BROWSER_LLM_URL = %q", os.Getenv("DW_BROWSER_LLM_URL"))
	}
	if os.Getenv("DW_BROWSER_VISION_URL") != "http://llm.local/v1" {
		t.Fatalf("DW_BROWSER_VISION_URL should default to llm url, got %q", os.Getenv("DW_BROWSER_VISION_URL"))
	}
	if os.Getenv("DW_BROWSER_LLM_PROVIDER") != "openai" || os.Getenv("DW_BROWSER_VISION_PROVIDER") != "openai" {
		t.Fatalf("provider env not set: llm=%q vision=%q", os.Getenv("DW_BROWSER_LLM_PROVIDER"), os.Getenv("DW_BROWSER_VISION_PROVIDER"))
	}
	if os.Getenv("DW_BROWSER_LLM_MODEL") != "planner" || os.Getenv("DW_BROWSER_VISION_MODEL") != "vision" {
		t.Fatalf("model env not set: llm=%q vision=%q", os.Getenv("DW_BROWSER_LLM_MODEL"), os.Getenv("DW_BROWSER_VISION_MODEL"))
	}
	if os.Getenv("DW_BROWSER_LLM_API_KEY") != "planner-key" || os.Getenv("DW_BROWSER_VISION_API_KEY") != "vision-key" {
		t.Fatalf("api key env not set")
	}
	if len(remaining) != 3 || remaining[0] != "--id" || remaining[1] != "s1" || remaining[2] != "goal text" {
		t.Fatalf("remaining args = %#v", remaining)
	}
}

func TestResolveSessionPresetID(t *testing.T) {
	tests := []struct {
		name  string
		flags commonFlags
		want  string
	}{
		{
			name:  "iphone ua maps to safari ua simulation",
			flags: commonFlags{userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"},
			want:  browser.PresetIPhoneSafariUA,
		},
		{
			name:  "mac safari ua maps to safari ua simulation",
			flags: commonFlags{userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.3 Safari/605.1.15"},
			want:  browser.PresetMacOSSafariUA,
		},
		{
			name:  "android ua maps to android chrome",
			flags: commonFlags{userAgent: "Mozilla/5.0 (Linux; Android 14; Pixel 7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36"},
			want:  browser.PresetAndroidChrome,
		},
	}

	for _, tc := range tests {
		if got := resolveSessionPresetID(tc.flags); got != tc.want {
			t.Fatalf("%s: resolveSessionPresetID() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestParseCommonFlagsParsesScenario(t *testing.T) {
	positional, flags := parseCommonFlags([]string{
		"--id", "svc1",
		"--scenario", "webvisit",
		"--allow-host", "example.com",
		"--url", "https://example.com",
	}, "test")
	if len(positional) != 0 {
		t.Fatalf("positional=%v, want empty", positional)
	}
	if flags.sessionID != "svc1" {
		t.Fatalf("sessionID=%q, want svc1", flags.sessionID)
	}
	if flags.scenario != "webvisit" {
		t.Fatalf("scenario=%q, want webvisit", flags.scenario)
	}
	if len(flags.allowHosts) != 1 || flags.allowHosts[0] != "example.com" {
		t.Fatalf("allowHosts=%v, want [example.com]", flags.allowHosts)
	}
	// --kind is removed; parseCommonFlags leaves kind at the task default.
	// Scenario → kind/policy/render derivation happens in the command handler.
	if flags.sessionKind != browser.SessionKindTask {
		t.Fatalf("sessionKind=%q, want task", flags.sessionKind)
	}
	if flags.url != "https://example.com" {
		t.Fatalf("url=%q", flags.url)
	}
}

func TestParseCommonFlagsExplicitProfileMakesTaskDedicated(t *testing.T) {
	_, flags := parseCommonFlags([]string{
		"--id", "task1",
		"--profile", "kept-profile",
	}, "test")
	if flags.sessionKind != browser.SessionKindTask || flags.mode != browser.ModeHeadless {
		t.Fatalf("unexpected task defaults: %+v", flags)
	}
	if flags.isolation != browser.SessionIsolationDedicated {
		t.Fatalf("explicit profile must not be treated as ephemeral, got %+v", flags)
	}
	if got := defaultProfileID(flags); got != "task-task1" {
		t.Fatalf("defaultProfileID=%q", got)
	}
}

// ============================================================
// § 可见集塌缩回归 [BUG-VISIBLE-SET-COLLAPSE]
// ============================================================

// buildCollapseFixtureRefs 复刻实机现场: 同屏 55 个可见元素,其中若干 button 把
// 整张卡片文案当 accessible name (实测 401 runes / 单元素 JSON 1416B),原生
// select/checkbox 排在它们之后。
func buildCollapseFixtureRefs() []browser.ElementRef {
	giant := strings.Repeat("当前代际产出分布决策事实坑位人格技能运行中排队失败分类", 20)
	refs := make([]browser.ElementRef, 0, 55)
	add := func(role, name string) {
		i := len(refs) + 1
		short := name
		if r := []rune(short); len(r) > 50 {
			short = string(r[:47]) + "..."
		}
		refs = append(refs, browser.ElementRef{
			Ref:                fmt.Sprintf("@r%d", i),
			BackendNodeID:      int64(100 + i),
			Role:               role,
			Name:               name,
			NameFull:           name,
			NameShort:          short,
			RecommendedLocator: role + ":'" + name + "'",
			Interactable:       true,
			VisibilityKnown:    true,
			VisibleInViewport:  true,
			MatchCount:         1,
		})
	}
	for i := 0; i < 14; i++ {
		add("button", fmt.Sprintf("导航 %d", i))
	}
	for i := 0; i < 5; i++ {
		add("button", fmt.Sprintf("卡片 %d %s", i, giant))
		add("button", "原理 ▸")
	}
	for i := 0; i < 4; i++ {
		add("checkbox", fmt.Sprintf("选择 session %d", i))
	}
	for i := 0; i < 5; i++ {
		add("combobox", "")
	}
	for i := 0; i < 22; i++ {
		add("button", fmt.Sprintf("尾部按钮 %d", i))
	}
	return refs
}

// TestBriefElementsKeepsEveryRoleUnderDefaultBudget 钉死症状 1:
// 默认 --top/--budget 下,同屏的 combobox/checkbox 不得被大文案 button 整类挤掉。
func TestBriefElementsKeepsEveryRoleUnderDefaultBudget(t *testing.T) {
	refs := buildCollapseFixtureRefs()
	elements, total, truncated, omitted := briefElements(refs, defaultBriefTopN, defaultBriefBudget)
	if total != len(refs) {
		t.Fatalf("total=%d, want %d", total, len(refs))
	}
	if !truncated {
		t.Fatal("fixture must truncate at the default budget, otherwise it does not reproduce the bug")
	}
	seen := map[string]int{}
	for _, el := range elements {
		seen[el["role"].(string)]++
	}
	for _, role := range []string{"button", "checkbox", "combobox"} {
		if seen[role] == 0 {
			t.Fatalf("role %q vanished from the printed set: %v (omitted=%v)", role, seen, omitted)
		}
	}
	// 预算是硬约束,不能靠"全打印"取巧通过。
	encoded, err := json.Marshal(elements)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(encoded) > defaultBriefBudget+perElementBudgetEst {
		t.Fatalf("printed %dB, budget %dB", len(encoded), defaultBriefBudget)
	}
	if omitted == nil {
		t.Fatal("truncated output must account for what it dropped (omitted_by_role)")
	}
	sum := 0
	for _, n := range omitted {
		sum += n
	}
	if sum+len(elements) != total {
		t.Fatalf("shown %d + omitted %d != total %d", len(elements), sum, total)
	}
	// 输出次序必须仍是 DFS 阅读序(ref 编号单调递增),公平选择不得打乱视觉次序。
	prev := 0
	for _, el := range elements {
		n := 0
		if _, err := fmt.Sscanf(el["ref"].(string), "@r%d", &n); err != nil {
			t.Fatalf("bad ref %v", el["ref"])
		}
		if n <= prev {
			t.Fatalf("printed order is not DFS-monotonic: %d after %d", n, prev)
		}
		prev = n
	}
}

// TestLeanElementCapsNameFullSoOneElementCannotEatTheBudget 钉死"单元素独吞预算"。
func TestLeanElementCapsNameFullSoOneElementCannotEatTheBudget(t *testing.T) {
	giant := strings.Repeat("整卡片文案", 200)
	el := leanElement(browser.ElementRef{
		Ref: "@r1", Role: "button", Name: giant, NameFull: giant, NameShort: "卡片",
		RecommendedLocator: "button:'卡片'", Interactable: true,
	})
	full, ok := el["name_full"].(string)
	if !ok {
		t.Fatalf("name_full missing: %#v", el)
	}
	if r := []rune(full); len(r) > nameFullDisplayRunes+1 {
		t.Fatalf("name_full=%d runes, want <= %d", len(r), nameFullDisplayRunes+1)
	}
	if el["locator"] != "button:'卡片'" {
		t.Fatalf("locator must stay verbatim (it is the act handle): %#v", el["locator"])
	}
}

// TestObserveListingCountsAreSelfConsistent 钉死症状 3: shown/visible/total 三个数
// 必须同时出现且可互相解释, hit_audit 的采样口径 = visible 而非 shown。
func TestObserveListingCountsAreSelfConsistent(t *testing.T) {
	refs := buildCollapseFixtureRefs()
	snap := &browser.Snapshot{
		SeeToClick:                 true,
		Refs:                       refs,
		DocumentInteractableCount:  len(refs) + 146,
		VisibleInteractableCount:   len(refs),
		OffscreenInteractableCount: 146,
	}
	listing, truncated := observeListing(snap, false, defaultBriefTopN, defaultBriefBudget)
	if !truncated {
		t.Fatal("fixture must truncate")
	}
	shown := listing["shown"].(int)
	visible := listing["visible"].(int)
	total := listing["total"].(int)
	if visible != len(refs) {
		t.Fatalf("visible=%d, want %d (= hit_audit_sampled)", visible, len(refs))
	}
	if shown > visible {
		t.Fatalf("shown=%d must never exceed visible=%d", shown, visible)
	}
	if total != visible+snap.OffscreenInteractableCount {
		t.Fatalf("total=%d != visible=%d + offscreen=%d", total, visible, snap.OffscreenInteractableCount)
	}
}
