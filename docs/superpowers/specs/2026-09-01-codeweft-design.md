# Codeweft MVP Design

- Status: Draft for final review
- Date: 2026-09-01
- Repository: `https://github.com/ratibordas/go-mcp-codeweft`
- Go module: `github.com/ratibordas/go-mcp-codeweft`
- Binary: `codeweft`

## 1. Summary

Codeweft is a local, single-project MCP server that gives coding agents compact, current project context without forcing a vendor model to scan the repository repeatedly. It builds a structural graph for Go and JavaScript/TypeScript source, creates embeddings for Markdown and Obsidian documentation, retrieves the most relevant evidence, and uses a local Ollama model to synthesize a cited answer.

The MVP favors a deterministic Go pipeline over a general agent framework. It uses the official MCP Go SDK, ClickHouse, direct Ollama HTTP calls, Go analysis packages, and tree-sitter. LangChain, LangGraph, code embeddings, file watchers, and remote MCP transport are not required for the first release.

## 2. Problem

Coding agents spend vendor-model tokens discovering repository structure, reading documentation, locating symbols, and tracing dependencies. The same discovery work is repeated after small edits, while a full reindex after every edit would be too expensive locally.

Codeweft must:

1. Maintain a current index while files are edited by Codex, Claude Code, the user, or another process.
2. Distinguish code structure from prose documentation.
3. Return a compact synthesis plus exact, verifiable evidence.
4. Expose progress and ETA when indexing delays a request.
5. Continue providing useful retrieval when optional local-model features are unavailable.

## 3. Goals

- Serve one explicitly configured project root per process.
- Integrate with Codex and Claude Code over MCP STDIO.
- Index Go, TypeScript, TSX, JavaScript, JSX, MJS, CJS, declaration files, and Markdown.
- Build symbol and dependency graphs for supported source code.
- Build full-text and vector indexes for Markdown, including Obsidian conventions.
- Detect and process only relevant changes before every context request.
- Never return evidence from an obsolete version of a changed file.
- Return indexing progress, elapsed time, throughput, and estimated remaining time.
- Keep the final context pack within a default estimated 3,500-token budget.
- Operate with a local Ollama generation model and embedding model.
- Provide a reproducible benchmark against a real Go backend and neutral JS/TS fixtures.

## 4. Non-goals

- Serving multiple project roots from one process.
- A web UI, authentication, multi-user tenancy, or hosted service.
- Streamable HTTP MCP transport.
- Executing project code, tests, package installation, or arbitrary shell commands.
- Indexing `node_modules`, vendored dependencies, generated output, binaries, source maps, SQL, MDX, Obsidian Canvas, or attachments.
- Modeling every variable reference or resolving every dynamic-language call.
- Embedding source-code chunks in the MVP.
- Replacing exact evidence with unverified model-generated text.
- A mandatory Codex or Claude skill; MCP instructions and small repository guidance snippets are sufficient.

## 5. Design principles

1. **Freshness before recall.** A smaller current result is better than a larger stale result.
2. **Deterministic retrieval before generation.** Exact identifiers, paths, full-text search, and graph traversal run before local-model query expansion.
3. **Models summarize; Codeweft proves.** Ollama may select and summarize evidence IDs, but Codeweft reads final snippets and quotations from current source files.
4. **Incremental by default.** Git narrows the candidate set; a persistent manifest establishes truth.
5. **Graceful degradation.** Full-text and graph retrieval remain useful without embeddings or synthesis.
6. **Few dependencies.** Native Go and platform capabilities are preferred over orchestration frameworks and sidecars.
7. **English project surface.** Documentation, configuration examples, errors, and user-facing text are written in English. Code comments are reserved for non-obvious constraints and invariants.

## 6. Runtime defaults

| Area | Default |
| --- | --- |
| Go | 1.27 |
| ClickHouse | 26.8.1.2041 |
| ClickHouse Go client | v2.48.0 |
| MCP Go SDK | v1.6.1 stable |
| MCP transport | STDIO |
| Generation model | `qwen3.6:35b-a3b-q4_K_M` |
| Generation context | 65,536 tokens |
| Internal synthesis input budget | 12,000 estimated tokens |
| Maximum synthesis output | 900 tokens |
| Ollama thinking | Disabled |
| Embedding model | `qwen3-embedding:0.6b` |
| Embedding dimensions | 1,024 |
| Embedding batch size | 16 chunks |
| Ollama concurrency | One serialized request |
| Returned context budget | 3,500 estimated tokens |
| Graph expansion depth | At most 2 |

