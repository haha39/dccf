// Package logger provides structured loggers for different components of DCCF.
// It wraps logrus and exposes category-specific log entries such as MainLog,
// CfgLog, SouthboundLog, etc. The logging level and caller reporting can be
// adjusted at runtime via InitLog.
package logger

import (
	"fmt"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

const (
	moduleNameDCCF = "DCCF"
)

var (
	initOnce sync.Once

	// MainLog is the primary logger for high-level lifecycle events
	// (startup, shutdown, major state transitions).
	MainLog *log.Entry

	// CfgLog is used for configuration loading, validation, and printing.
	CfgLog *log.Entry

	// SouthboundLog is for interactions with UPF EES (receiving Notify, errors, etc.).
	SouthboundLog *log.Entry

	// NorthboundLog is for Ndccf-like APIs exposed to AnLF/MTLF.
	NorthboundLog *log.Entry

	// StorageLog is for persistence-related logs (memory/Mongo/TSDB, etc.).
	StorageLog *log.Entry

	// AggregatorLog is for interval alignment, merge, deduplication, and slicing logic.
	AggregatorLog *log.Entry

	// SchedulerLog is for periodic tasks such as northbound push ticks.
	SchedulerLog *log.Entry

	// ContextLog is for runtime context changes (UPF subscription states, NB subs, shutdown flags).
	ContextLog *log.Entry

	// SbiLog is for generic SBA/HTTP client-server interactions that do not fit
	// more specific categories (e.g., UPF subscribe/unsubscribe).
	SbiLog *log.Entry
)

// InitLog configures the global logrus settings and initializes all category
// loggers. It is safe to call multiple times; the first call wins.
// Subsequent calls will update the log level and reportCaller flag.
func InitLog(levelString string, reportCaller bool) error {
	var initErr error

	initOnce.Do(func() {
		// Global formatter settings
		log.SetFormatter(&log.TextFormatter{
			FullTimestamp:   true,
			TimestampFormat: "2006-01-02T15:04:05.000Z07:00",
		})

		// Initialize category loggers with default level (info).
		log.SetLevel(log.InfoLevel)
		log.SetReportCaller(reportCaller)

		MainLog = log.WithFields(log.Fields{
			"module":   moduleNameDCCF,
			"category": "MAIN",
		})
		CfgLog = log.WithFields(log.Fields{
			"module":   moduleNameDCCF,
			"category": "CFG",
		})
		SouthboundLog = log.WithFields(log.Fields{
			"module":   moduleNameDCCF,
			"category": "SOUTHBOUND",
		})
		NorthboundLog = log.WithFields(log.Fields{
			"module":   moduleNameDCCF,
			"category": "NORTHBOUND",
		})
		StorageLog = log.WithFields(log.Fields{
			"module":   moduleNameDCCF,
			"category": "STORAGE",
		})
		AggregatorLog = log.WithFields(log.Fields{
			"module":   moduleNameDCCF,
			"category": "AGGREGATOR",
		})
		SchedulerLog = log.WithFields(log.Fields{
			"module":   moduleNameDCCF,
			"category": "SCHEDULER",
		})
		ContextLog = log.WithFields(log.Fields{
			"module":   moduleNameDCCF,
			"category": "CONTEXT",
		})
		SbiLog = log.WithFields(log.Fields{
			"module":   moduleNameDCCF,
			"category": "SBI",
		})
	})

	// Parse and apply the requested log level on every call.
	parsedLevel, parseErr := parseLogLevel(levelString)
	if parseErr != nil {
		// Fallback to info if parsing fails, but still return an error
		log.SetLevel(log.InfoLevel)
		if CfgLog != nil {
			CfgLog.Warnf("invalid log level %q, falling back to info: %v", levelString, parseErr)
		}
		initErr = parseErr
	} else {
		log.SetLevel(parsedLevel)
	}

	// Update report caller according to the latest configuration.
	log.SetReportCaller(reportCaller)

	return initErr
}

// parseLogLevel converts a string log level (case-insensitive) into a logrus.Level.
func parseLogLevel(levelString string) (log.Level, error) {
	normalized := strings.ToLower(strings.TrimSpace(levelString))

	switch normalized {
	case "trace":
		return log.TraceLevel, nil
	case "debug":
		return log.DebugLevel, nil
	case "info":
		return log.InfoLevel, nil
	case "warn", "warning":
		return log.WarnLevel, nil
	case "error":
		return log.ErrorLevel, nil
	case "fatal":
		return log.FatalLevel, nil
	case "panic":
		return log.PanicLevel, nil
	default:
		return log.InfoLevel, fmt.Errorf("unknown log level: %s", levelString)
	}
}
