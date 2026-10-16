// skills.go — Browser Skills CLI subcommand + snap skill hint integration
// Implements: dw-browser skills {list|read|write}
// Implements: formatSkillHint() for snap output enrichment
//
// All SKILL.md parsing is delegated to the browser/skills library.
// This file is a THIN wrapper: CLI I/O, user-facing output, and session
// step-counter state only.
package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/brightman-ai/deepwork-browser/browser/skills"
)

// ============================================================
// § Skills base directory
// ============================================================

// skillsBaseDir returns ~/.deepwork/browser-skills/
func skillsBaseDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".deepwork", "browser-skills")
}

// ============================================================
// § Thin wrappers — delegates to browser/skills library
// ============================================================

// parseSkillFrontmatter is a compatibility shim: returns (skillMeta, body)
// by delegating to skills.Parse. Used only by write path that needs meta+body
// separately for reconstruction.
func parseSkillFrontmatter(content string) (skillMeta, string) {
	var meta skillMeta
	doc, err := skills.Parse([]byte(content))
	if err != nil {
		// Return empty meta + full content as body (graceful degradation)
		return meta, content
	}
	meta.Name = doc.Name
	meta.Description = doc.Description
	meta.Domain = doc.Domain
	meta.VerifiedAt = doc.VerifiedAt
	for _, a := range doc.Actions {
		meta.Actions = append(meta.Actions, a.Name)
	}
	// Retrieve body by stripping frontmatter manually (needed for replaceSection)
	_, body, _ := splitSkillFrontmatterRaw(content)
	return meta, body
}

// splitSkillFrontmatterRaw splits content at --- markers and returns
// (frontmatter text, body text, ok).  Used by write path.
func splitSkillFrontmatterRaw(content string) (string, string, bool) {
	lines := strings.Split(content, "\n")
	fmStart, fmEnd := -1, -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
			if fmStart == -1 {
				fmStart = i
			} else {
				fmEnd = i
				break
			}
		}
	}
	if fmStart == -1 || fmEnd == -1 {
		return "", content, false
	}
	front := strings.Join(lines[fmStart+1:fmEnd], "\n")
	body := strings.Join(lines[fmEnd+1:], "\n")
	return front, body, true
}

// extractSection delegates to the library.
func extractSection(body, sectionName string) (string, bool) {
	return skills.ExtractSection(body, sectionName)
}

// replaceSection delegates to the library.
func replaceSection(body, sectionName, newContent string) string {
	return skills.ReplaceSection(body, sectionName, newContent)
}

// parseYAMLInlineArray delegates to the library (unexported wrapper).
func parseYAMLInlineArray(s string) []string {
	// Use the library's inline array parser via a round-trip.
	// The library's FormatInlineArray and parseInlineArray are internal, but
	// the exported FormatInlineArray gives us the inverse; we parse inline arrays
	// the same way the library does.
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// formatYAMLInlineArray delegates to the library.
func formatYAMLInlineArray(items []string) string {
	return skills.FormatInlineArray(items)
}

// ============================================================
// § skillMeta — local CLI-only struct (used by write path)
// ============================================================

// skillMeta holds parsed frontmatter for write/rebuild path.
// This is not exported; only the CLI write/list paths use it directly.
type skillMeta struct {
	Name        string
	Description string
	Domain      string
	Actions     []string
	VerifiedAt  string
}

// ============================================================
// § Skills CLI subcommand entry point
// ============================================================

// runSkills handles: dw-browser skills {list|read|write} [args...]
func runSkillsListResult(args []string) (commandOutput CommandResult, resultErr error) {
	baseDir := skillsBaseDir()
	if baseDir == "" {
		commandOutput.note("%v\n", "dw-browser skills list: cannot resolve home directory")
		return finishCommand(commandOutput, exitRunErr)
	}

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		commandOutput.note("dw-browser skills list: %v\n", err)
		return finishCommand(commandOutput, exitRunErr)
	}

	type skillEntry struct {
		dirName string
		meta    skillMeta
	}
	var skillList []skillEntry

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillFile := filepath.Join(baseDir, entry.Name(), "SKILL.md")
		data, err := os.ReadFile(skillFile)
		if err != nil {
			continue // skip directories without SKILL.md
		}
		meta, _ := parseSkillFrontmatter(string(data))
		skillList = append(skillList, skillEntry{dirName: entry.Name(), meta: meta})
	}

	sort.Slice(skillList, func(i, j int) bool {
		return skillList[i].dirName < skillList[j].dirName
	})

	skillRows := []map[string]any{}
	for _, s := range skillList {
		domain := s.meta.Domain
		if domain == "" {
			domain = "(generic)"
		}
		actions := formatYAMLInlineArray(s.meta.Actions)
		skillRows = append(skillRows, map[string]any{"name": s.dirName, "domain": domain, "actions": actions})
	}
	commandOutput.Payload = map[string]any{"skills": skillRows}
	return finishCommand(commandOutput, exitOK)
}

