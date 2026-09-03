package retrieval

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/graph"
)

type Freshener interface {
	EnsureFresh(context.Context, []string, core.ProgressSink) (core.SyncResult, error)
	Graph() *graph.Graph
	Manifest() map[string]core.FileState
	Status() core.IndexStatus
}

type SearchStore interface {
	SearchCode(context.Context, string, string, []string, int) ([]core.Candidate, error)
	SearchDocsFTS(context.Context, string, string, []string, int) ([]core.Candidate, error)
	SearchDocsVector(context.Context, string, []float32, []string, int) ([]core.Candidate, error)
}

type Config struct {
	ProjectID  string
	Root       string
	Freshener  Freshener
	Store      SearchStore
	Embedder   core.Embedder
	Generator  core.Generator
	GraphDepth int
	MaxTokens  int
}

type Service struct{ cfg Config }

func New(cfg Config) *Service {
	if cfg.GraphDepth < 1 || cfg.GraphDepth > 2 {
		cfg.GraphDepth = 2
	}
	return &Service{cfg: cfg}
}

func (s *Service) Retrieve(ctx context.Context, req core.SearchRequest, sink core.ProgressSink) (core.RetrievalResult, error) {
	started := time.Now()
	if strings.TrimSpace(req.Question) == "" {
		return core.RetrievalResult{}, errors.New("question is required")
	}
	if s == nil || s.cfg.Freshener == nil || s.cfg.Store == nil {
		return core.RetrievalResult{}, errors.New("retrieval service is not configured")
	}
	paths, err := normalizePaths(req.Paths)
	if err != nil {
		return core.RetrievalResult{}, err
	}
	syncResult, err := s.cfg.Freshener.EnsureFresh(ctx, paths, sink)
	if err != nil {
		return core.RetrievalResult{}, err
	}
	searchPaths := expandPathScopes(paths, s.cfg.Freshener.Manifest())
	indexing := time.Since(started)
	retrievalStarted := time.Now()
	terms := extractTerms(req.Question)
	query := strings.TrimSpace(req.Question)
	if extra := strings.Join(terms, " "); extra != "" {
		query += " " + extra
	}

	type searchResult struct {
		kind       string
		candidates []core.Candidate
		err        error
	}
	results := make(chan searchResult, 3)
	go func() {
		items, searchErr := s.cfg.Store.SearchCode(ctx, s.cfg.ProjectID, query, searchPaths, 50)
		results <- searchResult{kind: "code", candidates: items, err: searchErr}
	}()
	go func() {
		items, searchErr := s.cfg.Store.SearchDocsFTS(ctx, s.cfg.ProjectID, query, searchPaths, 50)
		results <- searchResult{kind: "docs", candidates: items, err: searchErr}
	}()
	go func() {
		if s.cfg.Embedder == nil {
			results <- searchResult{kind: "vector", err: errors.New("embedder is not configured")}
			return
		}
		vectors, embedErr := s.cfg.Embedder.Embed(ctx, []string{req.Question})
		if embedErr != nil || len(vectors) != 1 {
			if embedErr == nil {
				embedErr = errors.New("query embedding is missing")
			}
			results <- searchResult{kind: "vector", err: embedErr}
			return
		}
		items, searchErr := s.cfg.Store.SearchDocsVector(ctx, s.cfg.ProjectID, vectors[0], searchPaths, 50)
		results <- searchResult{kind: "vector", candidates: items, err: searchErr}
	}()

	found := make(map[string]searchResult, 3)
	warnings := append([]string(nil), syncResult.Warnings...)
	for range 3 {
		result := <-results
		found[result.kind] = result
	}
	lists := make([][]core.Candidate, 0, 4)
	var requiredErr error
	for _, kind := range []string{"code", "docs", "vector"} {
		result := found[kind]
		if result.err != nil {
			if result.kind == "vector" {
				warnings = append(warnings, "document vector search unavailable: "+result.err.Error())
			} else {
				requiredErr = errors.Join(requiredErr, fmt.Errorf("%s search: %w", result.kind, result.err))
			}
			continue
		}
		if len(result.candidates) != 0 {
			lists = append(lists, result.candidates)
		}
	}
	if requiredErr != nil {
		return core.RetrievalResult{}, requiredErr
	}
	if graphCandidates := expandGraph(s.cfg.Freshener.Graph(), terms, searchPaths, s.cfg.GraphDepth); len(graphCandidates) != 0 {
		lists = append(lists, graphCandidates)
	}
	candidates := withinCandidateBudget(rank(lists, terms), candidateTokenBudget)
	sort.Strings(warnings)
	return core.RetrievalResult{
		Candidates: candidates, Warnings: unique(warnings), Generation: syncResult.Generation,
		Indexing: indexing, Retrieval: time.Since(retrievalStarted),
	}, nil
}

