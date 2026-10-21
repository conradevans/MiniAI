package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeMiniDeployRestarter struct {
	mu              sync.Mutex
	applications    []string
	restartError    error
	firstEntered    chan struct{}
	releaseFirst    chan struct{}
	firstSignalOnce sync.Once
}

func (fake *fakeMiniDeployRestarter) RestartApplication(
	_ context.Context,
	applicationID string,
) error {
	fake.mu.Lock()
	fake.applications = append(fake.applications, applicationID)
	callNumber := len(fake.applications)
	restartError := fake.restartError
	fake.mu.Unlock()
	if callNumber == 1 && fake.firstEntered != nil {
		fake.firstSignalOnce.Do(func() { close(fake.firstEntered) })
		<-fake.releaseFirst
	}
	return restartError
}

func (fake *fakeMiniDeployRestarter) calls() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]string(nil), fake.applications...)
}

func TestRestartApplicationExecutorSucceedsOnceWithValidatedTarget(t *testing.T) {
	store := newActionTestStore(t)
	created := mustCreateRestart(t, store, ActionOriginDirectUser)
	restarter := &fakeMiniDeployRestarter{}
	executor := newRestartApplicationExecutor(store, restarter)

	result, err := executor.Execute(context.Background(), created.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != RestartApplicationExecutionSucceeded ||
		result.Action.Proposal.State() != ActionStateSucceeded ||
		result.FailureCategory != "" {

		t.Fatalf("execution result=%+v", result)
	}
	if calls := restarter.calls(); len(calls) != 1 || calls[0] != "myscheduler" {
		t.Fatalf("restart calls=%v", calls)
	}
	events, err := store.listActionEvents(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	assertActionEventTypes(
		t,
		events,
		ActionEventPolicyAuthorizedOnCreation,
		ActionEventExecutionStarted,
		ActionEventExecutionSucceeded,
	)
	if events[1].FromState == nil || *events[1].FromState != ActionStateAuthorized ||
		events[1].ToState != ActionStateExecuting ||
		events[2].FromState == nil || *events[2].FromState != ActionStateExecuting ||
		events[2].ToState != ActionStateSucceeded {

		t.Fatalf("execution events=%+v", events)
	}
	assertActionSupportsExecution(t, ActionKindRestartApplication, true)
}

func TestRestartApplicationExecutorRejectsEveryUnauthorizedShape(t *testing.T) {
	tests := []struct {
		name     string
		proposal func(*testing.T, *chatStore) ActionProposal
	}{
		{
			name: "awaiting confirmation",
			proposal: func(t *testing.T, store *chatStore) ActionProposal {
				return mustCreateRestart(t, store, ActionOriginAssistantSuggested).Proposal
			},
		},
		{
			name: "cancelled",
			proposal: func(t *testing.T, store *chatStore) ActionProposal {
				created := mustCreateRestart(t, store, ActionOriginAssistantSuggested)
				cancelled, err := store.cancelAction(created.Proposal.ID())
				if err != nil {
					t.Fatal(err)
				}
				return cancelled.Proposal
			},
		},
		{
			name: "executing",
			proposal: func(t *testing.T, store *chatStore) ActionProposal {
				created := mustCreateRestart(t, store, ActionOriginDirectUser)
				executing, err := store.beginRestartApplicationExecution(created.Proposal)
				if err != nil {
					t.Fatal(err)
				}
				return executing.Proposal
			},
		},
		{
			name: "succeeded",
			proposal: func(t *testing.T, store *chatStore) ActionProposal {
				created := mustCreateRestart(t, store, ActionOriginDirectUser)
				executing, err := store.beginRestartApplicationExecution(created.Proposal)
				if err != nil {
					t.Fatal(err)
				}
				succeeded, err := store.completeRestartApplicationSuccess(executing.Proposal.ID())
				if err != nil {
					t.Fatal(err)
				}
				return succeeded.Proposal
			},
		},
		{
			name: "failed",
			proposal: func(t *testing.T, store *chatStore) ActionProposal {
				created := mustCreateRestart(t, store, ActionOriginDirectUser)
				executing, err := store.beginRestartApplicationExecution(created.Proposal)
				if err != nil {
					t.Fatal(err)
				}
				failed, err := store.completeRestartApplicationFailure(
					executing.Proposal.ID(),
					ActionExecutionFailureUpstreamRejected,
				)
				if err != nil {
					t.Fatal(err)
				}
				return failed.Proposal
			},
		},
		{
			name: "rollback",
			proposal: func(t *testing.T, store *chatStore) ActionProposal {
				created := mustCreateRollback(t, store, ActionOriginDirectUser, "")
				authorized, err := store.confirmAction(created.Proposal.ID())
				if err != nil {
					t.Fatal(err)
				}
				return authorized.Proposal
			},
		},
		{
			name: "deploy",
			proposal: func(t *testing.T, store *chatStore) ActionProposal {
				created := mustCreateAction(t, store, actionProposalCreateInput{
					Kind: ActionKindDeployRepository, Origin: ActionOriginDirectUser,
					Target: ActionTarget{Type: ActionTargetRepository, ID: "owner/repository"},
					Arguments: DeployRepositoryArguments{
						Provider: RepositoryProviderGitHub, ContainerPort: 8080, HealthPath: "/health",
					},
					CreatedAt: actionTestTime(),
				})
				authorized, err := store.confirmAction(created.Proposal.ID())
				if err != nil {
					t.Fatal(err)
				}
				return authorized.Proposal
			},
		},
		{
			name: "zero corrupted proposal",
			proposal: func(*testing.T, *chatStore) ActionProposal {
				return ActionProposal{}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newActionTestStore(t)
			restarter := &fakeMiniDeployRestarter{}
			executor := newRestartApplicationExecutor(store, restarter)
			_, err := executor.Execute(context.Background(), test.proposal(t, store))
			if !errors.Is(err, errRestartApplicationExecutionNotAllowed) {
				t.Fatalf("execution error=%v", err)
			}
			if calls := restarter.calls(); len(calls) != 0 {
				t.Fatalf("rejected proposal invoked restart: %v", calls)
			}
		})
	}
}

func TestRestartApplicationExecutorRejectsWrongTargetAndCorruptPersistence(t *testing.T) {
	store := newActionTestStore(t)
	restarter := &fakeMiniDeployRestarter{}
	executor := newRestartApplicationExecutor(store, restarter)

	_, err := phase4ActionCatalog().propose(ActionProposalInput{
		ID:        "action_0123456789abcdef0123456789abcdef",
		Kind:      ActionKindRestartApplication,
		Origin:    ActionOriginDirectUser,
		Target:    ActionTarget{Type: ActionTargetRepository, ID: "owner/repository"},
		Arguments: RestartApplicationArguments{},
		CreatedAt: actionTestTime(),
	})
	if !errors.Is(err, errInvalidActionTarget) {
		t.Fatalf("wrong target error=%v", err)
	}

	created := mustCreateRestart(t, store, ActionOriginDirectUser)
	execActionTestSQL(
		t,
		store,
		`UPDATE action_proposals SET safety_class = 'reversible' WHERE action_id = ?`,
		created.Proposal.ID(),
	)
	_, err = executor.Execute(context.Background(), created.Proposal)
	if !errors.Is(err, errRestartApplicationExecutionNotAllowed) ||
		!errors.Is(err, errInvalidPersistedAction) {

		t.Fatalf("corrupt persistence execution error=%v", err)
	}
	if calls := restarter.calls(); len(calls) != 0 {
		t.Fatalf("corrupt persistence invoked restart: %v", calls)
	}
}

func TestRestartApplicationExecutorMapsSafeFailuresWithoutRetry(t *testing.T) {
	tests := []struct {
		name          string
		restartError  error
		wantOutcome   RestartApplicationExecutionOutcome
		wantState     ActionState
		wantCategory  ActionExecutionFailureCategory
		wantError     error
		wantEventType ActionEventType
	}{
		{
			name: "known timeout", restartError: miniDeployRestartFailure{
				category: ActionExecutionFailureTimeout, outcomeKnown: true,
			},
			wantOutcome: RestartApplicationExecutionFailed, wantState: ActionStateFailed,
			wantCategory:  ActionExecutionFailureTimeout,
			wantError:     errRestartApplicationExecutionFailed,
			wantEventType: ActionEventExecutionFailed,
		},
		{
			name: "upstream rejection", restartError: miniDeployRestartFailure{
				category: ActionExecutionFailureUpstreamRejected, outcomeKnown: true,
			},
			wantOutcome: RestartApplicationExecutionFailed, wantState: ActionStateFailed,
			wantCategory:  ActionExecutionFailureUpstreamRejected,
			wantError:     errRestartApplicationExecutionFailed,
			wantEventType: ActionEventExecutionFailed,
		},
		{
			name: "unknown internal error", restartError: errors.New("secret token at /private/path"),
			wantOutcome: RestartApplicationExecutionFailed, wantState: ActionStateFailed,
			wantCategory:  ActionExecutionFailureInternalError,
			wantError:     errRestartApplicationExecutionFailed,
			wantEventType: ActionEventExecutionFailed,
		},
		{
			name: "ambiguous response", restartError: miniDeployRestartFailure{
				category: ActionExecutionFailureInvalidResponse, outcomeKnown: false,
			},
			wantOutcome:   RestartApplicationExecutionOutcomeUnknown,
			wantState:     ActionStateExecuting,
			wantCategory:  ActionExecutionFailureInvalidResponse,
			wantError:     errRestartApplicationOutcomeUnknown,
			wantEventType: ActionEventExecutionStarted,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newActionTestStore(t)
			created := mustCreateRestart(t, store, ActionOriginDirectUser)
			restarter := &fakeMiniDeployRestarter{restartError: test.restartError}
			executor := newRestartApplicationExecutor(store, restarter)
			result, err := executor.Execute(context.Background(), created.Proposal)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("execution error=%v want=%v", err, test.wantError)
			}
			if result.Outcome != test.wantOutcome ||
				result.Action.Proposal.State() != test.wantState ||
				result.FailureCategory != test.wantCategory {

				t.Fatalf("execution result=%+v", result)
			}
			if calls := restarter.calls(); len(calls) != 1 || calls[0] != "myscheduler" {
				t.Fatalf("restart calls=%v", calls)
			}
			events, err := store.listActionEvents(created.Proposal.ID())
			if err != nil {
				t.Fatal(err)
			}
			if events[len(events)-1].Type != test.wantEventType {
				t.Fatalf("events=%+v", events)
			}
			if test.wantEventType == ActionEventExecutionFailed &&
				events[len(events)-1].FailureCategory != test.wantCategory {

				t.Fatalf("failure event=%+v", events[len(events)-1])
			}
			var leaked int
			if err := store.db.QueryRow(
				`SELECT COUNT(*) FROM action_events WHERE failure_category LIKE '%secret%'
				 OR failure_category LIKE '%path%'`,
			).Scan(&leaked); err != nil {
				t.Fatal(err)
			}
			if leaked != 0 {
				t.Fatal("arbitrary upstream error text was persisted")
			}
		})
	}
}