// ============================================================
// § skills read
// ============================================================

func runSkillsReadResult(args []string) (commandOutput CommandResult, resultErr error) {
	if wantsHelp(args) {
		commandOutput.Payload = generatedHelp("skills")
		return finishCommand(commandOutput, exitOK)
	}
	if len(args) < 1 {
		commandOutput.note("%v\n", "dw-browser skills read: requires skill name")
		return finishCommand(commandOutput, exitRunErr)
	}

	name := args[0]

	// Parse --action flag
	var actionName string
	for i := 1; i < len(args); i++ {
		if args[i] == "--action" && i+1 < len(args) {
			actionName = args[i+1]
			break
		}
		if strings.HasPrefix(args[i], "--action=") {
			actionName = args[i][len("--action="):]
			break
		}
	}

	baseDir := skillsBaseDir()
	skillFile := filepath.Join(baseDir, name, "SKILL.md")
	data, err := os.ReadFile(skillFile)
	if err != nil {
		// Disk root miss — fall back to embedded public corpus.
		embedPath := "corpus/" + name + "/SKILL.md"
		embedded, embedErr := skills.ReadEmbedded(embedPath)
		if embedErr != nil {
			commandOutput.note("dw-browser skills read: skill %q not found (%s)\n", name, skillFile)
			return finishCommand(commandOutput, exitRunErr)
		}
		data = embedded
		skillFile = "[embed]" + embedPath
	}

	if actionName == "" {
		// Print full content
		commandOutput.Payload = map[string]any{"content": string(data)}
		return finishCommand(commandOutput, exitOK)
	}

	// Extract specific action section via library
	_, body := parseSkillFrontmatter(string(data))
	section, found := extractSection(body, actionName)
	if !found {
		commandOutput.note("dw-browser skills read: action %q not found in skill %q\n", actionName, name)
		return finishCommand(commandOutput, exitRunErr)
	}
	commandOutput.appendContent(fmt.Sprintf("## %s\n%s", actionName, section))
	return finishCommand(commandOutput, exitOK)
}

// ============================================================
// § skills write
// ============================================================

func runSkillsWriteResult(args []string) (commandOutput CommandResult, resultErr error) {
	if wantsHelp(args) {
		commandOutput.Payload = generatedHelp("skills")
		return finishCommand(commandOutput, exitOK)
	}
	if len(args) < 1 {
		commandOutput.note("%v\n", "dw-browser skills write: requires skill name")
		return finishCommand(commandOutput, exitRunErr)
	}

	name := args[0]

	// Parse --action flag
	var actionName string
	for i := 1; i < len(args); i++ {
		if args[i] == "--action" && i+1 < len(args) {
			actionName = args[i+1]
			break
		}
		if strings.HasPrefix(args[i], "--action=") {
			actionName = args[i][len("--action="):]
			break
		}
	}
	if actionName == "" {
		commandOutput.note("%v\n", "dw-browser skills write: --action is required")
		return finishCommand(commandOutput, exitRunErr)
	}

	// Read stdin
	stdinContent, stdinErr := readStdinE()
	if stdinErr != nil {
		return commandOutput, commandError("validation", exitRunErr, stdinErr)
	}
	if stdinContent == "" {
		commandOutput.note("%v\n", "dw-browser skills write: no input on stdin")
		return finishCommand(commandOutput, exitRunErr)
	}

	// Parse stdin fields: intent, recipe, gotcha
	intent, recipe, gotchas := parseWriteInput(stdinContent)

	// Build the section content in SKILL.md format
	sectionContent := buildActionSection(intent, recipe, gotchas)

	baseDir := skillsBaseDir()
	skillDir := filepath.Join(baseDir, name)
	skillFile := filepath.Join(skillDir, "SKILL.md")

	today := time.Now().Format("2006-01-02")

	if data, err := os.ReadFile(skillFile); err == nil {
		// File exists: update
		content := string(data)
		meta, body := parseSkillFrontmatter(content)

		// Update actions list
		if !containsString(meta.Actions, actionName) {
			meta.Actions = append(meta.Actions, actionName)
		}
		meta.VerifiedAt = today

		// Replace or append section
		body = replaceSection(body, actionName, sectionContent)

		// Rebuild file
		newContent := buildSkillFile(meta, body)
		if err := os.WriteFile(skillFile, []byte(newContent), 0644); err != nil {
			commandOutput.note("dw-browser skills write: %v\n", err)
			return finishCommand(commandOutput, exitRunErr)
		}
	} else {
		// New skill: create directory + file
		if err := os.MkdirAll(skillDir, 0755); err != nil {
			commandOutput.note("dw-browser skills write: %v\n", err)
			return finishCommand(commandOutput, exitRunErr)
		}

		domain := name // name is already the dotted domain (e.g. "chatgpt.com")
		meta := skillMeta{
			Name:       name,
			Domain:     domain,
			Actions:    []string{actionName},
			VerifiedAt: today,
		}
		body := "\n# " + domain + "\n\n## Site\n\n" + "## " + actionName + "\n" + sectionContent + "\n"
		newContent := buildSkillFile(meta, body)
		if err := os.WriteFile(skillFile, []byte(newContent), 0644); err != nil {
			commandOutput.note("dw-browser skills write: %v\n", err)
			return finishCommand(commandOutput, exitRunErr)
		}
	}

	commandOutput.appendContent(fmt.Sprintf("dw-browser: wrote action %q to skill %q\n", actionName, name))
	return finishCommand(commandOutput, exitOK)
}

