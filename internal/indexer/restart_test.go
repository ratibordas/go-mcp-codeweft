package indexer

import (
	"slices"
	"testing"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

func TestMetadataFromActiveRebuildsGoReverseImportsByPackage(t *testing.T) {
	manifest := map[string]core.FileState{
		"lib/lib.go":      {Path: "lib/lib.go", Extension: ".go"},
		"app/app.go":      {Path: "app/app.go", Extension: ".go"},
		"app/app_test.go": {Path: "app/app_test.go", Extension: ".go"},
	}
	units := []core.CodeUnit{
		{ID: "lib-package", Kind: "package", Language: "go", QualifiedName: "example/lib"},
		{ID: "app-package", Kind: "package", Language: "go", QualifiedName: "example/app"},
		{ID: "test-package", Kind: "package", Language: "go", QualifiedName: "example/app_test"},
		{ID: "lib-file", Kind: "file", Language: "go", Extension: ".go", Path: "lib/lib.go"},
		{ID: "app-file", Kind: "file", Language: "go", Extension: ".go", Path: "app/app.go"},
		{ID: "test-file", Kind: "file", Language: "go", Extension: ".go", Path: "app/app_test.go"},
	}
	edges := []core.CodeEdge{
		{SourceID: "lib-package", TargetID: "lib-file", Relation: "contains"},
		{SourceID: "app-package", TargetID: "app-file", Relation: "contains"},
		{SourceID: "test-package", TargetID: "test-file", Relation: "contains"},
		{SourceID: "app-file", TargetID: "lib-package", Relation: "imports"},
		{SourceID: "test-file", TargetID: "app-package", Relation: "imports"},
	}

	metadata := metadataFromActive(manifest, units, edges)
	if metadata.GoFilePackage["app/app.go"] != "example/app" || metadata.GoFilePackage["app/app_test.go"] != "example/app_test" {
		t.Fatalf("file ownership = %#v", metadata.GoFilePackage)
	}
	if !slices.Equal(metadata.GoReverseImport["example/lib"], []string{"example/app"}) || !slices.Equal(metadata.GoReverseImport["example/app"], []string{"example/app_test"}) {
		t.Fatalf("reverse imports = %#v", metadata.GoReverseImport)
	}
}

func TestRunStatusRestoresSuccessfulTargetGeneration(t *testing.T) {
	manifest := map[string]core.FileState{"a.go": {Path: "a.go", Generation: 3}}
	ready := runStatus(RunSnapshot{State: "ready", StartGeneration: 3, TargetGeneration: 5}, manifest)
	if ready.ActiveGeneration != 5 {
		t.Fatalf("ready active generation = %d, want 5", ready.ActiveGeneration)
	}
	pending := runStatus(RunSnapshot{State: "degraded", StartGeneration: 5, TargetGeneration: 6, Pending: []string{"a.go"}}, manifest)
	if pending.ActiveGeneration != 5 {
		t.Fatalf("pending active generation = %d, want 5", pending.ActiveGeneration)
	}
}
