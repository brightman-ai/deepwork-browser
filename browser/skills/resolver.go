package skills

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Resolve resolves rawURL against the provided skill roots (searched in order).
// The first root that contains a matching SKILL.md wins (private overlay first).
//
// Domain derivation: strip scheme/www./path/port → return dotted host as-is.
// Example: "https://chatgpt.com/chat" → domain "chatgpt.com" → looks for
// <root>/chatgpt.com/SKILL.md.
//
// If roots is empty, DefaultSkillRoots() is used.
func Resolve(rawURL string, roots ...string) (Binding, error) {
	if len(roots) == 0 {
		roots = DefaultSkillRoots()
	}

	domain := DeriveDomain(rawURL)
	if domain == "" {
		return Binding{Status: MatchNone, Domain: domain}, nil
	}

	for _, root := range roots {
		if root == "" {
			continue
		}
		candidate := filepath.Join(root, domain, "SKILL.md")
		st, err := os.Stat(candidate)
		if err != nil || st.IsDir() {
			continue
		}
		data, err := os.ReadFile(candidate)
		if err != nil {
			return Binding{
				Status:    MatchError,
				Domain:    domain,
				SkillPath: candidate,
				Err:       fmt.Sprintf("read %s: %v", candidate, err),
			}, nil
		}
		doc, err := Parse(data)
		if err != nil {
			return Binding{
				Status:    MatchError,
				Domain:    domain,
				SkillPath: candidate,
				Err:       err.Error(),
			}, nil
		}
		return Binding{
			Status:    MatchExact,
			Domain:    domain,
			Actions:   doc.Actions,
			SkillPath: candidate,
		}, nil
	}

	// Disk roots exhausted — fall back to the embedded public corpus.
	if b, ok := resolveFromEmbed(domain); ok {
		return b, nil
	}

	return Binding{Status: MatchGeneric, Domain: domain}, nil
}

// resolveFromEmbed looks up domain in the embedded corpus FS.
// Returns (Binding, true) on hit, (zero, false) on miss.
func resolveFromEmbed(domain string) (Binding, bool) {
	embedPath := "corpus/" + domain + "/SKILL.md"
	data, err := fs.ReadFile(CorpusFS, embedPath)
	if err != nil {
		// File not present in embedded corpus — not an error, just a miss.
		return Binding{}, false
	}
	doc, err := Parse(data)
	if err != nil {
		return Binding{
			Status:    MatchError,
			Domain:    domain,
			SkillPath: "[embed]" + embedPath,
			Err:       err.Error(),
		}, true
	}
	return Binding{
		Status:    MatchExact,
		Domain:    domain,
		Actions:   doc.Actions,
		SkillPath: "[embed]" + embedPath,
	}, true
}

// DeriveDomain converts a raw URL (or bare domain) into the canonical skill directory name.
// Rules:
//  1. Trim whitespace and lowercase.
//  2. If no scheme, prepend "https://".
//  3. Parse the host, strip port.
//  4. Strip "www." prefix.
//
// The dotted host is returned as-is — dots are preserved, not replaced.
// Example: "https://chatgpt.com/chat" → "chatgpt.com"
//
//	"https://meta.discourse.org/t/123" → "meta.discourse.org"
func DeriveDomain(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	rawURL = strings.TrimSpace(strings.ToLower(rawURL))
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

// DefaultSkillRoots returns the default ordered list of skill root directories.
// Order: ~/.deepwork/browser-skills (user private overlay) only by default.
// The public corpus root (inside deepwork-browser) is intentionally not included
// here — consumers can pass it explicitly as an additional root argument.
func DefaultSkillRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".deepwork", "browser-skills")}
}
