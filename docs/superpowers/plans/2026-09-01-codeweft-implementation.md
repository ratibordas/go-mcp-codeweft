# Codeweft MVP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a local, single-project MCP server that incrementally indexes Go, JavaScript/TypeScript, and Markdown into ClickHouse and returns compact, current, cited project context through Ollama.

**Architecture:** A single Go process owns project discovery, deterministic language parsing, an in-memory dependency graph, ClickHouse persistence, retrieval, and MCP STDIO. Git narrows freshness candidates, a hash manifest establishes truth, and per-file activation prevents stale evidence. Ollama calls are direct, serialized, bounded, and optional at retrieval time.

**Tech Stack:** Go 1.27, official MCP Go SDK v1.6.1, ClickHouse 26.8.1.2041 with `clickhouse-go` v2.48.0, `golang.org/x/tools` v0.49.0, tree-sitter Go binding v0.25.0, JavaScript grammar v0.25.0, TypeScript grammar v0.23.2, Ollama native HTTP API, YAML v3.

**Spec:** `docs/superpowers/specs/2026-09-01-codeweft-design.md`

## Global Constraints

- Canonical module: `github.com/ratibordas/go-mcp-codeweft`; binary and product name: `codeweft`.
- One absolute project root per process; MCP input can never change it.
- Go 1.27; macOS and Linux on amd64 and arm64; Windows through WSL or Docker.
- MCP uses STDIO only; ClickHouse and Codeweft use Docker Compose by default; Ollama is external.
- Generation model: `qwen3.6:35b-a3b-q4_K_M`, 65,536 context, 12,000-token synthesis input, 900-token maximum output, thinking disabled.
- Embedding model: `qwen3-embedding:0.6b`, 1,024 dimensions, batch size 16; all Ollama calls are serialized.
- Returned context defaults to 3,500 estimated tokens; graph expansion depth never exceeds 2.
- Source support: `.go`, `.ts`, `.tsx`, `.d.ts`, `.js`, `.jsx`, `.mjs`, `.cjs`, and `.md`.
- No LangChain, LangGraph, source-code embeddings, watcher, Node sidecar, HTTP MCP, telemetry, project-code execution, or dependency download in the MVP.
- Documentation and user-facing text are English. Comments explain only non-obvious safety or consistency invariants.
- Tests are written before production behavior and use the Go standard testing package.
- Do not create Git commits. Each task ends with a read-only review checkpoint and leaves changes uncommitted.

---

## File Structure

```text
.
├── cmd/codeweft/main.go                 command entry point
├── internal/app/app.go                  dependency construction and command services
├── internal/config/config.go            flags, environment, YAML, validation
├── internal/core/types.go               shared immutable domain records
├── internal/core/interfaces.go          narrow cross-package contracts
├── internal/project/discover.go         supported-file discovery and safety policy
├── internal/project/git.go              NUL-delimited Git inspection
├── internal/project/freshness.go        manifest comparison and affected candidates
├── internal/ollama/client.go             serialized native Ollama client
├── internal/markdown/parser.go           Markdown and Obsidian chunk extraction
├── internal/goparser/parser.go           Go declarations, types, SSA, and edges
├── internal/tsparser/parser.go           JS/TS tree-sitter extraction
├── internal/tsparser/resolve.go          local module and alias resolution
├── internal/graph/graph.go               adjacency maps and bounded traversal
├── internal/graph/affected.go            reverse-dependent invalidation
├── internal/store/store.go               ClickHouse implementation
├── internal/store/search.go              current-row search queries
├── internal/store/migrations.go          embedded migration runner
├── internal/store/migrations/001_init.sql five MVP tables and indexes
├── internal/indexer/indexer.go           shared delta/full indexing coordinator
├── internal/indexer/progress.go          run status, throughput, and ETA
├── internal/retrieval/retrieval.go       exact/FTS/vector retrieval and fusion
├── internal/retrieval/rank.go            RRF, boosts, graph expansion, budgets
├── internal/retrieval/evidence.go        current-file evidence extraction
├── internal/retrieval/synthesis.go       expansion and cited local synthesis
├── internal/mcpserver/server.go          SDK server and tool registration
├── internal/mcpserver/tools.go           typed tool inputs and handlers
├── internal/benchmark/benchmark.go       timing and freshness benchmark runner
├── internal/testutil/fake_store.go        small shared in-memory test double
├── testdata/go-multimodule/              Go parser fixture
├── testdata/script-project/              JS/TS parser fixture
├── testdata/obsidian/                     Markdown fixture vault
├── testdata/eval/queries.json             reviewed retrieval cases
├── integration/e2e_test.go                process-level freshness tests
├── Dockerfile
├── compose.yaml
├── .dockerignore
├── .env.example
├── .gitignore
├── .codeweft.example.yaml
├── LICENSE
├── README.md
└── docs/integrations/{codex,claude}.md
```

Tests live beside their package unless they exercise the complete process. Keep production files focused; do not introduce repositories, factories, event buses, or one-implementation interfaces beyond the cross-package contracts listed below.

---

### Task 1: Bootstrap the Module, Domain Types, and Configuration

**Files:**

- Create: `go.mod`
- Create: `cmd/codeweft/main.go`
- Create: `internal/core/types.go`
- Create: `internal/core/interfaces.go`
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`
- Create: `.codeweft.example.yaml`
- Modify: `.gitignore`

**Interfaces:**

- Produces: `config.Load(args []string, lookup func(string) (string, bool)) (config.Config, error)`.
- Produces: canonical records `core.FileState`, `core.CodeUnit`, `core.CodeEdge`, `core.DocChunk`, `core.IndexedFile`, `core.Candidate`, `core.Progress`, `core.SearchRequest`, `core.RetrievalResult`, `core.ContextResult`, `core.ImpactRequest`, `core.ImpactResult`, `core.GenerateRequest`, `core.ModelHealth`, and `core.Evidence`.
- Produces: `core.Embedder`, `core.Generator`, and `core.ProgressSink` contracts used by later tasks.

- [ ] **Step 1: Write configuration tests**

Create table-driven tests for defaults, environment precedence, YAML overrides, absolute-root enforcement, root canonicalization, output-budget bounds, graph-depth bounds, and missing `--project`.

```go
func TestLoadDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load([]string{"--project", dir}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	wantRoot, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectRoot != wantRoot || cfg.Retrieval.MaxTokens != 3500 || cfg.Retrieval.GraphDepth != 2 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.Ollama.GenerationModel != "qwen3.6:35b-a3b-q4_K_M" || cfg.Ollama.ContextTokens != 65536 {
		t.Fatalf("unexpected generation defaults: %+v", cfg.Ollama)
	}
	if cfg.Ollama.EmbeddingModel != "qwen3-embedding:0.6b" || cfg.Ollama.EmbeddingDimensions != 1024 {
		t.Fatalf("unexpected embedding defaults: %+v", cfg.Ollama)
	}
}

func TestLoadRejectsRelativeProject(t *testing.T) {
	_, err := Load([]string{"--project", "relative/project"}, func(string) (string, bool) { return "", false })
	if err == nil {
		t.Fatal("expected relative project root to fail")
	}
}
```

- [ ] **Step 2: Run the tests and verify the expected failure**

Run: `go test ./internal/config`

Expected: FAIL because `Load` and configuration types do not exist.

- [ ] **Step 3: Create the pinned Go module**

Use this direct dependency set and let `go mod tidy` record indirect dependencies:

```go
module github.com/ratibordas/go-mcp-codeweft

go 1.27.0

