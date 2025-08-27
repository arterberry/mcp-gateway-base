// internal/config/config.go
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v2"
)

// Load reads and parses configuration from the given file path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file at %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse YAML config: %w", err)
	}

	// Apply defaults and validate
	if err := setDefaults(&cfg); err != nil {
		return nil, fmt.Errorf("failed to apply defaults: %w", err)
	}

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &cfg, nil
}

// setDefaults applies default values to configuration.
func setDefaults(cfg *Config) error {
	// Gateway defaults
	if cfg.Gateway.Name == "" {
		cfg.Gateway.Name = "mcp-gateway"
	}
	if cfg.Gateway.Version == "" {
		cfg.Gateway.Version = "1.0.0"
	}
	if cfg.Gateway.Environment == "" {
		cfg.Gateway.Environment = "kubernetes"
	}

	// Kubernetes defaults
	if cfg.Kubernetes.Namespace == "" {
		cfg.Kubernetes.Namespace = "mcp-services"
	}
	if cfg.Kubernetes.LabelSelector == "" {
		cfg.Kubernetes.LabelSelector = "mcp-server=enabled"
	}
	if cfg.Kubernetes.PollInterval == 0 {
		cfg.Kubernetes.PollInterval = 30 * time.Second
	}
	// InCluster defaults to true for Kubernetes deployment
	if cfg.Gateway.Environment == "kubernetes" {
		cfg.Kubernetes.InCluster = true
	}

	// Transport defaults
	if cfg.Transport.Mode == "" {
		cfg.Transport.Mode = "http"
	}
	if cfg.Transport.Addr == "" {
		cfg.Transport.Addr = ":8081"
	}

	// Health defaults
	if cfg.Health.Port == 0 {
		cfg.Health.Port = 8085
	}

	return nil
}

// validate checks configuration for required values and constraints.
func validate(cfg *Config) error {
	// Validate environment
	if cfg.Gateway.Environment != "kubernetes" {
		return fmt.Errorf("only 'kubernetes' environment is supported, got: %q", cfg.Gateway.Environment)
	}

	// Validate transport
	if cfg.Transport.Mode != "http" {
		return fmt.Errorf("only 'http' transport is supported, got: %q", cfg.Transport.Mode)
	}

	// Validate required fields
	if cfg.Gateway.Name == "" {
		return fmt.Errorf("gateway.name is required")
	}
	if cfg.Gateway.Version == "" {
		return fmt.Errorf("gateway.version is required")
	}
	if cfg.Kubernetes.Namespace == "" {
		return fmt.Errorf("kubernetes.namespace is required")
	}
	if cfg.Kubernetes.LabelSelector == "" {
		return fmt.Errorf("kubernetes.label_selector is required")
	}
	if cfg.Transport.Addr == "" {
		return fmt.Errorf("transport.addr is required")
	}

	// Validate port ranges
	if cfg.Health.Port <= 0 || cfg.Health.Port > 65535 {
		return fmt.Errorf("health.port must be between 1 and 65535, got: %d", cfg.Health.Port)
	}

	return nil
}