// Package context holds the in-memory runtime state for DCCF, including:
//   - UPF subscription states (toward UPF EES)
//   - Northbound subscription states (toward AnLF/MTLF)
//   - Shutdown flags and basic lifecycle helpers.
//
// Note: This package is named "context", so we alias the standard library
// "context" package to avoid name collisions.
package context

import (
	stdctx "context"
	"fmt"
	"sync"
	"time"

	"github.com/free5gc/dccf/internal/logger"
	"github.com/free5gc/dccf/internal/model"
)

// UpfSubscriptionState describes the state of one subscription that DCCF
// holds towards a given UPF EES instance.
type UpfSubscriptionState struct {
	UpfID             string
	EESBaseURL        string
	NotifURI          string
	PeriodSec         int
	UpfSubscriptionID string
	LastSeenAt        time.Time
}

// NorthboundSubscription represents a subscription created by an external
// consumer (e.g., AnLF/MTLF) via the Ndccf-like northbound API.
type NorthboundSubscription struct {
	ID              string
	NotifURI        string
	Event           model.EventType
	Granularity     model.GranularityType
	PeriodSec       int
	AnyUE           bool
	CreatedAt       time.Time
	LastDeliveredAt time.Time
	BatchMaxItems   int
}

// NorthboundSubscriptionView is an immutable view used by the scheduler
// to make decisions without holding internal locks.
type NorthboundSubscriptionView struct {
	ID              string
	NotifURI        string
	Event           model.EventType
	Granularity     model.GranularityType
	PeriodSec       int
	AnyUE           bool
	CreatedAt       time.Time
	LastDeliveredAt time.Time
	BatchMaxItems   int
}

// RuntimeContext provides concurrency-safe accessors to UPF and northbound
// subscriptions, as well as shutdown flags.
type RuntimeContext interface {
	// ---- UPF subscription state ----

	// SetOrUpdateUpfSubscription records or updates the local view of a UPF
	// EES subscription.
	SetOrUpdateUpfSubscription(
		ctx stdctx.Context,
		upfID string,
		eesBaseURL string,
		notifURI string,
		periodSec int,
		upfSubscriptionID string,
	) error

	// DeleteUpfSubscription removes the UPF subscription state for the given
	// upfID, if it exists.
	DeleteUpfSubscription(ctx stdctx.Context, upfID string) error

	// GetUpfSubscriptionsSnapshot returns a copy of all known UPF subscriptions.
	GetUpfSubscriptionsSnapshot() []UpfSubscriptionState

	// ---- Northbound subscription state ----

	// CreateNorthboundSubscription allocates a new subscription ID, records
	// the subscription and returns the assigned ID.
	CreateNorthboundSubscription(
		ctx stdctx.Context,
		request model.NdccfSubscriptionRequest,
		effectivePeriodSec int,
	) (string, error)

	// DeleteNorthboundSubscription removes the subscription with the given ID.
	// It returns (true, nil) if the subscription existed; otherwise (false, nil).
	DeleteNorthboundSubscription(
		ctx stdctx.Context,
		subscriptionID string,
	) (bool, error)

	// GetNorthboundSubscriptionsSnapshot returns a slice of read-only views
	// for all active northbound subscriptions.
	GetNorthboundSubscriptionsSnapshot() []NorthboundSubscriptionView

	// UpdateNorthboundSubscriptionDeliveryTime updates LastDeliveredAt for
	// the subscription with the given ID.
	UpdateNorthboundSubscriptionDeliveryTime(
		ctx stdctx.Context,
		subscriptionID string,
		deliveredAt time.Time,
	) error

	// ---- Shutdown flag ----

	// SetShutdownRequested marks whether a graceful shutdown has been requested.
	SetShutdownRequested(ctx stdctx.Context, requested bool)

	// IsShutdownRequested returns true if shutdown has been requested.
	IsShutdownRequested() bool
}

// runtimeContextImpl is the concrete implementation of RuntimeContext.
// It keeps all state in memory guarded by a RWMutex.
type runtimeContextImpl struct {
	mutexForUPFState sync.RWMutex
	upfStateByID     map[string]*UpfSubscriptionState

	mutexForNorthboundState sync.RWMutex
	northboundStateByID     map[string]*NorthboundSubscription
	nextSubscriptionNumeric uint64

	mutexForShutdown  sync.RWMutex
	shutdownRequested bool

	defaultBatchMaxItems int
}

