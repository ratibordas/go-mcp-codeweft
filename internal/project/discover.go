package project

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ratibordas/go-mcp-codeweft/internal/config"
)

const defaultMaxFileBytes int64 = 2 << 20

var generatedGo = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

type File struct {
	Path, Kind, Language, Extension, Hash string
	Size, MTimeNS                         int64
}

var extensions = map[string][2]string{
	".adoc": {"document", "asciidoc"}, ".bash": {"code", "bash"}, ".c": {"code", "c"}, ".cc": {"code", "cpp"},
	".cpp": {"code", "cpp"}, ".cs": {"code", "csharp"}, ".css": {"code", "css"}, ".go": {"code", "go"},
	".h": {"code", "c"}, ".hpp": {"code", "cpp"}, ".html": {"code", "html"}, ".java": {"code", "java"},
	".js": {"code", "javascript"}, ".json": {"document", "json"}, ".jsx": {"code", "javascript"}, ".kt": {"code", "kotlin"},
	".cjs": {"code", "javascript"},
	".kts": {"code", "kotlin"}, ".lua": {"code", "lua"}, ".md": {"document", "markdown"}, ".mdx": {"document", "markdown"},
	".m": {"code", "objective-c"}, ".mm": {"code", "objective-cpp"}, ".php": {"code", "php"}, ".py": {"code", "python"},
	".mjs": {"code", "javascript"},
	".rb":  {"code", "ruby"}, ".rs": {"code", "rust"}, ".rst": {"document", "rst"}, ".scala": {"code", "scala"},
	".sh": {"code", "shell"}, ".sql": {"code", "sql"}, ".swift": {"code", "swift"}, ".toml": {"document", "toml"},
	".ts": {"code", "typescript"}, ".tsx": {"code", "typescript"}, ".txt": {"document", "text"}, ".xml": {"document", "xml"},
	".yaml": {"document", "yaml"}, ".yml": {"document", "yaml"}, ".zsh": {"code", "shell"},
}

func Discover(ctx context.Context, root string) ([]File, error) {
	return DiscoverWithIndex(ctx, root, config.Index{})
}

func DiscoverWithIndex(ctx context.Context, root string, index config.Index) ([]File, error) {
	files, _, err := discover(ctx, root, index)
	return files, err
}

// InspectWithIndex applies the same root, exclusion, size, and content policy
// used by discovery to one project-relative candidate.
func InspectWithIndex(root, path string, index config.Index) (File, string, error) {
	root, err := canonicalRoot(root)
	if err != nil {
		return File{}, "", err
	}
	file, reason := inspectFile(root, path, index, false)
	return file, reason, nil
}

func discover(ctx context.Context, root string, index config.Index) ([]File, []string, error) {
	root, err := canonicalRoot(root)
	if err != nil {
		return nil, nil, err
	}
	paths, warnings, err := candidatePaths(ctx, root)
	if err != nil {
		return nil, warnings, err
	}
	files := make([]File, 0, len(paths))
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, warnings, err
		}
		file, reason := inspectFile(root, path, index, true)
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

