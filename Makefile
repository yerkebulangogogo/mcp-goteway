BINARY      := mcp-gateway
BUILD_DIR   := bin
PROJECT_DIR := $(shell pwd)
CLAUDE_CFG  := $(HOME)/Library/Application Support/Claude/claude_desktop_config.json

.PHONY: build build-examples run tidy test clean install claude-config all

## Build the gateway binary
build:
	go build -o $(BUILD_DIR)/$(BINARY) ./cmd/mcp-gateway

## Build example downstream servers (for testing without go run)
build-examples:
	go build -o $(BUILD_DIR)/dummy-db   ./examples/dummy-db
	go build -o $(BUILD_DIR)/dummy-jira ./examples/dummy-jira

## Run the gateway directly (uses go run for downstream servers via config.yaml)
run:
	go run ./cmd/mcp-gateway --config config.yaml

tidy:
	go mod tidy

test:
	go test ./... -timeout 60s

clean:
	rm -rf $(BUILD_DIR)

## Build the binary and write Claude Desktop config pointing at it.
## Backs up any existing claude_desktop_config.json first.
install: build
	@mkdir -p "$(HOME)/Library/Application Support/Claude"
	@if [ -f "$(CLAUDE_CFG)" ]; then \
		cp "$(CLAUDE_CFG)" "$(CLAUDE_CFG).bak"; \
		echo "Backed up existing config to $(CLAUDE_CFG).bak"; \
	fi
	@printf '{\n  "mcpServers": {\n    "mcp-gateway": {\n      "command": "$(PROJECT_DIR)/$(BUILD_DIR)/$(BINARY)",\n      "args": ["--config", "$(PROJECT_DIR)/config.yaml"]\n    }\n  }\n}\n' \
		> "$(CLAUDE_CFG)"
	@echo "Installed. Claude Desktop config written to:"
	@echo "  $(CLAUDE_CFG)"
	@echo ""
	@echo "Restart Claude Desktop to apply changes."

## Print the Claude Desktop config block without writing it (dry run)
claude-config: build
	@echo ""
	@echo "Add this to ~/Library/Application Support/Claude/claude_desktop_config.json:"
	@echo ""
	@printf '{\n  "mcpServers": {\n    "mcp-gateway": {\n      "command": "$(PROJECT_DIR)/$(BUILD_DIR)/$(BINARY)",\n      "args": ["--config", "$(PROJECT_DIR)/config.yaml"]\n    }\n  }\n}\n'
	@echo ""

## Build everything
all: tidy build build-examples
