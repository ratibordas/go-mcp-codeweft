package testutil

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
	"github.com/ratibordas/go-mcp-codeweft/internal/indexer"
)

type FakeStore struct {
	mu sync.Mutex

	ManifestState     map[string]core.FileState
	Embeddings        map[string][]float32
	Derived           []core.IndexedFile
	Activated         []core.FileState
	Runs              []indexer.RunSnapshot
	Calls             []string
	Generation        uint64
	CleanupError      error
	LookupError       error
	WriteError        error
	ActivateError     error
	LatestRun         indexer.RunSnapshot
	PendingEmbeddings []string
}

func NewFakeStore() *FakeStore {
	return &FakeStore{
		ManifestState: map[string]core.FileState{},
		Embeddings:    map[string][]float32{},
		Generation:    1,
	}
}

func (s *FakeStore) LoadManifest(context.Context, string) (map[string]core.FileState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneManifest(s.ManifestState), nil
}

func (s *FakeStore) NextGeneration(context.Context, string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	generation := s.Generation
	s.Generation++
	return generation, nil
}

func (s *FakeStore) LookupEmbeddings(_ context.Context, _ string, hashes []string) (map[string][]float32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.LookupError != nil {
		return nil, s.LookupError
	}
	result := map[string][]float32{}
	for _, hash := range hashes {
		if embedding, ok := s.Embeddings[hash]; ok {
			result[hash] = append([]float32(nil), embedding...)
		}
	}
	return result, nil
}

func (s *FakeStore) WriteDerived(_ context.Context, file core.IndexedFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, "write:"+file.File.Path)
	s.Derived = append(s.Derived, cloneIndexedFile(file))
	return s.WriteError
}

func (s *FakeStore) ActivateFile(_ context.Context, file core.FileState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, "activate:"+file.Path)
	if s.ActivateError != nil {
		return s.ActivateError
	}
	s.Activated = append(s.Activated, file)
	if file.Deleted {
		delete(s.ManifestState, file.Path)
	} else {
		s.ManifestState[file.Path] = file
	}
	return nil
}

func (s *FakeStore) LoadGraph(context.Context, string) ([]core.CodeUnit, []core.CodeEdge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	units := []core.CodeUnit{}
	edges := []core.CodeEdge{}
	for _, file := range s.Derived {
		active, ok := s.ManifestState[file.File.Path]
		if !ok || active.Hash != file.File.Hash || active.Generation != file.File.Generation {
			continue
		}
		units = append(units, file.Units...)
		edges = append(edges, file.Edges...)
	}
	return append([]core.CodeUnit(nil), units...), append([]core.CodeEdge(nil), edges...), nil
}

func (s *FakeStore) LoadLatestRun(context.Context, string) (indexer.RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.LatestRun.Clone(), nil
}

func (s *FakeStore) LoadEmbeddingPending(context.Context, string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.PendingEmbeddings...), nil
}

func (s *FakeStore) WriteRun(_ context.Context, run indexer.RunSnapshot) error {
	if !validRunID(run.RunID) {
		return fmt.Errorf("invalid run UUID %q", run.RunID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, "run:"+run.State)
	s.Runs = append(s.Runs, run.Clone())
	return nil
}

func validRunID(value string) bool {
	parts := strings.Split(value, "-")
	if len(parts) != 5 || len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 || parts[2][0] != '4' || !strings.ContainsRune("89ab", rune(parts[3][0])) {
		return false
	}
	for _, part := range parts {
		if _, err := strconv.ParseUint(part, 16, 64); err != nil {
			return false
		}
	}
	return true
}

func (s *FakeStore) CleanupObsolete(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, "cleanup")
	return s.CleanupError
}

func (s *FakeStore) WasActivated(path, hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, file := range s.Activated {
		if file.Path == path && file.Hash == hash {
			return true
		}
	}
	return false
}

func (s *FakeStore) SnapshotCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.Calls...)
}

func cloneManifest(source map[string]core.FileState) map[string]core.FileState {
	result := make(map[string]core.FileState, len(source))
	for path, file := range source {
		result[path] = file
	}
	return result
}

func cloneIndexedFile(file core.IndexedFile) core.IndexedFile {
	file.Units = append([]core.CodeUnit(nil), file.Units...)
	file.Edges = append([]core.CodeEdge(nil), file.Edges...)
	file.Warnings = append([]string(nil), file.Warnings...)
	file.Chunks = append([]core.DocChunk(nil), file.Chunks...)
	for i := range file.Chunks {
		file.Chunks[i].Tags = append([]string(nil), file.Chunks[i].Tags...)
		file.Chunks[i].Links = append([]string(nil), file.Chunks[i].Links...)
		file.Chunks[i].Embedding = append([]float32(nil), file.Chunks[i].Embedding...)
	}
	return file
}

func SortedPaths(files []core.FileState) []string {
	paths := make([]string, len(files))
	for i, file := range files {
		paths[i] = file.Path
	}
	sort.Strings(paths)
	return paths
}