// NewRuntimeContext creates a new, empty RuntimeContext.
func NewRuntimeContext(defaultBatchMaxItems int) RuntimeContext {
	if defaultBatchMaxItems <= 0 {
		defaultBatchMaxItems = 1000
	}

	return &runtimeContextImpl{
		upfStateByID:         make(map[string]*UpfSubscriptionState),
		northboundStateByID:  make(map[string]*NorthboundSubscription),
		defaultBatchMaxItems: defaultBatchMaxItems,
	}
}

// -----------------------------------------------------------------------------
// UPF subscription state
// -----------------------------------------------------------------------------

// SetOrUpdateUpfSubscription implements RuntimeContext.SetOrUpdateUpfSubscription.
func (runtime *runtimeContextImpl) SetOrUpdateUpfSubscription(
	ctx stdctx.Context,
	upfID string,
	eesBaseURL string,
	notifURI string,
	periodSec int,
	upfSubscriptionID string,
) error {
	if upfID == "" {
		return fmt.Errorf("upfID must not be empty")
	}
	if periodSec <= 0 {
		return fmt.Errorf("periodSec must be > 0")
	}

	runtime.mutexForUPFState.Lock()
	defer runtime.mutexForUPFState.Unlock()

	state, exists := runtime.upfStateByID[upfID]
	if !exists {
		state = &UpfSubscriptionState{
			UpfID: upfID,
		}
		runtime.upfStateByID[upfID] = state
	}

	state.EESBaseURL = eesBaseURL
	state.NotifURI = notifURI
	state.PeriodSec = periodSec
	state.UpfSubscriptionID = upfSubscriptionID
	state.LastSeenAt = time.Now().UTC()

	logger.ContextLog.Debugf(
		"UPF subscription updated upfId=%s subscriptionId=%s periodSec=%d",
		upfID, upfSubscriptionID, periodSec,
	)

	return nil
}

// DeleteUpfSubscription implements RuntimeContext.DeleteUpfSubscription.
func (runtime *runtimeContextImpl) DeleteUpfSubscription(
	ctx stdctx.Context,
	upfID string,
) error {
	if upfID == "" {
		return nil
	}

	runtime.mutexForUPFState.Lock()
	defer runtime.mutexForUPFState.Unlock()

	delete(runtime.upfStateByID, upfID)

	logger.ContextLog.Debugf("UPF subscription removed upfId=%s", upfID)
	return nil
}

// GetUpfSubscriptionsSnapshot implements RuntimeContext.GetUpfSubscriptionsSnapshot.
func (runtime *runtimeContextImpl) GetUpfSubscriptionsSnapshot() []UpfSubscriptionState {
	runtime.mutexForUPFState.RLock()
	defer runtime.mutexForUPFState.RUnlock()

	result := make([]UpfSubscriptionState, 0, len(runtime.upfStateByID))
	for _, state := range runtime.upfStateByID {
		if state == nil {
			continue
		}
		copyState := *state
		result = append(result, copyState)
	}
	return result
}

// -----------------------------------------------------------------------------
// Northbound subscription state
// -----------------------------------------------------------------------------

// CreateNorthboundSubscription implements RuntimeContext.CreateNorthboundSubscription.
func (runtime *runtimeContextImpl) CreateNorthboundSubscription(
	ctx stdctx.Context,
	request model.NdccfSubscriptionRequest,
	effectivePeriodSec int,
) (string, error) {
	if effectivePeriodSec <= 0 {
		return "", fmt.Errorf("effectivePeriodSec must be > 0")
	}

	runtime.mutexForNorthboundState.Lock()
	defer runtime.mutexForNorthboundState.Unlock()

	newID := runtime.allocateSubscriptionIDLocked()

	subscription := &NorthboundSubscription{
		ID:            newID,
		NotifURI:      request.NotifURI,
		Event:         request.Event,
		Granularity:   request.Granularity,
		PeriodSec:     effectivePeriodSec,
		AnyUE:         request.AnyUE,
		CreatedAt:     time.Now().UTC(),
		BatchMaxItems: runtime.defaultBatchMaxItems,
	}

	runtime.northboundStateByID[newID] = subscription

	logger.ContextLog.Infof(
		"northbound subscription created id=%s event=%s granularity=%s periodSec=%d notifUri=%s anyUe=%t",
		newID, subscription.Event, subscription.Granularity,
		subscription.PeriodSec, subscription.NotifURI, subscription.AnyUE,
	)

	return newID, nil
}