require (
	github.com/ClickHouse/clickhouse-go/v2 v2.48.0
	github.com/modelcontextprotocol/go-sdk v1.6.1
	github.com/tree-sitter/go-tree-sitter v0.25.0
	github.com/tree-sitter/tree-sitter-javascript v0.25.0
	github.com/tree-sitter/tree-sitter-typescript v0.23.2
	golang.org/x/tools v0.49.0
	gopkg.in/yaml.v3 v3.0.1
)
```

Run: `go mod tidy`

Expected: `go.sum` is created and module resolution succeeds.

- [ ] **Step 4: Define the shared records without behavior-heavy abstractions**

Use strings for SHA-256 hex values and stable IDs so they map directly to ClickHouse and JSON.

```go
type FileState struct {
	ProjectID     string
	Path          string
	Kind          string
	Language      string
	Extension     string
	Size          int64
	MTimeNS       int64
	Hash          string
	ParserVersion string
	Generation    uint64
	Deleted       bool
}

type CodeUnit struct {
	ID, Name, QualifiedName, Kind, Language, Extension, Path, Source, FileHash string
	StartLine, EndLine                                                         uint32
	Generation                                                                 uint64
	Weight                                                                     float64
}

type CodeEdge struct {
	SourceID, TargetID, Relation, Path, FileHash, Resolution string
	StartLine, EndLine                                       uint32
	Generation                                               uint64
}

type DocChunk struct {
	ID, Path, Extension, Heading, Content, SearchText, ChunkHash, FileHash string
	StartLine, EndLine                                                    uint32
	Tags, Links                                                           []string
	Embedding                                                             []float32
	Generation                                                            uint64
}

type IndexedFile struct {
	File   FileState
	Units  []CodeUnit
	Edges  []CodeEdge
	Chunks []DocChunk
	Warnings []string
}

type Candidate struct {
	ID, Type, Match, Language, Extension, Path, Symbol, Relation, Heading, FileHash string
	StartLine, EndLine                                                                uint32
	Score, Weight                                                                     float64
	Content                                                                           string
}

type SearchRequest struct {
	Question  string
	Paths     []string
	MaxTokens int
}

type ImpactRequest struct {
	Symbol, Path, Direction string
	Depth                   int
}

type GenerateRequest struct {
	Prompt string
	Schema json.RawMessage
}

type Embedder interface {
	Embed(context.Context, []string) ([][]float32, error)
}

type Generator interface {
	Generate(context.Context, GenerateRequest) (string, error)
}

type Progress struct {
	Phase                                      string
	Completed, Total                          uint64
	Changed, Deleted, Skipped, Failed         uint64
	Elapsed, ETA                              time.Duration
	FilesPerSecond, ChunksPerSecond           float64
}

type IndexStatus struct {
	State, Phase, LastError                    string
	ActiveGeneration, TargetGeneration         uint64
	Progress                                   Progress
	LastSuccess                                time.Time
	Pending, Warnings                          []string
	PhaseTimings                               map[string]time.Duration
}

type SyncResult struct {
	Generation uint64
	Changed, Deleted, Skipped, Failed int
	Pending, Warnings []string
}

type ModelHealth struct {
	GenerationAvailable, EmbeddingAvailable bool
	Warnings                                []string
}

type ProgressSink func(context.Context, Progress)
```

Define `RetrievalResult`, `Evidence`, `ContextResult`, and `ImpactResult` as data-only structs. `Evidence` has both code and documentation fields plus a `Type` discriminator. `ContextResult` has `Summary`, `Evidence`, `Warnings`, `Freshness`, `Timing`, and `Budget` fields matching the design JSON contract. `RetrievalResult` contains ranked `Candidates`, warnings, generation, and indexing/retrieval timing. `ImpactResult` contains the resolved origin, sorted graph matches, warnings, generation, and timing. Give `Progress` a `Message() string` method that formats phase, elapsed time, rates, and ETA without evidence text.

- [ ] **Step 5: Implement configuration precedence and validation**

```go
type Config struct {
	ProjectRoot string
	ClickHouse  ClickHouse
	Ollama      Ollama
	Index       Index
	Retrieval   Retrieval
}

type Ollama struct {
	BaseURL, BearerToken, GenerationModel, EmbeddingModel string
	ContextTokens, SynthesisInputTokens, MaxOutputTokens  int
	EmbeddingDimensions, EmbeddingBatch                   int
}

type Index struct {
	MaxFileBytes    int64
	IncludeTests    bool
	ExcludePaths    []string
	ExcludeDirNames []string
	ExcludeFileNames []string
}

func (c Config) Validate() error {
	if !filepath.IsAbs(c.ProjectRoot) {
		return errors.New("project root must be absolute")
	}
	if c.Retrieval.MaxTokens < 256 || c.Retrieval.MaxTokens > 12000 {
		return errors.New("retrieval max_tokens must be between 256 and 12000")
	}
	if c.Retrieval.GraphDepth < 1 || c.Retrieval.GraphDepth > 2 {
		return errors.New("graph depth must be 1 or 2")
	}
	if c.Ollama.EmbeddingDimensions != 1024 {
		return errors.New("embedding dimensions must be 1024")
	}
	return nil
}
```

Parse flags with `flag.FlagSet`, YAML with `yaml.Decoder.KnownFields(true)`, and environment keys `CODEWEFT_CLICKHOUSE_DSN`, `CODEWEFT_CLICKHOUSE_USER`, `CODEWEFT_CLICKHOUSE_PASSWORD`, `CODEWEFT_OLLAMA_URL`, and `CODEWEFT_OLLAMA_TOKEN`. Never print secret values.

Support `--config <path>`, resolving a relative path from the current working directory. Without it, load `<project>/.codeweft.yaml` when present. The checked-in example is:

```yaml
index:
  max_file_bytes: 2097152
  include_tests: true
  exclude_paths: []
  exclude_dir_names: []
  exclude_file_names: []
retrieval:
  max_tokens: 3500
  graph_depth: 2
```

Add these repository-local ignores:

```gitignore
.env
codeweft
coverage.out
*.test
```

- [ ] **Step 6: Add a compiling command shell**

`main` must call a temporary `run(context.Context, []string) error` dispatcher that supports `help` and returns a clear error for commands whose service is not wired yet. This shell is replaced in Task 12, not expanded into a command framework.

- [ ] **Step 7: Verify Task 1**

Run: `gofmt -w cmd internal && go test ./internal/config ./internal/core && go vet ./internal/config ./internal/core && git diff --check`

Expected: PASS; only configuration and shared-domain behavior are present.

---

### Task 2: Create the ClickHouse Schema and Current-Row Store

**Files:**

- Create: `internal/store/migrations/001_init.sql`
- Create: `internal/store/migrations.go`
- Create: `internal/store/store.go`
- Create: `internal/store/search.go`
- Create: `internal/store/store_test.go`
- Create: `internal/store/integration_test.go`

**Interfaces:**

- Consumes: `core.FileState`, `core.IndexedFile`, `core.CodeUnit`, `core.DocChunk`.
- Produces: `store.Store` with `Migrate`, `LoadManifest`, `NextGeneration`, `WriteDerived`, `ActivateFile`, `LoadGraph`, `SearchCode`, `SearchDocsFTS`, `SearchDocsVector`, run-history, `CleanupObsolete`, and `Purge` methods.

- [ ] **Step 1: Write store contract tests against a fake driver boundary**

Test that activation happens after derived writes, manifest loading selects `argMax` values without `FINAL`, search candidates require an active matching hash, and purge is scoped by `project_id`.

```go
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
```

- [ ] **Step 2: Run the unit test and verify failure**

Run: `go test ./internal/store -run 'TestActivateFileIsSeparateFromDerivedWrite|TestCurrentQueriesFilterActiveHash'`

Expected: FAIL because the store does not exist.

- [ ] **Step 3: Write the embedded migration**

Create the database externally through Compose, then create these tables in the configured database:

```sql
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
```

Do not add an HNSW index initially. Exact `cosineDistance` over the documentation candidate set is sufficient until the benchmark proves otherwise.

- [ ] **Step 4: Implement the store with one concrete ClickHouse client**

```go
type Store struct {
	conn clickhouse.Conn
	db   string
}

