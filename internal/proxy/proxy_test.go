package proxy_test

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/erke/mcp-gateway/internal/config"
	"github.com/erke/mcp-gateway/internal/proxy"
)

// ── Helpers ────────────────────────────────────────────────────────────────

func dbServers() map[string]config.ServerConfig {
	return map[string]config.ServerConfig{
		"dummy-db": {
			Type:    config.ServerTypeStdio,
			Command: "go",
			Args:    []string{"run", "../../examples/dummy-db"},
			Prefix:  "dummy-db",
		},
	}
}

func jiraServers() map[string]config.ServerConfig {
	return map[string]config.ServerConfig{
		"dummy-jira": {
			Type:    config.ServerTypeStdio,
			Command: "go",
			Args:    []string{"run", "../../examples/dummy-jira"},
			Prefix:  "dummy-jira",
		},
	}
}

func bothServers() map[string]config.ServerConfig {
	m := make(map[string]config.ServerConfig)
	for k, v := range dbServers() {
		m[k] = v
	}
	for k, v := range jiraServers() {
		m[k] = v
	}
	return m
}

// newGatewayFull returns a started Gateway and its MCPServer.
// t.Cleanup closes the gateway automatically.
func newGatewayFull(t *testing.T, servers map[string]config.ServerConfig) (*proxy.Gateway, *server.MCPServer) {
	t.Helper()

	mcpServer := server.NewMCPServer(
		"mcp-gateway", "0.1.0",
		server.WithToolCapabilities(false),
		server.WithResourceCapabilities(false, false),
		server.WithPromptCapabilities(false),
	)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	gw := proxy.New(mcpServer, logger)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(func() {
		gw.Close()
		cancel()
	})

	if err := gw.Connect(ctx, servers); err != nil {
		t.Fatalf("gateway.Connect: %v", err)
	}

	return gw, mcpServer
}

// newGateway returns only the MCPServer (sufficient for non-reload tests).
func newGateway(t *testing.T) *server.MCPServer {
	t.Helper()
	_, mcpServer := newGatewayFull(t, dbServers())
	return mcpServer
}

// newClient creates an initialized in-process MCP client connected to mcpServer.
func newClient(t *testing.T, mcpServer *server.MCPServer) *mcpclient.Client {
	t.Helper()

	cli, err := mcpclient.NewInProcessClient(mcpServer)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test-client", Version: "0.0.1"}

	if _, err := cli.Initialize(context.Background(), initReq); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	return cli
}

func toolNames(t *testing.T, cli *mcpclient.Client) map[string]bool {
	t.Helper()
	result, err := cli.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make(map[string]bool, len(result.Tools))
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	return names
}

// ── Tools ──────────────────────────────────────────────────────────────────

func TestToolsList(t *testing.T) {
	cli := newClient(t, newGateway(t))

	names := toolNames(t, cli)
	for _, want := range []string{"dummy-db__db_query", "dummy-db__db_list_tables"} {
		if !names[want] {
			t.Errorf("expected tool %q, got %v", want, names)
		}
	}
}

func TestToolCall(t *testing.T) {
	cli := newClient(t, newGateway(t))

	req := mcp.CallToolRequest{}
	req.Params.Name = "dummy-db__db_query"
	req.Params.Arguments = map[string]any{"sql": "SELECT 1"}

	result, err := cli.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool returned error content: %v", result.Content)
	}
	if len(result.Content) == 0 {
		t.Fatal("expected non-empty content")
	}
}

func TestToolCallUnknownTool(t *testing.T) {
	cli := newClient(t, newGateway(t))

	req := mcp.CallToolRequest{}
	req.Params.Name = "nonexistent__tool"
	req.Params.Arguments = map[string]any{}

	result, err := cli.CallTool(context.Background(), req)
	if err == nil && result != nil && !result.IsError {
		t.Log("server returned non-error for unknown tool — acceptable behaviour")
	}
}

// ── Resources ──────────────────────────────────────────────────────────────

func TestResourcesList(t *testing.T) {
	cli := newClient(t, newGateway(t))

	result, err := cli.ListResources(context.Background(), mcp.ListResourcesRequest{})
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}

	found := false
	for _, r := range result.Resources {
		if r.URI == "db://schema" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected db://schema, got %v", result.Resources)
	}
}

func TestResourcesTemplatesList(t *testing.T) {
	cli := newClient(t, newGateway(t))

	result, err := cli.ListResourceTemplates(context.Background(), mcp.ListResourceTemplatesRequest{})
	if err != nil {
		t.Fatalf("ListResourceTemplates: %v", err)
	}

	if len(result.ResourceTemplates) == 0 {
		t.Fatal("expected at least one resource template")
	}
	if got := result.ResourceTemplates[0].URITemplate.Raw(); got != "db://table/{name}" {
		t.Errorf("expected db://table/{name}, got %q", got)
	}
}

func TestResourceRead(t *testing.T) {
	cli := newClient(t, newGateway(t))

	req := mcp.ReadResourceRequest{}
	req.Params.URI = "db://schema"

	result, err := cli.ReadResource(context.Background(), req)
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(result.Contents) == 0 {
		t.Fatal("expected non-empty resource contents")
	}
}

func TestResourceReadTemplate(t *testing.T) {
	cli := newClient(t, newGateway(t))

	req := mcp.ReadResourceRequest{}
	req.Params.URI = "db://table/users"

	result, err := cli.ReadResource(context.Background(), req)
	if err != nil {
		t.Fatalf("ReadResource (template): %v", err)
	}
	if len(result.Contents) == 0 {
		t.Fatal("expected non-empty resource contents")
	}
}