func TestRestartApplicationExecutorClassifiesHTTPRejectionsWithoutRetry(t *testing.T) {
	tests := []struct {
		name           string
		status         int
		wantOutcome    RestartApplicationExecutionOutcome
		wantState      ActionState
		wantError      error
		wantEventTypes []ActionEventType
	}{
		{
			name:        "server error is ambiguous",
			status:      http.StatusInternalServerError,
			wantOutcome: RestartApplicationExecutionOutcomeUnknown,
			wantState:   ActionStateExecuting,
			wantError:   errRestartApplicationOutcomeUnknown,
			wantEventTypes: []ActionEventType{
				ActionEventPolicyAuthorizedOnCreation,
				ActionEventExecutionStarted,
			},
		},
		{
			name:        "detached database is definitive",
			status:      http.StatusConflict,
			wantOutcome: RestartApplicationExecutionFailed,
			wantState:   ActionStateFailed,
			wantError:   errRestartApplicationExecutionFailed,
			wantEventTypes: []ActionEventType{
				ActionEventPolicyAuthorizedOnCreation,
				ActionEventExecutionStarted,
				ActionEventExecutionFailed,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				http.Error(w, "partial restart detail must not escape", test.status)
			}))
			defer server.Close()
			client, err := newMiniDeployRestartClient(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			store := newActionTestStore(t)
			created := mustCreateRestart(t, store, ActionOriginDirectUser)
			executor := newRestartApplicationExecutor(store, client)

			result, err := executor.Execute(context.Background(), created.Proposal)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("execution error=%v want=%v", err, test.wantError)
			}
			if result.Outcome != test.wantOutcome ||
				result.Action.Proposal.State() != test.wantState ||
				result.FailureCategory != ActionExecutionFailureUpstreamRejected {

				t.Fatalf("execution result=%+v", result)
			}
			if calls.Load() != 1 {
				t.Fatalf("MiniDeploy restart calls=%d, want 1", calls.Load())
			}
			loaded, err := store.getActionProposal(created.Proposal.ID())
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Proposal.State() != test.wantState {
				t.Fatalf("persisted state=%q want=%q", loaded.Proposal.State(), test.wantState)
			}
			events, err := store.listActionEvents(created.Proposal.ID())
			if err != nil {
				t.Fatal(err)
			}
			assertActionEventTypes(t, events, test.wantEventTypes...)
			if test.wantState == ActionStateExecuting {
				for _, event := range events {
					if event.Type == ActionEventExecutionFailed {
						t.Fatalf("ambiguous result appended failure event: %+v", events)
					}
				}
			} else if events[len(events)-1].FailureCategory !=
				ActionExecutionFailureUpstreamRejected {

				t.Fatalf("definitive rejection event=%+v", events[len(events)-1])
			}
		})
	}
}

