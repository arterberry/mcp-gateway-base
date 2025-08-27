// internal/registry/registry.go
package registry

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	discoverypkg "mcp-gateway/internal/discovery"
	"mcp-gateway/internal/rbac"
)

// DiscoveryProvider defines the interface for server discovery.
type DiscoveryProvider interface {
	Start() error
	Stop() error
	GetServers() ([]discoverypkg.ServerInfo, error)
}

// ServerRegistry manages discovered MCP servers and provides RBAC-filtered access.
type ServerRegistry struct {
	discoveryProv DiscoveryProvider
	rbacProvider  rbac.Provider
	logger        *slog.Logger

	// Server cache and synchronization
	mu       sync.RWMutex
	servers  []discoverypkg.ServerInfo
	lastSync time.Time

	// Lifecycle management
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// RegistryConfig contains configuration for the server registry.
type RegistryConfig struct {
	RefreshInterval time.Duration // How often to refresh server list from discovery
	CacheTimeout    time.Duration // How long cached servers are valid
}

// DefaultConfig returns sensible defaults for registry configuration.
func DefaultConfig() RegistryConfig {
	return RegistryConfig{
		RefreshInterval: 30 * time.Second,
		CacheTimeout:    5 * time.Minute,
	}
}

// NewServerRegistry creates a new server registry.
func NewServerRegistry(discProv DiscoveryProvider, rbacProvider rbac.Provider, logger *slog.Logger) *ServerRegistry {
	ctx, cancel := context.WithCancel(context.Background())

	return &ServerRegistry{
		discoveryProv: discProv,
		rbacProvider:  rbacProvider,
		logger:        logger,
		servers:       make([]discoverypkg.ServerInfo, 0),
		ctx:           ctx,
		cancel:        cancel,
		done:          make(chan struct{}),
	}
}

// Start initializes the registry and begins periodic server discovery.
func (r *ServerRegistry) Start(ctx context.Context) error {
	r.logger.Info("starting server registry")

	// Perform initial server discovery
	if err := r.refresh(); err != nil {
		r.logger.Error("initial server discovery failed", "error", err)
		// Don't fail startup on discovery errors - continue with empty list
	}

	// Start background refresh goroutine
	config := DefaultConfig()
	go r.refreshLoop(config.RefreshInterval)

	r.logger.Info("server registry started successfully",
		"refresh_interval", config.RefreshInterval)
	return nil
}

// Stop gracefully shuts down the registry.
func (r *ServerRegistry) Stop() error {
	r.logger.Info("stopping server registry")

	r.cancel()

	// Wait for refresh goroutine to exit
	select {
	case <-r.done:
		r.logger.Info("server registry stopped successfully")
	case <-time.After(5 * time.Second):
		r.logger.Warn("server registry stop timeout - forcing shutdown")
	}

	return nil
}

// GetServersForUser returns the list of servers accessible to the given user.
// This is the main method called by the MCP resource handler.
func (r *ServerRegistry) GetServersForUser(ctx context.Context, userToken string) ([]discoverypkg.ServerInfo, error) {
	// Get current server list (with caching)
	servers, err := r.getServers(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get servers: %w", err)
	}

	// Convert to RBAC ServerInfo format
	rbacServers := make([]rbac.ServerInfo, len(servers))
	for i, srv := range servers {
		rbacServers[i] = rbac.ServerInfo{
			ID:      srv.ID,
			Address: srv.Address,
			Prefix:  srv.Prefix,
			Labels:  srv.Labels,
		}
	}

	// Apply RBAC filtering
	filteredRBACServers, err := r.rbacProvider.FilterServers(ctx, userToken, rbacServers)
	if err != nil {
		r.logger.Error("RBAC filtering failed", "error", err, "user_token_present", userToken != "")
		return nil, fmt.Errorf("authorization failed: %w", err)
	}

	// Convert back to discovery ServerInfo format
	result := make([]discoverypkg.ServerInfo, len(filteredRBACServers))
	for i, rbacSrv := range filteredRBACServers {
		result[i] = discoverypkg.ServerInfo{
			ID:      rbacSrv.ID,
			Address: rbacSrv.Address,
			Prefix:  rbacSrv.Prefix,
			Labels:  rbacSrv.Labels,
		}
	}

	r.logger.Debug("returning filtered servers for user",
		"total_servers", len(servers),
		"filtered_servers", len(result),
		"user_token_present", userToken != "")

	return result, nil
}

// GetAllServers returns all discovered servers without RBAC filtering.
// Used for internal monitoring/debugging.
func (r *ServerRegistry) GetAllServers(ctx context.Context) ([]discoverypkg.ServerInfo, error) {
	return r.getServers(ctx)
}

// getServers returns cached servers or refreshes from discovery if needed.
func (r *ServerRegistry) getServers(ctx context.Context) ([]discoverypkg.ServerInfo, error) {
	r.mu.RLock()

	// Check if cache is still valid
	config := DefaultConfig()
	if time.Since(r.lastSync) < config.CacheTimeout && len(r.servers) > 0 {
		servers := make([]discoverypkg.ServerInfo, len(r.servers))
		copy(servers, r.servers)
		r.mu.RUnlock()

		r.logger.Debug("returning cached servers", "server_count", len(servers))
		return servers, nil
	}

	r.mu.RUnlock()

	// Cache expired or empty - refresh from discovery
	r.logger.Debug("server cache expired - refreshing from discovery")
	if err := r.refresh(); err != nil {
		return nil, fmt.Errorf("failed to refresh servers: %w", err)
	}

	r.mu.RLock()
	servers := make([]discoverypkg.ServerInfo, len(r.servers))
	copy(servers, r.servers)
	r.mu.RUnlock()

	return servers, nil
}

// refresh fetches the latest server list from discovery and updates the cache.
func (r *ServerRegistry) refresh() error {
	r.logger.Debug("refreshing server list from discovery")

	servers, err := r.discoveryProv.GetServers()
	if err != nil {
		return fmt.Errorf("discovery GetServers failed: %w", err)
	}

	r.mu.Lock()
	r.servers = servers
	r.lastSync = time.Now()
	r.mu.Unlock()

	r.logger.Info("server list refreshed",
		"server_count", len(servers),
		"servers", r.serverNames(servers))

	return nil
}

// refreshLoop runs periodic server discovery in the background.
func (r *ServerRegistry) refreshLoop(interval time.Duration) {
	defer close(r.done)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	r.logger.Debug("starting server refresh loop", "interval", interval)

	for {
		select {
		case <-r.ctx.Done():
			r.logger.Debug("refresh loop shutting down")
			return
		case <-ticker.C:
			if err := r.refresh(); err != nil {
				r.logger.Error("periodic server refresh failed", "error", err)
				// Continue running despite errors
			}
		}
	}
}

// serverNames extracts server names for logging.
func (r *ServerRegistry) serverNames(servers []discoverypkg.ServerInfo) []string {
	names := make([]string, len(servers))
	for i, srv := range servers {
		names[i] = srv.ID
	}
	return names
}

// GetServerByID returns a specific server by ID, with RBAC checks.
func (r *ServerRegistry) GetServerByID(ctx context.Context, userToken, serverID string) (*discoverypkg.ServerInfo, error) {
	servers, err := r.GetServersForUser(ctx, userToken)
	if err != nil {
		return nil, err
	}

	for _, srv := range servers {
		if srv.ID == serverID {
			return &srv, nil
		}
	}

	return nil, fmt.Errorf("server %q not found or not authorized", serverID)
}

// GetStats returns registry statistics for monitoring.
func (r *ServerRegistry) GetStats() map[string]interface{} {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return map[string]interface{}{
		"total_servers": len(r.servers),
		"last_sync":     r.lastSync,
		"cache_age":     time.Since(r.lastSync).String(),
	}
}