// readStdin reads all content from stdin.
func readStdinE() (string, error) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return strings.Join(lines, "\n"), scanner.Err()
}

// parseWriteInput parses the stdin format:
//
//	intent: ...
//	recipe:
//	  line1
//	  line2
//	gotcha: ...
func parseWriteInput(input string) (intent string, recipe []string, gotchas []string) {
	lines := strings.Split(input, "\n")
	section := "" // current section: "", "recipe", "gotcha"

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "intent:") {
			intent = strings.TrimSpace(trimmed[len("intent:"):])
			section = ""
			continue
		}
		if trimmed == "recipe:" || strings.HasPrefix(trimmed, "recipe:") {
			section = "recipe"
			// Check for inline content after "recipe:"
			after := strings.TrimSpace(trimmed[len("recipe:"):])
			if after != "" {
				recipe = append(recipe, after)
			}
			continue
		}
		if strings.HasPrefix(trimmed, "gotcha:") {
			section = "gotcha"
			after := strings.TrimSpace(trimmed[len("gotcha:"):])
			if after != "" {
				gotchas = append(gotchas, after)
			}
			continue
		}
		switch section {
		case "recipe":
			// Dedent: strip leading 2 spaces or 1 tab
			dedented := line
			if strings.HasPrefix(dedented, "  ") {
				dedented = dedented[2:]
			} else if strings.HasPrefix(dedented, "\t") {
				dedented = dedented[1:]
			}
			if strings.TrimSpace(dedented) != "" {
				recipe = append(recipe, dedented)
			}
		case "gotcha":
			if trimmed != "" {
				gotchas = append(gotchas, trimmed)
			}
		}
	}
	return
}

// buildActionSection builds the markdown content for an action section (without ## header).
func buildActionSection(intent string, recipe []string, gotchas []string) string {
	var sb strings.Builder
	if intent != "" {
		sb.WriteString("intent: " + intent + "\n")
	}
	if len(recipe) > 0 {
		sb.WriteString("\n```dw-browser\n")
		for _, line := range recipe {
			sb.WriteString(line + "\n")
		}
		sb.WriteString("```\n")
	}
	if len(gotchas) > 0 {
		sb.WriteString("\n")
		for _, g := range gotchas {
			if !strings.HasPrefix(g, "- ") {
				g = "- " + g
			}
			sb.WriteString(g + "\n")
		}
	}
	return sb.String()
}

// buildSkillFile reconstructs a SKILL.md from metadata and body.
func buildSkillFile(meta skillMeta, body string) string {
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString("name: " + meta.Name + "\n")
	if meta.Description != "" {
		sb.WriteString("description: " + meta.Description + "\n")
	}
	if meta.Domain != "" {
		sb.WriteString("domain: " + meta.Domain + "\n")
	}
	sb.WriteString("dependencies:\n  - dw-browser\n")
	sb.WriteString("actions: " + formatYAMLInlineArray(meta.Actions) + "\n")
	sb.WriteString("verified_at: " + meta.VerifiedAt + "\n")
	sb.WriteString("---\n")
	sb.WriteString(body)
	return sb.String()
}

