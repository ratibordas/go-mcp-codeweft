package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ratibordas/go-mcp-codeweft/internal/core"
)

const activeFilesCTE = `WITH active_files AS (
    SELECT project_id, path,
        tupleElement(state, 1) AS file_hash,
        tupleElement(state, 2) AS deleted,
        tupleElement(state, 3) AS generation
    FROM (
        SELECT project_id, path,
            argMax(tuple(file_hash, deleted, generation), generation) AS state
        FROM files
        WHERE project_id = ?
        GROUP BY project_id, path
    )
)`

func (s *Store) SearchCode(ctx context.Context, projectID, query string, paths []string, limit int) ([]core.Candidate, error) {
	normalized := normalizeSearchText(query)
	if normalized == "" {
		return nil, nil
	}
	pathSQL, pathArgs := pathPredicate("units", paths)
	sql := activeFilesCTE + `
        SELECT units.id, units.name, units.qualified_name, units.kind, units.language, units.extension,
            units.path, units.start_line, units.end_line, units.source, units.search_text, units.file_hash, units.weight
        FROM code_units AS units
		INNER JOIN active_files ON units.project_id = active_files.project_id
			AND units.path = active_files.path AND units.file_hash = active_files.file_hash
			AND units.generation = active_files.generation
        WHERE units.project_id = ? AND active_files.deleted = false
            AND hasAnyTokens(units.search_text, ?)` + pathSQL + `
        LIMIT ?`
	args := []any{projectID, projectID, normalized}
	args = append(args, pathArgs...)
	args = append(args, 50)
	rows, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("search code: %w", err)
	}
	defer rows.Close()
	candidates := make([]core.Candidate, 0, 50)
	for rows.Next() {
		var candidate core.Candidate
		var name, qualifiedName, kind, searchText string
		if err := rows.Scan(&candidate.ID, &name, &qualifiedName, &kind, &candidate.Language,
			&candidate.Extension, &candidate.Path, &candidate.StartLine, &candidate.EndLine,
			&candidate.Content, &searchText, &candidate.FileHash, &candidate.Weight); err != nil {
			return nil, fmt.Errorf("scan code search: %w", err)
		}
		candidate.Type = "code"
		candidate.Match = "full_text"
		candidate.Symbol = qualifiedName
		if candidate.Symbol == "" {
			candidate.Symbol = name
		}
		candidate.Score = lexicalScore(normalized, searchText, candidate.Weight)
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search code: %w", err)
	}
	return rankLexical(candidates, limit), nil
}

func (s *Store) SearchDocsFTS(ctx context.Context, projectID, query string, paths []string, limit int) ([]core.Candidate, error) {
	normalized := normalizeSearchText(query)
	if normalized == "" {
		return nil, nil
	}
	pathSQL, pathArgs := pathPredicate("chunks", paths)
	sql := activeFilesCTE + `
        SELECT chunks.id, chunks.path, chunks.extension, chunks.heading, chunks.start_line, chunks.end_line,
            chunks.content, chunks.search_text, chunks.file_hash
        FROM doc_chunks AS chunks
		INNER JOIN active_files ON chunks.project_id = active_files.project_id
			AND chunks.path = active_files.path AND chunks.file_hash = active_files.file_hash
			AND chunks.generation = active_files.generation
        WHERE chunks.project_id = ? AND active_files.deleted = false
            AND hasAnyTokens(chunks.search_text, ?)` + pathSQL + `
        LIMIT ?`
	args := []any{projectID, projectID, normalized}
	args = append(args, pathArgs...)
	args = append(args, 50)
	rows, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("search documentation: %w", err)
	}
	defer rows.Close()
	candidates := make([]core.Candidate, 0, 50)
	for rows.Next() {
		candidate := core.Candidate{Type: "doc", Match: "full_text", Weight: 1}
		var searchText string
		if err := rows.Scan(&candidate.ID, &candidate.Path, &candidate.Extension, &candidate.Heading,
			&candidate.StartLine, &candidate.EndLine, &candidate.Content, &searchText, &candidate.FileHash); err != nil {
			return nil, fmt.Errorf("scan documentation search: %w", err)
		}
		candidate.Score = lexicalScore(normalized, searchText, candidate.Weight)
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search documentation: %w", err)
	}
	return rankLexical(candidates, limit), nil
}

func (s *Store) SearchDocsVector(ctx context.Context, projectID string, vector []float32, paths []string, limit int) ([]core.Candidate, error) {
	if len(vector) == 0 {
		return nil, nil
	}
	if err := validateEmbedding(vector); err != nil {
		return nil, fmt.Errorf("search documentation vectors: %w", err)
	}
	pathSQL, pathArgs := pathPredicate("chunks", paths)
	sql := activeFilesCTE + `
        SELECT chunks.id, chunks.path, chunks.extension, chunks.heading, chunks.start_line, chunks.end_line,
            chunks.content, chunks.file_hash, cosineDistance(chunks.embedding, ?) AS distance
        FROM doc_chunks AS chunks
		INNER JOIN active_files ON chunks.project_id = active_files.project_id
			AND chunks.path = active_files.path AND chunks.file_hash = active_files.file_hash
			AND chunks.generation = active_files.generation
		WHERE chunks.project_id = ? AND active_files.deleted = false AND length(chunks.embedding) = 1024` + pathSQL + `
        ORDER BY distance ASC
        LIMIT ?`
	args := []any{projectID, vector, projectID}
	args = append(args, pathArgs...)
	args = append(args, boundedLimit(limit))
	rows, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("search documentation vectors: %w", err)
	}
	defer rows.Close()
	candidates := make([]core.Candidate, 0, boundedLimit(limit))
	for rows.Next() {
		candidate := core.Candidate{Type: "doc", Match: "vector", Weight: 1}
		var distance float64
		if err := rows.Scan(&candidate.ID, &candidate.Path, &candidate.Extension, &candidate.Heading,
			&candidate.StartLine, &candidate.EndLine, &candidate.Content, &candidate.FileHash, &distance); err != nil {
			return nil, fmt.Errorf("scan documentation vector search: %w", err)
		}
		candidate.Score = 1 - distance
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search documentation vectors: %w", err)
	}
	return candidates, nil
}

