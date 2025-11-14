// Package aggregator implements the logic that ingests southbound UPF EES
// notifications, normalizes them into UsageMeasurement records, and hands
// them to the storage backend. It also exposes a query helper used by the
// northbound API (fetch / push).
//
// MVP behavior:
//   - Treats all incoming measurements as interval values over [StartTime, EndTime].
//   - Normalizes UPF-specific payloads into model.UsageMeasurement with SourceUPFID.
//   - Delegates persistence and TTL/limit policies to the storage.Store.
//   - Provides a simple BuildBatchForInterval helper for northbound use.
package aggregator

import (
	"context"
	"fmt"
	"time"

	"github.com/free5gc/dccf/internal/logger"
	"github.com/free5gc/dccf/internal/model"
	"github.com/free5gc/dccf/internal/storage"
)

// Aggregator is the abstraction used by southbound handlers (UPF EES receiver)
// and northbound handlers (fetch / push) to work with normalized usage data.
type Aggregator interface {
	// IngestUPFNotify consumes a single UPF EES Notify message, normalizes
	// its interval items, and persists them via the storage backend.
	// It returns the number of items accepted.
	IngestUPFNotify(
		ctx context.Context,
		upfID string,
		upfSubscriptionID string,
		notifyPayload model.UpfEesNotify,
	) (acceptedCount int, err error)

	// BuildBatchForInterval queries the storage backend for measurements
	// that fall within the given time window. It is meant to be used by
	// northbound fetch and periodic push paths.
	BuildBatchForInterval(
		ctx context.Context,
		since time.Time,
		until time.Time,
		anyUE bool,
		limit int,
	) ([]model.UsageMeasurement, error)

	// Vacuum allows the caller (e.g., a scheduler) to trigger TTL-based
	// cleanup in the storage backend. For some backends this may be a no-op.
	Vacuum(ctx context.Context) error
}

// aggregatorImpl is the concrete implementation of Aggregator.
type aggregatorImpl struct {
	store storage.Store
}

// NewAggregator creates a new Aggregator using the given storage backend.
func NewAggregator(store storage.Store) Aggregator {
	return &aggregatorImpl{
		store: store,
	}
}

// IngestUPFNotify implements Aggregator.IngestUPFNotify.
func (aggregatorImplInstance *aggregatorImpl) IngestUPFNotify(
	ctx context.Context,
	upfID string,
	upfSubscriptionID string,
	notifyPayload model.UpfEesNotify,
) (int, error) {
	if upfID == "" {
		return 0, fmt.Errorf("upfID must not be empty")
	}
	if len(notifyPayload.Items) == 0 {
		return 0, nil
	}

	normalizedMeasurements := make([]model.UsageMeasurement, 0, len(notifyPayload.Items))

	for index, item := range notifyPayload.Items {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}

		if item.EndTime.Before(item.StartTime) {
			// Log and skip obviously inconsistent intervals.
			logger.AggregatorLog.Warnf(
				"skipping item %d from upfId=%s subscriptionId=%s due to EndTime < StartTime (start=%s end=%s)",
				index, upfID, notifyPayload.SubscriptionID,
				item.StartTime.Format(time.RFC3339Nano),
				item.EndTime.Format(time.RFC3339Nano),
			)
			continue
		}

		measurement := model.UsageMeasurement{
			SourceUPFID: upfID,
			LocalSEID:   item.LocalSEID,
			RemoteSEID:  item.RemoteSEID,

			ULBytes:   item.ULBytes,
			DLBytes:   item.DLBytes,
			ULPackets: item.ULPackets,
			DLPackets: item.DLPackets,

			StartTime: item.StartTime,
			EndTime:   item.EndTime,

			ULThroughputBps: item.ULThroughputBps,
			DLThroughputBps: item.DLThroughputBps,
		}

		normalizedMeasurements = append(normalizedMeasurements, measurement)
	}

	if len(normalizedMeasurements) == 0 {
		return 0, nil
	}

	saveError := aggregatorImplInstance.store.SaveUsageMeasurements(ctx, normalizedMeasurements)
	if saveError != nil {
		logger.AggregatorLog.Errorf(
			"failed to save %d measurement(s) for upfId=%s subscriptionId=%s: %v",
			len(normalizedMeasurements), upfID, notifyPayload.SubscriptionID, saveError,
		)
		return 0, saveError
	}

	logger.AggregatorLog.Debugf(
		"saved %d measurement(s) from upfId=%s subscriptionId=%s",
		len(normalizedMeasurements), upfID, notifyPayload.SubscriptionID,
	)

	return len(normalizedMeasurements), nil
}

// BuildBatchForInterval implements Aggregator.BuildBatchForInterval.
func (aggregatorImplInstance *aggregatorImpl) BuildBatchForInterval(
	ctx context.Context,
	since time.Time,
	until time.Time,
	anyUE bool,
	limit int,
) ([]model.UsageMeasurement, error) {
	if !anyUE {
		// MVP only supports AnyUE=true. Future work may add more fine-grained
		// filtering (UE IP, DNN, S-NSSAI, etc.).
		return nil, fmt.Errorf("BuildBatchForInterval currently supports only AnyUE=true")
	}

	var sincePtr *time.Time
	var untilPtr *time.Time

	if !since.IsZero() {
		s := since
		sincePtr = &s
	}
	if !until.IsZero() {
		u := until
		untilPtr = &u
	}

	query := storage.Query{
		AnyUE: anyUE,
		Since: sincePtr,
		Until: untilPtr,
		Limit: limit,
	}

	measurements, queryError := aggregatorImplInstance.store.QueryUsageMeasurements(ctx, query)
	if queryError != nil {
		logger.AggregatorLog.Errorf(
			"failed to query measurements (since=%s until=%s anyUE=%t limit=%d): %v",
			formatTimeForLog(since), formatTimeForLog(until), anyUE, limit, queryError,
		)
		return nil, queryError
	}

	logger.AggregatorLog.Debugf(
		"BuildBatchForInterval produced %d measurement(s) (since=%s until=%s anyUE=%t limit=%d)",
		len(measurements), formatTimeForLog(since), formatTimeForLog(until), anyUE, limit,
	)

	return measurements, nil
}

// Vacuum implements Aggregator.Vacuum.
func (aggregatorImplInstance *aggregatorImpl) Vacuum(ctx context.Context) error {
	vacuumError := aggregatorImplInstance.store.Vacuum(ctx)
	if vacuumError != nil {
		logger.AggregatorLog.Errorf("storage vacuum failed: %v", vacuumError)
		return vacuumError
	}
	return nil
}

// formatTimeForLog converts a time value into a compact string for logging.
// Zero time values are rendered as "-".
func formatTimeForLog(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Format(time.RFC3339Nano)
}
