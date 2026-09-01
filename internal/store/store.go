package store

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

type Store struct {
	conn clickhouse.Conn
	db   string
}

const embeddingDimensions = 1024

type Run struct {
	ProjectID        string
	RunID            string
	Mode             string
	State            string
	StartedAt        time.Time
	FinishedAt       *time.Time
	Phase            string
	Completed        uint64
	Total            uint64
	Changed          uint64
	Deleted          uint64
	Skipped          uint64
	Failed           uint64
	FilesPerSecond   float64
	ChunksPerSecond  float64
	ETAMillis        *uint64
	PhaseTimings     map[string]uint64
	Warnings         []string
	Error            string
	StartGeneration  uint64
	TargetGeneration uint64
	GitHead          string
	DirtyPaths       []string
	UpdatedAt        time.Time
}

func New(dsn string) (*Store, error) {
	options, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse ClickHouse DSN: %w", err)
	}
	conn, err := clickhouse.Open(options)
	if err != nil {
		return nil, fmt.Errorf("open ClickHouse: %w", err)
	}
	return &Store{conn: conn, db: options.Auth.Database}, nil
}

func NewWithDB(conn clickhouse.Conn) *Store { return &Store{conn: conn} }

func (s *Store) Close() error { return s.conn.Close() }

func (s *Store) WriteDerived(ctx context.Context, file core.IndexedFile) error {
	for _, chunk := range file.Chunks {
		if err := validateEmbedding(chunk.Embedding); err != nil {
			return fmt.Errorf("document chunk %q: %w", chunk.ID, err)
		}
	}
	unitRows := make([][]any, 0, len(file.Units))
	for _, unit := range file.Units {
		searchText := normalizeSearchText(unit.Name + " " + unit.QualifiedName + " " + unit.Source)
		unitRows = append(unitRows, []any{
			file.File.ProjectID, unit.ID, unit.Name, unit.QualifiedName, unit.Kind, unit.Language,
			unit.Extension, unit.Path, unit.StartLine, unit.EndLine, unit.Source, searchText,
			unit.FileHash, unit.Generation, unit.Weight,
		})
	}
	if err := s.insertRows(ctx, "INSERT INTO code_units (project_id, id, name, qualified_name, kind, language, extension, path, start_line, end_line, source, search_text, file_hash, generation, weight)", unitRows); err != nil {
		return fmt.Errorf("write code units: %w", err)
	}

	edgeRows := make([][]any, 0, len(file.Edges))
	for _, edge := range file.Edges {
		edgeRows = append(edgeRows, []any{
			file.File.ProjectID, edge.SourceID, edge.TargetID, edge.Relation, edge.Path,
			edge.StartLine, edge.EndLine, edge.Resolution, edge.FileHash, edge.Generation,
		})
	}
	if err := s.insertRows(ctx, "INSERT INTO code_edges (project_id, source_id, target_id, relation, path, start_line, end_line, resolution, file_hash, generation)", edgeRows); err != nil {
		return fmt.Errorf("write code edges: %w", err)
	}

	chunkRows := make([][]any, 0, len(file.Chunks))
	for _, chunk := range file.Chunks {
		searchText := chunk.SearchText
		if searchText == "" {
			searchText = chunk.Heading + " " + chunk.Content
		}
		chunkRows = append(chunkRows, []any{
			file.File.ProjectID, chunk.ID, chunk.Path, chunk.Extension, chunk.Heading,
			chunk.StartLine, chunk.EndLine, chunk.Content, normalizeSearchText(searchText), chunk.Tags,
			chunk.Links, chunk.ChunkHash, chunk.FileHash, chunk.Generation, chunk.Embedding,
		})
	}
	if err := s.insertRows(ctx, "INSERT INTO doc_chunks (project_id, id, path, extension, heading, start_line, end_line, content, search_text, tags, links, chunk_hash, file_hash, generation, embedding)", chunkRows); err != nil {
		return fmt.Errorf("write document chunks: %w", err)
	}
	return nil
}

func validateEmbedding(embedding []float32) error {
	if len(embedding) != 0 && len(embedding) != embeddingDimensions {
		return fmt.Errorf("embedding must contain %d floats, got %d", embeddingDimensions, len(embedding))
	}
	return nil
}

func (s *Store) insertRows(ctx context.Context, query string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	batch, err := s.conn.PrepareBatch(ctx, query)
	if err != nil {
		return err
	}
	defer batch.Close()
	for _, row := range rows {
		if err := batch.Append(row...); err != nil {
			return err
		}
	}
	return batch.Send()
}