The generation model is approximately 24 GB and therefore uses mixed GPU and system memory on the target 16 GB GPU. Serialization avoids competing allocations between generation and embedding workloads. The smaller embedding model is the default because retrieval quality can be measured and upgraded independently without increasing baseline memory pressure.

## 7. System architecture

```text
Codex / Claude Code
        |
        | MCP STDIO
        v
  MCP tool handlers
        |
        +---- freshness coordinator ---- Git + manifest + filesystem
        |              |
        |              v
        |       language indexers
        |       Go / JS-TS / Markdown
        |              |
        |              +---- Ollama embeddings
        |              v
        |          ClickHouse
        |
        +---- retrieval coordinator ---- exact + FTS + vectors + graph
                       |
                       +---- optional Ollama expansion and synthesis
                       v
              validated context pack
```

The process keeps the active manifest and adjacency maps in memory. ClickHouse is the durable search and run-history store. The filesystem remains the source of truth for evidence text.

## 8. Component boundaries

- `mcp`: server lifecycle, tool schemas, request cancellation, progress notifications, and response serialization.
- `indexer`: discovery, change planning, generation management, scheduling, throughput measurement, and activation.
- `goparser`: Go packages, types, declarations, SSA-derived calls, interfaces, embedded types, and package dependencies.
- `tsparser`: JavaScript/TypeScript syntax, declarations, imports, exports, inheritance, and conservative call edges.
- `markdown`: frontmatter, heading hierarchy, tags, wiki-links, chunk boundaries, and embedding requests.
- `store`: ClickHouse migrations, batches, queries, retention, and active-hash filtering.
- `graph`: in-memory nodes, adjacency lists, reverse dependencies, bounded traversals, and impact analysis.
- `ollama`: health checks, serialized generation and embedding calls, timeouts, and structured response validation.
- `retrieval`: query classification, parallel candidate retrieval, fusion, graph expansion, diversification, synthesis, citations, and evidence extraction.
- `config`: CLI flags, `.codeweft.yaml`, environment variables, validation, and safe defaults.

Packages depend inward on small domain types and interfaces. There is no generic workflow engine or repository-wide dependency-injection framework.

## 9. Process lifecycle

1. `codeweft serve --project <absolute-path>` validates and canonicalizes the root.
2. It connects to ClickHouse, applies compatible migrations, loads the latest active manifest, and starts MCP immediately.
3. It launches initial indexing in the background if no compatible completed generation exists.
4. Tool calls unrelated to indexed content, such as `index_status`, remain available during initial indexing.
5. `search_context` and `impact_analysis` wait for required index work, emitting MCP progress notifications when the client supplied a progress token.
6. Shutdown cancels queued work, finishes or abandons the current unactivated batch, and closes resources.

An abandoned batch is harmless because only activated file hashes are visible to retrieval.

## 10. Configuration and deployment

The default deployment uses Docker Compose for Codeweft and ClickHouse. Ollama runs outside the Compose stack and is reached through a configured URL. A native `codeweft` binary may instead connect to the same services.

Configuration precedence, from highest to lowest, is:

1. CLI flags.
2. Environment variables and optional `.env` loading by the launcher.
3. `<project>/.codeweft.yaml`.
4. Built-in defaults.

The project root comes from `--project` and cannot be changed by MCP input. URLs, credentials, and tokens belong in environment variables. `.codeweft.yaml` contains only project-scoped indexing and retrieval policy, such as extra exclusions or budget overrides. The repository includes `.env.example`, never working credentials or a private network address.

Supported platforms are macOS and Linux on amd64 and arm64. Windows is supported through WSL or Docker in the MVP.

The project is distributed under the MIT License.

## 11. Source discovery and policy

### 11.1 Supported inputs

