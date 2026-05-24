// skills.go — Browser Skills CLI subcommand + snap skill hint integration
// Implements: dw-browser skills {list|read|write}
// Implements: formatSkillHint() for snap output enrichment
package main

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
// § SKILL.md frontmatter parsing (lightweight, no heavy YAML)
// ============================================================

// skillMeta holds parsed frontmatter from a SKILL.md file.
type skillMeta struct {
	Name        string
	Description string
	Domain      string
	Actions     []string
	VerifiedAt  string
}

// parseSkillFrontmatter extracts YAML frontmatter between --- markers.
// Returns metadata and the body (content after second ---).
func parseSkillFrontmatter(content string) (skillMeta, string) {
	var meta skillMeta
	lines := strings.Split(content, "\n")

	// Find frontmatter boundaries
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
		return meta, content
	}

	// Parse key-value pairs from frontmatter
	for i := fmStart + 1; i < fmEnd; i++ {
		line := lines[i]
		// Skip indented lines (YAML sub-items like "  - dw-browser")
		if strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") {
			continue
		}
		idx := strings.Index(line, ": ")
		if idx == -1 {
			// Try key:value without space (e.g., "name:foo")
			idx = strings.Index(line, ":")
			if idx == -1 || idx == len(line)-1 {
				continue
			}
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])

		switch key {
		case "name":
			meta.Name = val
		case "description":
			meta.Description = val
		case "domain":
			meta.Domain = val
		case "verified_at":
			meta.VerifiedAt = val
		case "actions":
			meta.Actions = parseYAMLInlineArray(val)
		}
	}

	body := strings.Join(lines[fmEnd+1:], "\n")
	return meta, body
}

// parseYAMLInlineArray parses "[a, b, c]" into []string{"a","b","c"}.
func parseYAMLInlineArray(s string) []string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		// Single value or empty
		if s == "" {
			return nil
		}
		return []string{s}
	}
	s = s[1 : len(s)-1] // strip [ ]
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

// formatYAMLInlineArray formats []string as "[a, b, c]".
func formatYAMLInlineArray(items []string) string {
	return "[" + strings.Join(items, ", ") + "]"
}

// ============================================================
// § SKILL.md section extraction/replacement
// ============================================================

// extractSection extracts content from ## {name} to next ## or EOF.
// Returns the section content (without the ## header) and whether it was found.
func extractSection(body, sectionName string) (string, bool) {
	lines := strings.Split(body, "\n")
	header := "## " + sectionName
	inSection := false
	var sectionLines []string

	for _, line := range lines {
		if strings.TrimSpace(line) == header || strings.HasPrefix(line, header+"\n") ||
			(strings.HasPrefix(line, "## ") && inSection) {
			if inSection {
				// Hit next ## header — stop
				break
			}
			if strings.TrimSpace(line) == header {
				inSection = true
				continue
			}
		}
		if inSection {
			sectionLines = append(sectionLines, line)
		}
	}
	if !inSection {
		return "", false
	}
	return strings.Join(sectionLines, "\n"), true
}

// replaceSection replaces the content of ## {name} section, or appends if not found.
func replaceSection(body, sectionName, newContent string) string {
	lines := strings.Split(body, "\n")
	header := "## " + sectionName
	var result []string
	inSection := false
	replaced := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == header {
			inSection = true
			replaced = true
			// Write the new section
			result = append(result, header)
			result = append(result, newContent)
			continue
		}
		if inSection && strings.HasPrefix(trimmed, "## ") {
			// Hit next section header — stop skipping
			inSection = false
		}
		if !inSection {
			result = append(result, line)
		}
	}
	if !replaced {
		// Append at end
		// Ensure blank line before new section
		text := strings.Join(result, "\n")
		text = strings.TrimRight(text, "\n")
		text += "\n\n" + header + "\n" + newContent + "\n"
		return text
	}
	return strings.Join(result, "\n")
}

// ============================================================
// § Skills CLI subcommand entry point
// ============================================================

// runSkills handles: dw-browser skills {list|read|write} [args...]
func runSkills(args []string) {
	if len(args) == 1 && wantsHelp(args) {
		printCommandUsage("skills")
		os.Exit(exitOK)
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser skills: requires subcommand (list, read, write)")
		os.Exit(exitRunErr)
	}

	sub := args[0]
	switch sub {
	case "list":
		runSkillsList()
	case "read":
		runSkillsRead(args[1:])
	case "write":
		runSkillsWrite(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "dw-browser skills: unknown subcommand %q (use: list, read, write)\n", sub)
		os.Exit(exitRunErr)
	}
}

// ============================================================
// § skills list
// ============================================================

func runSkillsList() {
	baseDir := skillsBaseDir()
	if baseDir == "" {
		fmt.Fprintln(os.Stderr, "dw-browser skills list: cannot resolve home directory")
		os.Exit(exitRunErr)
	}

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dw-browser skills list: %v\n", err)
		os.Exit(exitRunErr)
	}

	type skillEntry struct {
		dirName string
		meta    skillMeta
	}
	var skills []skillEntry

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
		skills = append(skills, skillEntry{dirName: entry.Name(), meta: meta})
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].dirName < skills[j].dirName
	})

	for _, s := range skills {
		domain := s.meta.Domain
		if domain == "" {
			domain = "(generic)"
		}
		actions := formatYAMLInlineArray(s.meta.Actions)
		fmt.Printf("%-18s %-14s %s\n", s.dirName, domain, actions)
	}
	os.Exit(exitOK)
}

// ============================================================
// § skills read
// ============================================================

