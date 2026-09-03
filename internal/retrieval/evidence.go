package retrieval

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

func extractEvidence(root string, candidate core.Candidate, manifests ...map[string]core.FileState) (core.Evidence, error) {
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return core.Evidence{}, fmt.Errorf("resolve project root: %w", err)
	}
	canonicalRoot, err = filepath.Abs(canonicalRoot)
	if err != nil {
		return core.Evidence{}, fmt.Errorf("resolve project root: %w", err)
	}
	relative := filepath.Clean(filepath.FromSlash(candidate.Path))
	if candidate.Path == "" || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return core.Evidence{}, fmt.Errorf("evidence path %q escapes project root", candidate.Path)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(canonicalRoot, relative))
	if err != nil {
		return core.Evidence{}, fmt.Errorf("resolve evidence path %q: %w", candidate.Path, err)
	}
	rel, err := filepath.Rel(canonicalRoot, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return core.Evidence{}, fmt.Errorf("evidence path %q escapes project root", candidate.Path)
	}
	if len(manifests) != 0 {
		active, ok := manifests[0][candidate.Path]
		if !ok || active.Deleted || active.Hash == "" || active.Hash != candidate.FileHash {
			return core.Evidence{}, fmt.Errorf("evidence path %q is inactive", candidate.Path)
		}
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return core.Evidence{}, fmt.Errorf("read evidence path %q: %w", candidate.Path, err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(data))
	if candidate.FileHash == "" || sum != candidate.FileHash {
		return core.Evidence{}, fmt.Errorf("evidence path %q changed after retrieval", candidate.Path)
	}
	content := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(content, "\n")
	if candidate.StartLine == 0 || candidate.EndLine < candidate.StartLine || uint64(candidate.EndLine) > uint64(len(lines)) {
		return core.Evidence{}, fmt.Errorf("evidence path %q has invalid line range %d..%d", candidate.Path, candidate.StartLine, candidate.EndLine)
	}
	text := strings.Join(lines[candidate.StartLine-1:candidate.EndLine], "\n")
	extension := candidate.Extension
	if extension == "" {
		extension = filepath.Ext(candidate.Path)
	}
	relation := candidate.Relation
	if relation == "" {
		relation = candidate.Match
	}
	evidence := core.Evidence{
		ID: candidate.ID, Type: "code", Language: candidate.Language, Extension: extension,
		Path: candidate.Path, Symbol: candidate.Symbol, Relation: relation,
		StartLine: candidate.StartLine, EndLine: candidate.EndLine,
	}
	if extension == ".md" || candidate.Type == "doc" || candidate.Type == "documentation" {
		evidence.Type = "documentation"
		evidence.Format = "markdown"
		evidence.Heading = candidate.Heading
		evidence.Quote = text
	} else {
		evidence.Snippet = text
	}
	return evidence, nil
}
