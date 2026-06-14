// dummy-db is a minimal MCP server that simulates a database backend.
// It exposes tools, resources, and prompts for testing mcp-gateway.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	s := server.NewMCPServer("dummy-db", "0.1.0",
		server.WithToolCapabilities(false),
		server.WithResourceCapabilities(false, false),
		server.WithPromptCapabilities(false),
	)

	// --- Tools ---
	s.AddTool(mcp.NewTool("db_query",
		mcp.WithDescription("Execute a SQL query and return results"),
		mcp.WithString("sql", mcp.Required(), mcp.Description("SQL to execute")),
	), handleQuery)

	s.AddTool(mcp.NewTool("db_list_tables",
		mcp.WithDescription("List all tables in the database"),
	), handleListTables)

	// --- Resources ---
	s.AddResource(mcp.Resource{
		URI:         "db://schema",
		Name:        "Database Schema",
		Description: "Full schema definition of all tables",
		MIMEType:    "text/plain",
	}, handleSchemaResource)

	s.AddResourceTemplate(mcp.ResourceTemplate{
		URITemplate: mustURITemplate("db://table/{name}"),
		Name:        "Table Contents",
		Description: "Rows from a specific table",
		MIMEType:    "text/plain",
	}, handleTableResource)

	// --- Prompts ---
	s.AddPrompt(mcp.NewPrompt("sql_review",
		mcp.WithPromptDescription("Review a SQL query for correctness and performance"),
		mcp.WithArgument("sql", mcp.ArgumentDescription("The SQL query to review"), mcp.RequiredArgument()),
	), handleSQLReviewPrompt)

	s.AddPrompt(mcp.NewPrompt("db_summary",
		mcp.WithPromptDescription("Generate a summary of the database state"),
	), handleDBSummaryPrompt)

	if err := server.NewStdioServer(s).Listen(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// Tools

func handleQuery(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	sql, err := req.RequireString("sql")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Query executed: %q\nRows returned: 42 (simulated)", sql)), nil
}

func handleListTables(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("Tables: users, orders, products, inventory"), nil
}

// Resources

func handleSchemaResource(_ context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	schema := `CREATE TABLE users (id INT, name TEXT, email TEXT);
CREATE TABLE orders (id INT, user_id INT, total DECIMAL);
CREATE TABLE products (id INT, name TEXT, price DECIMAL);
CREATE TABLE inventory (product_id INT, stock INT);`
	return []mcp.ResourceContents{mcp.TextResourceContents{URI: "db://schema", Text: schema}}, nil
}

func handleTableResource(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	text := fmt.Sprintf("Simulated rows for resource: %s", req.Params.URI)
	return []mcp.ResourceContents{mcp.TextResourceContents{URI: req.Params.URI, Text: text}}, nil
}

// Prompts

func handleSQLReviewPrompt(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	sql := req.Params.Arguments["sql"]
	return &mcp.GetPromptResult{
		Description: "SQL Review",
		Messages: []mcp.PromptMessage{
			{
				Role: mcp.RoleUser,
				Content: mcp.TextContent{
					Type: "text",
					Text: fmt.Sprintf("Please review this SQL query for correctness and performance:\n\n```sql\n%s\n```", sql),
				},
			},
		},
	}, nil
}

func handleDBSummaryPrompt(_ context.Context, _ mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	return &mcp.GetPromptResult{
		Description: "Database Summary",
		Messages: []mcp.PromptMessage{
			{
				Role: mcp.RoleUser,
				Content: mcp.TextContent{
					Type: "text",
					Text: "Summarize the current state of the database: tables, row counts, and any anomalies.",
				},
			},
		},
	}, nil
}

func mustURITemplate(s string) *mcp.URITemplate {
	t := &mcp.URITemplate{}
	if err := t.UnmarshalJSON([]byte(`"` + s + `"`)); err != nil {
		panic(err)
	}
	return t
}