func (s *Store) WriteDerived(ctx context.Context, f core.IndexedFile) error
func (s *Store) ActivateFile(ctx context.Context, f core.FileState) error
func (s *Store) LoadManifest(ctx context.Context, projectID string) (map[string]core.FileState, error)
func (s *Store) NextGeneration(ctx context.Context, projectID string) (uint64, error)
func (s *Store) SearchCode(ctx context.Context, projectID, query string, paths []string, limit int) ([]core.Candidate, error)
func (s *Store) SearchDocsFTS(ctx context.Context, projectID, query string, paths []string, limit int) ([]core.Candidate, error)
func (s *Store) SearchDocsVector(ctx context.Context, projectID string, vector []float32, paths []string, limit int) ([]core.Candidate, error)
func (s *Store) CleanupObsolete(ctx context.Context, projectID string) error
```

Every search query must construct an `active_files` subquery using `argMax(tuple(file_hash, deleted, generation), generation)` grouped by `(project_id, path)`, then join on path and file hash. FTS filters use `hasAnyTokens(search_text, ?)` so the ClickHouse text index performs candidate selection. Return at most 50 rows per list; compute lexical ordering in Go from matched normalized query-token count, exact phrase presence, and source weight. Parameterize all user values; construct path predicates from fixed SQL fragments and bound arguments.

`CleanupObsolete` issues scoped ClickHouse delete mutations for derived rows whose `(path, file_hash)` pair is absent from the current active, non-deleted manifest. It runs only after a successful full refresh and does not block retrieval correctness. `Purge` removes all five tables' rows for exactly one bound `project_id`.

- [ ] **Step 5: Add opt-in integration tests against ClickHouse**

Guard the test with `CODEWEFT_TEST_CLICKHOUSE_DSN`; skip when unset. Insert old and new hashes for one path, activate only the new hash, and assert all three search methods hide old rows. Insert a tombstone and assert both hashes disappear.

Run: `CODEWEFT_TEST_CLICKHOUSE_DSN=clickhouse://localhost:9000/codeweft_test go test -tags=integration ./internal/store`

Expected: PASS against ClickHouse 26.8.1.2041.

- [ ] **Step 6: Verify Task 2**

Run: `gofmt -w internal/store && go test ./internal/store && go vet ./internal/store && git diff --check`

Expected: PASS; storage correctness does not depend on background merges.

---

### Task 3: Implement Safe Discovery and Git-Assisted Freshness Planning

**Files:**

- Create: `internal/project/discover.go`
- Create: `internal/project/discover_test.go`
- Create: `internal/project/git.go`
- Create: `internal/project/git_test.go`
- Create: `internal/project/freshness.go`
- Create: `internal/project/freshness_test.go`

**Interfaces:**

- Consumes: active `map[string]core.FileState` from the store.
- Produces: `project.Discover(ctx, root string) ([]project.File, error)` and `project.Plan(ctx, root, recordedHead string, manifest map[string]core.FileState) (project.ChangePlan, error)`.
- Produces: `ChangePlan{Changed, Deleted, Renames, Head, DirtyPaths, UsedGit, Warnings}`.

- [ ] **Step 1: Write discovery and porcelain fixtures**

Cover tracked, untracked, ignored, renamed, deleted, spaces, non-ASCII names, multiple NUL-delimited records, symlink escape, generated Go, `.env`, private keys, binary NUL bytes, unsupported extensions, and a metadata change with identical SHA-256.

```go
func TestParsePorcelainV2Rename(t *testing.T) {
	in := []byte("2 R. N... 100644 100644 100644 abc def R100 new name.ts\x00old name.ts\x00")
	got, err := parsePorcelainV2(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Renames) != 1 || got.Renames[0].Old != "old name.ts" || got.Renames[0].New != "new name.ts" {
		t.Fatalf("unexpected rename: %+v", got.Renames)
	}
}

func TestResolveInsideRootRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveInsideRoot(root, "escape/secret.md"); err == nil {
		t.Fatal("expected symlink escape to fail")
	}
}
```

- [ ] **Step 2: Run the tests and verify failure**

Run: `go test ./internal/project`

Expected: FAIL because discovery and planning functions do not exist.

- [ ] **Step 3: Implement source classification and exclusions**

Use a fixed extension map and fixed path/name exclusions. Secret-name exclusions are `.env`, `.env.*`, `id_rsa`, `id_ed25519`, `credentials`, `credentials.*`, `*.pem`, `*.key`, and `*.p12`. Apply configured `exclude_paths` as exact paths or directory prefixes, `exclude_dir_names` to any path segment, and `exclude_file_names` to exact basenames. Skip common test filenames when `include_tests` is false. Limit source files to 2 MiB by default, skip files containing a NUL byte in the first 8 KiB, detect generated Go through `^// Code generated .* DO NOT EDIT\.$`, and return a reason for every skip. Use `filepath.EvalSymlinks` and `filepath.Rel` to enforce the root.

Initial Git discovery uses:

```text
git -C <root> ls-files -co --exclude-standard -z
```

Non-Git discovery uses `filepath.WalkDir` with the same exclusion function. Never invoke a shell.

- [ ] **Step 4: Implement NUL-safe Git candidate inspection**

Use `exec.CommandContext` with argument arrays for:

```text
git -C <root> status --porcelain=v2 -z --untracked-files=all
git -C <root> rev-parse HEAD
git -C <root> diff --name-status -z <recorded-head> <current-head> --
```

Treat Git failures as warnings and switch to the filesystem path. Record dirty paths after every successful sync so a file becoming clean is still reconsidered once.

- [ ] **Step 5: Implement manifest comparison**

```go
type ChangePlan struct {
	Changed    []File
	Deleted    []string
	Renames    []Rename
	Head       string
	DirtyPaths []string
	UsedGit    bool
	Warnings   []string
}

type File struct {
	Path, Kind, Language, Extension, Hash string
	Size, MTimeNS                         int64
}

type Rename struct {
	Old, New string
}

func changed(meta File, old core.FileState) bool {
	return meta.Size != old.Size || meta.MTimeNS != old.MTimeNS || old.ParserVersion != ParserVersion
}
```

Hash only candidate files whose metadata, Git state, parser version, or prior dirty state requires it. Remove a candidate from `Changed` when its SHA-256 equals the active hash and only metadata changed. Sort all outputs by project-relative slash path for reproducibility.

- [ ] **Step 6: Verify Task 3**

Run: `gofmt -w internal/project && go test ./internal/project && go vet ./internal/project && git diff --check`

Expected: PASS on Git and non-Git fixtures without reading outside the root.

---

### Task 4: Add the Serialized Native Ollama Client

**Files:**

- Create: `internal/ollama/client.go`
- Create: `internal/ollama/client_test.go`

**Interfaces:**

- Consumes: `config.Ollama`.
- Produces: `Embed(ctx context.Context, texts []string) ([][]float32, error)`.
- Produces: `Generate(ctx context.Context, req core.GenerateRequest) (string, error)`.
- Produces: `Health(ctx context.Context) core.ModelHealth`.

- [ ] **Step 1: Write deterministic HTTP tests**

Use `httptest.Server` to assert `/api/embed`, `/api/generate`, bearer-token handling, `think:false`, `stream:false`, model names, timeouts, 1,024-dimension validation, response-body limits, and serialization of one embedding and one generation call.

