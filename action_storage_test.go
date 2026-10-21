package main

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	actiondomain "miniai/internal/actiondomain"
)

func TestActionPersistenceDirectRestartUsesPolicyAuthorizationAudit(t *testing.T) {
	store := newActionTestStore(t)
	created := mustCreateAction(t, store, actionProposalCreateInput{
		Kind:      ActionKindRestartApplication,
		Origin:    ActionOriginDirectUser,
		Target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
		Arguments: RestartApplicationArguments{},
		CreatedAt: actionTestTime(),
	})

	if created.Proposal.Origin() != ActionOriginDirectUser ||
		created.Proposal.SafetyClass() != ActionSafetyRecoverable ||
		created.Proposal.State() != ActionStateAuthorized ||
		created.Proposal.ConfirmationRequired() ||
		created.Proposal.WarningRequired() {

		t.Fatalf("unexpected direct restart: origin=%q safety=%q state=%q confirmation=%t warning=%t",
			created.Proposal.Origin(), created.Proposal.SafetyClass(), created.Proposal.State(),
			created.Proposal.ConfirmationRequired(), created.Proposal.WarningRequired())
	}
	assertActionDoesNotSupportExecution(t, created.Proposal.Kind())
	events, err := store.listActionEvents(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != ActionEventPolicyAuthorizedOnCreation ||
		events[0].FromState != nil || events[0].ToState != ActionStateAuthorized {

		t.Fatalf("direct restart events=%+v", events)
	}
	loaded, err := store.getActionProposal(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Proposal.Origin() != ActionOriginDirectUser ||
		loaded.Proposal.State() != ActionStateAuthorized ||
		loaded.Proposal.SafetyClass() != ActionSafetyRecoverable {

		t.Fatalf("reloaded direct restart=%+v", loaded)
	}
}

func TestActionPersistenceRollbackConfirmationAndCancellation(t *testing.T) {
	t.Run("confirm", func(t *testing.T) {
		store := newActionTestStore(t)
		created := mustCreateRollback(t, store, ActionOriginDirectUser, "")
		if created.Proposal.SafetyClass() != ActionSafetyIrreversible ||
			created.Proposal.State() != ActionStateAwaitingConfirmation ||
			!created.Proposal.ConfirmationRequired() || !created.Proposal.WarningRequired() {

			t.Fatalf("unexpected rollback proposal=%+v", created)
		}
		confirmed, err := store.confirmAction(created.Proposal.ID())
		if err != nil {
			t.Fatal(err)
		}
		if confirmed.Proposal.State() != ActionStateAuthorized {
			t.Fatalf("confirmed state=%q", confirmed.Proposal.State())
		}
		events, err := store.listActionEvents(created.Proposal.ID())
		if err != nil {
			t.Fatal(err)
		}
		assertActionEventTypes(t, events, ActionEventProposalCreated, ActionEventConfirmationGranted)
		if events[1].FromState == nil || *events[1].FromState != ActionStateAwaitingConfirmation ||
			events[1].ToState != ActionStateAuthorized {

			t.Fatalf("confirmation event=%+v", events[1])
		}
	})

	t.Run("cancel", func(t *testing.T) {
		store := newActionTestStore(t)
		created := mustCreateRollback(t, store, ActionOriginDirectUser, "")
		cancelled, err := store.cancelAction(created.Proposal.ID())
		if err != nil {
			t.Fatal(err)
		}
		if cancelled.Proposal.State() != ActionStateCancelled {
			t.Fatalf("cancelled state=%q", cancelled.Proposal.State())
		}
		events, err := store.listActionEvents(created.Proposal.ID())
		if err != nil {
			t.Fatal(err)
		}
		assertActionEventTypes(t, events, ActionEventProposalCreated, ActionEventCancelled)
		if events[1].FromState == nil || *events[1].FromState != ActionStateAwaitingConfirmation ||
			events[1].ToState != ActionStateCancelled {

			t.Fatalf("cancellation event=%+v", events[1])
		}
	})
}

func TestActionPersistenceAssistantRestartRequiresDurableConfirmation(t *testing.T) {
	store := newActionTestStore(t)
	created := mustCreateAction(t, store, actionProposalCreateInput{
		Kind:      ActionKindRestartApplication,
		Origin:    ActionOriginAssistantSuggested,
		Target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
		Arguments: RestartApplicationArguments{},
		CreatedAt: actionTestTime(),
	})
	if created.Proposal.Origin() != ActionOriginAssistantSuggested ||
		created.Proposal.State() != ActionStateAwaitingConfirmation ||
		!created.Proposal.ConfirmationRequired() {

		t.Fatalf("assistant restart=%+v", created)
	}
	confirmed, err := store.confirmAction(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Proposal.Origin() != ActionOriginAssistantSuggested ||
		confirmed.Proposal.State() != ActionStateAuthorized {

		t.Fatalf("confirmed assistant restart=%+v", confirmed)
	}
	loaded, err := store.getActionProposal(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Proposal.Origin() != ActionOriginAssistantSuggested ||
		loaded.Proposal.State() != ActionStateAuthorized {

		t.Fatalf("reloaded assistant restart=%+v", loaded)
	}
}

func TestActionPersistenceReopenAndTypedDeployArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "actions.db")
	store, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	arguments := DeployRepositoryArguments{
		Provider: RepositoryProviderGitHub, ContainerPort: 8080, HealthPath: "/healthz",
	}
	created := mustCreateAction(t, store, actionProposalCreateInput{
		Kind:      ActionKindDeployRepository,
		Origin:    ActionOriginDirectUser,
		Target:    ActionTarget{Type: ActionTargetRepository, ID: "ReactorLab/MyScheduler"},
		Arguments: arguments,
		CreatedAt: actionTestTime(),
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	loaded, err := reopened.getActionProposal(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	got, ok := loaded.Proposal.Arguments().(DeployRepositoryArguments)
	if !ok || got != arguments {
		t.Fatalf("typed deploy arguments=%#v (%T), want %#v", loaded.Proposal.Arguments(), loaded.Proposal.Arguments(), arguments)
	}
	if loaded.Proposal.State() != ActionStateAwaitingConfirmation ||
		loaded.Proposal.SafetyClass() != ActionSafetyCompensatable {

		t.Fatalf("reopened deploy=%+v", loaded)
	}
	events, err := reopened.listActionEvents(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	assertActionEventTypes(t, events, ActionEventProposalCreated)
	assertActionDoesNotSupportExecution(t, loaded.Proposal.Kind())
}

func TestActionPersistenceRejectsTamperedRecords(t *testing.T) {
	tests := []struct {
		name   string
		create func(*testing.T, *chatStore) storedActionProposal
		mutate func(*testing.T, *chatStore, ActionID)
		want   error
	}{
		{
			name: "cross-kind arguments",
			create: func(t *testing.T, store *chatStore) storedActionProposal {
				return mustCreateRestart(t, store, ActionOriginDirectUser)
			},
			mutate: func(t *testing.T, store *chatStore, id ActionID) {
				execActionTestSQL(t, store, `UPDATE action_proposals SET arguments_json = ? WHERE action_id = ?`,
					`{"provider":"github","containerPort":8080,"healthPath":"/healthz"}`, id)
			},
			want: errInvalidActionArguments,
		},
		{
			name: "malformed arguments",
			create: func(t *testing.T, store *chatStore) storedActionProposal {
				return mustCreateRestart(t, store, ActionOriginDirectUser)
			},
			mutate: func(t *testing.T, store *chatStore, id ActionID) {
				execActionTestSQL(t, store, `UPDATE action_proposals SET arguments_json = '{' WHERE action_id = ?`, id)
			},
			want: errInvalidActionArguments,
		},
		{
			name: "downgraded safety",
			create: func(t *testing.T, store *chatStore) storedActionProposal {
				return mustCreateRollback(t, store, ActionOriginDirectUser, "")
			},
			mutate: func(t *testing.T, store *chatStore, id ActionID) {
				execActionTestSQL(t, store, `UPDATE action_proposals SET safety_class = 'recoverable' WHERE action_id = ?`, id)
			},
		},
		{
			name: "wrong target type",
			create: func(t *testing.T, store *chatStore) storedActionProposal {
				return mustCreateRollback(t, store, ActionOriginDirectUser, "")
			},
			mutate: func(t *testing.T, store *chatStore, id ActionID) {
				execActionTestSQL(t, store, `UPDATE action_proposals SET target_type = 'repository' WHERE action_id = ?`, id)
			},
		},
		{
			name: "zero origin",
			create: func(t *testing.T, store *chatStore) storedActionProposal {
				return mustCreateRollback(t, store, ActionOriginDirectUser, "")
			},
			mutate: func(t *testing.T, store *chatStore, id ActionID) {
				setIgnoredActionChecks(t, store, true)
				execActionTestSQL(t, store, `UPDATE action_proposals SET origin = '' WHERE action_id = ?`, id)
				setIgnoredActionChecks(t, store, false)
			},
			want: errInvalidActionOrigin,
		},
		{
			name: "invalid lifecycle state",
			create: func(t *testing.T, store *chatStore) storedActionProposal {
				return mustCreateRollback(t, store, ActionOriginDirectUser, "")
			},
			mutate: func(t *testing.T, store *chatStore, id ActionID) {
				setIgnoredActionChecks(t, store, true)
				execActionTestSQL(t, store, `UPDATE action_proposals SET current_state = '' WHERE action_id = ?`, id)
				setIgnoredActionChecks(t, store, false)
			},
			want: errInvalidActionState,
		},
		{
			name: "authorized without confirmation event",
			create: func(t *testing.T, store *chatStore) storedActionProposal {
				return mustCreateRollback(t, store, ActionOriginDirectUser, "")
			},
			mutate: func(t *testing.T, store *chatStore, id ActionID) {
				execActionTestSQL(t, store, `UPDATE action_proposals SET current_state = 'authorized' WHERE action_id = ?`, id)
			},
			want: actiondomain.ErrActionAuditMismatch,
		},
		{
			name: "tampered execution support",
			create: func(t *testing.T, store *chatStore) storedActionProposal {
				return mustCreateRestart(t, store, ActionOriginDirectUser)
			},
			mutate: func(t *testing.T, store *chatStore, id ActionID) {
				execActionTestSQL(t, store, `UPDATE action_proposals SET supports_execution = 1 WHERE action_id = ?`, id)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newActionTestStore(t)
			created := test.create(t, store)
			test.mutate(t, store, created.Proposal.ID())
			_, err := store.getActionProposal(created.Proposal.ID())
			if !errors.Is(err, errInvalidPersistedAction) {
				t.Fatalf("expected invalid persisted action, got %v", err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("expected %v, got %v", test.want, err)
			}
		})
	}
}

func TestActionPersistenceRejectsRestartExecutionStateWithoutAuditChain(t *testing.T) {
	for _, state := range []ActionState{
		ActionStateExecuting, ActionStateSucceeded, ActionStateFailed,
	} {
		t.Run(string(state), func(t *testing.T) {
			store := newActionTestStore(t)
			created := mustCreateRestart(t, store, ActionOriginDirectUser)
			execActionTestSQL(t, store, `UPDATE action_proposals SET current_state = ? WHERE action_id = ?`, state, created.Proposal.ID())
			_, err := store.getActionProposal(created.Proposal.ID())
			if !errors.Is(err, errInvalidPersistedAction) ||
				!errors.Is(err, actiondomain.ErrActionAuditMismatch) {

				t.Fatalf("state %q should fail closed, got %v", state, err)
			}
		})
	}
}

func TestActionPersistenceRejectsOtherActionExecutionStates(t *testing.T) {
	tests := []struct {
		name   string
		create func(*testing.T, *chatStore) storedActionProposal
	}{
		{
			name: "rollback",
			create: func(t *testing.T, store *chatStore) storedActionProposal {
				created := mustCreateRollback(t, store, ActionOriginDirectUser, "")
				authorized, err := store.confirmAction(created.Proposal.ID())
				if err != nil {
					t.Fatal(err)
				}
				return authorized
			},
		},
		{
			name: "deploy",
			create: func(t *testing.T, store *chatStore) storedActionProposal {
				created := mustCreateAction(t, store, actionProposalCreateInput{
					Kind: ActionKindDeployRepository, Origin: ActionOriginDirectUser,
					Target: ActionTarget{Type: ActionTargetRepository, ID: "owner/repository"},
					Arguments: DeployRepositoryArguments{
						Provider:      RepositoryProviderGitHub,
						ContainerPort: 8080,
						HealthPath:    "/health",
					},
					CreatedAt: actionTestTime(),
				})
				authorized, err := store.confirmAction(created.Proposal.ID())
				if err != nil {
					t.Fatal(err)
				}
				return authorized
			},
		},
	}
	for _, test := range tests {
		for _, state := range []ActionState{
			ActionStateExecuting, ActionStateSucceeded, ActionStateFailed,
		} {
			t.Run(test.name+"/"+string(state), func(t *testing.T) {
				store := newActionTestStore(t)
				created := test.create(t, store)
				execActionTestSQL(
					t, store,
					`UPDATE action_proposals SET current_state = ? WHERE action_id = ?`,
					state, created.Proposal.ID(),
				)
				_, err := store.getActionProposal(created.Proposal.ID())
				if !errors.Is(err, errInvalidPersistedAction) ||
					!errors.Is(err, actiondomain.ErrActionExecutionUnsupported) {

					t.Fatalf("state %q should fail closed, got %v", state, err)
				}
			})
		}
	}
}

func TestActionConfirmationIsOneShotUnderConcurrency(t *testing.T) {
	store := newActionTestStore(t)
	created := mustCreateRollback(t, store, ActionOriginDirectUser, "")
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := store.confirmAction(created.Proposal.ID())
			results <- err
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, errActionConfirmationNotAllowed) &&
			!errors.Is(err, errActionPersistenceConflict) {

			t.Fatalf("unexpected losing confirmation error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful confirmations=%d, want 1", successes)
	}
	events, err := store.listActionEvents(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	assertActionEventTypes(t, events, ActionEventProposalCreated, ActionEventConfirmationGranted)
}

func TestActionConfirmationAndCancellationConflictAtomically(t *testing.T) {
	store := newActionTestStore(t)
	created := mustCreateRollback(t, store, ActionOriginDirectUser, "")
	start := make(chan struct{})
	type result struct {
		operation string
		err       error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		_, err := store.confirmAction(created.Proposal.ID())
		results <- result{operation: "confirm", err: err}
	}()
	go func() {
		defer workers.Done()
		<-start
		_, err := store.cancelAction(created.Proposal.ID())
		results <- result{operation: "cancel", err: err}
	}()
	close(start)
	workers.Wait()
	close(results)

	successes := 0
	for result := range results {
		if result.err == nil {
			successes++
			continue
		}
		allowed := errors.Is(result.err, errActionPersistenceConflict)
		if result.operation == "confirm" {
			allowed = allowed || errors.Is(result.err, errActionConfirmationNotAllowed)
		} else {
			allowed = allowed || errors.Is(result.err, errActionCancellationNotAllowed)
		}
		if !allowed {
			t.Fatalf("unexpected losing %s error: %v", result.operation, result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful terminal operations=%d, want 1", successes)
	}
	loaded, err := store.getActionProposal(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.listActionEvents(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events=%+v", events)
	}
	switch loaded.Proposal.State() {
	case ActionStateAuthorized:
		assertActionEventTypes(t, events, ActionEventProposalCreated, ActionEventConfirmationGranted)
	case ActionStateCancelled:
		assertActionEventTypes(t, events, ActionEventProposalCreated, ActionEventCancelled)
	default:
		t.Fatalf("final state=%q", loaded.Proposal.State())
	}
}

func TestActionStateAndAuditEventCommitAtomically(t *testing.T) {
	store := newActionTestStore(t)
	created := mustCreateRollback(t, store, ActionOriginDirectUser, "")
	execActionTestSQL(t, store, `CREATE TRIGGER reject_confirmation_event
		BEFORE INSERT ON action_events
		WHEN NEW.event_type = 'confirmation_granted'
		BEGIN SELECT RAISE(ABORT, 'test rejection'); END`)
	if _, err := store.confirmAction(created.Proposal.ID()); !errors.Is(err, errActionPersistenceConflict) {

		t.Fatalf("confirmation error=%v", err)
	}
	execActionTestSQL(t, store, `DROP TRIGGER reject_confirmation_event`)
	loaded, err := store.getActionProposal(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Proposal.State() != ActionStateAwaitingConfirmation {
		t.Fatalf("state escaped rolled-back transaction: %q", loaded.Proposal.State())
	}
	events, err := store.listActionEvents(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	assertActionEventTypes(t, events, ActionEventProposalCreated)
}

func TestActionPersistenceUnknownIDIsTypedNotFound(t *testing.T) {
	store := newActionTestStore(t)
	id := ActionID("action_00000000000000000000000000000000")
	if _, err := store.getActionProposal(id); !errors.Is(err, errActionNotFound) {
		t.Fatalf("get error=%v", err)
	}
	if _, err := store.listActionEvents(id); !errors.Is(err, errActionNotFound) {
		t.Fatalf("list events error=%v", err)
	}
	if _, err := store.confirmAction(id); !errors.Is(err, errActionNotFound) {
		t.Fatalf("confirm error=%v", err)
	}
	if _, err := store.cancelAction(id); !errors.Is(err, errActionNotFound) {
		t.Fatalf("cancel error=%v", err)
	}
}

func TestActionHistoryLinkageRoundTripsAndRejectsUnknownHistory(t *testing.T) {
	store := newActionTestStore(t)
	entry, _, err := store.startHistoryEntry("", "prepare rollback", actionTestTime())
	if err != nil {
		t.Fatal(err)
	}
	entry, err = store.finalizeHistoryEntry(entry.ID, historyFinalizeInput{
		Kind: HistoryKindAction, Status: HistoryStatusSucceeded,
		ResultText: "rollback proposed", CompletedAt: actionTestTime().Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	created := mustCreateRollback(t, store, ActionOriginDirectUser, entry.ID)
	loaded, err := store.getActionProposal(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.HistoryEntryID != entry.ID {
		t.Fatalf("history link=%q, want %q", loaded.HistoryEntryID, entry.ID)
	}
	ids, err := store.historyActionProposalIDs(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != created.Proposal.ID() {
		t.Fatalf("history action IDs=%v", ids)
	}
	if _, err := store.createActionProposal(actionProposalCreateInput{
		Kind: ActionKindRollbackDeployment, Origin: ActionOriginDirectUser,
		Target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
		Arguments: RollbackDeploymentArguments{}, HistoryEntryID: "missing-history",
		CreatedAt: actionTestTime(),
	}); !errors.Is(err, errActionHistoryLink) {

		t.Fatalf("invalid History linkage error=%v", err)
	}
	if _, err := store.historyActionProposalIDs("missing-history"); !errors.Is(err, errActionHistoryLink) {

		t.Fatalf("unknown History lookup error=%v", err)
	}
	gotHistory, err := store.getHistoryEntry(entry.ID)
	if err != nil || gotHistory.Status != HistoryStatusSucceeded ||
		gotHistory.ResultText != "rollback proposed" {

		t.Fatalf("History semantics changed: entry=%+v err=%v", gotHistory, err)
	}
}

func TestActionSchemaMigratesPhase3DatabaseWithoutDataLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "phase3.db")
	store, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := store.createChat("Phase 3 fixture")
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err := store.startHistoryEntry(chat.ID, "why is it failing", actionTestTime())
	if err != nil {
		t.Fatal(err)
	}
	readStarted := actionTestTime().Add(time.Second)
	readCompleted := readStarted.Add(time.Second)
	entry, err = store.finalizeHistoryEntry(entry.ID, historyFinalizeInput{
		Kind: HistoryKindInvestigation, Status: HistoryStatusSucceeded,
		ResultText: "safe answer", AssistantText: "safe answer", PersistAssistant: true,
		AssistantEvidence: []evidence{{
			ToolName: "get_app_context", App: "myscheduler", Source: "app_context",
			Arguments: map[string]any{"app": "myscheduler"}, Summary: "current context",
		}},
		CompletedAt: readCompleted.Add(time.Second),
		Metadata:    HistoryExecutionMetadata{EvidenceRounds: 1, ToolCalls: 1},
		Targets: []HistoryTarget{{
			Kind: "application", CanonicalID: "myscheduler", DisplayName: "MyScheduler",
		}},
		EvidenceReads: []HistoryEvidenceRead{{
			RequestID: "fixture-read", EvidenceRound: 1, Capability: "get_app_context",
			Arguments: map[string]any{"app": "myscheduler"}, SafeSummary: "current context",
			Status: "completed", Availability: "complete",
			StartedAt: &readStarted, CompletedAt: &readCompleted,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TABLE action_events`,
		`DROP TABLE action_proposals`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	var version int
	if err := migrated.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil ||
		version != miniAISchemaVersion {

		t.Fatalf("schema version=%d err=%v", version, err)
	}
	detail, err := migrated.getChat(chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Messages) != 2 || detail.Messages[0].Content != "why is it failing" ||
		detail.Messages[1].Content != "safe answer" || len(detail.Messages[1].Evidence) != 1 {

		t.Fatalf("chat/messages/evidence changed during migration: %+v", detail)
	}
	history, err := migrated.getHistoryEntry(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if history.Status != HistoryStatusSucceeded || history.ResultText != "safe answer" ||
		len(history.Targets) != 1 || len(history.EvidenceReads) != 1 ||
		history.EvidenceReads[0].Capability != "get_app_context" {

		t.Fatalf("History data changed during migration: %+v", history)
	}
	for _, table := range []string{"action_proposals", "action_events"} {
		var name string
		if err := migrated.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&name); err != nil || name != table {

			t.Fatalf("missing migrated table %q: name=%q err=%v", table, name, err)
		}
	}
}

func newActionTestStore(t *testing.T) *chatStore {
	t.Helper()
	store, err := openChatStore(filepath.Join(t.TempDir(), "actions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close action test store: %v", err)
		}
	})
	return store
}

func actionTestTime() time.Time {
	return time.Date(2020, time.October, 20, 12, 34, 56, 123456789, time.UTC)
}

func mustCreateAction(
	t *testing.T,
	store *chatStore,
	input actionProposalCreateInput,
) storedActionProposal {
	t.Helper()
	record, err := store.createActionProposal(input)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func mustCreateRestart(
	t *testing.T,
	store *chatStore,
	origin ActionOrigin,
) storedActionProposal {
	t.Helper()
	return mustCreateAction(t, store, actionProposalCreateInput{
		Kind: ActionKindRestartApplication, Origin: origin,
		Target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
		Arguments: RestartApplicationArguments{}, CreatedAt: actionTestTime(),
	})
}

func mustCreateRollback(
	t *testing.T,
	store *chatStore,
	origin ActionOrigin,
	historyEntryID string,
) storedActionProposal {
	t.Helper()
	return mustCreateAction(t, store, actionProposalCreateInput{
		Kind: ActionKindRollbackDeployment, Origin: origin,
		Target:    ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
		Arguments: RollbackDeploymentArguments{}, HistoryEntryID: historyEntryID,
		CreatedAt: actionTestTime(),
	})
}

func assertActionEventTypes(
	t *testing.T,
	events []ActionEvent,
	want ...ActionEventType,
) {
	t.Helper()
	if len(events) != len(want) {
		t.Fatalf("events=%+v, want types=%v", events, want)
	}
	for index, eventType := range want {
		if events[index].Type != eventType || events[index].Ordinal != index {
			t.Fatalf("event[%d]=%+v, want type=%q ordinal=%d", index, events[index], eventType, index)
		}
	}
}

func assertActionDoesNotSupportExecution(t *testing.T, kind ActionKind) {
	t.Helper()
	definition, ok := phase4ActionCatalog().lookup(kind)
	if !ok || definition.SupportsExecution {
		t.Fatalf("action %q execution support=%t found=%t", kind, definition.SupportsExecution, ok)
	}
}

func execActionTestSQL(
	t *testing.T,
	store *chatStore,
	statement string,
	arguments ...any,
) {
	t.Helper()
	if _, err := store.db.Exec(statement, arguments...); err != nil {
		t.Fatal(err)
	}
}

func setIgnoredActionChecks(t *testing.T, store *chatStore, ignored bool) {
	t.Helper()
	value := "OFF"
	if ignored {
		value = "ON"
	}
	execActionTestSQL(t, store, `PRAGMA ignore_check_constraints = `+value)
}
