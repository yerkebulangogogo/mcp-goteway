# MCP Gateway

A reverse proxy for [Model Context Protocol](https://modelcontextprotocol.io) servers. Aggregate multiple MCP servers behind a single endpoint — run locally via STDIO or as a shared HTTP service your whole team connects to.

```
                    ┌─────────────────────────────────┐
Claude Desktop ─────┤                                 ├──── context7      (npx)
LM Studio      ─────┤        MCP Gateway              ├──── filesystem    (npx)
Cursor         ─────┤       (STDIO or SSE)            ├──── your-api      (stdio)
any MCP client ─────┤                                 ├──── any MCP server
                    └─────────────────────────────────┘
```

## Features

- **Two transport modes** — STDIO for local use, SSE/HTTP for shared remote access
- **Aggregation** — Tools, Resources, and Prompts from all servers in one place
- **Namespacing** — `server__tool_name` prevents collisions across servers
- **Tool filtering** — per-server `allow` / `deny` lists to control what the LLM sees
- **Circuit Breaker** — automatic failover when a downstream server goes down
- **Configurable timeouts** — per-server connect and call deadlines
- **Hot reload** — add/remove servers with `kill -HUP` without restarting
- **Audit logging** — every call logged as NDJSON with PII masking
- **Observability** — Prometheus metrics + health check HTTP endpoint

---

## Requirements

- Go 1.21+
- Any MCP servers you want to aggregate (stdio or SSE)

---

## Quick Start

```bash
git clone https://github.com/yerkebulangogogo/mcp-goteway
cd mcp-goteway
make build
```

Edit `config.yaml`, then connect to your LLM client:

```bash
make install       # Claude Desktop — builds binary + writes config automatically
```

---

## Modes

### STDIO — local, Claude Desktop / LM Studio

The gateway runs as a subprocess. The LLM client spawns it and communicates over stdin/stdout.

```yaml
gateway:
  mode: stdio      # default
```

**Claude Desktop** — `make install` writes the config automatically.

**LM Studio** — add to `~/.lmstudio/mcp.json`:
```json
{
  "mcpServers": {
    "mcp-gateway": {
      "command": "/path/to/mcp-gateway/bin/mcp-gateway",
      "args": ["--config", "/path/to/mcp-gateway/config.yaml"]
    }
  }
}
```

### SSE — remote, shared team server

The gateway runs as an HTTP server. Any MCP client connects to it over the network.

```yaml
gateway:
  mode: sse
  addr: ":8080"
  base_url: "http://your-server.com:8080"
```

```bash
./bin/mcp-gateway --config config.yaml
# → mcp-gateway ready, serving on SSE  addr=:8080
```

**Claude Desktop (SSE):**
```json
{
  "mcpServers": {
    "mcp-gateway": {
      "url": "http://your-server.com:8080/sse"
    }
  }
}
```

**LM Studio (SSE)** — add URL `http://your-server.com:8080/sse` in MCP settings.

**Cursor / VS Code** — add `http://your-server.com:8080/sse` as MCP server URL.

---

## Configuration

Full reference with all available options:

```yaml
# ── Transport ────────────────────────────────────────────────────────────────
gateway:
  mode: stdio            # "stdio" (default) or "sse"
  addr: ":8080"          # SSE listen address
  base_url: "http://localhost:8080"  # public URL advertised to SSE clients

# ── Admin HTTP server ────────────────────────────────────────────────────────
admin:
  enabled: true
  addr: ":9090"          # exposes GET /healthz and GET /metrics

# ── Audit logging ────────────────────────────────────────────────────────────
audit:
  enabled: true
  output: file           # "file" or "stderr"
  path: audit.log        # appended on each run; required when output=file
  mask:
    enabled: true        # mask PII in logged arguments
    patterns:            # extra regex patterns on top of built-ins
      - '\b[A-Z]{2}\d{2}[A-Z0-9]{4}\d{7}([A-Z0-9]?){0,16}\b'  # IBAN

# ── Downstream MCP servers ───────────────────────────────────────────────────
servers:
  my-server:
    # Transport
    type: stdio            # "stdio" or "sse"
    command: npx           # executable to spawn (stdio only)
    args:
      - -y
      - some-mcp-package
    env:                   # extra environment variables (optional)
      - API_KEY=secret

    # For SSE downstream:
    # type: sse
    # url: http://localhost:3000/sse

    # Naming
    prefix: myserver       # tool/prompt prefix; defaults to server key
                           # tools appear as "myserver__tool_name"

    # Tool filtering — control what the LLM sees from this server
    tools:
      allow:               # whitelist: only these tools are exposed (by original name)
        - search
        - get_item
      # deny:              # blacklist: expose everything except these
      #   - dangerous_tool

    # Timeouts
    timeout:
      connect: 30s         # deadline for startup handshake + capability discovery
      call: 10s            # per-request deadline for tool/resource/prompt calls

    # Circuit breaker
    circuit_breaker:
      enabled: true
      threshold: 5         # consecutive failures before opening
      open_duration: 30s   # how long to stay open before probing again
```

### Tool filtering

Use `tools.allow` to expose only specific tools from a server, or `tools.deny` to hide specific tools. Both use the **original tool name** (without prefix).

```yaml
servers:
  context7:
    tools:
      allow:
        - resolve-library-id   # only this tool is visible to the LLM
                               # query-docs is hidden

  github:
    tools:
      deny:
        - delete_repository    # hide dangerous tools, expose the rest

  my-server:
    # no tools section = all tools exposed (default)
```

`allow` takes precedence over `deny`. Changes take effect immediately after `kill -HUP`.

### Built-in PII masking patterns

When `audit.mask.enabled: true`, the following are always masked in logs:

| Pattern | Example |
|---------|---------|
| 16-digit card number | `4111111111111111` |
| 12-digit IIN (Kazakhstan) | `123456789012` |
| Email address | `user@example.com` |
| Russian/Kazakh phone | `+7 (777) 123-45-67` |

---

## Example: Context7 + Filesystem

```yaml
gateway:
  mode: stdio

admin:
  enabled: true

audit:
  enabled: true
  output: file
  path: audit.log
  mask:
    enabled: true

servers:
  context7:
    type: stdio
    command: npx
    args: ["-y", "@upstash/context7-mcp"]
    timeout:
      connect: 60s
      call: 30s
    circuit_breaker:
      enabled: true
      threshold: 3
      open_duration: 60s

  filesystem:
    type: stdio
    command: npx
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/Users/you/projects"]
    timeout:
      connect: 10s
      call: 5s
    circuit_breaker:
      enabled: true
      threshold: 5
      open_duration: 30s
    tools:
      allow:
        - read_file
        - list_directory
        - search_files
```

After `make install` and restarting Claude Desktop, you'll see:
- `context7__resolve-library-id`
- `context7__query-docs`
- `filesystem__read_file`
- `filesystem__list_directory`
- `filesystem__search_files`

---

## Hot Reload

Add or remove servers without restarting the gateway or interrupting your LLM session:

```bash
# Edit config.yaml, then:
kill -HUP $(pgrep mcp-gateway)
```

The gateway diffs old vs new config — only changed servers are affected.

---

## Observability

### Health check

```bash
curl http://localhost:9090/healthz
```

```json
{
  "status": "ok",
  "servers": {
    "context7":    { "circuit_breaker": "closed" },
    "filesystem":  { "circuit_breaker": "closed" }
  }
}
```

Returns `200 OK` when all healthy, `503` if any circuit breaker is open.

### Prometheus metrics

```bash
curl http://localhost:9090/metrics
```

| Metric | Description |
|--------|-------------|
| `mcp_gateway_requests_total{server,method,result}` | Request counter — result is `ok`, `error`, `timeout`, or `circuit_open` |
| `mcp_gateway_request_duration_seconds{server,method}` | Latency histogram |
| `mcp_gateway_circuit_breaker_open{server}` | `1` if circuit is open, `0` otherwise |

### Audit log

Every call is written as one JSON line:

```json
{"ts":"2026-06-15T10:00:00Z","id":"1","method":"tools/call","name":"context7__query-docs","server":"context7","args":"{\"libraryId\":\"/vercel/next.js\"}","result":"ok","duration_ms":312}
```

---

## Circuit Breaker

```
Closed ──N failures──► Open ──open_duration──► Half-Open ──success──► Closed
                                                   │
                                                failure
                                                   ▼
                                                 Open
```

- **Closed** — requests pass through normally
- **Open** — requests are immediately rejected with a readable error to the LLM
- **Half-Open** — one probe request allowed to test recovery

---

## Development

```bash
make build           # build gateway binary
make build-examples  # build dummy-db and dummy-jira test servers
make run             # run with default config.yaml
make test            # run all tests
make install         # build + write Claude Desktop config
make clean           # remove build artifacts
```

### Project structure

```
cmd/mcp-gateway/     — entry point
internal/
  config/            — YAML config loader with defaults and validation
  proxy/             — core: routing, connect, reload, tool filtering
  registry/          — tool/resource/prompt → client mapping
  breaker/           — circuit breaker (closed/open/half-open)
  audit/             — NDJSON logger + PII masker
  metrics/           — Prometheus metrics
examples/
  dummy-db/          — example downstream MCP server (DB tools)
  dummy-jira/        — example downstream MCP server (Jira tools)
```

---

## License

MIT
