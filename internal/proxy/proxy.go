package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/yerkebulangogogo/mcp-goteway/internal/audit"
	"github.com/yerkebulangogogo/mcp-goteway/internal/breaker"
	"github.com/yerkebulangogogo/mcp-goteway/internal/config"
	"github.com/yerkebulangogogo/mcp-goteway/internal/metrics"
	"github.com/yerkebulangogogo/mcp-goteway/internal/registry"
)

// downstream holds everything the gateway needs to talk to one MCP server.
type downstream struct {
	name        string
	client      *client.Client
	callTimeout time.Duration
	breaker     *breaker.Breaker // nil when circuit breaker is disabled
}

// execute wraps fn with a per-call timeout and circuit breaker protection.
// Zero callTimeout means no timeout (parent context is used as-is).
func (ds *downstream) execute(ctx context.Context, fn func(context.Context) error) error {
	callCtx := ctx
	if ds.callTimeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, ds.callTimeout)
		defer cancel()
	}
	if ds.breaker != nil {
		return ds.breaker.Execute(func() error { return fn(callCtx) })
	}
	return fn(callCtx)
}

// ── Gateway ────────────────────────────────────────────────────────────────

// Gateway aggregates downstream MCP servers and exposes them as one.
type Gateway struct {
	mcpServer   *server.MCPServer
	registry    *registry.Registry
	logger      *slog.Logger
	auditLogger *audit.Logger   // nil = audit disabled
	metrics     *metrics.Metrics // nil = metrics disabled

	mu          sync.Mutex // guards all mutable fields below during reload
	downstreams []*downstream

	// Per-server registered entities (with handlers) — needed to rebuild
	// MCPServer state when a server is removed during hot reload.
	serverTools     map[string][]server.ServerTool
	serverResources map[string][]server.ServerResource
	serverTemplates map[string][]server.ServerResourceTemplate
	serverPrompts   map[string][]server.ServerPrompt
}

func New(mcpServer *server.MCPServer, logger *slog.Logger) *Gateway {
	return &Gateway{
		mcpServer:       mcpServer,
		registry:        registry.New(),
		logger:          logger,
		serverTools:     make(map[string][]server.ServerTool),
		serverResources: make(map[string][]server.ServerResource),
		serverTemplates: make(map[string][]server.ServerResourceTemplate),
		serverPrompts:   make(map[string][]server.ServerPrompt),
	}
}

// SetAuditLogger attaches an audit logger to the gateway.
// Must be called before Connect. A nil value disables audit logging.
func (g *Gateway) SetAuditLogger(al *audit.Logger) {
	g.auditLogger = al
}

// SetMetrics attaches Prometheus metrics to the gateway.
// Must be called before Connect. A nil value disables metrics recording.
func (g *Gateway) SetMetrics(m *metrics.Metrics) {
	g.metrics = m
}

// ServerStatus describes the current state of one downstream server.
type ServerStatus struct {
	CircuitBreaker string `json:"circuit_breaker"` // "closed", "open", "half-open", "disabled"
}

// ServerStatuses returns the current status of all connected downstream servers.
func (g *Gateway) ServerStatuses() map[string]ServerStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]ServerStatus, len(g.downstreams))
	for _, ds := range g.downstreams {
		st := ServerStatus{CircuitBreaker: "disabled"}
		if ds.breaker != nil {
			st.CircuitBreaker = ds.breaker.State()
		}
		out[ds.name] = st
	}
	return out
}

// Connect dials every downstream server listed in servers and aggregates
// their tools, resources, and prompts. Safe to call only before serving starts.
func (g *Gateway) Connect(ctx context.Context, servers map[string]config.ServerConfig) error {
	for name, cfg := range servers {
		if err := g.connectOne(ctx, name, cfg); err != nil {
			return fmt.Errorf("connect to %q: %w", name, err)
		}
	}
	return nil
}

