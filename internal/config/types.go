// internal/config/types.go
package config

import "time"

// Config represents the complete application configuration.
type Config struct {
	Gateway    GatewayConfig    `yaml:"gateway"`
	Kubernetes KubernetesConfig `yaml:"kubernetes"`
	Transport  TransportConfig  `yaml:"transport"`
	Health     HealthConfig     `yaml:"health"`
}

// GatewayConfig contains core gateway settings.
type GatewayConfig struct {
	Name        string `yaml:"name"`
	Version     string `yaml:"version"`
	Environment string `yaml:"environment"` // Must be "kubernetes"
}

// KubernetesConfig contains Kubernetes-specific settings.
type KubernetesConfig struct {
	Namespace     string        `yaml:"namespace"`
	LabelSelector string        `yaml:"label_selector"`
	PollInterval  time.Duration `yaml:"poll_interval"`
	InCluster     bool          `yaml:"in_cluster"`
}

// TransportConfig defines MCP transport settings.
type TransportConfig struct {
	Mode string `yaml:"mode"` // Must be "http"
	Addr string `yaml:"addr"` // e.g., ":8081"
}

// HealthConfig contains health check server settings.
type HealthConfig struct {
	Port int `yaml:"port"` // e.g., 8085
}
