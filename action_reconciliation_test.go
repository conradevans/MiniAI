package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestActionOutcomeSurvivesReopenAndReadOnlyHistoryReconciliation(t *testing.T) {
	for _, outcome := range []string{"succeeded", "failed", "outcome_unknown", "interrupted"} {
		t.Run(outcome, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "actions.db")
			store, err := openChatStore(path)
			if err != nil {
				t.Fatal(err)
			}
			history := startAdminActionHistory(t, store, "inspect canary")
			created, err := store.createAssistantRestartSuggestion(history.ID, ActionTarget{Type: ActionTargetApplication, ID: "miniai-restart-canary"})
			if err != nil {
				t.Fatal(err)
			}
			authorized, err := store.confirmAction(created.Proposal.ID())
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			// After dispatch the upstream target is running even when its reply
			// is lost. The result may only follow execution evidence, not state.
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = w.Write([]byte(`{"status":"running"}`))
					return
				}
				calls.Add(1)
				switch outcome {
				case "succeeded":
					_, _ = w.Write([]byte(`{"status":"running","app":"miniai-restart-canary","container":"canary"}`))
				case "failed":
					http.Error(w, "private rejection token /secret/path", 404)
				default: // Disconnect after accepting a POST, before a usable reply.
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
				}
			}))
			defer upstream.Close()
			boundary, token := cryptographicAdminTestBoundary(t)
			handler := newAdminActionTypedMiniDeployTestHandler(t, store, boundary, upstream)
			var immediate adminActionResponse
			if outcome == "interrupted" {
				if _, err := store.beginRestartApplicationExecution(authorized.Proposal); err != nil {
					t.Fatal(err)
				}
			} else {
				response := performAdminActionHTTPRequest(handler, http.MethodPost, actionExecutionTestURL(created.Proposal.ID()), "", token, testAdminOrigin)
				if response.Code != 200 && response.Code != 202 && response.Code != 502 {
					t.Fatalf("execute: %d %s", response.Code, response.Body.String())
				}
				immediate = decodeAdminActionHTTPResponse(t, response)
			}
			beforeEvents, err := store.listActionEvents(created.Proposal.ID())
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := openChatStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			handler = newAdminActionTypedMiniDeployTestHandler(t, reopened, boundary, upstream)
			want := outcome
			if want == "interrupted" {
				want = "outcome_unknown"
			}
			for range 3 {
				get := performAdminActionHTTPRequest(handler, http.MethodGet, testAdminOrigin+"/api/admin/actions/"+string(created.Proposal.ID()), "", token, "")
				loaded := decodeAdminActionHTTPResponse(t, get)
				if get.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("reconciliation can be cached")
				}
				if string(loaded.ExecutionOutcome) != want || loaded.ExecutionStartedAt == nil {
					t.Fatalf("restored %+v", loaded)
				}
				if outcome != "interrupted" && !reflect.DeepEqual(immediate, loaded) {
					t.Fatalf("Execute/GET differ:\n%+v\n%+v", immediate, loaded)
				}
				if want == "outcome_unknown" && (loaded.State != ActionStateExecuting || loaded.ExecutionCompletedAt != nil) {
					t.Fatalf("unknown became terminal: %+v", loaded)
				}
				if outcome != "interrupted" && loaded.OutcomeRecordedAt == nil {
					t.Fatal("missing audit timestamp")
				}
				linked := performAdminActionHTTPRequest(handler, http.MethodGet, testAdminOrigin+"/api/admin/actions?history_id="+history.ID, "", token, "")
				var restored adminHistoryActionsResponse
				if err := json.Unmarshal(linked.Body.Bytes(), &restored); err != nil {
					t.Fatal(err)
				}
				if len(restored.Actions) != 1 || !reflect.DeepEqual(loaded, restored.Actions[0]) {
					t.Fatalf("History loses outcome: %s", linked.Body.String())
				}
				if strings.Contains(get.Body.String(), "/secret/path") {
					t.Fatal("private error leaked")
				}
			}
			afterEvents, err := reopened.listActionEvents(created.Proposal.ID())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeEvents, afterEvents) {
				t.Fatal("GET/reopen changed audit trail")
			}
			if outcome == "outcome_unknown" {
				if afterEvents[len(afterEvents)-1].Type != ActionEventExecutionOutcomeUnknown {
					t.Fatal("ambiguity missing from audit")
				}
				if _, err := reopened.completeRestartApplicationSuccess(created.Proposal.ID()); !errors.Is(err, errActionExecutionCompletionNotAllowed) {
					t.Fatalf("unknown was relabeled success: %v", err)
				}
			}
			duplicate := performAdminActionHTTPRequest(handler, http.MethodPost, actionExecutionTestURL(created.Proposal.ID()), "", token, testAdminOrigin)
			if duplicate.Code != 409 {
				t.Fatalf("duplicate execute: %d", duplicate.Code)
			}
			wantCalls := int32(1)
			if outcome == "interrupted" {
				wantCalls = 0
			}
			if calls.Load() != wantCalls {
				t.Fatalf("reconciliation retried: %d calls", calls.Load())
			}
		})
	}
}

func TestUnknownOutcomeAndSuccessCompletionHaveOneWinner(t *testing.T) {
	store := newActionTestStore(t)
	created := mustCreateRestart(t, store, ActionOriginDirectUser)
	if _, err := store.beginRestartApplicationExecution(created.Proposal); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() {
		_, err := store.recordRestartApplicationUnknown(created.Proposal.ID(), ActionExecutionFailureTimeout)
		results <- err
	}()
	go func() { _, err := store.completeRestartApplicationSuccess(created.Proposal.ID()); results <- err }()
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("completion winners: %v, %v", first, second)
	}
	if _, err := store.getActionProposal(created.Proposal.ID()); err != nil {
		t.Fatal(err)
	}
}

func TestUnclassifiedExecutorErrorCannotClaimDefinitiveFailure(t *testing.T) {
	store := newActionTestStore(t)
	created := mustCreateRestart(t, store, ActionOriginDirectUser)
	restarter := &fakeMiniDeployRestarter{restartError: errors.New("response lost after mutation token=private")}
	result, err := newRestartApplicationExecutor(store, restarter).Execute(context.Background(), created.Proposal)
	if !errors.Is(err, errRestartApplicationOutcomeUnknown) || result.Outcome != RestartApplicationExecutionOutcomeUnknown {
		t.Fatalf("invented failure: %+v %v", result, err)
	}
}