| Kind | Extensions or files | Treatment |
| --- | --- | --- |
| Go | `.go` | Structural graph and full-text source |
| TypeScript | `.ts`, `.tsx`, `.d.ts` | Structural graph and full-text source |
| JavaScript | `.js`, `.jsx`, `.mjs`, `.cjs` | Structural graph and full-text source |
| Documentation | `.md` | Heading-aware chunks, full-text search, embeddings |
| Resolution metadata | `tsconfig.json`, `jsconfig.json`, `package.json` | Module-resolution input, not evidence documents |

Markdown is indexed wherever it appears under the root, including README files, ADRs, codebase documentation, and Obsidian vaults.

### 11.2 Default exclusions

Codeweft excludes:

- `.git`, `node_modules`, `vendor`, cache directories, IDE metadata, build output, coverage output, minified bundles, and source maps.
- Binary files, oversized files, secrets, credential files, private keys, and environment files.
- Generated Go files detected by standard generated-code headers.
- Paths ignored by Git, unless explicitly included by project configuration.

Tests receive a lower retrieval weight in the general policy. `.d.ts` files also receive a lower weight. Repository-local instructions may tighten the policy. The `crm-api` benchmark excludes tests, mocks, generated Swagger artifacts, and documentation in accordance with that repository's agent instructions; neutral Markdown fixtures test the documentation pipeline separately.

### 11.3 Root safety

Every discovered and requested path is cleaned, resolved, and checked against the canonical root. Symlinks resolving outside the root are skipped. Tool input cannot override exclusions or request arbitrary filesystem reads.

## 12. Incremental freshness

### 12.1 Persistent manifest

The active manifest stores, for each path:

- size and nanosecond modification time;
- SHA-256 content hash;
- source kind and language;
- parser and schema version;
- active generation;
- hashes of derived chunks or units;
- deletion state.

Metadata is a cheap change signal. Content hashes are authoritative.

### 12.2 Candidate discovery

Before every retrieval or explicit delta refresh, the coordinator builds a candidate set from:

1. `git status --porcelain=v2 -z`, including untracked files and renames.
2. The name-only diff between the manifest's recorded HEAD and the current HEAD.
3. Paths that were dirty during the previous successful sync.
4. Previously known paths whose parent directories or resolution metadata changed.
5. A cheap metadata walk when Git is unavailable, reports an ambiguous state, or cannot account for the manifest.

Git is only a fast candidate source. It is never the freshness authority. Candidate metadata is compared with the manifest and content is hashed only when metadata or Git state indicates possible change. Deletes and renames are represented explicitly.

### 12.3 Affected-set expansion

- Markdown updates only the changed document's chunks and wiki-link edges.
- Go changes reindex the changed package and known reverse-dependent packages when exported API, package membership, or dependency information may have changed.
- JavaScript/TypeScript changes reindex the changed module and known reverse-import dependents when exports or resolution metadata may have changed.
- A content-only function-body edit may update the file and local call edges without rebuilding unrelated dependents.
- Changes to `go.mod`, `go.work`, `tsconfig.json`, `jsconfig.json`, or relevant `package.json` fields invalidate the corresponding resolution scope.

The planner errs toward a bounded superset when it cannot prove that a dependent is unaffected.

### 12.4 Activation and race handling

Each sync obtains a monotonically increasing generation ID and records the input hash for every affected file. Derived rows are written first. The active `files` row is written last as that file's commit marker.

Immediately before activation, Codeweft re-stats each affected path. If size or modification time changed during processing, it re-hashes the path. A different hash prevents activation and schedules the file again. Deleted files are activated as tombstones. Retrieval accepts a derived row only when its file hash equals the active, non-deleted file hash.

This per-file activation avoids a long global transaction while guaranteeing zero stale evidence for changed files. A request waits until its discovered affected set is activated or fails. Concurrent requests share the same in-flight sync instead of starting duplicate indexing.

### 12.5 Parser failure policy

If a changed file cannot be parsed, its previous derived rows become invisible once the new file state is activated. Recoverable lexical data may be indexed with a warning; otherwise the file contributes no structural evidence until a successful refresh. Codeweft never silently serves the old representation of the changed file.

## 13. Go graph

Go indexing uses `golang.org/x/tools/go/packages`, `go/types`, SSA, and conservative call-graph analysis.

Node kinds:

- package;
- file;
- named type and interface;
- function;
- method.

Edge kinds:

