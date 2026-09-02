package tsparser

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseProgramAndGeneratorHoistingUsesExactCallScopes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "library.ts", `export function programVar() {}
export function programFunction() {}
export function nestedCallableVar() {}
export function bodyGenerator() {}
export function blockGenerator() {}
export function nestedVar() {}
`)
	writeFile(t, root, "main.ts", `import { programVar, programFunction, nestedCallableVar, bodyGenerator, blockGenerator, nestedVar } from "./library";
programVar();
programFunction();
nestedCallableVar();
var programVar = 1;
function programFunction() {}
function nestedCallable() { var nestedCallableVar = 1; }
export function programScopeInside() {
  programVar();
  programFunction();
}
export function directGenerator() { bodyGenerator(); function* bodyGenerator() {} }
export function blockGeneratorScope() {
  { function* blockGenerator() {} blockGenerator(); }
  blockGenerator();
}
export function nestedVarScope() { nestedVar(); { var nestedVar = 1; } }
`)
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	mainModule := moduleID("main.ts")
	for _, call := range []struct {
		name                 string
		line                 uint32
		source, target, kind string
	}{
		{"program var", 2, mainModule, "", "unresolved"},
		{"program function", 3, mainModule, "", "unresolved"},
		{"nested callable var excluded", 4, mainModule, findUnit(t, result.Files, "nestedCallableVar").ID, "local"},
		{"program var inside function", 9, findUnit(t, result.Files, "programScopeInside").ID, "", "unresolved"},
		{"program function inside function", 10, findUnit(t, result.Files, "programScopeInside").ID, "", "unresolved"},
		{"direct body generator", 12, findUnit(t, result.Files, "directGenerator").ID, "", "unresolved"},
		{"block generator", 14, findUnit(t, result.Files, "blockGeneratorScope").ID, "", "unresolved"},
		{"block generator sibling", 15, findUnit(t, result.Files, "blockGeneratorScope").ID, findUnit(t, result.Files, "blockGenerator").ID, "local"},
		{"nested block var", 17, findUnit(t, result.Files, "nestedVarScope").ID, "", "unresolved"},
	} {
		t.Run(call.name, func(t *testing.T) {
			assertCallAtLine(t, result, call.line, call.source, call.target, call.kind)
		})
	}
}

func TestResolverContainsEveryPackageTargetProbe(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, root, "src/main.ts", "")
	writeFile(t, root, "okpkg/package.json", `{"main":"./entry"}`)
	writeFile(t, root, "okpkg/entry.ts", "")
	writeFile(t, root, "insidepkg/package.json", `{"main":"./entry"}`)
	writeFile(t, root, "insidepkg/index.ts", "")
	writeFile(t, root, "elsewhere/entry.ts", "")
	writeFile(t, root, "escapepkg/package.json", `{"main":"./entry"}`)
	writeFile(t, root, "escapepkg/index.ts", "")
	writeFile(t, outside, "entry.ts", "")
	if err := os.Symlink(filepath.Join(root, "elsewhere", "entry.ts"), filepath.Join(root, "insidepkg", "entry.ts")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "entry.ts"), filepath.Join(root, "escapepkg", "entry.ts")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r, err := newResolver(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, resolution := range []struct{ specifier, want string }{
		{"../okpkg", "okpkg/entry.ts"},
		{"../insidepkg", "insidepkg/index.ts"},
		{"../escapepkg", "escapepkg/index.ts"},
	} {
		if got, ok := r.Resolve("src/main.ts", resolution.specifier); !ok || got != resolution.want {
			t.Fatalf("Resolve(%q) = %q, %v; want %q, true", resolution.specifier, got, ok, resolution.want)
		}
	}
	for _, warning := range []string{
		"insidepkg/package.json: unsafe package target ./entry",
		"escapepkg/package.json: unsafe package target ./entry",
	} {
		if !containsWarning(r.Warnings(), warning) {
			t.Fatalf("missing warning %q in %q", warning, r.Warnings())
		}
	}
}

func assertCallAtLine(t *testing.T, result Result, line uint32, source, target, resolution string) {
	t.Helper()
	count := 0
	for _, file := range result.Files {
		for _, edge := range file.Edges {
			if edge.Path != "main.ts" || edge.Relation != "calls" || edge.StartLine != line {
				continue
			}
			count++
			if edge.SourceID != source || edge.TargetID != target || edge.Resolution != resolution {
				t.Fatalf("call at line %d = source=%q target=%q resolution=%q; want source=%q target=%q resolution=%q", line, edge.SourceID, edge.TargetID, edge.Resolution, source, target, resolution)
			}
		}
	}
	if count != 1 {
		t.Fatalf("calls at line %d = %d; want 1", line, count)
	}
}
