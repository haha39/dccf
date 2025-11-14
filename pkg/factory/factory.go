// pkg/factory/factory.go
//
// Package factory provides configuration loading and validation helpers
// for DCCF. It reads YAML files, applies defaults, validates using
// govalidator and additional semantic checks, and logs the resulting config.
package factory

import (
	"os"

	"github.com/asaskevich/govalidator"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"

	"github.com/free5gc/dccf/internal/logger"
)

// InitConfigFactory reads the configuration file from the given path into
// the provided Config structure. It does not perform validation.
func InitConfigFactory(configPath string, config *Config) error {
	if configPath == "" {
		configPath = DccfDefaultConfigPath
	}

	rawConfig, readError := os.ReadFile(configPath)
	if readError != nil {
		return errors.Errorf("failed to read config file [%s]: %+v", configPath, readError)
	}

	logger.CfgLog.Infof("Read config from [%s]", configPath)

	if unmarshalError := yaml.Unmarshal(rawConfig, config); unmarshalError != nil {
		return errors.Errorf("failed to unmarshal config file [%s]: %+v", configPath, unmarshalError)
	}

	return nil
}

// ReadConfig loads, applies defaults, validates, and prints the DCCF config.
// It is the main entry point used by cmd/main.go at startup.
func ReadConfig(configPath string) (*Config, error) {
	config := &Config{}

	if initError := InitConfigFactory(configPath, config); initError != nil {
		return nil, errors.Errorf("ReadConfig [%s] error: %+v", configPath, initError)
	}

	// Allow custom validation tags to be registered before validation.
	RegisterCustomValidators()

	// Apply default values before running tag-based validation.
	config.ApplyDefaults()

	// First pass: tag-based validation (required fields, enums, etc.).
	if _, validationError := govalidator.ValidateStruct(config); validationError != nil {
		logger.CfgLog.Errorf("[-- PLEASE REFER TO SAMPLE CONFIG FILE COMMENTS --]")
		return nil, errors.Errorf("config tag validation error: %+v", validationError)
	}

	// Second pass: semantic validation.
	if semanticError := config.Validate(); semanticError != nil {
		return nil, errors.Errorf("config semantic validation error: %+v", semanticError)
	}

	config.Print()
	return config, nil
}
