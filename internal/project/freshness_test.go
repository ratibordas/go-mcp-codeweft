package project

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

func TestPlanSkipsMetadataOnlyChangeWithSameHash(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", "package main\n")
	path := filepath.Join(root, "main.go")
	if err := os.Chtimes(path, testTime, testTime); err != nil {
		t.Fatal(err)
	}
	first, err := Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]core.FileState{"main.go": {
		Path: "main.go", Size: first[0].Size, MTimeNS: first[0].MTimeNS - 1, Hash: first[0].Hash, ParserVersion: ParserVersion,
	}}

	plan, err := Plan(context.Background(), root, "", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changed) != 0 || len(plan.Deleted) != 0 {
		t.Fatalf("metadata-only update should be skipped: %+v", plan)
	}
}

func TestPlanFindsDeletedAndChangedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "changed.go", "package changed\n")
	files, err := Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]core.FileState{
		"changed.go": {Path: "changed.go", Size: files[0].Size, MTimeNS: files[0].MTimeNS, Hash: "old", ParserVersion: ParserVersion},
		"gone.go":    {Path: "gone.go", Hash: "gone", ParserVersion: ParserVersion},
	}
	writeFile(t, root, "changed.go", "package changed\n\n// changed\n")
	plan, err := Plan(context.Background(), root, "", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changed) != 1 || plan.Changed[0].Path != "changed.go" || len(plan.Deleted) != 1 || plan.Deleted[0] != "gone.go" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestPlanUsesGitDiffForCommittedRename(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "config", "user.email", "test@example.com")
	git(t, root, "config", "user.name", "Test")
	writeFile(t, root, "old name.ts", "export const name = 'old'\n")
	git(t, root, "add", "old name.ts")
	git(t, root, "commit", "-m", "old")
	head, err := gitHead(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	git(t, root, "mv", "old name.ts", "new name.ts")
	git(t, root, "commit", "-m", "rename")

	plan, err := Plan(context.Background(), root, head, map[string]core.FileState{
		"old name.ts": {Path: "old name.ts", Hash: files[0].Hash, ParserVersion: ParserVersion},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.UsedGit || len(plan.Changed) != 1 || plan.Changed[0].Path != "new name.ts" || len(plan.Deleted) != 1 || plan.Deleted[0] != "old name.ts" || len(plan.Renames) != 1 || plan.Renames[0] != (Rename{Old: "old name.ts", New: "new name.ts"}) {
		t.Fatalf("unexpected plan: %+v", plan)
	}
}

func TestPlanFallsBackToWalkAfterGitProbeFailure(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "listed.go", "package listed\n")
	writeFile(t, root, "walked.go", "package walked\n")
	files, err := Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := make(map[string]core.FileState, len(files))
	for _, file := range files {
		manifest[file.Path] = core.FileState{Path: file.Path, Size: file.Size, MTimeNS: file.MTimeNS, Hash: file.Hash, ParserVersion: ParserVersion}
	}
	for _, probe := range []string{"rev-parse", "status", "diff"} {
		t.Run(probe, func(t *testing.T) {
			fakeGit(t, "listed.go", probe)
			plan, err := Plan(context.Background(), root, "old", manifest)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Deleted) != 0 {
				t.Fatalf("Git probe fallback lost filesystem candidates: %+v", plan)
			}
		})
	}
}

func TestPlanRehashesPreviousDirtyPathAfterItBecomesClean(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "config", "user.email", "test@example.com")
	git(t, root, "config", "user.name", "Test")
	writeFile(t, root, "main.go", "package main\n")
	writeFile(t, root, "other.go", "package other\n")
	git(t, root, "add", "main.go", "other.go")
	git(t, root, "commit", "-m", "initial")
	files, err := Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := make(map[string]core.FileState, len(files))
	var main File
	for _, file := range files {
		manifest[file.Path] = core.FileState{Path: file.Path, Size: file.Size, MTimeNS: file.MTimeNS, Hash: file.Hash, ParserVersion: ParserVersion}
		if file.Path == "main.go" {
			main = file
		}
	}
	manifest["main.go"] = core.FileState{Path: "main.go", Size: main.Size, MTimeNS: main.MTimeNS, Hash: "old hash", ParserVersion: ParserVersion}
	writeFile(t, root, "other.go", "package other\n// dirty\n")

	plan, err := PlanWithInput(context.Background(), root, PlanInput{Manifest: manifest, DirtyPaths: []string{"main.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changed) != 2 || plan.Changed[0].Path != "main.go" || plan.Changed[0].Hash != main.Hash || len(plan.DirtyPaths) != 2 || plan.DirtyPaths[0] != "main.go" || plan.DirtyPaths[1] != "other.go" {
		t.Fatalf("previous dirty path was not rehashed: %+v", plan)
	}
}

func TestPlanRejectsOptionShapedRecordedHeadWithoutWritingTarget(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "config", "user.email", "test@example.com")
	git(t, root, "config", "user.name", "Test")
	writeFile(t, root, "main.go", "package main\n")
	git(t, root, "add", "main.go")
	git(t, root, "commit", "-m", "initial")
	files, err := Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	file := files[0]
	manifest := map[string]core.FileState{"main.go": {
		Path: "main.go", Size: file.Size, MTimeNS: file.MTimeNS, Hash: file.Hash, ParserVersion: ParserVersion,
	}}
	target := filepath.Join(t.TempDir(), "target")

	plan, err := Plan(context.Background(), root, "--output="+target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("invalid head created target: %v", err)
	}
	if plan.UsedGit || len(plan.Changed) != 0 || len(plan.Deleted) != 0 || !strings.Contains(strings.Join(plan.Warnings, "\n"), "invalid recorded Git head") {
		t.Fatalf("invalid head did not safely fall back: %+v", plan)
	}
}

func TestPlanDeduplicatesDirtyPaths(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "config", "user.email", "test@example.com")
	git(t, root, "config", "user.name", "Test")
	writeFile(t, root, "main.go", "package main\n")
	git(t, root, "add", "main.go")
	git(t, root, "commit", "-m", "initial")
	files, err := Discover(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	file := files[0]
	manifest := map[string]core.FileState{"main.go": {
		Path: "main.go", Size: file.Size, MTimeNS: file.MTimeNS, Hash: file.Hash, ParserVersion: ParserVersion,
	}}

	plan, err := PlanWithInput(context.Background(), root, PlanInput{Manifest: manifest, DirtyPaths: []string{"main.go", "main.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.DirtyPaths) != 1 || plan.DirtyPaths[0] != "main.go" {
		t.Fatalf("duplicate dirty paths: %+v", plan.DirtyPaths)
	}
}
