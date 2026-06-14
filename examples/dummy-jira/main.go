// dummy-jira is a minimal MCP server that simulates a Jira-like backend.
// Used to demonstrate mcp-gateway aggregating multiple downstream servers.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	s := server.NewMCPServer("dummy-jira", "0.1.0",
		server.WithToolCapabilities(false),
		server.WithResourceCapabilities(false, false),
		server.WithPromptCapabilities(false),
	)

	// --- Tools ---
	s.AddTool(mcp.NewTool("create_issue",
		mcp.WithDescription("Create a new Jira issue"),
		mcp.WithString("title", mcp.Required(), mcp.Description("Issue title")),
		mcp.WithString("description", mcp.Description("Issue description")),
		mcp.WithString("priority", mcp.Description("Priority: low, medium, high")),
	), handleCreateIssue)

	s.AddTool(mcp.NewTool("search_issues",
		mcp.WithDescription("Search Jira issues with JQL"),
		mcp.WithString("jql", mcp.Required(), mcp.Description("JQL query string")),
	), handleSearchIssues)

	s.AddTool(mcp.NewTool("get_issue",
		mcp.WithDescription("Get a Jira issue by key"),
		mcp.WithString("key", mcp.Required(), mcp.Description("Issue key e.g. PROJ-123")),
	), handleGetIssue)

	// --- Resources ---
	s.AddResource(mcp.Resource{
		URI:         "jira://projects",
		Name:        "Jira Projects",
		Description: "List of all Jira projects",
		MIMEType:    "text/plain",
	}, handleProjectsResource)

	s.AddResourceTemplate(mcp.ResourceTemplate{
		URITemplate: mustURITemplate("jira://issue/{key}"),
		Name:        "Jira Issue",
		Description: "Details of a specific Jira issue",
		MIMEType:    "text/plain",
	}, handleIssueResource)

	// --- Prompts ---
	s.AddPrompt(mcp.NewPrompt("write_issue",
		mcp.WithPromptDescription("Draft a well-structured Jira issue from a brief description"),
		mcp.WithArgument("brief", mcp.ArgumentDescription("Short description of the problem or feature"), mcp.RequiredArgument()),
	), handleWriteIssuePrompt)

	if err := server.NewStdioServer(s).Listen(context.Background(), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func handleCreateIssue(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	title, err := req.RequireString("title")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Created issue PROJ-101: %q (simulated)", title)), nil
}

func handleSearchIssues(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	jql, err := req.RequireString("jql")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("JQL: %q\nResults: PROJ-42, PROJ-99, PROJ-101 (simulated)", jql)), nil
}

func handleGetIssue(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	key, err := req.RequireString("key")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("Issue %s\nTitle: Simulated issue\nStatus: In Progress\nAssignee: Alice", key)), nil
}

func handleProjectsResource(_ context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	return []mcp.ResourceContents{
		mcp.TextResourceContents{URI: "jira://projects", Text: "Projects: PROJ, BACKEND, FRONTEND, INFRA"},
	}, nil
}

func handleIssueResource(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	text := fmt.Sprintf("Issue resource: %s\nTitle: Simulated\nStatus: Open", req.Params.URI)
	return []mcp.ResourceContents{mcp.TextResourceContents{URI: req.Params.URI, Text: text}}, nil
}

func handleWriteIssuePrompt(_ context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	brief := req.Params.Arguments["brief"]
	return &mcp.GetPromptResult{
		Description: "Write Jira Issue",
		Messages: []mcp.PromptMessage{
			{
				Role: mcp.RoleUser,
				Content: mcp.TextContent{
					Type: "text",
					Text: fmt.Sprintf(
						"Write a well-structured Jira issue based on this brief:\n\n%s\n\n"+
							"Include: Title, Description, Acceptance Criteria, and Priority.",
						brief,
					),
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
