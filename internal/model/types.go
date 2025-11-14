// Package model defines shared data structures for DCCF, including:
// - Southbound payloads received from UPF EES (Nupf_EventExposure Notify)
// - Northbound payloads exposed to AnLF/MTLF (Ndccf-like Data Management)
// - Internal normalized representations reused by aggregator and storage.
//
// All types here are intentionally free of dependencies on other internal
// packages to avoid circular imports.
package model

import "time"

// EventType enumerates supported event identifiers.
// MVP only: USER_DATA_USAGE_MEASURES.
type EventType string

const (
	EventUserDataUsageMeasures EventType = "USER_DATA_USAGE_MEASURES"
)

// GranularityType enumerates supported measurement granularities.
// MVP only: perPduSession.
type GranularityType string

const (
	GranularityPerPduSession GranularityType = "perPduSession"
)

// ---------------------------------------------------------------------------
// Southbound (UPF → DCCF): UPF EES Notify payloads
// ---------------------------------------------------------------------------

// UpfUserDataUsageItem represents a single per-session measurement interval
// reported by the UPF EES. The interval is defined by [StartTime, EndTime].
//
// The semantics are "interval values", not cumulative counters:
// - ULBytes/DLBytes: number of bytes observed in this interval
// - ULPackets/DLPackets: number of packets observed in this interval
// UPF may optionally include derived throughputs.
type UpfUserDataUsageItem struct {
	LocalSEID       uint64    `json:"localSeid"`
	RemoteSEID      uint64    `json:"remoteSeid"`
	ULBytes         uint64    `json:"ulBytes"`
	DLBytes         uint64    `json:"dlBytes"`
	ULPackets       uint64    `json:"ulPackets"`
	DLPackets       uint64    `json:"dlPackets"`
	StartTime       time.Time `json:"startTime"`
	EndTime         time.Time `json:"endTime"`
	ULThroughputBps float64   `json:"ulThroughputBps,omitempty"`
	DLThroughputBps float64   `json:"dlThroughputBps,omitempty"`
}

// UpfEesNotify is the full southbound payload sent by UPF EES to DCCF.
// It mirrors the USER_DATA_USAGE_MEASURES Notify body defined at the UPF side.
type UpfEesNotify struct {
	SubscriptionID string                 `json:"subscriptionId"`
	EventID        EventType              `json:"eventId"`
	Granularity    GranularityType        `json:"granularity"`
	Timestamp      time.Time              `json:"timestamp"`
	Items          []UpfUserDataUsageItem `json:"items"`
}

// ---------------------------------------------------------------------------
// Internal normalized representation
// ---------------------------------------------------------------------------

// UsageMeasurement is the normalized representation used inside DCCF.
// It is derived from UpfUserDataUsageItem and enriched with SourceUPFID
// (which UPF produced this interval) so that multi-UPF deployments can be
// handled correctly in storage and aggregation.
type UsageMeasurement struct {
	SourceUPFID string `json:"sourceUpfId"`
	LocalSEID   uint64 `json:"localSeid"`
	RemoteSEID  uint64 `json:"remoteSeid"`

	ULBytes   uint64 `json:"ulBytes"`
	DLBytes   uint64 `json:"dlBytes"`
	ULPackets uint64 `json:"ulPackets"`
	DLPackets uint64 `json:"dlPackets"`

	StartTime time.Time `json:"startTime"`
	EndTime   time.Time `json:"endTime"`

	ULThroughputBps float64 `json:"ulThroughputBps,omitempty"`
	DLThroughputBps float64 `json:"dlThroughputBps,omitempty"`
}

// ---------------------------------------------------------------------------
// Northbound (AnLF/MTLF → DCCF): Ndccf-like subscription and fetch
// ---------------------------------------------------------------------------

// NdccfSubscriptionRequest represents a northbound request to create a
// subscription for periodic push notifications from DCCF to an AnLF/MTLF.
type NdccfSubscriptionRequest struct {
	// NotifURI is the HTTP endpoint where DCCF will push notifications.
	// If empty, the subscription is effectively disabled for push and
	// the client is expected to use fetch instead.
	NotifURI string `json:"notifUri,omitempty"`

	// Event identifies which type of measurements are requested.
	// MVP: "USER_DATA_USAGE_MEASURES".
	Event EventType `json:"event"`

	// Granularity describes measurement granularity.
	// MVP: "perPduSession".
	Granularity GranularityType `json:"granularity"`

	// PeriodSec is the requested push interval in seconds.
	// If zero or negative, DCCF will fall back to a default period.
	PeriodSec int `json:"periodSec,omitempty"`

	// AnyUE indicates that the subscription targets all UEs served by
	// the underlying UPFs. Future extensions may add filters based on
	// UE IP, DNN, S-NSSAI, etc.
	AnyUE bool `json:"anyUe"`
}

// NdccfSubscriptionResponse is returned after a successful subscription
// creation.
type NdccfSubscriptionResponse struct {
	SubscriptionID string `json:"subscriptionId"`
}

// NdccfFetchRequest represents a one-shot fetch of usage measurements.
// It is the MVP replacement for earlier on-demand semantics.
type NdccfFetchRequest struct {
	Event       EventType       `json:"event"`           // MUST be USER_DATA_USAGE_MEASURES in MVP
	Granularity GranularityType `json:"granularity"`     // MUST be perPduSession in MVP
	Since       *time.Time      `json:"since,omitempty"` // optional lower bound of interval start
	Until       *time.Time      `json:"until,omitempty"` // optional upper bound of interval end
	AnyUE       bool            `json:"anyUe"`           // MVP: only AnyUE=true is supported
	Limit       int             `json:"limit,omitempty"` // optional maximum number of items
}

// NdccfNotify is the payload pushed from DCCF to AnLF/MTLF for a given
// subscription. It carries normalized UsageMeasurement items.
type NdccfNotify struct {
	SubscriptionID string             `json:"subscriptionId"`
	EventID        EventType          `json:"eventId"`
	Granularity    GranularityType    `json:"granularity"`
	Timestamp      time.Time          `json:"timestamp"`
	Items          []UsageMeasurement `json:"items"`
}
