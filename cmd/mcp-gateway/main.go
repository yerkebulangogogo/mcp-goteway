package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yerkebulangogogo/mcp-goteway/internal/audit"
	"github.com/yerkebulangogogo/mcp-goteway/internal/config"
	"github.com/yerkebulangogogo/mcp-goteway/internal/metrics"
	"github.com/yerkebulangogogo/mcp-goteway/internal/proxy"
)

//go:embed dashboard.html
var dashboardHTML []byte

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// chdir to the config file's directory so that relative paths in config
	// (commands, audit log path) work regardless of where the process was launched from.
	absConfig, err := filepath.Abs(*configPath)
	if err != nil {
		logger.Error("failed to resolve config path", "err", err)
		os.Exit(1)
	}
	if err := os.Chdir(filepath.Dir(absConfig)); err != nil {
		logger.Error("failed to chdir to config directory", "err", err)
		os.Exit(1)
	}

	cfg, err := config.Load(absConfig)
	if err != nil {
		logger.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	mcpServer := server.NewMCPServer(
		"mcp-gateway",
		"0.1.0",
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(false, true),
		server.WithPromptCapabilities(true),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	gateway := proxy.New(mcpServer, logger)

	auditLogger, err := audit.New(cfg.Audit)
	if err != nil {
		logger.Error("failed to create audit logger", "err", err)
		os.Exit(1)
	}
	if auditLogger != nil {
		gateway.SetAuditLogger(auditLogger)
		defer auditLogger.Close()
		logger.Info("audit logging enabled", "output", cfg.Audit.Output, "mask", cfg.Audit.Mask.Enabled)
	}

	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	gateway.SetMetrics(m)

	if cfg.Admin.Enabled {
		startAdminServer(ctx, cfg.Admin.Addr, reg, gateway, auditLogger, absConfig, logger)
	}

	logger.Info("connecting to downstream servers", "count", len(cfg.Servers))
	if err := gateway.Connect(ctx, cfg.Servers); err != nil {
		logger.Error("failed to connect to downstream servers", "err", err)
		os.Exit(1)
	}

	go watchSIGHUP(ctx, *configPath, gateway, logger)

	switch cfg.Gateway.Mode {
	case "sse":
		runSSE(ctx, cfg.Gateway, mcpServer, logger)
	default:
		runStdio(ctx, mcpServer, gateway, logger)
	}

	logger.Info("shutting down")
	gateway.Close()
	logger.Info("shutdown complete")
}

func runStdio(ctx context.Context, mcpServer *server.MCPServer, gateway *proxy.Gateway, logger *slog.Logger) {
	logger.Info("mcp-gateway ready, serving on stdio")
	stdioServer := server.NewStdioServer(mcpServer)
	if err := stdioServer.Listen(ctx, os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("stdio server error", "err", err)
		gateway.Close()
		os.Exit(1)
	}
}

func runSSE(ctx context.Context, cfg config.GatewayConfig, mcpServer *server.MCPServer, logger *slog.Logger) {
	sseServer := server.NewSSEServer(mcpServer, server.WithBaseURL(cfg.BaseURL))

	go func() {
		logger.Info("mcp-gateway ready, serving on SSE", "addr", cfg.Addr, "base_url", cfg.BaseURL)
		if err := sseServer.Start(cfg.Addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("SSE server error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sseServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("SSE server shutdown error", "err", err)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func startAdminServer(ctx context.Context, addr string, reg *prometheus.Registry, gw *proxy.Gateway, al *audit.Logger, configPath string, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", healthHandler(gw))
	mux.HandleFunc("/capabilities", capabilitiesHandler(gw))
	mux.HandleFunc("/admin/servers", serversHandler(ctx, gw, configPath, logger))
	mux.HandleFunc("/admin/servers/", serverByNameHandler(ctx, gw, configPath, logger))
	mux.HandleFunc("/admin/logs", logsHandler(al))
	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(dashboardHTML)
	})

	srv := &http.Server{Addr: addr, Handler: corsMiddleware(mux)}
	go func() {
		logger.Info("admin server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("admin server error", "err", err)
		}
	}()
}

func capabilitiesHandler(gw *proxy.Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tools, resources, prompts := gw.Capabilities()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tools":     tools,
			"resources": resources,
			"prompts":   prompts,
			"totals": map[string]int{
				"tools":     len(tools),
				"resources": len(resources),
				"prompts":   len(prompts),
			},
		})
	}
}

func healthHandler(gw *proxy.Gateway) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		statuses := gw.ServerStatuses()

		overallOK := true
		for _, s := range statuses {
			if s.CircuitBreaker == "open" {
				overallOK = false
				break
			}
		}

		status := "ok"
		code := http.StatusOK
		if !overallOK {
			status = "degraded"
			code = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  status,
			"servers": statuses,
		})
	}
}

// ── Dynamic server management (/admin/servers) ─────────────────────────────

type serverAddRequest struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Env     []string `json:"env"`
	URL     string   `json:"url"`
	Prefix  string   `json:"prefix"`
	Timeout struct {
		Connect string `json:"connect"`
		Call    string `json:"call"`
	} `json:"timeout"`
	CircuitBreaker struct {
		Enabled      bool   `json:"enabled"`
		Threshold    uint32 `json:"threshold"`
		OpenDuration string `json:"open_duration"`
	} `json:"circuit_breaker"`
}