// DeleteNorthboundSubscription implements RuntimeContext.DeleteNorthboundSubscription.
func (runtime *runtimeContextImpl) DeleteNorthboundSubscription(
	ctx stdctx.Context,
	subscriptionID string,
) (bool, error) {
	if subscriptionID == "" {
		return false, nil
	}

	runtime.mutexForNorthboundState.Lock()
	defer runtime.mutexForNorthboundState.Unlock()

	if _, exists := runtime.northboundStateByID[subscriptionID]; !exists {
		return false, nil
	}

	delete(runtime.northboundStateByID, subscriptionID)

	logger.ContextLog.Infof("northbound subscription deleted id=%s", subscriptionID)
	return true, nil
}

// GetNorthboundSubscriptionsSnapshot implements RuntimeContext.GetNorthboundSubscriptionsSnapshot.
func (runtime *runtimeContextImpl) GetNorthboundSubscriptionsSnapshot() []NorthboundSubscriptionView {
	runtime.mutexForNorthboundState.RLock()
	defer runtime.mutexForNorthboundState.RUnlock()

	result := make([]NorthboundSubscriptionView, 0, len(runtime.northboundStateByID))
	for _, subscription := range runtime.northboundStateByID {
		if subscription == nil {
			continue
		}
		result = append(result, NorthboundSubscriptionView{
			ID:              subscription.ID,
			NotifURI:        subscription.NotifURI,
			Event:           subscription.Event,
			Granularity:     subscription.Granularity,
			PeriodSec:       subscription.PeriodSec,
			AnyUE:           subscription.AnyUE,
			CreatedAt:       subscription.CreatedAt,
			LastDeliveredAt: subscription.LastDeliveredAt,
			BatchMaxItems:   subscription.BatchMaxItems,
		})
	}
	return result
}

// UpdateNorthboundSubscriptionDeliveryTime implements
// RuntimeContext.UpdateNorthboundSubscriptionDeliveryTime.
func (runtime *runtimeContextImpl) UpdateNorthboundSubscriptionDeliveryTime(
	ctx stdctx.Context,
	subscriptionID string,
	deliveredAt time.Time,
) error {
	if subscriptionID == "" {
		return fmt.Errorf("subscriptionId must not be empty")
	}

	runtime.mutexForNorthboundState.Lock()
	defer runtime.mutexForNorthboundState.Unlock()

	subscription, exists := runtime.northboundStateByID[subscriptionID]
	if !exists || subscription == nil {
		return fmt.Errorf("subscriptionId %q not found", subscriptionID)
	}

	subscription.LastDeliveredAt = deliveredAt

	logger.ContextLog.Debugf(
		"northbound subscription delivery time updated id=%s deliveredAt=%s",
		subscriptionID, deliveredAt.Format(time.RFC3339Nano),
	)

	return nil
}

// allocateSubscriptionIDLocked allocates a new subscription ID. It assumes
// that mutexForNorthboundState is already held by the caller.
func (runtime *runtimeContextImpl) allocateSubscriptionIDLocked() string {
	runtime.nextSubscriptionNumeric++
	return fmt.Sprintf("sub-%d", runtime.nextSubscriptionNumeric)
}

// -----------------------------------------------------------------------------
// Shutdown flag
// -----------------------------------------------------------------------------

// SetShutdownRequested implements RuntimeContext.SetShutdownRequested.
func (runtime *runtimeContextImpl) SetShutdownRequested(
	ctx stdctx.Context,
	requested bool,
) {
	runtime.mutexForShutdown.Lock()
	defer runtime.mutexForShutdown.Unlock()
	runtime.shutdownRequested = requested

	logger.ContextLog.Infof("shutdown requested=%t", requested)
}

// IsShutdownRequested implements RuntimeContext.IsShutdownRequested.
func (runtime *runtimeContextImpl) IsShutdownRequested() bool {
	runtime.mutexForShutdown.RLock()
	defer runtime.mutexForShutdown.RUnlock()
	return runtime.shutdownRequested
}
