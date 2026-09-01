package project

import (
	"context"
	"sort"

	"github.com/ratibordas/go-mcp-codeweft/internal/config"
	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

const ParserVersion = "1"

type ChangePlan struct {
	Changed    []File
	Deleted    []string
	Renames    []Rename
	Head       string
	DirtyPaths []string
	UsedGit    bool
	Warnings   []string
}

type Rename struct {
	Old, New string
}

type PlanInput struct {
	RecordedHead string
	Manifest     map[string]core.FileState
	DirtyPaths   []string
	Index        config.Index
}

func Plan(ctx context.Context, root, recordedHead string, manifest map[string]core.FileState) (ChangePlan, error) {
	return PlanWithInput(ctx, root, PlanInput{RecordedHead: recordedHead, Manifest: manifest})
}

func PlanWithIndex(ctx context.Context, root, recordedHead string, manifest map[string]core.FileState, index config.Index) (ChangePlan, error) {
	return PlanWithInput(ctx, root, PlanInput{RecordedHead: recordedHead, Manifest: manifest, Index: index})
}

func PlanWithInput(ctx context.Context, root string, input PlanInput) (ChangePlan, error) {
	root, err := canonicalRoot(root)
	if err != nil {
		return ChangePlan{}, err
	}
	files, warnings, err := discoverMeta(ctx, root, input.Index)
	if err != nil {
		return ChangePlan{}, err
	}
	plan := ChangePlan{Warnings: warnings}
	for _, path := range input.DirtyPaths {
		path, err := safePath(path)
		if err != nil {
			plan.Warnings = append(plan.Warnings, "previous dirty path: "+err.Error())
			continue
		}
		plan.DirtyPaths = append(plan.DirtyPaths, path)
	}
	gitChanged, gitUsable := applyGit(ctx, root, input.RecordedHead, &plan)
	if !gitUsable {
		files, warnings, err = discoverMetaWalk(ctx, root, input.Index)
		if err != nil {
			return ChangePlan{}, err
		}
		plan.Warnings = append(plan.Warnings, warnings...)
	}
	dirty := make(map[string]bool, len(plan.DirtyPaths)+len(gitChanged))
	for _, path := range plan.DirtyPaths {
		dirty[path] = true
	}
	for _, path := range gitChanged {
		dirty[path] = true
	}
	for _, file := range files {
		old, exists := input.Manifest[file.Path]
		if !gitUsable || !exists || changed(file, old) || dirty[file.Path] {
			file.Hash, err = fileHashFromRoot(root, file.Path)
			if err != nil {
				plan.Warnings = append(plan.Warnings, file.Path+": "+err.Error())
				continue
			}
			if !exists || file.Hash != old.Hash || old.ParserVersion != ParserVersion {
				plan.Changed = append(plan.Changed, file)
			}
		}
	}
	present := make(map[string]bool, len(files))
	for _, file := range files {
		present[file.Path] = true
	}
	for path := range input.Manifest {
		if path, err := safePath(path); err == nil && !present[path] {
			plan.Deleted = append(plan.Deleted, path)
		}
	}
	sortFiles(plan.Changed)
	sort.Strings(plan.Deleted)
	plan.DirtyPaths = uniquePaths(plan.DirtyPaths)
	sort.Strings(plan.DirtyPaths)
	sort.Strings(plan.Warnings)
	sortRenames(plan.Renames)
	return plan, nil
}

func discoverMetaWalk(ctx context.Context, root string, index config.Index) ([]File, []string, error) {
	paths, err := walkFiles(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	files := make([]File, 0, len(paths))
	warnings := []string{}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, warnings, err
		}
		file, reason := inspectFile(root, path, index, false)
		if reason != "" {
			warnings = append(warnings, path+": "+reason)
			continue
		}
		files = append(files, file)
	}
	sortFiles(files)
	sort.Strings(warnings)
	return files, warnings, nil
}

func changed(meta File, old core.FileState) bool {
	return meta.Size != old.Size || meta.MTimeNS != old.MTimeNS || old.ParserVersion != ParserVersion
}

func fileHashFromRoot(root, path string) (string, error) {
	full, err := resolveInsideRoot(root, path)
	if err != nil {
		return "", err
	}
	return fileHash(full)
}

func applyGit(ctx context.Context, root, recordedHead string, plan *ChangePlan) ([]string, bool) {
	head, err := gitHead(ctx, root)
	if err != nil {
		plan.Warnings = append(plan.Warnings, "git head unavailable: "+err.Error())
		return nil, false
	}
	status, err := gitStatusPaths(ctx, root)
	if err != nil {
		plan.Warnings = append(plan.Warnings, "git status unavailable: "+err.Error())
		return nil, false
	}
	diff, err := gitDiff(ctx, root, recordedHead, head)
	if err != nil {
		plan.Warnings = append(plan.Warnings, "git diff unavailable: "+err.Error())
		return nil, false
	}
	plan.Head = head
	plan.UsedGit = true
	plan.DirtyPaths = append(plan.DirtyPaths, status.Paths...)
	plan.Renames = append(plan.Renames, status.Renames...)
	plan.Renames = append(plan.Renames, diff.Renames...)
	plan.Renames = uniqueRenames(plan.Renames)
	return uniquePaths(diff.Paths), true
}

func uniquePaths(paths []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	return result
}

func uniqueRenames(renames []Rename) []Rename {
	seen := map[Rename]bool{}
	result := make([]Rename, 0, len(renames))
	for _, rename := range renames {
		if !seen[rename] {
			seen[rename] = true
			result = append(result, rename)
		}
	}
	return result
}