// Reload performs a hot reload against the new server map.
// Added servers are connected; removed servers are disconnected and their
// tools/resources/prompts are deregistered from the MCPServer — all without
// restarting the STDIO server or interrupting the LLM session.
func (g *Gateway) Reload(ctx context.Context, newServers map[string]config.ServerConfig) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	// Index current downstreams by name for O(1) lookup.
	current := make(map[string]*downstream, len(g.downstreams))
	for _, ds := range g.downstreams {
		current[ds.name] = ds
	}

	// Identify what changed.
	var toRemove []string
	for name := range current {
		if _, exists := newServers[name]; !exists {
			toRemove = append(toRemove, name)
		}
	}
	var toAdd []string
	for name := range newServers {
		if _, exists := current[name]; !exists {
			toAdd = append(toAdd, name)
		}
	}

	if len(toRemove) == 0 && len(toAdd) == 0 {
		g.logger.Info("reload: config unchanged, nothing to do")
		return nil
	}

	g.logger.Info("reload diff", "removing", toRemove, "adding", toAdd)

	// Remove servers that are no longer in the config.
	for _, name := range toRemove {
		g.removeServer(name, current[name])
	}

	// Connect newly added servers.
	for _, name := range toAdd {
		if err := g.connectOne(ctx, name, newServers[name]); err != nil {
			g.logger.Error("reload: failed to connect new server", "server", name, "err", err)
			// Continue with the rest — partial reload is better than none.
		}
	}

	g.logger.Info("reload complete",
		"active_servers", len(g.downstreams),
		"removed", len(toRemove),
		"added", len(toAdd),
	)
	return nil
}

// removeServer closes a downstream and removes all its registered entities
// from the MCPServer and registry, then rebuilds the MCPServer state.
func (g *Gateway) removeServer(name string, ds *downstream) {
	g.logger.Info("removing server", "server", name)

	ds.client.Close()

	// Remove from downstreams slice.
	filtered := g.downstreams[:0]
	for _, d := range g.downstreams {
		if d.name != name {
			filtered = append(filtered, d)
		}
	}
	g.downstreams = filtered

	// Remove from registry.
	g.registry.DeleteByServer(name)

	// Remove tracking entries.
	delete(g.serverTools, name)
	delete(g.serverResources, name)
	delete(g.serverTemplates, name)
	delete(g.serverPrompts, name)

	// Rebuild MCPServer state from the remaining tracking entries.
	g.rebuildMCPServer()
}

// rebuildMCPServer replaces all tools/resources/templates/prompts in the
// MCPServer with the current set from the tracking maps. The MCPServer's
// internal mutexes make each Set* call atomic.
func (g *Gateway) rebuildMCPServer() {
	var tools []server.ServerTool
	for _, ts := range g.serverTools {
		tools = append(tools, ts...)
	}
	g.mcpServer.SetTools(tools...)

	var resources []server.ServerResource
	for _, rs := range g.serverResources {
		resources = append(resources, rs...)
	}
	g.mcpServer.SetResources(resources...)

	var templates []server.ServerResourceTemplate
	for _, ts := range g.serverTemplates {
		templates = append(templates, ts...)
	}
	g.mcpServer.SetResourceTemplates(templates...)

	var prompts []server.ServerPrompt
	for _, ps := range g.serverPrompts {
		prompts = append(prompts, ps...)
	}
	g.mcpServer.SetPrompts(prompts...)
}

// ── Connection ─────────────────────────────────────────────────────────────

