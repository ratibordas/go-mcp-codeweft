package tsparser

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var scriptExtensions = []string{".ts", ".tsx", ".d.ts", ".js", ".jsx", ".mjs", ".cjs"}

type pathRule struct {
	pattern string
	targets []string
}

type resolver struct {
	root       string
	baseURL    string
	useBaseURL bool
	paths      []pathRule
	allowed    map[string]bool
	policy     func(string) bool
	warnings   map[string]bool
}

func (r *resolver) withPolicy(policy func(string) bool) *resolver {
	r.policy = policy
	return r
}

func (r *resolver) withAllowed(paths []string) *resolver {
	r.allowed = make(map[string]bool, len(paths))
	for _, path := range paths {
		r.allowed[path] = true
	}
	return r
}

func (r *resolver) Warnings() []string {
	result := make([]string, 0, len(r.warnings))
	for warning := range r.warnings {
		result = append(result, warning)
	}
	sort.Strings(result)
	return result
}

type compilerConfig struct {
	CompilerOptions struct {
		BaseURL string              `json:"baseUrl"`
		Paths   map[string][]string `json:"paths"`
	} `json:"compilerOptions"`
}

type packageConfig struct {
	Main    string          `json:"main"`
	Module  string          `json:"module"`
	Types   string          `json:"types"`
	Exports json.RawMessage `json:"exports"`
}

func newResolver(root string) (*resolver, error) {
	root, err := canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	r := &resolver{root: root, warnings: map[string]bool{}}
	for _, name := range []string{"tsconfig.json", "jsconfig.json"} {
		var config compilerConfig
		if err := readSafeJSON(root, name, &config); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		if config.CompilerOptions.BaseURL != "" {
			r.useBaseURL = true
			base := filepath.ToSlash(filepath.Clean(config.CompilerOptions.BaseURL))
			if base == "." {
				base = ""
			} else if clean, err := safePath(base); err != nil {
				return nil, fmt.Errorf("unsafe baseUrl: %w", err)
			} else {
				base = clean
			}
			r.baseURL = base
		}
		patterns := make([]string, 0, len(config.CompilerOptions.Paths))
		for pattern := range config.CompilerOptions.Paths {
			patterns = append(patterns, pattern)
		}
		sort.Slice(patterns, func(i, j int) bool {
			leftStar, rightStar := strings.IndexByte(patterns[i], '*'), strings.IndexByte(patterns[j], '*')
			if (leftStar < 0) != (rightStar < 0) {
				return leftStar < 0
			}
			leftPrefix, rightPrefix := patterns[i], patterns[j]
			if leftStar >= 0 {
				leftPrefix = leftPrefix[:leftStar]
			}
			if rightStar >= 0 {
				rightPrefix = rightPrefix[:rightStar]
			}
			if len(leftPrefix) != len(rightPrefix) {
				return len(leftPrefix) > len(rightPrefix)
			}
			return patterns[i] < patterns[j]
		})
		for _, pattern := range patterns {
			r.paths = append(r.paths, pathRule{pattern: pattern, targets: append([]string(nil), config.CompilerOptions.Paths[pattern]...)})
		}
		break
	}
	return r, nil
}

func (r *resolver) Resolve(importer, specifier string) (string, bool) {
	importer, err := safePath(importer)
	if err != nil || specifier == "" || filepath.IsAbs(specifier) || strings.HasPrefix(specifier, "/") {
		return "", false
	}
	var candidates []string
	if strings.HasPrefix(specifier, ".") {
		candidate, err := safeCandidatePath(filepath.ToSlash(filepath.Join(filepath.Dir(importer), filepath.FromSlash(specifier))))
		if err != nil {
			return "", false
		}
		candidates = append(candidates, candidate)
	} else {
		matchedRule := false
		for _, rule := range r.paths {
			match, ok := wildcardMatch(rule.pattern, specifier)
			if !ok {
				continue
			}
			matchedRule = true
			for _, target := range rule.targets {
				target = strings.ReplaceAll(target, "*", match)
				candidate, err := safeCandidatePath(filepath.ToSlash(filepath.Join(filepath.FromSlash(r.baseURL), filepath.FromSlash(target))))
				if err == nil {
					candidates = append(candidates, candidate)
				}
			}
			break
		}
		if r.useBaseURL && !matchedRule {
			candidate, err := safeCandidatePath(filepath.ToSlash(filepath.Join(filepath.FromSlash(r.baseURL), filepath.FromSlash(specifier))))
			if err == nil {
				candidates = append(candidates, candidate)
			}
		}
	}
	for _, candidate := range candidates {
		if resolved, ok := r.resolveCandidate(candidate, map[string]bool{}); ok {
			return resolved, true
		}
	}
	return "", false
}

