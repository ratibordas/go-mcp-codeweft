package tsparser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseScopesFunctionsClassesVarsAndPatternDefaults(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "library.ts", "export function helper() {}\nexport function blockFunction() {}\nexport function blockClass() {}\nexport function local() {}\nexport function variable() {}\n")
	writeFile(t, root, "main.ts", `import { helper, blockFunction, blockClass, local, variable } from "./library";
export function run({ value = helper } = {}) {
  helper();
  { function blockFunction() {} blockFunction(); }
  blockFunction();
  { class blockClass {} blockClass(); }
  blockClass();
}
export function functionHoist() { local(); function local() {} }
export function varHoist() { variable(); { var variable = 1; } }
`)
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	assertCallResolutions(t, result, "run", 3, 2)
	assertCallResolutions(t, result, "functionHoist", 0, 1)
	assertCallResolutions(t, result, "varHoist", 0, 1)
}

func TestParseUsesOverloadImplementationAsBaseWithObjectReturnSignatures(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.ts", `export function render(value: string): { body: string };
export function render(value: number): { body: number };
export function render(value: string | number): { body: string | number } { return { body: value }; }
`)
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	base := stableID("main.ts", "function", "", "render")
	for _, file := range result.Files {
		for _, unit := range file.Units {
			if unit.ID == base && strings.Contains(unit.Source, "return { body: value }") {
				return
			}
		}
	}
	t.Fatalf("overload base %q did not select the implementation: %+v", base, result.Files)
}

func TestParsePropagatesNestedStarAmbiguityButPrefersExplicitExports(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "one.ts", "export function shared() {}\nexport default function hidden() {}\n")
	writeFile(t, root, "two.ts", "export function shared() {}\n")
	writeFile(t, root, "fallback.ts", "export function shared() {}\n")
	writeFile(t, root, "explicit.ts", "export function explicit() {}\n")
	writeFile(t, root, "inner.ts", "export * from './one'; export * from './two';\n")
	writeFile(t, root, "ambiguous.ts", "export * from './inner'; export * from './fallback';\n")
	writeFile(t, root, "explicit-barrel.ts", "export * from './inner'; export { explicit as shared } from './explicit';\n")
	writeFile(t, root, "main.ts", `import { shared as ambiguous, hidden } from "./ambiguous";
import { shared as chosen } from "./explicit-barrel";
export function run() { ambiguous(); hidden(); chosen(); }
`)
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	run := findUnit(t, result.Files, "run")
	explicit := findUnit(t, result.Files, "explicit")
	local, unresolved := 0, 0
	for _, file := range result.Files {
		for _, edge := range file.Edges {
			if edge.SourceID != run.ID || edge.Relation != "calls" {
				continue
			}
			if edge.TargetID == explicit.ID && edge.Resolution == "local" {
				local++
			}
			if edge.Resolution == "unresolved" {
				unresolved++
			}
		}
	}
	if local != 1 || unresolved != 2 {
		t.Fatalf("nested star calls local=%d unresolved=%d", local, unresolved)
	}
}

func TestResolverWarnsAndFallsBackForExtensionlessPackageTargetSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, root, "src/main.ts", "")
	writeFile(t, root, "okpkg/package.json", `{"main":"./entry"}`)
	writeFile(t, root, "okpkg/entry.ts", "")
	writeFile(t, root, "pkg/package.json", `{"main":"./entry"}`)
	writeFile(t, root, "pkg/index.ts", "")
	writeFile(t, outside, "entry.ts", "")
	if err := os.Symlink(filepath.Join(outside, "entry.ts"), filepath.Join(root, "pkg", "entry.ts")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r, err := newResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Resolve("src/main.ts", "../okpkg"); !ok || got != "okpkg/entry.ts" {
		t.Fatalf("extensionless package target = %q, %v", got, ok)
	}
	if got, ok := r.Resolve("src/main.ts", "../pkg"); !ok || got != "pkg/index.ts" {
		t.Fatalf("escaping extensionless target = %q, %v", got, ok)
	}
	if !containsWarning(r.Warnings(), "unsafe package target ./entry") {
		t.Fatalf("missing target escape warning: %q", r.Warnings())
	}
}

func TestValidateHashesReportsStableConflictPairWithoutSorting(t *testing.T) {
	values := map[string]string{"main.ts": "a", "./main.ts": "b", "dir/../main.ts": "c"}
	var want string
	for range 64 {
		_, err := validateHashes(context.Background(), t.TempDir(), values)
		if err == nil {
			t.Fatal("conflicting normalized hashes were accepted")
		}
		if want == "" {
			want = err.Error()
		} else if err.Error() != want {
			t.Fatalf("conflict error changed with map order:\nfirst: %s\nnow:   %s", want, err)
		}
	}
	if !strings.Contains(want, `("./main.ts", "dir/../main.ts")`) {
		t.Fatalf("conflict pair = %q", want)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := validateHashes(ctx, t.TempDir(), values); err != context.Canceled {
		t.Fatalf("canceled validation = %v", err)
	}
}

func assertCallResolutions(t *testing.T, result Result, source string, local, unresolved int) {
	t.Helper()
	unit := findUnit(t, result.Files, source)
	gotLocal, gotUnresolved := 0, 0
	for _, file := range result.Files {
		for _, edge := range file.Edges {
			if edge.SourceID != unit.ID || edge.Relation != "calls" {
				continue
			}
			switch edge.Resolution {
			case "local":
				gotLocal++
			case "unresolved":
				gotUnresolved++
			}
		}
	}
	if gotLocal != local || gotUnresolved != unresolved {
		t.Fatalf("%s call resolutions local=%d unresolved=%d; want %d/%d", source, gotLocal, gotUnresolved, local, unresolved)
	}
}
