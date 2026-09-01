CREATE TABLE IF NOT EXISTS files (
    project_id String, path String, kind LowCardinality(String), language LowCardinality(String),
    extension LowCardinality(String), size Int64, mtime_ns Int64, file_hash FixedString(64),
    parser_version String, generation UInt64, weight Float64, deleted Bool,
    activated_at DateTime64(3)
) ENGINE = ReplacingMergeTree(generation)
ORDER BY (project_id, path);

CREATE TABLE IF NOT EXISTS code_units (
    project_id String, id String, name String, qualified_name String, kind LowCardinality(String),
    language LowCardinality(String), extension LowCardinality(String), path String,
    start_line UInt32, end_line UInt32, source String, search_text String,
    file_hash FixedString(64), generation UInt64, weight Float64,
    INDEX code_text_idx search_text TYPE text(tokenizer = asciiCJK, preprocessor = lowerUTF8(search_text))
) ENGINE = MergeTree ORDER BY (project_id, file_hash, id);

CREATE TABLE IF NOT EXISTS code_edges (
    project_id String, source_id String, target_id String, relation LowCardinality(String),
    path String, start_line UInt32, end_line UInt32, resolution LowCardinality(String),
    file_hash FixedString(64), generation UInt64
) ENGINE = MergeTree ORDER BY (project_id, file_hash, source_id, relation, target_id);

CREATE TABLE IF NOT EXISTS doc_chunks (
    project_id String, id String, path String, extension LowCardinality(String), heading String,
    start_line UInt32, end_line UInt32, content String, search_text String,
    tags Array(String), links Array(String), chunk_hash FixedString(64),
    file_hash FixedString(64), generation UInt64, embedding Array(Float32),
    INDEX doc_text_idx search_text TYPE text(tokenizer = asciiCJK, preprocessor = lowerUTF8(search_text))
) ENGINE = MergeTree ORDER BY (project_id, file_hash, id);

CREATE TABLE IF NOT EXISTS index_runs (
    project_id String, run_id UUID, mode LowCardinality(String), state LowCardinality(String),
    started_at DateTime64(3), finished_at Nullable(DateTime64(3)), phase LowCardinality(String),
    completed UInt64, total UInt64, changed UInt64, deleted UInt64, skipped UInt64, failed UInt64,
    files_per_second Float64, chunks_per_second Float64, eta_ms Nullable(UInt64),
    phase_timings Map(String, UInt64), warnings Array(String), error String,
    start_generation UInt64, target_generation UInt64, git_head String,
    dirty_paths Array(String), updated_at DateTime64(3)
) ENGINE = ReplacingMergeTree(updated_at)
ORDER BY (project_id, run_id);
