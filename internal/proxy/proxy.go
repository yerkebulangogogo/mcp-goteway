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

// downstream holds everything the gateway needs to talk to one MCP server instance.
type downstream struct {
	name        string
	instanceIdx int
	client      *client.Client
	callTimeout time.Duration
	breaker     *breaker.Breaker
}

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

// ── Sticky pool ────────────────────────────────────────────────────────────────

// stickyPool holds N instances of the same logical server and routes requests
// using session affinity: each SSE session is always sent to the same instance.
// If the pinned instance's circuit breaker is open, the session is transparently
// reassigned to the next healthy instance.
type stickyPool struct {
	serverName string
	instances  []*downstream // immutable after creation

	mu         sync.Mutex
	sessions   map[string]int // sessionID → instance index
	nextAssign int            // round-robin cursor for new session assignment
}

// pick returns the downstream instance for this request.
// Reads the MCP session ID from ctx (set by mcp-go for every SSE client).
func (p *stickyPool) pick(ctx context.Context) *downstream {
	if len(p.instances) == 1 {
		return p.instances[0]
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	sessionID := ""
	if s := server.ClientSessionFromContext(ctx); s != nil {
		sessionID = s.SessionID()
	}

	// Return pinned instance if healthy.
	if sessionID != "" {
		if idx, ok := p.sessions[sessionID]; ok {
			ds := p.instances[idx]
			if ds.breaker == nil || ds.breaker.State() != "open" {
				return ds
			}
			// Pinned instance is down — reassign.
			delete(p.sessions, sessionID)
		}
	}

	// Find next healthy instance (round-robin among healthy).
	for i := range p.instances {
		idx := (p.nextAssign + i) % len(p.instances)
		ds := p.instances[idx]
		if ds.breaker == nil || ds.breaker.State() != "open" {
			p.nextAssign = (idx + 1) % len(p.instances)
			if sessionID != "" {
				p.sessions[sessionID] = idx
			}
			return ds
		}
	}

	// All instances unhealthy — return next anyway; circuit breaker will reject.
	idx := p.nextAssign % len(p.instances)
	p.nextAssign = (idx + 1) % len(p.instances)
	return p.instances[idx]
}

func (p *stickyPool) close() {
	for _, ds := range p.instances {
		ds.client.Close()
	}
}

// ── Gateway ────────────────────────────────────────────────────────────────────

// Gateway aggregates downstream MCP servers and exposes them as one.
type Gateway struct {
	mcpServer   *server.MCPServer
	registry    *registry.Registry
	logger      *slog.Logger
	auditLogger *audit.Logger
	metrics     *metrics.Metrics

	mu          sync.Mutex
	serverPools map[string]*stickyPool

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
		serverPools:     make(map[string]*stickyPool),
		serverTools:     make(map[string][]server.ServerTool),
		serverResources: make(map[string][]server.ServerResource),
		serverTemplates: make(map[string][]server.ServerResourceTemplate),
		serverPrompts:   make(map[string][]server.ServerPrompt),
	}
}

func (g *Gateway) SetAuditLogger(al *audit.Logger) { g.auditLogger = al }
func (g *Gateway) SetMetrics(m *metrics.Metrics)    { g.metrics = m }

// ServerStatus describes the current state of one downstream server.
type ServerStatus struct {
	// "closed" all healthy, "degraded" some open, "open" all down, "disabled" no circuit breaker
	CircuitBreaker string `json:"circuit_breaker"`
	Replicas       int    `json:"replicas,omitempty"` // set only when > 1
}

// CapabilityInfo describes one tool, resource, or prompt exposed by the gateway.
type CapabilityInfo struct {
	Name        string `json:"name"`
	Original    string `json:"original,omitempty"`
	Server      string `json:"server"`
	Description string `json:"description,omitempty"`
}

