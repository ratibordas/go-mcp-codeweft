package store

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

type recordedCall struct {
	query string
	args  []any
}

type recordingDB struct {
	clickhouse.Conn
	execs   []recordedCall
	queries []recordedCall
	batches []*recordingBatch
	results []driver.Rows
}

func newRecordingDB() *recordingDB { return &recordingDB{} }

func (db *recordingDB) Exec(_ context.Context, query string, args ...any) error {
	db.execs = append(db.execs, recordedCall{query: query, args: args})
	return nil
}

func (db *recordingDB) Query(_ context.Context, query string, args ...any) (driver.Rows, error) {
	db.queries = append(db.queries, recordedCall{query: query, args: args})
	if len(db.results) > 0 {
		rows := db.results[0]
		db.results = db.results[1:]
		return rows, nil
	}
	return emptyRows{}, nil
}

func (db *recordingDB) QueryRow(_ context.Context, query string, args ...any) driver.Row {
	db.queries = append(db.queries, recordedCall{query: query, args: args})
	return valueRow{values: []any{uint64(1)}}
}

func (db *recordingDB) PrepareBatch(_ context.Context, query string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	batch := &recordingBatch{query: query}
	db.batches = append(db.batches, batch)
	return batch, nil
}

func (db *recordingDB) containsInsert(table string) bool {
	needle := "INSERT INTO " + table
	for _, call := range db.execs {
		if strings.Contains(call.query, needle) {
			return true
		}
	}
	return false
}

type emptyRows struct{ driver.Rows }

func (emptyRows) Next() bool   { return false }
func (emptyRows) Close() error { return nil }
func (emptyRows) Err() error   { return nil }

type valueRow struct {
	driver.Row
	values []any
}

type sliceRows struct {
	driver.Rows
	rows  [][]any
	index int
}

func (r *sliceRows) Next() bool {
	if r.index >= len(r.rows) {
		return false
	}
	r.index++
	return true
}

func (r *sliceRows) Scan(dest ...any) error {
	for i, value := range r.rows[r.index-1] {
		target := reflect.ValueOf(dest[i]).Elem()
		source := reflect.ValueOf(value)
		if source.Type().ConvertibleTo(target.Type()) {
			source = source.Convert(target.Type())
		}
		target.Set(source)
	}
	return nil
}

func (*sliceRows) Close() error { return nil }
func (*sliceRows) Err() error   { return nil }

type recordingBatch struct {
	driver.Batch
	query string
	rows  [][]any
	sent  bool
}

func (b *recordingBatch) Append(values ...any) error {
	b.rows = append(b.rows, values)
	return nil
}

func (b *recordingBatch) Send() error  { b.sent = true; return nil }
func (b *recordingBatch) Close() error { return nil }

func (r valueRow) Scan(dest ...any) error {
	for i := range dest {
		switch target := dest[i].(type) {
		case *uint64:
			*target = r.values[i].(uint64)
		}
	}
	return nil
}

