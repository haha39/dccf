package factory

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Loader provides methods to load and validate the configuration.
type Loader interface {
	Load(path string) (*Config, error)
}

// DefaultLoader is a simple YAML file loader/validator with defaults.
type DefaultLoader struct{}

// Load reads YAML from the given path, applies defaults, and validates.
func (l *DefaultLoader) Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal yaml: %w", err)
	}
	applyDefaults(&cfg)
	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("validate config: %w", err)
	}
	return &cfg, nil
}
