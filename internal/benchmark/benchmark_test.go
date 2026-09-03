package benchmark

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/indexer"
)

func TestPercentile95UsesCeilingRank(t *testing.T) {
	values := []time.Duration{5, 1, 3, 2, 4, 8, 7, 6, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	if got := percentile95(values); got != 19 {
		t.Fatalf("p95 = %s", got)
	}
}

func TestRunCountsEvidenceThatDoesNotMatchCurrentFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "api.md"), []byte("current\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	services := &fakeServices{contextResult: core.ContextResult{
		Evidence: []core.Evidence{{Type: "documentation", Path: "api.md", StartLine: 1, EndLine: 1, Quote: "old"}},
		Budget:   core.Budget{Requested: 3500, Used: 100},
	}}
	report, err := Run(context.Background(), root, Suite{{Question: "Where?"}}, Services{Index: services, Retrieval: services})
	if err != nil {
		t.Fatal(err)
	}
	if report.StaleEvidence != 1 {
		t.Fatalf("stale evidence = %d", report.StaleEvidence)
	}
}

func TestRunMeasuresFullIndexThirtyWarmRetrievalsAndGeneration(t *testing.T) {
	services := &fakeServices{}
	report, err := Run(context.Background(), "/project", Suite{{Question: "Where?", MaxTokens: 3500}}, Services{Index: services, Retrieval: services})
	if err != nil {
		t.Fatal(err)
	}
	if services.syncMode != indexer.Full || services.retrievalCalls != 30 || services.contextCalls != 1 {
		t.Fatalf("sync=%q retrieval=%d context=%d", services.syncMode, services.retrievalCalls, services.contextCalls)
	}
	if report.Project != "/project" || report.Files != 2 || len(report.GenerationMS) != 1 || report.BudgetViolations != 0 {
		t.Fatalf("report = %+v", report)
	}
}

type fakeServices struct {
	syncMode                     indexer.Mode
	retrievalCalls, contextCalls int
	contextResult                core.ContextResult
}

func (f *fakeServices) Sync(_ context.Context, mode indexer.Mode, _ core.ProgressSink) (core.SyncResult, error) {
	f.syncMode = mode
	return core.SyncResult{Generation: 2, Changed: 2}, nil
}
func (*fakeServices) Status() core.IndexStatus {
	return core.IndexStatus{Progress: core.Progress{FilesPerSecond: 10}}
}
func (*fakeServices) Manifest() map[string]core.FileState {
	return map[string]core.FileState{"a.go": {Path: "a.go"}, "b.md": {Path: "b.md"}}
}
func (f *fakeServices) Retrieve(context.Context, core.SearchRequest, core.ProgressSink) (core.RetrievalResult, error) {
	f.retrievalCalls++
	return core.RetrievalResult{}, nil
}
func (f *fakeServices) SearchContext(context.Context, core.SearchRequest, core.ProgressSink) (core.ContextResult, error) {
	f.contextCalls++
	if f.contextResult.Budget.Requested != 0 {
		return f.contextResult, nil
	}
	return core.ContextResult{Timing: core.Timing{Generation: 2 * time.Millisecond}, Budget: core.Budget{Requested: 3500, Used: 100}}, nil
}