func TestActivateFileIsSeparateFromDerivedWrite(t *testing.T) {
	db := newRecordingDB()
	s := NewWithDB(db)
	batch := core.IndexedFile{File: core.FileState{ProjectID: "p", Path: "a.go", Hash: "new", Generation: 2}}
	if err := s.WriteDerived(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if db.containsInsert("files") {
		t.Fatal("derived write activated the file")
	}
	if err := s.ActivateFile(context.Background(), batch.File); err != nil {
		t.Fatal(err)
	}
	if !db.containsInsert("files") {
		t.Fatal("activation row was not written")
	}
}

func TestWriteDerivedWritesAllDerivedTables(t *testing.T) {
	db := newRecordingDB()
	file := core.FileState{ProjectID: "p", Path: "a.go", Hash: "hash", Generation: 2}
	indexed := core.IndexedFile{
		File:   file,
		Units:  []core.CodeUnit{{ID: "unit", Name: "FindMe", Path: file.Path, Source: "func FindMe() {}", FileHash: file.Hash, Generation: file.Generation}},
		Edges:  []core.CodeEdge{{SourceID: "unit", TargetID: "target", Relation: "calls", Path: file.Path, FileHash: file.Hash, Generation: file.Generation}},
		Chunks: []core.DocChunk{{ID: "chunk", Path: "README.md", Content: "Find me", FileHash: file.Hash, Generation: file.Generation}},
	}
	if err := NewWithDB(db).WriteDerived(context.Background(), indexed); err != nil {
		t.Fatal(err)
	}
	if len(db.batches) != 3 {
		t.Fatalf("got %d batches, want 3", len(db.batches))
	}
	for _, batch := range db.batches {
		if len(batch.rows) != 1 || !batch.sent {
			t.Fatalf("batch not written: %#v", batch)
		}
		if batch.rows[0][0] != "p" {
			t.Fatalf("project id not inherited from file: %#v", batch.rows[0])
		}
	}
	if db.containsInsert("files") {
		t.Fatal("derived batches activated the file")
	}
}

func TestWriteDerivedRejectsInvalidEmbeddingBeforeWriting(t *testing.T) {
	db := newRecordingDB()
	indexed := core.IndexedFile{
		File:   core.FileState{ProjectID: "p"},
		Units:  []core.CodeUnit{{ID: "would-have-been-written"}},
		Chunks: []core.DocChunk{{ID: "bad", Embedding: []float32{1, 2}}},
	}
	err := NewWithDB(db).WriteDerived(context.Background(), indexed)
	if err == nil || !strings.Contains(err.Error(), "embedding must contain 1024 floats") {
		t.Fatalf("got error %v, want embedding dimension error", err)
	}
	if len(db.batches) != 0 {
		t.Fatalf("invalid embedding caused partial derived writes: %#v", db.batches)
	}
}

func TestSearchDocsVectorRejectsInvalidEmbedding(t *testing.T) {
	db := newRecordingDB()
	_, err := NewWithDB(db).SearchDocsVector(context.Background(), "p", []float32{1, 2}, nil, 10)
	if err == nil || !strings.Contains(err.Error(), "embedding must contain 1024 floats") {
		t.Fatalf("got error %v, want embedding dimension error", err)
	}
	if len(db.queries) != 0 {
		t.Fatalf("invalid query embedding reached ClickHouse: %#v", db.queries)
	}
}

func TestMigrateCreatesOnlyConfiguredTables(t *testing.T) {
	db := newRecordingDB()
	if err := NewWithDB(db).Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(db.execs) != 5 {
		t.Fatalf("got %d migration statements, want 5", len(db.execs))
	}
	for _, table := range []string{"files", "code_units", "code_edges", "doc_chunks", "index_runs"} {
		found := false
		for _, call := range db.execs {
			found = found || strings.Contains(call.query, "CREATE TABLE IF NOT EXISTS "+table)
		}
		if !found {
			t.Fatalf("migration omitted %s", table)
		}
	}
	for _, call := range db.execs {
		if strings.Contains(strings.ToUpper(call.query), "CREATE DATABASE") {
			t.Fatalf("migration creates a database: %s", call.query)
		}
	}
}

func TestNextGenerationReadsMaximum(t *testing.T) {
	db := newRecordingDB()
	generation, err := NewWithDB(db).NextGeneration(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if generation != 1 {
		t.Fatalf("got generation %d, want 1", generation)
	}
	call := db.queries[0]
	if !strings.Contains(call.query, "max(generation)") || len(call.args) != 1 || call.args[0] != "p" {
		t.Fatalf("maximum generation query is not project scoped: %#v", call)
	}
}

func TestLoadManifestUsesArgMaxWithoutFinal(t *testing.T) {
	db := newRecordingDB()
	s := NewWithDB(db)
	if _, err := s.LoadManifest(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	query := db.queries[0].query
	if !strings.Contains(query, "argMax(tuple(") {
		t.Fatalf("manifest query does not select one aggregate current row:\n%s", query)
	}
	if strings.Contains(strings.ToUpper(query), "FINAL") {
		t.Fatalf("manifest correctness depends on merges:\n%s", query)
	}
}

func TestCurrentQueriesFilterActiveHash(t *testing.T) {
	db := newRecordingDB()
	s := NewWithDB(db)
	ctx := context.Background()
	if _, err := s.SearchCode(ctx, "p", "needle", nil, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SearchDocsFTS(ctx, "p", "needle", nil, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SearchDocsVector(ctx, "p", make([]float32, 1024), nil, 10); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LoadGraph(ctx, "p"); err != nil {
		t.Fatal(err)
	}

	if len(db.queries) != 5 {
		t.Fatalf("got %d queries, want 5", len(db.queries))
	}
	for _, call := range db.queries {
		query := compactSQL(call.query)
		if !strings.Contains(query, "argMax(tuple(file_hash, deleted, generation), generation)") {
			t.Fatalf("query lacks active row aggregate:\n%s", call.query)
		}
		if !strings.Contains(query, "tupleElement(state, 3) AS generation") {
			t.Fatalf("active row does not expose generation:\n%s", call.query)
		}
		if !strings.Contains(query, ".path = active_files.path") || !strings.Contains(query, ".file_hash = active_files.file_hash") {
			t.Fatalf("query does not join active path and hash:\n%s", call.query)
		}
		if !strings.Contains(query, ".generation = active_files.generation") {
			t.Fatalf("query does not hide older writes for the active hash:\n%s", call.query)
		}
		if !strings.Contains(query, "active_files.deleted = false") {
			t.Fatalf("query does not hide tombstones:\n%s", call.query)
		}
		if strings.Contains(strings.ToUpper(query), "FINAL") {
			t.Fatalf("query correctness depends on merges:\n%s", call.query)
		}
	}
	for _, index := range []int{0, 1} {
		if !strings.Contains(db.queries[index].query, "hasAnyTokens(") {
			t.Fatalf("FTS query %d does not use the text index:\n%s", index, db.queries[index].query)
		}
	}
	if !strings.Contains(compactSQL(db.queries[2].query), "length(chunks.embedding) = 1024") {
		t.Fatalf("vector search accepts invalid stored embedding dimensions:\n%s", db.queries[2].query)
	}
}

func TestSearchCodeRanksLexicalMatchesAndBindsPaths(t *testing.T) {
	db := newRecordingDB()
	db.results = []driver.Rows{&sliceRows{rows: [][]any{
		{"low", "Low", "pkg.Low", "function", "go", ".go", "safe.go", uint32(1), uint32(2), "needle only", "needle only", "hash", float64(1)},
		{"high", "High", "pkg.High", "function", "go", ".go", "safe.go", uint32(3), uint32(4), "needle phrase", "needle phrase", "hash", float64(2)},
	}}}
	path := "safe.go') OR 1 = 1 --"
	got, err := NewWithDB(db).SearchCode(context.Background(), "p", "needle phrase", []string{path}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "high" {
		t.Fatalf("lexical ranking did not prefer matched phrase and weight: %+v", got)
	}
	call := db.queries[0]
	if strings.Contains(call.query, path) || !strings.Contains(call.query, "units.path IN (?)") {
		t.Fatalf("path was not represented by a fixed SQL fragment: %#v", call)
	}
	if len(call.args) != 5 || call.args[3] != path || call.args[4] != 50 {
		t.Fatalf("path or candidate cap was not bound: %#v", call.args)
	}
}

func TestPurgeScopesEveryTable(t *testing.T) {
	db := newRecordingDB()
	if err := NewWithDB(db).Purge(context.Background(), "project-one"); err != nil {
		t.Fatal(err)
	}
	if len(db.execs) != 5 {
		t.Fatalf("got %d purge statements, want 5", len(db.execs))
	}
	for _, call := range db.execs {
		if !strings.Contains(call.query, "WHERE project_id = ?") {
			t.Fatalf("unscoped purge statement: %s", call.query)
		}
		if len(call.args) != 1 || call.args[0] != "project-one" {
			t.Fatalf("project scope is not bound: %#v", call)
		}
		if strings.Contains(call.query, "project-one") {
			t.Fatalf("project value interpolated into SQL: %s", call.query)
		}
	}
}

func TestCleanupObsoleteScopesDerivedRowsByActiveHash(t *testing.T) {
	db := newRecordingDB()
	if err := NewWithDB(db).CleanupObsolete(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if len(db.execs) != 3 {
		t.Fatalf("got %d cleanup statements, want 3", len(db.execs))
	}
	for _, call := range db.execs {
		query := compactSQL(call.query)
		if !strings.Contains(query, "argMax(tuple(file_hash, deleted, generation), generation)") ||
			!strings.Contains(query, "(path, file_hash, generation) NOT IN") {
			t.Fatalf("cleanup does not compare active path, hash, and generation: %s", call.query)
		}
		if len(call.args) != 2 || call.args[0] != "p" || call.args[1] != "p" {
			t.Fatalf("cleanup is not project scoped: %#v", call)
		}
	}
}

func TestWriteRunSnapshotIsProjectScoped(t *testing.T) {
	db := newRecordingDB()
	run := Run{ProjectID: "p", RunID: "00000000-0000-0000-0000-000000000001", Mode: "full", State: "running", StartedAt: time.Unix(1, 0), UpdatedAt: time.Unix(2, 0)}
	if err := NewWithDB(db).WriteRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if len(db.execs) != 1 || !strings.Contains(db.execs[0].query, "INSERT INTO index_runs") {
		t.Fatalf("run snapshot was not inserted: %#v", db.execs)
	}
	if len(db.execs[0].args) == 0 || db.execs[0].args[0] != "p" {
		t.Fatalf("run snapshot lost project scope: %#v", db.execs[0])
	}
}

func TestLoadRunHistoryUsesReplacingKeyWithoutFinal(t *testing.T) {
	db := newRecordingDB()
	if _, err := NewWithDB(db).LoadRunHistory(context.Background(), "p", 10); err != nil {
		t.Fatal(err)
	}
	query := db.queries[0].query
	if !strings.Contains(query, "argMax(") || strings.Contains(strings.ToUpper(query), "FINAL") {
		t.Fatalf("run history is merge-dependent: %s", query)
	}
	if len(db.queries[0].args) < 1 || db.queries[0].args[0] != "p" {
		t.Fatalf("run history is not project scoped: %#v", db.queries[0])
	}
}

func compactSQL(query string) string { return strings.Join(strings.Fields(query), " ") }
