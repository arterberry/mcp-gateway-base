// internal/discovery/k8s.go
package discovery

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// ServerInfo represents a discovered MCP server.
type ServerInfo struct {
	ID      string            // Server identifier
	Address string            // HTTP endpoint URL
	Prefix  string            // Tool name prefix for namespacing
	Labels  map[string]string // Kubernetes labels for RBAC/filtering
}

// DiscoveryProvider defines the interface for server discovery.
type DiscoveryProvider interface {
	Start() error
	Stop() error
	GetServers() ([]ServerInfo, error)
}

// K8sDiscoveryProvider discovers MCP servers from Kubernetes services.
type K8sDiscoveryProvider struct {
	clientset    *kubernetes.Clientset
	namespace    string
	selector     string
	pollInterval time.Duration
	logger       *slog.Logger

	// Server cache and synchronization
	mu      sync.RWMutex
	servers map[string]*ServerInfo

	// Lifecycle management
	stopCh   chan struct{}
	stopOnce sync.Once
}

// Validation patterns for ID and prefix sanitization
var (
	// IDs and prefixes must match: ^[A-Za-z0-9_-]{1,64}$ for IDs, {1,32}$ for prefixes
	idSanitizeRE     = regexp.MustCompile(`[^A-Za-z0-9_-]`)
	prefixSanitizeRE = regexp.MustCompile(`[^A-Za-z0-9_-]`)
)

// NewK8sDiscoveryProvider creates a new Kubernetes-based discovery provider.
func NewK8sDiscoveryProvider(namespace, selector string, pollInterval time.Duration, inCluster bool, logger *slog.Logger) (DiscoveryProvider, error) {
	var cfg *rest.Config
	var err error

	if inCluster {
		cfg, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
		}
	} else {
		// For local development - would need kubeconfig support
		cfg, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to get kubeconfig (only in-cluster supported): %w", err)
		}
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes clientset: %w", err)
	}

	return &K8sDiscoveryProvider{
		clientset:    clientset,
		namespace:    namespace,
		selector:     selector,
		pollInterval: pollInterval,
		logger:       logger,
		servers:      make(map[string]*ServerInfo),
		stopCh:       make(chan struct{}),
	}, nil
}

// Start begins watching for Kubernetes service changes.
func (p *K8sDiscoveryProvider) Start() error {
	p.logger.Info("starting Kubernetes discovery",
		"namespace", p.namespace,
		"selector", p.selector)

	// Create shared informer factory
	factory := informers.NewSharedInformerFactoryWithOptions(
		p.clientset,
		0, // No periodic resync - event-driven only
		informers.WithNamespace(p.namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			if p.selector != "" {
				opts.LabelSelector = p.selector
			}
		}),
	)

	// Create service informer
	serviceInformer := factory.Core().V1().Services().Informer()

	// Add event handlers
	serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    p.onServiceAdd,
		UpdateFunc: p.onServiceUpdate,
		DeleteFunc: p.onServiceDelete,
	})

	// Start the informer
	factory.Start(p.stopCh)

	// Wait for caches to sync
	if !cache.WaitForCacheSync(p.stopCh, serviceInformer.HasSynced) {
		return fmt.Errorf("failed to sync Kubernetes service cache")
	}

	p.logger.Info("Kubernetes discovery started successfully")
	return nil
}

// Stop gracefully shuts down the discovery provider.
func (p *K8sDiscoveryProvider) Stop() error {
	p.logger.Info("stopping Kubernetes discovery")
	
	p.stopOnce.Do(func() {
		close(p.stopCh)
	})
	
	p.logger.Info("Kubernetes discovery stopped")
	return nil
}

// GetServers returns the current list of discovered servers.
func (p *K8sDiscoveryProvider) GetServers() ([]ServerInfo, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	servers := make([]ServerInfo, 0, len(p.servers))
	for _, server := range p.servers {
		servers = append(servers, *server)
	}

	p.logger.Debug("returning discovered servers", "count", len(servers))
	return servers, nil
}

// Event handlers for Kubernetes service changes

func (p *K8sDiscoveryProvider) onServiceAdd(obj interface{}) {
	service, ok := obj.(*corev1.Service)
	if !ok {
		p.logger.Warn("received non-service object in add handler")
		return
	}

	serverInfo := p.extractMCPServerInfo(service)
	if serverInfo == nil {
		return // Service doesn't expose MCP server
	}

	p.mu.Lock()
	p.servers[serverInfo.ID] = serverInfo
	p.mu.Unlock()

	p.logger.Info("discovered new MCP server",
		"server_id", serverInfo.ID,
		"address", serverInfo.Address,
		"prefix", serverInfo.Prefix)
}

func (p *K8sDiscoveryProvider) onServiceUpdate(oldObj, newObj interface{}) {
	newService, ok := newObj.(*corev1.Service)
	if !ok {
		p.logger.Warn("received non-service object in update handler")
		return
	}

	serverInfo := p.extractMCPServerInfo(newService)
	if serverInfo == nil {
		// Service no longer exposes MCP server - remove it
		p.onServiceDelete(newObj)
		return
	}

	p.mu.Lock()
	p.servers[serverInfo.ID] = serverInfo
	p.mu.Unlock()

	p.logger.Debug("updated MCP server",
		"server_id", serverInfo.ID,
		"address", serverInfo.Address)
}

