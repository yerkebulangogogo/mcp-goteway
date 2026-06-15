BINARY      := mcp-gateway
BUILD_DIR   := bin
PROJECT_DIR := $(shell pwd)
CLAUDE_CFG  := $(HOME)/Library/Application Support/Claude/claude_desktop_config.json
LMSTUDIO_CFG := $(HOME)/.lmstudio/mcp.json
PLIST_ID    := com.mcp-gateway
PLIST_FILE  := $(HOME)/Library/LaunchAgents/$(PLIST_ID).plist
LOG_DIR     := $(HOME)/Library/Logs/mcp-gateway
SSE_URL     := http://localhost:8080/sse

.PHONY: build build-examples run tidy test clean \
        install install-sse \
        install-service uninstall-service start-service stop-service status-service \
        all

## Build the gateway binary
build:
	go build -o $(BUILD_DIR)/$(BINARY) ./cmd/mcp-gateway

## Build example downstream servers
build-examples:
	go build -o $(BUILD_DIR)/dummy-db   ./examples/dummy-db
	go build -o $(BUILD_DIR)/dummy-jira ./examples/dummy-jira

## Run the gateway directly
run:
	go run ./cmd/mcp-gateway --config config.yaml

tidy:
	go mod tidy

test:
	go test ./... -timeout 60s

clean:
	rm -rf $(BUILD_DIR)

## Install gateway as a macOS launchd service (auto-start on login, SSE mode)
## Also configures Claude Desktop and LM Studio to use the SSE URL.
install-service: build
	@mkdir -p "$(LOG_DIR)"
	@echo "Writing launchd plist → $(PLIST_FILE)"
	@printf '<?xml version="1.0" encoding="UTF-8"?>\n\
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n\
<plist version="1.0">\n\
<dict>\n\
  <key>Label</key>            <string>$(PLIST_ID)</string>\n\
  <key>ProgramArguments</key>\n\
  <array>\n\
    <string>$(PROJECT_DIR)/$(BUILD_DIR)/$(BINARY)</string>\n\
    <string>--config</string>\n\
    <string>$(PROJECT_DIR)/config.yaml</string>\n\
  </array>\n\
  <key>EnvironmentVariables</key>\n\
  <dict>\n\
    <key>PATH</key><string>$(PATH)</string>\n\
  </dict>\n\
  <key>RunAtLoad</key>        <true/>\n\
  <key>KeepAlive</key>        <true/>\n\
  <key>StandardOutPath</key>  <string>$(LOG_DIR)/gateway.log</string>\n\
  <key>StandardErrorPath</key><string>$(LOG_DIR)/gateway.log</string>\n\
  <key>WorkingDirectory</key> <string>$(PROJECT_DIR)</string>\n\
</dict>\n\
</plist>\n' > "$(PLIST_FILE)"
	@launchctl unload "$(PLIST_FILE)" 2>/dev/null || true
	@launchctl load "$(PLIST_FILE)"
	@echo "Service started."
	@echo ""
	@$(MAKE) --no-print-directory install-sse
	@echo ""
	@echo "Logs: tail -f $(LOG_DIR)/gateway.log"

## Uninstall the launchd service
uninstall-service:
	@launchctl unload "$(PLIST_FILE)" 2>/dev/null || true
	@rm -f "$(PLIST_FILE)"
	@echo "Service uninstalled."

## Start the service manually
start-service:
	@launchctl load "$(PLIST_FILE)"
	@echo "Service started."

## Stop the service manually
stop-service:
	@launchctl unload "$(PLIST_FILE)"
	@echo "Service stopped."

## Show service status and recent logs
status-service:
	@echo "=== Service status ==="
	@launchctl list | grep mcp-gateway || echo "  (not running)"
	@echo ""
	@echo "=== Capabilities ==="
	@curl -s http://localhost:9090/capabilities 2>/dev/null | python3 -c "\
import json,sys; d=json.load(sys.stdin); \
[print(f\"  {t['server']:<12} {t['name']}\") for t in sorted(d['tools'],key=lambda x:x['server'])]; \
print(f\"  --- {d['totals']['tools']} tools, {d['totals']['resources']} resources, {d['totals']['prompts']} prompts ---\")" \
	|| echo "  (gateway not reachable on :9090)"
	@echo ""
	@echo "=== Recent logs ==="
	@tail -20 "$(LOG_DIR)/gateway.log" 2>/dev/null || echo "  (no logs yet)"

## Configure Claude Desktop and LM Studio to use the running SSE gateway
install-sse: build
	@echo "Configuring Claude Desktop → $(SSE_URL)"
	@mkdir -p "$(HOME)/Library/Application Support/Claude"
	@if [ -f "$(CLAUDE_CFG)" ]; then cp "$(CLAUDE_CFG)" "$(CLAUDE_CFG).bak"; fi
	@python3 -c "\
import json, sys; \
cfg = {}; \
f = open('$(CLAUDE_CFG)') if __import__('os').path.exists('$(CLAUDE_CFG)') else None; \
cfg = json.load(f) if f else {}; \
f and f.close(); \
cfg.setdefault('mcpServers', {})['mcp-gateway'] = {'url': '$(SSE_URL)'}; \
open('$(CLAUDE_CFG)', 'w').write(json.dumps(cfg, indent=2))"
	@echo "Configuring LM Studio   → $(SSE_URL)"
	@mkdir -p "$(HOME)/.lmstudio"
	@printf '{\n  "mcpServers": {\n    "mcp-gateway": {\n      "url": "$(SSE_URL)"\n    }\n  }\n}\n' > "$(LMSTUDIO_CFG)"
	@echo ""
	@echo "Restart Claude Desktop and LM Studio to apply."

## Install gateway as STDIO subprocess for Claude Desktop only (original mode)
install: build
	@mkdir -p "$(HOME)/Library/Application Support/Claude"
	@if [ -f "$(CLAUDE_CFG)" ]; then cp "$(CLAUDE_CFG)" "$(CLAUDE_CFG).bak"; fi
	@python3 -c "\
import json, sys; \
f = open('$(CLAUDE_CFG)') if __import__('os').path.exists('$(CLAUDE_CFG)') else None; \
cfg = json.load(f) if f else {}; \
f and f.close(); \
cfg.setdefault('mcpServers', {})['mcp-gateway'] = {'command': '$(PROJECT_DIR)/$(BUILD_DIR)/$(BINARY)', 'args': ['--config', '$(PROJECT_DIR)/config.yaml']}; \
open('$(CLAUDE_CFG)', 'w').write(json.dumps(cfg, indent=2))"
	@echo "Installed (STDIO mode). Restart Claude Desktop to apply."

## Build everything
all: tidy build build-examples