```go
func TestGenerateDisablesThinkingAndStreaming(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(w).Encode(map[string]any{"response": "{\"summary\":\"ok\",\"citations\":[\"C1\"]}"})
	}))
	defer srv.Close()
	c := New(testConfig(srv.URL), srv.Client())
	if _, err := c.Generate(context.Background(), core.GenerateRequest{Prompt: "question"}); err != nil {
		t.Fatal(err)
	}
	if body["think"] != false || body["stream"] != false {
		t.Fatalf("unsafe generation options: %#v", body)
	}
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `go test ./internal/ollama`

Expected: FAIL because the client does not exist.

- [ ] **Step 3: Implement one shared semaphore and bounded JSON I/O**

```go
type Client struct {
	baseURL string
	token   string
	http    *http.Client
	sem     chan struct{}
	cfg     config.Ollama
}

func (c *Client) locked(ctx context.Context, fn func() error) error {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
		return fn()
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

Normalize the configured base URL by removing a trailing `/v1` because native endpoints are used. Limit response bodies to 16 MiB. Embed in batches of at most 16. Reject a vector whose dimension differs from 1,024. Generation sets `num_ctx: 65536`, `num_predict: 900`, `think:false`, and JSON format when a schema is supplied.

- [ ] **Step 4: Verify Task 4**

Run: `gofmt -w internal/ollama && go test -race ./internal/ollama && go vet ./internal/ollama && git diff --check`

Expected: PASS; concurrent calls never overlap at the fake server.

---

### Task 5: Parse Markdown and Obsidian into Stable Chunks

**Files:**

- Create: `internal/markdown/parser.go`
- Create: `internal/markdown/parser_test.go`
- Create: `testdata/obsidian/README.md`
- Create: `testdata/obsidian/architecture/services.md`
- Create: `testdata/obsidian/architecture/database.md`

**Interfaces:**

- Consumes: project-relative path, current bytes, and SHA-256 file hash.
- Produces: `markdown.Parse(path string, data []byte, fileHash string) ([]core.DocChunk, []string, error)`.

- [ ] **Step 1: Write chunking tests with exact expected lines and hashes**

Cover frontmatter, aliases, tags, ATX headings, setext headings, wiki-links, fenced-code headings, an oversized section, an unchanged paragraph after another section changes, and an empty document.

```go
func TestParsePreservesHeadingAndQuoteLines(t *testing.T) {
	data := []byte("---\ntags: [api]\n---\n# API\nIntro.\n\n## Create\nUse `POST /customers`.\n")
	chunks, warnings, err := Parse("docs/api.md", data, strings.Repeat("a", 64))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("parse failed: %v %v", err, warnings)
	}
	last := chunks[len(chunks)-1]
	if last.Heading != "API > Create" || last.StartLine != 8 || last.EndLine != 8 {
		t.Fatalf("unexpected chunk: %+v", last)
	}
	if last.Extension != ".md" || !slices.Contains(last.Tags, "api") {
		t.Fatalf("metadata missing: %+v", last)
	}
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `go test ./internal/markdown`

Expected: FAIL because `Parse` does not exist.

- [ ] **Step 3: Implement a line-oriented parser using stdlib plus YAML v3**

Parse only YAML frontmatter at the beginning of the file. Treat fenced code blocks as opaque when scanning headings and wiki-links. Build heading ancestry with a six-level stack. Extract `[[target]]` and `[[target|label]]` links and normalize tags without resolving them to filesystem paths.

```go
func chunkID(path, heading string, start, end uint32, content string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%d\x00%d\x00%s", path, heading, start, end, content)
	return hex.EncodeToString(h.Sum(nil))
}
```

Keep normal sections intact up to 1,200 estimated tokens. Split oversized sections at paragraph boundaries into at most 900-token pieces with one-paragraph overlap. Use the deterministic estimate `(utf8.RuneCountInString(text)+3)/4`; the same estimator is used by retrieval budgets.

- [ ] **Step 4: Verify Task 5**

Run: `gofmt -w internal/markdown && go test ./internal/markdown && go vet ./internal/markdown && git diff --check`

Expected: PASS; changing one section keeps unrelated chunk hashes stable.

---

### Task 6: Build the Go Structural Indexer

**Files:**

- Create: `internal/goparser/parser.go`
- Create: `internal/goparser/parser_test.go`
- Create: `testdata/go-multimodule/go.work`
- Create: `testdata/go-multimodule/service/go.mod`
- Create: `testdata/go-multimodule/service/customer/customer.go`
- Create: `testdata/go-multimodule/service/customer/customer_test.go`
- Create: `testdata/go-multimodule/shared/go.mod`
- Create: `testdata/go-multimodule/shared/model/model.go`

**Interfaces:**

- Consumes: root, requested changed Go paths, generation, active file hashes, and project source policy.
- Produces: `(*goparser.Parser).Parse(ctx context.Context, req goparser.Request) (goparser.Result, error)` containing `[]core.IndexedFile`, package imports, reverse-package metadata, and warnings.

- [ ] **Step 1: Write fixture assertions for stable IDs and graph relations**

The fixture must contain two modules, a shared interface, a struct implementing it, an embedded type, one cross-package call, one method call, and a test file. Assert package/file/type/interface/function/method nodes and `contains`, `imports`, `calls`, `implements`, and `embeds` edges. Assert IDs do not change after blank lines are inserted.

```go
func TestParseBuildsImplementsAndCalls(t *testing.T) {
	root := fixtureRoot(t, "go-multimodule")
	result, err := New().Parse(context.Background(), Request{Root: root, Patterns: []string{"./..."}, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	assertEdge(t, result.Files, "implements", "customer.Service", "model.Creator")
	assertEdge(t, result.Files, "calls", "customer.Service.Create", "model.NewCustomer")
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `go test ./internal/goparser`

Expected: FAIL because the parser does not exist.

- [ ] **Step 3: Load packages offline without executing project code**

Define the request and result at the package boundary:

```go
type Request struct {
	Root       string
	Patterns   []string
	Generation uint64
	FileHashes map[string]string
}

type Result struct {
	Files          []core.IndexedFile
	PackageImports map[string][]string
	FilePackages   map[string]string
	Warnings       []string
}
```

```go
cfg := &packages.Config{
	Context: ctx,
	Dir:     req.Root,
	Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
		packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo |
		packages.NeedImports | packages.NeedDeps | packages.NeedModule,
	Env: append(os.Environ(), "GOPROXY=off"),
}
pkgs, err := packages.Load(cfg, req.Patterns...)
```

Do not invoke generators, tests, builds, or package downloads. Convert package load errors into warnings when usable syntax exists; return an error only when no requested package can be analyzed.

- [ ] **Step 4: Extract declarations, types, imports, embeddings, and interface satisfaction**

Use `go/ast` and `go/types` for declarations and identity. Stable IDs use module path, package path, kind, receiver, and qualified name; never line numbers. Build searchable source from the exact declaration range. Weight production code `1.0`, tests `0.6`, and generated files `0.0` so excluded generated files cannot enter output.

- [ ] **Step 5: Add SSA and conservative call edges**

Build SSA for successfully typed packages, then use `golang.org/x/tools/go/callgraph/vta` for reachable calls. Keep only callsites whose source file is under the project root. Unresolved or external targets receive terminal IDs with `resolution="external"`; their source is absent.

- [ ] **Step 6: Implement changed-package selection**

Map every Go file to its package. A delta request provides package patterns for the changed package plus reverse dependents supplied by Task 8. Changes to `go.mod`, `go.sum`, or `go.work` select all packages in that resolution scope. Multiple modules are loaded independently when `go.work` does not cover them.

- [ ] **Step 7: Verify Task 6**

Run: `gofmt -w internal/goparser testdata/go-multimodule && go test ./internal/goparser && go vet ./internal/goparser && git diff --check`

Expected: PASS with `GOPROXY=off`; stable IDs survive line movement.

---

### Task 7: Build the JavaScript and TypeScript Structural Indexer

**Files:**

- Create: `internal/tsparser/parser.go`
- Create: `internal/tsparser/parser_test.go`
- Create: `internal/tsparser/resolve.go`
- Create: `internal/tsparser/resolve_test.go`
- Create: `testdata/script-project/package.json`
- Create: `testdata/script-project/tsconfig.json`
- Create: `testdata/script-project/src/api.ts`
- Create: `testdata/script-project/src/service.tsx`
- Create: `testdata/script-project/src/model.js`
- Create: `testdata/script-project/src/legacy.cjs`
- Create: `testdata/script-project/src/esm.mjs`
- Create: `testdata/script-project/src/types.d.ts`

**Interfaces:**

- Consumes: root, changed script paths, generation, and file hashes.
- Produces: `(*tsparser.Parser).Parse(ctx context.Context, req tsparser.Request) (tsparser.Result, error)` containing `[]core.IndexedFile`, module imports, reverse imports, and warnings.

- [ ] **Step 1: Write parser and resolution tests**

Cover all supported extensions, relative imports, extension omission, directory index lookup, `require` with a literal, tsconfig aliases, exports, re-exports, classes, interfaces, type aliases, functions, methods, React-shaped components, `implements`, `extends`, direct identifier calls, member calls, cycles, external packages, and dynamic imports that stay unresolved.

```go
func TestResolveAliasAndIndex(t *testing.T) {
	r := newResolver(fixtureRoot(t, "script-project"))
	got, ok := r.Resolve("src/api.ts", "@app/service")
	if !ok || got != "src/service.tsx" {
		t.Fatalf("unexpected resolution: %q %v", got, ok)
	}
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `go test ./internal/tsparser`

Expected: FAIL because parser and resolver do not exist.

- [ ] **Step 3: Select tree-sitter languages by extension**

Define the parser boundary:

```go
type Request struct {
	Root       string
	Paths      []string
	Generation uint64
	FileHashes map[string]string
}

type Result struct {
	Files         []core.IndexedFile
	ModuleImports map[string][]string
	Warnings      []string
}
```

```go
func languageFor(ext string) *tree_sitter.Language {
	switch ext {
	case ".tsx":
		return tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTSX())
	case ".ts", ".d.ts":
		return tree_sitter.NewLanguage(tree_sitter_typescript.LanguageTypescript())
	case ".js", ".jsx", ".mjs", ".cjs":
		return tree_sitter.NewLanguage(tree_sitter_javascript.Language())
	default:
		return nil
	}
}
```

Use `Parser.Parse` with request cancellation checked between files. Record tree-sitter error nodes as warnings but still extract unambiguous declarations and lexical source.

- [ ] **Step 4: Extract nodes and conservative edges**

Walk named nodes once per file. Build stable IDs from relative path, kind, owner, and exported/local name. Emit only relations supported by syntax. A call target is resolved only for direct local identifiers, imported identifiers, `this.method`, and statically named module members. All other calls receive `resolution="unresolved"` and do not invent a target.

- [ ] **Step 5: Resolve local modules without Node.js**

Read only `compilerOptions.baseUrl` and `compilerOptions.paths` from `tsconfig.json` or `jsconfig.json`, plus `main`, `module`, `types`, and `exports` string targets from local `package.json`. Resolve candidates in this order: exact file, supported extension, directory package target, directory index. Never enter `node_modules`. Bare packages become external nodes.

- [ ] **Step 6: Apply source weights and reverse-import metadata**

Weight normal sources `1.0`, common test patterns `0.6`, and `.d.ts` `0.5`. Return import adjacency even when a file contains no declarations so Task 8 can invalidate reverse importers.

- [ ] **Step 7: Verify Task 7**

Run: `gofmt -w internal/tsparser && go test ./internal/tsparser && go vet ./internal/tsparser && git diff --check`

Expected: PASS; every supported extension is parsed and unresolved dynamic behavior is explicit.

---

### Task 8: Add the In-Memory Graph and Affected-Set Expansion

**Files:**

- Create: `internal/graph/graph.go`
- Create: `internal/graph/graph_test.go`
- Create: `internal/graph/affected.go`
- Create: `internal/graph/affected_test.go`

**Interfaces:**

- Consumes: current `[]core.CodeUnit`, `[]core.CodeEdge`, package imports, and module imports.
- Produces: `graph.New(units, edges) *graph.Graph`, `Neighbors(ids []string, direction string, depth int) []graph.Match`, and `Impact(symbolOrPath, direction string, depth int) core.ImpactResult`.
- Produces: `AffectedGo(changed []string, metadata ChangeMetadata) []string` and `AffectedScript(changed []string, metadata ChangeMetadata) []string`.

- [ ] **Step 1: Write deterministic traversal and invalidation tests**

Cover cycles, duplicate edges, missing targets, path lookup, symbol ambiguity, upstream/downstream/both, depth rejection above 2, Go exported-signature changes, Go body-only changes, TS export changes, TS body-only changes, and resolution-metadata changes.

```go
func TestNeighborsStopsAtDepthTwo(t *testing.T) {
	g := New(testUnits("A", "B", "C", "D"), testEdges("A", "B", "B", "C", "C", "D"))
	got := g.Neighbors([]string{"A"}, Downstream, 2)
	if ids := matchIDs(got); !slices.Equal(ids, []string{"B", "C"}) {
		t.Fatalf("unexpected traversal: %v", ids)
	}
}
```

- [ ] **Step 2: Run tests and verify failure**

Run: `go test ./internal/graph`

Expected: FAIL because graph functions do not exist.

- [ ] **Step 3: Implement adjacency maps and deterministic BFS**

```go
type Graph struct {
	unitsByID   map[string]core.CodeUnit
	idsByPath   map[string][]string
	idsByName   map[string][]string
	out         map[string][]core.CodeEdge
	in          map[string][]core.CodeEdge
}
```

Deduplicate edges by `(source, relation, target, path, start_line)`. Sort all adjacency lists and traversal results. BFS tracks minimum distance and rejects depth outside `1..2`.

- [ ] **Step 4: Implement bounded affected-set rules**

```go
type ChangeMetadata struct {
	GoFilePackage   map[string]string
	GoReverseImport map[string][]string
	ScriptReverseImport map[string][]string
	SurfaceChanged  map[string]bool
	ResolutionScope map[string][]string
}
```

For Go, body-only edits rebuild the owning package; exported declaration changes add all known reverse package dependents. For JS/TS, body-only edits rebuild the file; export/import surface changes add reverse importers. Resolution metadata changes rebuild the relevant module or workspace scope. When surface classification fails, choose the bounded reverse-dependent superset.

- [ ] **Step 5: Verify Task 8**

Run: `gofmt -w internal/graph && go test ./internal/graph && go vet ./internal/graph && git diff --check`

Expected: PASS; traversal and invalidation output is sorted and cycle-safe.

---

### Task 9: Coordinate Full and Delta Indexing with Progress and Activation

**Files:**

- Create: `internal/indexer/progress.go`
- Create: `internal/indexer/progress_test.go`
- Create: `internal/indexer/indexer.go`
- Create: `internal/indexer/indexer_test.go`
- Create: `internal/testutil/fake_store.go`

**Interfaces:**

- Consumes: project planner, parsers, graph invalidation, Ollama embedder, and store.
- Produces: `(*indexer.Indexer).Sync(ctx context.Context, mode indexer.Mode, sink core.ProgressSink) (core.SyncResult, error)`.
- Produces: `Status() core.IndexStatus`, `Manifest() map[string]core.FileState`, and `Graph() *graph.Graph`.

- [x] **Step 1: Write coordinator tests around fakes**

Cover phase order, shared in-flight sync, full versus delta, embedding only new chunk hashes, embedding batch size 16, changed-file restat race, tombstone activation, parser failure hiding old rows, abandoned derived rows, cancellation, and degraded embedding behavior.

```go
func TestFileChangedDuringParseIsNotActivated(t *testing.T) {
	deps := fakeDependenciesWithOneFile("a.md", "first")
	deps.markChangedDuringParse("a.md", "second")
	idx := New(deps.config())
	result, err := idx.Sync(context.Background(), Delta, nil)
	if err != nil {
		t.Fatal(err)
	}
	if deps.store.wasActivated("a.md", hash("first")) {
		t.Fatal("raced file hash was activated")
	}
	if !slices.Contains(result.Pending, "a.md") {
		t.Fatalf("changed file was not rescheduled: %+v", result)
	}
}
```

- [x] **Step 2: Run tests and verify failure**

Run: `go test ./internal/indexer`

Expected: FAIL because the coordinator does not exist.

- [x] **Step 3: Implement progress snapshots and ETA**

```go
type Tracker struct {
	mu      sync.RWMutex
	status  core.IndexStatus
	history map[string]float64
}

func estimateETA(completed, total uint64, currentRate, historicRate float64) time.Duration {
	rate := currentRate
	if rate == 0 {
		rate = historicRate
	}
	if rate <= 0 || total <= completed {
		return 0
	}
	return time.Duration(float64(total-completed)/rate*float64(time.Second))
}
```

Phases are exactly `scan`, `parse`, `graph`, `embed`, and `persist`. Emit immutable snapshots no more often than every 100 ms plus every phase transition. Persist run snapshots without questions or evidence content.

- [x] **Step 4: Implement one shared in-flight synchronization**

Guard a single `inflight` result with a mutex and completion channel. A full request may replace a queued delta but never interrupt an active activation. Waiting callers receive the same result and their contexts may stop waiting without cancelling work required by other callers.

- [x] **Step 5: Implement grouped parsing and per-file last-write activation**

Read and hash a stable input snapshot for the complete affected set. Parse Markdown per file, group Go inputs by affected package pattern, and pass script inputs as a batch to their parser. Convert every parser result back to `core.IndexedFile`, fill generation and hash fields, reuse stored embeddings by chunk hash, and embed remaining Markdown chunks in batches of 16. For each output file, call `WriteDerived`, re-stat and re-hash, then call `ActivateFile` only when the hash is still current. Activate deletes as tombstones. On parser failure, activate the current hash with no derived structural rows and a warning. When an affected Go package is rebuilt, include every supported file in that package in the activation set.

- [x] **Step 6: Rebuild in-memory state only from activated rows**

After all requested affected files settle, update the manifest and graph atomically under one lock. Persist the current Git HEAD and dirty path set with the successful run. A retrieval request succeeds only after its discovered affected set is active or returns a freshness error. Trigger `CleanupObsolete` after a successful full refresh; log cleanup failures as warnings because active-hash filtering already guarantees correctness.

- [x] **Step 7: Verify Task 9**

Run: `gofmt -w internal/indexer internal/testutil && go test -race ./internal/indexer && go vet ./internal/indexer && git diff --check`

Expected: PASS; race tests show no stale activation and concurrent callers share work.

---

### Task 10: Implement Deterministic Retrieval, Fusion, and Impact Analysis

**Files:**

- Create: `internal/retrieval/retrieval.go`
- Create: `internal/retrieval/retrieval_test.go`
- Create: `internal/retrieval/rank.go`
- Create: `internal/retrieval/rank_test.go`

**Interfaces:**

- Consumes: indexer freshness, ClickHouse search, embedder, and current graph.
- Produces: `(*retrieval.Service).Retrieve(ctx context.Context, req core.SearchRequest, sink core.ProgressSink) (core.RetrievalResult, error)`.
- Produces: `(*retrieval.Service).Impact(ctx context.Context, req core.ImpactRequest, sink core.ProgressSink) (core.ImpactResult, error)`.

- [x] **Step 1: Write ranking and budget tests**

Cover identifier extraction, exact path/symbol priority, RRF stability, code versus docs weighting, test and `.d.ts` penalties, documentation symbol boost, graph distances 1 and 2, file diversification, path scopes, vector failure fallback, 12,000-token candidate cap, and deterministic impact results without generator calls.

```go
func TestRRFKeepsExactSymbolFirst(t *testing.T) {
	lists := [][]core.Candidate{
		{{ID: "C1", Match: "exact_symbol"}, {ID: "C2"}},
		{{ID: "C2"}, {ID: "C1"}},
	}
	got := fuse(lists, 60)
	boostExact(got)
	if got[0].ID != "C1" {
		t.Fatalf("exact symbol lost priority: %+v", got)
	}
}
```

- [x] **Step 2: Run tests and verify failure**

Run: `go test ./internal/retrieval -run 'TestRRF|TestRetrieve|TestImpact'`

Expected: FAIL because retrieval does not exist.

- [x] **Step 3: Implement exact term extraction and parallel search**

Extract quoted paths, slash-containing paths, Go exported identifiers, dotted receivers, camel/snake identifiers, and Markdown heading phrases with deterministic regexes. Run code FTS and document FTS concurrently. Run document vector search only after a successful query embedding. Bound each list to 50 rows and cancel siblings when the request context ends.

- [x] **Step 4: Implement RRF, boosts, graph expansion, and diversification**

Use `score += 1/(60+rank)` per list. Add fixed boosts: exact path `+2.0`, exact symbol `+1.5`, direct graph edge `+1.0`, heading token match `+0.5`; multiply by stored source weight and by `0.75` at graph distance 2. Keep at most three primary candidates per file before filling unused capacity.

- [x] **Step 5: Implement budget accounting**

Use the same rune-based estimator as Markdown. Stop deterministic candidates at 12,000 estimated tokens. `max_tokens` defaults to 3,500 and is clamped to `256..12000`. Preserve complete candidates rather than slicing bytes.

- [x] **Step 6: Implement impact analysis as graph-only output**

Call delta sync, resolve a path or symbol, reject ambiguous symbols with sorted alternatives, traverse at depth `1..2`, and return nodes grouped by distance and relation. Do not call the embedder or generator.

- [x] **Step 7: Verify Task 10**

Run: `gofmt -w internal/retrieval && go test -race ./internal/retrieval && go vet ./internal/retrieval && git diff --check`

Expected: PASS; retrieval still succeeds when vector search returns an availability error.

---

### Task 11: Add Bounded Query Expansion, Synthesis, and Exact Evidence

**Files:**

- Create: `internal/retrieval/synthesis.go`
- Create: `internal/retrieval/synthesis_test.go`
- Create: `internal/retrieval/evidence.go`
- Create: `internal/retrieval/evidence_test.go`

**Interfaces:**

- Consumes: deterministic `core.RetrievalResult`, `core.Generator`, canonical root, and active manifest.
- Produces: `(*retrieval.Service).SearchContext(ctx context.Context, req core.SearchRequest, sink core.ProgressSink) (core.ContextResult, error)`.

- [x] **Step 1: Write adversarial citation and evidence tests**

Cover valid `[C1]` and `[D1]`, unknown IDs, duplicate IDs, inactive hash, changed file after retrieval, out-of-range lines, path escape, summary with no citations, generator outage, malformed JSON, and final budget truncation. Assert the generator can never supply final snippet or quote text.

```go
func TestEvidenceRejectsHashChangedAfterRetrieval(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "docs.md")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := core.Candidate{ID: "D1", Path: "docs.md", FileHash: hash("old\n"), StartLine: 1, EndLine: 1}
	if err := os.WriteFile(path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := extractEvidence(root, candidate); err == nil {
		t.Fatal("expected stale candidate rejection")
	}
}
```

- [x] **Step 2: Run tests and verify failure**

Run: `go test ./internal/retrieval -run 'TestEvidence|TestSynthesis|TestExpansion'`

Expected: FAIL because synthesis and evidence extraction do not exist.

- [x] **Step 3: Add one weak-recall expansion call**

Weak recall means no exact match and fewer than five candidates above the fixed RRF threshold `0.02`. Ask the generator for this strict shape:

```json
{"terms":["customer handler","create endpoint"]}
```

Accept at most six unique strings of 2 to 80 UTF-8 characters. Reject paths, shell syntax, control characters, and model prose. Rerun deterministic retrieval once; never recurse.

- [x] **Step 4: Add cited synthesis with a fixed schema**

Request only:

```json
{"summary":"The customer route is registered in [C1] and its contract is documented in [D1].","citations":["C1","D1"]}
```

The prompt states that only supplied IDs may be cited and caps input at 12,000 estimated tokens. Parse JSON strictly, intersect citations with supplied candidates, remove citation markers for rejected IDs, and fall back to an evidence list when no valid citation remains.

- [x] **Step 5: Extract exact current evidence from the filesystem**

Resolve every candidate under the canonical root, verify the current SHA-256 equals both candidate and active manifest hashes, then read complete requested lines. Code records populate `snippet`; Markdown records populate `quote`, `format:"markdown"`, heading, and `.md` extension. Normalize line endings to `\n` without altering other text.

- [x] **Step 6: Assemble the final context budget and timing**

Include summary first, then complete evidence by citation order and rank. If a record does not fit, skip it and set `budget.truncated=true`. Populate indexing, retrieval, generation, and total milliseconds plus freshness generation and warnings.

- [x] **Step 7: Verify Task 11**

Run: `gofmt -w internal/retrieval && go test -race ./internal/retrieval && go vet ./internal/retrieval && git diff --check`

Expected: PASS; stale evidence is rejected even when a file changes between retrieval and response assembly.

---

### Task 12: Expose MCP STDIO and the Complete CLI

**Files:**

- Create: `internal/app/app.go`
- Create: `internal/app/app_test.go`
- Create: `internal/mcpserver/server.go`
- Create: `internal/mcpserver/tools.go`
- Create: `internal/mcpserver/server_test.go`
- Modify: `cmd/codeweft/main.go`

**Interfaces:**

- Consumes: configuration, store, Ollama, indexer, retrieval, and benchmark services.
- Produces: `mcpserver.Run(ctx context.Context, services mcpserver.Services) error`.
- Produces commands `serve`, `index`, `search`, `status`, `benchmark`, and `purge --yes`.

- [x] **Step 1: Write MCP in-memory protocol tests**

Use `mcp.NewInMemoryTransports` to list and call exactly four tools. Assert inferred input schemas, structured output, progress forwarding, cancellation, validation errors, immediate server startup during background initial indexing, and no writes to stdout outside MCP frames.

```go
func TestServerRegistersFourTools(t *testing.T) {
	server := New(fakeServices())
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0.0.1"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		t.Fatal(err)
	}
	session, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if names := toolNames(tools.Tools); !slices.Equal(names, []string{"impact_analysis", "index_status", "refresh_index", "search_context"}) {
		t.Fatalf("unexpected tools: %v", names)
	}
}
```

- [x] **Step 2: Run MCP tests and verify failure**

Run: `go test ./internal/mcpserver ./internal/app`

Expected: FAIL because the server and application wiring do not exist.

- [x] **Step 3: Register typed tools with concise instructions**

```go
server := mcp.NewServer(
	&mcp.Implementation{Name: "codeweft", Version: version},
	&mcp.ServerOptions{Instructions: "Use search_context before broad repository scanning. Use impact_analysis before changing public symbols or shared modules. Inspect files directly when Codeweft reports incomplete evidence."},
)
mcp.AddTool(server, &mcp.Tool{Name: "search_context", Description: "Return current compact project context with exact cited evidence."}, searchHandler)
mcp.AddTool(server, &mcp.Tool{Name: "impact_analysis", Description: "Return deterministic upstream or downstream code impact."}, impactHandler)
mcp.AddTool(server, &mcp.Tool{Name: "refresh_index", Description: "Refresh the project index in delta or full mode."}, refreshHandler)
mcp.AddTool(server, &mcp.Tool{Name: "index_status", Description: "Return index state, progress, throughput, and ETA."}, statusHandler)
```

Input structs use JSON and `jsonschema` tags. `search_context` accepts required `question`, optional `paths`, and optional `max_tokens`. `impact_analysis` validates exactly one of `symbol` and `path`, direction, and depth. `refresh_index` accepts only `delta` or `full`.

- [x] **Step 4: Forward tracker snapshots through MCP progress**

```go
func progressSink(req *mcp.CallToolRequest) core.ProgressSink {
	token := req.Params.GetProgressToken()
	if token == nil {
		return nil
	}
	return func(ctx context.Context, p core.Progress) {
		_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: token,
			Progress:      float64(p.Completed),
			Total:         float64(p.Total),
			Message:       p.Message(),
		})
	}
}
```

The message includes phase, elapsed time, throughput, and ETA when known. Tool handlers return structured content and a short text serialization for clients that ignore structured output.

- [x] **Step 5: Wire application startup and background initial indexing**

`app.Open` canonicalizes root, derives `project_id` as SHA-256 of the canonical root, connects and migrates ClickHouse, loads manifest and graph, creates one Ollama client, and constructs services. `serve` starts MCP first and launches initial full indexing only when no compatible active generation exists.

- [x] **Step 6: Implement CLI commands with `flag.FlagSet`**

All commands require `--project`. `index --full` maps to full mode; otherwise delta. `search --question` prints indented JSON. `status` prints JSON. `benchmark --suite` invokes Task 13. `purge` refuses to run unless `--yes` is present and prints the exact scoped project identity before deletion. Logs use `slog` on stderr only.

- [x] **Step 7: Run SDK and subprocess tests**

Run: `go test -race ./internal/mcpserver ./internal/app ./cmd/codeweft && go build ./cmd/codeweft`

Expected: PASS and a successful binary build.

- [x] **Step 8: Verify Task 12**

Run: `gofmt -w cmd internal && go test -race ./... && go vet ./... && git diff --check`

Expected: PASS; the complete CLI and MCP process compile without a second command framework.

---

### Task 13: Add Deployment, Integration Guidance, Security Tests, and Benchmarks

**Files:**

- Create: `Dockerfile`
- Create: `compose.yaml`
- Create: `.dockerignore`
- Create: `.env.example`
- Create: `LICENSE`
- Modify: `.gitignore`
- Modify: `README.md`
- Create: `docs/integrations/codex.md`
- Create: `docs/integrations/claude.md`
- Create: `internal/benchmark/benchmark.go`
- Create: `internal/benchmark/benchmark_test.go`
- Create: `testdata/eval/queries.json`
- Create: `testdata/benchmark/crm-api.yaml`
- Create: `integration/e2e_test.go`

**Interfaces:**

- Consumes: the same application services used by CLI and MCP.
- Produces: `benchmark.Run(ctx context.Context, project string, suite benchmark.Suite) (benchmark.Report, error)` and JSON report output.
- Produces: reproducible local deployment and agent setup instructions.

- [ ] **Step 1: Write end-to-end freshness and degradation tests**

The integration suite creates a temporary Git project, indexes it against ClickHouse and a fake Ollama server, then edits, renames, deletes, and parse-breaks files between searches. Assert zero stale snippets or quotes, exact line evidence, embedding reuse, retrieval-only fallback, Markdown FTS fallback, status transitions, and root rejection.

```go
func TestChangedFileNeverReturnsOldEvidence(t *testing.T) {
	env := newE2E(t)
	env.write("docs/api.md", "# API\nUse POST /v1/customers.\n")
	env.fullIndex()
	env.write("docs/api.md", "# API\nUse POST /v2/customers.\n")
	result := env.search("customer endpoint")
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("/v1/customers")) || !bytes.Contains(encoded, []byte("/v2/customers")) {
		t.Fatalf("stale evidence: %s", encoded)
	}
}
```

- [ ] **Step 2: Run integration tests and verify failure**

Run: `CODEWEFT_TEST_CLICKHOUSE_DSN=clickhouse://localhost:9000/codeweft_test go test -tags=integration ./integration`

