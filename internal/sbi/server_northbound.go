// Package sbi provides service-based interfaces used by DCCF to communicate
// with external consumers and producers. This file implements the northbound
// HTTP server that exposes Ndccf-like Data Management APIs to AnLF/MTLF.
//
// Exposed endpoints (MVP):
//
//	POST   /ndccf/v1/subscriptions           - create a periodic push subscription
//	DELETE /ndccf/v1/subscriptions/{id}      - delete a subscription
//	POST   /ndccf/v1/fetch                   - one-shot fetch of usage data
//
// Semantics:
//   - Event:       USER_DATA_USAGE_MEASURES
//   - Granularity: perPduSession
//   - Target:      AnyUE only (MVP)
//   - Push:        controlled by subscription.PeriodSec (or defaultPeriodSec)
//   - Fetch:       one-shot, replaces the earlier "on-demand" mixed semantics
package sbi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/free5gc/dccf/internal/aggregator"
	dccfctx "github.com/free5gc/dccf/internal/context"
	"github.com/free5gc/dccf/internal/logger"
	"github.com/free5gc/dccf/internal/model"
)

// NorthboundServer serves Ndccf-like HTTP APIs to AnLF/MTLF.
type NorthboundServer struct {
	aggregator        aggregator.Aggregator
	runtimeContext    dccfctx.RuntimeContext
	defaultPeriodSec  int
	maxRequestBodyLen int64
}

// NewNorthboundServer creates a new northbound server with the given
// aggregator, runtime context and default push period.
func NewNorthboundServer(
	aggregatorInstance aggregator.Aggregator,
	runtimeContext dccfctx.RuntimeContext,
	defaultPeriodSec int,
) *NorthboundServer {
	if defaultPeriodSec <= 0 {
		defaultPeriodSec = 10
	}

	return &NorthboundServer{
		aggregator:        aggregatorInstance,
		runtimeContext:    runtimeContext,
		defaultPeriodSec:  defaultPeriodSec,
		maxRequestBodyLen: 1 << 20, // 1 MiB
	}
}

// Routes registers the northbound handlers on the given mux.
func (server *NorthboundServer) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/ndccf/v1/subscriptions", server.handleSubscriptionsRoot)
	mux.HandleFunc("/ndccf/v1/subscriptions/", server.handleSubscriptionsWithID)
	mux.HandleFunc("/ndccf/v1/fetch", server.handleFetch)
}

// Serve starts a standalone HTTP server for the northbound endpoints.
// In many deployments, an external component (e.g., app package) is expected
// to call Routes() on a shared mux instead of using Serve() directly.
func (server *NorthboundServer) Serve(listenAddr string) error {
	mux := http.NewServeMux()
	server.Routes(mux)

	httpServer := &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	logger.NorthboundLog.Infof("Starting northbound Ndccf server on %s", listenAddr)
	return httpServer.ListenAndServe()
}