func (s *Store) ActivateFile(ctx context.Context, file core.FileState) error {
	const query = `INSERT INTO files
        (project_id, path, kind, language, extension, size, mtime_ns, file_hash, parser_version, generation, weight, deleted, activated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if err := s.conn.Exec(ctx, query, file.ProjectID, file.Path, file.Kind, file.Language,
		file.Extension, file.Size, file.MTimeNS, file.Hash, file.ParserVersion,
		file.Generation, float64(1), file.Deleted, time.Now().UTC()); err != nil {
		return fmt.Errorf("activate file %q: %w", file.Path, err)
	}
	return nil
}

func (s *Store) LoadManifest(ctx context.Context, projectID string) (map[string]core.FileState, error) {
	const query = `SELECT path, state.1, state.2, state.3, state.4, state.5, state.6, state.7, state.8, state.9
        FROM (
            SELECT path,
                argMax(tuple(kind, language, extension, size, mtime_ns, file_hash, parser_version, generation, deleted), generation) AS state
            FROM files
            WHERE project_id = ?
            GROUP BY path
        )
        WHERE state.9 = false`
	rows, err := s.conn.Query(ctx, query, projectID)
	if err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}
	defer rows.Close()
	manifest := make(map[string]core.FileState)
	for rows.Next() {
		file := core.FileState{ProjectID: projectID}
		if err := rows.Scan(&file.Path, &file.Kind, &file.Language, &file.Extension, &file.Size,
			&file.MTimeNS, &file.Hash, &file.ParserVersion, &file.Generation, &file.Deleted); err != nil {
			return nil, fmt.Errorf("scan manifest: %w", err)
		}
		manifest[file.Path] = file
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}
	return manifest, nil
}

func (s *Store) NextGeneration(ctx context.Context, projectID string) (uint64, error) {
	const query = `SELECT coalesce(max(generation), 0) + 1 FROM files WHERE project_id = ?`
	var generation uint64
	if err := s.conn.QueryRow(ctx, query, projectID).Scan(&generation); err != nil {
		return 0, fmt.Errorf("load next generation: %w", err)
	}
	return generation, nil
}

func (s *Store) WriteRun(ctx context.Context, run Run) error {
	if run.UpdatedAt.IsZero() {
		run.UpdatedAt = time.Now().UTC()
	}
	const query = `INSERT INTO index_runs
        (project_id, run_id, mode, state, started_at, finished_at, phase, completed, total, changed, deleted, skipped, failed,
         files_per_second, chunks_per_second, eta_ms, phase_timings, warnings, error, start_generation, target_generation, git_head, dirty_paths, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if err := s.conn.Exec(ctx, query, run.ProjectID, run.RunID, run.Mode, run.State, run.StartedAt,
		run.FinishedAt, run.Phase, run.Completed, run.Total, run.Changed, run.Deleted, run.Skipped,
		run.Failed, run.FilesPerSecond, run.ChunksPerSecond, run.ETAMillis, run.PhaseTimings,
		run.Warnings, run.Error, run.StartGeneration, run.TargetGeneration, run.GitHead,
		run.DirtyPaths, run.UpdatedAt); err != nil {
		return fmt.Errorf("write index run: %w", err)
	}
	return nil
}

func (s *Store) LoadRunHistory(ctx context.Context, projectID string, limit int) ([]Run, error) {
	limit = boundedLimit(limit)
	const query = `SELECT run_id,
            argMax(mode, updated_at), argMax(state, updated_at), argMax(started_at, updated_at), argMax(finished_at, updated_at),
            argMax(phase, updated_at), argMax(completed, updated_at), argMax(total, updated_at), argMax(changed, updated_at),
            argMax(deleted, updated_at), argMax(skipped, updated_at), argMax(failed, updated_at),
            argMax(files_per_second, updated_at), argMax(chunks_per_second, updated_at), argMax(eta_ms, updated_at),
            argMax(phase_timings, updated_at), argMax(warnings, updated_at), argMax(error, updated_at),
            argMax(start_generation, updated_at), argMax(target_generation, updated_at), argMax(git_head, updated_at),
            argMax(dirty_paths, updated_at), max(updated_at) AS latest
        FROM index_runs
        WHERE project_id = ?
        GROUP BY run_id
        ORDER BY latest DESC
        LIMIT ?`
	rows, err := s.conn.Query(ctx, query, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("load run history: %w", err)
	}
	defer rows.Close()
	runs := make([]Run, 0, limit)
	for rows.Next() {
		run := Run{ProjectID: projectID}
		if err := rows.Scan(&run.RunID, &run.Mode, &run.State, &run.StartedAt, &run.FinishedAt,
			&run.Phase, &run.Completed, &run.Total, &run.Changed, &run.Deleted, &run.Skipped,
			&run.Failed, &run.FilesPerSecond, &run.ChunksPerSecond, &run.ETAMillis,
			&run.PhaseTimings, &run.Warnings, &run.Error, &run.StartGeneration,
			&run.TargetGeneration, &run.GitHead, &run.DirtyPaths, &run.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan run history: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load run history: %w", err)
	}
	return runs, nil
}

func (s *Store) CleanupObsolete(ctx context.Context, projectID string) error {
	const active = `(SELECT path, file_hash, generation FROM (
        SELECT path,
            tupleElement(state, 1) AS file_hash,
            tupleElement(state, 2) AS deleted,
            tupleElement(state, 3) AS generation
        FROM (
            SELECT path, argMax(tuple(file_hash, deleted, generation), generation) AS state
            FROM files WHERE project_id = ? GROUP BY path
        )
	) WHERE deleted = false)`
	for _, table := range []string{"code_units", "code_edges", "doc_chunks"} {
		query := "ALTER TABLE " + table + " DELETE WHERE project_id = ? AND (path, file_hash, generation) NOT IN " + active
		if err := s.conn.Exec(ctx, query, projectID, projectID); err != nil {
			return fmt.Errorf("cleanup %s: %w", table, err)
		}
	}
	return nil
}

func (s *Store) Purge(ctx context.Context, projectID string) error {
	for _, table := range []string{"code_units", "code_edges", "doc_chunks", "files", "index_runs"} {
		if err := s.conn.Exec(ctx, "ALTER TABLE "+table+" DELETE WHERE project_id = ?", projectID); err != nil {
			return fmt.Errorf("purge %s: %w", table, err)
		}
	}
	return nil
}
