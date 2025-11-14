// Package app wires together all major DCCF components:
//   - configuration
//   - logging
//   - runtime context
//   - storage backend
//   - southbound UPF EES receiver
//   - northbound Ndccf-like API server
//   - scheduler for periodic northbound push
//   - UPF EES subscribe/unsubscribe client.
//
// The App implementation is intentionally small and procedural, so that
// cmd/main.go can simply create an App from the loaded Config and call
// Start/Stop without knowing internal details.
package app

import (
	stdctx "context"
	"fmt"
	"sync"

	"github.com/free5gc/dccf/internal/aggregator"
	dccfctx "github.com/free5gc/dccf/internal/context"
	"github.com/free5gc/dccf/internal/logger"
	"github.com/free5gc/dccf/internal/northbound"
	"github.com/free5gc/dccf/internal/sbi"
	"github.com/free5gc/dccf/internal/scheduler"
	"github.com/free5gc/dccf/internal/southbound"
	"github.com/free5gc/dccf/internal/storage"
	"github.com/free5gc/dccf/pkg/factory"
)

// App is the high-level interface implemented by DCCF. It hides wiring,
// HTTP server startup and scheduler lifecycle from cmd/main.go.
type App interface {
	// Start brings the whole DCCF instance online. It is expected to:
	//   - initialise logging
	//   - start southbound and northbound HTTP servers
	//   - subscribe to configured UPF EES instances
	//   - start the scheduler
	Start(ctx stdctx.Context) error

	// Stop attempts a graceful shutdown:
	//   - mark shutdown requested
	//   - stop the scheduler
	//   - unsubscribe from UPF EES
	//   - request storage cleanup for affected UPFs
	//
	// Note: HTTP servers are not yet gracefully shut down in the MVP and
	// will typically be brought down when the process exits. This can be
	// extended in future work.
	Stop(ctx stdctx.Context) error
}

// appImpl is the concrete implementation of App.
type appImpl struct {
	config *factory.Config

	runtimeContext dccfctx.RuntimeContext
	storageStore   storage.Store
	aggregator     aggregator.Aggregator
	notifier       northbound.Notifier
	scheduler      scheduler.Scheduler
	upfClient      sbi.UpfEesClient

	southboundReceiver *southbound.UpfEesReceiver
	northboundServer   *sbi.NorthboundServer

	startStopMutex sync.Mutex
	started        bool
}

// NewApp constructs a new App from a validated configuration. It creates
// the internal components but does not start any network listeners yet;
// that is handled by Start().
func NewApp(config *factory.Config) (App, error) {
	if config == nil {
		return nil, fmt.Errorf("config must not be nil")
	}

	// Initialise logging according to configuration. It is safe if main()
	// calls InitLog again; InitLog is idempotent w.r.t logger instances and
	// updates only the level and reportCaller flag.
	if initError := logger.InitLog(config.Logging.Level, config.Logging.ReportCaller); initError != nil {
		// We log a warning but still continue; falling back to "info" is fine.
		logger.MainLog.Warnf("InitLog failed with level=%s, using fallback: %v",
			config.Logging.Level, initError)
	}

	logger.MainLog.Infof(
		"Starting DCCF version=%s description=%q",
		config.Info.Version, config.Info.Description,
	)

	// Build storage backend from configuration.
	storageStore, storageError := storage.NewStoreFromConfig(config.Storage)
	if storageError != nil {
		return nil, fmt.Errorf("failed to create storage backend: %w", storageError)
	}

	// Create runtime context with default BatchMaxItems taken from config.
	runtimeContext := dccfctx.NewRuntimeContext(config.Northbound.BatchMaxItems)

	// Aggregator normalises UPF notifications and talks to storage.
	aggregatorInstance := aggregator.NewAggregator(storageStore)

	// Northbound notifier (push mode) uses HTTP/JSON.
	notifierInstance := northbound.NewHTTPNotifier()

	// Scheduler controls periodic push to subscribers.
	schedulerInstance := scheduler.NewScheduler(
		aggregatorInstance,
		notifierInstance,
		runtimeContext,
		config.Northbound.DelayToleranceSec,
	)

	// Client for Nupf_EventExposure (UPF EES subscribe/unsubscribe).
	upfClient := sbi.NewUpfEesClient()

	// Southbound receiver for UPF EES Notify.
	southboundReceiver := southbound.NewUpfEesReceiver(aggregatorInstance)

	// Northbound Ndccf-like API server.
	northboundServer := sbi.NewNorthboundServer(
		aggregatorInstance,
		runtimeContext,
		config.Northbound.DefaultPeriodSec,
	)

	return &appImpl{
		config:             config,
		runtimeContext:     runtimeContext,
		storageStore:       storageStore,
		aggregator:         aggregatorInstance,
		notifier:           notifierInstance,
		scheduler:          schedulerInstance,
		upfClient:          upfClient,
		southboundReceiver: southboundReceiver,
		northboundServer:   northboundServer,
	}, nil
}

