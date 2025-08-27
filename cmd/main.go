// cmd/main.go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"mcp-gateway/internal/config"
	"mcp-gateway/internal/discovery"
	"mcp-gateway/internal/rbac"
	"mcp-gateway/internal/registry"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Timeout constants to avoid magic numbers
const (
	DefaultShutdownTimeout = 15 * time.Second
	DefaultServerTimeout   = 5 * time.Second
	DefaultHealthTimeout   = 3 * time.Second
)

// mcpRunner abstracts MCP server transport implementations.
type mcpRunner interface {
	Start() error
	Stop(ctx context.Context) error
	Addr() string
}

// httpRunner implements MCP server over HTTP transport for registry service.
type httpRunner struct {
	srv       *http.Server
	ln        net.Listener
	logger    *slog.Logger
	mcpServer *server.MCPServer
	reg       *registry.ServerRegistry
}

// newHTTPRunner creates a new HTTP-based MCP server runner for registry.
func newHTTPRunner(addr string, mcpServer *server.MCPServer, reg *registry.ServerRegistry, logger *slog.Logger) (*httpRunner, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()

	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		logger.Debug("MCP HTTP request received", "method", r.Method, "path", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		if err := handleMCPRequest(w, r, mcpServer, reg, logger); err != nil {
			logger.Error("MCP request handling failed", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal server error"}`))
		}
	}))

	return &httpRunner{
		srv: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: DefaultServerTimeout,
		},
		ln:        ln,
		logger:    logger,
		mcpServer: mcpServer,
		reg:       reg,
	}, nil
}

// handleMCPRequest processes MCP requests over HTTP for the registry.
type jsonrpcReq struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Method  string `json:"method"`
	Params  struct {
		URI string `json:"uri"`
	} `json:"params"`
}

func handleMCPRequest(w http.ResponseWriter, r *http.Request, _ *server.MCPServer, reg *registry.ServerRegistry, logger *slog.Logger) error {
	var req jsonrpcReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	if req.Method != "resources/read" || req.Params.URI != "mcp://registry/servers" {
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error": map[string]any{
				"code":    -32601,
				"message": "method or uri not supported",
			},
		}
		return json.NewEncoder(w).Encode(resp)
	}

	servers, err := reg.GetServersForUser(r.Context(), "")
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}

	payload := map[string]map[string]string{}
	for _, s := range servers {
		payload[s.ID] = map[string]string{"url": s.Address}
	}

	textBytes, _ := json.Marshal(map[string]any{"mcpServers": payload})

	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      req.ID,
		"result": map[string]any{
			"contents": []any{
				map[string]any{
					"uri":      "mcp://registry/servers",
					"mimeType": "application/json",
					"text":     string(textBytes),
				},
			},
		},
	}

	logger.Info("returning server list", "server_count", len(servers))
	return json.NewEncoder(w).Encode(resp)
}

func (r *httpRunner) Start() error {
	r.logger.Info("starting MCP HTTP server", "addr", r.srv.Addr)
	return r.srv.Serve(r.ln)
}

func (r *httpRunner) Stop(ctx context.Context) error {
	r.logger.Info("stopping MCP HTTP server")
	_ = r.ln.Close() // unblocks Serve
	return r.srv.Shutdown(ctx)
}

func (r *httpRunner) Addr() string {
	return r.ln.Addr().String()
}

// Gateway encapsulates all components of the MCP gateway registry.
type Gateway struct {
	cfg          *config.Config
	logger       *slog.Logger
	disc         discovery.DiscoveryProvider
	rbacProvider rbac.Provider
	registry     *registry.ServerRegistry
	mcpServer    *server.MCPServer
	runner       mcpRunner
	healthServer *http.Server
	ready        atomic.Bool
	wg           sync.WaitGroup
}

// NewGateway creates and initializes a new Gateway instance with all fallible initialization.
func NewGateway(cfg *config.Config, logger *slog.Logger) (*Gateway, error) {
	g := &Gateway{
		cfg:    cfg,
		logger: logger,
	}

	// Perform all fallible initialization here
	if err := g.initDiscovery(); err != nil {
		return nil, fmt.Errorf("discovery init failed: %w", err)
	}

	g.initRBAC()

	if err := g.initRegistry(); err != nil {
		return nil, fmt.Errorf("registry init failed: %w", err)
	}

	if err := g.initMCPServer(); err != nil {
		return nil, fmt.Errorf("MCP server init failed: %w", err)
	}

	if err := g.initRunner(); err != nil {
		return nil, fmt.Errorf("transport runner init failed: %w", err)
	}

	g.initHealthServer()

	logger.Info("gateway initialized successfully",
		"transport", cfg.Transport.Mode,
		"mcp_addr", g.runner.Addr())

	return g, nil
}

