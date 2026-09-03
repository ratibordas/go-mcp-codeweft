package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/indexer"
	"github.com/ratibordas/go-mcp-codeweft/internal/project"
)

func TestProjectIDUsesCanonicalRoot(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "project")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	first, err := ProjectID(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProjectID(link)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 64 {
		t.Fatalf("ids = %q %q", first, second)
	}
}

func TestInitialModeUsesFullOnlyWithoutCompatibleManifest(t *testing.T) {
	if initialMode(nil) != indexer.Full {
		t.Fatal("empty manifest did not request full indexing")
	}
	compatible := map[string]core.FileState{"a.go": {Path: "a.go", ParserVersion: project.ParserVersion}}
	if initialMode(compatible) != indexer.Delta {
		t.Fatal("compatible manifest did not request delta indexing")
	}
	compatible["a.go"] = core.FileState{Path: "a.go", ParserVersion: "old"}
	if initialMode(compatible) != indexer.Full {
		t.Fatal("parser mismatch did not request full indexing")
	}
}