Expected: FAIL until deployment and test harness are present.

- [x] **Step 3: Create minimal container deployment**

Use a two-stage Go build and a non-root runtime image. Mount the target project read-only at `/project`. Compose defines only `clickhouse` and a `codeweft` CLI service; it does not start Ollama. Pin ClickHouse to `clickhouse/clickhouse-server:26.8.1.2041`. Add health checks and make Codeweft depend on healthy ClickHouse. Keep ClickHouse running with `docker compose up -d clickhouse`; MCP clients launch the STDIO process with `docker compose run --rm -T codeweft serve --project /project`. Do not run the MCP service detached because STDIO belongs to the client process.

Required environment example:

```dotenv
CODEWEFT_CLICKHOUSE_DSN=clickhouse://clickhouse:9000/codeweft
CODEWEFT_CLICKHOUSE_USER=default
CODEWEFT_CLICKHOUSE_PASSWORD=
CODEWEFT_OLLAMA_URL=http://host.docker.internal:11434
CODEWEFT_OLLAMA_TOKEN=
CODEWEFT_PROJECT_PATH=/absolute/path/to/project
```

Linux documentation explains adding `host-gateway` for `host.docker.internal`. Do not include the user's private LAN address or API key.

- [x] **Step 4: Implement the benchmark runner and report schema**

