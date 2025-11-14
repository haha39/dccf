// cmd/main.go
//
// Entry point for the DCCF NF. Responsibilities:
//   - Parse command-line flags (config path, etc.).
//   - Initialise a temporary logger so config loading has a logger.
//   - Load and validate configuration from YAML.
//   - Construct the App (wires all internal components).
//   - Start the App and block until SIGINT/SIGTERM.
//   - Trigger a best-effort graceful shutdown on signal.
package main

import (
	stdctx "context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/free5gc/dccf/internal/logger"
	"github.com/free5gc/dccf/pkg/app"
	"github.com/free5gc/dccf/pkg/factory"
)

func main() {
	// ---- 1. Parse flags ------------------------------------------------------

	configPath := flag.String("c", factory.DccfDefaultConfigPath, "path to DCCF config file (YAML)")
	flag.Parse()

	// ---- 2. Temporary logger initialisation ---------------------------------
	//
	// We initialise logging with a safe default so that configuration loading
	// and validation can use logger.CfgLog / logger.MainLog. NewApp() will call
	// InitLog again with the level from the config, which is safe.
	_ = logger.InitLog("info", false)

	logger.MainLog.Infof("DCCF starting, configPath=%s", *configPath)

	// ---- 3. Load configuration ----------------------------------------------

	config, readError := factory.ReadConfig(*configPath)
	if readError != nil {
		logger.MainLog.Errorf("failed to read config: %v", readError)
		os.Exit(1)
	}

	// ---- 4. Build App --------------------------------------------------------

	dccfApp, appError := app.NewApp(config)
	if appError != nil {
		logger.MainLog.Errorf("failed to create DCCF app: %v", appError)
		os.Exit(1)
	}

	// Root context for Start; Stop will create its own timeout context.
	rootContext, rootCancel := stdctx.WithCancel(stdctx.Background())
	if startError := dccfApp.Start(rootContext); startError != nil {
		logger.MainLog.Errorf("failed to start DCCF: %v", startError)
		rootCancel()
		os.Exit(1)
	}

	// ---- 5. Start DCCF -------------------------------------------------------

	if startError := dccfApp.Start(rootContext); startError != nil {
		logger.MainLog.Errorf("failed to start DCCF: %v", startError)
		rootCancel()
		os.Exit(1)
	}

	// ---- 6. Wait for OS signals (Ctrl-C / kill) -----------------------------

	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel, syscall.SIGINT, syscall.SIGTERM)

	receivedSignal := <-signalChannel
	logger.MainLog.Infof("received signal=%s, initiating shutdown", receivedSignal.String())

	// Let any Start()-spawned logic that honours the root context know we are
	// shutting down.
	rootCancel()

	// ---- 7. Graceful shutdown ------------------------------------------------
	//
	// We give the App a bounded time window to finish cleanup. If it cannot
	// complete in time, we log a warning and exit anyway.
	shutdownTimeout := 10 * time.Second
	shutdownContext, shutdownCancel := stdctx.WithTimeout(stdctx.Background(), shutdownTimeout)
	defer shutdownCancel()

	if stopError := dccfApp.Stop(shutdownContext); stopError != nil {
		logger.MainLog.Warnf("DCCF shutdown encountered error: %v", stopError)
	} else {
		logger.MainLog.Infof("DCCF shutdown completed within %s", shutdownTimeout)
	}
}