func (p *K8sDiscoveryProvider) onServiceDelete(obj interface{}) {
	service, ok := obj.(*corev1.Service)
	if !ok {
		// Handle tombstone objects
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			if s, ok := tombstone.Obj.(*corev1.Service); ok {
				service = s
			} else {
				p.logger.Warn("received invalid tombstone object")
				return
			}
		} else {
			p.logger.Warn("received non-service object in delete handler")
			return
		}
	}

	// Extract server info to get the ID for removal
	serverInfo := p.extractMCPServerInfo(service)
	if serverInfo == nil {
		return
	}

	p.mu.Lock()
	delete(p.servers, serverInfo.ID)
	p.mu.Unlock()

	p.logger.Info("removed MCP server", "server_id", serverInfo.ID)
}

// extractMCPServerInfo implements your detailed annotation/label hierarchy.
func (p *K8sDiscoveryProvider) extractMCPServerInfo(svc *corev1.Service) *ServerInfo {
	ann := svc.Annotations
	if ann == nil {
		ann = make(map[string]string)
	}
	lbl := svc.Labels
	if lbl == nil {
		lbl = make(map[string]string)
	}

	// Extract ID with fallback chain
	id := p.extractID(ann, lbl, svc.Name)
	if id == "" {
		p.logger.Debug("service has no valid MCP server ID", "service", svc.Name)
		return nil
	}

	// Extract prefix with fallback chain
	prefix := p.extractPrefix(ann, lbl, id)

	// Extract or construct address
	address := p.extractAddress(ann, svc)
	if address == "" {
		p.logger.Debug("service has no valid address", "service", svc.Name)
		return nil
	}

	return &ServerInfo{
		ID:      id,
		Address: address,
		Prefix:  prefix,
		Labels:  lbl, // Include all labels for RBAC decisions
	}
}

// extractID implements ID extraction with annotation → label → fallback hierarchy.
func (p *K8sDiscoveryProvider) extractID(ann, lbl map[string]string, serviceName string) string {
	// Priority order for ID
	candidates := []string{
		ann["mcp.gateway/id"],
		ann["mcp.mark3labs.io/id"],
		lbl["app.kubernetes.io/instance"],
		lbl["app.kubernetes.io/name"],
		serviceName,
	}

	for _, candidate := range candidates {
		if candidate != "" {
			return sanitizeID(candidate)
		}
	}

	return ""
}

// extractPrefix implements prefix extraction with annotation → label → fallback hierarchy.
func (p *K8sDiscoveryProvider) extractPrefix(ann, lbl map[string]string, id string) string {
	// Priority order for prefix
	candidates := []string{
		ann["mcp.gateway/prefix"],
		ann["mcp.mark3labs.io/prefix"],
		lbl["mcp-prefix"],
		lbl["app.kubernetes.io/name"],
		strings.ToLower(id), // Fallback to lowercased ID
	}

	for _, candidate := range candidates {
		if candidate != "" {
			return sanitizePrefix(candidate)
		}
	}

	return sanitizePrefix(id) // Should never be empty if ID is valid
}

// extractAddress implements address extraction with annotation override or construction.
func (p *K8sDiscoveryProvider) extractAddress(ann map[string]string, svc *corev1.Service) string {
	// Check for full address annotation override
	if fullAddr := ann["mcp.gateway/address"]; fullAddr != "" {
		return fullAddr
	}

	// Construct address from components
	scheme := firstNonEmpty(ann["mcp.gateway/scheme"], "http")
	host := p.extractHost(ann, svc)
	if host == "" {
		return ""
	}
	port := p.extractPort(ann, svc)
	if port == "" {
		return ""
	}
	path := firstNonEmpty(ann["mcp.gateway/path"], "/mcp")

	return fmt.Sprintf("%s://%s:%s%s", scheme, host, port, path)
}

// extractHost determines the hostname, preferring LoadBalancer ingress over cluster DNS.
func (p *K8sDiscoveryProvider) extractHost(ann map[string]string, svc *corev1.Service) string {
	// Check for LoadBalancer ingress
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		ingress := svc.Status.LoadBalancer.Ingress[0]
		if ingress.Hostname != "" {
			return ingress.Hostname
		}
		if ingress.IP != "" {
			return ingress.IP
		}
	}

	// Fallback to cluster DNS
	return fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace)
}

// extractPort determines the port, with annotation override or smart detection.
func (p *K8sDiscoveryProvider) extractPort(ann map[string]string, svc *corev1.Service) string {
	// Check for port annotation override
	if portOverride := ann["mcp.gateway/port"]; portOverride != "" {
		return portOverride
	}

	// Smart port detection: prefer named ports
	preferredNames := []string{"mcp", "http", "https"}
	for _, name := range preferredNames {
		for _, port := range svc.Spec.Ports {
			if port.Name == name {
				return fmt.Sprintf("%d", port.Port)
			}
		}
	}

	// Fallback to first TCP port
	for _, port := range svc.Spec.Ports {
		if port.Protocol == corev1.ProtocolTCP || port.Protocol == "" {
			return fmt.Sprintf("%d", port.Port)
		}
	}

	return ""
}

// Utility functions

// sanitizeID ensures ID matches ^[A-Za-z0-9_-]{1,64}$
func sanitizeID(s string) string {
	s = idSanitizeRE.ReplaceAllString(s, "_")
	if len(s) > 64 {
		s = s[:64]
	}
	if len(s) == 0 {
		return ""
	}
	return s
}

// sanitizePrefix ensures prefix matches ^[A-Za-z0-9_-]{1,32}$
func sanitizePrefix(s string) string {
	s = prefixSanitizeRE.ReplaceAllString(s, "_")
	if len(s) > 32 {
		s = s[:32]
	}
	if len(s) == 0 {
		return ""
	}
	return s
}

// firstNonEmpty returns the first non-empty string from the arguments.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}