```go
type Report struct {
	Project             string             `json:"project"`
	Files               int                `json:"files"`
	Initial             RunMetrics         `json:"initial"`
	OneFileDelta        RunMetrics         `json:"one_file_delta"`
	AffectedPackageDelta RunMetrics        `json:"affected_package_delta"`
	WarmRetrievalP95MS  float64            `json:"warm_retrieval_p95_ms"`
	GenerationMS        []float64          `json:"generation_ms"`
	StaleEvidence       int                `json:"stale_evidence"`
	BudgetViolations    int                `json:"budget_violations"`
}
```

Run initial indexing once, execute at least 30 warm retrievals, and calculate p95 by sorting durations and selecting `ceil(0.95*n)-1`. Mutation benchmarks operate only on a copied temporary fixture. The real `crm-api` benchmark is strictly read-only and measures initial index and retrieval unless an explicit copy path is supplied.

- [x] **Step 5: Add the reviewed evaluation suite format**

```json
[
  {
    "question": "Where is the customer creation endpoint registered?",
    "expected_paths": ["internal"],
    "expected_symbols": ["Create"],
    "max_tokens": 3500
  }
]
```

The checked-in fixture query is neutral. After the first `crm-api` index, replace or extend the suite only with reviewed expected paths and symbols; do not persist proprietary source or quotations.

