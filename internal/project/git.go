package project

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

type gitStatus struct {
	Paths   []string
	Renames []Rename
}

func gitFiles(ctx context.Context, root string) ([]string, string) {
	out, err := gitOutput(ctx, root, "ls-files", "-co", "--exclude-standard", "-z")
	if err != nil {
		return nil, "git discovery unavailable: " + err.Error()
	}
	paths := []string{}
	for _, path := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if path == "" {
			continue
		}
		path, err := safePath(path)
		if err != nil {
			continue
		}
		paths = append(paths, path)
	}
	return uniquePaths(paths), ""
}

func gitOutput(ctx context.Context, root string, args ...string) ([]byte, error) {
	args = append([]string{"-C", root}, args...)
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

func gitHead(ctx context.Context, root string) (string, error) {
	out, err := gitOutput(ctx, root, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitStatusPaths(ctx context.Context, root string) (gitStatus, error) {
	out, err := gitOutput(ctx, root, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil {
		return gitStatus{}, err
	}
	return parsePorcelainV2(out)
}

func gitDiff(ctx context.Context, root, oldHead, newHead string) (gitStatus, error) {
	if oldHead == "" || newHead == "" || oldHead == newHead {
		return gitStatus{}, nil
	}
	if !fullObjectID(oldHead) {
		return gitStatus{}, fmt.Errorf("invalid recorded Git head")
	}
	out, err := gitOutput(ctx, root, "diff", "--name-status", "-z", oldHead, newHead, "--")
	if err != nil {
		return gitStatus{}, err
	}
	return parseNameStatus(out)
}

func fullObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f' || character >= 'A' && character <= 'F') {
			return false
		}
	}
	return true
}

func parsePorcelainV2(input []byte) (gitStatus, error) {
	parts := strings.Split(string(input), "\x00")
	status := gitStatus{}
	for i := 0; i < len(parts); i++ {
		record := parts[i]
		if record == "" || record[0] == '#' || record[0] == '!' {
			continue
		}
		if record[0] == '?' {
			path, err := safePath(strings.TrimPrefix(record, "? "))
			if err != nil {
				return gitStatus{}, err
			}
			status.Paths = append(status.Paths, path)
			continue
		}
		fields := strings.Fields(record)
		if len(fields) < 2 {
			return gitStatus{}, fmt.Errorf("invalid porcelain record %q", record)
		}
		if fields[0] == "2" {
			if i+1 >= len(parts) || parts[i+1] == "" {
				return gitStatus{}, fmt.Errorf("rename missing original path")
			}
			newPath, err := porcelainPath(record, 9)
			if err != nil {
				return gitStatus{}, err
			}
			oldPath, err := safePath(parts[i+1])
			if err != nil {
				return gitStatus{}, err
			}
			status.Paths = append(status.Paths, newPath, oldPath)
			status.Renames = append(status.Renames, Rename{Old: oldPath, New: newPath})
			i++
			continue
		}
		if fields[0] != "1" && fields[0] != "u" {
			return gitStatus{}, fmt.Errorf("unsupported porcelain record %q", record)
		}
		fieldCount := 8
		if fields[0] == "u" {
			fieldCount = 10
		}
		path, err := porcelainPath(record, fieldCount)
		if err != nil {
			return gitStatus{}, err
		}
		status.Paths = append(status.Paths, path)
	}
	sort.Strings(status.Paths)
	sortRenames(status.Renames)
	return status, nil
}

func porcelainPath(record string, fields int) (string, error) {
	for i := 0; i < fields; i++ {
		space := strings.IndexByte(record, ' ')
		if space < 0 {
			return "", fmt.Errorf("invalid porcelain record %q", record)
		}
		record = record[space+1:]
	}
	return safePath(record)
}

func parseNameStatus(input []byte) (gitStatus, error) {
	parts := strings.Split(string(input), "\x00")
	status := gitStatus{}
	for i := 0; i < len(parts)-1; {
		kind := parts[i]
		i++
		if kind == "" {
			continue
		}
		if i >= len(parts) || parts[i] == "" {
			return gitStatus{}, fmt.Errorf("diff record missing path")
		}
		first, err := safePath(parts[i])
		if err != nil {
			return gitStatus{}, err
		}
		i++
		if strings.HasPrefix(kind, "R") || strings.HasPrefix(kind, "C") {
			if i >= len(parts) || parts[i] == "" {
				return gitStatus{}, fmt.Errorf("rename missing new path")
			}
			second, err := safePath(parts[i])
			if err != nil {
				return gitStatus{}, err
			}
			i++
			status.Paths = append(status.Paths, first, second)
			if strings.HasPrefix(kind, "R") {
				status.Renames = append(status.Renames, Rename{Old: first, New: second})
			}
			continue
		}
		status.Paths = append(status.Paths, first)
	}
	sort.Strings(status.Paths)
	sortRenames(status.Renames)
	return status, nil
}

func sortRenames(renames []Rename) {
	sort.Slice(renames, func(i, j int) bool {
		if renames[i].Old == renames[j].Old {
			return renames[i].New < renames[j].New
		}
		return renames[i].Old < renames[j].Old
	})
}
