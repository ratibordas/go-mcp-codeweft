package graph

import (
	"path"
	"sort"
	"strings"
)

type ChangeMetadata struct {
	GoFilePackage       map[string]string
	GoReverseImport     map[string][]string
	ScriptReverseImport map[string][]string
	SurfaceChanged      map[string]bool
	ResolutionScope     map[string][]string
}

func AffectedGo(changed []string, metadata ChangeMetadata) []string {
	owners := normalizedStrings(metadata.GoFilePackage)
	reverse := normalizedLists(metadata.GoReverseImport)
	surface := normalizedBools(metadata.SurfaceChanged)
	scopes := normalizedLists(metadata.ResolutionScope)
	known := knownGoPackages(owners, reverse)
	affected := map[string]bool{}
	for _, changedPath := range normalizedPaths(changed) {
		if scope, exists := scopes[changedPath]; exists {
			if scope = knownScopePackages(scope, known); len(scope) != 0 {
				addAll(affected, scope)
				addAll(affected, reverseClosure(scope, reverse))
				continue
			}
		}
		owner, exists := owners[changedPath]
		if !exists {
			nearby := packageOwnersInParent(owners, changedPath)
			if len(nearby) == 0 {
				continue
			}
			addAll(affected, nearby)
			addAll(affected, reverseClosure(nearby, reverse))
			continue
		}
		affected[owner] = true
		if changedSurface, known := surface[changedPath]; !known || changedSurface {
			addAll(affected, reverseClosure([]string{owner}, reverse))
		}
	}
	return sortedSet(affected)
}

func AffectedScript(changed []string, metadata ChangeMetadata) []string {
	reverse := normalizedLists(metadata.ScriptReverseImport)
	surface := normalizedBools(metadata.SurfaceChanged)
	scopes := normalizedLists(metadata.ResolutionScope)
	affected := map[string]bool{}
	for _, changedPath := range normalizedPaths(changed) {
		if scope, exists := scopes[changedPath]; exists {
			addAll(affected, scope)
			continue
		}
		affected[changedPath] = true
		if changedSurface, known := surface[changedPath]; !known || changedSurface {
			addAll(affected, reverseClosure([]string{changedPath}, reverse))
		}
	}
	return sortedSet(affected)
}

func reverseClosure(seeds []string, reverse map[string][]string) []string {
	seen := map[string]bool{}
	queue := uniqueSorted(seeds)
	for _, seed := range queue {
		seen[seed] = true
	}
	for head := 0; head < len(queue); head++ {
		for _, dependent := range reverse[queue[head]] {
			if !seen[dependent] {
				seen[dependent] = true
				queue = append(queue, dependent)
			}
		}
	}
	return sortedSet(seen)
}

func packageOwnersInParent(owners map[string]string, changedPath string) []string {
	parent := path.Dir(changedPath)
	result := []string{}
	for file, owner := range owners {
		if path.Dir(file) == parent {
			result = append(result, owner)
		}
	}
	return uniqueSorted(result)
}

func knownGoPackages(owners map[string]string, reverse map[string][]string) map[string]bool {
	known := map[string]bool{}
	for _, owner := range owners {
		known[owner] = true
	}
	for source, dependents := range reverse {
		known[source] = true
		addAll(known, dependents)
	}
	return known
}

func knownScopePackages(scope []string, known map[string]bool) []string {
	result := []string{}
	for _, value := range scope {
		if known[value] {
			result = append(result, value)
		}
	}
	return uniqueSorted(result)
}

func normalizedStrings(values map[string]string) map[string]string {
	result := map[string]string{}
	for key, value := range values {
		key, value = normalizePath(key), strings.TrimSpace(value)
		if key != "" && value != "" {
			if current, exists := result[key]; !exists || value < current {
				result[key] = value
			}
		}
	}
	return result
}

func normalizedBools(values map[string]bool) map[string]bool {
	result := map[string]bool{}
	for key, value := range values {
		key = normalizePath(key)
		if key != "" {
			result[key] = result[key] || value
		}
	}
	return result
}

func normalizedLists(values map[string][]string) map[string][]string {
	result := map[string][]string{}
	for key, list := range values {
		key = normalizePath(key)
		if key == "" {
			continue
		}
		for _, value := range list {
			value = normalizePath(value)
			if value != "" {
				result[key] = append(result[key], value)
			}
		}
	}
	for key, list := range result {
		result[key] = uniqueSorted(list)
	}
	return result
}

func normalizedPaths(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = normalizePath(value); value != "" {
			result = append(result, value)
		}
	}
	return uniqueSorted(result)
}

func normalizePath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return ""
	}
	value = path.Clean(value)
	if value == "." || value == ".." || strings.HasPrefix(value, "../") || strings.HasPrefix(value, "/") {
		return ""
	}
	return value
}

func addAll(set map[string]bool, values []string) {
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
}

func sortedSet(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