func wildcardMatch(pattern, value string) (string, bool) {
	star := strings.IndexByte(pattern, '*')
	if star < 0 {
		return "", pattern == value
	}
	if strings.IndexByte(pattern[star+1:], '*') >= 0 {
		return "", false
	}
	prefix, suffix := pattern[:star], pattern[star+1:]
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, suffix) || len(value) < len(prefix)+len(suffix) {
		return "", false
	}
	return value[len(prefix) : len(value)-len(suffix)], true
}

func (r *resolver) resolveCandidate(candidate string, seen map[string]bool) (string, bool) {
	candidate, err := safeCandidatePath(candidate)
	if err != nil || containsPathPart(candidate, "node_modules") || seen[candidate] {
		return "", false
	}
	seen[candidate] = true
	if sourceExtension(candidate) != "" && r.allowedPath(candidate) && r.safeRegularFile(candidate) {
		return candidate, true
	}
	if sourceExtension(candidate) == "" {
		for _, extension := range scriptExtensions {
			path := candidate + extension
			if r.allowedPath(path) && r.safeRegularFile(path) {
				return path, true
			}
		}
	}
	if !r.safeDirectory(candidate) {
		return "", false
	}
	var config packageConfig
	if err := readSafeJSON(r.root, candidate+"/package.json", &config); err == nil {
		targets := packageTargets(config)
		for _, target := range targets {
			cleanTarget, ok := safePackageTarget(target)
			if !ok {
				r.warnings[candidate+"/package.json: unsafe package target "+target] = true
				continue
			}
			path, err := safeCandidatePath(filepath.ToSlash(filepath.Join(filepath.FromSlash(candidate), filepath.FromSlash(cleanTarget))))
			if err != nil {
				continue
			}
			if resolved, ok := r.resolvePackageTarget(candidate, path, target, seen); ok {
				return resolved, true
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		r.warnings[candidate+"/package.json: "+err.Error()] = true
	}
	for _, extension := range scriptExtensions {
		path := candidate + "/index" + extension
		if r.allowedPath(path) && r.safeRegularFile(path) {
			return path, true
		}
	}
	return "", false
}

func (r *resolver) resolvePackageTarget(packageDir, target, declared string, seen map[string]bool) (string, bool) {
	relative, err := filepath.Rel(filepath.FromSlash(packageDir), filepath.FromSlash(target))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		r.warnings[packageDir+"/package.json: unsafe package target "+declared] = true
		return "", false
	}
	return r.resolveContainedPackageCandidate(packageDir, target, declared, seen)
}

func (r *resolver) resolveContainedPackageCandidate(packageDir, candidate, declared string, seen map[string]bool) (string, bool) {
	candidate, err := safeCandidatePath(candidate)
	if err != nil || seen[candidate] {
		return "", false
	}
	seen[candidate] = true
	if sourceExtension(candidate) != "" {
		if r.containedPackageFile(packageDir, candidate, declared) {
			return candidate, true
		}
		return "", false
	}
	candidates := make([]string, 0, len(scriptExtensions))
	for _, extension := range scriptExtensions {
		candidates = append(candidates, candidate+extension)
	}
	for _, probe := range candidates {
		if r.containedPackageFile(packageDir, probe, declared) {
			return probe, true
		}
	}
	if !r.containedPackageDirectory(packageDir, candidate, declared) {
		return "", false
	}
	var config packageConfig
	if err := readSafeJSON(r.root, candidate+"/package.json", &config); err == nil {
		for _, target := range packageTargets(config) {
			cleanTarget, ok := safePackageTarget(target)
			if !ok {
				r.warnings[candidate+"/package.json: unsafe package target "+target] = true
				continue
			}
			path, err := safeCandidatePath(filepath.ToSlash(filepath.Join(filepath.FromSlash(candidate), filepath.FromSlash(cleanTarget))))
			if err != nil {
				continue
			}
			if resolved, ok := r.resolvePackageTarget(candidate, path, target, seen); ok {
				return resolved, true
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		r.warnings[candidate+"/package.json: "+err.Error()] = true
	}
	for _, extension := range scriptExtensions {
		if path := candidate + "/index" + extension; r.containedPackageFile(candidate, path, declared) {
			return path, true
		}
	}
	return "", false
}

func (r *resolver) containedPackageFile(packageDir, candidate, declared string) bool {
	if !r.packageTargetInside(packageDir, candidate) {
		r.warnings[packageDir+"/package.json: unsafe package target "+declared] = true
		return false
	}
	return r.allowedPath(candidate) && r.safeRegularFile(candidate)
}

func (r *resolver) containedPackageDirectory(packageDir, candidate, declared string) bool {
	if !r.packageTargetInside(packageDir, candidate) {
		r.warnings[packageDir+"/package.json: unsafe package target "+declared] = true
		return false
	}
	return r.safeDirectory(candidate)
}

func (r *resolver) packageTargetInside(packageDir, candidate string) bool {
	packageRoot, ok := r.safeExisting(packageDir)
	if !ok {
		return false
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(r.root, filepath.FromSlash(candidate)))
	if err != nil {
		return true
	}
	relative, err := filepath.Rel(packageRoot, resolved)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func safePackageTarget(target string) (string, bool) {
	if target == "" || filepath.IsAbs(target) || strings.HasPrefix(target, "/") {
		return "", false
	}
	path, err := safeCandidatePath(target)
	if err != nil || containsPathPart(path, "node_modules") || containsExcludedPath(path) {
		return "", false
	}
	return path, true
}

func containsExcludedPath(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		switch part {
		case ".git", ".hg", ".svn", ".cache", ".idea", ".next", ".nuxt", ".vscode", "build", "coverage", "dist", "node_modules", "out", "target", "vendor":
			return true
		}
	}
	return false
}

func (r *resolver) allowedPath(path string) bool {
	if r.allowed != nil {
		return r.allowed[path]
	}
	return r.policy == nil || r.policy(path)
}

func packageTargets(config packageConfig) []string {
	var exports string
	_ = json.Unmarshal(config.Exports, &exports)
	result := make([]string, 0, 4)
	for _, target := range []string{exports, config.Module, config.Main, config.Types} {
		if target != "" {
			result = append(result, target)
		}
	}
	return result
}

func (r *resolver) safeRegularFile(path string) bool {
	full, ok := r.safeExisting(path)
	if !ok {
		return false
	}
	info, err := os.Stat(full)
	return err == nil && info.Mode().IsRegular()
}

func (r *resolver) safeDirectory(path string) bool {
	full, ok := r.safeExisting(path)
	if !ok {
		return false
	}
	info, err := os.Stat(full)
	return err == nil && info.IsDir()
}

func (r *resolver) safeExisting(path string) (string, bool) {
	path, err := safeCandidatePath(path)
	if err != nil || containsPathPart(path, "node_modules") {
		return "", false
	}
	full, err := filepath.EvalSymlinks(filepath.Join(r.root, filepath.FromSlash(path)))
	if err != nil {
		return "", false
	}
	if full == r.root {
		return full, true
	}
	relative, ok := relativeInside(r.root, full)
	if !ok || containsPathPart(relative, "node_modules") {
		return "", false
	}
	return full, true
}

func safeCandidatePath(path string) (string, error) {
	path = filepath.ToSlash(filepath.Clean(path))
	if path == "." {
		return path, nil
	}
	return safePath(path)
}

func readJSON(path string, value any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("metadata exceeds 1MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func readSafeJSON(root, path string, value any) error {
	path, err := safePath(path)
	if err != nil || containsPathPart(path, "node_modules") {
		return errors.New("unsafe metadata path")
	}
	full, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		return err
	}
	relative, ok := relativeInside(root, full)
	if !ok || containsPathPart(relative, "node_modules") {
		return errors.New("metadata path escapes project root")
	}
	return readJSON(full, value)
}

func canonicalRoot(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", errors.New("project root must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("canonicalize project root: %w", err)
	}
	return resolved, nil
}

func safePath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", errors.New("path is absolute")
	}
	path = filepath.ToSlash(filepath.Clean(path))
	if path == "." || path == ".." || strings.HasPrefix(path, "../") {
		return "", errors.New("path escapes project root")
	}
	return path, nil
}

func relativeInside(root, path string) (string, bool) {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", false
	}
	relative, err = safePath(relative)
	return relative, err == nil
}

func containsPathPart(path, part string) bool {
	for _, current := range strings.Split(filepath.ToSlash(path), "/") {
		if current == part {
			return true
		}
	}
	return false
}

func sourceExtension(path string) string {
	lower := strings.ToLower(path)
	if strings.HasSuffix(lower, ".d.ts") {
		return ".d.ts"
	}
	for _, extension := range scriptExtensions {
		if strings.HasSuffix(lower, extension) {
			return extension
		}
	}
	return ""
}
