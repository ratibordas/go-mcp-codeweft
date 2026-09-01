//go:build integration

package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

func TestSearchesUseOnlyTheActiveFileHash(t *testing.T) {
	dsn := os.Getenv("CODEWEFT_TEST_CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("CODEWEFT_TEST_CLICKHOUSE_DSN is unset")
	}
	ctx := context.Background()
	s, err := New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	projectID := "store-integration-" + time.Now().UTC().Format("20060102T150405.000000000")
	defer func() {
		if err := s.Purge(context.Background(), projectID); err != nil {
			t.Logf("purge integration rows: %v", err)
		}
	}()
	oldHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	embedding := make([]float32, 1024)
	embedding[0] = 1
	old := core.IndexedFile{
		File:   core.FileState{ProjectID: projectID, Path: "a.md", Hash: oldHash, Generation: 1},
		Units:  []core.CodeUnit{{ID: "old-code", Name: "VisibilityOld", Path: "a.md", Source: "visibility old", FileHash: oldHash, Generation: 1, Weight: 1}},
		Chunks: []core.DocChunk{{ID: "old-doc", Path: "a.md", Extension: ".md", Content: "visibility old", SearchText: "visibility old", ChunkHash: oldHash, FileHash: oldHash, Generation: 1, Embedding: embedding}},
	}
	staleSameHash := core.IndexedFile{
		File:   core.FileState{ProjectID: projectID, Path: "a.md", Hash: newHash, Generation: 2},
		Units:  []core.CodeUnit{{ID: "stale-same-hash-code", Name: "VisibilityStale", Path: "a.md", Source: "visibility stale", FileHash: newHash, Generation: 2, Weight: 1}},
		Chunks: []core.DocChunk{{ID: "stale-same-hash-doc", Path: "a.md", Extension: ".md", Content: "visibility stale", SearchText: "visibility stale", ChunkHash: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", FileHash: newHash, Generation: 2, Embedding: embedding}},
	}
	newer := core.IndexedFile{
		File:   core.FileState{ProjectID: projectID, Path: "a.md", Hash: newHash, Generation: 3},
		Units:  []core.CodeUnit{{ID: "new-code", Name: "VisibilityNew", Path: "a.md", Source: "visibility new", FileHash: newHash, Generation: 3, Weight: 1}},
		Chunks: []core.DocChunk{{ID: "new-doc", Path: "a.md", Extension: ".md", Content: "visibility new", SearchText: "visibility new", ChunkHash: newHash, FileHash: newHash, Generation: 3, Embedding: embedding}},
	}
	if err := s.WriteDerived(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteDerived(ctx, staleSameHash); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteDerived(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if err := s.ActivateFile(ctx, newer.File); err != nil {
		t.Fatal(err)
	}

	assertCandidateIDs(t, searchCode(t, s, ctx, projectID), "new-code")
	assertCandidateIDs(t, searchDocsFTS(t, s, ctx, projectID), "new-doc")
	assertCandidateIDs(t, searchDocsVector(t, s, ctx, projectID), "new-doc")

	tombstone := newer.File
	tombstone.Generation = 4
	tombstone.Deleted = true
	if err := s.ActivateFile(ctx, tombstone); err != nil {
		t.Fatal(err)
	}
	assertCandidateIDs(t, searchCode(t, s, ctx, projectID))
	assertCandidateIDs(t, searchDocsFTS(t, s, ctx, projectID))
	assertCandidateIDs(t, searchDocsVector(t, s, ctx, projectID))
}

func searchCode(t *testing.T, s *Store, ctx context.Context, projectID string) []core.Candidate {
	t.Helper()
	got, err := s.SearchCode(ctx, projectID, "visibility", nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func searchDocsFTS(t *testing.T, s *Store, ctx context.Context, projectID string) []core.Candidate {
	t.Helper()
	got, err := s.SearchDocsFTS(ctx, projectID, "visibility", nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func searchDocsVector(t *testing.T, s *Store, ctx context.Context, projectID string) []core.Candidate {
	t.Helper()
	vector := make([]float32, 1024)
	vector[0] = 1
	got, err := s.SearchDocsVector(ctx, projectID, vector, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func assertCandidateIDs(t *testing.T, got []core.Candidate, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got candidates %+v, want IDs %v", got, want)
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("got candidates %+v, want IDs %v", got, want)
		}
	}
}
