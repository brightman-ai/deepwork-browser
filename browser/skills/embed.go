package skills

import (
	"embed"
	"io/fs"
)

// CorpusFS holds the public skill corpus (chatgpt.com, claude.ai, meta.discourse.org, interaction)
// embedded at build time. It is used as the final fallback by Resolve when no disk root
// contains a matching SKILL.md.
//
//go:embed corpus
var CorpusFS embed.FS

// ReadEmbedded reads a file from the embedded corpus by its path relative to the
// skills package directory (e.g. "corpus/chatgpt.com/SKILL.md").
// Returns the raw bytes or an error if not found.
func ReadEmbedded(path string) ([]byte, error) {
	return fs.ReadFile(CorpusFS, path)
}