Use this external benchmark policy so the real repository remains unchanged:

```yaml
index:
  include_tests: false
  exclude_paths: ["docs"]
  exclude_dir_names: ["mocks"]
  exclude_file_names: ["README.md"]
```

- [x] **Step 6: Write concise English documentation**

README order: purpose, prerequisites, quick start, Ollama models, Docker Compose, native install, CLI, MCP tools, configuration, freshness guarantees, degraded behavior, supported files, exclusions, benchmarking, security, limitations, license. Include `ollama pull qwen3.6:35b-a3b-q4_K_M` and `ollama pull qwen3-embedding:0.6b`.

Codex example:

```toml
[mcp_servers.codeweft]
command = "codeweft"
args = ["serve", "--project", "/absolute/path/to/project"]
startup_timeout_sec = 10
tool_timeout_sec = 600
enabled = true
```

Claude example:

```json
{"mcpServers":{"codeweft":{"command":"codeweft","args":["serve","--project","/absolute/path/to/project"]}}}
```

Both integration guides include a short optional `AGENTS.md` or `CLAUDE.md` instruction: call `search_context` before broad scanning and `impact_analysis` before shared-symbol changes; inspect files directly when evidence is incomplete.

- [ ] **Step 7: Run the complete verification matrix**

Run:

```text
gofmt -w cmd internal integration
go test -race ./...
go vet ./...
go build ./cmd/codeweft
docker compose config
CODEWEFT_TEST_CLICKHOUSE_DSN=clickhouse://localhost:9000/codeweft_test go test -tags=integration ./internal/store ./integration
git diff --check
```