// Capabilities returns all currently registered tools, resources, and prompts.
func (g *Gateway) Capabilities() (tools, resources, prompts []CapabilityInfo) {
	for _, e := range g.registry.AllTools() {
		tools = append(tools, CapabilityInfo{
			Name: e.Tool.Name, Original: e.OriginalName,
			Server: e.ServerName, Description: e.Tool.Description,
		})
	}
	for _, e := range g.registry.AllResources() {
		resources = append(resources, CapabilityInfo{Name: e.Resource.URI, Server: e.ServerName})
	}
	for _, e := range g.registry.AllResourceTemplates() {
		resources = append(resources, CapabilityInfo{Name: e.Template.URITemplate.Raw(), Server: e.ServerName})
	}
	for _, e := range g.registry.AllPrompts() {
		prompts = append(prompts, CapabilityInfo{
			Name: e.Prompt.Name, Original: e.OriginalName, Server: e.ServerName,
		})
	}
	return
}

// ServerStatuses returns the current status of all connected downstream servers.
func (g *Gateway) ServerStatuses() map[string]ServerStatus {
	g.mu.Lock()
	defer g.mu.Unlock()

	out := make(map[string]ServerStatus, len(g.serverPools))
	for name, pool := range g.serverPools {
		n := len(pool.instances)
		openCount, disabledCount := 0, 0
		for _, ds := range pool.instances {
			if ds.breaker == nil {
				disabledCount++
			} else if ds.breaker.State() == "open" {
				openCount++
			}
		}

		cbState := "closed"
		switch {
		case disabledCount == n:
			cbState = "disabled"
		case openCount == n:
			cbState = "open"
		case openCount > 0:
			cbState = "degraded"
		}

		st := ServerStatus{CircuitBreaker: cbState}
		if n > 1 {
			st.Replicas = n
		}
		out[name] = st
	}
	return out
}

// Connect dials every downstream server listed in servers.
func (g *Gateway) Connect(ctx context.Context, servers map[string]config.ServerConfig) error {
	for name, cfg := range servers {
		if err := g.connectOne(ctx, name, cfg); err != nil {
			return fmt.Errorf("connect to %q: %w", name, err)
		}
	}
	return nil
}

// Reload performs a hot reload: added servers are connected, removed servers
// are disconnected — without restarting or interrupting active sessions.
func (g *Gateway) Reload(ctx context.Context, newServers map[string]config.ServerConfig) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	var toRemove, toAdd []string
	for name := range g.serverPools {
		if _, exists := newServers[name]; !exists {
			toRemove = append(toRemove, name)
		}
	}
	for name := range newServers {
		if _, exists := g.serverPools[name]; !exists {
			toAdd = append(toAdd, name)
		}
	}

	if len(toRemove) == 0 && len(toAdd) == 0 {
		g.logger.Info("reload: config unchanged, nothing to do")
		return nil
	}

	g.logger.Info("reload diff", "removing", toRemove, "adding", toAdd)

	for _, name := range toRemove {
		g.removeServer(name)
	}
	for _, name := range toAdd {
		if err := g.connectOne(ctx, name, newServers[name]); err != nil {
			g.logger.Error("reload: failed to connect new server", "server", name, "err", err)
		}
	}

	g.logger.Info("reload complete",
		"active_servers", len(g.serverPools),
		"removed", len(toRemove),
		"added", len(toAdd),
	)
	return nil
}

func (g *Gateway) removeServer(name string) {
	g.logger.Info("removing server", "server", name)
	pool, ok := g.serverPools[name]
	if !ok {
		return
	}
	pool.close()
	delete(g.serverPools, name)

	g.registry.DeleteByServer(name)
	delete(g.serverTools, name)
	delete(g.serverResources, name)
	delete(g.serverTemplates, name)
	delete(g.serverPrompts, name)

	g.rebuildMCPServer()
}

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