func (g *Gateway) connectOne(ctx context.Context, name string, cfg config.ServerConfig) error {
	connectCtx := ctx
	if cfg.Timeout.Connect.Duration > 0 {
		var cancel context.CancelFunc
		connectCtx, cancel = context.WithTimeout(ctx, cfg.Timeout.Connect.Duration)
		defer cancel()
	}

	cli, err := g.dial(connectCtx, cfg)
	if err != nil {
		return err
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "mcp-gateway", Version: "0.1.0"}

	initResult, err := cli.Initialize(connectCtx, initReq)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	ds := &downstream{
		name:        name,
		client:      cli,
		callTimeout: cfg.Timeout.Call.Duration,
	}
	if cfg.CircuitBreaker.Enabled {
		ds.breaker = breaker.New(name, cfg.CircuitBreaker.Threshold, cfg.CircuitBreaker.OpenDuration.Duration)
		g.logger.Info("circuit breaker enabled",
			"server", name,
			"threshold", cfg.CircuitBreaker.Threshold,
			"open_duration", cfg.CircuitBreaker.OpenDuration.Duration,
		)
	}

	g.logger.Info("connected",
		"server", name,
		"call_timeout", cfg.Timeout.Call.Duration,
		"circuit_breaker", cfg.CircuitBreaker.Enabled,
	)

	caps := initResult.Capabilities
	if caps.Tools != nil {
		if err := g.aggregateTools(connectCtx, ds, cfg.Prefix, cfg.Tools); err != nil {
			return err
		}
	}
	if caps.Resources != nil {
		if err := g.aggregateResources(connectCtx, ds); err != nil {
			return err
		}
	}
	if caps.Prompts != nil {
		if err := g.aggregatePrompts(connectCtx, ds, cfg.Prefix); err != nil {
			return err
		}
	}

	g.downstreams = append(g.downstreams, ds)
	return nil
}

func (g *Gateway) dial(ctx context.Context, cfg config.ServerConfig) (*client.Client, error) {
	switch cfg.Type {
	case config.ServerTypeStdio:
		cli, err := client.NewStdioMCPClient(cfg.Command, cfg.Env, cfg.Args...)
		if err != nil {
			return nil, fmt.Errorf("start stdio process: %w", err)
		}
		return cli, nil

	case config.ServerTypeSSE:
		t, err := transport.NewSSE(cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("create SSE transport: %w", err)
		}
		if err := t.Start(ctx); err != nil {
			return nil, fmt.Errorf("start SSE transport: %w", err)
		}
		return client.NewClient(t), nil

	default:
		return nil, fmt.Errorf("unsupported server type %q", cfg.Type)
	}
}

// ── Tools ──────────────────────────────────────────────────────────────────

func (g *Gateway) aggregateTools(ctx context.Context, ds *downstream, prefix string, toolsCfg config.ToolsConfig) error {
	result, err := ds.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}

	allow := toSet(toolsCfg.Allow)
	deny := toSet(toolsCfg.Deny)

	for _, tool := range result.Tools {
		originalName := tool.Name

		if len(allow) > 0 && !allow[originalName] {
			g.logger.Info("tool filtered (not in allow list)", "tool", originalName, "server", ds.name)
			continue
		}
		if deny[originalName] {
			g.logger.Info("tool filtered (in deny list)", "tool", originalName, "server", ds.name)
			continue
		}

		prefixedName := prefix + "__" + originalName

		proxiedTool := tool
		proxiedTool.Name = prefixedName

		g.registry.RegisterTool(prefixedName, registry.ToolEntry{
			Tool:         proxiedTool,
			OriginalName: originalName,
			Client:       ds.client,
			ServerName:   ds.name,
		})

		handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			start := time.Now()
			req.Params.Name = originalName
			var result *mcp.CallToolResult

			err := ds.execute(ctx, func(ctx context.Context) error {
				var callErr error
				result, callErr = ds.client.CallTool(ctx, req)
				return callErr
			})

			elapsed := time.Since(start)
			res := auditResult(err)
			g.auditLogger.Log(audit.Entry{
				Method:     audit.MethodToolCall,
				Name:       prefixedName,
				Server:     ds.name,
				Args:       req.Params.Arguments,
				Result:     res,
				Error:      auditError(err),
				DurationMS: elapsed.Milliseconds(),
			})
			g.metrics.Record(ds.name, audit.MethodToolCall, res, elapsed.Seconds())
			g.metrics.SetCircuitOpen(ds.name, ds.breaker != nil && ds.breaker.State() == "open")

			if err != nil {
				return toolError(ds.name, originalName, err), nil
			}
			return result, nil
		}

		g.mcpServer.AddTool(proxiedTool, handler)
		g.serverTools[ds.name] = append(g.serverTools[ds.name], server.ServerTool{
			Tool:    proxiedTool,
			Handler: handler,
		})

		g.logger.Info("registered tool",
			"exposed_as", prefixedName, "original", originalName, "server", ds.name)
	}

	g.logger.Info("aggregated tools", "server", ds.name, "count", len(result.Tools))
	return nil
}

