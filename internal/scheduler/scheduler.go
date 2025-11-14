// Package scheduler implements the periodic northbound push logic for DCCF.
//
// The scheduler is responsible for:
//   - Walking through all active northbound subscriptions in the RuntimeContext
//   - Deciding which subscriptions are due for a push based on PeriodSec
//   - Building time windows and querying the Aggregator for fresh measurements
//   - Delivering NdccfNotify payloads via the northbound.Notifier
//   - Updating the last delivery timestamp for each subscription
//
// MVP behaviour:
//   - Only AnyUE=true subscriptions are supported
//   - Each subscription has its own PeriodSec configured at creation time
//   - The scheduler runs a simple periodic tick (e.g., every second)
//   - For each subscription, it sends a notification if the elapsed time since
//     LastDeliveredAt is >= PeriodSec - delayTolerance
package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/free5gc/dccf/internal/aggregator"
	dccfctx "github.com/free5gc/dccf/internal/context"
	"github.com/free5gc/dccf/internal/logger"
	"github.com/free5gc/dccf/internal/model"
	"github.com/free5gc/dccf/internal/northbound"
)

// Scheduler controls periodic northbound notifications based on
// subscription state held in the RuntimeContext.
type Scheduler interface {
	// Start launches the scheduler loop in a background goroutine. It returns
	// immediately after successful start. The provided context is used only
	// for initialisation; cancellation should be signalled via Stop().
	Start(ctx context.Context) error

	// Stop requests the scheduler to stop and waits for the background loop
	// to exit. It is safe to call Stop() multiple times.
	Stop(ctx context.Context) error
}

// schedulerImpl is the concrete implementation of Scheduler.
type schedulerImpl struct {
	aggregatorInstance aggregator.Aggregator
	notifier           northbound.Notifier
	runtimeContext     dccfctx.RuntimeContext

	// tickInterval controls how often we evaluate subscriptions.
	tickInterval time.Duration
	// delayTolerance is a soft margin used when deciding if a subscription
	// is "due" for a notification (PeriodSec - delayTolerance).
	delayTolerance time.Duration

	startStopMutex sync.Mutex
	started        bool
	stopChannel    chan struct{}
	stoppedChannel chan struct{}
}

// NewScheduler creates a new Scheduler instance.
//
// Parameters:
//   - aggregatorInstance: source of normalized usage measurements
//   - notifier:           mechanism for delivering NdccfNotify payloads
//   - runtimeContext:     provider of subscription state
//   - delayToleranceSec:  soft margin (seconds) when comparing elapsed time
//     with subscription.PeriodSec; if <= 0, no margin.
func NewScheduler(
	aggregatorInstance aggregator.Aggregator,
	notifier northbound.Notifier,
	runtimeContext dccfctx.RuntimeContext,
	delayToleranceSec int,
) Scheduler {
	delayTolerance := time.Duration(0)
	if delayToleranceSec > 0 {
		delayTolerance = time.Duration(delayToleranceSec) * time.Second
	}

	return &schedulerImpl{
		aggregatorInstance: aggregatorInstance,
		notifier:           notifier,
		runtimeContext:     runtimeContext,
		tickInterval:       time.Second,
		delayTolerance:     delayTolerance,
		stopChannel:        make(chan struct{}),
		stoppedChannel:     make(chan struct{}),
	}
}

// Start implements Scheduler.Start.
func (schedulerInstance *schedulerImpl) Start(ctx context.Context) error {
	schedulerInstance.startStopMutex.Lock()
	defer schedulerInstance.startStopMutex.Unlock()

	if schedulerInstance.started {
		logger.SchedulerLog.Warn("Scheduler.Start called more than once; ignoring subsequent call")
		return nil
	}

	schedulerInstance.started = true

	go schedulerInstance.runLoop()

	logger.SchedulerLog.Info("Scheduler started")
	return nil
}

// Stop implements Scheduler.Stop.
func (schedulerInstance *schedulerImpl) Stop(ctx context.Context) error {
	schedulerInstance.startStopMutex.Lock()
	defer schedulerInstance.startStopMutex.Unlock()

	if !schedulerInstance.started {
		return nil
	}

	select {
	case <-schedulerInstance.stopChannel:
		// Already closing or closed.
	default:
		close(schedulerInstance.stopChannel)
	}

	// Wait for the loop to exit or for the context to expire.
	select {
	case <-schedulerInstance.stoppedChannel:
	case <-ctx.Done():
		return ctx.Err()
	}

	logger.SchedulerLog.Info("Scheduler stopped")
	return nil
}

// runLoop executes the periodic scheduling logic until stopChannel is closed.
func (schedulerInstance *schedulerImpl) runLoop() {
	defer close(schedulerInstance.stoppedChannel)

	ticker := time.NewTicker(schedulerInstance.tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-schedulerInstance.stopChannel:
			return
		case <-ticker.C:
			schedulerInstance.processTick()
		}
	}
}

