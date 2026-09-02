package goparser

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

func TestParseBuildsModuleAwareGraph(t *testing.T) {
	root := fixtureRoot(t)
	result, err := New().Parse(context.Background(), Request{Root: root, Patterns: []string{"./..."}, Generation: 7})
	if err != nil {
		t.Fatal(err)
	}

	for name, kind := range map[string]string{
		"example.com/service/customer": "package",
		"service/customer/customer.go": "file",
		"customer.Service":             "type",
		"model.Creator":                "interface",
		"model.Customer":               "type",
		"customer.Clean":               "function",
		"customer.Service.Create":      "method",
	} {
		assertUnit(t, result.Files, name, kind)
	}
	assertEdge(t, result.Files, "contains", "example.com/service/customer", "service/customer/customer.go")
	assertEdge(t, result.Files, "contains", "service/customer/customer.go", "customer.Service")
	assertEdge(t, result.Files, "imports", "service/customer/customer.go", "example.com/shared/model")
	assertEdge(t, result.Files, "implements", "customer.Service", "model.Creator")
	assertEdge(t, result.Files, "implements", "customer.PointerService", "model.Creator")
	assertNoEdge(t, result.Files, "implements", "customer.Service", "model.RestrictedCreator")
	assertEdge(t, result.Files, "embeds", "customer.Service", "model.Metadata")
	assertEdge(t, result.Files, "calls", "customer.Service.Create", "model.NewCustomer")
	assertEdge(t, result.Files, "calls", "customer.CreateWith", "customer.Service.Create")

	method := findUnit(t, result.Files, "customer.Service.Create")
	wantSource := "func (Service) Create(name string) model.Customer {\n\treturn model.NewCustomer(name)\n}"
	if method.Source != wantSource || method.Generation != 7 || method.Weight != 1 || method.FileHash != "" {
		t.Fatalf("unexpected method unit: %+v", method)
	}
	if packageUnit := findUnit(t, result.Files, "example.com/shared/model"); packageUnit.StartLine != 3 || packageUnit.EndLine != 3 {
		t.Fatalf("package location = %d-%d, want 3-3", packageUnit.StartLine, packageUnit.EndLine)
	}
	testUnit := findUnit(t, result.Files, "customer.TestCreate")
	if testUnit.Weight != 0.6 {
		t.Fatalf("test weight = %v, want 0.6", testUnit.Weight)
	}
	if hasUnit(result.Files, "customer.GeneratedHelper") {
		t.Fatal("generated declaration entered parser output")
	}

	assertSortedResult(t, result)
	if got := result.FilePackages["service/customer/customer.go"]; got != "example.com/service/customer" {
		t.Fatalf("file package = %q", got)
	}
	if got := result.PackageImports["example.com/service/customer"]; !reflect.DeepEqual(got, []string{"example.com/shared/model", "strings", "testing"}) {
		t.Fatalf("package imports = %q", got)
	}
	if got := result.ReversePackageImports["example.com/shared/model"]; !reflect.DeepEqual(got, []string{"example.com/service/customer"}) {
		t.Fatalf("reverse imports = %q", got)
	}

	foundExternal := false
	for _, indexed := range result.Files {
		for _, edge := range indexed.Edges {
			if edge.Relation == "calls" && edge.SourceID == findUnit(t, result.Files, "customer.Clean").ID && edge.Resolution == "external" {
				foundExternal = true
				if unitByID(result.Files, edge.TargetID) != nil {
					t.Fatalf("external target %q has indexed source", edge.TargetID)
				}
			}
		}
	}
	if !foundExternal {
		t.Fatal("missing terminal external call target")
	}
}

func TestParseDeltaKeepsFullGraphTargetIdentities(t *testing.T) {
	root := fixtureRoot(t)
	full, err := New().Parse(context.Background(), Request{Root: root, Patterns: []string{"./..."}})
	if err != nil {
		t.Fatal(err)
	}
	delta, err := New().Parse(context.Background(), Request{Root: root, Patterns: []string{"example.com/service/customer"}})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		relation, source, target string
	}{
		{"imports", "service/customer/customer.go", "example.com/shared/model"},
		{"embeds", "customer.Service", "model.Metadata"},
		{"calls", "customer.Service.Create", "model.NewCustomer"},
		{"implements", "customer.Service", "model.Creator"},
	}
	for _, tc := range cases {
		want := findUnit(t, full.Files, tc.target).ID
		source := findUnit(t, delta.Files, tc.source).ID
		edge := findEdgeTarget(t, delta.Files, tc.relation, source, want)
		if edge.Resolution != "local" {
			t.Fatalf("delta %s target %q resolution = %s, want local", tc.relation, want, edge.Resolution)
		}
	}
}