func runSkillsRead(args []string) {
	if wantsHelp(args) {
		printCommandUsage("skills")
		os.Exit(exitOK)
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser skills read: requires skill name")
		os.Exit(exitRunErr)
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
		fmt.Fprintf(os.Stderr, "dw-browser skills read: skill %q not found (%s)\n", name, skillFile)
		os.Exit(exitRunErr)
	}

	if actionName == "" {
		// Print full content
		fmt.Print(string(data))
		os.Exit(exitOK)
	}

	// Extract specific action section
	_, body := parseSkillFrontmatter(string(data))
	section, found := extractSection(body, actionName)
	if !found {
		fmt.Fprintf(os.Stderr, "dw-browser skills read: action %q not found in skill %q\n", actionName, name)
		os.Exit(exitRunErr)
	}
	fmt.Printf("## %s\n%s", actionName, section)
	os.Exit(exitOK)
}

// ============================================================
// § skills write
// ============================================================

func runSkillsWrite(args []string) {
	if wantsHelp(args) {
		printCommandUsage("skills")
		os.Exit(exitOK)
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "dw-browser skills write: requires skill name")
		os.Exit(exitRunErr)
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
		fmt.Fprintln(os.Stderr, "dw-browser skills write: --action is required")
		os.Exit(exitRunErr)
	}

	// Read stdin
	stdinContent := readStdin()
	if stdinContent == "" {
		fmt.Fprintln(os.Stderr, "dw-browser skills write: no input on stdin")
		os.Exit(exitRunErr)
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
			fmt.Fprintf(os.Stderr, "dw-browser skills write: %v\n", err)
			os.Exit(exitRunErr)
		}
	} else {
		// New skill: create directory + file
		if err := os.MkdirAll(skillDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser skills write: %v\n", err)
			os.Exit(exitRunErr)
		}

		domain := strings.ReplaceAll(name, "-", ".")
		meta := skillMeta{
			Name:       name,
			Domain:     domain,
			Actions:    []string{actionName},
			VerifiedAt: today,
		}
		body := "\n# " + domain + "\n\n## Site\n\n" + "## " + actionName + "\n" + sectionContent + "\n"
		newContent := buildSkillFile(meta, body)
		if err := os.WriteFile(skillFile, []byte(newContent), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "dw-browser skills write: %v\n", err)
			os.Exit(exitRunErr)
		}
	}

	fmt.Printf("dw-browser: wrote action %q to skill %q\n", actionName, name)
	os.Exit(exitOK)
}

// readStdin reads all content from stdin.
func readStdin() string {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return strings.Join(lines, "\n")
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
// § Snap skill hint (Task 2)
// ============================================================

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

	// Extract domain from URL
	domain := extractDomain(pageURL)
	if domain == "" {
		return ""
	}

	// Convert domain to skill directory name (. → -)
	skillName := strings.ReplaceAll(domain, ".", "-")
	skillFile := filepath.Join(baseDir, skillName, "SKILL.md")

	data, err := os.ReadFile(skillFile)
	if err != nil {
		return "" // No skill for this domain
	}

	meta, body := parseSkillFrontmatter(string(data))
	if len(meta.Actions) == 0 {
		return ""
	}

	// Build action(intent) pairs
	var pairs []string
	for _, action := range meta.Actions {
		intent := extractActionIntent(body, action)
		if intent != "" {
			pairs = append(pairs, action+"("+intent+")")
		} else {
			pairs = append(pairs, action)
		}
	}

	return fmt.Sprintf("[Skill: %s — %s]", skillName, strings.Join(pairs, " "))
}

// extractDomain extracts the domain from a URL string.
// Strips port, strips "www." prefix.
func extractDomain(rawURL string) string {
	// Ensure URL has scheme
	if !strings.Contains(rawURL, "://") {
		rawURL = "https://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Hostname() // strips port
	host = strings.TrimPrefix(host, "www.")
	return host
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
// Populated once from SKILL.md, carried through act execution.
type skillExecContext struct {
	ActionName string `json:"action"`     // "send-message"
	Intent     string `json:"intent"`     // "发送消息"
	Domain     string `json:"domain"`     // "chatgpt.com"
	SkillName  string `json:"skill_name"` // "chatgpt-com"
	Step       int    `json:"step"`       // 1-based, auto-incremented per session
	Total      int    `json:"total"`      // recipe line count (0 = unknown)
}

// sessionSkillState tracks step counter across act calls within a session.
// Key: sessionID + ":" + actionName → current step count.
var sessionSkillSteps = map[string]int{}

// resolveSkillContext builds a skillExecContext from the --skill flag value
// and the current page URL.
//
// pageURL is used to derive domain → skill directory.
// If the skill or action is not found, returns nil (graceful no-op).
func resolveSkillContext(skillFlag, pageURL, sessionID string) *skillExecContext {
	if skillFlag == "" {
		return nil
	}

	domain := extractDomain(pageURL)
	if domain == "" {
		// No URL context — use skillFlag as-is, minimal context
		return &skillExecContext{ActionName: skillFlag}
	}

	skillName := strings.ReplaceAll(domain, ".", "-")
	baseDir := skillsBaseDir()
	if baseDir == "" {
		return &skillExecContext{ActionName: skillFlag, Domain: domain, SkillName: skillName}
	}

	skillFile := filepath.Join(baseDir, skillName, "SKILL.md")
	data, err := os.ReadFile(skillFile)
	if err != nil {
		return &skillExecContext{ActionName: skillFlag, Domain: domain, SkillName: skillName}
	}

	_, body := parseSkillFrontmatter(string(data))
	intent := extractActionIntent(body, skillFlag)
	total := countRecipeLines(body, skillFlag)

	// Auto-increment step counter per session+action
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

// countRecipeLines counts lines inside the ```dw-browser fenced block
// in an action's ## section. Returns 0 if not found.
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
// No-op if sc is nil.
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