// containsString checks if a slice contains a string.
func containsString(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

// ============================================================
// § Snap skill hint
// ============================================================

// formatPersonaHint 在 AI-native 测试场景(app-test-explore/app-test-baseline)的
// observe 输出里主动提醒:测试保真人格(--persona)可用 —— 让 AI 不必读全 help 就发现
// 能力(与 skill_hint 同构的能力浮现)。webvisit 不提示;已激活 persona 时不重复提示
// (只浮现能力,不替 AI 选)。scenario 值须与 browser.Scenario 常量一致。
func formatPersonaHint(scenario, activePersonaID string) string {
	if activePersonaID != "" {
		return ""
	}
	switch scenario {
	case "app-test-explore", "app-test-baseline":
		return "测试保真人格可用: --persona wechat-iphone|wecom-android|desktop-cn-dark 等" +
			"(headless 忠实还原 in-app 微信遮罩/移动端/暗色/断网,让被测 app 适配逻辑触发); dw-browser --help 看全部"
	default:
		return ""
	}
}

// formatSkillHint checks for browser skills matching the page URL's domain.
// Returns a one-line hint string, or "" if no skill found.
func formatSkillHint(pageURL string) string {
	if pageURL == "" {
		return ""
	}

	baseDir := skillsBaseDir()
	if baseDir == "" {
		return ""
	}

	// Use the library resolver
	b, err := skills.Resolve(pageURL, baseDir)
	if err != nil || b.Status != skills.MatchExact {
		return ""
	}

	if len(b.Actions) == 0 {
		return ""
	}

	// Build action(intent) pairs
	var pairs []string
	for _, action := range b.Actions {
		if action.Intent != "" {
			pairs = append(pairs, action.Name+"("+action.Intent+")")
		} else {
			pairs = append(pairs, action.Name)
		}
	}

	return fmt.Sprintf("[Skill: %s — %s]", b.Domain, strings.Join(pairs, " "))
}

// extractDomain is kept for backward compatibility with resolveSkillContext.
func extractDomain(rawURL string) string {
	return skills.DeriveDomain(rawURL)
}

// extractActionIntent reads the "intent: ..." line from a ## {action} section.
func extractActionIntent(body, actionName string) string {
	section, found := extractSection(body, actionName)
	if !found {
		return ""
	}
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "intent:") {
			return strings.TrimSpace(trimmed[len("intent:"):])
		}
	}
	return ""
}

// ============================================================
// § Skill execution context (--skill flag observability)
// ============================================================

// skillExecContext holds auto-derived context for --skill flag.
type skillExecContext struct {
	ActionName string `json:"action"`
	Intent     string `json:"intent"`
	Domain     string `json:"domain"`
	SkillName  string `json:"skill_name"`
	Step       int    `json:"step"`
	Total      int    `json:"total"`
}

// sessionSkillState tracks step counter across act calls within a session.
var sessionSkillSteps = map[string]int{}

// resolveSkillContext builds a skillExecContext from the --skill flag value
// and the current page URL.
func resolveSkillContext(skillFlag, pageURL, sessionID string) *skillExecContext {
	if skillFlag == "" {
		return nil
	}

	domain := skills.DeriveDomain(pageURL)
	if domain == "" {
		return &skillExecContext{ActionName: skillFlag}
	}

	skillName := domain
	baseDir := skillsBaseDir()
	if baseDir == "" {
		return &skillExecContext{ActionName: skillFlag, Domain: domain, SkillName: skillName}
	}

	b, err := skills.Resolve(pageURL, baseDir)
	if err != nil || b.Status != skills.MatchExact {
		return &skillExecContext{ActionName: skillFlag, Domain: domain, SkillName: skillName}
	}

	// Find the matching action
	var intent string
	var total int
	for _, action := range b.Actions {
		if action.Name == skillFlag {
			intent = action.Intent
			total = len(action.Recipe)
			break
		}
	}

	key := sessionID + ":" + skillFlag
	sessionSkillSteps[key]++
	step := sessionSkillSteps[key]

	return &skillExecContext{
		ActionName: skillFlag,
		Intent:     intent,
		Domain:     domain,
		SkillName:  skillName,
		Step:       step,
		Total:      total,
	}
}

// countRecipeLines is kept for any remaining callers (delegates to library).
func countRecipeLines(body, actionName string) int {
	section, found := extractSection(body, actionName)
	if !found {
		return 0
	}
	inBlock := false
	count := 0
	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```dw-browser") {
			inBlock = true
			continue
		}
		if inBlock && strings.HasPrefix(trimmed, "```") {
			break
		}
		if inBlock && trimmed != "" {
			count++
		}
	}
	return count
}

// injectSkillFields adds skill context fields into an output map.
func injectSkillFields(output map[string]interface{}, sc *skillExecContext) {
	if sc == nil {
		return
	}
	skill := map[string]interface{}{
		"action": sc.ActionName,
	}
	if sc.Intent != "" {
		skill["intent"] = sc.Intent
	}
	if sc.Domain != "" {
		skill["domain"] = sc.Domain
	}
	if sc.Step > 0 {
		skill["step"] = sc.Step
	}
	if sc.Total > 0 {
		skill["total"] = sc.Total
	}
	output["skill"] = skill
}
