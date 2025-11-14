// pkg/factory/config.go
//
// Package factory defines the configuration structures for DCCF and basic helpers
// such as printing and applying default values. The actual loading and validation
// logic lives in factory.go.
package factory

import (
	"fmt"

	"github.com/asaskevich/govalidator"
	"github.com/davecgh/go-spew/spew"

	"github.com/free5gc/dccf/internal/logger"
)

const (
	// DccfDefaultConfigPath is the default configuration file path.
	DccfDefaultConfigPath = "./config/dccfcfg.yaml"

	// DccfConfigExpectedVersion is the version string we expect to see in the YAML.
	// This can be bumped when incompatible config changes are introduced.
	DccfConfigExpectedVersion = "1.0.0"
)

// Config is the top-level configuration structure for DCCF.
// It is populated from config/dccfcfg.yaml.
type Config struct {
	Info       InfoConfig       `yaml:"info"       valid:"required"`
	Southbound SouthboundConfig `yaml:"southbound" valid:"required"`
	Northbound NorthboundConfig `yaml:"northbound" valid:"required"`
	Storage    StorageConfig    `yaml:"storage"    valid:"required"`
	Security   SecurityConfig   `yaml:"security"   valid:"optional"`
	NRF        NRFConfig        `yaml:"nrf"        valid:"optional"`
	Logging    LoggingConfig    `yaml:"logging"    valid:"required"`
}

// InfoConfig contains metadata about the running DCCF instance.
type InfoConfig struct {
	Version     string `yaml:"version"     valid:"required"`
	Description string `yaml:"description" valid:"optional"`
}

// SouthboundConfig controls how DCCF interacts with UPF EES (Nupf_EventExposure).
type SouthboundConfig struct {
	// ListenAddr is where DCCF receives UPF EES Notify requests.
	// Example: "0.0.0.0:8088"
	ListenAddr string `yaml:"listenAddr" valid:"required"`

	// UpstreamUPFs contains the list of UPFs to which DCCF will subscribe.
	UpstreamUPFs []UpstreamUPFConfig `yaml:"upstreamUpfs" valid:"required"`
}

// UpstreamUPFConfig describes one upstream UPF EES endpoint.
type UpstreamUPFConfig struct {
	// ID is a human-readable identifier for this UPF (e.g., "upf-a").
	ID string `yaml:"id" valid:"required"`

	// EESBaseURL is the base URL of the UPF EES API.
	// Example: "http://127.0.0.1:8088/nupf-ee/v1"
	EESBaseURL string `yaml:"eesBaseUrl" valid:"required"`

	// PeriodSec is the subscription interval (seconds) for this UPF.
	PeriodSec int `yaml:"periodSec" valid:"required"`
}

// NorthboundConfig controls the Ndccf-like services exposed to AnLF/MTLF.
type NorthboundConfig struct {
	// ListenAddr is where DCCF exposes its northbound REST API.
	// Example: "0.0.0.0:8090"
	ListenAddr string `yaml:"listenAddr" valid:"required"`

	// EnablePush controls whether DCCF will periodically push notifications
	// to subscribed AnLF/MTLF endpoints.
	EnablePush bool `yaml:"enablePush" valid:"optional"`

	// DefaultPeriodSec defines the default northbound push interval (seconds)
	// for subscriptions that do not explicitly specify a period.
	DefaultPeriodSec int `yaml:"defaultPeriodSec" valid:"required"`

	// BatchMaxItems is a soft limit on the number of items per notification batch.
	BatchMaxItems int `yaml:"batchMaxItems" valid:"optional"`

	// DelayToleranceSec indicates how much delay (seconds) is acceptable when
	// aligning and batching measurements before sending them northbound.
	DelayToleranceSec int `yaml:"delayToleranceSec" valid:"optional"`
}

// StorageConfig controls where and how DCCF stores collected measurements.
type StorageConfig struct {
	// Driver selects the storage backend.
	// MVP supports: "memory"; future: "mongo", "tsdb".
	Driver string `yaml:"driver" valid:"required,in(memory|mongo|tsdb)"`

	// MaxItems is an optional upper bound for in-memory backends.
	// Zero or negative means "no explicit limit".
	MaxItems int `yaml:"maxItems" valid:"optional"`

	// TTLSeconds is an optional time-to-live for stored items.
	// Zero or negative means "no auto-expiration".
	TTLSeconds int `yaml:"ttlSec" valid:"optional"`

	// DSN is an optional data source name for external backends (e.g., Mongo URI).
	DSN string `yaml:"dsn" valid:"optional"`
}

