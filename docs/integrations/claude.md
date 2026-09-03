# Claude Code integration

Build Codeweft, then add the local STDIO server:

```sh
claude mcp add --transport stdio --scope project codeweft -- \
  /absolute/path/to/codeweft serve --project /absolute/path/to/project
```

Alternatively, add `.mcp.json` at the project root:

```json
{
  "mcpServers": {
    "codeweft": {
      "type": "stdio",
      "command": "/absolute/path/to/codeweft",
      "args": ["serve", "--project", "/absolute/path/to/project"]
    }
  }
}
```

Run `claude mcp list` and approve the project-scoped server when prompted. See the current Claude Code MCP reference at <https://code.claude.com/docs/en/mcp>.

An optional `CLAUDE.md` instruction:

```md
Use Codeweft `search_context` before broad repository scanning. Use `impact_analysis` before changing public symbols or shared modules. Inspect files directly when Codeweft reports incomplete evidence.
```

For Docker, set `command` to `docker`, set the working directory to the Codeweft checkout, and use `compose run --rm -T codeweft serve --project /project` as the arguments. Keep ClickHouse running separately with `docker compose up -d clickhouse`.
