// Package storage provides the abstraction and implementations for persisting
// usage measurements inside DCCF. The MVP implementation uses an in-memory
// backend, but the design allows future backends such as MongoDB or TSDB
// without changing the rest of the codebase.
package storage

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/free5gc/dccf/internal/logger"
	"github.com/free5gc/dccf/internal/model"
	"github.com/free5gc/dccf/pkg/factory"
)

// Store is the high-level storage interface used by aggregator and northbound
// handlers. All operations are safe to be called from concurrent goroutines.
type Store interface {
	// SaveUsageMeasurements persists a batch of usage measurements.
	// The call should be atomic from the caller's perspective: either all
	// measurements are accepted or an error is returned.
	SaveUsageMeasurements(ctx context.Context, measurements []model.UsageMeasurement) error

	// QueryUsageMeasurements returns all measurements that match the query
	// constraints, ordered by (StartTime, EndTime) ascending.
	QueryUsageMeasurements(ctx context.Context, query Query) ([]model.UsageMeasurement, error)

	// DeleteByUPFSubscription removes measurements associated with a given
	// UPF subscription. This is primarily used during graceful shutdown
	// or when a UPF subscription is cancelled.
	DeleteByUPFSubscription(ctx context.Context, upfID string, upfSubscriptionID string) error

	// Vacuum performs maintenance operations such as removing expired entries.
	// For some backends this may be a no-op.
	Vacuum(ctx context.Context) error
}

// Query defines constraints used when selecting measurements from the store.
type Query struct {
	AnyUE bool

	// Since is an optional lower bound on the measurement interval.
	// If non-nil, only measurements with EndTime >= *Since are returned.
	Since *time.Time

	// Until is an optional upper bound on the measurement interval.
	// If non-nil, only measurements with StartTime <= *Until are returned.
	Until *time.Time

	// Limit is an optional maximum number of results.
	// If Limit <= 0, no explicit limit is applied.
	Limit int
}

// NewStoreFromConfig creates a Store based on the storage configuration.
func NewStoreFromConfig(storageConfig factory.StorageConfig) (Store, error) {
	switch storageConfig.Driver {
	case "memory":
		logger.StorageLog.Infof("Using in-memory storage backend (maxItems=%d, ttlSec=%d)",
			storageConfig.MaxItems, storageConfig.TTLSeconds)
		return newMemoryStore(storageConfig), nil
	case "mongo", "tsdb":
		return nil, fmt.Errorf("storage driver %q is not implemented yet", storageConfig.Driver)
	default:
		return nil, fmt.Errorf("unknown storage driver %q", storageConfig.Driver)
	}
}

// -----------------------------------------------------------------------------
// In-memory implementation (MVP)
// -----------------------------------------------------------------------------

// memoryStore keeps all measurements in memory. It is suitable for functional
// testing and small-scale demos. It is NOT intended for production-scale
// deployments.
type memoryStore struct {
	mutexForEntries sync.RWMutex
	entries         []memoryEntry

	maxItems int           // 0 or negative means "no explicit limit"
	ttl      time.Duration // 0 means "no TTL"
}

type memoryEntry struct {
	measurement   model.UsageMeasurement
	insertionTime time.Time
}

func newMemoryStore(storageConfig factory.StorageConfig) *memoryStore {
	var ttlDuration time.Duration
	if storageConfig.TTLSeconds > 0 {
		ttlDuration = time.Duration(storageConfig.TTLSeconds) * time.Second
	}

	return &memoryStore{
		entries:  make([]memoryEntry, 0),
		maxItems: storageConfig.MaxItems,
		ttl:      ttlDuration,
	}
}

