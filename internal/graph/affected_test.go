package graph

import (
	"reflect"
	"testing"
)

func TestAffectedGoKeepsBodyOnlyChangesInOwningPackage(t *testing.T) {
	metadata := ChangeMetadata{
		GoFilePackage:   map[string]string{"pkg/a.go": "example/pkg"},
		GoReverseImport: map[string][]string{"example/pkg": {"example/direct"}, "example/direct": {"example/transitive"}},
		SurfaceChanged:  map[string]bool{"pkg/a.go": false},
	}
	if got := AffectedGo([]string{"./pkg/a.go"}, metadata); !reflect.DeepEqual(got, []string{"example/pkg"}) {
		t.Fatalf("affected packages = %v", got)
	}
}

func TestAffectedGoExpandsSurfaceAndUnknownChangesTransitively(t *testing.T) {
	metadata := ChangeMetadata{
		GoFilePackage:   map[string]string{"pkg/a.go": "example/pkg"},
		GoReverseImport: map[string][]string{"example/pkg": {"example/direct", "example/direct"}, "example/direct": {"example/transitive"}, "example/transitive": {"example/pkg"}},
		SurfaceChanged:  map[string]bool{"pkg/a.go": true},
	}
	if got := AffectedGo([]string{"pkg/a.go"}, metadata); !reflect.DeepEqual(got, []string{"example/direct", "example/pkg", "example/transitive"}) {
		t.Fatalf("surface packages = %v", got)
	}
	delete(metadata.SurfaceChanged, "pkg/a.go")
	if got := AffectedGo([]string{"pkg/a.go"}, metadata); !reflect.DeepEqual(got, []string{"example/direct", "example/pkg", "example/transitive"}) {
		t.Fatalf("unknown packages = %v", got)
	}
}

func TestAffectedScriptSupportsBodySurfaceDeletionAndResolutionScope(t *testing.T) {
	metadata := ChangeMetadata{
		ScriptReverseImport: map[string][]string{
			"src/api.ts":     {"src/client.ts", "src/client.ts"},
			"src/client.ts":  {"src/page.tsx"},
			"src/removed.ts": {"src/client.ts"},
		},
		SurfaceChanged:  map[string]bool{"src/api.ts": true, "src/body.ts": false, "src/removed.ts": true},
		ResolutionScope: map[string][]string{"tsconfig.json": {"src/api.ts", "src/body.ts", "src/api.ts"}},
	}
	if got := AffectedScript([]string{"src/body.ts"}, metadata); !reflect.DeepEqual(got, []string{"src/body.ts"}) {
		t.Fatalf("body files = %v", got)
	}
	if got := AffectedScript([]string{"src/api.ts", "src/removed.ts"}, metadata); !reflect.DeepEqual(got, []string{"src/api.ts", "src/client.ts", "src/page.tsx", "src/removed.ts"}) {
		t.Fatalf("surface/deletion files = %v", got)
	}
	if got := AffectedScript([]string{"./tsconfig.json"}, metadata); !reflect.DeepEqual(got, []string{"src/api.ts", "src/body.ts"}) {
		t.Fatalf("resolution scope files = %v", got)
	}
}

func TestAffectedGoUsesResolutionScopeAndUnknownOwnersConservatively(t *testing.T) {
	metadata := ChangeMetadata{
		GoFilePackage:   map[string]string{"pkg/a.go": "example/a", "pkg/b.go": "example/b", "other/c.go": "example/other"},
		GoReverseImport: map[string][]string{"example/a": {"example/b"}, "example/b": {"example/dependent"}, "example/other": {"example/unrelated"}},
		ResolutionScope: map[string][]string{"go.mod": {"example/a", "example/b", "example/a"}},
	}
	if got := AffectedGo([]string{"go.mod"}, metadata); !reflect.DeepEqual(got, []string{"example/a", "example/b", "example/dependent"}) {
		t.Fatalf("resolution packages = %v", got)
	}
	if got := AffectedGo([]string{"pkg/deleted.go", "pkg/renamed.go"}, metadata); !reflect.DeepEqual(got, []string{"example/a", "example/b", "example/dependent"}) {
		t.Fatalf("unknown sibling packages = %v", got)
	}
	if got := AffectedGo([]string{"gone/deleted.go"}, metadata); len(got) != 0 {
		t.Fatalf("unknown owner packages = %v", got)
	}
}

func TestAffectedGoFiltersSharedResolutionScopeToKnownPackages(t *testing.T) {
	metadata := ChangeMetadata{
		GoFilePackage:   map[string]string{"pkg/a.go": "example/a", "pkg/b.go": "example/b"},
		GoReverseImport: map[string][]string{"example/a": {"example/dependent"}},
		ResolutionScope: map[string][]string{
			"go.work":          {"example/a", "src/app.ts", "other/file.go", "arbitrary"},
			"pkg/package.json": {"src/app.ts"},
		},
	}
	if got := AffectedGo([]string{"go.work"}, metadata); !reflect.DeepEqual(got, []string{"example/a", "example/dependent"}) {
		t.Fatalf("mixed scope packages = %v", got)
	}
	if got := AffectedGo([]string{"pkg/package.json"}, metadata); !reflect.DeepEqual(got, []string{"example/a", "example/b", "example/dependent"}) {
		t.Fatalf("empty scope fallback packages = %v", got)
	}
}

func TestAffectedScriptNormalizesMetadataPathsAndAvoidsDuplicates(t *testing.T) {
	metadata := ChangeMetadata{
		ScriptReverseImport: map[string][]string{"./src/api.ts": {"src\\client.ts", "./src/client.ts"}},
		SurfaceChanged:      map[string]bool{"src\\api.ts": true},
	}
	if got := AffectedScript([]string{"./src/api.ts", "src\\api.ts"}, metadata); !reflect.DeepEqual(got, []string{"src/api.ts", "src/client.ts"}) {
		t.Fatalf("normalized files = %v", got)
	}
}

func TestAffectedSetsRejectEscapingPaths(t *testing.T) {
	metadata := ChangeMetadata{GoFilePackage: map[string]string{"pkg/a.go": "example/a"}}
	if got := AffectedGo([]string{"../pkg/a.go", "/pkg/a.go"}, metadata); len(got) != 0 {
		t.Fatalf("escaping Go paths = %v", got)
	}
	if got := AffectedScript([]string{"../src/a.ts", "/src/a.ts"}, ChangeMetadata{}); len(got) != 0 {
		t.Fatalf("escaping script paths = %v", got)
	}
}