func (s *Store) LoadGraph(ctx context.Context, projectID string) ([]core.CodeUnit, []core.CodeEdge, error) {
	unitSQL := activeFilesCTE + `
        SELECT units.id, units.name, units.qualified_name, units.kind, units.language, units.extension,
            units.path, units.start_line, units.end_line, units.source, units.file_hash, units.generation, units.weight
        FROM code_units AS units
		INNER JOIN active_files ON units.project_id = active_files.project_id
			AND units.path = active_files.path AND units.file_hash = active_files.file_hash
			AND units.generation = active_files.generation
        WHERE units.project_id = ? AND active_files.deleted = false`
	unitRows, err := s.conn.Query(ctx, unitSQL, projectID, projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("load graph units: %w", err)
	}
	units := make([]core.CodeUnit, 0)
	for unitRows.Next() {
		var unit core.CodeUnit
		if err := unitRows.Scan(&unit.ID, &unit.Name, &unit.QualifiedName, &unit.Kind, &unit.Language,
			&unit.Extension, &unit.Path, &unit.StartLine, &unit.EndLine, &unit.Source,
			&unit.FileHash, &unit.Generation, &unit.Weight); err != nil {
			unitRows.Close()
			return nil, nil, fmt.Errorf("scan graph unit: %w", err)
		}
		units = append(units, unit)
	}
	if err := unitRows.Err(); err != nil {
		unitRows.Close()
		return nil, nil, fmt.Errorf("load graph units: %w", err)
	}
	if err := unitRows.Close(); err != nil {
		return nil, nil, fmt.Errorf("close graph units: %w", err)
	}

	edgeSQL := activeFilesCTE + `
        SELECT edges.source_id, edges.target_id, edges.relation, edges.path, edges.start_line,
            edges.end_line, edges.resolution, edges.file_hash, edges.generation
        FROM code_edges AS edges
		INNER JOIN active_files ON edges.project_id = active_files.project_id
			AND edges.path = active_files.path AND edges.file_hash = active_files.file_hash
			AND edges.generation = active_files.generation
        WHERE edges.project_id = ? AND active_files.deleted = false`
	edgeRows, err := s.conn.Query(ctx, edgeSQL, projectID, projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("load graph edges: %w", err)
	}
	defer edgeRows.Close()
	edges := make([]core.CodeEdge, 0)
	for edgeRows.Next() {
		var edge core.CodeEdge
		if err := edgeRows.Scan(&edge.SourceID, &edge.TargetID, &edge.Relation, &edge.Path,
			&edge.StartLine, &edge.EndLine, &edge.Resolution, &edge.FileHash, &edge.Generation); err != nil {
			return nil, nil, fmt.Errorf("scan graph edge: %w", err)
		}
		edges = append(edges, edge)
	}
	if err := edgeRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("load graph edges: %w", err)
	}
	return units, edges, nil
}

func pathPredicate(alias string, paths []string) (string, []any) {
	if len(paths) == 0 {
		return "", nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(paths)), ",")
	args := make([]any, len(paths))
	for i := range paths {
		args[i] = paths[i]
	}
	return " AND " + alias + ".path IN (" + placeholders + ")", args
}

func normalizeSearchText(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func lexicalScore(query, text string, weight float64) float64 {
	text = normalizeSearchText(text)
	seen := make(map[string]struct{})
	matched := 0
	for _, token := range strings.Fields(query) {
		if _, ok := seen[token]; !ok && strings.Contains(text, token) {
			seen[token] = struct{}{}
			matched++
		}
	}
	phrase := 0
	if strings.Contains(text, query) {
		phrase = 1
	}
	return float64(matched+phrase) * weight
}

func rankLexical(candidates []core.Candidate, limit int) []core.Candidate {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if candidates[i].Weight != candidates[j].Weight {
			return candidates[i].Weight > candidates[j].Weight
		}
		if candidates[i].Path != candidates[j].Path {
			return candidates[i].Path < candidates[j].Path
		}
		if candidates[i].StartLine != candidates[j].StartLine {
			return candidates[i].StartLine < candidates[j].StartLine
		}
		return candidates[i].ID < candidates[j].ID
	})
	limit = boundedLimit(limit)
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}

func boundedLimit(limit int) int {
	if limit <= 0 || limit > 50 {
		return 50
	}
	return limit
}
