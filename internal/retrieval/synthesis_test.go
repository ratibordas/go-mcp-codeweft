package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/graph"
)

func TestSynthesisUsesOnlyValidCitationsAndFilesystemEvidence(t *testing.T) {
	root := t.TempDir()
	writeEvidenceFile(t, root, "api.go", "package api\nfunc Handle() {}\n")
	manifest := evidenceManifest(root, "api.go")
	fresh := &contextFreshener{generation: 4, manifest: manifest}
	store := &fakeSearchStore{code: []core.Candidate{{ID: "stable", Type: "code", Path: "api.go", Symbol: "Handle", FileHash: manifest["api.go"].Hash, StartLine: 2, EndLine: 2, Content: "model copy", Weight: 1}}}
	generator := &fakeGenerator{responses: []string{`{"summary":"Use [C1], ignore [X9].","citations":["C1","C1","X9"]}`}}
	result, err := New(Config{ProjectID: "p", Root: root, Freshener: fresh, Store: store, Embedder: fakeEmbedder{}, Generator: generator}).SearchContext(
		context.Background(), core.SearchRequest{Question: "Handle", MaxTokens: 3500}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary != "Use [C1], ignore ." || len(result.Evidence) != 1 || result.Evidence[0].Snippet != "func Handle() {}" {
		t.Fatalf("result = %+v", result)
	}
}

func TestSynthesisGeneratorOutageFallsBackToRankedEvidence(t *testing.T) {
	root := t.TempDir()
	writeEvidenceFile(t, root, "api.go", "func Handle() {}\n")
	manifest := evidenceManifest(root, "api.go")
	fresh := &contextFreshener{generation: 2, manifest: manifest}
	store := &fakeSearchStore{code: []core.Candidate{{Type: "code", Path: "api.go", Symbol: "Handle", FileHash: manifest["api.go"].Hash, StartLine: 1, EndLine: 1, Content: "func Handle() {}", Weight: 1}}}
	result, err := New(Config{ProjectID: "p", Root: root, Freshener: fresh, Store: store, Generator: &fakeGenerator{err: errors.New("offline")}}).SearchContext(
		context.Background(), core.SearchRequest{Question: "Handle"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Evidence) != 1 || !containsText(result.Warnings, "offline") {
		t.Fatalf("result = %+v", result)
	}
}

func TestExpansionRunsOnceAndRejectsUnsafeTerms(t *testing.T) {
	root := t.TempDir()
	writeEvidenceFile(t, root, "docs.md", "# Customer\nCreate the handler.\n")
	manifest := evidenceManifest(root, "docs.md")
	store := &expandingStore{candidate: core.Candidate{Type: "doc", Path: "docs.md", Heading: "Customer", FileHash: manifest["docs.md"].Hash, StartLine: 1, EndLine: 2, Content: "Create the handler.", Weight: 1}}
	generator := &fakeGenerator{responses: []string{
		`{"terms":["customer handler","../secret","x; rm","customer handler"]}`,
		`{"summary":"See [D1].","citations":["D1"]}`,
	}}
	result, err := New(Config{ProjectID: "p", Root: root, Freshener: &contextFreshener{manifest: manifest}, Store: store, Generator: generator}).SearchContext(
		context.Background(), core.SearchRequest{Question: "where is it"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if generator.calls != 2 || store.calls.Load() != 4 || len(result.Evidence) != 1 {
		t.Fatalf("calls generator=%d store=%d result=%+v", generator.calls, store.calls.Load(), result)
	}
}

func TestMalformedSynthesisCannotSupplyEvidenceText(t *testing.T) {
	root := t.TempDir()
	writeEvidenceFile(t, root, "docs.md", "trusted quote\n")
	manifest := evidenceManifest(root, "docs.md")
	store := &fakeSearchStore{docs: []core.Candidate{{Type: "doc", Path: "docs.md", Heading: "Trusted", FileHash: manifest["docs.md"].Hash, StartLine: 1, EndLine: 1, Content: "trusted quote", Weight: 1}}}
	generator := &fakeGenerator{responses: []string{`{"summary":"Fake [D1]","citations":["D1"],"quote":"invented"}`}}
	result, err := New(Config{ProjectID: "p", Root: root, Freshener: &contextFreshener{manifest: manifest}, Store: store, Generator: generator}).SearchContext(
		context.Background(), core.SearchRequest{Question: "docs.md"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Evidence) != 1 || result.Evidence[0].Quote != "trusted quote" || result.Summary == "Fake [D1]" {
		t.Fatalf("result = %+v", result)
	}
}

func TestFinalBudgetSkipsWholeEvidence(t *testing.T) {
	root := t.TempDir()
	one := strings.Repeat("a", 700)
	two := strings.Repeat("b", 700)
	writeEvidenceFile(t, root, "one.md", one+"\n")
	writeEvidenceFile(t, root, "two.md", two+"\n")
	manifest := evidenceManifest(root, "one.md", "two.md")
	store := &fakeSearchStore{docs: []core.Candidate{
		{Type: "doc", Path: "one.md", Heading: "One", FileHash: manifest["one.md"].Hash, StartLine: 1, EndLine: 1, Content: one, Weight: 1},
		{Type: "doc", Path: "two.md", Heading: "Two", FileHash: manifest["two.md"].Hash, StartLine: 1, EndLine: 1, Content: two, Weight: 1},
	}}
	generator := &fakeGenerator{responses: []string{`{"summary":"Use [D1] and [D2].","citations":["D1","D2"]}`}}
	result, err := New(Config{ProjectID: "p", Root: root, Freshener: &contextFreshener{manifest: manifest}, Store: store, Generator: generator}).SearchContext(
		context.Background(), core.SearchRequest{Question: "one.md", MaxTokens: 256}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Budget.Truncated || len(result.Evidence) != 1 || result.Evidence[0].Quote != one || strings.Contains(result.Summary, "[D2]") {
		t.Fatalf("result = %+v", result)
	}
}

func TestModelSchemasAreValidJSON(t *testing.T) {
	if !json.Valid(expansionSchema) || !json.Valid(synthesisSchema) {
		t.Fatal("model schema is invalid JSON")
	}
}

type contextFreshener struct {
	generation uint64
	manifest   map[string]core.FileState
}

func (f *contextFreshener) EnsureFresh(context.Context, []string, core.ProgressSink) (core.SyncResult, error) {
	return core.SyncResult{Generation: f.generation}, nil
}
func (*contextFreshener) Graph() *graph.Graph                   { return nil }
func (f *contextFreshener) Manifest() map[string]core.FileState { return f.manifest }
func (f *contextFreshener) Status() core.IndexStatus {
	return core.IndexStatus{State: "ready", ActiveGeneration: f.generation}
}

type fakeGenerator struct {
	responses []string
	err       error
	calls     int
}

func (g *fakeGenerator) Generate(context.Context, core.GenerateRequest) (string, error) {
	g.calls++
	if g.err != nil {
		return "", g.err
	}
	if len(g.responses) == 0 {
		return "", errors.New("no response")
	}
	response := g.responses[0]
	g.responses = g.responses[1:]
	return response, nil
}

type expandingStore struct {
	candidate core.Candidate
	calls     atomic.Int64
}

func (s *expandingStore) SearchCode(context.Context, string, string, []string, int) ([]core.Candidate, error) {
	s.calls.Add(1)
	return nil, nil
}
func (s *expandingStore) SearchDocsFTS(_ context.Context, _, query string, _ []string, _ int) ([]core.Candidate, error) {
	s.calls.Add(1)
	if strings.Contains(query, "customer handler") {
		return []core.Candidate{s.candidate}, nil
	}
	return nil, nil
}
func (s *expandingStore) SearchDocsVector(context.Context, string, []float32, []string, int) ([]core.Candidate, error) {
	s.calls.Add(1)
	return nil, nil
}

func containsText(values []string, text string) bool {
	for _, value := range values {
		if strings.Contains(value, text) {
			return true
		}
	}
	return false
}
