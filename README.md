# MCP Gateway

A reverse proxy for [Model Context Protocol](https://modelcontextprotocol.io) servers. Aggregate multiple MCP servers behind a single endpoint — run locally via STDIO or as a shared HTTP service your whole team connects to.

```
                    ┌─────────────────────────────────┐
Claude Desktop ─────┤                                 ├──── context7   (npx)
Cursor         ─────┤      MCP Gateway                ├──── filesystem (npx)
VS Code        ─────┤      (STDIO or SSE/HTTP)        ├──── your-api   (stdio)
any MCP client ─────┤                                 ├──── any MCP server
                    └─────────────────────────────────┘
```

**Two modes:**
- **STDIO** — runs as a local subprocess, perfect for Claude Desktop
- **SSE** — runs as an HTTP server (`http://your-server:8080/sse`), any MCP client connects remotely

## Features

- **Two transport modes** — STDIO for local use, SSE/HTTP for shared remote access
- **Aggregation** — Tools, Resources, and Prompts from all servers appear as one
- **Namespacing** — `server__tool_name` prevents collisions across servers
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

### 1. Clone and build

```bash
git clone https://github.com/yerkebulangogogo/mcp-goteway
cd mcp-gateway
make build
```

### 2. Configure

Edit `config.yaml` (see [Configuration](#configuration) below).

### 3. Run

```bash
# Run directly
make run

# Or with a custom config path
./bin/mcp-gateway --config /path/to/config.yaml
```

### 4. Connect to Claude Desktop

```bash
make install
```

This builds the binary and writes the Claude Desktop config automatically. Restart Claude Desktop to apply.

> To preview the config without writing it: `make claude-config`

---

## Modes

### STDIO mode (local, Claude Desktop)

The gateway runs as a subprocess. Claude Desktop spawns it and communicates over stdin/stdout. This is the default.

```yaml
gateway:
  mode: stdio
```

```bash
make install   # builds binary + writes Claude Desktop config automatically
```

### SSE mode (remote, shared team server)

The gateway runs as an HTTP server. Any MCP client can connect to it from anywhere.

```yaml
gateway:
  mode: sse
  addr: ":8080"
  base_url: "http://your-server.com:8080"  # public URL clients will use
```

```bash
./bin/mcp-gateway --config config.yaml
# → mcp-gateway ready, serving on SSE  addr=:8080  base_url=http://your-server.com:8080
```

Clients connect to: `http://your-server.com:8080/sse`

**Claude Desktop (SSE mode):**
```json
{
  "mcpServers": {
    "mcp-gateway": {
      "url": "http://your-server.com:8080/sse"
    }
  }
}
```

**Cursor / VS Code:**  
Add `http://your-server.com:8080/sse` as an MCP server URL in settings.

---

## Configuration

Full reference with all available options:

```yaml
# Transport mode
gateway:
  mode: stdio          # "stdio" or "sse"
  addr: ":8080"        # SSE listen address (sse mode only)
  base_url: "http://localhost:8080"  # public URL for SSE clients

# HTTP admin server — exposes /healthz and /metrics
admin:
  enabled: true
  addr: ":9090"          # default

# Audit logging — every tool/resource/prompt call is logged as NDJSON
audit:
  enabled: true
  output: file           # "file" or "stderr"
  path: audit.log        # required when output=file; file is appended on each run
  mask:
    enabled: true        # mask PII in logged arguments
    patterns:            # extra regex patterns on top of built-ins
      - '\b[A-Z]{2}\d{2}[A-Z0-9]{4}\d{7}([A-Z0-9]?){0,16}\b'  # IBAN example

# Downstream MCP servers
servers:
  my-server:
    # ── Transport ──────────────────────────────────────────────────────────
    type: stdio            # "stdio" or "sse"
    command: npx           # executable to spawn (stdio only)
    args:
      - -y
      - some-mcp-package
    env:                   # extra environment variables (optional)
      - API_KEY=secret

    # For SSE type:
    # type: sse
    # url: http://localhost:3000/sse

    # ── Naming ─────────────────────────────────────────────────────────────
    prefix: myserver       # tool prefix; default is the server key ("my-server")
                           # tools will appear as "myserver__tool_name"

    # ── Timeouts ───────────────────────────────────────────────────────────
    timeout:
      connect: 30s         # deadline for startup handshake + capability discovery
      call: 10s            # per-request deadline for every tool/resource/prompt call

    # ── Circuit Breaker ────────────────────────────────────────────────────
    circuit_breaker:
      enabled: true
      threshold: 5         # consecutive failures before opening the circuit
      open_duration: 30s   # how long to stay open before probing again
```

### Built-in PII masking patterns

When `audit.mask.enabled: true`, the following are always masked in audit logs:

| Pattern | Example |
|---------|---------|
| 16-digit card number | `4111111111111111` |
| 12-digit IIN (Kazakhstan) | `123456789012` |
| Email address | `user@example.com` |
| Russian/Kazakh phone | `+7 (777) 123-45-67` |

Add any custom regex patterns under `audit.mask.patterns`.

---

## Real-world example: Context7 + Filesystem

```yaml
admin:
  enabled: true
  addr: ":9090"

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
    args:
      - -y
      - @upstash/context7-mcp
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
    args:
      - -y
      - "@modelcontextprotocol/server-filesystem"
      - /Users/you/projects
    timeout:
      connect: 10s
      call: 5s
    circuit_breaker:
      enabled: true
      threshold: 5
      open_duration: 30s
```

After `make install` and restarting Claude Desktop, you'll see tools like:
- `context7__resolve-library-id`
- `context7__get-library-docs`
- `filesystem__read_file`
- `filesystem__list_directory`

---

## Hot Reload

Add or remove servers without restarting the gateway or interrupting your LLM session:

```bash
# Edit config.yaml, then:
kill -HUP $(pgrep mcp-gateway)
```

The gateway diffs the old and new config — only changed servers are affected.

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

Returns `200 OK` when all servers are healthy, `503 Service Unavailable` if any circuit breaker is open.

### Prometheus metrics

```bash
curl http://localhost:9090/metrics
```

Key metrics:

| Metric | Description |
|--------|-------------|
| `mcp_gateway_requests_total{server, method, result}` | Request counter partitioned by server, method (`tools/call`, `resources/read`, `prompts/get`), and result (`ok`, `error`, `timeout`, `circuit_open`) |
| `mcp_gateway_request_duration_seconds{server, method}` | Request latency histogram |
| `mcp_gateway_circuit_breaker_open{server}` | `1` if circuit is open (server blocked), `0` otherwise |

### Audit log

Every call is written as one JSON line to the configured path:

```json
{"ts":"2026-06-14T10:00:00Z","id":"1","method":"tools/call","name":"context7__get-library-docs","server":"context7","args":"{\"libraryId\":\"/vercel/next.js\"}","result":"ok","duration_ms":312}
```

---

## Circuit Breaker

The circuit breaker protects against cascading failures:

```
Closed ──N failures──► Open ──open_duration──► Half-Open ──success──► Closed
                                                    │
                                                 failure
                                                    │
                                                  Open
```

- **Closed** — requests pass through normally
- **Open** — requests are immediately rejected with an error the LLM can read
- **Half-Open** — one probe request is allowed to test if the server recovered

---

## Development

```bash
make test          # run all tests
make build         # build gateway binary
make build-examples  # build dummy-db and dummy-jira test servers
make run           # run with default config.yaml
make clean         # remove build artifacts
```

### Project structure

```
cmd/mcp-gateway/     — entry point
internal/
  config/            — YAML config loader
  proxy/             — core routing, connect, reload, handlers
  registry/          — tool/resource/prompt → client mapping
  breaker/           — circuit breaker
  audit/             — NDJSON logger + PII masker
  metrics/           — Prometheus metrics
examples/
  dummy-db/          — example downstream MCP server (DB tools)
  dummy-jira/        — example downstream MCP server (Jira tools)
```

---

## License

MIT
