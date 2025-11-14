// Package sbi provides service-based interfaces used by DCCF to communicate
// with external network functions. This file implements the client side of
// Nupf_EventExposure towards UPF EES, i.e., subscribe and unsubscribe calls
// for USER_DATA_USAGE_MEASURES.
//
// MVP behavior:
//   - One periodic subscription per configured UPF endpoint
//   - Event:       USER_DATA_USAGE_MEASURES
//   - Granularity: perPduSession
//   - Mode:        PERIODIC
//   - Target:      anyUe = true
//
// On-demand / fetch semantics are handled northbound by DCCF and are not
// requested from UPF in this client.
package sbi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/free5gc/dccf/internal/logger"
	"github.com/free5gc/dccf/internal/model"
	"github.com/free5gc/dccf/pkg/factory"
)

// UpfEesClient is the abstraction used by DCCF to manage subscriptions
// toward UPF EES instances.
type UpfEesClient interface {
	// Subscribe creates a periodic subscription for USER_DATA_USAGE_MEASURES
	// on the given UPF. The notifURI is the callback URL where the UPF will
	// send Notify requests (i.e., DCCF's southbound receiver).
	Subscribe(
		ctx context.Context,
		upfEndpoint factory.UpstreamUPFConfig,
		notifURI string,
	) (subscriptionID string, err error)

	// Unsubscribe cancels an existing subscription identified by subscriptionID
	// on the given UPF.
	Unsubscribe(
		ctx context.Context,
		upfEndpoint factory.UpstreamUPFConfig,
		subscriptionID string,
	) error
}

// -----------------------------------------------------------------------------
// Concrete HTTP client implementation
// -----------------------------------------------------------------------------

// upfEesClient is a concrete implementation of UpfEesClient using net/http.
type upfEesClient struct {
	httpClient         *http.Client
	maxResponseBodyLen int64
}

// upfEesSubscribeRequest is the JSON payload sent to UPF EES when creating
// a subscription.
type upfEesSubscribeRequest struct {
	NotifURI    string                `json:"notifUri"`
	Event       model.EventType       `json:"event"`
	Granularity model.GranularityType `json:"granularity"`
	Mode        string                `json:"mode"`      // "PERIODIC"
	PeriodSec   int                   `json:"periodSec"` // from config
	Target      upfEesTarget          `json:"target"`
}

type upfEesTarget struct {
	AnyUE bool `json:"anyUe"`
}

// upfEesSubscribeResponse is the expected JSON body returned by UPF EES
// when a subscription is successfully created.
type upfEesSubscribeResponse struct {
	SubscriptionID string `json:"subscriptionId"`
}

// NewUpfEesClient creates a new HTTP-based client for UPF EES. It sets
// reasonable timeouts suitable for control-plane workloads.
func NewUpfEesClient() UpfEesClient {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &upfEesClient{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Second,
		},
		maxResponseBodyLen: 4 << 10, // 4 KiB for logging snippets
	}
}

// Subscribe implements UpfEesClient.Subscribe.
func (client *upfEesClient) Subscribe(
	ctx context.Context,
	upfEndpoint factory.UpstreamUPFConfig,
	notifURI string,
) (string, error) {
	if notifURI == "" {
		return "", fmt.Errorf("notifURI must not be empty")
	}
	if upfEndpoint.EESBaseURL == "" {
		return "", fmt.Errorf("UPF %s EES base URL is empty", upfEndpoint.ID)
	}
	if upfEndpoint.PeriodSec <= 0 {
		return "", fmt.Errorf("UPF %s periodSec must be > 0", upfEndpoint.ID)
	}

	requestBody := upfEesSubscribeRequest{
		NotifURI:    notifURI,
		Event:       model.EventUserDataUsageMeasures,
		Granularity: model.GranularityPerPduSession,
		Mode:        "PERIODIC",
		PeriodSec:   upfEndpoint.PeriodSec,
		Target: upfEesTarget{
			AnyUE: true,
		},
	}

	jsonBytes, marshalError := json.Marshal(requestBody)
	if marshalError != nil {
		return "", fmt.Errorf("failed to marshal UPF EES subscribe request: %w", marshalError)
	}

	subscribeURL := joinURL(upfEndpoint.EESBaseURL, "ee-subscriptions")

	httpRequest, requestError := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		subscribeURL,
		bytes.NewReader(jsonBytes),
	)
	if requestError != nil {
		return "", fmt.Errorf("failed to create HTTP request to %s: %w", subscribeURL, requestError)
	}

	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("User-Agent", "dccf-upf-ees-client/1.0")

	logger.SbiLog.Infof(
		"Sending UPF EES Subscribe to upfId=%s url=%s periodSec=%d",
		upfEndpoint.ID, subscribeURL, upfEndpoint.PeriodSec,
	)

	httpResponse, doError := client.httpClient.Do(httpRequest)
	if doError != nil {
		logger.SbiLog.Errorf(
			"UPF EES Subscribe failed for upfId=%s url=%s: %v",
			upfEndpoint.ID, subscribeURL, doError,
		)
		return "", fmt.Errorf("UPF EES subscribe request failed: %w", doError)
	}

	defer func() {
		if closeErr := httpResponse.Body.Close(); closeErr != nil {
			logger.SbiLog.Debugf("failed to close UPF EES response body: %v", closeErr)
		}
	}()

	if httpResponse.StatusCode/100 != 2 {
		bodySnippet := client.readBodySnippet(httpResponse.Body)
		logger.SbiLog.Warnf(
			"UPF EES Subscribe non-2xx for upfId=%s url=%s status=%s bodySnippet=%q",
			upfEndpoint.ID, subscribeURL, httpResponse.Status, bodySnippet,
		)
		return "", fmt.Errorf("UPF EES subscribe non-2xx status: %s", httpResponse.Status)
	}

	var responseBody upfEesSubscribeResponse
	decoder := json.NewDecoder(httpResponse.Body)
	if decodeError := decoder.Decode(&responseBody); decodeError != nil {
		logger.SbiLog.Warnf(
			"UPF EES Subscribe response decode failed for upfId=%s url=%s: %v",
			upfEndpoint.ID, subscribeURL, decodeError,
		)
		return "", fmt.Errorf("failed to decode UPF EES subscribe response: %w", decodeError)
	}

	if responseBody.SubscriptionID == "" {
		return "", fmt.Errorf("UPF EES subscribe returned empty subscriptionId for upfId=%s", upfEndpoint.ID)
	}

	logger.SbiLog.Infof(
		"UPF EES Subscribe success upfId=%s subscriptionId=%s",
		upfEndpoint.ID, responseBody.SubscriptionID,
	)

	return responseBody.SubscriptionID, nil
}

