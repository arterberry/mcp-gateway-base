// internal/rbac/provider.go
package rbac

import (
	"context"
	"log/slog"
)

// ServerInfo represents a discovered MCP server for RBAC filtering.
type ServerInfo struct {
	ID      string            // Server identifier
	Address string            // Server HTTP endpoint
	Prefix  string            // Tool prefix for namespacing
	Labels  map[string]string // Kubernetes labels for RBAC decisions
}

// Provider defines the interface for RBAC authorization of MCP servers.
type Provider interface {
	// FilterServers returns the subset of servers that the user is authorized to access.
	// userToken should be extracted from request headers/context (JWT, API key, etc.)
	// Returns filtered server list or error if authorization fails.
	FilterServers(ctx context.Context, userToken string, servers []ServerInfo) ([]ServerInfo, error)

	// IsAuthorized checks if a user can access a specific server.
	// Useful for individual server access checks.
	IsAuthorized(ctx context.Context, userToken string, serverID string) (bool, error)

	// GetUserPermissions returns the user's permission context for logging/debugging.
	// Returns empty map for NoOp provider.
	GetUserPermissions(ctx context.Context, userToken string) (map[string]interface{}, error)
}

// NoOpProvider implements Provider with no authorization - allows all access.
// This is a placeholder until real RBAC is implemented.
type NoOpProvider struct {
	logger *slog.Logger
}

// NewNoOpProvider creates a new no-operation RBAC provider.
func NewNoOpProvider(logger *slog.Logger) Provider {
	return &NoOpProvider{
		logger: logger,
	}
}

// FilterServers returns all servers without filtering (no authorization).
func (p *NoOpProvider) FilterServers(ctx context.Context, userToken string, servers []ServerInfo) ([]ServerInfo, error) {
	p.logger.Debug("RBAC filtering bypassed - returning all servers",
		"server_count", len(servers),
		"user_token_present", userToken != "")

	// Return all servers - no filtering applied
	return servers, nil
}

// IsAuthorized always returns true (no authorization checks).
func (p *NoOpProvider) IsAuthorized(ctx context.Context, userToken string, serverID string) (bool, error) {
	p.logger.Debug("RBAC authorization bypassed - allowing access",
		"server_id", serverID,
		"user_token_present", userToken != "")

	// Always allow access
	return true, nil
}

// GetUserPermissions returns empty permissions map for NoOp provider.
func (p *NoOpProvider) GetUserPermissions(ctx context.Context, userToken string) (map[string]interface{}, error) {
	p.logger.Debug("RBAC permissions bypassed - no restrictions",
		"user_token_present", userToken != "")

	// Return empty permissions - no restrictions
	return map[string]interface{}{
		"rbac_enabled": false,
		"access_level": "full",
		"restrictions": []string{},
	}, nil
}

// TODO: Future RBAC implementations could include:
//
// type JWTProvider struct {
//     secretKey []byte
//     logger    *slog.Logger
// }
//
// type RoleBasedProvider struct {
//     roleService RoleService
//     logger      *slog.Logger
// }
//
// These would implement the same Provider interface but with real
// authorization logic based on JWT tokens, role assignments, etc.
