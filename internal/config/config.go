package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config represents the semerge configuration
type Config struct {
	Version  string         `yaml:"version,omitempty"`
	LLM      LLMConfig      `yaml:"llm"`
	Verifier VerifierConfig `yaml:"verifier"`
	Context  ContextConfig  `yaml:"context"`
	Git      GitConfig      `yaml:"git"`
}

// LLMConfig represents LLM configuration
type LLMConfig struct {
	Provider string `yaml:"provider"` // "claude" or "openai"
	APIKey   string `yaml:"api_key"`
	Model    string `yaml:"model"`
	Timeout  int    `yaml:"timeout"` // seconds
	BaseURL  string `yaml:"base_url,omitempty"`
}

// VerifierConfig represents verifier configuration
type VerifierConfig struct {
	Enabled bool              `yaml:"enabled"`
	Tools   map[string]string `yaml:"tools"` // language -> command
	Retries int               `yaml:"retries"`
}

// ContextConfig represents context extraction configuration
type ContextConfig struct {
	MaxLines       int  `yaml:"max_lines"`
	IncludeImports bool `yaml:"include_imports"`
	UseTreeSitter  bool `yaml:"use_tree_sitter"`
}

// GitConfig represents git-related configuration
type GitConfig struct {
	MergeStrategy string `yaml:"merge_strategy"` // "auto", "semantic", "text"
	FallbackText  bool   `yaml:"fallback_text"`
}

// Load loads configuration from a file
func Load(path string) (*Config, error) {
	// Read file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Parse YAML
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Set defaults
	setDefaults(&cfg)

	return &cfg, nil
}

// LoadOrDefault loads configuration from file or returns default config
func LoadOrDefault(path string) (*Config, error) {
	// Check if file exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// Return default config
		return DefaultConfig(), nil
	}

	return Load(path)
}

// Save saves configuration to a file
func (c *Config) Save(path string) error {
	// Create directory if needed
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Marshal to YAML
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Write file
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// DefaultConfig returns default configuration
func DefaultConfig() *Config {
	return &Config{
		Version: "v1",
		LLM: LLMConfig{
			Provider: "claude",
			APIKey:   "",
			Model:    "claude-3-5-sonnet-20241022",
			Timeout:  120,
			BaseURL:  "",
		},
		Verifier: VerifierConfig{
			Enabled: true,
			Tools:   GetDefaultTools(),
			Retries: 3,
		},
		Context: ContextConfig{
			MaxLines:       1000,
			IncludeImports: true,
			UseTreeSitter:  true,
		},
		Git: GitConfig{
			MergeStrategy: "semantic",
			FallbackText:  true,
		},
	}
}

// setDefaults sets default values for missing config options
func setDefaults(cfg *Config) {
	if cfg.Version == "" {
		cfg.Version = "v1"
	}

	// LLM defaults
	if cfg.LLM.Provider == "" {
		cfg.LLM.Provider = "claude"
	}
	if cfg.LLM.Model == "" {
		cfg.LLM.Model = "claude-3-5-sonnet-20241022"
	}
	if cfg.LLM.Timeout == 0 {
		cfg.LLM.Timeout = 120
	}

	// Verifier defaults
	if cfg.Verifier.Tools == nil {
		cfg.Verifier.Tools = GetDefaultTools()
	}
	if cfg.Verifier.Retries == 0 {
		cfg.Verifier.Retries = 3
	}

	// Context defaults
	if cfg.Context.MaxLines == 0 {
		cfg.Context.MaxLines = 1000
	}

	// Git defaults
	if cfg.Git.MergeStrategy == "" {
		cfg.Git.MergeStrategy = "semantic"
	}
}

// GetConfigPath returns the default config file path
func GetConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".semerge", "config.yaml"), nil
}

// GetProjectConfigPath returns the project-specific config path
func GetProjectConfigPath() string {
	return ".semerge.yaml"
}

// LoadConfig loads config from project, then user home, then defaults
func LoadConfig() (*Config, error) {
	// Try project config first
	projectCfg := GetProjectConfigPath()
	if cfg, err := LoadOrDefault(projectCfg); err == nil {
		return cfg, nil
	}

	// Try user config
	userCfg, err := GetConfigPath()
	if err != nil {
		return DefaultConfig(), nil
	}

	return LoadOrDefault(userCfg)
}

// GetDefaultTools returns default linter commands
func GetDefaultTools() map[string]string {
	return map[string]string{
		"go":         "go vet {file}",
		"python":     "ruff check {file}",
		"javascript": "eslint {file}",
		"typescript": "eslint {file}",
		"java":       "javac -Xlint:all {file}",
		"cpp":        "g++ -fsyntax-only -Wall -Werror {file}",
		"c":          "gcc -fsyntax-only -Wall -Werror {file}",
		"rust":       "rustc --crate-type lib {file}",
		"ruby":       "rubocop {file}",
		"php":        "php -l {file}",
	}
}

// Validate validates the configuration
func (c *Config) Validate() error {
	// Validate LLM config
	if c.LLM.Provider != "claude" && c.LLM.Provider != "openai" {
		return fmt.Errorf("invalid LLM provider: %s", c.LLM.Provider)
	}

	if c.LLM.APIKey == "" {
		// Check environment variable
		if c.LLM.Provider == "claude" {
			if os.Getenv("ANTHROPIC_API_KEY") == "" {
				return fmt.Errorf("ANTHROPIC_API_KEY not set")
			}
		} else if c.LLM.Provider == "openai" {
			if os.Getenv("OPENAI_API_KEY") == "" {
				return fmt.Errorf("OPENAI_API_KEY not set")
			}
		}
	}

	// Validate verifier config
	if c.Verifier.Retries < 0 {
		return fmt.Errorf("verifier retries cannot be negative")
	}

	// Validate context config
	if c.Context.MaxLines < 0 {
		return fmt.Errorf("context max lines cannot be negative")
	}

	// Validate git config
	validStrategies := map[string]bool{
		"auto":     true,
		"semantic": true,
		"text":     true,
	}
	if !validStrategies[c.Git.MergeStrategy] {
		return fmt.Errorf("invalid merge strategy: %s", c.Git.MergeStrategy)
	}

	return nil
}