func TestParseKeepsDistinctInitDeclarationsAndCalls(t *testing.T) {
	result, err := New().Parse(context.Background(), Request{Root: fixtureRoot(t), Patterns: []string{"./..."}})
	if err != nil {
		t.Fatal(err)
	}
	inits := map[string]bool{}
	for _, indexed := range result.Files {
		for _, unit := range indexed.Units {
			if unit.Name == "init" && unit.Path == "service/customer/customer.go" {
				inits[unit.ID] = true
			}
		}
	}
	if len(inits) != 2 {
		t.Fatalf("init declaration IDs = %v, want two", inits)
	}
	first := findCallTo(t, result.Files, findUnit(t, result.Files, "customer.initOne").ID)
	second := findCallTo(t, result.Files, findUnit(t, result.Files, "customer.initTwo").ID)
	if first.SourceID == second.SourceID || !inits[first.SourceID] || !inits[second.SourceID] {
		t.Fatalf("init call sources = %q and %q, want distinct init units", first.SourceID, second.SourceID)
	}
}

func TestParseDoesNotAttributeClosureTargetToOuterFunction(t *testing.T) {
	result, err := New().Parse(context.Background(), Request{Root: fixtureRoot(t), Patterns: []string{"./..."}})
	if err != nil {
		t.Fatal(err)
	}
	outer := findUnit(t, result.Files, "customer.InvokeClosure")
	for _, indexed := range result.Files {
		for _, edge := range indexed.Edges {
			if edge.Relation == "calls" && edge.SourceID == outer.ID {
				if edge.TargetID == outer.ID {
					t.Fatal("closure invocation produced an outer-function self-call")
				}
			}
		}
	}
}

