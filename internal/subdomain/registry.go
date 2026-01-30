// Package subdomain manages unique subdomain allocation and routing for project servers.
package subdomain

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
)

const (
	// Port range for project servers
	MinPort = 3001
	MaxPort = 3999

	// SubdomainLength is the length of generated subdomains
	SubdomainLength = 8

	// Domain suffix
	Domain = "hellotron.com"
)

// Registry manages subdomain-to-port mappings for project servers.
type Registry struct {
	mu sync.RWMutex

	// subdomain → port mapping
	subdomains map[string]int

	// port → subdomain reverse mapping
	ports map[int]string

	// project → subdomain mapping (for lookup by project name)
	projects map[string]string
}

// NewRegistry creates a new subdomain registry.
func NewRegistry() *Registry {
	return &Registry{
		subdomains: make(map[string]int),
		ports:      make(map[int]string),
		projects:   make(map[string]string),
	}
}

// Allocation represents an allocated subdomain and port.
type Allocation struct {
	Subdomain string
	Port      int
	URL       string
}

// Allocate assigns a unique subdomain and port for a project.
// Returns the allocation or an error if no ports are available.
func (r *Registry) Allocate(projectName string) (*Allocation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Check if project already has an allocation
	if subdomain, exists := r.projects[projectName]; exists {
		port := r.subdomains[subdomain]
		return &Allocation{
			Subdomain: subdomain,
			Port:      port,
			URL:       fmt.Sprintf("https://%s.%s", subdomain, Domain),
		}, nil
	}

	// Generate unique subdomain
	subdomain, err := r.generateUniqueSubdomain()
	if err != nil {
		return nil, fmt.Errorf("failed to generate subdomain: %w", err)
	}

	// Allocate port
	port, err := r.allocatePort()
	if err != nil {
		return nil, fmt.Errorf("failed to allocate port: %w", err)
	}

	// Store mappings
	r.subdomains[subdomain] = port
	r.ports[port] = subdomain
	r.projects[projectName] = subdomain

	return &Allocation{
		Subdomain: subdomain,
		Port:      port,
		URL:       fmt.Sprintf("https://%s.%s", subdomain, Domain),
	}, nil
}

// Release frees a subdomain and port allocation for a project.
func (r *Registry) Release(projectName string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	subdomain, exists := r.projects[projectName]
	if !exists {
		return
	}

	port := r.subdomains[subdomain]

	delete(r.projects, projectName)
	delete(r.subdomains, subdomain)
	delete(r.ports, port)
}

// GetBySubdomain returns the port for a subdomain.
func (r *Registry) GetBySubdomain(subdomain string) (int, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	port, exists := r.subdomains[subdomain]
	return port, exists
}

// GetByProject returns the allocation for a project.
func (r *Registry) GetByProject(projectName string) (*Allocation, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	subdomain, exists := r.projects[projectName]
	if !exists {
		return nil, false
	}

	port := r.subdomains[subdomain]
	return &Allocation{
		Subdomain: subdomain,
		Port:      port,
		URL:       fmt.Sprintf("https://%s.%s", subdomain, Domain),
	}, true
}

// IsValidSubdomain checks if a subdomain is registered.
func (r *Registry) IsValidSubdomain(subdomain string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, exists := r.subdomains[subdomain]
	return exists
}

// List returns all current allocations.
func (r *Registry) List() []Allocation {
	r.mu.RLock()
	defer r.mu.RUnlock()

	allocations := make([]Allocation, 0, len(r.subdomains))
	for subdomain, port := range r.subdomains {
		allocations = append(allocations, Allocation{
			Subdomain: subdomain,
			Port:      port,
			URL:       fmt.Sprintf("https://%s.%s", subdomain, Domain),
		})
	}
	return allocations
}

// generateUniqueSubdomain creates a cryptographically random subdomain.
func (r *Registry) generateUniqueSubdomain() (string, error) {
	// Use base32 encoding (lowercase, no padding) for URL-safe subdomains
	encoder := base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

	for attempts := 0; attempts < 100; attempts++ {
		// Generate random bytes (5 bytes = 8 base32 chars)
		b := make([]byte, 5)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}

		subdomain := encoder.EncodeToString(b)[:SubdomainLength]

		// Ensure uniqueness
		if _, exists := r.subdomains[subdomain]; !exists {
			return subdomain, nil
		}
	}

	return "", fmt.Errorf("failed to generate unique subdomain after 100 attempts")
}

// allocatePort finds an available port in the range.
func (r *Registry) allocatePort() (int, error) {
	for port := MinPort; port <= MaxPort; port++ {
		if _, used := r.ports[port]; used {
			continue
		}

		// Check if port is actually available on the system
		if isPortAvailable(port) {
			return port, nil
		}
	}

	return 0, fmt.Errorf("no available ports in range %d-%d", MinPort, MaxPort)
}

// isPortAvailable checks if a port is available for binding.
func isPortAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// Middleware returns an HTTP middleware that routes subdomain requests.
func (r *Registry) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host := req.Host

		// Strip port if present
		if idx := strings.LastIndex(host, ":"); idx != -1 {
			host = host[:idx]
		}

		// Check if this is a subdomain request
		if !strings.HasSuffix(host, "."+Domain) {
			next.ServeHTTP(w, req)
			return
		}

		// Extract subdomain
		subdomain := strings.TrimSuffix(host, "."+Domain)

		// Look up port - if subdomain isn't registered, pass through to main handler
		// This allows reserved subdomains like "api" to work normally
		port, exists := r.GetBySubdomain(subdomain)
		if !exists {
			next.ServeHTTP(w, req)
			return
		}

		// Reverse proxy to the project server
		target, _ := url.Parse(fmt.Sprintf("http://localhost:%d", port))
		proxy := httputil.NewSingleHostReverseProxy(target)

		// Set forwarding headers
		req.Header.Set("X-Forwarded-Host", req.Host)
		req.Header.Set("X-Forwarded-Proto", "https")

		proxy.ServeHTTP(w, req)
	})
}

// HandleCaddyAsk handles Caddy's on-demand TLS verification requests.
// Caddy calls this endpoint to check if a subdomain should get a certificate.
func (r *Registry) HandleCaddyAsk(w http.ResponseWriter, req *http.Request) {
	domain := req.URL.Query().Get("domain")
	if domain == "" {
		http.Error(w, "missing domain parameter", http.StatusBadRequest)
		return
	}

	// Check if it's a valid subdomain of hellotron.com
	if !strings.HasSuffix(domain, "."+Domain) {
		http.Error(w, "not a valid subdomain", http.StatusForbidden)
		return
	}

	subdomain := strings.TrimSuffix(domain, "."+Domain)

	// Check if this subdomain is registered
	if r.IsValidSubdomain(subdomain) {
		w.WriteHeader(http.StatusOK)
		return
	}

	http.Error(w, "subdomain not registered", http.StatusForbidden)
}