// Run executes the main application lifecycle, blocking until shutdown.
func (g *Gateway) Run(ctx context.Context) error {
	g.logger.Info("starting MCP Gateway Registry lifecycle")

	// Start discovery
	if err := g.disc.Start(); err != nil {
		return fmt.Errorf("discovery start failed: %w", err)
	}

	// Start registry
	if err := g.registry.Start(ctx); err != nil {
		return fmt.Errorf("registry start failed: %w", err)
	}

	// Mark as ready only after successful startup
	g.ready.Store(true)
	g.logger.Info("MCP Gateway Registry is ready")

	// Start MCP server
	serveErr := make(chan error, 1)
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := g.runner.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("MCP serve failed: %w", err)
		}
	}()

	g.logger.Info("MCP Gateway Registry is running")

	// Wait for shutdown signal or server error
	select {
	case <-ctx.Done():
		g.logger.Info("shutdown signal received")
	case err := <-serveErr:
		if err != nil {
			g.logger.Error("MCP server exited with error", "error", err)
			return err
		}
	}

	return g.shutdown()
}

// initDiscovery initializes the Kubernetes discovery provider.
func (g *Gateway) initDiscovery() error {
	g.logger.Info("initializing Kubernetes discovery provider",
		"namespace", g.cfg.Kubernetes.Namespace,
		"selector", g.cfg.Kubernetes.LabelSelector)

	k8sProv, err := discovery.NewK8sDiscoveryProvider(
		g.cfg.Kubernetes.Namespace,
		g.cfg.Kubernetes.LabelSelector,
		g.cfg.Kubernetes.PollInterval,
		g.cfg.Kubernetes.InCluster,
		g.logger,
	)
	if err != nil {
		return fmt.Errorf("kubernetes discovery init failed: %w", err)
	}

	g.disc = k8sProv
	g.logger.Info("Kubernetes discovery initialized successfully")
	return nil
}

// initRBAC initializes the RBAC provider.
func (g *Gateway) initRBAC() {
	g.logger.Warn("RBAC is disabled - ALL servers visible to ALL users",
		"component", "rbac")
	g.rbacProvider = rbac.NewNoOpProvider(g.logger)
}

// initRegistry initializes the server registry.
func (g *Gateway) initRegistry() error {
	g.registry = registry.NewServerRegistry(g.disc, g.rbacProvider, g.logger)
	g.logger.Info("server registry initialized successfully")
	return nil
}

// initMCPServer creates and configures the MCP server.
func (g *Gateway) initMCPServer() error {
	g.logger.Info("initializing MCP server")

	serverListResource := g.createServerListResource()

	g.mcpServer = server.NewMCPServer(
		g.cfg.Gateway.Name,
		g.cfg.Gateway.Version,
		server.WithResourceCapabilities(false /* subscribe */, true /* listChanged */),
		server.WithRecovery(),
	)

	g.mcpServer.SetResources(serverListResource)
	g.logger.Info("MCP server initialized successfully")
	return nil
}

// initRunner initializes the HTTP transport runner.
func (g *Gateway) initRunner() error {
	// Only HTTP transport supported for Kubernetes deployment
	if g.cfg.Transport.Mode != "http" {
		return fmt.Errorf("only HTTP transport supported, got: %s", g.cfg.Transport.Mode)
	}

	runner, err := newHTTPRunner(g.cfg.Transport.Addr, g.mcpServer, g.registry, g.logger)

	if err != nil {
		return fmt.Errorf("HTTP runner init failed: %w", err)
	}

	g.runner = runner
	g.logger.Info("HTTP transport runner initialized", "addr", g.cfg.Transport.Addr)
	return nil
}

// createServerListResource creates the MCP resource that returns the server list.
func (g *Gateway) createServerListResource() server.ServerResource {
	resourceURI := "mcp://registry/servers"

	resource := mcp.NewResource(
		resourceURI,
		"Available MCP Servers",
		mcp.WithResourceDescription("List of discoverable MCP servers in the cluster"),
		mcp.WithMIMEType("application/json"),
	)

	handler := func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		g.logger.Debug("serving server list resource request")

		userToken := extractUserToken(ctx)
		servers, err := g.registry.GetServersForUser(ctx, userToken)
		if err != nil {
			g.logger.Error("failed to get servers", "error", err)
			return nil, fmt.Errorf("failed to retrieve servers: %w", err)
		}

		mcpServers := make(map[string]map[string]string)
		for _, srv := range servers {
			mcpServers[srv.ID] = map[string]string{
				"url": srv.Address,
			}
		}

		response := map[string]interface{}{
			"mcpServers": mcpServers,
		}

		jsonData, err := json.MarshalIndent(response, "", "  ")
		if err != nil {
			g.logger.Error("failed to marshal server list", "error", err)
			return nil, fmt.Errorf("failed to marshal server list: %w", err)
		}

		g.logger.Info("returning server list",
			"server_count", len(servers),
			"user_token_present", userToken != "")

		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      resourceURI,
				MIMEType: "application/json",
				Text:     string(jsonData),
			},
		}, nil
	}

	return server.ServerResource{
		Resource: resource,
		Handler:  handler,
	}
}