func TestRestartExecutionPersistenceIsOneShotUnderConcurrency(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		store := newActionTestStore(t)
		created := mustCreateRestart(t, store, ActionOriginDirectUser)
		start := make(chan struct{})
		results := make(chan error, 2)
		var workers sync.WaitGroup
		for range 2 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				_, err := store.beginRestartApplicationExecution(created.Proposal)
				results <- err
			}()
		}
		close(start)
		workers.Wait()
		close(results)
		assertOneExecutionTransitionWinner(t, results, errActionExecutionNotAllowed)
		events, err := store.listActionEvents(created.Proposal.ID())
		if err != nil {
			t.Fatal(err)
		}
		assertActionEventTypes(
			t,
			events,
			ActionEventPolicyAuthorizedOnCreation,
			ActionEventExecutionStarted,
		)
	})

	t.Run("success versus failure", func(t *testing.T) {
		store := newActionTestStore(t)
		created := mustCreateRestart(t, store, ActionOriginDirectUser)
		executing, err := store.beginRestartApplicationExecution(created.Proposal)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		var workers sync.WaitGroup
		workers.Add(2)
		go func() {
			defer workers.Done()
			<-start
			_, err := store.completeRestartApplicationSuccess(executing.Proposal.ID())
			results <- err
		}()
		go func() {
			defer workers.Done()
			<-start
			_, err := store.completeRestartApplicationFailure(
				executing.Proposal.ID(),
				ActionExecutionFailureUpstreamUnavailable,
			)
			results <- err
		}()
		close(start)
		workers.Wait()
		close(results)
		assertOneExecutionTransitionWinner(
			t,
			results,
			errActionExecutionCompletionNotAllowed,
		)
		loaded, err := store.getActionProposal(created.Proposal.ID())
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Proposal.State() != ActionStateSucceeded &&
			loaded.Proposal.State() != ActionStateFailed {

			t.Fatalf("terminal state=%q", loaded.Proposal.State())
		}
		events, err := store.listActionEvents(created.Proposal.ID())
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 3 || events[2].Ordinal != 2 {
			t.Fatalf("terminal events=%+v", events)
		}
		if _, err := store.completeRestartApplicationSuccess(created.Proposal.ID()); !errors.Is(err, errActionExecutionCompletionNotAllowed) {

			t.Fatalf("duplicate success error=%v", err)
		}
	})
}

