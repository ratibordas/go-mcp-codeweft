package retrieval

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/graph"
)

func TestRetrieveUsesPathScopeAndFallsBackWhenVectorsFail(t *testing.T) {
	store := &fakeSearchStore{
		code:      []core.Candidate{{ID: "C1", Path: "src/api.ts", Symbol: "getUser", Content: "export function getUser() {}", Weight: 1}},
		docs:      []core.Candidate{{ID: "D1", Path: "docs/api.md", Heading: "Get User", Content: "Call getUser.", Weight: 1}},
		vectorErr: errors.New("vectors unavailable"),
	}
	fresh := &fakeFreshener{generation: 7}
	service := New(Config{ProjectID: "p", Freshener: fresh, Store: store, Embedder: fakeEmbedder{}})
	result, err := service.Retrieve(context.Background(), core.SearchRequest{Question: "getUser", Paths: []string{"src/api.ts"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Generation != 7 || len(result.Candidates) != 2 || len(result.Warnings) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if !reflect.DeepEqual(store.paths, []string{"src/api.ts"}) || !reflect.DeepEqual(fresh.paths, []string{"src/api.ts"}) {
		t.Fatalf("scopes store=%v fresh=%v", store.paths, fresh.paths)
	}
}

func TestRetrieveExpandsPathPrefixesFromActiveManifest(t *testing.T) {
	store := &fakeSearchStore{}
	fresh := &fakeFreshener{manifest: map[string]core.FileState{
		"docs/api.md":      {Path: "docs/api.md"},
		"docs/internal.md": {Path: "docs/internal.md", Deleted: true},
		"src/api.ts":       {Path: "src/api.ts"},
	}}
	service := New(Config{ProjectID: "p", Freshener: fresh, Store: store})
	_, err := service.Retrieve(context.Background(), core.SearchRequest{Question: "API", Paths: []string{"docs"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.paths, []string{"docs"}) {
		t.Fatalf("freshness scope = %v", fresh.paths)
	}
	if !reflect.DeepEqual(store.paths, []string{"docs/api.md"}) {
		t.Fatalf("search scope = %v", store.paths)
	}
}

func TestRetrieveAddsGraphNeighborsAtDepthTwo(t *testing.T) {
	units := []core.CodeUnit{
		{ID: "A", Name: "Handle", QualifiedName: "api.Handle", Path: "api.go", Source: "func Handle()", Weight: 1},
		{ID: "B", Name: "Serve", Path: "server.go", Source: "func Serve()", Weight: 1},
		{ID: "C", Name: "Run", Path: "main.go", Source: "func Run()", Weight: 1},
	}
	edges := []core.CodeEdge{{SourceID: "A", TargetID: "B", Relation: "calls"}, {SourceID: "B", TargetID: "C", Relation: "calls"}}
	fresh := &fakeFreshener{generation: 3, graph: graph.New(units, edges)}
	store := &fakeSearchStore{code: []core.Candidate{{ID: "A", Symbol: "api.Handle", Path: "api.go", Weight: 1}}}
	result, err := New(Config{ProjectID: "p", Freshener: fresh, Store: store, GraphDepth: 2}).Retrieve(
		context.Background(), core.SearchRequest{Question: "Handle"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if match(result.Candidates, "B") != "graph_distance_1" || match(result.Candidates, "C") != "graph_distance_2" {
		t.Fatalf("graph candidates = %+v", result.Candidates)
	}
}

func TestRetrieveFusionIsDeterministic(t *testing.T) {
	store := &fakeSearchStore{
		docs:    []core.Candidate{{ID: "D1", Path: "docs/api.md", Heading: "API Guide", Match: "full_text", Weight: 1}},
		vectors: []core.Candidate{{ID: "D1", Path: "docs/api.md", Heading: "API Guide", Match: "vector", Weight: 1}},
	}
	service := New(Config{ProjectID: "p", Freshener: &fakeFreshener{}, Store: store, Embedder: fakeEmbedder{}})
	for range 20 {
		result, err := service.Retrieve(context.Background(), core.SearchRequest{Question: "API Guide"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Candidates) != 1 || result.Candidates[0].Match != "full_text" {
			t.Fatalf("result = %+v", result)
		}
	}
}

func TestImpactIsFreshAndGraphOnly(t *testing.T) {
	units := []core.CodeUnit{{ID: "A", Name: "Handle", Path: "api.go"}, {ID: "B", Name: "Serve", Path: "server.go"}}
	fresh := &fakeFreshener{generation: 9, graph: graph.New(units, []core.CodeEdge{{SourceID: "A", TargetID: "B", Relation: "calls"}})}
	embed := &countingEmbedder{}
	result, err := New(Config{ProjectID: "p", Freshener: fresh, Store: &fakeSearchStore{}, Embedder: embed}).Impact(
		context.Background(), core.ImpactRequest{Symbol: "Handle", Direction: graph.Downstream, Depth: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Generation != 9 || result.Origin.ID != "A" || len(result.Matches) != 1 || embed.calls != 0 {
		t.Fatalf("impact = %+v, embed calls = %d", result, embed.calls)
	}
}

func TestImpactRejectsAmbiguousSymbolWithSortedAlternatives(t *testing.T) {
	units := []core.CodeUnit{{ID: "z", Name: "Open", Path: "z.go"}, {ID: "a", Name: "Open", Path: "a.go"}}
	fresh := &fakeFreshener{graph: graph.New(units, nil)}
	_, err := New(Config{ProjectID: "p", Freshener: fresh, Store: &fakeSearchStore{}}).Impact(
		context.Background(), core.ImpactRequest{Symbol: "Open", Direction: graph.Both, Depth: 1}, nil)
	if err == nil || err.Error() != `ambiguous symbol "Open": a, z` {
		t.Fatalf("error = %v", err)
	}
}

type fakeFreshener struct {
	generation uint64
	paths      []string
	graph      *graph.Graph
	manifest   map[string]core.FileState
}

func (f *fakeFreshener) EnsureFresh(_ context.Context, paths []string, _ core.ProgressSink) (core.SyncResult, error) {
	f.paths = append([]string(nil), paths...)
	return core.SyncResult{Generation: f.generation}, nil
}

func (f *fakeFreshener) Graph() *graph.Graph { return f.graph }

func (f *fakeFreshener) Manifest() map[string]core.FileState { return f.manifest }

func (f *fakeFreshener) Status() core.IndexStatus {
	return core.IndexStatus{State: "ready", ActiveGeneration: f.generation}
}

type fakeSearchStore struct {
	code, docs, vectors []core.Candidate
	vectorErr           error
	paths               []string
}

func (s *fakeSearchStore) SearchCode(_ context.Context, _ string, _ string, paths []string, _ int) ([]core.Candidate, error) {
	s.paths = append([]string(nil), paths...)
	return append([]core.Candidate(nil), s.code...), nil
}

func (s *fakeSearchStore) SearchDocsFTS(_ context.Context, _ string, _ string, paths []string, _ int) ([]core.Candidate, error) {
	return append([]core.Candidate(nil), s.docs...), nil
}

func (s *fakeSearchStore) SearchDocsVector(_ context.Context, _ string, _ []float32, paths []string, _ int) ([]core.Candidate, error) {
	return append([]core.Candidate(nil), s.vectors...), s.vectorErr
}

type fakeEmbedder struct{}

func (fakeEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return [][]float32{{1}}, nil
}

type countingEmbedder struct{ calls int }

func (e *countingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	e.calls++
	return [][]float32{{1}}, nil
}

func match(values []core.Candidate, id string) string {
	for _, value := range values {
		if value.ID == id {
			return value.Match
		}
	}
	return ""
}