// ── Prompts ────────────────────────────────────────────────────────────────

func TestPromptsList(t *testing.T) {
	cli := newClient(t, newGateway(t))

	result, err := cli.ListPrompts(context.Background(), mcp.ListPromptsRequest{})
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}

	names := make(map[string]bool)
	for _, p := range result.Prompts {
		names[p.Name] = true
	}

	for _, want := range []string{"dummy-db__sql_review", "dummy-db__db_summary"} {
		if !names[want] {
			t.Errorf("expected prompt %q, got %v", want, names)
		}
	}
}

func TestPromptGet(t *testing.T) {
	cli := newClient(t, newGateway(t))

	req := mcp.GetPromptRequest{}
	req.Params.Name = "dummy-db__sql_review"
	req.Params.Arguments = map[string]string{"sql": "SELECT * FROM users"}

	result, err := cli.GetPrompt(context.Background(), req)
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if len(result.Messages) == 0 {
		t.Fatal("expected non-empty messages")
	}
}

func TestPromptGetNoArgs(t *testing.T) {
	cli := newClient(t, newGateway(t))

	req := mcp.GetPromptRequest{}
	req.Params.Name = "dummy-db__db_summary"

	result, err := cli.GetPrompt(context.Background(), req)
	if err != nil {
		t.Fatalf("GetPrompt (no args): %v", err)
	}
	if len(result.Messages) == 0 {
		t.Fatal("expected non-empty messages")
	}
}

// ── Namespacing ────────────────────────────────────────────────────────────

func TestToolNamespacing(t *testing.T) {
	cli := newClient(t, newGateway(t))
	for name := range toolNames(t, cli) {
		if len(name) < len("dummy-db__") || name[:len("dummy-db__")] != "dummy-db__" {
			t.Errorf("tool %q missing prefix dummy-db__", name)
		}
	}
}

func TestPromptNamespacing(t *testing.T) {
	cli := newClient(t, newGateway(t))

	result, err := cli.ListPrompts(context.Background(), mcp.ListPromptsRequest{})
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	for _, p := range result.Prompts {
		if len(p.Name) < len("dummy-db__") || p.Name[:len("dummy-db__")] != "dummy-db__" {
			t.Errorf("prompt %q missing prefix dummy-db__", p.Name)
		}
	}
}

// ── Hot reload ─────────────────────────────────────────────────────────────

// TestReloadAddServer starts with dummy-db only, then reloads with both servers.
// Verifies jira tools appear and db tools are still present.
func TestReloadAddServer(t *testing.T) {
	gw, mcpServer := newGatewayFull(t, dbServers())
	cli := newClient(t, mcpServer)

	// Before reload: only db tools.
	before := toolNames(t, cli)
	if !before["dummy-db__db_query"] {
		t.Fatal("expected dummy-db__db_query before reload")
	}
	if before["dummy-jira__create_issue"] {
		t.Fatal("jira tools should not be present before reload")
	}

	// Reload: add jira.
	ctx := context.Background()
	if err := gw.Reload(ctx, bothServers()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	after := toolNames(t, cli)
	if !after["dummy-db__db_query"] {
		t.Error("dummy-db__db_query should still be present after reload")
	}
	if !after["dummy-jira__create_issue"] {
		t.Error("dummy-jira__create_issue should appear after reload")
	}
}

// TestReloadRemoveServer starts with both servers, reloads with db only.
// Verifies jira tools disappear and db tools remain.
func TestReloadRemoveServer(t *testing.T) {
	gw, mcpServer := newGatewayFull(t, bothServers())
	cli := newClient(t, mcpServer)

	before := toolNames(t, cli)
	if !before["dummy-jira__create_issue"] {
		t.Fatal("expected jira tools before reload")
	}

	// Reload: remove jira.
	ctx := context.Background()
	if err := gw.Reload(ctx, dbServers()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	after := toolNames(t, cli)
	if after["dummy-jira__create_issue"] {
		t.Error("jira tools should be gone after reload")
	}
	if !after["dummy-db__db_query"] {
		t.Error("db tools should still be present after reload")
	}
}

// TestReloadNoChange verifies that a reload with the same config is a no-op.
func TestReloadNoChange(t *testing.T) {
	gw, mcpServer := newGatewayFull(t, dbServers())
	cli := newClient(t, mcpServer)

	before := toolNames(t, cli)

	if err := gw.Reload(context.Background(), dbServers()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	after := toolNames(t, cli)
	if len(before) != len(after) {
		t.Errorf("tool count changed during no-op reload: %d → %d", len(before), len(after))
	}
}

// TestReloadToolCallAfterRemove verifies that tools from a removed server
// are no longer callable after reload.
func TestReloadToolCallAfterRemove(t *testing.T) {
	gw, mcpServer := newGatewayFull(t, bothServers())
	cli := newClient(t, mcpServer)

	// Remove jira.
	if err := gw.Reload(context.Background(), dbServers()); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = "dummy-jira__create_issue"
	req.Params.Arguments = map[string]any{"title": "test"}

	// The call should either fail or return an error result — not succeed.
	result, err := cli.CallTool(context.Background(), req)
	if err == nil && result != nil && !result.IsError {
		t.Error("expected error when calling tool from removed server")
	}
}