func expandPathScopes(scopes []string, manifest map[string]core.FileState) []string {
	if len(scopes) == 0 {
		return nil
	}
	result := []string{}
	for _, scope := range scopes {
		matched := false
		prefix := strings.TrimSuffix(scope, "/") + "/"
		for filePath, file := range manifest {
			if !file.Deleted && (filePath == scope || strings.HasPrefix(filePath, prefix)) {
				result = append(result, filePath)
				matched = true
			}
		}
		if !matched {
			result = append(result, scope)
		}
	}
	sort.Strings(result)
	return unique(result)
}

func (s *Service) Impact(ctx context.Context, req core.ImpactRequest, sink core.ProgressSink) (core.ImpactResult, error) {
	started := time.Now()
	if s == nil || s.cfg.Freshener == nil {
		return core.ImpactResult{}, errors.New("retrieval service is not configured")
	}
	origin := strings.TrimSpace(req.Symbol)
	paths := []string(nil)
	if req.Path != "" {
		if origin != "" {
			return core.ImpactResult{}, errors.New("exactly one of symbol and path is required")
		}
		var err error
		paths, err = normalizePaths([]string{req.Path})
		if err != nil {
			return core.ImpactResult{}, err
		}
		origin = paths[0]
	}
	if origin == "" {
		return core.ImpactResult{}, errors.New("exactly one of symbol and path is required")
	}
	if req.Direction != graph.Upstream && req.Direction != graph.Downstream && req.Direction != graph.Both {
		return core.ImpactResult{}, fmt.Errorf("unknown graph direction %q", req.Direction)
	}
	if req.Depth < 1 || req.Depth > 2 {
		return core.ImpactResult{}, errors.New("graph depth must be 1 or 2")
	}
	syncResult, err := s.cfg.Freshener.EnsureFresh(ctx, paths, sink)
	if err != nil {
		return core.ImpactResult{}, err
	}
	indexing := time.Since(started)
	graphStarted := time.Now()
	g := s.cfg.Freshener.Graph()
	if g == nil {
		return core.ImpactResult{}, errors.New("code graph is unavailable")
	}
	result := g.Impact(origin, req.Direction, req.Depth)
	if len(result.Warnings) != 0 {
		return core.ImpactResult{}, errors.New(result.Warnings[0])
	}
	result.Generation = syncResult.Generation
	result.Warnings = append(result.Warnings, syncResult.Warnings...)
	result.Timing.Indexing = indexing
	result.Timing.Retrieval = time.Since(graphStarted)
	result.Timing.Total = time.Since(started)
	return result, nil
}

func expandGraph(g *graph.Graph, terms, paths []string, depth int) []core.Candidate {
	if g == nil {
		return nil
	}
	allowed := map[string]bool{}
	for _, value := range paths {
		allowed[value] = true
	}
	result := []core.Candidate{}
	seen := map[string]bool{}
	for _, term := range terms {
		impact := g.Impact(term, graph.Both, depth)
		for _, candidate := range impact.Matches {
			if seen[candidateKey(candidate)] || len(allowed) != 0 && !allowed[candidate.Path] {
				continue
			}
			seen[candidateKey(candidate)] = true
			result = append(result, candidate)
		}
	}
	return result
}

func normalizePaths(values []string) ([]string, error) {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
		clean := path.Clean(value)
		if value == "" || clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("invalid project-relative path %q", value)
		}
		if !seen[clean] {
			seen[clean] = true
			result = append(result, clean)
		}
	}
	sort.Strings(result)
	return result, nil
}

func unique(values []string) []string {
	result := values[:0]
	for index, value := range values {
		if index == 0 || value != values[index-1] {
			result = append(result, value)
		}
	}
	return result
}
