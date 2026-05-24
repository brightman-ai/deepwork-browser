// Package skills provides zero-intelligence mechanical parsing and resolution
// of SKILL.md files used by the dw-browser toolchain.
//
// No LLM calls, no distillation, no NL planning — only deterministic file I/O
// and text parsing. Intelligence stays in the consuming application layer.
package skills

// SkillDoc is the first formal Go representation of the SKILL.md schema.
// A SKILL.md file consists of YAML frontmatter (between --- markers)
// followed by markdown body with ## <ActionName> sections.
type SkillDoc struct {
	// Name is the skill identifier (frontmatter "name" field).
	Name string
	// Description is a human-readable summary (frontmatter "description" field).
	Description string
	// Domain is the site domain this skill targets, e.g. "chatgpt.com"
	// (frontmatter "domain" field, dots preserved).
	Domain string
	// Dependencies lists required tooling (frontmatter "dependencies" field).
	// Typically ["dw-browser"].
	Dependencies []string
	// Actions is the ordered list of action definitions from the body sections.
	Actions []Action
	// VerifiedAt is the ISO date the skill was last verified (frontmatter "verified_at").
	VerifiedAt string
}

// Action is a single ## <name> section in a SKILL.md body.
type Action struct {
	// Name is the section header text (the string after "## ").
	Name string
	// Intent is the one-line description from the "intent: ..." line.
	Intent string
	// Recipe contains the lines inside the ```dw-browser fenced block.
	// Each entry is one command line (no surrounding backtick fences).
	Recipe []string
	// Gotchas holds the bullet-point warning lines (without leading "- ").
	Gotchas []string
}

// MatchStatus describes how closely a Binding matches the queried URL.
type MatchStatus string

const (
	// MatchExact means a SKILL.md was found for the exact domain.
	MatchExact MatchStatus = "exact"
	// MatchGeneric means no domain-specific skill exists; generic defaults apply.
	MatchGeneric MatchStatus = "generic"
	// MatchNone means no skill directory was found and no generic fallback exists.
	MatchNone MatchStatus = "none"
	// MatchError means a SKILL.md was found but could not be parsed.
	MatchError MatchStatus = "error"
)

// Binding is the result of resolving a URL against one or more skill roots.
type Binding struct {
	// Status describes the quality of the match.
	Status MatchStatus
	// Domain is the normalised domain derived from the URL (e.g. "chatgpt.com").
	Domain string
	// Actions is the list of actions from the matched SkillDoc (empty for non-exact).
	Actions []Action
	// SkillPath is the filesystem path of the matched SKILL.md (empty if not found).
	SkillPath string
	// Err holds a parse error description when Status == MatchError.
	Err string
}
