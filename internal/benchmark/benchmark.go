package benchmark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/indexer"
)

type Query struct {
	Question        string   `json:"question"`
	ExpectedPaths   []string `json:"expected_paths,omitempty"`
	ExpectedSymbols []string `json:"expected_symbols,omitempty"`
	MaxTokens       int      `json:"max_tokens,omitempty"`
}

type Suite []Query

type RunMetrics struct {
	DurationMS      float64                  `json:"duration_ms"`
	Changed         int                      `json:"changed"`
	Deleted         int                      `json:"deleted"`
	Skipped         int                      `json:"skipped"`
	Failed          int                      `json:"failed"`
	FilesPerSecond  float64                  `json:"files_per_second"`
	ChunksPerSecond float64                  `json:"chunks_per_second"`
	PhaseTimings    map[string]time.Duration `json:"phase_timings,omitempty"`
}

type Report struct {
	Project              string     `json:"project"`
	Files                int        `json:"files"`
	Initial              RunMetrics `json:"initial"`
	OneFileDelta         RunMetrics `json:"one_file_delta"`
	AffectedPackageDelta RunMetrics `json:"affected_package_delta"`
	WarmRetrievalP95MS   float64    `json:"warm_retrieval_p95_ms"`
	GenerationMS         []float64  `json:"generation_ms"`
	StaleEvidence        int        `json:"stale_evidence"`
	BudgetViolations     int        `json:"budget_violations"`
}

type IndexService interface {
	Sync(context.Context, indexer.Mode, core.ProgressSink) (core.SyncResult, error)
	Status() core.IndexStatus
	Manifest() map[string]core.FileState
}

type RetrievalService interface {
	Retrieve(context.Context, core.SearchRequest, core.ProgressSink) (core.RetrievalResult, error)
	SearchContext(context.Context, core.SearchRequest, core.ProgressSink) (core.ContextResult, error)
}

type Services struct {
	Index     IndexService
	Retrieval RetrievalService
}

func LoadSuite(path string) (Suite, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open benchmark suite: %w", err)
	}
	defer file.Close()
	var suite Suite
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&suite); err != nil {
		return nil, fmt.Errorf("decode benchmark suite: %w", err)
	}
	if len(suite) == 0 {
		return nil, errors.New("benchmark suite is empty")
	}
	for index, query := range suite {
		if query.Question == "" {
			return nil, fmt.Errorf("benchmark query %d has no question", index+1)
		}
		if query.MaxTokens != 0 && (query.MaxTokens < 256 || query.MaxTokens > 12000) {
			return nil, fmt.Errorf("benchmark query %d max_tokens is out of range", index+1)
		}
	}
	return suite, nil
}

func Run(ctx context.Context, project string, suite Suite, dependencies ...Services) (Report, error) {
	if len(suite) == 0 {
		return Report{}, errors.New("benchmark suite is empty")
	}
	if len(dependencies) == 0 || dependencies[0].Index == nil || dependencies[0].Retrieval == nil {
		return Report{}, errors.New("benchmark services are required")
	}
	services := dependencies[0]
	started := time.Now()
	syncResult, err := services.Index.Sync(ctx, indexer.Full, nil)
	if err != nil {
		return Report{}, err
	}
	status := services.Index.Status()
	report := Report{Project: project}
	for _, file := range services.Index.Manifest() {
		if !file.Deleted {
			report.Files++
		}
	}
	report.Initial = RunMetrics{
		DurationMS: milliseconds(time.Since(started)), Changed: syncResult.Changed, Deleted: syncResult.Deleted,
		Skipped: syncResult.Skipped, Failed: syncResult.Failed, FilesPerSecond: status.Progress.FilesPerSecond,
		ChunksPerSecond: status.Progress.ChunksPerSecond, PhaseTimings: status.PhaseTimings,
	}
	durations := make([]time.Duration, 30)
	for index := range durations {
		query := suite[index%len(suite)]
		started = time.Now()
		if _, err := services.Retrieval.Retrieve(ctx, core.SearchRequest{Question: query.Question, MaxTokens: query.MaxTokens}, nil); err != nil {
			return Report{}, err
		}
		durations[index] = time.Since(started)
	}
	report.WarmRetrievalP95MS = milliseconds(percentile95(durations))
	for _, query := range suite {
		result, err := services.Retrieval.SearchContext(ctx, core.SearchRequest{Question: query.Question, MaxTokens: query.MaxTokens}, nil)
		if err != nil {
			return Report{}, err
		}
		report.GenerationMS = append(report.GenerationMS, milliseconds(result.Timing.Generation))
		if result.Budget.Used > result.Budget.Requested {
			report.BudgetViolations++
		}
		for _, evidence := range result.Evidence {
			if !currentEvidence(project, evidence) {
				report.StaleEvidence++
			}
		}
	}
	return report, nil
}

func currentEvidence(root string, evidence core.Evidence) bool {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return false
	}
	relative := filepath.Clean(filepath.FromSlash(evidence.Path))
	if evidence.Path == "" || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, relative))
	if err != nil {
		return false
	}
	relative, err = filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return false
	}
	lines := strings.Split(strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n"), "\n")
	if evidence.StartLine == 0 || evidence.EndLine < evidence.StartLine || uint64(evidence.EndLine) > uint64(len(lines)) {
		return false
	}
	want := evidence.Snippet
	if evidence.Type == "documentation" {
		want = evidence.Quote
	}
	return want != "" && strings.Join(lines[evidence.StartLine-1:evidence.EndLine], "\n") == want
}

func percentile95(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	values = append([]time.Duration(nil), values...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values[(95*len(values)+99)/100-1]
}

func milliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}