// SecurityConfig captures security-related knobs (TLS, tokens, etc.).
// MVP does not enforce them but keeps the shape ready for future work.
type SecurityConfig struct {
	EnableTLS bool   `yaml:"enableTLS" valid:"optional"`
	CertFile  string `yaml:"certFile"  valid:"optional"`
	KeyFile   string `yaml:"keyFile"   valid:"optional"`
	CAFile    string `yaml:"caFile"    valid:"optional"`
}

// NRFConfig contains parameters for optional Nnrf_NFDiscovery integration.
type NRFConfig struct {
	EnableDiscovery bool   `yaml:"enableDiscovery" valid:"optional"`
	URI             string `yaml:"uri"             valid:"optional"`
}

// LoggingConfig controls the log level and formatting.
type LoggingConfig struct {
	Level        string `yaml:"level"        valid:"required,in(trace|debug|info|warn|error|fatal|panic)"`
	ReportCaller bool   `yaml:"reportCaller" valid:"optional"`
}

// GetVersion returns the Info.Version field.
func (config *Config) GetVersion() string {
	return config.Info.Version
}

// Print outputs the entire configuration to the configuration logger.
// This is useful for debugging and confirming loaded values at startup.
func (config *Config) Print() {
	spew.Config.Indent = "\t"
	configDump := spew.Sdump(config)
	logger.CfgLog.Infof("==================================================")
	logger.CfgLog.Infof("%s", configDump)
	logger.CfgLog.Infof("==================================================")
}

// ApplyDefaults fills in reasonable defaults for fields that are left empty
// in the configuration file. It does not override explicitly set values.
func (config *Config) ApplyDefaults() {
	// Southbound defaults
	if config.Southbound.ListenAddr == "" {
		config.Southbound.ListenAddr = "0.0.0.0:8088"
	}

	// Northbound defaults
	if config.Northbound.ListenAddr == "" {
		config.Northbound.ListenAddr = "0.0.0.0:8090"
	}
	if config.Northbound.DefaultPeriodSec <= 0 {
		config.Northbound.DefaultPeriodSec = 10
	}
	if config.Northbound.BatchMaxItems <= 0 {
		config.Northbound.BatchMaxItems = 500
	}
	if config.Northbound.DelayToleranceSec < 0 {
		config.Northbound.DelayToleranceSec = 0
	}

	// Storage defaults
	if config.Storage.Driver == "" {
		config.Storage.Driver = "memory"
	}
}

// Validate performs additional semantic validation that goes beyond
// the struct tags used by govalidator. It should be called after
// ApplyDefaults and govalidator.ValidateStruct.
func (config *Config) Validate() error {
	if config.Info.Version != DccfConfigExpectedVersion {
		return fmt.Errorf("unsupported config version %q (expected %q)",
			config.Info.Version, DccfConfigExpectedVersion)
	}

	if len(config.Southbound.UpstreamUPFs) == 0 {
		return fmt.Errorf("southbound.upstreamUpfs must contain at least one UPF endpoint")
	}

	for index, upfEndpoint := range config.Southbound.UpstreamUPFs {
		if upfEndpoint.PeriodSec <= 0 {
			return fmt.Errorf("southbound.upstreamUpfs[%d].periodSec must be > 0", index)
		}
	}

	if config.Northbound.DefaultPeriodSec <= 0 {
		return fmt.Errorf("northbound.defaultPeriodSec must be > 0")
	}

	if config.Storage.Driver == "memory" {
		if config.Storage.MaxItems < 0 {
			return fmt.Errorf("storage.maxItems must be >= 0 for memory driver")
		}
		if config.Storage.TTLSeconds < 0 {
			return fmt.Errorf("storage.ttlSec must be >= 0 for memory driver")
		}
	}

	return nil
}

// RegisterCustomValidators allows us to extend govalidator with additional tags
// if needed. For now this is a placeholder in case we want tags like "cidr" or
// more specific URL validations.
func RegisterCustomValidators() {
	// Example: govalidator.TagMap["cidr"] = govalidator.Validator(func(str string) bool {
	// 	return govalidator.IsCIDR(str)
	// })
	_ = govalidator.TagMap
}
