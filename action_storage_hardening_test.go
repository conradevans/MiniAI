package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type invalidDurableIDCase struct {
	name  string
	value any
}

func TestActionSchemaRejectsInvalidProposalIDs(t *testing.T) {
	for _, test := range invalidActionProposalIDs() {
		t.Run(test.name, func(t *testing.T) {
			store := newActionTestStore(t)
			if err := insertRawRestartProposal(store, test.value); err == nil {
				t.Fatalf("proposal ID %#v was accepted", test.value)
			}
			var count int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM action_proposals`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("invalid proposal row persisted: count=%d", count)
			}
		})
	}
}

func TestActionSchemaRejectsInvalidEventIDs(t *testing.T) {
	for _, test := range invalidActionEventIDs() {
		t.Run(test.name, func(t *testing.T) {
			store := newActionTestStore(t)
			created := mustCreateRestart(t, store, ActionOriginDirectUser)
			err := insertRawActionEvent(
				store,
				test.value,
				created.Proposal.ID(),
				1,
				ActionEventConfirmationGranted,
				ActionStateAwaitingConfirmation,
				ActionStateAuthorized,
				created.UpdatedAt.Add(time.Second),
			)
			if err == nil {
				t.Fatalf("event ID %#v was accepted", test.value)
			}
			var count int
			if err := store.db.QueryRow(
				`SELECT COUNT(*) FROM action_events WHERE action_id = ?`,
				created.Proposal.ID(),
			).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("invalid event row persisted: count=%d", count)
			}
		})
	}
}

func TestActionPersistenceRejectsTamperedPolicySnapshots(t *testing.T) {
	tests := []struct {
		name      string
		statement string
		value     any
	}{
		{
			name:      "confirmation required",
			statement: `UPDATE action_proposals SET confirmation_required = ? WHERE action_id = ?`,
			value:     false,
		},
		{
			name:      "warning required",
			statement: `UPDATE action_proposals SET warning_required = ? WHERE action_id = ?`,
			value:     false,
		},
		{
			name:      "policy reason",
			statement: `UPDATE action_proposals SET policy_reason = ? WHERE action_id = ?`,
			value:     ActionPolicyDirectUserNoConfirmation,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newActionTestStore(t)
			created := mustCreateRollback(t, store, ActionOriginDirectUser, "")
			execActionTestSQL(
				t,
				store,
				test.statement,
				test.value,
				created.Proposal.ID(),
			)
			requireInvalidStoredAction(t, store, created.Proposal.ID())
		})
	}
}

func TestActionPersistenceRejectsTamperedAuditChains(t *testing.T) {
	t.Run("invalid confirmation transition", func(t *testing.T) {
		store := newActionTestStore(t)
		created := mustCreateRollback(t, store, ActionOriginDirectUser, "")
		eventTime := created.UpdatedAt.Add(time.Second)
		execActionTestSQL(
			t,
			store,
			`UPDATE action_proposals SET current_state = ?, updated_at = ? WHERE action_id = ?`,
			ActionStateAuthorized,
			encodeDBTime(eventTime),
			created.Proposal.ID(),
		)
		if err := insertRawActionEvent(
			store,
			mustActionEventID(t),
			created.Proposal.ID(),
			1,
			ActionEventConfirmationGranted,
			ActionStateAuthorized,
			ActionStateAuthorized,
			eventTime,
		); err != nil {
			t.Fatal(err)
		}
		requireInvalidStoredAction(t, store, created.Proposal.ID())
	})

	t.Run("ordinal gap", func(t *testing.T) {
		store := newActionTestStore(t)
		confirmed := mustConfirmRollback(t, store)
		execActionTestSQL(
			t,
			store,
			`UPDATE action_events SET ordinal = 2
			 WHERE action_id = ? AND event_type = 'confirmation_granted'`,
			confirmed.Proposal.ID(),
		)
		requireInvalidStoredAction(t, store, confirmed.Proposal.ID())
	})

	t.Run("reordered invalid first event", func(t *testing.T) {
		store := newActionTestStore(t)
		confirmed := mustConfirmRollback(t, store)
		execActionTestSQL(
			t,
			store,
			`UPDATE action_events SET ordinal = ordinal + 10 WHERE action_id = ?`,
			confirmed.Proposal.ID(),
		)
		execActionTestSQL(
			t,
			store,
			`UPDATE action_events
			 SET ordinal = 0,
			     created_at = (SELECT created_at FROM action_proposals WHERE action_id = ?)
			 WHERE action_id = ? AND event_type = 'confirmation_granted'`,
			confirmed.Proposal.ID(),
			confirmed.Proposal.ID(),
		)
		execActionTestSQL(
			t,
			store,
			`UPDATE action_events
			 SET ordinal = 1,
			     created_at = (SELECT updated_at FROM action_proposals WHERE action_id = ?)
			 WHERE action_id = ? AND event_type = 'proposal_created'`,
			confirmed.Proposal.ID(),
			confirmed.Proposal.ID(),
		)
		requireInvalidStoredAction(t, store, confirmed.Proposal.ID())
	})

	t.Run("mixed confirmation and cancellation", func(t *testing.T) {
		store := newActionTestStore(t)
		confirmed := mustConfirmRollback(t, store)
		eventTime := confirmed.UpdatedAt.Add(time.Second)
		if err := insertRawActionEvent(
			store,
			mustActionEventID(t),
			confirmed.Proposal.ID(),
			2,
			ActionEventCancelled,
			ActionStateAwaitingConfirmation,
			ActionStateCancelled,
			eventTime,
		); err != nil {
			t.Fatal(err)
		}
		execActionTestSQL(
			t,
			store,
			`UPDATE action_proposals SET updated_at = ? WHERE action_id = ?`,
			encodeDBTime(eventTime),
			confirmed.Proposal.ID(),
		)
		requireInvalidStoredAction(t, store, confirmed.Proposal.ID())
	})

	t.Run("event moved to another action", func(t *testing.T) {
		store := newActionTestStore(t)
		first := mustCreateRollback(t, store, ActionOriginDirectUser, "")
		second := mustCreateAction(t, store, actionProposalCreateInput{
			Kind:      ActionKindRollbackDeployment,
			Origin:    ActionOriginDirectUser,
			Target:    ActionTarget{Type: ActionTargetApplication, ID: "other-app"},
			Arguments: RollbackDeploymentArguments{},
			CreatedAt: actionTestTime().Add(time.Hour),
		})
		execActionTestSQL(
			t,
			store,
			`DELETE FROM action_events WHERE action_id = ?`,
			second.Proposal.ID(),
		)
		execActionTestSQL(
			t,
			store,
			`UPDATE action_events SET action_id = ? WHERE action_id = ?`,
			second.Proposal.ID(),
			first.Proposal.ID(),
		)
		requireInvalidStoredAction(t, store, first.Proposal.ID())
		requireInvalidStoredAction(t, store, second.Proposal.ID())
	})

	t.Run("final state disagrees with confirmation", func(t *testing.T) {
		store := newActionTestStore(t)
		confirmed := mustConfirmRollback(t, store)
		execActionTestSQL(
			t,
			store,
			`UPDATE action_proposals SET current_state = 'cancelled' WHERE action_id = ?`,
			confirmed.Proposal.ID(),
		)
		requireInvalidStoredAction(t, store, confirmed.Proposal.ID())
	})

	t.Run("wrong event type for valid transition", func(t *testing.T) {
		store := newActionTestStore(t)
		confirmed := mustConfirmRollback(t, store)
		execActionTestSQL(
			t,
			store,
			`UPDATE action_events SET event_type = 'cancelled'
			 WHERE action_id = ? AND event_type = 'confirmation_granted'`,
			confirmed.Proposal.ID(),
		)
		requireInvalidStoredAction(t, store, confirmed.Proposal.ID())
	})
}

func invalidActionProposalIDs() []invalidDurableIDCase {
	return []invalidDurableIDCase{
		{name: "null", value: nil},
		{name: "empty", value: ""},
		{name: "too short", value: "action_" + strings.Repeat("0", 31)},
		{name: "too long", value: "action_" + strings.Repeat("0", 33)},
		{name: "invalid prefix", value: "actiom_" + strings.Repeat("0", 32)},
		{name: "uppercase hexadecimal", value: "action_" + strings.Repeat("0", 31) + "A"},
		{name: "non hexadecimal", value: "action_" + strings.Repeat("0", 31) + "g"},
	}
}

func invalidActionEventIDs() []invalidDurableIDCase {
	return []invalidDurableIDCase{
		{name: "null", value: nil},
		{name: "empty", value: ""},
		{name: "too short", value: "action_event_" + strings.Repeat("0", 31)},
		{name: "too long", value: "action_event_" + strings.Repeat("0", 33)},
		{name: "invalid prefix", value: "action_evenu_" + strings.Repeat("0", 32)},
		{name: "uppercase hexadecimal", value: "action_event_" + strings.Repeat("0", 31) + "A"},
		{name: "non hexadecimal", value: "action_event_" + strings.Repeat("0", 31) + "g"},
	}
}

func insertRawRestartProposal(store *chatStore, id any) error {
	_, err := store.db.Exec(
		`INSERT INTO action_proposals(
			action_id, action_kind, origin, target_type, target_id, arguments_json,
			safety_class, confirmation_required, warning_required, supports_execution,
			policy_reason, current_state, history_entry_id, created_at, updated_at
		 ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id,
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
		encodeDBTime(actionTestTime()),
		encodeDBTime(actionTestTime()),
	)
	return err
}

func insertRawActionEvent(
	store *chatStore,
	eventID any,
	actionID ActionID,
	ordinal int,
	eventType ActionEventType,
	fromState ActionState,
	toState ActionState,
	createdAt time.Time,
) error {
	_, err := store.db.Exec(
		`INSERT INTO action_events(
			event_id, action_id, ordinal, event_type, from_state, to_state, created_at
		 ) VALUES(?,?,?,?,?,?,?)`,
		eventID,
		actionID,
		ordinal,
		eventType,
		fromState,
		toState,
		encodeDBTime(createdAt),
	)
	return err
}

func mustActionEventID(t *testing.T) string {
	t.Helper()
	id, err := newActionEventID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustConfirmRollback(t *testing.T, store *chatStore) storedActionProposal {
	t.Helper()
	created := mustCreateRollback(t, store, ActionOriginDirectUser, "")
	confirmed, err := store.confirmAction(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	return confirmed
}

func requireInvalidStoredAction(t *testing.T, store *chatStore, id ActionID) {
	t.Helper()
	if _, err := store.getActionProposal(id); !errors.Is(err, errInvalidPersistedAction) {
		t.Fatalf("expected invalid persisted action, got %v", err)
	}
}
