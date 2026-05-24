package skills

import (
	"fmt"
	"strings"
)

// Parse parses a SKILL.md file from its raw bytes into a SkillDoc.
//
// The format is:
//
//	---
//	name: ...
//	description: ...
//	domain: ...
//	dependencies:
//	  - dw-browser
//	actions: [action1, action2]
//	verified_at: YYYY-MM-DD
//	---
//
//	## action1
//	intent: ...
//
//	```dw-browser
//	command line 1
//	command line 2
//	```
//
//	- gotcha line
//
// Behavior matches the parsing logic previously in cmd/dw-browser/skills.go.
// Returns an error for structurally invalid frontmatter (e.g. unterminated array).
func Parse(data []byte) (*SkillDoc, error) {
	content := string(data)

	front, body, err := splitFrontmatter(content)
	if err != nil {
		return nil, fmt.Errorf("skills.Parse: %w", err)
	}

	if err := validateFrontmatter(front); err != nil {
		return nil, fmt.Errorf("skills.Parse: invalid frontmatter: %w", err)
	}

	doc := &SkillDoc{}
	parseFrontmatter(doc, front)

	// Parse ## sections for each action declared in the frontmatter.
	for _, name := range frontmatterActionNames(front) {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		action := parseActionSection(name, body)
		doc.Actions = append(doc.Actions, action)
	}

	return doc, nil
}

// validateFrontmatter performs lightweight structural validation of the frontmatter.
// It checks for common malformed constructs such as unterminated inline arrays.
func validateFrontmatter(front string) error {
	for _, line := range strings.Split(front, "\n") {
		// Skip indented lines
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		val := strings.TrimSpace(line[idx+1:])
		// Unterminated inline array: starts with [ but does not end with ]
		if strings.HasPrefix(val, "[") && !strings.HasSuffix(val, "]") {
			return fmt.Errorf("unterminated inline array in line %q", strings.TrimSpace(line))
		}
	}
	return nil
}

// splitFrontmatter splits content at --- markers.
// Returns (frontmatter text, body text, error).
// The frontmatter text does not include the --- lines.
// The body text starts immediately after the closing ---.
func splitFrontmatter(content string) (string, string, error) {
	// Strip UTF-8 BOM (EF BB BF)
	content = strings.TrimPrefix(content, "\xef\xbb\xbf")

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
		return "", content, fmt.Errorf("missing frontmatter delimiters")
	}
	front := strings.Join(lines[fmStart+1:fmEnd], "\n")
	body := strings.Join(lines[fmEnd+1:], "\n")
	return front, body, nil
}

// parseFrontmatter populates doc fields from frontmatter YAML-like text.
// Uses the same lightweight key-value parser as the original CLI code
// (no heavy YAML library, no sub-item indented parsing for string fields).
func parseFrontmatter(doc *SkillDoc, front string) {
	lines := strings.Split(front, "\n")
	for _, line := range lines {
		// Skip indented lines (YAML sub-items like "  - dw-browser")
		if strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") {
			// But capture dependency items
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "- ") {
				// We'll collect dependencies separately below.
			}
			continue
		}
		idx := strings.Index(line, ": ")
		if idx == -1 {
			idx = strings.Index(line, ":")
			if idx == -1 || idx == len(line)-1 {
				continue
			}
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])

		switch key {
		case "name":
			doc.Name = val
		case "description":
			doc.Description = val
		case "domain":
			doc.Domain = val
		case "verified_at":
			doc.VerifiedAt = val
		case "dependencies":
			// Inline array or empty — sub-items collected below
			if val != "" {
				doc.Dependencies = parseInlineArray(val)
			}
		case "actions":
			// Inline array like "[a, b, c]"
			doc.Actions = nil // reset; actual Action structs built from body
		}
	}

	// Collect dependencies from indented "  - item" lines
	inDeps := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") {
			if inDeps && strings.HasPrefix(trimmed, "- ") {
				dep := strings.TrimPrefix(trimmed, "- ")
				doc.Dependencies = append(doc.Dependencies, strings.TrimSpace(dep))
			}
			continue
		}
		// Reset inDeps whenever a non-indented key line appears
		if strings.HasPrefix(trimmed, "dependencies") {
			inDeps = true
		} else if strings.Contains(trimmed, ":") {
			inDeps = false
		}
	}
	// Deduplicate dependencies (inline + block)
	if len(doc.Dependencies) > 0 {
		seen := make(map[string]bool)
		deduped := doc.Dependencies[:0]
		for _, d := range doc.Dependencies {
			if !seen[d] {
				seen[d] = true
				deduped = append(deduped, d)
			}
		}
		doc.Dependencies = deduped
	}
}

