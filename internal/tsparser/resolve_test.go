package tsparser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveAliasExtensionsIndexAndPackageTarget(t *testing.T) {
	r, err := newResolver(fixtureRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for specifier, want := range map[string]string{
		"@app/service":       "src/service.tsx",
		"@feature":           "src/feature/index.ts",
		"..":                 "src/esm.mjs",
		"./model":            "src/model.js",
		"./types.d.ts":       "src/types.d.ts",
		"./package-dir":      "src/package-dir/entry.js",
		"./feature/index.ts": "src/feature/index.ts",
	} {
		got, ok := r.Resolve("src/api.ts", specifier)
		if !ok || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q, true", specifier, got, ok, want)
		}
	}
	if got, ok := r.Resolve("src/api.ts", "react"); ok || got != "" {
		t.Fatalf("bare package resolved locally: %q, %v", got, ok)
	}
}

func TestResolverRejectsRootAndSymlinkEscapesAndNodeModules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tsconfig.json", `{"compilerOptions":{"baseUrl":".","paths":{"@bad/*":["../outside/*"],"@deps/*":["node_modules/*"]}}}`)
	writeFile(t, root, "src/main.ts", "")
	writeFile(t, root, "node_modules/pkg/index.ts", "")
	outside := t.TempDir()
	writeFile(t, outside, "outside.ts", "")
	if err := os.Symlink(filepath.Join(outside, "outside.ts"), filepath.Join(root, "src", "escape.ts")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r, err := newResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, specifier := range []string{"../../outside", "./escape", "@bad/outside", "@deps/pkg"} {
		if got, ok := r.Resolve("src/main.ts", specifier); ok {
			t.Errorf("unsafe Resolve(%q) = %q, true", specifier, got)
		}
	}
}

func TestResolverRejectsSymlinkIntoNodeModules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/main.ts", "")
	writeFile(t, root, "node_modules/pkg/index.ts", "export const dependency = true;")
	if err := os.Symlink(filepath.Join(root, "node_modules", "pkg", "index.ts"), filepath.Join(root, "src", "dependency.ts")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r, err := newResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Resolve("src/main.ts", "./dependency"); ok {
		t.Fatalf("node_modules symlink resolved: %q", got)
	}
}

func TestResolverPackageTargetCycleTerminates(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/main.ts", "")
	writeFile(t, root, "src/cycle/package.json", `{"exports":"."}`)
	r, err := newResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Resolve("src/main.ts", "./cycle"); ok {
		t.Fatalf("cyclic package target resolved: %q", got)
	}
}

func TestResolverUsesBaseURLWithoutPathAlias(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tsconfig.json", `{"compilerOptions":{"baseUrl":"src"}}`)
	writeFile(t, root, "src/main.ts", "")
	writeFile(t, root, "src/lib/tool.ts", "")
	r, err := newResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Resolve("src/main.ts", "lib/tool"); !ok || got != "src/lib/tool.ts" {
		t.Fatalf("baseUrl resolution = %q, %v", got, ok)
	}
}

func TestResolverRejectsEscapingConfigSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "tsconfig.json", `{"compilerOptions":{"baseUrl":"."}}`)
	if err := os.Symlink(filepath.Join(outside, "tsconfig.json"), filepath.Join(root, "tsconfig.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := newResolver(root); err == nil {
		t.Fatal("escaping config symlink accepted")
	}
}

func TestResolverRejectsConfigSymlinkIntoNodeModules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "node_modules/config/tsconfig.json", `{"compilerOptions":{"baseUrl":"."}}`)
	if err := os.Symlink(filepath.Join(root, "node_modules", "config", "tsconfig.json"), filepath.Join(root, "tsconfig.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := newResolver(root); err == nil {
		t.Fatal("node_modules config symlink accepted")
	}
}

func TestResolverPrefersSpecificPathsAndDoesNotAssumeBaseURL(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tsconfig.json", `{"compilerOptions":{"paths":{"@/*":["general/*"],"@/special/*":["specific/*"]}}}`)
	writeFile(t, root, "src/main.ts", "")
	writeFile(t, root, "general/special/tool.ts", "")
	writeFile(t, root, "specific/tool.ts", "")
	writeFile(t, root, "plain.ts", "")
	r, err := newResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Resolve("src/main.ts", "@/special/tool"); !ok || got != "specific/tool.ts" {
		t.Fatalf("specific path resolution = %q, %v", got, ok)
	}
	if got, ok := r.Resolve("src/main.ts", "plain"); ok || got != "" {
		t.Fatalf("unconfigured baseUrl resolved bare path: %q, %v", got, ok)
	}
}

func TestResolverPrefersExactPathsAndAllowsRootTargets(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tsconfig.json", `{"compilerOptions":{"baseUrl":".","paths":{"@root":["."],"@/*":["wild/*"],"@/exact":["exact"]}}}`)
	writeFile(t, root, "package.json", `{"exports":"./root.ts"}`)
	writeFile(t, root, "root.ts", "")
	writeFile(t, root, "wild/exact.ts", "")
	writeFile(t, root, "exact.ts", "")
	writeFile(t, root, "src/main.ts", "")
	r, err := newResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Resolve("src/main.ts", "@/exact"); !ok || got != "exact.ts" {
		t.Fatalf("exact path precedence = %q, %v", got, ok)
	}
	if got, ok := r.Resolve("src/main.ts", "@root"); !ok || got != "root.ts" {
		t.Fatalf("root path target = %q, %v", got, ok)
	}
}

func TestResolverWarnsDeterministicallyForMalformedDirectoryPackageJSON(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/main.ts", "")
	writeFile(t, root, "pkg/package.json", "{")
	writeFile(t, root, "pkg/index.ts", "")
	r, err := newResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Resolve("src/main.ts", "../pkg"); !ok || got != "pkg/index.ts" {
		t.Fatalf("index fallback = %q, %v", got, ok)
	}
	warnings := r.Warnings()
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "pkg/package.json: ") {
		t.Fatalf("package warnings = %q", warnings)
	}
}

func TestResolverRejectsTrailingConfigJSONValue(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tsconfig.json", `{} {}`)
	if _, err := newResolver(root); err == nil {
		t.Fatal("multiple config JSON values accepted")
	}
}

func TestResolverDoesNotFallThroughMatchedExactAliasAndRejectsUnsafePackageTargets(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tsconfig.json", `{"compilerOptions":{"paths":{"@item":["missing"],"@*":["wild/*"]}}}`)
	writeFile(t, root, "src/main.ts", "")
	writeFile(t, root, "wild/item.ts", "")
	writeFile(t, root, "pkg/package.json", `{"main":"../escape.ts"}`)
	writeFile(t, root, "escape.ts", "")
	writeFile(t, root, "pkg/index.ts", "")
	r, err := newResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Resolve("src/main.ts", "@item"); ok || got != "" {
		t.Fatalf("matched alias fell through: %q", got)
	}
	if got, ok := r.Resolve("src/main.ts", "../pkg"); !ok || got != "pkg/index.ts" {
		t.Fatalf("unsafe target fallback = %q, %v", got, ok)
	}
	if !containsWarning(r.Warnings(), "unsafe package target") {
		t.Fatalf("unsafe target warning = %q", r.Warnings())
	}
}

func TestResolverRejectsOversizedMetadata(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tsconfig.json", strings.Repeat(" ", 1<<20+1))
	if _, err := newResolver(root); err == nil || !strings.Contains(err.Error(), "exceeds 1MiB") {
		t.Fatalf("oversized metadata error = %v", err)
	}
}
