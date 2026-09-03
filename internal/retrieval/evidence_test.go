package retrieval

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

func TestEvidenceExtractsCurrentCodeAndMarkdown(t *testing.T) {
	root := t.TempDir()
	writeEvidenceFile(t, root, "code.go", "one\r\ntwo\r\nthree\r\n")
	writeEvidenceFile(t, root, "docs.md", "# API\nExact quote.\n")
	manifest := evidenceManifest(root, "code.go", "docs.md")
	code, err := extractEvidence(root, core.Candidate{ID: "C1", Type: "code", Language: "go", Path: "code.go", FileHash: manifest["code.go"].Hash, StartLine: 2, EndLine: 3}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := extractEvidence(root, core.Candidate{ID: "D1", Type: "doc", Path: "docs.md", Heading: "API", FileHash: manifest["docs.md"].Hash, StartLine: 1, EndLine: 2}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if code.Snippet != "two\nthree" || code.Quote != "" || doc.Quote != "# API\nExact quote." || doc.Snippet != "" || doc.Format != "markdown" {
		t.Fatalf("code=%+v doc=%+v", code, doc)
	}
}

func TestEvidenceRejectsStaleInactiveRangeAndEscape(t *testing.T) {
	root := t.TempDir()
	writeEvidenceFile(t, root, "docs.md", "old\n")
	hash := fileHash(t, filepath.Join(root, "docs.md"))
	candidate := core.Candidate{ID: "D1", Type: "doc", Path: "docs.md", FileHash: hash, StartLine: 1, EndLine: 1}
	writeEvidenceFile(t, root, "docs.md", "new\n")
	if _, err := extractEvidence(root, candidate, map[string]core.FileState{"docs.md": {Path: "docs.md", Hash: hash}}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("stale error = %v", err)
	}
	candidate.FileHash = fileHash(t, filepath.Join(root, "docs.md"))
	if _, err := extractEvidence(root, candidate, map[string]core.FileState{"docs.md": {Path: "docs.md", Hash: candidate.FileHash, Deleted: true}}); err == nil || !strings.Contains(err.Error(), "inactive") {
		t.Fatalf("inactive error = %v", err)
	}
	candidate.EndLine = 3
	if _, err := extractEvidence(root, candidate, map[string]core.FileState{"docs.md": {Path: "docs.md", Hash: candidate.FileHash}}); err == nil || !strings.Contains(err.Error(), "line range") {
		t.Fatalf("range error = %v", err)
	}
	candidate.Path, candidate.StartLine, candidate.EndLine = "../docs.md", 1, 1
	if _, err := extractEvidence(root, candidate); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("escape error = %v", err)
	}
}

func writeEvidenceFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func evidenceManifest(root string, paths ...string) map[string]core.FileState {
	result := map[string]core.FileState{}
	for _, path := range paths {
		data, _ := os.ReadFile(filepath.Join(root, path))
		sum := sha256.Sum256(data)
		result[path] = core.FileState{Path: path, Hash: fmt.Sprintf("%x", sum)}
	}
	return result
}

func fileHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}