// AddServer dynamically connects a new downstream server and registers its capabilities.
func (g *Gateway) AddServer(ctx context.Context, name string, cfg config.ServerConfig) (tools, resources, prompts []CapabilityInfo, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.serverPools[name]; ok {
		return nil, nil, nil, fmt.Errorf("server %q already registered", name)
	}

	if err = g.connectOne(ctx, name, cfg); err != nil {
		return
	}

	for _, e := range g.registry.AllTools() {
		if e.ServerName == name {
			tools = append(tools, CapabilityInfo{Name: e.Tool.Name, Original: e.OriginalName, Server: name, Description: e.Tool.Description})
		}
	}
	for _, e := range g.registry.AllResources() {
		if e.ServerName == name {
			resources = append(resources, CapabilityInfo{Name: e.Resource.URI, Server: name})
		}
	}
	for _, e := range g.registry.AllResourceTemplates() {
		if e.ServerName == name {
			resources = append(resources, CapabilityInfo{Name: e.Template.URITemplate.Raw(), Server: name})
		}
	}
	for _, e := range g.registry.AllPrompts() {
		if e.ServerName == name {
			prompts = append(prompts, CapabilityInfo{Name: e.Prompt.Name, Original: e.OriginalName, Server: name})
		}
	}
	return
}

// RemoveServer dynamically disconnects a server and deregisters all its capabilities.
func (g *Gateway) RemoveServer(name string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if _, ok := g.serverPools[name]; !ok {
		return fmt.Errorf("server %q not found", name)
	}
	g.removeServer(name)
	return nil
}

// Close shuts down all downstream connections.
func (g *Gateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, pool := range g.serverPools {
		pool.close()
	}
	g.serverPools = nil
}

// ── Connection ─────────────────────────────────────────────────────────────────

// connectOne dials cfg.Replicas instances of the server, wraps them in a
// stickyPool, then discovers capabilities from the first instance.
func (g *Gateway) connectOne(ctx context.Context, name string, cfg config.ServerConfig) error {
	replicas := cfg.Replicas
	if replicas < 1 {
		replicas = 1
	}

	instances := make([]*downstream, 0, replicas)
	var firstInit *mcp.InitializeResult

	for i := 0; i < replicas; i++ {
		ds, initResult, err := g.connectInstance(ctx, name, cfg, i, replicas)
		if err != nil {
			for _, d := range instances {
				d.client.Close()
			}
			if replicas > 1 {
				return fmt.Errorf("replica %d: %w", i, err)
			}
			return err
		}
		instances = append(instances, ds)
		if i == 0 {
			firstInit = initResult
		}
	}

	pool := &stickyPool{
		serverName: name,
		instances:  instances,
		sessions:   make(map[string]int),
	}

	// Discover capabilities from the first instance; all replicas have the same tools.
	caps := firstInit.Capabilities
	if caps.Tools != nil {
		if err := g.aggregateTools(ctx, instances[0], pool, cfg.Prefix, cfg.Tools); err != nil {
			pool.close()
			return err
		}
	}
	if caps.Resources != nil {
		if err := g.aggregateResources(ctx, instances[0], pool); err != nil {
			pool.close()
			return err
		}
	}
	if caps.Prompts != nil {
		if err := g.aggregatePrompts(ctx, instances[0], pool, cfg.Prefix); err != nil {
			pool.close()
			return err
		}
	}

	g.serverPools[name] = pool
	return nil
}