// SaveUsageMeasurements appends a batch of measurements to the in-memory store.
// It also performs best-effort cleanup based on TTL and MaxItems.
func (store *memoryStore) SaveUsageMeasurements(
	ctx context.Context,
	measurements []model.UsageMeasurement,
) error {
	if len(measurements) == 0 {
		return nil
	}

	now := time.Now()

	store.mutexForEntries.Lock()
	defer store.mutexForEntries.Unlock()

	// TTL-based cleanup before append
	if store.ttl > 0 {
		store.removeExpiredLocked(now)
	}

	for _, measurement := range measurements {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			store.entries = append(store.entries, memoryEntry{
				measurement:   measurement,
				insertionTime: now,
			})
		}
	}

	// Enforce maxItems if configured.
	if store.maxItems > 0 && len(store.entries) > store.maxItems {
		overflow := len(store.entries) - store.maxItems
		if overflow > 0 && overflow < len(store.entries) {
			logger.StorageLog.Warnf(
				"memory storage reached maxItems=%d, dropping oldest %d entries",
				store.maxItems, overflow,
			)
			store.entries = store.entries[overflow:]
		} else if overflow >= len(store.entries) {
			// Should not normally happen, but keep a safe guard.
			logger.StorageLog.Warnf(
				"memory storage overflow calculation exceeded array length, clearing all entries",
			)
			store.entries = store.entries[:0]
		}
	}

	return nil
}

// QueryUsageMeasurements scans the in-memory slice and returns a filtered copy.
func (store *memoryStore) QueryUsageMeasurements(
	ctx context.Context,
	query Query,
) ([]model.UsageMeasurement, error) {
	now := time.Now()

	store.mutexForEntries.RLock()
	defer store.mutexForEntries.RUnlock()

	results := make([]model.UsageMeasurement, 0)

	for _, entry := range store.entries {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		measurement := entry.measurement

		// AnyUE is the only target supported in MVP; if it is false, we currently
		// do not have additional filters to apply.
		if !query.AnyUE {
			continue
		}

		if store.ttl > 0 {
			if now.Sub(entry.insertionTime) > store.ttl {
				// expired; skip here, and let Vacuum() or future Save() calls
				// clean them up from the slice.
				continue
			}
		}

		if query.Since != nil {
			if measurement.EndTime.Before(*query.Since) {
				continue
			}
		}

		if query.Until != nil {
			if measurement.StartTime.After(*query.Until) {
				continue
			}
		}

		results = append(results, measurement)

		if query.Limit > 0 && len(results) >= query.Limit {
			break
		}
	}

	return results, nil
}

// DeleteByUPFSubscription removes entries associated with a given UPF and
// subscription. The MVP implementation only uses the upfID (SourceUPFID) and
// ignores the upfSubscriptionID, but keeps it in the signature for future
// alignment with more advanced backends.
func (store *memoryStore) DeleteByUPFSubscription(
	ctx context.Context,
	upfID string,
	upfSubscriptionID string,
) error {
	store.mutexForEntries.Lock()
	defer store.mutexForEntries.Unlock()

	if upfID == "" {
		return nil
	}

	filtered := make([]memoryEntry, 0, len(store.entries))

	for _, entry := range store.entries {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if entry.measurement.SourceUPFID == upfID {
			// Drop this entry. For now we do not inspect upfSubscriptionID;
			// future implementations may track it explicitly.
			continue
		}

		filtered = append(filtered, entry)
	}

	droppedCount := len(store.entries) - len(filtered)
	if droppedCount > 0 {
		logger.StorageLog.Infof(
			"deleted %d measurement(s) for upfId=%s (subscriptionId=%s, ignored in MVP)",
			droppedCount, upfID, upfSubscriptionID,
		)
	}

	store.entries = filtered
	return nil
}

// Vacuum removes expired entries according to TTL. It is safe to call this
// periodically; if TTL is not configured, it becomes a no-op.
func (store *memoryStore) Vacuum(ctx context.Context) error {
	if store.ttl <= 0 {
		return nil
	}

	now := time.Now()

	store.mutexForEntries.Lock()
	defer store.mutexForEntries.Unlock()

	beforeCount := len(store.entries)
	store.removeExpiredLocked(now)
	afterCount := len(store.entries)

	if beforeCount != afterCount {
		logger.StorageLog.Debugf(
			"vacuum removed %d expired measurement(s) from memory store",
			beforeCount-afterCount,
		)
	}

	return nil
}

// removeExpiredLocked is a helper that assumes mutexForEntries is already held.
func (store *memoryStore) removeExpiredLocked(referenceTime time.Time) {
	if store.ttl <= 0 {
		return
	}

	if len(store.entries) == 0 {
		return
	}

	filtered := store.entries[:0]
	for _, entry := range store.entries {
		if referenceTime.Sub(entry.insertionTime) <= store.ttl {
			filtered = append(filtered, entry)
		}
	}
	store.entries = filtered
}