// Start implements App.Start.
func (app *appImpl) Start(ctx stdctx.Context) error {
	app.startStopMutex.Lock()
	defer app.startStopMutex.Unlock()

	if app.started {
		logger.MainLog.Warn("App.Start called more than once; ignoring subsequent call")
		return nil
	}

	// Clear shutdown flag just in case.
	app.runtimeContext.SetShutdownRequested(ctx, false)

	// Start southbound HTTP server (UPF → DCCF Notify).
	// We run it in a dedicated goroutine; in the MVP we do not support
	// programmatic graceful shutdown of this server yet.
	go func(listenAddr string) {
		if listenAddr == "" {
			// This should have been prevented by config validation.
			logger.SouthboundLog.Error("southbound listenAddr is empty; server will not start")
			return
		}

		if serveError := app.southboundReceiver.Serve(listenAddr); serveError != nil {
			logger.SouthboundLog.Errorf("southbound server stopped with error: %v", serveError)
		}
	}(app.config.Southbound.ListenAddr)

	// Start northbound HTTP server (AnLF/MTLF → DCCF).
	go func(listenAddr string) {
		if listenAddr == "" {
			logger.NorthboundLog.Error("northbound listenAddr is empty; server will not start")
			return
		}

		if serveError := app.northboundServer.Serve(listenAddr); serveError != nil {
			logger.NorthboundLog.Errorf("northbound server stopped with error: %v", serveError)
		}
	}(app.config.Northbound.ListenAddr)

	// For each configured UPF, send a USER_DATA_USAGE_MEASURES periodic
	// subscription request and record the resulting state.
	if subscribeError := app.subscribeToConfiguredUPFs(ctx); subscribeError != nil {
		// We log the error but do not fail the whole Start(), because some UPFs
		// might still be reachable and useful.
		logger.MainLog.Warnf("one or more UPF subscriptions failed: %v", subscribeError)
	}

	// Start the scheduler loop for periodic push.
	if schedulerError := app.scheduler.Start(ctx); schedulerError != nil {
		return fmt.Errorf("failed to start scheduler: %w", schedulerError)
	}

	app.started = true
	logger.MainLog.Infof("DCCF successfully started")
	return nil
}

// Stop implements App.Stop.
func (app *appImpl) Stop(ctx stdctx.Context) error {
	app.startStopMutex.Lock()
	defer app.startStopMutex.Unlock()

	if !app.started {
		return nil
	}

	logger.MainLog.Infof("DCCF shutdown requested")

	// Mark shutdown requested so that long-running operations can adapt.
	app.runtimeContext.SetShutdownRequested(ctx, true)

	// Stop scheduler first to avoid pushing more data while we are cleaning up.
	if schedulerError := app.scheduler.Stop(ctx); schedulerError != nil {
		logger.MainLog.Warnf("scheduler stop returned error: %v", schedulerError)
	}

	// Unsubscribe from UPF EES and request storage cleanup per UPF.
	app.unsubscribeFromUPFs(ctx)

	// future work: gracefully shutdown HTTP servers (southbound and
	// northbound) using http.Server.Shutdown and a shared mux/server instead
	// of the per-component Serve() helpers.

	app.started = false
	logger.MainLog.Infof("DCCF shutdown completed")
	return nil
}