func (req serverAddRequest) validate() error {
	if req.Name == "" {
		return fmt.Errorf("name is required")
	}
	switch req.Type {
	case "stdio":
		if req.Command == "" {
			return fmt.Errorf("command is required for stdio type")
		}
	case "sse":
		if req.URL == "" {
			return fmt.Errorf("url is required for sse type")
		}
	default:
		return fmt.Errorf("type must be \"stdio\" or \"sse\", got %q", req.Type)
	}
	return nil
}

func (req serverAddRequest) toConfig() (config.ServerConfig, error) {
	cfg := config.ServerConfig{
		Type:    config.ServerType(req.Type),
		Command: req.Command,
		Args:    req.Args,
		Env:     req.Env,
		URL:     req.URL,
		Prefix:  req.Prefix,
	}
	if cfg.Prefix == "" {
		cfg.Prefix = req.Name
	}

	parseDur := func(s string, fallback time.Duration) (config.Duration, error) {
		if s == "" {
			return config.Duration{Duration: fallback}, nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return config.Duration{}, err
		}
		return config.Duration{Duration: d}, nil
	}

	var err error
	if cfg.Timeout.Connect, err = parseDur(req.Timeout.Connect, 30*time.Second); err != nil {
		return cfg, fmt.Errorf("invalid timeout.connect: %w", err)
	}
	if cfg.Timeout.Call, err = parseDur(req.Timeout.Call, 30*time.Second); err != nil {
		return cfg, fmt.Errorf("invalid timeout.call: %w", err)
	}

	cfg.CircuitBreaker.Enabled = req.CircuitBreaker.Enabled
	cfg.CircuitBreaker.Threshold = req.CircuitBreaker.Threshold
	if cfg.CircuitBreaker.Threshold == 0 {
		cfg.CircuitBreaker.Threshold = 5
	}
	if cfg.CircuitBreaker.OpenDuration, err = parseDur(req.CircuitBreaker.OpenDuration, 30*time.Second); err != nil {
		return cfg, fmt.Errorf("invalid circuit_breaker.open_duration: %w", err)
	}

	return cfg, nil
}

func serversHandler(ctx context.Context, gw *proxy.Gateway, configPath string, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			listServersHandler(w, gw)
		case http.MethodPost:
			addServerHandler(w, r, ctx, gw, configPath, logger)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func serverByNameHandler(ctx context.Context, gw *proxy.Gateway, configPath string, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/admin/servers/")
		if name == "" {
			http.Error(w, "server name required", http.StatusBadRequest)
			return
		}
		if err := config.PersistRemove(configPath, name); err != nil {
			http.Error(w, fmt.Sprintf("update config: %s", err), http.StatusInternalServerError)
			return
		}
		if err := gw.RemoveServer(name); err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		logger.Info("server removed", "server", name)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"removed": name})
	}
}

func listServersHandler(w http.ResponseWriter, gw *proxy.Gateway) {
	type serverInfo struct {
		Status    proxy.ServerStatus     `json:"status"`
		Tools     []proxy.CapabilityInfo `json:"tools"`
		Resources []proxy.CapabilityInfo `json:"resources"`
		Prompts   []proxy.CapabilityInfo `json:"prompts"`
	}

	statuses := gw.ServerStatuses()
	tools, resources, prompts := gw.Capabilities()

	out := make(map[string]*serverInfo, len(statuses))
	for name, st := range statuses {
		st := st
		out[name] = &serverInfo{Status: st, Tools: []proxy.CapabilityInfo{}, Resources: []proxy.CapabilityInfo{}, Prompts: []proxy.CapabilityInfo{}}
	}
	for _, t := range tools {
		if s, ok := out[t.Server]; ok {
			s.Tools = append(s.Tools, t)
		}
	}
	for _, r := range resources {
		if s, ok := out[r.Server]; ok {
			s.Resources = append(s.Resources, r)
		}
	}
	for _, p := range prompts {
		if s, ok := out[p.Server]; ok {
			s.Prompts = append(s.Prompts, p)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"servers": out})
}

func addServerHandler(w http.ResponseWriter, r *http.Request, ctx context.Context, gw *proxy.Gateway, configPath string, logger *slog.Logger) {
	var req serverAddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %s", err), http.StatusBadRequest)
		return
	}
	if err := req.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cfg, err := req.toConfig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := config.PersistAdd(configPath, req.Name, cfg); err != nil {
		http.Error(w, fmt.Sprintf("update config: %s", err), http.StatusInternalServerError)
		return
	}

	tools, resources, prompts, err := gw.AddServer(ctx, req.Name, cfg)
	if err != nil {
		// Config was written — roll back to avoid drift
		_ = config.PersistRemove(configPath, req.Name)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if tools == nil {
		tools = []proxy.CapabilityInfo{}
	}
	if resources == nil {
		resources = []proxy.CapabilityInfo{}
	}
	if prompts == nil {
		prompts = []proxy.CapabilityInfo{}
	}

	logger.Info("server added via API", "server", req.Name, "tools", len(tools))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"server":    req.Name,
		"tools":     tools,
		"resources": resources,
		"prompts":   prompts,
	})
}

func logsHandler(al *audit.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		entries := al.Recent(limit)
		if entries == nil {
			entries = []audit.LogEntry{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries})
	}
}

func watchSIGHUP(ctx context.Context, configPath string, gateway *proxy.Gateway, logger *slog.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			logger.Info("SIGHUP received — reloading config", "path", configPath)

			newCfg, err := config.Load(configPath)
			if err != nil {
				logger.Error("reload: failed to parse config", "err", err)
				continue
			}

			if err := gateway.Reload(ctx, newCfg.Servers); err != nil {
				logger.Error("reload failed", "err", err)
			}
		}
	}
}