// connectInstance dials one server instance, runs the MCP handshake, and
// returns the downstream struct along with the initialize result.
func (g *Gateway) connectInstance(ctx context.Context, name string, cfg config.ServerConfig, idx, total int) (*downstream, *mcp.InitializeResult, error) {
	connectCtx := ctx
	if cfg.Timeout.Connect.Duration > 0 {
		var cancel context.CancelFunc
		connectCtx, cancel = context.WithTimeout(ctx, cfg.Timeout.Connect.Duration)
		defer cancel()
	}

	cli, err := g.dial(connectCtx, cfg)
	if err != nil {
		return nil, nil, err
	}

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "mcp-gateway", Version: "0.1.0"}

	initResult, err := cli.Initialize(connectCtx, initReq)
	if err != nil {
		cli.Close()
		return nil, nil, fmt.Errorf("initialize: %w", err)
	}

	ds := &downstream{
		name:        name,
		instanceIdx: idx,
		client:      cli,
		callTimeout: cfg.Timeout.Call.Duration,
	}
	if cfg.CircuitBreaker.Enabled {
		ds.breaker = breaker.New(name, cfg.CircuitBreaker.Threshold, cfg.CircuitBreaker.OpenDuration.Duration)
	}

	logArgs := []any{
		"server", name,
		"call_timeout", cfg.Timeout.Call.Duration,
		"circuit_breaker", cfg.CircuitBreaker.Enabled,
	}
	if total > 1 {
		logArgs = append(logArgs, "instance", idx, "replicas", total)
	}
	g.logger.Info("connected", logArgs...)

	return ds, initResult, nil
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

// ── Tools ──────────────────────────────────────────────────────────────────────

// aggregateTools discovers tools from discoverFrom and registers handlers that
// route each call through pool (respecting session affinity).
func (g *Gateway) aggregateTools(ctx context.Context, discoverFrom *downstream, pool *stickyPool, prefix string, toolsCfg config.ToolsConfig) error {
	result, err := discoverFrom.client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}

	allow := toSet(toolsCfg.Allow)
	deny := toSet(toolsCfg.Deny)

	serverName := discoverFrom.name

	for _, tool := range result.Tools {
		originalName := tool.Name

		if len(allow) > 0 && !allow[originalName] {
			g.logger.Info("tool filtered (not in allow list)", "tool", originalName, "server", serverName)
			continue
		}
		if deny[originalName] {
			g.logger.Info("tool filtered (in deny list)", "tool", originalName, "server", serverName)
			continue
		}

		prefixedName := prefix + "__" + originalName

		proxiedTool := tool
		proxiedTool.Name = prefixedName

		g.registry.RegisterTool(prefixedName, registry.ToolEntry{
			Tool:         proxiedTool,
			OriginalName: originalName,
			Client:       discoverFrom.client,
			ServerName:   serverName,
		})

		handler := func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ds := pool.pick(ctx)
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
				Server:     serverName,
				Args:       req.Params.Arguments,
				Result:     res,
				Error:      auditError(err),
				DurationMS: elapsed.Milliseconds(),
			})
			g.metrics.Record(serverName, audit.MethodToolCall, res, elapsed.Seconds())
			g.metrics.SetCircuitOpen(serverName, ds.breaker != nil && ds.breaker.State() == "open")

			if err != nil {
				return toolError(serverName, originalName, err), nil
			}
			return result, nil
		}

		g.mcpServer.AddTool(proxiedTool, handler)
		g.serverTools[serverName] = append(g.serverTools[serverName], server.ServerTool{
			Tool:    proxiedTool,
			Handler: handler,
		})

		g.logger.Info("registered tool",
			"exposed_as", prefixedName, "original", originalName, "server", serverName)
	}

	g.logger.Info("aggregated tools", "server", serverName, "count", len(result.Tools))
	return nil
}

// ── Resources ──────────────────────────────────────────────────────────────────

