package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestMiniDeployRestartClientUsesEstablishedTypedContract(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.EscapedPath() != "/deployments/myscheduler/restart" {
			t.Errorf("request=%s %s", r.Method, r.URL.EscapedPath())
		}
		if r.Body == nil {
			t.Error("request body is unexpectedly nil")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"running","app":"myscheduler","container":"minideploy-myscheduler"}`))
	}))
	defer server.Close()

	client, err := newMiniDeployRestartClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RestartApplication(context.Background(), "myscheduler"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("restart calls=%d", calls.Load())
	}
}

func TestMiniDeployRestartClientClassifiesFailuresWithoutRetry(t *testing.T) {
	tests := []struct {
		name             string
		handler          http.HandlerFunc
		wantCategory     ActionExecutionFailureCategory
		wantOutcomeKnown bool
		timeout          time.Duration
	}{
		{
			name: "deployment not found is definitive",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "internal detail must not escape", http.StatusNotFound)
			},
			wantCategory:     ActionExecutionFailureUpstreamRejected,
			wantOutcomeKnown: true,
			timeout:          time.Second,
		},
		{
			name: "detached database rejection is definitive",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "internal detail must not escape", http.StatusConflict)
			},
			wantCategory:     ActionExecutionFailureUpstreamRejected,
			wantOutcomeKnown: true,
			timeout:          time.Second,
		},
		{
			name: "server error after possible mutation is ambiguous",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "partial restart detail must not escape", http.StatusInternalServerError)
			},
			wantCategory:     ActionExecutionFailureUpstreamRejected,
			wantOutcomeKnown: false,
			timeout:          time.Second,
		},
		{
			name: "invalid success response",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"status":"running","app":"wrong","container":"value"}`))
			},
			wantCategory:     ActionExecutionFailureInvalidResponse,
			wantOutcomeKnown: false,
			timeout:          time.Second,
		},
		{
			name: "timeout after dispatch is ambiguous",
			handler: func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			},
			wantCategory:     ActionExecutionFailureTimeout,
			wantOutcomeKnown: false,
			timeout:          20 * time.Millisecond,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				test.handler(w, r)
			}))
			defer server.Close()
			client, err := newMiniDeployRestartClientWithTimeout(
				server.URL,
				server.Client(),
				test.timeout,
			)
			if err != nil {
				t.Fatal(err)
			}
			err = client.RestartApplication(context.Background(), "myscheduler")
			var failure miniDeployRestartFailure
			if !errors.As(err, &failure) || failure.category != test.wantCategory ||
				failure.outcomeKnown != test.wantOutcomeKnown {

				t.Fatalf("restart error=%#v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("restart calls=%d", calls.Load())
			}
			if test.wantCategory == ActionExecutionFailureUpstreamRejected &&
				err.Error() != "MiniDeploy restart failed: upstream_rejected" {

				t.Fatalf("unsafe error=%q", err)
			}
		})
	}
}

func TestMiniDeployRestartClientDoesNotFollowRedirects(t *testing.T) {
	var redirectedCalls atomic.Int32
	redirected := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedCalls.Add(1)
	}))
	defer redirected.Close()
	var sourceCalls atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sourceCalls.Add(1)
		w.Header().Set("Location", redirected.URL+"/unexpected")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	client, err := newMiniDeployRestartClient(source.URL, source.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.RestartApplication(context.Background(), "myscheduler")
	var failure miniDeployRestartFailure
	if !errors.As(err, &failure) ||
		failure.category != ActionExecutionFailureUpstreamRejected ||
		failure.outcomeKnown {

		t.Fatalf("redirect error=%v", err)
	}
	if sourceCalls.Load() != 1 || redirectedCalls.Load() != 0 {
		t.Fatalf(
			"source calls=%d redirected calls=%d",
			sourceCalls.Load(),
			redirectedCalls.Load(),
		)
	}
}

func TestMiniDeployRestartClientRejectsInvalidInputBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	client, err := newMiniDeployRestartClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, applicationID := range []string{"", "../escape", "My Scheduler", "https://example.com"} {
		err := client.RestartApplication(context.Background(), applicationID)
		var failure miniDeployRestartFailure
		if !errors.As(err, &failure) ||
			failure.category != ActionExecutionFailureInternalError ||
			!failure.outcomeKnown {

			t.Fatalf("application %q error=%v", applicationID, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid input made %d network calls", calls.Load())
	}
}