// ── Resources ──────────────────────────────────────────────────────────────

func (g *Gateway) aggregateResources(ctx context.Context, ds *downstream) error {
	resResult, err := ds.client.ListResources(ctx, mcp.ListResourcesRequest{})
	if err != nil {
		return fmt.Errorf("list resources: %w", err)
	}

	for _, res := range resResult.Resources {
		uri := res.URI
		g.registry.RegisterResource(uri, registry.ResourceEntry{
			Resource:   res,
			Client:     ds.client,
			ServerName: ds.name,
		})

		handler := func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			start := time.Now()
			var contents []mcp.ResourceContents
			err := ds.execute(ctx, func(ctx context.Context) error {
				r, callErr := ds.client.ReadResource(ctx, req)
				if callErr != nil {
					return callErr
				}
				contents = r.Contents
				return nil
			})
			elapsed := time.Since(start)
			res2 := auditResult(err)
			g.auditLogger.Log(audit.Entry{
				Method:     audit.MethodResourceRead,
				Name:       uri,
				Server:     ds.name,
				Args:       map[string]any{"uri": req.Params.URI},
				Result:     res2,
				Error:      auditError(err),
				DurationMS: elapsed.Milliseconds(),
			})
			g.metrics.Record(ds.name, audit.MethodResourceRead, res2, elapsed.Seconds())
			g.metrics.SetCircuitOpen(ds.name, ds.breaker != nil && ds.breaker.State() == "open")
			if err != nil {
				return nil, fmt.Errorf("server %q resource %q: %w", ds.name, uri, err)
			}
			return contents, nil
		}

		g.mcpServer.AddResource(res, handler)
		g.serverResources[ds.name] = append(g.serverResources[ds.name], server.ServerResource{
			Resource: res,
			Handler:  handler,
		})

		g.logger.Info("registered resource", "uri", uri, "server", ds.name)
	}

	tmplResult, err := ds.client.ListResourceTemplates(ctx, mcp.ListResourceTemplatesRequest{})
	if err != nil {
		return fmt.Errorf("list resource templates: %w", err)
	}

	for _, tmpl := range tmplResult.ResourceTemplates {
		uriTemplate := tmpl.URITemplate.Raw()
		g.registry.RegisterResourceTemplate(uriTemplate, registry.ResourceTemplateEntry{
			Template:   tmpl,
			Client:     ds.client,
			ServerName: ds.name,
		})

		handler := func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			start := time.Now()
			var contents []mcp.ResourceContents
			err := ds.execute(ctx, func(ctx context.Context) error {
				r, callErr := ds.client.ReadResource(ctx, req)
				if callErr != nil {
					return callErr
				}
				contents = r.Contents
				return nil
			})
			elapsed := time.Since(start)
			res3 := auditResult(err)
			g.auditLogger.Log(audit.Entry{
				Method:     audit.MethodResourceRead,
				Name:       uriTemplate,
				Server:     ds.name,
				Args:       map[string]any{"uri": req.Params.URI},
				Result:     res3,
				Error:      auditError(err),
				DurationMS: elapsed.Milliseconds(),
			})
			g.metrics.Record(ds.name, audit.MethodResourceRead, res3, elapsed.Seconds())
			g.metrics.SetCircuitOpen(ds.name, ds.breaker != nil && ds.breaker.State() == "open")
			if err != nil {
				return nil, fmt.Errorf("server %q template %q: %w", ds.name, uriTemplate, err)
			}
			return contents, nil
		}

		g.mcpServer.AddResourceTemplate(tmpl, handler)
		g.serverTemplates[ds.name] = append(g.serverTemplates[ds.name], server.ServerResourceTemplate{
			Template: tmpl,
			Handler:  handler,
		})

		g.logger.Info("registered resource template", "uri_template", uriTemplate, "server", ds.name)
	}

	g.logger.Info("aggregated resources", "server", ds.name,
		"static", len(resResult.Resources), "templates", len(tmplResult.ResourceTemplates))
	return nil
}