- `contains`;
- `imports`;
- `calls`;
- `implements`;
- `embeds`.

Stable symbol IDs are derived from module-aware package identity, relative path where required, kind, receiver, and qualified name. Line numbers are attributes, not identity. Multiple `go.mod` files and Go workspaces are supported. Dependency loading runs with `GOPROXY=off`; missing cached dependencies produce partial graphs and warnings rather than network access.

External dependencies may appear as terminal nodes, but their source is not indexed. The MVP does not execute generators, build scripts, tests, or project binaries.

## 14. JavaScript and TypeScript graph

Parsing uses the official Go tree-sitter binding and grammar packages. No Node.js or language-server sidecar is required.

Node kinds:

- module or file;
- class;
- interface;
- type alias;
- function;
- method;
- component when a function or class is syntactically identifiable as one.

Edge kinds:

- `contains`;
- `imports`;
- `exports`;
- `calls`;
- `implements`;
- `extends`.

Resolution supports relative imports, literal `require` calls, common index-file lookup, extensions in the supported set, and path aliases from `tsconfig.json` or `jsconfig.json`. Relevant `package.json` module fields inform local resolution. Bare packages become terminal external nodes and `node_modules` is not traversed.

Dynamic imports with non-literal expressions, runtime property dispatch, dependency-injection containers, re-export patterns that cannot be resolved syntactically, and generated module loaders may remain unresolved. Results expose warnings rather than inventing edges.

## 15. Markdown and Obsidian indexing

Markdown is split on heading structure while preserving useful surrounding context. Each chunk records:

- relative path and `.md` extension;
- YAML frontmatter fields allowed by configuration;
- heading ancestry;
- start and end lines;
- exact content;
- tags and aliases;
- outgoing Obsidian wiki-links;
- SHA-256 chunk hash;
- normalized embedding.

Chunking prefers complete sections, then paragraphs, then bounded text windows for oversized sections. A small overlap is used only when a section must be split. Unchanged chunk hashes reuse existing embeddings.

Wiki-links and exact code-symbol mentions provide deterministic boosts. They do not create a general-purpose semantic knowledge graph. Documentation evidence is returned as an exact quotation with heading and line range, not as a code-style snippet.

## 16. ClickHouse storage

The logical tables are:

### `files`

Path, kind, language, extension, size, modification time, SHA-256, parser version, generation, source weight, deletion flag, and activation time.

### `code_units`

Stable unit ID, qualified name, kind, language, extension, path, start and end lines, searchable source, file hash, generation, and source weight.

### `code_edges`

Source ID, target ID, relation, source path and location, resolution status, file hash, and generation.

### `doc_chunks`

Chunk ID, path, `.md` extension, heading path, start and end lines, content, searchable text, tags, links, chunk hash, file hash, generation, and `Array(Float32)` embedding with fixed dimension 1,024.

### `index_runs`

Run ID, mode, state, start and end times, phase timings, counters, throughput samples, ETA samples, warnings, errors, starting and resulting generations, and Git state when available.

Tables are append-oriented and versioned. ReplacingMergeTree-family engines support eventual physical compaction, but correctness never depends on merges or `FINAL`. Queries join or filter against the latest active file state and matching file hash. Full-text indexes cover code source and documentation text. A vector similarity index accelerates documentation retrieval when the ClickHouse version and data volume make it beneficial; exact distance calculation remains the correctness fallback.

The active manifest and graph are reconstructed from durable rows at startup and cached in memory. Old rows are logically hidden immediately and removed by retention cleanup or `purge`.

## 17. Retrieval pipeline

`search_context` performs these steps:

1. Synchronize all discovered changes required for a current answer.
2. Extract exact paths, filenames, identifiers, and symbol-shaped terms from the question.
3. Run exact lookups first.
4. In parallel, run code full-text search, documentation full-text search, and documentation vector search.
5. If deterministic recall is weak, ask the local generation model for a small structured set of alternate search terms, then rerun retrieval once.
6. Fuse ranked lists with reciprocal rank fusion.
7. Expand direct graph neighbors and then second-degree neighbors only when the remaining budget and relation type justify it.
8. Apply source weighting, exact-match boosts, heading boosts, graph-distance penalties, and per-file diversification.
9. Cap synthesis candidates at an estimated 12,000 tokens.
10. Ask Ollama for a concise answer containing only supplied evidence IDs.
11. Validate every cited ID and active file hash.
12. Read exact evidence from current files, discard invalid citations, and assemble a context pack within the requested budget, defaulting to 3,500 estimated tokens.

