package factory

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Config is the top-level configuration loaded from config/dccfcfg.yaml.
type Config struct {
	Info       InfoSection       `yaml:"info"`
	Southbound SouthboundSection `yaml:"southbound"`
	Northbound NorthboundSection `yaml:"northbound"`
	Storage    StorageSection    `yaml:"storage"`
	Security   SecuritySection   `yaml:"security"`
	NRF        NRFSection        `yaml:"nrf"`
	Logging    LoggingSection    `yaml:"logging"`
}

// ---------- info ----------

type InfoSection struct {
	Version     string `yaml:"version"`
	Description string `yaml:"description"`
}

// ---------- southbound (UPF → DCCF) ----------

type SouthboundSection struct {
	ListenAddr   string        `yaml:"listenAddr"`   // e.g. "0.0.0.0:8088"
	UpstreamUPFs []UPFEndpoint `yaml:"upstreamUpfs"` // UPFs to subscribe (direct)
}

type UPFEndpoint struct {
	ID         string `yaml:"id"`         // unique logical name, e.g. "upf-a"
	EESBaseURL string `yaml:"eesBaseUrl"` // e.g. "http://127.0.0.1:8088/nupf-ee/v1"
	PeriodSec  int    `yaml:"periodSec"`  // subscribe period toward this UPF
}

// ---------- northbound (DCCF → AnLF/MTLF) ----------

type NorthboundSection struct {
	ListenAddr        string `yaml:"listenAddr"`        // e.g. "0.0.0.0:8090"
	EnablePush        bool   `yaml:"enablePush"`        // enable push (subscription) mode
	DefaultPeriodSec  int    `yaml:"defaultPeriodSec"`  // publish cadence toward northbound consumers
	BatchMaxItems     int    `yaml:"batchMaxItems"`     // batch size for push
	DelayToleranceSec int    `yaml:"delayToleranceSec"` // acceptable delay for push aggregation
}

// ---------- storage ----------

type StorageSection struct {
	Driver  string `yaml:"driver"` // "memory" (MVP) | "mongo" | "tsdb" ...
	DSN     string `yaml:"dsn"`    // optional, driver-specific
	MaxItems int   `yaml:"maxItems,omitempty"`
	TTLSec   int   `yaml:"ttlSec,omitempty"`
}

// ---------- security ----------

type SecuritySection struct {
	EnableTLS bool   `yaml:"enableTLS"`
	// Future: certs / tokens / mTLS / oauth2 ...
}

// ---------- NRF ----------

type NRFSection struct {
	EnableDiscovery bool   `yaml:"enableDiscovery"`
	URI             string `yaml:"uri"`
}

// ---------- logging ----------

type LoggingSection struct {
	Level string `yaml:"level"` // "debug" | "info" | "warn" | "error"
}

// ---------- defaults ----------

func applyDefaults(cfg *Config) {
	// southbound
	if strings.TrimSpace(cfg.Southbound.ListenAddr) == "" {
		cfg.Southbound.ListenAddr = "0.0.0.0:8088"
	}
	// northbound
	if strings.TrimSpace(cfg.Northbound.ListenAddr) == "" {
		cfg.Northbound.ListenAddr = "0.0.0.0:8090"
	}
	if cfg.Northbound.DefaultPeriodSec <= 0 {
		cfg.Northbound.DefaultPeriodSec = 10
	}
	if cfg.Northbound.BatchMaxItems <= 0 {
		cfg.Northbound.BatchMaxItems = 500
	}
	if cfg.Northbound.DelayToleranceSec < 0 {
		cfg.Northbound.DelayToleranceSec = 0
	}
	// storage
	if strings.TrimSpace(cfg.Storage.Driver) == "" {
		cfg.Storage.Driver = "memory"
	}
	if cfg.Storage.MaxItems < 0 {
		cfg.Storage.MaxItems = 0
	}
	if cfg.Storage.TTLSec < 0 {
		cfg.Storage.TTLSec = 0
	}
	// logging
	if strings.TrimSpace(cfg.Logging.Level) == "" {
		cfg.Logging.Level = "debug"
	}
}

// ---------- validation helpers ----------

func isValidHostPort(hostport string) bool {
	// net.SplitHostPort requires a port; check first if it contains colon
	if !strings.Contains(hostport, ":") {
		return false
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	if strings.TrimSpace(host) == "" || strings.TrimSpace(port) == "" {
		return false
	}
	return true
}

func isValidBaseURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	return true
}

// ---------- Validate ----------

func validateConfig(cfg *Config) error {
	// southbound.listenAddr
	if !isValidHostPort(cfg.Southbound.ListenAddr) {
		return fmt.Errorf("southbound.listenAddr is invalid: %q", cfg.Southbound.ListenAddr)
	}
	// southbound.upstreamUpfs
	seen := make(map[string]struct{}, len(cfg.Southbound.UpstreamUPFs))
	for i, upf := range cfg.Southbound.UpstreamUPFs {
		if strings.TrimSpace(upf.ID) == "" {
			return fmt.Errorf("southbound.upstreamUpfs[%d].id is empty", i)
		}
		if _, ok := seen[upf.ID]; ok {
			return fmt.Errorf("southbound.upstreamUpfs[%d].id duplicated: %q", i, upf.ID)
		}
		seen[upf.ID] = struct{}{}

		if !isValidBaseURL(upf.EESBaseURL) {
			return fmt.Errorf("southbound.upstreamUpfs[%d].eesBaseUrl is invalid: %q", i, upf.EESBaseURL)
		}
		if upf.PeriodSec <= 0 {
			return fmt.Errorf("southbound.upstreamUpfs[%d].periodSec must be > 0", i)
		}
	}

	// northbound.listenAddr
	if !isValidHostPort(cfg.Northbound.ListenAddr) {
		return fmt.Errorf("northbound.listenAddr is invalid: %q", cfg.Northbound.ListenAddr)
	}
	if cfg.Northbound.DefaultPeriodSec <= 0 {
		return fmt.Errorf("northbound.defaultPeriodSec must be > 0")
	}
	if cfg.Northbound.BatchMaxItems <= 0 {
		return fmt.Errorf("northbound.batchMaxItems must be > 0")
	}
	if cfg.Northbound.DelayToleranceSec < 0 {
		return fmt.Errorf("northbound.delayToleranceSec must be >= 0")
	}

	// storage
	switch cfg.Storage.Driver {
	case "memory", "mongo", "tsdb":
	default:
		return fmt.Errorf("storage.driver unsupported: %q", cfg.Storage.Driver)
	}
	if cfg.Storage.Driver != "memory" && strings.TrimSpace(cfg.Storage.DSN) == "" {
		return fmt.Errorf("storage.dsn required for driver %q", cfg.Storage.Driver)
	}
	if cfg.Storage.MaxItems < 0 {
		return fmt.Errorf("storage.maxItems must be >= 0")
	}
	if cfg.Storage.TTLSec < 0 {
		return fmt.Errorf("storage.ttlSec must be >= 0")
	}

	// NRF
	if cfg.NRF.EnableDiscovery {
		if !isValidBaseURL(cfg.NRF.URI) {
			return fmt.Errorf("nrf.uri invalid (EnableDiscovery=true): %q", cfg.NRF.URI)
		}
	}

	// logging
	switch strings.ToLower(cfg.Logging.Level) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level unsupported: %q", cfg.Logging.Level)
	}
	return nil
}
