# Codeweft

Codeweft is a local, single-project MCP server that gives coding agents compact, current context instead of making vendor models repeatedly scan an entire repository. It builds structural graphs for Go and JavaScript/TypeScript, embeds Markdown and Obsidian documentation, retrieves relevant evidence, and optionally asks a local Ollama model to produce a cited summary.

## Prerequisites

- Go 1.27 for native builds
- ClickHouse 26.8.1.2041
- Ollama with enough combined GPU and system memory for the selected models
- Git for the fast incremental-change path; filesystem metadata and hashes remain the authority

## Quick start

```sh
cp .env.example .env
# Set CODEWEFT_PROJECT_PATH in .env.
ollama pull qwen3.6:35b-a3b-q4_K_M
ollama pull qwen3-embedding:0.6b
docker compose up -d clickhouse
docker compose run --rm -T codeweft index --full --project /project
```

Then connect Codex or Claude Code to `codeweft serve --project /absolute/path/to/project`. See [Codex](docs/integrations/codex.md) and [Claude Code](docs/integrations/claude.md).

## Ollama models

The default generation model is `qwen3.6:35b-a3b-q4_K_M`; the default 1,024-dimension embedding model is `qwen3-embedding:0.6b`. Embedding and generation requests are serialized so they do not compete for GPU and system memory. Generation and document vectors are optional: exact, full-text, and graph retrieval continue when either model is unavailable.

## Docker Compose

Compose starts ClickHouse but deliberately does not start Ollama. Ollama stays on the host:

```sh
docker compose up -d clickhouse
docker compose run --rm -T codeweft serve --project /project
```

Do not run the MCP process detached: its standard input and output belong to the MCP client. The project mount is read-only. On Linux, `extra_hosts: host.docker.internal:host-gateway` is already present in `compose.yaml`; keep it if Ollama runs on the host.

## Native install

```sh
go build -o codeweft ./cmd/codeweft
export CODEWEFT_CLICKHOUSE_DSN=clickhouse://localhost:9000/codeweft
./codeweft index --full --project /absolute/path/to/project
```

## CLI

```text
codeweft serve --project PATH [--config FILE]
codeweft index --project PATH [--config FILE] [--full]
codeweft search --project PATH [--config FILE] --question TEXT [--max-tokens N]
codeweft status --project PATH [--config FILE]
codeweft benchmark --project PATH [--config FILE] --suite FILE
codeweft purge --project PATH [--config FILE] --yes
```

Progress logs go to stderr. JSON results and MCP frames go to stdout. `purge` prints the canonical root and its SHA-256 project identity before deleting only that project's ClickHouse rows.

## MCP tools

- `search_context`: synchronize changes and return a compact cited context pack.
- `impact_analysis`: traverse deterministic upstream, downstream, or bidirectional code impact at depth 1 or 2.
- `refresh_index`: join or start a delta or full refresh with progress.
- `index_status`: report generation, phase, elapsed time, throughput, ETA, pending files, and warnings.

## Configuration

Copy `.codeweft.example.yaml` to `.codeweft.yaml` in the indexed project. This file contains only indexing and retrieval policy. Connections and credentials use environment variables:

```text
CODEWEFT_CLICKHOUSE_DSN
CODEWEFT_CLICKHOUSE_USER
CODEWEFT_CLICKHOUSE_PASSWORD
CODEWEFT_OLLAMA_URL
CODEWEFT_OLLAMA_TOKEN
```

`max_tokens` accepts 256 through 12,000 and defaults to 3,500. Graph depth accepts 1 or 2. Files default to a 2 MiB limit.

## Freshness guarantees

Git status and diffs produce a cheap candidate set. Metadata narrows hashing, but SHA-256 content hashes are authoritative. Changed files, affected reverse dependencies, resolution files, renames, and deletions receive a new generation. Derived rows are written first; the active file hash is changed last. A retrieval waits for its relevant refresh or reports pending freshness instead of returning known-stale evidence.

Before returning evidence, Codeweft reads the current file again and verifies both the active manifest hash and the current SHA-256. Code evidence is an exact line-range snippet. Markdown evidence is an exact quote with heading, extension, and line range.

## Degraded behavior

- Missing generation model: ranked evidence without a synthesized summary.
- Missing embeddings or vector search: code FTS, Markdown FTS, exact matches, and graph traversal remain available.
- Parser or mid-index file change: the affected path is reported as pending or contributes no stale structural rows.
- Incomplete graph resolution: conservative results and warnings; direct file inspection remains the fallback.

## Supported files

Structural indexing supports `.go`, `.js`, `.jsx`, `.mjs`, `.cjs`, `.ts`, `.tsx`, and `.d.ts`. Documentation indexing supports `.md`, including Obsidian headings, tags, aliases, and wiki-links. Other discovered text formats are not evidence sources in the MVP.

## Exclusions

Secrets, common dependency/build/cache directories, generated Go, minified assets, binary files, and files above the configured limit are excluded. Tests can be disabled. Exact path, directory-name, and filename exclusions are configurable. Codeweft does not run tests, generators, builds, package downloads, or project binaries while indexing.

## Benchmarking

The checked-in suite contains neutral expected paths and symbols:

```sh
codeweft benchmark \
  --project /absolute/path/to/project \
  --config testdata/benchmark/crm-api.yaml \
  --suite testdata/eval/queries.json
```

The runner performs one full index, at least 30 deterministic warm retrievals, and one optional generation sample per query. It never mutates the target project. Mutation latency belongs in a copied fixture or the integration suite.

## Security

The project root is canonicalized once and is not accepted from MCP inputs. Evidence paths must remain under that root, symlink escapes are rejected, SQL values are parameterized, and secrets are excluded before content reads. Keep credentials in environment variables, not `.codeweft.yaml`. Review any local MCP server before granting it access to a repository.

## Limitations

- One project per process.
- STDIO transport only.
- Go analysis uses cached dependencies with network access disabled.
- JavaScript/TypeScript resolution is syntax-level and intentionally conservative.
- No filesystem watcher; synchronization happens before retrieval or on explicit refresh.
- Local generation latency depends heavily on GPU/RAM offload and has no hard target.

## License

MIT. See [LICENSE](LICENSE).