// frontmatterActionNames returns the action name list from the frontmatter "actions" field.
// This is separate from parseFrontmatter because we need the order for body section lookup.
func frontmatterActionNames(front string) []string {
	for _, line := range strings.Split(front, "\n") {
		if strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") {
			continue
		}
		idx := strings.Index(line, ": ")
		if idx == -1 {
			idx = strings.Index(line, ":")
			if idx == -1 || idx == len(line)-1 {
				continue
			}
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if key == "actions" {
			return parseInlineArray(val)
		}
	}
	return nil
}

// parseActionSection extracts an Action from the ## {name} section of body.
func parseActionSection(name, body string) Action {
	a := Action{Name: name}
	section, found := extractSection(body, name)
	if !found {
		return a
	}

	lines := strings.Split(section, "\n")
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// intent line
		if !inFence && strings.HasPrefix(strings.ToLower(trimmed), "intent:") {
			a.Intent = strings.TrimSpace(trimmed[len("intent:"):])
			continue
		}

		// dw-browser fenced block
		if strings.HasPrefix(trimmed, "```dw-browser") {
			inFence = true
			continue
		}
		if inFence && strings.HasPrefix(trimmed, "```") {
			inFence = false
			continue
		}
		if inFence {
			if trimmed != "" {
				a.Recipe = append(a.Recipe, line) // preserve original indentation
			}
			continue
		}

		// gotcha bullets (outside fence)
		if strings.HasPrefix(trimmed, "- ") {
			a.Gotchas = append(a.Gotchas, strings.TrimPrefix(trimmed, "- "))
		}
	}
	return a
}

// extractSection returns the content of ## {name} section (without the header line)
// up to the next ## header or EOF. Matches the behaviour of the original CLI code.
func extractSection(body, sectionName string) (string, bool) {
	lines := strings.Split(body, "\n")
	header := "## " + sectionName
	inSection := false
	var sectionLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			if inSection {
				break // next section — stop
			}
			if trimmed == header {
				inSection = true
			}
			continue
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

// parseInlineArray parses "[a, b, c]" → []string{"a","b","c"}.
// Also handles a bare single value "foo" → []string{"foo"}.
func parseInlineArray(s string) []string {
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

// FormatInlineArray formats []string as "[a, b, c]" (for round-trip write support).
func FormatInlineArray(items []string) string {
	return "[" + strings.Join(items, ", ") + "]"
}

// ExtractSection is the exported form of extractSection, needed by the CLI write path.
func ExtractSection(body, sectionName string) (string, bool) {
	return extractSection(body, sectionName)
}

// ReplaceSection replaces the content of ## {name} section, or appends if not found.
// This preserves the write behaviour of the original CLI code.
func ReplaceSection(body, sectionName, newContent string) string {
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
			result = append(result, header)
			result = append(result, newContent)
			continue
		}
		if inSection && strings.HasPrefix(trimmed, "## ") {
			inSection = false
		}
		if !inSection {
			result = append(result, line)
		}
	}
	if !replaced {
		text := strings.Join(result, "\n")
		text = strings.TrimRight(text, "\n")
		text += "\n\n" + header + "\n" + newContent + "\n"
		return text
	}
	return strings.Join(result, "\n")
}
