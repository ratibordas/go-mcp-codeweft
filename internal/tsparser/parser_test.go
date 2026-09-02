package tsparser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

func TestLanguageForAllSupportedExtensions(t *testing.T) {
	for _, extension := range []string{".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs", ".d.ts"} {
		if languageFor(extension) == nil {
			t.Errorf("languageFor(%q) = nil", extension)
		}
	}
	if languageFor(".json") != nil {
		t.Fatal("JSON unexpectedly has a source grammar")
	}
}

func TestParseBuildsDeterministicScriptGraph(t *testing.T) {
	result, err := New().Parse(context.Background(), Request{Root: fixtureRoot(t), Generation: 9})
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"src/api.ts", "src/service.tsx", "src/model.js", "src/legacy.cjs", "src/esm.mjs", "src/types.d.ts", "src/view.jsx"} {
		if findFile(result.Files, path) == nil {
			t.Errorf("missing parsed file %q", path)
		}
	}
	for name, kind := range map[string]string{
		"API":            "interface",
		"ID":             "type",
		"Controller":     "class",
		"Controller.run": "method",
		"handle":         "function",
		"helper":         "function",
		"Widget":         "component",
		"Card":           "component",
		"Panel":          "component",
		"Model":          "class",
		"Model.create":   "method",
		"BaseAPI":        "interface",
		"BaseAPI.ready":  "method",
		"View":           "component",
	} {
		assertUnit(t, result.Files, name, kind)
	}

	controller := findUnit(t, result.Files, "Controller")
	if controller.Source != "class Controller extends Service implements API {\n  run() {\n    helper();\n    this.local();\n    esm.esmFunction();\n    Model.create();\n  }\n\n  local() {}\n}" || controller.StartLine != 14 || controller.EndLine != 23 {
		t.Fatalf("controller lexical range/source = %d-%d %q", controller.StartLine, controller.EndLine, controller.Source)
	}
	if controller.Generation != 9 || controller.Language != "typescript" || controller.Extension != ".ts" || controller.Weight != 1 {
		t.Fatalf("controller metadata = %+v", controller)
	}
	if got := findUnit(t, result.Files, "BaseAPI").Weight; got != 0.5 {
		t.Fatalf("declaration-file weight = %v, want 0.5", got)
	}

	assertEdge(t, result.Files, "extends", "Controller", "Service", "local")
	assertEdge(t, result.Files, "implements", "Controller", "API", "local")
	assertEdge(t, result.Files, "extends", "Panel", "Component", "external")
	assertEdge(t, result.Files, "calls", "Controller.run", "helper", "local")
	assertEdge(t, result.Files, "calls", "Controller.run", "Controller.local", "local")
	assertEdge(t, result.Files, "calls", "Controller.run", "esmFunction", "local")
	assertEdge(t, result.Files, "calls", "Controller.run", "Model.create", "local")
	assertEdge(t, result.Files, "imports", "src/api.ts", "src/service.tsx", "local")
	assertEdge(t, result.Files, "imports", "src/api.ts", "react", "external")
	assertEdge(t, result.Files, "exports", "src/api.ts", "makeModel", "local")
	assertEdge(t, result.Files, "imports", "src/legacy.cjs", "src/model.js", "local")
	assertEdge(t, result.Files, "exports", "src/legacy.cjs", "legacy", "local")
	assertEdge(t, result.Files, "calls", "src/api.ts", "createElement", "external")
	assertUnresolvedCall(t, result.Files, "src/api.ts", "runtime.dispatch")
	if len(result.Warnings) == 0 || !containsWarning(result.Warnings, "dynamic import") {
		t.Fatalf("dynamic import warning missing: %q", result.Warnings)
	}

	if got := result.ModuleImports["src/api.ts"]; !reflect.DeepEqual(got, []string{"external:react", "src/esm.mjs", "src/model.js", "src/service.tsx", "src/types.d.ts"}) {
		t.Fatalf("api imports = %q", got)
	}
	if got := result.ReverseModuleImports["src/api.ts"]; !reflect.DeepEqual(got, []string{"src/service.tsx"}) {
		t.Fatalf("reverse cycle imports = %q", got)
	}
	assertSorted(t, result)
}

