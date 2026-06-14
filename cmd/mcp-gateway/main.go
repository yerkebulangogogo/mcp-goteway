package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/yerkebulangogogo/mcp-goteway/internal/audit"
	"github.com/yerkebulangogogo/mcp-goteway/internal/config"
	"github.com/yerkebulangogogo/mcp-goteway/internal/metrics"
	"github.com/yerkebulangogogo/mcp-goteway/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	// Gateway logs must go to stderr — stdout is the MCP STDIO channel.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	mcpServer := server.NewMCPServer(
		"mcp-gateway",
		"0.1.0",
		server.WithToolCapabilities(false),
		server.WithResourceCapabilities(false, false),
		server.WithPromptCapabilities(false),
	)

	// SIGINT/SIGTERM cancel the root context → Listen returns → clean shutdown.
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

	// Prometheus metrics — always enabled; admin HTTP server is optional.
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	gateway.SetMetrics(m)

	if cfg.Admin.Enabled {
		startAdminServer(cfg.Admin.Addr, reg, gateway, logger)
	}

	logger.Info("connecting to downstream servers", "count", len(cfg.Servers))
	if err := gateway.Connect(ctx, cfg.Servers); err != nil {
		logger.Error("failed to connect to downstream servers", "err", err)
		os.Exit(1)
	}

	// SIGHUP triggers a hot reload: diff old vs new config, add/remove servers
	// without restarting the STDIO server or interrupting the LLM session.
	go watchSIGHUP(ctx, *configPath, gateway, logger)

	logger.Info("mcp-gateway ready, serving on stdio")

	stdioServer := server.NewStdioServer(mcpServer)
	if err := stdioServer.Listen(ctx, os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("stdio server error", "err", err)
		gateway.Close()
		os.Exit(1)
	}

	logger.Info("shutting down")
	gateway.Close()
	logger.Info("shutdown complete")
}

func startAdminServer(addr string, reg *prometheus.Registry, gw *proxy.Gateway, logger *slog.Logger) {
	mux := http.NewServeMux()

	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", healthHandler(gw))

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		logger.Info("admin server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("admin server error", "err", err)
		}
	}()
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
