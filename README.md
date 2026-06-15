# MCP Gateway

A reverse proxy for [Model Context Protocol](https://modelcontextprotocol.io) servers. Connect multiple MCP servers to multiple LLM clients through a single endpoint — with tool discovery, filtering, circuit breaking, audit logging, and a live management API.

```
                    ┌──────────────────────────────────────┐
Claude Desktop ─────┤                                      ├──── context7       (npx)
LM Studio      ─────┤           MCP Gateway                ├──── filesystem     (npx)
Cursor         ─────┤        (STDIO or SSE/HTTP)           ├──── your-api       (stdio)
any MCP client ─────┤                                      ├──── remote-server  (sse)
                    └──────────────────────────────────────┘
                              admin :9090
                         /healthz  /metrics
                         /capabilities
                         /admin/servers  ← add/remove at runtime
```

## Table of Contents

- [Features](#features)
- [Quick Start](#quick-start)
- [Transport Modes](#transport-modes)
- [Client Setup](#client-setup)
- [macOS Service](#macos-service)
- [Configuration Reference](#configuration-reference)
- [Server Management API](#server-management-api)
- [Tool Filtering](#tool-filtering)
- [Hot Reload](#hot-reload)
- [Admin API Reference](#admin-api-reference)
- [Observability](#observability)
- [Circuit Breaker](#circuit-breaker)
- [Audit Logging](#audit-logging)
- [Development](#development)

---

## Features

| Feature | Description |
|---------|-------------|
| **Aggregation** | Tools, Resources, and Prompts from all downstream servers in one place |
| **Two transports** | STDIO for local use, SSE/HTTP for shared multi-client access |
| **Namespacing** | `server__tool_name` format prevents collisions across servers |
| **Tool filtering** | Per-server `allow` / `deny` lists — control exactly what the LLM sees |
| **Dynamic management** | Add or remove servers at runtime via HTTP API — no restart needed |
| **Auto-discovery** | Gateway calls `tools/list` automatically — you never need to list tools manually |
| **Hot reload** | Edit `config.yaml` + `kill -HUP` to apply changes without downtime |
| **Circuit breaker** | Automatic failover when a downstream server goes down |
| **Configurable timeouts** | Per-server connect and per-call deadlines |
| **Audit logging** | Every call logged as NDJSON with built-in PII masking |
| **Prometheus metrics** | Request counters, latency histograms, circuit breaker state |
| **macOS service** | One command to install as a launchd service that auto-starts on login |

---

## Quick Start

```bash
git clone https://github.com/yerkebulangogogo/mcp-goteway
cd mcp-goteway
make build
```

**Option A — Claude Desktop only (STDIO mode):**
```bash
make install          # builds binary + writes Claude Desktop config
```
Restart Claude Desktop. Done.

**Option B — All clients simultaneously (SSE mode, macOS):**
```bash
make install-service  # builds binary + launchd service + configures all clients
```
Restart Claude Desktop and LM Studio. Done.

---

## Transport Modes

### STDIO — local, single client

The gateway runs as a subprocess. Your LLM client spawns it on demand and communicates over stdin/stdout. Simplest setup, zero network configuration.

```yaml
gateway:
  mode: stdio    # default
```

Best for: Claude Desktop only on one machine.

### SSE/HTTP — persistent, multi-client

The gateway runs as an HTTP server. Multiple clients connect simultaneously over the network via Server-Sent Events. One gateway instance serves Claude Desktop, LM Studio, and Cursor at the same time.

```yaml
gateway:
  mode: sse
  addr: ":8080"
  base_url: "http://localhost:8080"
```

```bash
./bin/mcp-gateway --config config.yaml
# time=... msg="mcp-gateway ready, serving on SSE" addr=:8080
```

Best for: multiple clients, persistent service, team server.

---

## Client Setup

### Claude Desktop

**Config file:** `~/Library/Application Support/Claude/claude_desktop_config.json`

STDIO mode — gateway runs as a subprocess:
```json
{
  "mcpServers": {
    "mcp-gateway": {
      "command": "/absolute/path/to/bin/mcp-gateway",
      "args": ["--config", "/absolute/path/to/config.yaml"]
    }
  }
}
```

SSE mode — gateway runs as a persistent server:
```json
{
  "mcpServers": {
    "mcp-gateway": {
      "url": "http://localhost:8080/sse"
    }
  }
}
```

`make install` writes the STDIO config automatically.
`make install-service` writes the SSE config automatically.

Always restart Claude Desktop after editing.

---

### LM Studio

**Config file:** `~/.lmstudio/mcp.json`

SSE mode:
```json
{
  "mcpServers": {
    "mcp-gateway": {
      "url": "http://localhost:8080/sse"
    }
  }
}
```

STDIO mode:
```json
{
  "mcpServers": {
    "mcp-gateway": {
      "command": "/absolute/path/to/bin/mcp-gateway",
      "args": ["--config", "/absolute/path/to/config.yaml"]
    }
  }
}
```

`make install-service` writes the SSE config automatically. Restart LM Studio after editing.

---

### Cursor

Open **Settings → Features → MCP Servers**, add type `SSE`, URL `http://localhost:8080/sse`.

Or edit `~/.cursor/mcp.json`:
```json
{
  "mcpServers": {
    "mcp-gateway": {
      "url": "http://localhost:8080/sse"
    }
  }
}
```

---

### Verify everything is connected

```bash
curl http://localhost:9090/capabilities
```
```json
{
  "tools": [
    { "server": "context7", "name": "context7__resolve-library-id", "original": "resolve-library-id" },
    { "server": "context7", "name": "context7__query-docs",          "original": "query-docs" }
  ],
  "totals": { "tools": 2, "resources": 0, "prompts": 0 }
}
```

---

## macOS Service

Run the gateway as a launchd service — auto-starts on login, auto-restarts on crash, logs to `~/Library/Logs/mcp-gateway/gateway.log`.

```bash
make install-service   # build + register launchd + configure all clients
make status-service    # show service status + registered tools + recent logs
make stop-service      # stop without uninstalling
make start-service     # start again
make uninstall-service # remove the service
```

`install-service` does all of this in one shot:
1. Builds the binary
2. Creates `~/Library/LaunchAgents/com.mcp-gateway.plist` with `KeepAlive: true`
3. Loads the service (`launchctl load`)
4. Writes SSE URL to Claude Desktop and LM Studio configs

After running it, restart Claude Desktop and LM Studio.

**Check logs:**
```bash
tail -f ~/Library/Logs/mcp-gateway/gateway.log
```

---

## Configuration Reference

```yaml
# ── Transport ─────────────────────────────────────────────────────────────────
gateway:
  mode: stdio              # "stdio" (default) or "sse"
  addr: ":8080"            # SSE listen address (sse mode only)
  base_url: "http://localhost:8080"  # URL advertised to SSE clients

# ── Admin HTTP server ─────────────────────────────────────────────────────────
admin:
  enabled: true
  addr: ":9090"            # serves /healthz, /metrics, /capabilities, /admin/*

# ── Audit logging ─────────────────────────────────────────────────────────────
audit:
  enabled: true
  output: stderr           # "stderr" (default) or "file"
  path: audit.log          # path to log file; required when output=file
  mask:
    enabled: true          # mask PII in logged arguments
    patterns:              # additional regex patterns on top of built-ins
      - '\b[A-Z]{2}\d{2}[A-Z0-9]{4}\d{7}([A-Z0-9]?){0,16}\b'  # IBAN

# ── Downstream MCP servers ────────────────────────────────────────────────────
servers:
  my-server:

    # Transport — stdio (local process)
    type: stdio
    command: npx
    args:
      - -y
      - "@some/mcp-package"
    env:
      - API_KEY=secret     # extra environment variables passed to the process

    # Transport — sse (remote server)
    # type: sse
    # url: http://remote-host:3000/sse

    # Naming
    prefix: myserver       # prefix for all tools/prompts; defaults to server key
                           # tool "search" becomes "myserver__search"

    # Tool filtering
    tools:
      allow:               # expose only these tools (original names, without prefix)
        - search
        - get_item
      deny:                # or hide specific tools (allow takes precedence over deny)
        - dangerous_tool

    # Timeouts
    timeout:
      connect: 30s         # deadline for startup + tools/list at boot
      call: 10s            # per-call deadline for tool calls, resource reads, prompts

    # Circuit breaker
    circuit_breaker:
      enabled: true
      threshold: 5         # consecutive failures before tripping open
      open_duration: 30s   # how long to stay open before allowing a probe
```

---

## Server Management API

The gateway exposes a live management API on the admin port (`:9090` by default). You can add and remove downstream servers at runtime — the gateway connects, calls `tools/list` automatically, and immediately registers everything it finds.

**Config is always kept in sync** — every API call writes the change to `config.yaml`, so the state survives gateway restarts.

### Add a server

```bash
POST http://localhost:9090/admin/servers
Content-Type: application/json
```

The gateway connects to the server, calls `tools/list`, registers all tools, and returns what it found — you never need to know the tool names in advance.

**STDIO server (local process):**
```bash
curl -X POST http://localhost:9090/admin/servers \
  -H "Content-Type: application/json" \
  -d '{
    "name": "filesystem",
    "type": "stdio",
    "command": "npx",
    "args": ["-y", "@modelcontextprotocol/server-filesystem", "/Users/you/projects"]
  }'
```

**SSE server (remote URL):**
```bash
curl -X POST http://localhost:9090/admin/servers \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-api",
    "type": "sse",
    "url": "http://localhost:3000/sse"
  }'
```

**Response — all discovered tools returned immediately:**
```json
{
  "server": "filesystem",
  "tools": [
    { "name": "filesystem__read_file",      "original": "read_file",      "server": "filesystem", "description": "Read a file from the filesystem" },
    { "name": "filesystem__list_directory", "original": "list_directory", "server": "filesystem", "description": "List contents of a directory" },
    { "name": "filesystem__search_files",   "original": "search_files",   "server": "filesystem", "description": "Search for files matching a pattern" }
  ],
  "resources": [],
  "prompts": []
}
```

Connected LLM clients receive a `tools/list_changed` notification automatically and update their tool list without reconnecting.

**Full request schema:**
```json
{
  "name":    "my-server",          // required — unique identifier
  "type":    "stdio",              // required — "stdio" or "sse"
  "command": "npx",                // required for stdio
  "args":    ["-y", "@pkg/name"],  // args passed to command (stdio)
  "env":     ["API_KEY=secret"],   // extra env vars (stdio)
  "url":     "http://host/sse",    // required for sse
  "prefix":  "myserver",          // tool prefix; defaults to name

  "timeout": {
    "connect": "30s",              // default: 30s
    "call":    "10s"               // default: 30s
  },
  "circuit_breaker": {
    "enabled":      true,
    "threshold":    3,             // default: 5
    "open_duration": "60s"         // default: 30s
  }
}
```

Only `name`, `type`, and `command`/`url` are required. Everything else has defaults.

---

### Remove a server

```bash
curl -X DELETE http://localhost:9090/admin/servers/filesystem
```

```json
{ "removed": "filesystem" }
```

All tools from that server are immediately unregistered. Connected LLM clients are notified and stop seeing those tools.

---

### List active servers

```bash
curl http://localhost:9090/admin/servers
```

```json
{
  "servers": {
    "context7": {
      "status": { "circuit_breaker": "closed" },
      "tools": [
        { "name": "context7__resolve-library-id", "original": "resolve-library-id", "server": "context7" },
        { "name": "context7__query-docs",          "original": "query-docs",          "server": "context7" }
      ],
      "resources": [],
      "prompts":   []
    },
    "filesystem": {
      "status": { "circuit_breaker": "closed" },
      "tools": [
        { "name": "filesystem__read_file",      "original": "read_file",      "server": "filesystem" },
        { "name": "filesystem__list_directory", "original": "list_directory", "server": "filesystem" }
      ],
      "resources": [],
      "prompts":   []
    }
  }
}
```

---

## Tool Filtering

Control exactly which tools from each server are visible to the LLM.

```yaml
servers:
  context7:
    tools:
      allow:               # whitelist — only these tools exposed
        - resolve-library-id
        # query-docs is now hidden

  github:
    tools:
      deny:                # blacklist — expose everything except these
        - delete_repository
        - delete_branch

  my-server:
    # no tools section = all tools exposed (default)
```

Rules:
- `allow` and `deny` use **original tool names** (without the prefix)
- `allow` takes precedence — if both are set, `deny` is ignored
- Changes take effect after hot reload (`kill -HUP`) or via the API

---

## Hot Reload

Edit `config.yaml` and send SIGHUP — no restart, no client disconnection:

```bash
kill -HUP $(pgrep mcp-gateway)
```

The gateway diffs old vs new config:
- **Added** servers are connected and their tools are registered
- **Removed** servers are disconnected and their tools are unregistered
- **Unchanged** servers are untouched — ongoing calls are not affected

---

## Admin API Reference

All endpoints are served on the admin port (`:9090` by default).

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/healthz` | Health check — `200 ok` or `503 degraded` |
| `GET` | `/metrics` | Prometheus metrics |
| `GET` | `/capabilities` | All registered tools, resources, prompts with totals |
| `GET` | `/admin/servers` | Active servers with status and capabilities |
| `POST` | `/admin/servers` | Add a server, auto-discover its tools |
| `DELETE` | `/admin/servers/{name}` | Remove a server |

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
    "context7":   { "circuit_breaker": "closed" },
    "filesystem": { "circuit_breaker": "closed" }
  }
}
```

Returns `200 OK` when all servers are healthy. Returns `503 Service Unavailable` when any circuit breaker is open — useful for load balancer health probes.

Circuit breaker states: `closed` (normal), `open` (tripped), `half-open` (probing), `disabled`.

---

### Capabilities

```bash
curl http://localhost:9090/capabilities
```

```json
{
  "tools": [
    {
      "name":        "context7__resolve-library-id",
      "original":    "resolve-library-id",
      "server":      "context7",
      "description": "Resolves a library name to its Context7-compatible ID"
    }
  ],
  "resources": [],
  "prompts":   [],
  "totals": { "tools": 2, "resources": 0, "prompts": 0 }
}
```

---

### Prometheus metrics

```bash
curl http://localhost:9090/metrics
```

| Metric | Labels | Description |
|--------|--------|-------------|
| `mcp_gateway_requests_total` | `server`, `method`, `result` | Request counter; `result` is `ok`, `error`, `timeout`, or `circuit_open` |
| `mcp_gateway_request_duration_seconds` | `server`, `method` | Latency histogram |
| `mcp_gateway_circuit_breaker_open` | `server` | `1` if circuit is open, `0` otherwise |

---

## Circuit Breaker

Protects the gateway from cascading failures when a downstream server is slow or down.

```
Closed ──[N failures]──► Open ──[open_duration]──► Half-Open ──[success]──► Closed
                                                        │
                                                    [failure]
                                                        ▼
                                                      Open
```

- **Closed** — requests pass through normally
- **Open** — requests are immediately rejected; the LLM receives a readable error message and can try a different tool
- **Half-Open** — one probe request is allowed to test if the server recovered

Configure per-server in `config.yaml`:
```yaml
circuit_breaker:
  enabled: true
  threshold: 3       # consecutive failures before opening
  open_duration: 30s # how long to stay open before probing
```

---

## Audit Logging

Every tool call, resource read, and prompt get is logged as one NDJSON line.

```yaml
audit:
  enabled: true
  output: file      # or "stderr"
  path: audit.log
  mask:
    enabled: true
```

**Example log line:**
```json
{
  "ts": "2026-06-15T10:00:00Z",
  "id": "42",
  "method": "tools/call",
  "name": "context7__query-docs",
  "server": "context7",
  "args": "{\"libraryId\":\"/vercel/next.js\",\"query\":\"streaming\"}",
  "result": "ok",
  "duration_ms": 312
}
```

`result` values: `ok`, `error`, `timeout`, `circuit_open`.

**Built-in PII masking** — when `mask.enabled: true`, these patterns are always redacted in logged arguments:

| Pattern | Example input | Logged as |
|---------|---------------|-----------|
| 16-digit card number | `4111111111111111` | `[MASKED]` |
| 12-digit IIN (Kazakhstan) | `123456789012` | `[MASKED]` |
| Email address | `user@example.com` | `[MASKED]` |
| Russian/Kazakh phone | `+7 (777) 123-45-67` | `[MASKED]` |
| IBAN (built-in example) | `DE89370400440532013000` | `[MASKED]` |

Add custom patterns under `audit.mask.patterns` (standard Go regex).

---

## Development

```bash
make build           # build gateway binary → bin/mcp-gateway
make build-examples  # build dummy-db and dummy-jira test servers
make run             # run with config.yaml (go run, no build step)
make test            # run all tests with 60s timeout
make clean           # remove bin/

make install         # build + write Claude Desktop config (STDIO mode)
make install-service # build + launchd service + configure all clients (SSE mode)
make install-sse     # configure Claude Desktop + LM Studio to use SSE URL
make status-service  # show service status, active tools, recent logs
make stop-service    # stop the launchd service
make start-service   # start the launchd service
make uninstall-service # remove the launchd service
```

### Project structure

```
cmd/mcp-gateway/        entry point — flags, server wiring, admin HTTP handlers
internal/
  config/               YAML config loader, defaults, validation, config.yaml writer
  proxy/                core — connect, route, reload, add/remove servers at runtime
  registry/             thread-safe store for tools/resources/prompts → client mapping
  breaker/              circuit breaker (closed / open / half-open state machine)
  audit/                NDJSON logger with PII masker
  metrics/              Prometheus metrics registration and recording
examples/
  dummy-db/             example downstream MCP server — database tools
  dummy-jira/           example downstream MCP server — Jira tools
```

### Adding a downstream server (example end-to-end)

1. Start the gateway: `make run`
2. Add a server via API:
```bash
curl -X POST http://localhost:9090/admin/servers \
  -H "Content-Type: application/json" \
  -d '{"name":"context7","type":"stdio","command":"npx","args":["-y","@upstash/context7-mcp"]}'
```
3. Check what was found: `curl http://localhost:9090/capabilities`
4. The server is now in `config.yaml` — it will come back after gateway restart.

---

## License

MIT
