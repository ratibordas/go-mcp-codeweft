# Codex integration

Codeweft is a local STDIO MCP server. Codex CLI, the IDE extension, and the ChatGPT desktop app share MCP configuration on the same Codex host. The current official MCP reference is <https://developers.openai.com/codex/mcp>.

Build Codeweft and add this to `~/.codex/config.toml`, or to `.codex/config.toml` in a trusted project:

```toml
[mcp_servers.codeweft]
command = "/absolute/path/to/codeweft"
args = ["serve", "--project", "/absolute/path/to/project"]
env_vars = [
  "CODEWEFT_CLICKHOUSE_DSN",
  "CODEWEFT_CLICKHOUSE_USER",
  "CODEWEFT_CLICKHOUSE_PASSWORD",
  "CODEWEFT_OLLAMA_URL",
  "CODEWEFT_OLLAMA_TOKEN",
]
startup_timeout_sec = 10
tool_timeout_sec = 600
enabled = true
```

The equivalent CLI command is:

```sh
codex mcp add codeweft -- /absolute/path/to/codeweft serve --project /absolute/path/to/project
codex mcp list
```

Use `/mcp` in the Codex TUI to inspect the connection. The 600-second tool timeout allows initial indexing and local generation on mixed GPU/RAM offload.

An optional project instruction in `AGENTS.md` makes adoption explicit:

```md
Use Codeweft `search_context` before broad repository scanning. Use `impact_analysis` before changing public symbols or shared modules. Inspect files directly when Codeweft reports incomplete evidence.
```

For Docker, configure the command as `docker` and use these arguments:

```toml
command = "docker"
args = ["compose", "run", "--rm", "-T", "codeweft", "serve", "--project", "/project"]
cwd = "/absolute/path/to/go-mcp-codeweft"
```