// extractUserToken extracts user authentication token from request context.
func extractUserToken(ctx context.Context) string {
	// TODO: Implement proper token extraction from HTTP headers/context
	return ""
}

// initHealthServer sets up the health and readiness HTTP endpoints.
func (g *Gateway) initHealthServer() {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("ok")); err != nil {
			g.logger.Warn("failed to write healthz response", "error", err)
		}
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if g.ready.Load() {
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte("ready")); err != nil {
				g.logger.Warn("failed to write readyz response", "error", err)
			}
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		if _, err := w.Write([]byte("not ready")); err != nil {
			g.logger.Warn("failed to write readyz response", "error", err)
		}
	})

	g.healthServer = &http.Server{
		Addr:              fmt.Sprintf(":%d", g.cfg.Health.Port),
		Handler:           mux,
		ReadHeaderTimeout: DefaultHealthTimeout,
	}

	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.logger.Info("health server starting", "port", g.cfg.Health.Port)
		if err := g.healthServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			g.logger.Error("health server error", "error", err)
		}
	}()
}

// shutdown performs graceful shutdown of all services.
func (g *Gateway) shutdown() error {
	g.logger.Info("beginning graceful shutdown")
	g.ready.Store(false)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), DefaultShutdownTimeout)
	defer cancel()

	// Simplified error collection - fail fast on first error
	if err := g.stopRunner(shutdownCtx); err != nil {
		return err
	}

	if err := g.stopHealthServer(shutdownCtx); err != nil {
		return err
	}

	if err := g.stopRegistry(); err != nil {
		return err
	}

	if err := g.stopDiscovery(); err != nil {
		return err
	}

	// Wait for all goroutines
	done := make(chan struct{})
	go func() {
		g.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		g.logger.Info("MCP Gateway Registry shut down successfully")
		return nil
	case <-shutdownCtx.Done():
		return fmt.Errorf("shutdown timeout exceeded")
	}
}

func (g *Gateway) stopRunner(ctx context.Context) error {
	if g.runner != nil {
		if err := g.runner.Stop(ctx); err != nil {
			g.logger.Error("MCP runner shutdown error", "error", err)
			return err
		}
	}
	return nil
}

func (g *Gateway) stopHealthServer(ctx context.Context) error {
	if g.healthServer != nil {
		if err := g.healthServer.Shutdown(ctx); err != nil {
			g.logger.Error("health server shutdown error", "error", err)
			return err
		}
	}
	return nil
}

func (g *Gateway) stopRegistry() error {
	if g.registry != nil {
		if err := g.registry.Stop(); err != nil {
			g.logger.Error("registry shutdown error", "error", err)
			return err
		}
	}
	return nil
}

func (g *Gateway) stopDiscovery() error {
	if g.disc != nil {
		if err := g.disc.Stop(); err != nil {
			g.logger.Error("discovery shutdown error", "error", err)
			return err
		}
	}
	return nil
}

// envOr returns environment variable value or default if not set.
func envOr(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

// loadConfig handles configuration loading with proper precedence.
func loadConfig() (*config.Config, error) {
	defaultCfg := envOr("MCP_GATEWAY_CONFIG", "config.yaml")
	cfgPath := flag.String("config", defaultCfg, "path to config file (also available as -c)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return nil, fmt.Errorf("error loading configuration from %s: %w", *cfgPath, err)
	}

	// Validate Kubernetes-only configuration
	if cfg.Gateway.Environment != "kubernetes" {
		return nil, fmt.Errorf("only 'kubernetes' environment is supported, got: %s", cfg.Gateway.Environment)
	}

	// Validate HTTP transport requirement
	if cfg.Transport.Mode != "http" {
		return nil, fmt.Errorf("only 'http' transport is supported, got: %s", cfg.Transport.Mode)
	}

	return cfg, nil
}

// initLogger creates and configures the structured logger.
func initLogger() *slog.Logger {
	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}

	// Use JSON format for Kubernetes deployment
	handler := slog.NewJSONHandler(os.Stdout, opts)
	logger := slog.New(handler)
	slog.SetDefault(logger)

	return logger
}

func main() {
	logger := initLogger()

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("configuration error", "error", err)
		return // Let main return the error instead of os.Exit
	}

	logger.Info("configuration loaded",
		"config_path", envOr("MCP_GATEWAY_CONFIG", "config.yaml"),
		"environment", cfg.Gateway.Environment,
		"namespace", cfg.Kubernetes.Namespace,
		"transport", cfg.Transport.Mode)

	// Root context with signal handling
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	gateway, err := NewGateway(cfg, logger)
	if err != nil {
		logger.Error("gateway initialization failed", "error", err)
		return // Return error instead of os.Exit
	}

	if err := gateway.Run(ctx); err != nil {
		logger.Error("gateway exited with error", "error", err)
		return // Return error instead of os.Exit
	}

	logger.Info("gateway exited cleanly")
}
