package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	actiondomain "miniai/internal/actiondomain"
)

const (
	miniDeployRestartTimeout          = 2 * time.Minute
	miniDeployRestartMaxResponseBytes = 16 << 10
)

type miniDeployRestartClient struct {
	baseURL    string
	httpClient *http.Client
	timeout    time.Duration
}

type miniDeployRestartFailure struct {
	category     ActionExecutionFailureCategory
	outcomeKnown bool
}

func (failure miniDeployRestartFailure) Error() string {
	return "MiniDeploy restart failed: " + string(failure.category)
}

func newMiniDeployRestartClient(
	baseURL string,
	httpClient *http.Client,
) (miniDeployRestartClient, error) {
	return newMiniDeployRestartClientWithTimeout(
		baseURL,
		httpClient,
		miniDeployRestartTimeout,
	)
}

func newMiniDeployRestartClientWithTimeout(
	baseURL string,
	httpClient *http.Client,
	timeout time.Duration,
) (miniDeployRestartClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Opaque != "" || timeout <= 0 {

		return miniDeployRestartClient{}, errors.New("invalid MiniDeploy restart configuration")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	boundedClient := &http.Client{
		Transport: httpClient.Transport,
		Jar:       httpClient.Jar,
		Timeout:   httpClient.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return miniDeployRestartClient{
		baseURL: baseURL, httpClient: boundedClient, timeout: timeout,
	}, nil
}

func (client miniDeployRestartClient) RestartApplication(
	ctx context.Context,
	applicationID string,
) error {
	if ctx == nil || !actiondomain.ValidApplicationTargetID(applicationID) {
		return miniDeployRestartFailure{
			category: ActionExecutionFailureInternalError, outcomeKnown: true,
		}
	}
	requestContext, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	if requestContext.Err() != nil {
		return miniDeployRestartFailure{
			category: ActionExecutionFailureTimeout, outcomeKnown: true,
		}
	}

	endpoint := client.baseURL + "/deployments/" + url.PathEscape(applicationID) + "/restart"
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, nil)
	if err != nil {
		return miniDeployRestartFailure{
			category: ActionExecutionFailureInternalError, outcomeKnown: true,
		}
	}
	var requestWritten atomic.Bool
	request = request.WithContext(httptrace.WithClientTrace(
		request.Context(),
		&httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) {
			requestWritten.Store(true)
		}},
	))
	response, err := client.httpClient.Do(request)
	if err != nil {
		category := ActionExecutionFailureUpstreamUnavailable
		if requestContext.Err() != nil {
			category = ActionExecutionFailureTimeout
		}
		return miniDeployRestartFailure{
			category: category, outcomeKnown: !requestWritten.Load(),
		}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound ||
		response.StatusCode == http.StatusConflict {

		return miniDeployRestartFailure{
			category: ActionExecutionFailureUpstreamRejected, outcomeKnown: true,
		}
	}
	if response.StatusCode != http.StatusOK {
		return miniDeployRestartFailure{
			category: ActionExecutionFailureUpstreamRejected, outcomeKnown: false,
		}
	}
	payload, err := io.ReadAll(io.LimitReader(
		response.Body,
		miniDeployRestartMaxResponseBytes+1,
	))
	if err != nil || len(payload) > miniDeployRestartMaxResponseBytes {
		return miniDeployRestartFailure{
			category: ActionExecutionFailureInvalidResponse, outcomeKnown: false,
		}
	}
	var result struct {
		Status    string `json:"status"`
		App       string `json:"app"`
		Container string `json:"container"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || ensureJSONEOF(decoder) != nil ||
		result.Status != "running" || result.App != applicationID ||
		strings.TrimSpace(result.Container) == "" || len(result.Container) > 512 {

		return miniDeployRestartFailure{
			category: ActionExecutionFailureInvalidResponse, outcomeKnown: false,
		}
	}
	return nil
}

func classifyMiniDeployRestartFailure(err error) miniDeployRestartFailure {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return miniDeployRestartFailure{
			category:     ActionExecutionFailureTimeout,
			outcomeKnown: false,
		}
	}
	var failure miniDeployRestartFailure
	if errors.As(err, &failure) &&
		actiondomain.ValidExecutionFailureCategory(failure.category) {

		return failure
	}
	return miniDeployRestartFailure{
		category:     ActionExecutionFailureInternalError,
		outcomeKnown: true,
	}
}