Expected: every command succeeds; the integration suite reports zero stale evidence.

- [ ] **Step 8: Run the real read-only benchmark and record results**

Run:

```text
go run ./cmd/codeweft benchmark --project /Users/ratiborshugaev/Desktop/GO/crm-api --config testdata/benchmark/crm-api.yaml --suite testdata/eval/queries.json
```

Expected acceptance gates:

- initial supported-scope index at or below 120 seconds;
- one-file copied-fixture delta at or below 5 seconds;
- affected-package copied-fixture delta at or below 30 seconds;
- warm retrieval without generation p95 below 500 ms;
- zero stale evidence and zero default-budget violations;
- phase, elapsed time, throughput, and ETA present when history is sufficient;
- generation latency recorded without a hard pass/fail threshold.

If a gate fails, preserve the JSON report and profile the failing phase before changing architecture. Do not add HNSW, a Node sidecar, code embeddings, extra model passes, or a watcher without a measured failure matching an upgrade trigger in the design.

---

## Final Spec Coverage Check

| Design requirement | Implemented by |
| --- | --- |
| Single project, config, defaults, English surface, MIT | Tasks 1 and 13 |
| ClickHouse durable current-hash model and retention | Tasks 2, 9, and 12 |
| Git plus manifest delta detection and race safety | Tasks 3 and 9 |
| Go graph and multiple modules | Tasks 6 and 8 |
| JS/TS/TSX/JSX/MJS/CJS/declaration graph | Tasks 7 and 8 |
| Markdown/Obsidian chunks, embeddings, quotations | Tasks 4, 5, 9, and 11 |
| Exact, FTS, vector, RRF, graph retrieval | Tasks 8 and 10 |
| Optional expansion, synthesis, citation validation | Task 11 |
| Four MCP tools, progress, Codex and Claude setup | Tasks 12 and 13 |
| Status, ETA, timing, throughput history | Tasks 9 and 12 |
| Degraded modes, privacy, root safety, no execution | Tasks 3, 4, 6, 9, 11, and 13 |
| CLI, deployment, tests, `crm-api` benchmark | Tasks 12 and 13 |

## Deferred Until an Approved Upgrade Trigger

- TypeScript compiler or language-server sidecar.
- `qwen3-embedding:4b`.
- Source-code embeddings.
- ClickHouse HNSW index.
- Streamable HTTP MCP.
- Second grounding model pass.
- Dedicated Codex or Claude skill.
- Filesystem watcher.

These omissions are intentional ponytail constraints: each has a deterministic MVP substitute and a benchmark-defined reason for reconsideration.