// subscribeToConfiguredUPFs iterates over southbound.upstreamUpfs and, for each
// entry, sends a periodic USER_DATA_USAGE_MEASURES subscription to UPF EES.
// It also records the resulting state in RuntimeContext.
func (app *appImpl) subscribeToConfiguredUPFs(ctx stdctx.Context) error {
	var firstError error

	for _, upfEndpoint := range app.config.Southbound.UpstreamUPFs {
		// Construct notifUri for this UPF. The host part of ListenAddr must be
		// reachable from the UPF; in many lab setups this may be a private IP
		// instead of "0.0.0.0".
		notifyURI := fmt.Sprintf(
			"http://%s/nupf-ee/v1/notify/%s",
			app.config.Southbound.ListenAddr,
			upfEndpoint.ID,
		)

		subscriptionID, subscribeError := app.upfClient.Subscribe(ctx, upfEndpoint, notifyURI)
		if subscribeError != nil {
			logger.MainLog.Errorf(
				"failed to subscribe to UPF EES upfId=%s: %v",
				upfEndpoint.ID, subscribeError,
			)
			if firstError == nil {
				firstError = subscribeError
			}
			continue
		}

		// Record the UPF subscription state in the runtime context so that
		// later we can attempt graceful unsubscribe and data cleanup.
		recordError := app.runtimeContext.SetOrUpdateUpfSubscription(
			ctx,
			upfEndpoint.ID,
			upfEndpoint.EESBaseURL,
			notifyURI,
			upfEndpoint.PeriodSec,
			subscriptionID,
		)
		if recordError != nil {
			logger.MainLog.Warnf(
				"failed to record UPF subscription state for upfId=%s subscriptionId=%s: %v",
				upfEndpoint.ID, subscriptionID, recordError,
			)
		}
	}

	return firstError
}

// unsubscribeFromUPFs attempts to cancel existing UPF EES subscriptions and
// remove associated measurements from the storage backend.
func (app *appImpl) unsubscribeFromUPFs(ctx stdctx.Context) {
	upfStates := app.runtimeContext.GetUpfSubscriptionsSnapshot()
	if len(upfStates) == 0 {
		return
	}

	logger.MainLog.Infof("unsubscribing from %d UPF EES subscription(s)", len(upfStates))

	for _, state := range upfStates {
		// Build a minimal UpstreamUPFConfig to feed into the client.
		upfConfig := factory.UpstreamUPFConfig{
			ID:         state.UpfID,
			EESBaseURL: state.EESBaseURL,
			PeriodSec:  state.PeriodSec,
		}

		if state.UpfSubscriptionID == "" {
			continue
		}

		unsubscribeError := app.upfClient.Unsubscribe(ctx, upfConfig, state.UpfSubscriptionID)
		if unsubscribeError != nil {
			logger.MainLog.Warnf(
				"failed to unsubscribe from UPF EES upfId=%s subscriptionId=%s: %v",
				state.UpfID, state.UpfSubscriptionID, unsubscribeError,
			)
		}

		// Best-effort storage cleanup for measurements originating from this UPF.
		deleteError := app.storageStore.DeleteByUPFSubscription(
			ctx,
			state.UpfID,
			state.UpfSubscriptionID,
		)
		if deleteError != nil {
			logger.MainLog.Warnf(
				"failed to delete measurements for upfId=%s subscriptionId=%s: %v",
				state.UpfID, state.UpfSubscriptionID, deleteError,
			)
		}

		// Remove UPF subscription state from runtime context.
		removeError := app.runtimeContext.DeleteUpfSubscription(ctx, state.UpfID)
		if removeError != nil {
			logger.MainLog.Warnf(
				"failed to remove UPF subscription state for upfId=%s: %v",
				state.UpfID, removeError,
			)
		}
	}
}
