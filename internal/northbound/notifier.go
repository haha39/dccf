// Package northbound provides helper utilities for delivering Ndccf-like
// notifications from DCCF to external consumers such as AnLF/MTLF.
//
// This file implements a Notifier abstraction and an HTTP-based concrete
// implementation suitable for pushing JSON-encoded NdccfNotify payloads
// to subscriber-provided notifUri endpoints.
package northbound

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/free5gc/dccf/internal/logger"
	"github.com/free5gc/dccf/internal/model"
)

// Notifier hides the details of how notifications are delivered. The MVP
// implementation uses HTTP POST with JSON, but future implementations could
// use message buses or other transports without changing the caller.
type Notifier interface {
	// Notify sends a single NdccfNotify payload to the given notifURI.
	Notify(ctx context.Context, notifURI string, notification model.NdccfNotify) error
}

// httpNotifier is the concrete HTTP/JSON implementation of Notifier.
type httpNotifier struct {
	httpClient         *http.Client
	maxResponseBodyLen int64
}

// NewHTTPNotifier creates a new Notifier that delivers notifications via
// HTTP POST with a JSON body.
func NewHTTPNotifier() Notifier {
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

	return &httpNotifier{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Second,
		},
		maxResponseBodyLen: 4 << 10, // 4 KiB for logging snippets
	}
}

// Notify implements the Notifier interface.
func (notifier *httpNotifier) Notify(
	ctx context.Context,
	notifURI string,
	notification model.NdccfNotify,
) error {
	if notifURI == "" {
		return fmt.Errorf("notifUri must not be empty")
	}

	jsonBytes, marshalError := json.Marshal(notification)
	if marshalError != nil {
		return fmt.Errorf("failed to marshal NdccfNotify payload: %w", marshalError)
	}

	httpRequest, requestError := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		notifURI,
		bytes.NewReader(jsonBytes),
	)
	if requestError != nil {
		return fmt.Errorf("failed to create HTTP request to %s: %w", notifURI, requestError)
	}

	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("User-Agent", "dccf-northbound-notifier/1.0")

	logger.NorthboundLog.Debugf(
		"Sending NdccfNotify to notifUri=%s items=%d eventId=%s granularity=%s",
		notifURI,
		len(notification.Items),
		notification.EventID,
		notification.Granularity,
	)

	httpResponse, doError := notifier.httpClient.Do(httpRequest)
	if doError != nil {
		logger.NorthboundLog.Errorf(
			"NdccfNotify delivery failed to notifUri=%s: %v",
			notifURI, doError,
		)
		return fmt.Errorf("NdccfNotify delivery failed: %w", doError)
	}

	defer func() {
		if closeErr := httpResponse.Body.Close(); closeErr != nil {
			logger.NorthboundLog.Debugf("failed to close response body: %v", closeErr)
		}
	}()

	if httpResponse.StatusCode/100 != 2 {
		bodySnippet := notifier.readBodySnippet(httpResponse.Body)
		logger.NorthboundLog.Warnf(
			"NdccfNotify delivery non-2xx status=%s notifUri=%s bodySnippet=%q",
			httpResponse.Status, notifURI, bodySnippet,
		)
		return fmt.Errorf("NdccfNotify delivery non-2xx status: %s", httpResponse.Status)
	}

	logger.NorthboundLog.Debugf(
		"NdccfNotify delivery success notifUri=%s items=%d",
		notifURI,
		len(notification.Items),
	)

	return nil
}

// readBodySnippet reads at most maxResponseBodyLen bytes from the response
// body for logging purposes. It never returns an error and is best-effort only.
func (notifier *httpNotifier) readBodySnippet(body io.Reader) string {
	if notifier.maxResponseBodyLen <= 0 {
		return ""
	}

	limitedReader := io.LimitedReader{
		R: body,
		N: notifier.maxResponseBodyLen,
	}
	rawBytes, readError := io.ReadAll(&limitedReader)
	if readError != nil {
		return ""
	}
	return string(rawBytes)
}