// ── Prompts ────────────────────────────────────────────────────────────────

func (g *Gateway) aggregatePrompts(ctx context.Context, ds *downstream, prefix string) error {
	result, err := ds.client.ListPrompts(ctx, mcp.ListPromptsRequest{})
	if err != nil {
		return fmt.Errorf("list prompts: %w", err)
	}

	for _, prompt := range result.Prompts {
		originalName := prompt.Name
		prefixedName := prefix + "__" + originalName

		proxiedPrompt := prompt
		proxiedPrompt.Name = prefixedName

		g.registry.RegisterPrompt(prefixedName, registry.PromptEntry{
			Prompt:       proxiedPrompt,
			OriginalName: originalName,
			Client:       ds.client,
			ServerName:   ds.name,
		})

		handler := func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			start := time.Now()
			req.Params.Name = originalName
			var result *mcp.GetPromptResult
			err := ds.execute(ctx, func(ctx context.Context) error {
				var callErr error
				result, callErr = ds.client.GetPrompt(ctx, req)
				return callErr
			})
			elapsed := time.Since(start)
			res4 := auditResult(err)
			g.auditLogger.Log(audit.Entry{
				Method:     audit.MethodPromptGet,
				Name:       prefixedName,
				Server:     ds.name,
				Args:       req.Params.Arguments,
				Result:     res4,
				Error:      auditError(err),
				DurationMS: elapsed.Milliseconds(),
			})
			g.metrics.Record(ds.name, audit.MethodPromptGet, res4, elapsed.Seconds())
			g.metrics.SetCircuitOpen(ds.name, ds.breaker != nil && ds.breaker.State() == "open")
			if err != nil {
				return nil, fmt.Errorf("server %q prompt %q: %w", ds.name, originalName, err)
			}
			return result, nil
		}

		g.mcpServer.AddPrompt(proxiedPrompt, handler)
		g.serverPrompts[ds.name] = append(g.serverPrompts[ds.name], server.ServerPrompt{
			Prompt:  proxiedPrompt,
			Handler: handler,
		})

		g.logger.Info("registered prompt",
			"exposed_as", prefixedName, "original", originalName, "server", ds.name)
	}

	g.logger.Info("aggregated prompts", "server", ds.name, "count", len(result.Prompts))
	return nil
}

// ── Helpers ────────────────────────────────────────────────────────────────

func auditResult(err error) string {
	if err == nil {
		return "ok"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, breaker.ErrOpen):
		return "circuit_open"
default:
		return "error"
	}
}

func auditError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// toolError converts infrastructure errors into MCP tool-level errors
// so the LLM can read them and adjust its plan.
func toolError(serverName, toolName string, err error) *mcp.CallToolResult {
	var msg string
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		msg = fmt.Sprintf("Tool %q on server %q timed out. The service may be slow — try again later.", toolName, serverName)
	case errors.Is(err, breaker.ErrOpen):
		msg = fmt.Sprintf("Tool %q is temporarily unavailable (%s). Try a different approach.", toolName, err)
default:
		msg = fmt.Sprintf("Tool %q failed: %s", toolName, err)
	}
	return mcp.NewToolResultError(msg)
}

func toSet(ss []string) map[string]bool {
	if len(ss) == 0 {
		return nil
	}
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// Close shuts down all downstream client connections.
func (g *Gateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, ds := range g.downstreams {
		ds.client.Close()
	}
	g.downstreams = nil
}