Ranking preference is generally: exact path or symbol, direct graph relationship, production code, documentation heading/full-text match, documentation vector similarity, test or declaration source. Query intent may adjust this order. Mixed code and documentation results are allowed.

If synthesis produces no valid citations, Codeweft returns a retrieval-only result with a warning. There is no second model-based grounding pass in the MVP.

## 18. Evidence contract

The response contains:

- `summary`: compact Markdown using citations such as `[C1]` and `[D1]`;
- `evidence`: current exact evidence records;
- `warnings`: partial-index, parser, resolution, or degraded-mode notices;
- `freshness`: generation and sync information;
- `timing`: indexing, retrieval, generation, and total durations;
- `budget`: requested, used, and truncated indicators.

Code evidence:

```json
{
  "id": "C1",
  "type": "code",
  "language": "go",
  "extension": ".go",
  "path": "internal/http/customer.go",
  "symbol": "CustomerHandler.Create",
  "relation": "exact_symbol",
  "start_line": 42,
  "end_line": 68,
  "snippet": "..."
}
```

Documentation evidence:

```json
{
  "id": "D1",
  "type": "documentation",
  "format": "markdown",
  "extension": ".md",
  "path": "docs/api.md",
  "heading": "Creating customers",
  "start_line": 17,
  "end_line": 25,
  "quote": "..."
}
```

The local model never supplies `snippet` or `quote`. Codeweft extracts those fields after citation validation. A result that cannot fit the budget preserves the most relevant complete evidence records and reports truncation.

## 19. MCP interface

### `search_context`

Inputs:

- `question` string, required;
- `paths` array of project-relative path prefixes, optional;
- `max_tokens` positive integer, optional.

Behavior: performs delta synchronization, retrieval, optional expansion, optional synthesis, and evidence validation.

### `impact_analysis`

Inputs:

- exactly one of `symbol` or `path`;
- `direction`: `upstream`, `downstream`, or `both`;
- `depth`: integer from 1 to 2.

Behavior: performs delta synchronization and deterministic graph traversal without an LLM call.

### `refresh_index`

Inputs:

- `mode`: `delta` or `full`.

Behavior: starts or joins the requested indexing operation and reports progress. Full refresh rebuilds all supported active inputs but retains run history until normal cleanup.

### `index_status`

Inputs: none.

Returns:

- state: `idle`, `indexing`, `ready`, or `degraded`;
- active and target generation;
- current phase;
- completed and total work;
- elapsed duration and ETA;
- changed, deleted, skipped, and failed counts;
- files and chunks per second;
- recent phase timings and throughput history;
- last successful completion;
- queued work and active warnings.

MCP server instructions are concise and self-contained: use `search_context` before broad repository scanning, use `impact_analysis` before changing public symbols or shared modules, and fall back to direct file inspection when Codeweft reports incomplete evidence.

## 20. Agent integration

Codex project configuration:

```toml
[mcp_servers.codeweft]
command = "codeweft"
args = ["serve", "--project", "/absolute/path/to/project"]
startup_timeout_sec = 10
tool_timeout_sec = 600
enabled = true
```

Claude Code project configuration:

```json
{
  "mcpServers": {
    "codeweft": {
      "command": "codeweft",
      "args": ["serve", "--project", "/absolute/path/to/project"]
    }
  }
}
```

The repository will include short optional English snippets for `AGENTS.md` and `CLAUDE.md`. A dedicated skill is deferred until real usage demonstrates instructions that MCP metadata and repository guidance cannot express reliably.

## 21. Progress, ETA, and performance measurement

Indexing phases are `scan`, `parse`, `graph`, `embed`, and `persist`. Each phase reports completed units, known total units, elapsed time, instantaneous or smoothed throughput, and ETA when enough observations exist.

ETA uses weighted historical throughput from successful runs, refined by current-run observations. Unknown totals are reported honestly during discovery. A phase transition may revise ETA. Progress is monotonic within a phase, but the overall total may grow when dependency expansion discovers more affected files.