// processTick evaluates all active northbound subscriptions and sends
// notifications for those that are due.
func (schedulerInstance *schedulerImpl) processTick() {
	now := time.Now().UTC()

	subscriptions := schedulerInstance.runtimeContext.GetNorthboundSubscriptionsSnapshot()
	if len(subscriptions) == 0 {
		return
	}

	for _, subscription := range subscriptions {
		// Skip subscriptions that do not have a notifUri; in MVP we treat them as
		// pull-only (fetch) subscriptions.
		if subscription.NotifURI == "" {
			continue
		}

		if subscription.PeriodSec <= 0 {
			// This should not normally happen because PeriodSec is normalised at
			// creation time, but we guard against it here.
			continue
		}

		if !subscription.AnyUE {
			// MVP only supports AnyUE=true.
			continue
		}

		if !schedulerInstance.isSubscriptionDue(subscription, now) {
			continue
		}

		windowStart, windowEnd := schedulerInstance.computeWindowForSubscription(subscription, now)
		if !windowEnd.After(windowStart) {
			continue
		}

		schedulerInstance.dispatchNotificationForSubscription(subscription, windowStart, windowEnd, now)
	}
}

// isSubscriptionDue decides whether a subscription is due for a notification
// at the given reference time.
func (schedulerInstance *schedulerImpl) isSubscriptionDue(
	subscription dccfctx.NorthboundSubscriptionView,
	now time.Time,
) bool {
	periodDuration := time.Duration(subscription.PeriodSec) * time.Second

	// First-time delivery: if LastDeliveredAt is zero, we treat the subscription
	// as due immediately.
	if subscription.LastDeliveredAt.IsZero() {
		return true
	}

	elapsed := now.Sub(subscription.LastDeliveredAt)
	threshold := periodDuration
	if schedulerInstance.delayTolerance > 0 && schedulerInstance.delayTolerance < periodDuration {
		threshold = periodDuration - schedulerInstance.delayTolerance
	}

	return elapsed >= threshold
}

// computeWindowForSubscription determines the [start, end] time window for
// which measurements should be included in the next notification.
func (schedulerInstance *schedulerImpl) computeWindowForSubscription(
	subscription dccfctx.NorthboundSubscriptionView,
	now time.Time,
) (time.Time, time.Time) {
	windowEnd := now

	if schedulerInstance.delayTolerance > 0 {
		windowEnd = now.Add(-schedulerInstance.delayTolerance)
	}

	var windowStart time.Time
	if subscription.LastDeliveredAt.IsZero() {
		// For the first notification, start from CreatedAt.
		windowStart = subscription.CreatedAt
	} else {
		windowStart = subscription.LastDeliveredAt
	}

	// Basic safety: if, for some reason, CreatedAt or LastDeliveredAt is
	// after windowEnd, clamp windowStart to windowEnd to avoid negative
	// intervals.
	if windowStart.After(windowEnd) {
		windowStart = windowEnd
	}

	return windowStart, windowEnd
}

// dispatchNotificationForSubscription pulls measurements from the aggregator
// and sends a NdccfNotify to the subscriber. On success, it updates the
// subscription's LastDeliveredAt in the RuntimeContext.
func (schedulerInstance *schedulerImpl) dispatchNotificationForSubscription(
	subscription dccfctx.NorthboundSubscriptionView,
	windowStart time.Time,
	windowEnd time.Time,
	now time.Time,
) {
	limit := subscription.BatchMaxItems
	if limit <= 0 {
		limit = 1000
	}

	measurements, queryError := schedulerInstance.aggregatorInstance.BuildBatchForInterval(
		context.Background(),
		windowStart,
		windowEnd,
		subscription.AnyUE,
		limit,
	)
	if queryError != nil {
		logger.SchedulerLog.Errorf(
			"BuildBatchForInterval failed for subscriptionId=%s notifUri=%s: %v",
			subscription.ID, subscription.NotifURI, queryError,
		)
		return
	}

	if len(measurements) == 0 {
		// No data for this window; we skip sending an empty notification.
		return
	}

	notification := model.NdccfNotify{
		SubscriptionID: subscription.ID,
		EventID:        subscription.Event,
		Granularity:    subscription.Granularity,
		Timestamp:      now,
		Items:          measurements,
	}

	notifyError := schedulerInstance.notifier.Notify(
		context.Background(),
		subscription.NotifURI,
		notification,
	)
	if notifyError != nil {
		logger.SchedulerLog.Errorf(
			"NdccfNotify delivery failed for subscriptionId=%s notifUri=%s: %v",
			subscription.ID, subscription.NotifURI, notifyError,
		)
		return
	}

	updateError := schedulerInstance.runtimeContext.UpdateNorthboundSubscriptionDeliveryTime(
		context.Background(),
		subscription.ID,
		now,
	)
	if updateError != nil {
		logger.SchedulerLog.Warnf(
			"failed to update LastDeliveredAt for subscriptionId=%s: %v",
			subscription.ID, updateError,
		)
		return
	}

	logger.SchedulerLog.Debugf(
		"delivered NdccfNotify for subscriptionId=%s notifUri=%s items=%d window=[%s,%s]",
		subscription.ID,
		subscription.NotifURI,
		len(measurements),
		windowStart.Format(time.RFC3339Nano),
		windowEnd.Format(time.RFC3339Nano),
	)
}