// handleSubscriptionsRoot handles POST /ndccf/v1/subscriptions (create).
func (server *NorthboundServer) handleSubscriptionsRoot(
	responseWriter http.ResponseWriter,
	request *http.Request,
) {
	switch request.Method {
	case http.MethodPost:
		server.handleCreateSubscription(responseWriter, request)
	default:
		http.Error(responseWriter, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSubscriptionsWithID handles DELETE /ndccf/v1/subscriptions/{id}.
func (server *NorthboundServer) handleSubscriptionsWithID(
	responseWriter http.ResponseWriter,
	request *http.Request,
) {
	if request.Method != http.MethodDelete {
		http.Error(responseWriter, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	subscriptionID, parseError := parseSubscriptionIDFromPath(request.URL.Path)
	if parseError != nil {
		logger.NorthboundLog.Warnf("failed to parse subscriptionId from path %q: %v", request.URL.Path, parseError)
		http.Error(responseWriter, "bad request", http.StatusBadRequest)
		return
	}

	server.handleDeleteSubscription(responseWriter, request, subscriptionID)
}

// handleCreateSubscription processes a POST /ndccf/v1/subscriptions request.
func (server *NorthboundServer) handleCreateSubscription(
	responseWriter http.ResponseWriter,
	request *http.Request,
) {
	limitedReader := http.MaxBytesReader(responseWriter, request.Body, server.maxRequestBodyLen)

	defer func() {
		if closeErr := limitedReader.Close(); closeErr != nil {
			logger.NorthboundLog.Debugf("failed to close request body reader: %v", closeErr)
		}
	}()

	var subscriptionRequest model.NdccfSubscriptionRequest
	jsonDecoder := json.NewDecoder(limitedReader)
	if decodeError := jsonDecoder.Decode(&subscriptionRequest); decodeError != nil {
		logger.NorthboundLog.Warnf("failed to decode NdccfSubscriptionRequest: %v", decodeError)
		http.Error(responseWriter, "invalid JSON body", http.StatusBadRequest)
		return
	}

	validationError := server.validateSubscriptionRequest(subscriptionRequest)
	if validationError != nil {
		logger.NorthboundLog.Warnf("invalid subscription request: %v", validationError)
		http.Error(responseWriter, validationError.Error(), http.StatusBadRequest)
		return
	}

	effectivePeriodSec := subscriptionRequest.PeriodSec
	if effectivePeriodSec <= 0 {
		effectivePeriodSec = server.defaultPeriodSec
	}

	subscriptionID, createError := server.runtimeContext.CreateNorthboundSubscription(
		request.Context(),
		subscriptionRequest,
		effectivePeriodSec,
	)
	if createError != nil {
		logger.NorthboundLog.Errorf("failed to create northbound subscription: %v", createError)
		http.Error(responseWriter, "internal server error", http.StatusInternalServerError)
		return
	}

	response := model.NdccfSubscriptionResponse{
		SubscriptionID: subscriptionID,
	}

	responseWriter.Header().Set("Content-Type", "application/json")
	responseWriter.WriteHeader(http.StatusCreated)

	if encodeError := json.NewEncoder(responseWriter).Encode(response); encodeError != nil {
		logger.NorthboundLog.Warnf(
			"failed to encode NdccfSubscriptionResponse for subscriptionId=%s: %v",
			subscriptionID, encodeError,
		)
	}
}

// handleDeleteSubscription processes a DELETE /ndccf/v1/subscriptions/{id} request.
func (server *NorthboundServer) handleDeleteSubscription(
	responseWriter http.ResponseWriter,
	request *http.Request,
	subscriptionID string,
) {
	deleted, deleteError := server.runtimeContext.DeleteNorthboundSubscription(
		request.Context(),
		subscriptionID,
	)
	if deleteError != nil {
		logger.NorthboundLog.Errorf(
			"failed to delete northbound subscriptionId=%s: %v",
			subscriptionID, deleteError,
		)
		http.Error(responseWriter, "internal server error", http.StatusInternalServerError)
		return
	}

	if !deleted {
		http.Error(responseWriter, "subscription not found", http.StatusNotFound)
		return
	}

	responseWriter.WriteHeader(http.StatusNoContent)
}

// handleFetch processes a POST /ndccf/v1/fetch request.
func (server *NorthboundServer) handleFetch(
	responseWriter http.ResponseWriter,
	request *http.Request,
) {
	if request.Method != http.MethodPost {
		http.Error(responseWriter, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limitedReader := http.MaxBytesReader(responseWriter, request.Body, server.maxRequestBodyLen)

	defer func() {
		if closeErr := limitedReader.Close(); closeErr != nil {
			logger.NorthboundLog.Debugf("failed to close request body reader: %v", closeErr)
		}
	}()

	var fetchRequest model.NdccfFetchRequest
	jsonDecoder := json.NewDecoder(limitedReader)
	if decodeError := jsonDecoder.Decode(&fetchRequest); decodeError != nil {
		logger.NorthboundLog.Warnf("failed to decode NdccfFetchRequest: %v", decodeError)
		http.Error(responseWriter, "invalid JSON body", http.StatusBadRequest)
		return
	}

	validationError := server.validateFetchRequest(fetchRequest)
	if validationError != nil {
		logger.NorthboundLog.Warnf("invalid fetch request: %v", validationError)
		http.Error(responseWriter, validationError.Error(), http.StatusBadRequest)
		return
	}

	var since time.Time
	var until time.Time
	if fetchRequest.Since != nil {
		since = *fetchRequest.Since
	}
	if fetchRequest.Until != nil {
		until = *fetchRequest.Until
	}

	limit := fetchRequest.Limit
	if limit <= 0 {
		limit = 1000
	}

	measurements, queryError := server.aggregator.BuildBatchForInterval(
		request.Context(),
		since,
		until,
		fetchRequest.AnyUE,
		limit,
	)
	if queryError != nil {
		logger.NorthboundLog.Errorf(
			"failed to build fetch batch (since=%s until=%s anyUE=%t limit=%d): %v",
			formatTimeForLog(since), formatTimeForLog(until), fetchRequest.AnyUE, limit, queryError,
		)
		http.Error(responseWriter, "internal server error", http.StatusInternalServerError)
		return
	}

	responsePayload := model.NdccfNotify{
		SubscriptionID: "", // fetch is one-shot, no persistent subscriptionId
		EventID:        fetchRequest.Event,
		Granularity:    fetchRequest.Granularity,
		Timestamp:      time.Now().UTC(),
		Items:          measurements,
	}

	responseWriter.Header().Set("Content-Type", "application/json")
	responseWriter.WriteHeader(http.StatusOK)

	if encodeError := json.NewEncoder(responseWriter).Encode(responsePayload); encodeError != nil {
		logger.NorthboundLog.Warnf(
			"failed to encode NdccfNotify for fetch (since=%s until=%s): %v",
			formatTimeForLog(since), formatTimeForLog(until), encodeError,
		)
	}
}

// validateSubscriptionRequest performs basic semantic checks on a subscription
// request before it is passed to the runtime context.
func (server *NorthboundServer) validateSubscriptionRequest(
	request model.NdccfSubscriptionRequest,
) error {
	if request.Event != model.EventUserDataUsageMeasures {
		return fmt.Errorf("unsupported event %q (only %q is supported in MVP)",
			request.Event, model.EventUserDataUsageMeasures,
		)
	}

	if request.Granularity != model.GranularityPerPduSession {
		return fmt.Errorf("unsupported granularity %q (only %q is supported in MVP)",
			request.Granularity, model.GranularityPerPduSession,
		)
	}

	if !request.AnyUE {
		return fmt.Errorf("only anyUe=true is supported in MVP")
	}

	// If push is desired, notifUri must be non-empty. If notifUri is empty,
	// the subscription can still exist but will be effectively "pull-only"
	// (future work).
	if request.NotifURI == "" {
		logger.NorthboundLog.Debugf(
			"creating subscription without notifUri (pull-only mode) is allowed in MVP",
		)
	}

	if request.PeriodSec < 0 {
		return fmt.Errorf("periodSec must be >= 0")
	}

	return nil
}

// validateFetchRequest performs basic semantic checks on a fetch request.
func (server *NorthboundServer) validateFetchRequest(
	request model.NdccfFetchRequest,
) error {
	if request.Event != model.EventUserDataUsageMeasures {
		return fmt.Errorf("unsupported event %q (only %q is supported in MVP)",
			request.Event, model.EventUserDataUsageMeasures,
		)
	}

	if request.Granularity != model.GranularityPerPduSession {
		return fmt.Errorf("unsupported granularity %q (only %q is supported in MVP)",
			request.Granularity, model.GranularityPerPduSession,
		)
	}

	if !request.AnyUE {
		return fmt.Errorf("only anyUe=true is supported in MVP")
	}

	if request.Since != nil && request.Until != nil && request.Since.After(*request.Until) {
		return fmt.Errorf("since must not be after until")
	}

	if request.Limit < 0 {
		return fmt.Errorf("limit must be >= 0")
	}

	return nil
}

// parseSubscriptionIDFromPath extracts the subscription ID from a path of the form
// /ndccf/v1/subscriptions/{id} or /ndccf/v1/subscriptions/{id}/.
func parseSubscriptionIDFromPath(path string) (string, error) {
	const prefix = "/ndccf/v1/subscriptions/"

	if !strings.HasPrefix(path, prefix) {
		return "", fmt.Errorf("path %q does not start with %q", path, prefix)
	}

	trimmed := strings.TrimPrefix(path, prefix)
	trimmed = strings.Trim(trimmed, "/")

	if trimmed == "" {
		return "", fmt.Errorf("missing subscriptionId in path %q", path)
	}

	return trimmed, nil
}

// formatTimeForLog reuses the helper from aggregator for compact logging.
func formatTimeForLog(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Format(time.RFC3339Nano)
}