Every `search_context` response reports:

- time waiting for freshness;
- retrieval time;
- local generation time;
- total time;
- whether query expansion or synthesis ran.

`index_runs` preserves enough history to compare cold and incremental throughput without recording user questions or generated summaries.

## 22. Failure and degraded modes

| Failure | Behavior |
| --- | --- |
| Generation model unavailable | Return ranked retrieval evidence without synthesis |
| Embedding model unavailable | Index and search Markdown with full-text only; queue missing embeddings for a future refresh |
| ClickHouse unavailable | Return a clear tool error; do not claim readiness |
| Changed source cannot be parsed | Hide stale structure for that file, return partial results and a warning |
| Git unavailable or repository not initialized | Use manifest plus metadata walk and hashes |
| External Go dependencies unavailable offline | Build a partial graph with unresolved external nodes |
| JS/TS construct cannot be resolved | Preserve lexical evidence and expose unresolved-edge warnings |
| Request cancelled | Stop optional work, leave unactivated rows invisible, and preserve reusable completed batches |

The process state is `degraded` when required storage is healthy but one or more optional capabilities are unavailable or the active generation has partial parser failures.

## 23. Security, privacy, and retention

- Codeweft binds no network listener in the MVP.
- It sends project content only to the configured ClickHouse and Ollama endpoints.
- It has no telemetry.
- It does not persist agent questions, synthesized answers, or MCP transcripts.
- It refuses paths outside the configured root.
- It excludes likely secrets and credential files by name and content-safe discovery policy.
- It never executes project code or automatically downloads dependencies.
- Ollama authentication may use an environment-provided bearer token.
- Logs avoid evidence content, tokens, secrets, and full user prompts.
- Replaced content is hidden immediately. Scheduled retention removes obsolete derived rows, and `purge --yes` removes all Codeweft data for the configured project identity.

## 24. CLI

```text
codeweft serve --project <path>
codeweft index --project <path> [--full]
codeweft search --project <path> --question <text> [--max-tokens N]
codeweft status --project <path>
codeweft benchmark --project <path> [--suite <path>]
codeweft purge --project <path> --yes
```

CLI search and status commands exercise the same application services as MCP handlers. This enables diagnostics and benchmarks without an MCP client.

## 25. Testing and evaluation

### 25.1 Automated tests

- Unit tests for manifest comparison, Git porcelain parsing, rename/delete handling, dependency invalidation, chunk stability, root enforcement, ranking, budget enforcement, ETA calculation, and citation validation.
- Parser fixtures for Go modules, multiple Go modules, interfaces, embedding, calls, TS/JS module formats, aliases, JSX/TSX, declaration files, Markdown headings, frontmatter, tags, and wiki-links.
- Store integration tests against the pinned ClickHouse version, including active-hash visibility and abandoned batches.
- Ollama client tests with a deterministic HTTP fake.
- MCP protocol tests over in-memory or subprocess STDIO.
- End-to-end tests that edit, rename, and delete files between queries and assert that no stale evidence is returned.

### 25.2 Real Go benchmark

`/Users/ratiborshugaev/Desktop/GO/crm-api` is a read-only benchmark input. It contains approximately 423 Go files and 144,000 Go lines across multiple modules. The benchmark follows its repository instructions and excludes tests, mocks, generated Swagger content, and repository documentation.

The benchmark records initial scan, parse, graph, persist, and total durations; file and unit throughput; ClickHouse storage size; warm retrieval latency; one-file delta latency; affected-package delta latency; and local generation latency.

### 25.3 JS/TS and documentation fixtures

A neutral fixture repository covers TS, TSX, JS, JSX, MJS, CJS, path aliases, re-exports, import cycles, and unresolved dynamic constructs. A neutral Markdown/Obsidian vault covers frontmatter, headings, tags, wiki-links, exact citations, renamed documents, and changed chunks.

### 25.4 Retrieval evaluation

After the first `crm-api` index, a small reviewed query set is derived from real routes, services, repositories, and cross-package changes. Each case records expected symbols or paths and checks citation freshness, evidence precision, budget compliance, and whether the answer is sufficient to begin a coding task without a broad repository scan.

### 25.5 MVP acceptance criteria