func (g *Gateway) aggregateResources(ctx context.Context, discoverFrom *downstream, pool *stickyPool) error {
	serverName := discoverFrom.name

	resResult, err := discoverFrom.client.ListResources(ctx, mcp.ListResourcesRequest{})
	if err != nil {
		return fmt.Errorf("list resources: %w", err)
	}

	for _, res := range resResult.Resources {
		uri := res.URI
		g.registry.RegisterResource(uri, registry.ResourceEntry{
			Resource:   res,
			Client:     discoverFrom.client,
			ServerName: serverName,
		})

		handler := func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			ds := pool.pick(ctx)
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
				Server:     serverName,
				Args:       map[string]any{"uri": req.Params.URI},
				Result:     res2,
				Error:      auditError(err),
				DurationMS: elapsed.Milliseconds(),
			})
			g.metrics.Record(serverName, audit.MethodResourceRead, res2, elapsed.Seconds())
			g.metrics.SetCircuitOpen(serverName, ds.breaker != nil && ds.breaker.State() == "open")
			if err != nil {
				return nil, fmt.Errorf("server %q resource %q: %w", serverName, uri, err)
			}
			return contents, nil
		}

		g.mcpServer.AddResource(res, handler)
		g.serverResources[serverName] = append(g.serverResources[serverName], server.ServerResource{
			Resource: res,
			Handler:  handler,
		})
		g.logger.Info("registered resource", "uri", uri, "server", serverName)
	}

	tmplResult, err := discoverFrom.client.ListResourceTemplates(ctx, mcp.ListResourceTemplatesRequest{})
	if err != nil {
		return fmt.Errorf("list resource templates: %w", err)
	}

	for _, tmpl := range tmplResult.ResourceTemplates {
		uriTemplate := tmpl.URITemplate.Raw()
		g.registry.RegisterResourceTemplate(uriTemplate, registry.ResourceTemplateEntry{
			Template:   tmpl,
			Client:     discoverFrom.client,
			ServerName: serverName,
		})

		handler := func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			ds := pool.pick(ctx)
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
				Server:     serverName,
				Args:       map[string]any{"uri": req.Params.URI},
				Result:     res3,
				Error:      auditError(err),
				DurationMS: elapsed.Milliseconds(),
			})
			g.metrics.Record(serverName, audit.MethodResourceRead, res3, elapsed.Seconds())
			g.metrics.SetCircuitOpen(serverName, ds.breaker != nil && ds.breaker.State() == "open")
			if err != nil {
				return nil, fmt.Errorf("server %q template %q: %w", serverName, uriTemplate, err)
			}
			return contents, nil
		}

		g.mcpServer.AddResourceTemplate(tmpl, handler)
		g.serverTemplates[serverName] = append(g.serverTemplates[serverName], server.ServerResourceTemplate{
			Template: tmpl,
			Handler:  handler,
		})
		g.logger.Info("registered resource template", "uri_template", uriTemplate, "server", serverName)
	}

	g.logger.Info("aggregated resources", "server", serverName,
		"static", len(resResult.Resources), "templates", len(tmplResult.ResourceTemplates))
	return nil
}

// ── Prompts ────────────────────────────────────────────────────────────────────

func (g *Gateway) aggregatePrompts(ctx context.Context, discoverFrom *downstream, pool *stickyPool, prefix string) error {
	serverName := discoverFrom.name

	result, err := discoverFrom.client.ListPrompts(ctx, mcp.ListPromptsRequest{})
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
			Client:       discoverFrom.client,
			ServerName:   serverName,
		})

		handler := func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			ds := pool.pick(ctx)
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
				Server:     serverName,
				Args:       req.Params.Arguments,
				Result:     res4,
				Error:      auditError(err),
				DurationMS: elapsed.Milliseconds(),
			})
			g.metrics.Record(serverName, audit.MethodPromptGet, res4, elapsed.Seconds())
			g.metrics.SetCircuitOpen(serverName, ds.breaker != nil && ds.breaker.State() == "open")
			if err != nil {
				return nil, fmt.Errorf("server %q prompt %q: %w", serverName, originalName, err)
			}
			return result, nil
		}

		g.mcpServer.AddPrompt(proxiedPrompt, handler)
		g.serverPrompts[serverName] = append(g.serverPrompts[serverName], server.ServerPrompt{
			Prompt:  proxiedPrompt,
			Handler: handler,
		})
		g.logger.Info("registered prompt",
			"exposed_as", prefixedName, "original", originalName, "server", serverName)
	}

	g.logger.Info("aggregated prompts", "server", serverName, "count", len(result.Prompts))
	return nil
}

// ── Helpers ────────────────────────────────────────────────────────────────────

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