func discoverMeta(ctx context.Context, root string, index config.Index) ([]File, []string, error) {
	root, err := canonicalRoot(root)
	if err != nil {
		return nil, nil, err
	}
	paths, warnings, err := candidatePaths(ctx, root)
	if err != nil {
		return nil, warnings, err
	}
	files := make([]File, 0, len(paths))
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

func candidatePaths(ctx context.Context, root string) ([]string, []string, error) {
	paths, warning := gitFiles(ctx, root)
	if warning == "" {
		return paths, nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	paths, err := walkFiles(ctx, root)
	if err != nil {
		return nil, []string{warning}, err
	}
	return paths, []string{warning}, nil
}

func walkFiles(ctx context.Context, root string) ([]string, error) {
	paths := []string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := projectPath(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() && fixedExcludedDir(entry.Name()) {
			return filepath.SkipDir
		}
		if !entry.IsDir() {
			paths = append(paths, rel)
		}
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func inspectFile(root, path string, index config.Index, withHash bool) (File, string) {
	path, err := safePath(path)
	if err != nil {
		return File{}, err.Error()
	}
	if reason := excluded(path, index); reason != "" {
		return File{}, reason
	}
	kindLanguage, ok := extensions[strings.ToLower(filepath.Ext(path))]
	if !ok {
		return File{}, "unsupported extension"
	}
	full, err := resolveInsideRoot(root, path)
	if err != nil {
		return File{}, err.Error()
	}
	info, err := os.Stat(full)
	if err != nil {
		return File{}, err.Error()
	}
	if !info.Mode().IsRegular() {
		return File{}, "not a regular file"
	}
	limit := index.MaxFileBytes
	if limit == 0 {
		limit = defaultMaxFileBytes
	}
	if info.Size() > limit {
		return File{}, "file exceeds size limit"
	}
	if reason, err := contentReason(full, strings.EqualFold(filepath.Ext(path), ".go")); err != nil {
		return File{}, err.Error()
	} else if reason != "" {
		return File{}, reason
	}
	file := File{Path: path, Kind: kindLanguage[0], Language: kindLanguage[1], Extension: strings.ToLower(filepath.Ext(path)), Size: info.Size(), MTimeNS: info.ModTime().UnixNano()}
	if withHash {
		file.Hash, err = fileHash(full)
		if err != nil {
			return File{}, err.Error()
		}
	}
	return file, ""
}

func contentReason(path string, isGo bool) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	buf := make([]byte, 8192)
	n, err := file.Read(buf)
	if err != nil && err != io.EOF {
		return "", err
	}
	buf = buf[:n]
	if strings.IndexByte(string(buf), 0) >= 0 {
		return "contains NUL byte", nil
	}
	if isGo && generatedGo.Match(buf) {
		return "generated Go source", nil
	}
	return "", nil
}

func fileHash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func excluded(path string, index config.Index) string {
	base := filepath.Base(path)
	if fixedExcludedDir(base) || fixedExcludedFile(base) || secretName(base) {
		return "excluded path or secret name"
	}
	parts := strings.Split(filepath.ToSlash(path), "/")
	for _, part := range parts[:len(parts)-1] {
		if fixedExcludedDir(part) || contains(index.ExcludeDirNames, part) {
			return "excluded directory"
		}
	}
	if contains(index.ExcludeFileNames, base) {
		return "excluded file name"
	}
	for _, excluded := range index.ExcludePaths {
		excluded, err := safePath(excluded)
		if err == nil && (path == excluded || strings.HasPrefix(path, excluded+"/")) {
			return "excluded configured path"
		}
	}
	if !index.IncludeTests && testName(base) {
		return "test file"
	}
	return ""
}

func resolveInsideRoot(root, path string) (string, error) {
	root, err := canonicalRoot(root)
	if err != nil {
		return "", err
	}
	path, err = safePath(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path escapes project root")
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if fixedExcludedDir(part) {
			return "", fmt.Errorf("path resolves into excluded directory")
		}
	}
	return resolved, nil
}

func canonicalRoot(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("project root must be absolute")
	}
	return filepath.EvalSymlinks(root)
}

func projectPath(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	return safePath(rel)
}

func safePath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("path is absolute")
	}
	path = filepath.ToSlash(filepath.Clean(path))
	if path == "." || path == ".." || strings.HasPrefix(path, "../") {
		return "", fmt.Errorf("path escapes project root")
	}
	return path, nil
}

func fixedExcludedDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", ".cache", ".idea", ".mypy_cache", ".next", ".nuxt", ".parcel-cache", ".pytest_cache", ".ruff_cache", ".turbo", ".vscode", "__pycache__", "build", "coverage", "dist", "node_modules", "out", "target", "vendor":
		return true
	}
	return false
}

func fixedExcludedFile(name string) bool {
	return strings.HasSuffix(name, ".min.js") || strings.HasSuffix(name, ".min.mjs") || strings.HasSuffix(name, ".min.cjs") || strings.HasSuffix(name, ".map")
}

func secretName(name string) bool {
	return name == ".env" || strings.HasPrefix(name, ".env.") || name == "id_rsa" || name == "id_ed25519" || name == "credentials" || strings.HasPrefix(name, "credentials.") || strings.HasSuffix(name, ".pem") || strings.HasSuffix(name, ".key") || strings.HasSuffix(name, ".p12")
}

func testName(name string) bool {
	return strings.HasSuffix(name, "_test.go") || strings.HasSuffix(name, ".test.js") || strings.HasSuffix(name, ".test.jsx") || strings.HasSuffix(name, ".test.ts") || strings.HasSuffix(name, ".test.tsx") || strings.HasSuffix(name, ".spec.js") || strings.HasSuffix(name, ".spec.jsx") || strings.HasSuffix(name, ".spec.ts") || strings.HasSuffix(name, ".spec.tsx") || strings.HasPrefix(name, "test_")
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func sortFiles(files []File) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
}