func TestRestartApplicationExecutorIsOneShotUnderConcurrency(t *testing.T) {
	store := newActionTestStore(t)
	created := mustCreateRestart(t, store, ActionOriginDirectUser)
	restarter := &fakeMiniDeployRestarter{
		firstEntered: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	executor := newRestartApplicationExecutor(store, restarter)
	type executionAttempt struct {
		result RestartApplicationExecutionResult
		err    error
	}
	start := make(chan struct{})
	results := make(chan executionAttempt, 2)
	for range 2 {
		go func() {
			<-start
			result, err := executor.Execute(context.Background(), created.Proposal)
			results <- executionAttempt{result: result, err: err}
		}()
	}
	close(start)

	<-restarter.firstEntered
	first := <-results
	close(restarter.releaseFirst)
	second := <-results

	successes := 0
	rejections := 0
	for _, attempt := range []executionAttempt{first, second} {
		switch {
		case attempt.err == nil:
			successes++
			if attempt.result.Outcome != RestartApplicationExecutionSucceeded ||
				attempt.result.Action.Proposal.State() != ActionStateSucceeded {

				t.Fatalf("successful execution result=%+v", attempt.result)
			}
		case errors.Is(attempt.err, errRestartApplicationExecutionNotAllowed):
			rejections++
		default:
			t.Fatalf("unexpected concurrent execution error=%v", attempt.err)
		}
	}
	if successes != 1 || rejections != 1 {
		t.Fatalf("concurrent outcomes successes=%d rejections=%d", successes, rejections)
	}
	if calls := restarter.calls(); len(calls) != 1 || calls[0] != "myscheduler" {
		t.Fatalf("concurrent MiniDeploy calls=%v", calls)
	}
	loaded, err := store.getActionProposal(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Proposal.State() != ActionStateSucceeded {
		t.Fatalf("final action state=%q", loaded.Proposal.State())
	}
	events, err := store.listActionEvents(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	assertActionEventTypes(
		t,
		events,
		ActionEventPolicyAuthorizedOnCreation,
		ActionEventExecutionStarted,
		ActionEventExecutionSucceeded,
	)
}

func TestRestartExecutionStatesRestoreAfterDatabaseReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "actions.db")
	store, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	create := func(application string) storedActionProposal {
		return mustCreateAction(t, store, actionProposalCreateInput{
			Kind: ActionKindRestartApplication, Origin: ActionOriginDirectUser,
			Target:    ActionTarget{Type: ActionTargetApplication, ID: application},
			Arguments: RestartApplicationArguments{}, CreatedAt: actionTestTime(),
		})
	}
	executingCreated := create("executing-app")
	executing, err := store.beginRestartApplicationExecution(executingCreated.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	succeededCreated := create("succeeded-app")
	succeededExecuting, err := store.beginRestartApplicationExecution(succeededCreated.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	succeeded, err := store.completeRestartApplicationSuccess(succeededExecuting.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	failedCreated := create("failed-app")
	failedExecuting, err := store.beginRestartApplicationExecution(failedCreated.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := store.completeRestartApplicationFailure(
		failedExecuting.Proposal.ID(),
		ActionExecutionFailureUpstreamRejected,
	)
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
	for _, expected := range []storedActionProposal{executing, succeeded, failed} {
		loaded, err := reopened.getActionProposal(expected.Proposal.ID())
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Proposal.State() != expected.Proposal.State() {
			t.Fatalf("reopened %s state=%q want=%q", expected.Proposal.ID(), loaded.Proposal.State(), expected.Proposal.State())
		}
	}
	failedEvents, err := reopened.listActionEvents(failed.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	if failedEvents[len(failedEvents)-1].FailureCategory != ActionExecutionFailureUpstreamRejected {
		t.Fatalf("reopened failure event=%+v", failedEvents[len(failedEvents)-1])
	}
}

func assertOneExecutionTransitionWinner(
	t *testing.T,
	results <-chan error,
	allowed error,
) {
	t.Helper()
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, allowed) && !errors.Is(err, errActionPersistenceConflict) {
			t.Fatalf("unexpected losing transition error=%v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful transitions=%d, want 1", successes)
	}
}
