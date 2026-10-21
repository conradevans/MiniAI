package main

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestActionSchemaV3MigratesV2RecordsWithoutDataLoss(t *testing.T) {
	path := filepath.Join(t.TempDir(), "actions-v2.db")
	store, err := openChatStore(path)
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
	tx, err := db.Begin()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TABLE action_events`,
		`DROP TABLE action_proposals`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := migrateMiniAIActionSchemaV2(tx); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		t.Fatal(err)
	}
	actionID := ActionID("action_0123456789abcdef0123456789abcdef")
	eventID := "action_event_0123456789abcdef0123456789abcdef"
	createdAt := encodeDBTime(actionTestTime())
	if _, err := tx.Exec(
		`INSERT INTO action_proposals(
			action_id, action_kind, origin, target_type, target_id, arguments_json,
			safety_class, confirmation_required, warning_required, supports_execution,
			policy_reason, current_state, history_entry_id, created_at, updated_at
		 ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		actionID,
		ActionKindRestartApplication,
		ActionOriginDirectUser,
		ActionTargetApplication,
		"myscheduler",
		`{}`,
		ActionSafetyRecoverable,
		false,
		false,
		false,
		ActionPolicyDirectUserNoConfirmation,
		ActionStateAuthorized,
		nil,
		createdAt,
		createdAt,
	); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := tx.Exec(
		`INSERT INTO action_events(
			event_id, action_id, ordinal, event_type, from_state, to_state, created_at
		 ) VALUES(?,?,?,?,NULL,?,?)`,
		eventID,
		actionID,
		0,
		ActionEventPolicyAuthorizedOnCreation,
		ActionStateAuthorized,
		createdAt,
	); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := tx.Exec(`PRAGMA user_version = 2`); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		t.Fatal(err)
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
	record, err := migrated.getActionProposal(actionID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Proposal.State() != ActionStateAuthorized ||
		record.Proposal.Target().ID != "myscheduler" || record.SupportsExecution {

		t.Fatalf("migrated proposal=%+v", record)
	}
	events, err := migrated.listActionEvents(actionID)
	if err != nil {
		t.Fatal(err)
	}
	assertActionEventTypes(t, events, ActionEventPolicyAuthorizedOnCreation)
	if events[0].FailureCategory != "" {
		t.Fatalf("migrated event failure category=%q", events[0].FailureCategory)
	}
}

type v2ActionSnapshot struct {
	ActionID             string
	Kind                 string
	Origin               string
	TargetType           string
	TargetID             string
	ArgumentsJSON        string
	SafetyClass          string
	ConfirmationRequired bool
	WarningRequired      bool
	SupportsExecution    bool
	PolicyReason         string
	CurrentState         string
	HistoryEntryID       sql.NullString
	CreatedAt            string
	UpdatedAt            string
}

type v2EventSnapshot struct {
	EventID   string
	ActionID  string
	Ordinal   int
	Type      string
	FromState sql.NullString
	ToState   string
	CreatedAt string
}

func TestActionSchemaV3MigratesRealisticPopulatedV2Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "populated-actions-v2.db")
	store, err := openChatStore(path)
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
	t.Cleanup(func() { _ = db.Close() })
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		`DROP TABLE action_events`,
		`DROP TABLE action_proposals`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateMiniAIActionSchemaV2(tx); err != nil {
		t.Fatal(err)
	}

	base := actionTestTime()
	awaitingCreated := encodeDBTime(base)
	cancelledCreated := encodeDBTime(base.Add(time.Minute))
	cancelledAt := encodeDBTime(base.Add(2 * time.Minute))
	restartCreated := encodeDBTime(base.Add(3 * time.Minute))
	const historyID = "history_v2_cancelled_action"
	if _, err := tx.Exec(
		`INSERT INTO history_entries(
			id, kind, status, request_text, started_at, completed_at
		 ) VALUES(?,?,?,?,?,?)`,
		historyID,
		HistoryKindAction,
		HistoryStatusSucceeded,
		"cancel legacy rollback",
		cancelledCreated,
		cancelledAt,
	); err != nil {
		t.Fatal(err)
	}

	expectedActions := []v2ActionSnapshot{
		{
			ActionID:             "action_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Kind:                 string(ActionKindRollbackDeployment),
			Origin:               string(ActionOriginDirectUser),
			TargetType:           string(ActionTargetApplication),
			TargetID:             "awaiting-app",
			ArgumentsJSON:        `{}`,
			SafetyClass:          string(ActionSafetyIrreversible),
			ConfirmationRequired: true,
			WarningRequired:      true,
			SupportsExecution:    false,
			PolicyReason:         string(ActionPolicyIrreversibleWarning),
			CurrentState:         string(ActionStateAwaitingConfirmation),
			CreatedAt:            awaitingCreated,
			UpdatedAt:            awaitingCreated,
		},
		{
			ActionID:             "action_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Kind:                 string(ActionKindRollbackDeployment),
			Origin:               string(ActionOriginDirectUser),
			TargetType:           string(ActionTargetApplication),
			TargetID:             "cancelled-app",
			ArgumentsJSON:        `{}`,
			SafetyClass:          string(ActionSafetyIrreversible),
			ConfirmationRequired: true,
			WarningRequired:      true,
			SupportsExecution:    false,
			PolicyReason:         string(ActionPolicyIrreversibleWarning),
			CurrentState:         string(ActionStateCancelled),
			HistoryEntryID:       sql.NullString{String: historyID, Valid: true},
			CreatedAt:            cancelledCreated,
			UpdatedAt:            cancelledAt,
		},
		{
			ActionID:             "action_cccccccccccccccccccccccccccccccc",
			Kind:                 string(ActionKindRestartApplication),
			Origin:               string(ActionOriginDirectUser),
			TargetType:           string(ActionTargetApplication),
			TargetID:             "myscheduler",
			ArgumentsJSON:        `{}`,
			SafetyClass:          string(ActionSafetyRecoverable),
			ConfirmationRequired: false,
			WarningRequired:      false,
			SupportsExecution:    false,
			PolicyReason:         string(ActionPolicyDirectUserNoConfirmation),
			CurrentState:         string(ActionStateAuthorized),
			CreatedAt:            restartCreated,
			UpdatedAt:            restartCreated,
		},
	}
	for _, action := range expectedActions {
		var historyValue any
		if action.HistoryEntryID.Valid {
			historyValue = action.HistoryEntryID.String
		}
		if _, err := tx.Exec(
			`INSERT INTO action_proposals(
				action_id, action_kind, origin, target_type, target_id, arguments_json,
				safety_class, confirmation_required, warning_required, supports_execution,
				policy_reason, current_state, history_entry_id, created_at, updated_at
			 ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			action.ActionID,
			action.Kind,
			action.Origin,
			action.TargetType,
			action.TargetID,
			action.ArgumentsJSON,
			action.SafetyClass,
			action.ConfirmationRequired,
			action.WarningRequired,
			action.SupportsExecution,
			action.PolicyReason,
			action.CurrentState,
			historyValue,
			action.CreatedAt,
			action.UpdatedAt,
		); err != nil {
			t.Fatal(err)
		}
	}

	expectedEvents := []v2EventSnapshot{
		{
			EventID:   "action_event_dddddddddddddddddddddddddddddddd",
			ActionID:  expectedActions[0].ActionID,
			Ordinal:   0,
			Type:      string(ActionEventProposalCreated),
			ToState:   string(ActionStateAwaitingConfirmation),
			CreatedAt: awaitingCreated,
		},
		{
			EventID:   "action_event_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
			ActionID:  expectedActions[1].ActionID,
			Ordinal:   0,
			Type:      string(ActionEventProposalCreated),
			ToState:   string(ActionStateAwaitingConfirmation),
			CreatedAt: cancelledCreated,
		},
		{
			EventID:  "action_event_ffffffffffffffffffffffffffffffff",
			ActionID: expectedActions[1].ActionID,
			Ordinal:  1,
			Type:     string(ActionEventCancelled),
			FromState: sql.NullString{
				String: string(ActionStateAwaitingConfirmation),
				Valid:  true,
			},
			ToState:   string(ActionStateCancelled),
			CreatedAt: cancelledAt,
		},
		{
			EventID:   "action_event_11111111111111111111111111111111",
			ActionID:  expectedActions[2].ActionID,
			Ordinal:   0,
			Type:      string(ActionEventPolicyAuthorizedOnCreation),
			ToState:   string(ActionStateAuthorized),
			CreatedAt: restartCreated,
		},
	}
	for _, event := range expectedEvents {
		var fromValue any
		if event.FromState.Valid {
			fromValue = event.FromState.String
		}
		if _, err := tx.Exec(
			`INSERT INTO action_events(
				event_id, action_id, ordinal, event_type, from_state, to_state, created_at
			 ) VALUES(?,?,?,?,?,?,?)`,
			event.EventID,
			event.ActionID,
			event.Ordinal,
			event.Type,
			fromValue,
			event.ToState,
			event.CreatedAt,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	beforeActions := readV2ActionSnapshots(t, db)
	beforeEvents := readV2EventSnapshots(t, db)
	if !reflect.DeepEqual(beforeActions, expectedActions) ||
		!reflect.DeepEqual(beforeEvents, expectedEvents) {

		t.Fatalf("schema-v2 fixture actions=%+v events=%+v", beforeActions, beforeEvents)
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
	afterActions := readV2ActionSnapshots(t, migrated.db)
	afterEvents := readV2EventSnapshots(t, migrated.db)
	if !reflect.DeepEqual(afterActions, beforeActions) {
		t.Fatalf("migrated actions changed: before=%+v after=%+v", beforeActions, afterActions)
	}
	if !reflect.DeepEqual(afterEvents, beforeEvents) {
		t.Fatalf("migrated events changed: before=%+v after=%+v", beforeEvents, afterEvents)
	}

	expectedStates := map[ActionID]ActionState{
		ActionID(expectedActions[0].ActionID): ActionStateAwaitingConfirmation,
		ActionID(expectedActions[1].ActionID): ActionStateCancelled,
		ActionID(expectedActions[2].ActionID): ActionStateAuthorized,
	}
	for id, state := range expectedStates {
		record, err := migrated.getActionProposal(id)
		if err != nil {
			t.Fatalf("restore %s: %v", id, err)
		}
		if record.Proposal.State() != state {
			t.Fatalf("restored %s state=%q want=%q", id, record.Proposal.State(), state)
		}
		if record.SupportsExecution {
			t.Fatalf("migrated pre-4B2 action %s gained execution support", id)
		}
	}
	cancelled, err := migrated.getActionProposal(ActionID(expectedActions[1].ActionID))
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.HistoryEntryID != historyID {
		t.Fatalf("History linkage=%q want=%q", cancelled.HistoryEntryID, historyID)
	}

	var executionEvents int
	if err := migrated.db.QueryRow(
		`SELECT COUNT(*) FROM action_events
		 WHERE event_type IN ('execution_started','execution_succeeded','execution_failed')`,
	).Scan(&executionEvents); err != nil {
		t.Fatal(err)
	}
	if executionEvents != 0 {
		t.Fatalf("migration invented %d execution events", executionEvents)
	}
	var failureCategories int
	if err := migrated.db.QueryRow(
		`SELECT COUNT(*) FROM action_events WHERE failure_category IS NOT NULL`,
	).Scan(&failureCategories); err != nil {
		t.Fatal(err)
	}
	if failureCategories != 0 {
		t.Fatalf("migration invented %d failure categories", failureCategories)
	}
}

func readV2ActionSnapshots(t *testing.T, db *sql.DB) []v2ActionSnapshot {
	t.Helper()
	rows, err := db.Query(
		`SELECT action_id, action_kind, origin, target_type, target_id, arguments_json,
			safety_class, confirmation_required, warning_required, supports_execution,
			policy_reason, current_state, history_entry_id, created_at, updated_at
		 FROM action_proposals ORDER BY action_id`,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	snapshots := []v2ActionSnapshot{}
	for rows.Next() {
		var snapshot v2ActionSnapshot
		if err := rows.Scan(
			&snapshot.ActionID,
			&snapshot.Kind,
			&snapshot.Origin,
			&snapshot.TargetType,
			&snapshot.TargetID,
			&snapshot.ArgumentsJSON,
			&snapshot.SafetyClass,
			&snapshot.ConfirmationRequired,
			&snapshot.WarningRequired,
			&snapshot.SupportsExecution,
			&snapshot.PolicyReason,
			&snapshot.CurrentState,
			&snapshot.HistoryEntryID,
			&snapshot.CreatedAt,
			&snapshot.UpdatedAt,
		); err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshots
}

func readV2EventSnapshots(t *testing.T, db *sql.DB) []v2EventSnapshot {
	t.Helper()
	rows, err := db.Query(
		`SELECT event_id, action_id, ordinal, event_type, from_state, to_state, created_at
		 FROM action_events ORDER BY action_id, ordinal`,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	snapshots := []v2EventSnapshot{}
	for rows.Next() {
		var snapshot v2EventSnapshot
		if err := rows.Scan(
			&snapshot.EventID,
			&snapshot.ActionID,
			&snapshot.Ordinal,
			&snapshot.Type,
			&snapshot.FromState,
			&snapshot.ToState,
			&snapshot.CreatedAt,
		); err != nil {
			t.Fatal(err)
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshots
}

func TestActionSchemaV3FreshDatabaseAndExecutionIntegrity(t *testing.T) {
	store := newActionTestStore(t)
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil ||
		version != miniAISchemaVersion {

		t.Fatalf("schema version=%d err=%v", version, err)
	}
	created := mustCreateRestart(t, store, ActionOriginDirectUser)
	executing, err := store.beginRestartApplicationExecution(created.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.completeRestartApplicationSuccess(executing.Proposal.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		`UPDATE action_events SET from_state = 'authorized'
		 WHERE action_id = ? AND event_type = 'execution_succeeded'`,
		created.Proposal.ID(),
	); err == nil {
		t.Fatal("schema accepted an invalid execution_succeeded transition")
	}
	loaded, err := store.getActionProposal(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Proposal.State() != ActionStateSucceeded {
		t.Fatalf("rejected tamper changed state to %q", loaded.Proposal.State())
	}
}

func TestActionSchemaV3EnforcesExactEventShapes(t *testing.T) {
	invalid := []struct {
		name            string
		eventType       ActionEventType
		fromState       any
		toState         ActionState
		failureCategory any
	}{
		{
			name:      "confirmation granted without from state",
			eventType: ActionEventConfirmationGranted,
			toState:   ActionStateAuthorized,
		},
		{
			name:      "cancelled without from state",
			eventType: ActionEventCancelled,
			toState:   ActionStateCancelled,
		},
		{
			name:      "execution started without from state",
			eventType: ActionEventExecutionStarted,
			toState:   ActionStateExecuting,
		},
		{
			name:      "execution succeeded without from state",
			eventType: ActionEventExecutionSucceeded,
			toState:   ActionStateSucceeded,
		},
		{
			name:            "execution failed without from state",
			eventType:       ActionEventExecutionFailed,
			toState:         ActionStateFailed,
			failureCategory: ActionExecutionFailureUpstreamRejected,
		},
		{
			name:      "execution started from awaiting confirmation",
			eventType: ActionEventExecutionStarted,
			fromState: ActionStateAwaitingConfirmation,
			toState:   ActionStateExecuting,
		},
		{
			name:      "execution started to succeeded",
			eventType: ActionEventExecutionStarted,
			fromState: ActionStateAuthorized,
			toState:   ActionStateSucceeded,
		},
		{
			name:      "execution succeeded from awaiting to cancelled",
			eventType: ActionEventExecutionSucceeded,
			fromState: ActionStateAwaitingConfirmation,
			toState:   ActionStateCancelled,
		},
		{
			name:            "execution succeeded with failure category",
			eventType:       ActionEventExecutionSucceeded,
			fromState:       ActionStateExecuting,
			toState:         ActionStateSucceeded,
			failureCategory: ActionExecutionFailureInternalError,
		},
		{
			name:            "execution failed from authorized",
			eventType:       ActionEventExecutionFailed,
			fromState:       ActionStateAuthorized,
			toState:         ActionStateFailed,
			failureCategory: ActionExecutionFailureUpstreamRejected,
		},
		{
			name:            "execution failed to succeeded",
			eventType:       ActionEventExecutionFailed,
			fromState:       ActionStateExecuting,
			toState:         ActionStateSucceeded,
			failureCategory: ActionExecutionFailureUpstreamRejected,
		},
		{
			name:      "execution failed without category",
			eventType: ActionEventExecutionFailed,
			fromState: ActionStateExecuting,
			toState:   ActionStateFailed,
		},
		{
			name:      "cancelled from authorized",
			eventType: ActionEventCancelled,
			fromState: ActionStateAuthorized,
			toState:   ActionStateCancelled,
		},
		{
			name:      "confirmation granted from authorized",
			eventType: ActionEventConfirmationGranted,
			fromState: ActionStateAuthorized,
			toState:   ActionStateAuthorized,
		},
	}
	for _, test := range invalid {
		t.Run("reject/"+test.name, func(t *testing.T) {
			store := newActionTestStore(t)
			actionID, createdAt := insertEventShapeTestAction(t, store)
			if _, err := store.db.Exec(
				`INSERT INTO action_events(
					event_id, action_id, ordinal, event_type, from_state, to_state,
					failure_category, created_at
				 ) VALUES(?,?,?,?,?,?,?,?)`,
				"action_event_22222222222222222222222222222222",
				actionID,
				0,
				test.eventType,
				test.fromState,
				test.toState,
				test.failureCategory,
				createdAt,
			); err == nil {
				t.Fatalf("schema accepted invalid event shape: %+v", test)
			}
		})
	}

	valid := []struct {
		name            string
		eventType       ActionEventType
		fromState       any
		toState         ActionState
		failureCategory any
	}{
		{
			name:      "proposal created",
			eventType: ActionEventProposalCreated,
			toState:   ActionStateAwaitingConfirmation,
		},
		{
			name:      "authorized on creation",
			eventType: ActionEventPolicyAuthorizedOnCreation,
			toState:   ActionStateAuthorized,
		},
		{
			name:      "confirmation granted",
			eventType: ActionEventConfirmationGranted,
			fromState: ActionStateAwaitingConfirmation,
			toState:   ActionStateAuthorized,
		},
		{
			name:      "cancelled",
			eventType: ActionEventCancelled,
			fromState: ActionStateAwaitingConfirmation,
			toState:   ActionStateCancelled,
		},
		{
			name:      "execution started",
			eventType: ActionEventExecutionStarted,
			fromState: ActionStateAuthorized,
			toState:   ActionStateExecuting,
		},
		{
			name:      "execution succeeded",
			eventType: ActionEventExecutionSucceeded,
			fromState: ActionStateExecuting,
			toState:   ActionStateSucceeded,
		},
		{
			name:            "execution failed",
			eventType:       ActionEventExecutionFailed,
			fromState:       ActionStateExecuting,
			toState:         ActionStateFailed,
			failureCategory: ActionExecutionFailureTimeout,
		},
	}
	for _, test := range valid {
		t.Run("accept/"+test.name, func(t *testing.T) {
			store := newActionTestStore(t)
			actionID, createdAt := insertEventShapeTestAction(t, store)
			if _, err := store.db.Exec(
				`INSERT INTO action_events(
					event_id, action_id, ordinal, event_type, from_state, to_state,
					failure_category, created_at
				 ) VALUES(?,?,?,?,?,?,?,?)`,
				"action_event_33333333333333333333333333333333",
				actionID,
				0,
				test.eventType,
				test.fromState,
				test.toState,
				test.failureCategory,
				createdAt,
			); err != nil {
				t.Fatalf("schema rejected valid event shape %+v: %v", test, err)
			}
		})
	}
}

func insertEventShapeTestAction(
	t *testing.T,
	store *chatStore,
) (ActionID, string) {
	t.Helper()
	actionID := ActionID("action_99999999999999999999999999999999")
	createdAt := encodeDBTime(actionTestTime())
	if _, err := store.db.Exec(
		`INSERT INTO action_proposals(
			action_id, action_kind, origin, target_type, target_id, arguments_json,
			safety_class, confirmation_required, warning_required, supports_execution,
			policy_reason, current_state, history_entry_id, created_at, updated_at
		 ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,NULL,?,?)`,
		actionID,
		ActionKindRestartApplication,
		ActionOriginDirectUser,
		ActionTargetApplication,
		"myscheduler",
		`{}`,
		ActionSafetyRecoverable,
		false,
		false,
		false,
		ActionPolicyDirectUserNoConfirmation,
		ActionStateAuthorized,
		createdAt,
		createdAt,
	); err != nil {
		t.Fatal(err)
	}
	return actionID, createdAt
}

func TestActionSchemaV3RejectsUnsafeFailureMetadata(t *testing.T) {
	store := newActionTestStore(t)
	created := mustCreateRestart(t, store, ActionOriginDirectUser)
	executing, err := store.beginRestartApplicationExecution(created.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.completeRestartApplicationFailure(
		executing.Proposal.ID(),
		ActionExecutionFailureUpstreamUnavailable,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(
		`UPDATE action_events SET failure_category = 'token=secret /private/path'
		 WHERE action_id = ? AND event_type = 'execution_failed'`,
		created.Proposal.ID(),
	); err == nil {

		t.Fatal("schema accepted arbitrary execution failure text")
	}
	if _, err := store.completeRestartApplicationFailure(
		created.Proposal.ID(),
		ActionExecutionFailureCategory("arbitrary"),
	); !errors.Is(err, errActionExecutionCompletionNotAllowed) {

		t.Fatalf("unsafe failure category error=%v", err)
	}
}