// Unsubscribe implements UpfEesClient.Unsubscribe.
func (client *upfEesClient) Unsubscribe(
	ctx context.Context,
	upfEndpoint factory.UpstreamUPFConfig,
	subscriptionID string,
) error {
	if subscriptionID == "" {
		return fmt.Errorf("subscriptionID must not be empty")
	}
	if upfEndpoint.EESBaseURL == "" {
		return fmt.Errorf("UPF %s EES base URL is empty", upfEndpoint.ID)
	}

	unsubscribeURL := joinURL(upfEndpoint.EESBaseURL, "ee-subscriptions", subscriptionID)

	httpRequest, requestError := http.NewRequestWithContext(
		ctx,
		http.MethodDelete,
		unsubscribeURL,
		nil,
	)
	if requestError != nil {
		return fmt.Errorf("failed to create HTTP request to %s: %w", unsubscribeURL, requestError)
	}

	httpRequest.Header.Set("User-Agent", "dccf-upf-ees-client/1.0")

	logger.SbiLog.Infof(
		"Sending UPF EES Unsubscribe to upfId=%s url=%s subscriptionId=%s",
		upfEndpoint.ID, unsubscribeURL, subscriptionID,
	)

	httpResponse, doError := client.httpClient.Do(httpRequest)
	if doError != nil {
		logger.SbiLog.Errorf(
			"UPF EES Unsubscribe failed for upfId=%s url=%s subscriptionId=%s: %v",
			upfEndpoint.ID, unsubscribeURL, subscriptionID, doError,
		)
		return fmt.Errorf("UPF EES unsubscribe request failed: %w", doError)
	}

	defer func() {
		if closeErr := httpResponse.Body.Close(); closeErr != nil {
			logger.SbiLog.Debugf("failed to close UPF EES response body: %v", closeErr)
		}
	}()

	if httpResponse.StatusCode/100 != 2 && httpResponse.StatusCode != http.StatusNoContent {
		bodySnippet := client.readBodySnippet(httpResponse.Body)
		logger.SbiLog.Warnf(
			"UPF EES Unsubscribe non-2xx for upfId=%s url=%s subscriptionId=%s status=%s bodySnippet=%q",
			upfEndpoint.ID, unsubscribeURL, subscriptionID, httpResponse.Status, bodySnippet,
		)
		return fmt.Errorf("UPF EES unsubscribe non-2xx status: %s", httpResponse.Status)
	}

	logger.SbiLog.Infof(
		"UPF EES Unsubscribe success upfId=%s subscriptionId=%s",
		upfEndpoint.ID, subscriptionID,
	)

	return nil
}

// readBodySnippet reads at most maxResponseBodyLen bytes from the response
// body for logging purposes. It never returns an error and is best-effort only.
func (client *upfEesClient) readBodySnippet(body io.Reader) string {
	if client.maxResponseBodyLen <= 0 {
		return ""
	}

	limitedReader := io.LimitedReader{
		R: body,
		N: client.maxResponseBodyLen,
	}
	rawBytes, readError := io.ReadAll(&limitedReader)
	if readError != nil {
		return ""
	}
	return string(rawBytes)
}

// joinURL safely concatenates base URL and additional path segments using
// a single slash. It does not perform URL escaping on segments, so the
// caller should only pass already-safe path elements.
func joinURL(base string, segments ...string) string {
	trimmedBase := strings.TrimRight(base, "/")
	if len(segments) == 0 {
		return trimmedBase
	}

	var cleanedSegments []string
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		cleanedSegments = append(cleanedSegments, strings.Trim(segment, "/"))
	}

	if len(cleanedSegments) == 0 {
		return trimmedBase
	}

	return trimmedBase + "/" + strings.Join(cleanedSegments, "/")
}
