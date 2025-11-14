// Package southbound exposes the HTTP endpoint where DCCF receives UPF EES
// (Nupf_EventExposure) Notify messages for USER_DATA_USAGE_MEASURES.
//
// Expected URL pattern (per UPF):
//
//	POST /nupf-ee/v1/notify/{upfId}
//
// The {upfId} segment must match the ID configured in dccfcfg.yaml under
// southbound.upstreamUpfs[].id, and is typically embedded into the notifUri
// sent in the UPF EES Subscribe requests.
//
// This receiver:
//   - Parses the UPF ID from the request path
//   - Decodes the JSON Notify payload into model.UpfEesNotify
//   - Performs lightweight validation (eventId / granularity)
//   - Hands the notification over to the Aggregator for further processing
package southbound

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/free5gc/dccf/internal/aggregator"
	"github.com/free5gc/dccf/internal/logger"
	"github.com/free5gc/dccf/internal/model"
)

// UpfEesReceiver handles incoming Notify requests from UPF EES.
type UpfEesReceiver struct {
	aggregator      aggregator.Aggregator
	maxRequestBytes int64
}

// NewUpfEesReceiver creates a new receiver that forwards decoded UPF EES
// notifications to the given Aggregator.
func NewUpfEesReceiver(targetAggregator aggregator.Aggregator) *UpfEesReceiver {
	return &UpfEesReceiver{
		aggregator:      targetAggregator,
		maxRequestBytes: 1 << 20, // 1 MiB limit for Notify payloads
	}
}

// Routes registers the southbound handler on the given mux. It mounts a
// prefix handler under /nupf-ee/v1/notify/ and expects the last path segment
// to be the UPF ID.
func (receiver *UpfEesReceiver) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/nupf-ee/v1/notify/", receiver.HandleNotifyFromUPF)
}

// Serve starts a standalone HTTP server for the southbound endpoint. In many
// deployments, an external component (e.g., app package) is expected to call
// Routes() on a shared mux instead of using Serve() directly.
func (receiver *UpfEesReceiver) Serve(listenAddr string) error {
	mux := http.NewServeMux()
	receiver.Routes(mux)

	server := &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	logger.SouthboundLog.Infof("Starting southbound UPF EES receiver on %s", listenAddr)
	return server.ListenAndServe()
}

// HandleNotifyFromUPF processes a single Notify request from a UPF EES.
// It expects the path to match /nupf-ee/v1/notify/{upfId} and the body to
// contain a JSON-encoded model.UpfEesNotify.
//
// On success, it returns 204 No Content. On client-side errors, it returns
// 4xx; on server-side errors, it returns 5xx.
func (receiver *UpfEesReceiver) HandleNotifyFromUPF(
	responseWriter http.ResponseWriter,
	request *http.Request,
) {
	if request.Method != http.MethodPost {
		http.Error(responseWriter, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	upfID, parseError := parseUpfIDFromPath(request.URL.Path)
	if parseError != nil {
		logger.SouthboundLog.Warnf("failed to parse upfId from path %q: %v", request.URL.Path, parseError)
		http.Error(responseWriter, "bad request", http.StatusBadRequest)
		return
	}

	limitedReader := http.MaxBytesReader(responseWriter, request.Body, receiver.maxRequestBytes)

	defer func() {
		if closeErr := limitedReader.Close(); closeErr != nil {
			logger.SouthboundLog.Debugf("failed to close request body reader: %v", closeErr)
		}
	}()

	var notifyPayload model.UpfEesNotify
	jsonDecoder := json.NewDecoder(limitedReader)
	if decodeError := jsonDecoder.Decode(&notifyPayload); decodeError != nil {
		logger.SouthboundLog.Warnf(
			"failed to decode UPF EES Notify from upfId=%s: %v",
			upfID, decodeError,
		)
		http.Error(responseWriter, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// Lightweight semantic checks; we still accept the message but log warnings
	// if we see unexpected eventId or granularity.
	if notifyPayload.EventID != model.EventUserDataUsageMeasures {
		logger.SouthboundLog.Warnf(
			"received Notify with unexpected eventId=%q from upfId=%s (expected %q)",
			notifyPayload.EventID, upfID, model.EventUserDataUsageMeasures,
		)
	}
	if notifyPayload.Granularity != model.GranularityPerPduSession {
		logger.SouthboundLog.Warnf(
			"received Notify with unexpected granularity=%q from upfId=%s (expected %q)",
			notifyPayload.Granularity, upfID, model.GranularityPerPduSession,
		)
	}

	if len(notifyPayload.Items) == 0 {
		logger.SouthboundLog.Debugf(
			"received empty USER_DATA_USAGE_MEASURES Notify from upfId=%s subscriptionId=%s",
			upfID, notifyPayload.SubscriptionID,
		)
		// Still return 204; empty intervals are not an error.
		responseWriter.WriteHeader(http.StatusNoContent)
		return
	}

	itemCount, ingestError := receiver.aggregator.IngestUPFNotify(
		request.Context(),
		upfID,
		notifyPayload.SubscriptionID,
		notifyPayload,
	)
	if ingestError != nil {
		logger.SouthboundLog.Errorf(
			"failed to ingest Notify from upfId=%s subscriptionId=%s: %v",
			upfID, notifyPayload.SubscriptionID, ingestError,
		)
		http.Error(responseWriter, "internal server error", http.StatusInternalServerError)
		return
	}

	logger.SouthboundLog.Debugf(
		"successfully ingested %d USER_DATA_USAGE_MEASURES item(s) from upfId=%s subscriptionId=%s",
		itemCount, upfID, notifyPayload.SubscriptionID,
	)

	responseWriter.WriteHeader(http.StatusNoContent)
}

// parseUpfIDFromPath extracts the UPF ID from a path of the form
// /nupf-ee/v1/notify/{upfId} or /nupf-ee/v1/notify/{upfId}/.
func parseUpfIDFromPath(path string) (string, error) {
	const prefix = "/nupf-ee/v1/notify/"

	if !strings.HasPrefix(path, prefix) {
		return "", fmt.Errorf("path %q does not start with %q", path, prefix)
	}

	trimmed := strings.TrimPrefix(path, prefix)
	trimmed = strings.Trim(trimmed, "/")

	if trimmed == "" {
		return "", fmt.Errorf("missing upfId in path %q", path)
	}

	return trimmed, nil
}