- Initial supported-scope index of `crm-api` completes within 2 minutes on the target machine.
- A one-file delta with no reverse dependents completes within 5 seconds.
- A larger affected-package refresh completes within 30 seconds.
- Warm retrieval without local generation has p95 latency below 500 ms.
- No changed, renamed, or deleted file produces stale evidence.
- Default context output stays within 3,500 estimated tokens.
- Progress includes phase, elapsed time, throughput, and ETA whenever sufficient history exists.
- Generation latency is measured and reported, but has no hard MVP SLA because the configured model partially offloads to system memory.

## 26. Dependencies

Direct production dependencies are limited to:

- the official MCP Go SDK;
- ClickHouse's official Go client;
- `golang.org/x/tools` for Go package, type, SSA, and call-graph analysis;
- the official tree-sitter Go binding and the JavaScript, TypeScript, and TSX grammars;
- a small YAML decoder for `.codeweft.yaml` and frontmatter;
- the Go standard library for HTTP, hashing, process interaction, concurrency, logging, and configuration plumbing.

Ollama integration uses its native HTTP API rather than an AI framework. All modules are pinned in `go.mod` and checksummed in `go.sum`. The runtime compatibility baseline is Go 1.27, ClickHouse 26.8.1.2041, `clickhouse-go` v2.48.0, and MCP Go SDK v1.6.1.

## 27. Risks and tradeoffs

- Tree-sitter cannot provide TypeScript checker-level semantic accuracy. The MVP accepts conservative unresolved edges; a TypeScript compiler or language-server sidecar is justified only if evaluation shows material retrieval failures.
- Local generation may be slow under mixed GPU and RAM offload. It is optional, serialized, bounded, and separately timed.
- ClickHouse full-text and vector indexes are version-sensitive. The pinned server version and integration tests control this risk, while exact scans remain correctness fallbacks for small candidate sets.
- Git status is fast but does not describe every filesystem state. The manifest and content hashes remain authoritative.
- Per-file activation can temporarily expose a mixture of old and new files during background indexing. Retrieval waits for the complete affected set discovered for its request, so a returned answer is internally current for that synchronization boundary.
- A strict 3,500-token output budget can omit useful context. Complete high-ranked evidence and explicit truncation are preferred over oversized responses.

## 28. Upgrade triggers

The following additions require measured evidence, not speculative abstraction:

- Add a TypeScript compiler or language-server sidecar if reviewed JS/TS queries fail because syntax-level edges are insufficient.
- Upgrade to `qwen3-embedding:4b` if retrieval evaluation shows a meaningful quality gain that justifies memory and latency cost.
- Add code embeddings if graph plus exact/full-text retrieval misses conceptual code queries.
- Add Streamable HTTP MCP when a real remote-client or multi-process use case exists.
- Add a second grounding pass if deterministic citation validation still permits materially misleading summaries.
- Add a Codex or Claude skill if MCP instructions and repository snippets produce unreliable tool adoption.
- Add file-system watching if pre-query synchronization causes unacceptable interactive latency.

## 29. External references

- [Model Context Protocol Go SDK](https://go.sdk.modelcontextprotocol.io/)
- [Codex MCP documentation](https://developers.openai.com/codex/mcp)
- [Claude Code MCP documentation](https://docs.anthropic.com/en/docs/claude-code/mcp)
- [Ollama API](https://docs.ollama.com/api/introduction)
- [Ollama embeddings API](https://docs.ollama.com/api/embed)
- [Qwen3.6 35B A3B Q4_K_M on Ollama](https://ollama.com/library/qwen3.6%3A35b-a3b-q4_K_M)
- [Qwen3 Embedding on Ollama](https://ollama.com/library/qwen3-embedding)
- [ClickHouse Go client](https://github.com/ClickHouse/clickhouse-go)
- [ClickHouse full-text search](https://clickhouse.com/docs/engines/table-engines/mergetree-family/invertedindexes)
- [ClickHouse vector similarity index](https://clickhouse.com/docs/engines/table-engines/mergetree-family/annindexes)
- [Go release history](https://go.dev/doc/devel/release)
- [Go packages analysis](https://pkg.go.dev/golang.org/x/tools/go/packages)
- [Tree-sitter Go binding](https://github.com/tree-sitter/go-tree-sitter)
