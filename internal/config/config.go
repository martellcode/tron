package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config holds the application configuration
type Config struct {
	// TronDir is the base ~/.tron directory
	TronDir string

	// WorkingDir is where agents can read/write files
	WorkingDir string

	// AgentsDir is where agent status/logs are stored
	AgentsDir string

	// ConfigFile is the path to the vega config file
	ConfigFile string

	// Port is the server port
	Port int
}

// DefaultTronDir returns the default tron directory (~/.tron)
func DefaultTronDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".tron"
	}
	return filepath.Join(home, ".tron")
}

// Load loads configuration from ~/.tron directory
func Load() (*Config, error) {
	tronDir := DefaultTronDir()

	// Load config file from ~/.tron/config if it exists
	configFile := filepath.Join(tronDir, "config")
	if err := loadEnvFile(configFile); err != nil {
		// Not fatal - config is optional
		fmt.Printf("Note: Could not load %s: %v\n", configFile, err)
	}

	// Also check current directory for .env (for development)
	if err := loadEnvFile(".env"); err == nil {
		// Loaded successfully
	}

	cfg := &Config{
		TronDir: tronDir,
	}

	// Working directory - check env var, fall back to ~/.tron/workspace
	cfg.WorkingDir = os.Getenv("WORKING_DIR")
	if cfg.WorkingDir == "" {
		cfg.WorkingDir = filepath.Join(tronDir, "workspace")
	}

	// Agents directory - check env var, fall back to ~/.tron/agents
	cfg.AgentsDir = os.Getenv("AGENTS_DIR")
	if cfg.AgentsDir == "" {
		cfg.AgentsDir = filepath.Join(tronDir, "agents")
	}

	// Config file - look in ~/.tron first, then current directory
	cfg.ConfigFile = findConfigFile(tronDir)

	// Port
	cfg.Port = 3000
	if portStr := os.Getenv("PORT"); portStr != "" {
		fmt.Sscanf(portStr, "%d", &cfg.Port)
	}

	// Ensure directories exist
	if err := os.MkdirAll(cfg.WorkingDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create working directory %s: %w", cfg.WorkingDir, err)
	}
	if err := os.MkdirAll(cfg.AgentsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create agents directory %s: %w", cfg.AgentsDir, err)
	}

	return cfg, nil
}

// findConfigFile looks for tron.vega.yaml in standard locations
func findConfigFile(tronDir string) string {
	// Priority: 1) ~/.tron/tron.vega.yaml, 2) ./tron.vega.yaml
	locations := []string{
		filepath.Join(tronDir, "tron.vega.yaml"),
		"tron.vega.yaml",
	}

	for _, loc := range locations {
		if _, err := os.Stat(loc); err == nil {
			return loc
		}
	}

	// Return default even if not found (will error later)
	return filepath.Join(tronDir, "tron.vega.yaml")
}

// loadEnvFile loads environment variables from a file
func loadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Parse KEY=value
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// Remove surrounding quotes if present
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		// Only set if not already set (existing env vars take precedence)
		if os.Getenv(key) == "" {
			os.Setenv(key, value)
		}
	}

	return scanner.Err()
}

// KnowledgeDir returns the path to the knowledge directory
func (c *Config) KnowledgeDir() string {
	return filepath.Join(c.TronDir, "knowledge")
}

// ProjectsDir returns the path to the projects directory within workspace
func (c *Config) ProjectsDir() string {
	return filepath.Join(c.WorkingDir, "projects")
}