func TestParseStableIDsIgnoreLineMovement(t *testing.T) {
	root := copyFixture(t)
	first, err := New().Parse(context.Background(), Request{Root: root, Patterns: []string{"./..."}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "service", "customer", "customer.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(source), "package customer\n", "package customer\n\n\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := New().Parse(context.Background(), Request{Root: root, Patterns: []string{"./..."}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := unitIDs(second.Files), unitIDs(first.Files); !reflect.DeepEqual(got, want) {
		t.Fatalf("IDs changed after line movement:\n got %q\nwant %q", got, want)
	}
}

func TestParseUsesActiveHashesAsSourcePolicy(t *testing.T) {
	root := fixtureRoot(t)
	hashes := map[string]string{
		"service/customer/customer.go": strings.Repeat("a", 64),
		"shared/model/model.go":        strings.Repeat("b", 64),
	}
	result, err := New().Parse(context.Background(), Request{Root: root, Patterns: []string{"./..."}, Generation: 3, FileHashes: hashes})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.FilePackages["service/customer/customer_test.go"]; ok {
		t.Fatal("file absent from active hashes entered output")
	}
	unit := findUnit(t, result.Files, "customer.Service")
	if unit.FileHash != hashes["service/customer/customer.go"] || unit.Generation != 3 {
		t.Fatalf("active state not copied to unit: %+v", unit)
	}
	assertNoEdge(t, result.Files, "implements", "customer.HiddenService", "model.Creator")
	assertNoEdge(t, result.Files, "implements", "customer.Service", "model.GeneratedCreator")
}

func TestParseSelectsRequestedPackagePatterns(t *testing.T) {
	result, err := New().Parse(context.Background(), Request{
		Root:     fixtureRoot(t),
		Patterns: []string{"example.com/service/customer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasUnit(result.Files, "model.Customer") {
		t.Fatal("dependency package was indexed outside the requested affected set")
	}
	if !hasUnit(result.Files, "customer.Service") {
		t.Fatal("requested package was not indexed")
	}
}

func TestParseLoadsModuleOutsideWorkspaceIndependently(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "go.work", "go 1.27.0\n\nuse ./first\n")
	writeTestFile(t, root, "first/go.mod", "module example.com/first\n\ngo 1.27.0\n")
	writeTestFile(t, root, "first/first.go", "package first\n\ntype First struct{}\n")
	writeTestFile(t, root, "second/go.mod", "module example.com/second\n\ngo 1.27.0\n")
	writeTestFile(t, root, "second/second.go", "package second\n\ntype Second struct{}\n")

	result, err := New().Parse(context.Background(), Request{Root: root, Patterns: []string{"./..."}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasUnit(result.Files, "first.First") || !hasUnit(result.Files, "second.Second") {
		t.Fatalf("independent modules not loaded: %q", unitIDs(result.Files))
	}
}

func TestParseKeepsUsableSyntaxAndWarnsOnPackageErrors(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "go.mod", "module example.com/broken\n\ngo 1.27.0\n")
	writeTestFile(t, root, "broken.go", "package broken\n\nfunc Usable() {}\nfunc Broken( {\n")
	result, err := New().Parse(context.Background(), Request{Root: root, Patterns: []string{"./..."}})
	if err != nil {
		t.Fatal(err)
	}
	if !hasUnit(result.Files, "broken.Usable") || len(result.Warnings) == 0 {
		t.Fatalf("partial result = %+v", result)
	}
	if !sort.StringsAreSorted(result.Warnings) {
		t.Fatalf("warnings are not sorted: %q", result.Warnings)
	}
}

func TestParseRejectsUnsafeInputsAndNoAnalyzablePackage(t *testing.T) {
	parser := New()
	if _, err := parser.Parse(context.Background(), Request{Root: "relative", Patterns: []string{"./..."}}); err == nil {
		t.Fatal("relative root was accepted")
	}
	root := copyFixture(t)
	if _, err := parser.Parse(context.Background(), Request{Root: root, Patterns: []string{"../outside"}}); err == nil {
		t.Fatal("escaping package pattern was accepted")
	}
	if _, err := parser.Parse(context.Background(), Request{Root: root, Patterns: []string{"./..."}, FileHashes: map[string]string{"../outside.go": "hash"}}); err == nil {
		t.Fatal("escaping active path was accepted")
	}
	outside := t.TempDir()
	writeTestFile(t, outside, "outside.go", "package outside\n")
	link := filepath.Join(root, "service", "customer", "escape.go")
	if err := os.Symlink(filepath.Join(outside, "outside.go"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := parser.Parse(context.Background(), Request{Root: root, Patterns: []string{"./..."}, FileHashes: map[string]string{"service/customer/escape.go": "hash"}}); err == nil {
		t.Fatal("escaping active symlink was accepted")
	}
	empty := t.TempDir()
	if _, err := parser.Parse(context.Background(), Request{Root: empty, Patterns: []string{"./..."}}); err == nil {
		t.Fatal("missing analyzable package did not fail")
	}
}

func TestParseRejectsConflictingNormalizedHashes(t *testing.T) {
	root := fixtureRoot(t)
	_, err := New().Parse(context.Background(), Request{
		Root: root, Patterns: []string{"./..."},
		FileHashes: map[string]string{
			"service/customer/customer.go":             strings.Repeat("a", 64),
			"service/customer/../customer/customer.go": strings.Repeat("b", 64),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("conflicting normalized hashes error = %v", err)
	}
}

func TestParseReturnsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New().Parse(ctx, Request{Root: fixtureRoot(t), Patterns: []string{"./..."}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled parse error = %v", err)
	}
}

func TestPackageEnvironmentPreservesCallerValues(t *testing.T) {
	environment := []string{"FIRST=1", "GOWORK=/workspace/go.work", "GOPROXY=https://proxy.example", "LAST=2"}
	wantNormal := []string{"FIRST=1", "GOWORK=/workspace/go.work", "GOPROXY=off", "LAST=2"}
	if got := packageEnvironment(environment, false); !reflect.DeepEqual(got, wantNormal) {
		t.Fatalf("normal environment = %q, want %q", got, wantNormal)
	}
	wantIndependent := []string{"FIRST=1", "GOWORK=off", "GOPROXY=off", "LAST=2"}
	if got := packageEnvironment(environment, true); !reflect.DeepEqual(got, wantIndependent) {
		t.Fatalf("independent environment = %q, want %q", got, wantIndependent)
	}
}

func fixtureRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "testdata", "go-multimodule"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func copyFixture(t *testing.T) string {
	t.Helper()
	source := fixtureRoot(t)
	destination := t.TempDir()
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil || rel == "." {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, contents, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return destination
}

func writeTestFile(t *testing.T, root, path, contents string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertUnit(t *testing.T, files []core.IndexedFile, name, kind string) {
	t.Helper()
	unit := findUnit(t, files, name)
	if unit.Kind != kind {
		t.Fatalf("%s kind = %q, want %q", name, unit.Kind, kind)
	}
}

func findUnit(t *testing.T, files []core.IndexedFile, name string) core.CodeUnit {
	t.Helper()
	for _, indexed := range files {
		for _, unit := range indexed.Units {
			if unit.QualifiedName == name {
				return unit
			}
		}
	}
	t.Fatalf("missing unit %q", name)
	return core.CodeUnit{}
}

func hasUnit(files []core.IndexedFile, name string) bool {
	for _, indexed := range files {
		for _, unit := range indexed.Units {
			if unit.QualifiedName == name {
				return true
			}
		}
	}
	return false
}

func unitByID(files []core.IndexedFile, id string) *core.CodeUnit {
	for _, indexed := range files {
		for i := range indexed.Units {
			if indexed.Units[i].ID == id {
				return &indexed.Units[i]
			}
		}
	}
	return nil
}

func assertEdge(t *testing.T, files []core.IndexedFile, relation, source, target string) {
	t.Helper()
	sourceID := findUnit(t, files, source).ID
	targetID := findUnit(t, files, target).ID
	for _, indexed := range files {
		for _, edge := range indexed.Edges {
			if edge.Relation == relation && edge.SourceID == sourceID && edge.TargetID == targetID {
				return
			}
		}
	}
	t.Fatalf("missing %s edge %s -> %s", relation, source, target)
}

func assertNoEdge(t *testing.T, files []core.IndexedFile, relation, source, target string) {
	t.Helper()
	sourceID := findUnit(t, files, source).ID
	targetID := findUnit(t, files, target).ID
	for _, indexed := range files {
		for _, edge := range indexed.Edges {
			if edge.Relation == relation && edge.SourceID == sourceID && edge.TargetID == targetID {
				t.Fatalf("unexpected %s edge %s -> %s", relation, source, target)
			}
		}
	}
}

func findEdgeTarget(t *testing.T, files []core.IndexedFile, relation, sourceID, targetID string) core.CodeEdge {
	t.Helper()
	for _, indexed := range files {
		for _, edge := range indexed.Edges {
			if edge.Relation == relation && edge.SourceID == sourceID && edge.TargetID == targetID {
				return edge
			}
		}
	}
	t.Fatalf("missing %s edge %s -> %s", relation, sourceID, targetID)
	return core.CodeEdge{}
}

func findCallTo(t *testing.T, files []core.IndexedFile, targetID string) core.CodeEdge {
	t.Helper()
	for _, indexed := range files {
		for _, edge := range indexed.Edges {
			if edge.Relation == "calls" && edge.TargetID == targetID {
				return edge
			}
		}
	}
	t.Fatalf("missing call to %s", targetID)
	return core.CodeEdge{}
}

func unitIDs(files []core.IndexedFile) []string {
	ids := []string{}
	for _, indexed := range files {
		for _, unit := range indexed.Units {
			ids = append(ids, unit.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func assertSortedResult(t *testing.T, result Result) {
	t.Helper()
	paths := make([]string, len(result.Files))
	for i, indexed := range result.Files {
		paths[i] = indexed.File.Path
		if !sort.SliceIsSorted(indexed.Units, func(i, j int) bool { return indexed.Units[i].ID < indexed.Units[j].ID }) {
			t.Fatalf("units for %s are not sorted", indexed.File.Path)
		}
		if !sort.SliceIsSorted(indexed.Edges, func(i, j int) bool {
			a, b := indexed.Edges[i], indexed.Edges[j]
			return fmt.Sprint(a.SourceID, "\x00", a.Relation, "\x00", a.TargetID, "\x00", a.StartLine) < fmt.Sprint(b.SourceID, "\x00", b.Relation, "\x00", b.TargetID, "\x00", b.StartLine)
		}) {
			t.Fatalf("edges for %s are not sorted", indexed.File.Path)
		}
	}
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("files are not sorted: %q", paths)
	}
}