func TestParseKeepsImportAdjacencyWithoutDeclarationsAndAppliesTestWeight(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/empty.test.js", `import "./target";`)
	writeFile(t, root, "src/target.ts", `export function target() {}`)
	result, err := New().Parse(context.Background(), Request{Root: root, FileHashes: map[string]string{"src/empty.test.js": "test", "src/target.ts": "target"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.ModuleImports["src/empty.test.js"]; !reflect.DeepEqual(got, []string{"src/target.ts"}) {
		t.Fatalf("import-only adjacency = %q", got)
	}
	file := findFile(result.Files, "src/empty.test.js")
	if file == nil || len(file.Units) != 1 || file.Units[0].Weight != 0.6 {
		t.Fatalf("import-only file output = %+v", file)
	}
}

func TestParseStableIDsAndDeltaTargetsIgnoreLineMovement(t *testing.T) {
	root := copyFixture(t)
	full, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	delta, err := New().Parse(context.Background(), Request{Root: root, Paths: []string{"src/api.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	assertSameTarget(t, full.Files, delta.Files, "calls", "Controller.run", "helper")
	assertSameTarget(t, full.Files, delta.Files, "extends", "Controller", "Service")

	path := filepath.Join(root, "src", "api.ts")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("\n\n"), data...), 0o600); err != nil {
		t.Fatal(err)
	}
	moved, err := New().Parse(context.Background(), Request{Root: root, Paths: []string{"src/api.ts"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := unitIDs(moved.Files), unitIDs(delta.Files); !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs changed with line movement:\n got %q\nwant %q", got, want)
	}
}

func TestParseHashSelectedDeltaKeepsFullGraphTargetIdentity(t *testing.T) {
	root := fixtureRoot(t)
	full, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	delta, err := New().Parse(context.Background(), Request{
		Root: root, Paths: []string{"src/api.ts"}, FileHashes: map[string]string{
			"src/api.ts": strings.Repeat("a", 64), "src/service.tsx": strings.Repeat("b", 64), "src/model.js": strings.Repeat("c", 64), "src/esm.mjs": strings.Repeat("d", 64), "src/types.d.ts": strings.Repeat("e", 64),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSameTarget(t, full.Files, delta.Files, "calls", "Controller.run", "helper")
	assertSameTarget(t, full.Files, delta.Files, "extends", "Controller", "Service")
}

func TestParseUsesCanonicalSafePathsAndHashAllowlist(t *testing.T) {
	root := copyFixture(t)
	hashes := map[string]string{"src/api.ts": strings.Repeat("a", 64), "src/service.tsx": strings.Repeat("b", 64)}
	result, err := New().Parse(context.Background(), Request{Root: root, Paths: []string{"src/../src/api.ts"}, Generation: 4, FileHashes: hashes})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].File.Path != "src/api.ts" || result.Files[0].File.Hash != hashes["src/api.ts"] {
		t.Fatalf("allowlisted result = %+v", result.Files)
	}
	for _, unit := range result.Files[0].Units {
		if unit.FileHash != hashes["src/api.ts"] || unit.Generation != 4 {
			t.Fatalf("unit did not inherit active state: %+v", unit)
		}
	}
	if _, err := New().Parse(context.Background(), Request{Root: "relative"}); err == nil {
		t.Fatal("relative root accepted")
	}
	if _, err := New().Parse(context.Background(), Request{Root: root, Paths: []string{"../outside.ts"}}); err == nil {
		t.Fatal("escaping path accepted")
	}
	if _, err := New().Parse(context.Background(), Request{Root: root, FileHashes: map[string]string{"src/api.ts": "a", "src/../src/api.ts": "b"}}); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("conflicting normalized hashes error = %v", err)
	}
}

func TestParseSkipsSymlinksIntoNodeModules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/main.ts", "export function local() {}")
	writeFile(t, root, "node_modules/pkg/index.ts", "export function dependency() {}")
	if err := os.Symlink(filepath.Join(root, "node_modules", "pkg", "index.ts"), filepath.Join(root, "src", "dependency.ts")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if findFile(result.Files, "src/dependency.ts") != nil {
		t.Fatal("node_modules symlink entered parser output")
	}
}

func TestParseUnwrapsAmbientDeclarations(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "types.d.ts", "export declare function ready(): boolean;\nexport declare class Service {\n  run(): void;\n}\n")
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	assertUnit(t, result.Files, "ready", "function")
	assertUnit(t, result.Files, "Service", "class")
	assertUnit(t, result.Files, "Service.run", "method")
}

func TestParseKeepsDistinctMethodOverloadsAndStaticMethods(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.ts", "export class Service {\n  run(value: string): void;\n  run(value: number): void;\n  run(value: string | number) {}\n  static run(): void {}\n}\n")
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	count := 0
	for _, file := range result.Files {
		for _, unit := range file.Units {
			if unit.QualifiedName == "Service.run" {
				seen[unit.ID] = true
				count++
			}
		}
	}
	if count != 4 || len(seen) != 4 {
		t.Fatalf("Service.run methods = %d unique=%d, want four distinct methods", count, len(seen))
	}
}

func TestParseIndexesOnlyTopLevelVariableDeclarators(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.ts", "export const top = () => true;\nexport function outer() {\n  const nested = () => false;\n  return nested();\n}\n")
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	top := findUnit(t, result.Files, "top")
	if top.Source != "top = () => true" || top.StartLine != 1 || top.EndLine != 1 {
		t.Fatalf("top declarator = %+v", top)
	}
	for _, file := range result.Files {
		for _, unit := range file.Units {
			if unit.Name == "nested" {
				t.Fatal("nested variable declarator entered catalog")
			}
		}
	}
}

func TestParseTreatsShadowedImportsAndCatalogNamesAsUnresolved(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "library.ts", "export function helper() {}\n")
	writeFile(t, root, "main.ts", "import { helper } from './library';\nexport function parameter(helper: () => void) { helper(); }\nexport function local() { const helper = 0; helper(); }\n")
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"parameter", "local"} {
		unit := findUnit(t, result.Files, source)
		found := false
		for _, file := range result.Files {
			for _, edge := range file.Edges {
				if edge.SourceID == unit.ID && edge.Relation == "calls" && edge.Resolution == "unresolved" {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("shadowed %s call was resolved", source)
		}
	}
}

func TestParseResolvesAliasedAndStarReexportsTransitively(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "leaf.ts", "export function leaf() {}\n")
	writeFile(t, root, "middle.ts", "export { leaf as middle } from './leaf';\n")
	writeFile(t, root, "barrel.ts", "export * from './middle';\n")
	writeFile(t, root, "main.ts", "import { middle } from './barrel';\nexport function run() { middle(); }\n")
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	assertEdge(t, result.Files, "calls", "run", "leaf", "local")
}

func TestParseUsesDefaultPolicyAndHashCatalogWithoutWalkingUnrelatedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/main.ts", "import { dependency } from './dependency'; export function main() { dependency(); }\n")
	writeFile(t, root, "src/dependency.ts", "export function dependency() {}\n")
	writeFile(t, root, "src/unrelated.ts", "function broken( {\n")
	writeFile(t, root, ".idea/ignored.ts", "export const ignored = true")
	writeFile(t, root, ".vscode/ignored.ts", "export const ignored = true")
	writeFile(t, root, "src/bundle.min.js", "export const ignored = true")
	writeFile(t, root, "src/huge.ts", strings.Repeat("x", 2<<20+1))
	full, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if findFile(full.Files, ".idea/ignored.ts") != nil || findFile(full.Files, ".vscode/ignored.ts") != nil || findFile(full.Files, "src/bundle.min.js") != nil || findFile(full.Files, "src/huge.ts") != nil {
		t.Fatalf("default policy included excluded files: %+v", full.Files)
	}
	hashes := map[string]string{"src/main.ts": strings.Repeat("a", 64), "src/dependency.ts": strings.Repeat("b", 64)}
	delta, err := New().Parse(context.Background(), Request{Root: root, Paths: []string{"src/main.ts"}, FileHashes: hashes})
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Files) != 1 || delta.Files[0].File.Path != "src/main.ts" || containsWarning(delta.Warnings, "unrelated.ts") {
		t.Fatalf("delta read or emitted unrelated files: %+v", delta)
	}
	main := findUnit(t, delta.Files, "main")
	if edge := findEdgeBySourceAndRelation(delta.Files, main.ID, "calls"); edge == nil || edge.TargetID != findUnit(t, full.Files, "dependency").ID || edge.Resolution != "local" {
		t.Fatalf("delta did not catalog imported dependency: %+v", edge)
	}
}

func TestParseDoesNotResolveImportsOutsideActiveHashCatalog(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.ts", "import './excluded';")
	writeFile(t, root, "excluded.ts", "export const hidden = true;")
	result, err := New().Parse(context.Background(), Request{Root: root, FileHashes: map[string]string{"main.ts": strings.Repeat("a", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range findFile(result.Files, "main.ts").Edges {
		if edge.Relation == "imports" && edge.Resolution != "unresolved" {
			t.Fatalf("excluded import resolution = %+v", edge)
		}
	}
}

func TestParsePreseedsReverseImportsForSelectedFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.ts", "export const value = true;")
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := result.ReverseModuleImports["main.ts"]; !ok || len(got) != 0 {
		t.Fatalf("reverse imports = %q, %v; want empty initialized entry", got, ok)
	}
}

func TestParseKeepsDistinctTopLevelOverloadsAndUnresolvedSameLineCalls(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.ts", "export function run(value: string): void;\nexport function run(value: number): void;\nexport function run(value: string | number) {}\nexport function calls() { missing(); missing(); }\n")
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, file := range result.Files {
		for _, unit := range file.Units {
			if unit.QualifiedName == "run" {
				ids[unit.ID] = true
			}
		}
	}
	if len(ids) != 3 {
		t.Fatalf("top-level overload IDs = %v", ids)
	}
	calls := findUnit(t, result.Files, "calls")
	unresolved := 0
	for _, file := range result.Files {
		for _, edge := range file.Edges {
			if edge.SourceID == calls.ID && edge.Relation == "calls" && edge.Resolution == "unresolved" {
				unresolved++
			}
		}
	}
	if unresolved != 2 {
		t.Fatalf("same-line unresolved calls = %d", unresolved)
	}
}

func TestParseUsesOnlyVisibleShadowBindings(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.ts", "export function helper() {}\nexport function run() { helper(); { const helper = 0; helper(); } helper(); }\nexport function caught() { try {} catch ({ helper: renamed, ...rest }) { renamed(); rest(); } }\n")
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	run := findUnit(t, result.Files, "run")
	local, unresolved := 0, 0
	for _, file := range result.Files {
		for _, edge := range file.Edges {
			if edge.SourceID == run.ID && edge.Relation == "calls" {
				if edge.Resolution == "local" {
					local++
				}
				if edge.Resolution == "unresolved" {
					unresolved++
				}
			}
		}
	}
	if local != 2 || unresolved != 1 {
		t.Fatalf("run calls local=%d unresolved=%d", local, unresolved)
	}
	caught := findUnit(t, result.Files, "caught")
	caughtUnresolved := 0
	for _, file := range result.Files {
		for _, edge := range file.Edges {
			if edge.SourceID == caught.ID && edge.Relation == "calls" && edge.Resolution == "unresolved" {
				caughtUnresolved++
			}
		}
	}
	if caughtUnresolved != 2 {
		t.Fatalf("catch destructuring/rest calls unresolved = %d", caughtUnresolved)
	}
}

func TestParseTreatsAmbiguousStarReexportsAsUnresolved(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "one.ts", "export function shared() {}\nexport default function hidden() {}")
	writeFile(t, root, "two.ts", "export function shared() {}")
	writeFile(t, root, "barrel.ts", "export * from './one'; export * from './two';")
	writeFile(t, root, "main.ts", "import { shared, hidden } from './barrel'; export function run() { shared(); hidden(); }")
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	run := findUnit(t, result.Files, "run")
	count := 0
	for _, file := range result.Files {
		for _, edge := range file.Edges {
			if edge.SourceID == run.ID && edge.Relation == "calls" && edge.Resolution == "unresolved" {
				count++
			}
		}
	}
	if count != 2 {
		t.Fatalf("ambiguous/default star calls unresolved = %d", count)
	}
}

func TestParseTreatsEmptyHashCatalogAndPathOnlyDeltaAsClosedCatalogs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.ts", "export function main() {}")
	writeFile(t, root, "other.ts", "function broken( {")
	empty, err := New().Parse(context.Background(), Request{Root: root, FileHashes: map[string]string{}})
	if err != nil || len(empty.Files) != 0 {
		t.Fatalf("empty catalog = %+v, %v", empty, err)
	}
	delta, err := New().Parse(context.Background(), Request{Root: root, Paths: []string{"main.ts"}})
	if err != nil || len(delta.Files) != 1 || containsWarning(delta.Warnings, "other.ts") {
		t.Fatalf("path-only delta = %+v, %v", delta, err)
	}
}

func findEdgeBySourceAndRelation(files []core.IndexedFile, source, relation string) *core.CodeEdge {
	for i := range files {
		for j := range files[i].Edges {
			edge := &files[i].Edges[j]
			if edge.SourceID == source && edge.Relation == relation {
				return edge
			}
		}
	}
	return nil
}

func TestParseWarnsButKeepsUsableSyntax(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "broken.ts", "export function usable() {}\nfunction broken( {\n")
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	assertUnit(t, result.Files, "usable", "function")
	if len(result.Warnings) == 0 || !containsWarning(result.Warnings, "parse error") {
		t.Fatalf("parse warning missing: %q", result.Warnings)
	}
	if file := findFile(result.Files, "broken.ts"); file == nil || !containsWarning(file.Warnings, "parse error") {
		t.Fatalf("file parse warning missing: %+v", file)
	}
}

func TestParseReturnsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New().Parse(ctx, Request{Root: fixtureRoot(t)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled parse error = %v", err)
	}
}

func TestParseDoesNotInventTargetsForUnexportedOrMissingImports(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.ts", "function hidden() {}\nexport function visible() {}\n")
	writeFile(t, root, "b.ts", "import { hidden, missing, visible } from './a';\nexport function run() {\n  hidden();\n  missing();\n  visible();\n}\n")
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	assertEdge(t, result.Files, "calls", "run", "visible", "local")
	run := findUnit(t, result.Files, "run")
	unresolved := 0
	for _, file := range result.Files {
		for _, edge := range file.Edges {
			if edge.SourceID == run.ID && edge.Relation == "calls" && edge.Resolution == "unresolved" && edge.TargetID == "" {
				unresolved++
			}
		}
	}
	if unresolved != 2 {
		t.Fatalf("unresolved imported calls = %d, want 2", unresolved)
	}
}

func TestParseKeepsDefaultArrowAndWarnsOnUnresolvedModules(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.ts", "import './missing';\nconst name = './other';\nrequire(name);\nexport default () => true;\n")
	result, err := New().Parse(context.Background(), Request{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	assertUnit(t, result.Files, "default", "function")
	assertEdge(t, result.Files, "exports", "main.ts", "default", "local")
	if !containsWarning(result.Warnings, "unresolved import ./missing") || !containsWarning(result.Warnings, "unresolved dynamic require") {
		t.Fatalf("unresolved module warnings = %q", result.Warnings)
	}
	unresolved := 0
	for _, edge := range findFile(result.Files, "main.ts").Edges {
		if edge.Relation == "imports" && edge.Resolution == "unresolved" && edge.TargetID == "" {
			unresolved++
		}
	}
	if unresolved != 2 {
		t.Fatalf("unresolved module edges = %d, want 2", unresolved)
	}
}

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "testdata", "script-project"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func copyFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	err := filepath.WalkDir(fixtureRoot(t), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(fixtureRoot(t), path)
		if err != nil || rel == "." {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(filepath.Join(root, rel), 0o700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, rel), data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func writeFile(t *testing.T, root, path, contents string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func findFile(files []core.IndexedFile, path string) *core.IndexedFile {
	for i := range files {
		if files[i].File.Path == path {
			return &files[i]
		}
	}
	return nil
}

func findUnit(t *testing.T, files []core.IndexedFile, qualified string) core.CodeUnit {
	t.Helper()
	for _, file := range files {
		for _, unit := range file.Units {
			if unit.QualifiedName == qualified {
				return unit
			}
		}
	}
	t.Fatalf("unit %q not found; got %q", qualified, unitIDs(files))
	return core.CodeUnit{}
}

func assertUnit(t *testing.T, files []core.IndexedFile, qualified, kind string) {
	t.Helper()
	if got := findUnit(t, files, qualified); got.Kind != kind {
		t.Fatalf("unit %q kind = %q, want %q", qualified, got.Kind, kind)
	}
}

func assertEdge(t *testing.T, files []core.IndexedFile, relation, sourceName, targetName, resolution string) {
	t.Helper()
	source := findUnit(t, files, sourceName)
	for _, file := range files {
		for _, edge := range file.Edges {
			if edge.Relation == relation && edge.SourceID == source.ID && edge.Resolution == resolution {
				target := unitByID(files, edge.TargetID)
				if target != nil && target.QualifiedName == targetName || target == nil && targetName == externalName(edge.TargetID) {
					return
				}
			}
		}
	}
	t.Fatalf("missing %s edge %q -> %q (%s)", relation, sourceName, targetName, resolution)
}

func assertUnresolvedCall(t *testing.T, files []core.IndexedFile, path, lexicalTarget string) {
	t.Helper()
	for _, file := range files {
		for _, edge := range file.Edges {
			if edge.Path == path && edge.Relation == "calls" && edge.Resolution == "unresolved" && edge.TargetID == "" {
				for _, warning := range file.Warnings {
					if strings.Contains(warning, lexicalTarget) {
						return
					}
				}
			}
		}
	}
	t.Fatalf("missing unresolved call %q in %q", lexicalTarget, path)
}

func assertSameTarget(t *testing.T, full, delta []core.IndexedFile, relation, sourceName, targetName string) {
	t.Helper()
	want := findUnit(t, full, targetName).ID
	source := findUnit(t, delta, sourceName).ID
	for _, file := range delta {
		for _, edge := range file.Edges {
			if edge.Relation == relation && edge.SourceID == source && edge.TargetID == want {
				return
			}
		}
	}
	t.Fatalf("delta %s target for %q does not match full unit %q", relation, sourceName, targetName)
}

func unitByID(files []core.IndexedFile, id string) *core.CodeUnit {
	for i := range files {
		for j := range files[i].Units {
			if files[i].Units[j].ID == id {
				return &files[i].Units[j]
			}
		}
	}
	return nil
}

func unitIDs(files []core.IndexedFile) []string {
	var ids []string
	for _, file := range files {
		for _, unit := range file.Units {
			ids = append(ids, unit.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func containsWarning(warnings []string, text string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, text) {
			return true
		}
	}
	return false
}

func assertSorted(t *testing.T, result Result) {
	t.Helper()
	paths := make([]string, len(result.Files))
	for i, file := range result.Files {
		paths[i] = file.File.Path
		if !sort.SliceIsSorted(file.Units, func(i, j int) bool { return file.Units[i].ID < file.Units[j].ID }) {
			t.Fatalf("units are not sorted in %q", file.File.Path)
		}
		if !sort.SliceIsSorted(file.Edges, func(i, j int) bool { return edgeKey(file.Edges[i]) < edgeKey(file.Edges[j]) }) {
			t.Fatalf("edges are not sorted in %q", file.File.Path)
		}
	}
	if !sort.StringsAreSorted(paths) || !sort.StringsAreSorted(result.Warnings) {
		t.Fatalf("result is not sorted: paths=%q warnings=%q", paths, result.Warnings)
	}
}
