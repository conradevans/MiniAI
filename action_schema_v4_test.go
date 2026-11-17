package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

func TestActionSchemaV4PreservesV3FixturesAndAuditBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "production-v3.db")
	store, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the actual v3 event constraints before constructing legacy rows.
	for _, statement := range []string{
		`DROP TABLE action_events`, actionSchemaV3TableStatements[1],
		actionSchemaV3IndexStatements[1], actionSchemaV3IndexStatements[2],
		`PRAGMA user_version = 3`,
	} {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	ids := []ActionID{
		"action_6b29ccf5ff1eb4312a2c5b6769be8e90",
		"action_ec81dad4db3e2a1d536c326bb78860a6",
		"action_a8047de7d06225c0e6db08be367fe651",
		"action_b845e58f1426fb3f6362451bc6c71ae8",
		"action_0ef0b9ae027bc192714520136c5a61ca",
	}
	for index, id := range ids {
		history := startAdminActionHistory(t, store, "legacy audit fixture")
		if _, err := store.db.Exec(`UPDATE history_entries SET status='succeeded', completed_at=started_at WHERE id=?`, history.ID); err != nil {
			t.Fatal(err)
		}
		kind := ActionKindRestartApplication
		var arguments ActionArguments = RestartApplicationArguments{}
		origin := ActionOriginAssistantSuggested
		if index == 0 {
			kind, arguments = ActionKindRollbackDeployment, RollbackDeploymentArguments{}
		}
		if index == 1 {
			origin = ActionOriginDirectUser
		}
		record := mustCreateAction(t, store, actionProposalCreateInput{Kind: kind, Arguments: arguments, Origin: origin,
			Target: ActionTarget{Type: ActionTargetApplication, ID: "miniai-restart-canary"}, HistoryEntryID: history.ID, CreatedAt: actionTestTime()})
		if index == 0 || index == 3 {
			if _, err := store.cancelAction(record.Proposal.ID()); err != nil {
				t.Fatal(err)
			}
		} else {
			if origin == ActionOriginAssistantSuggested {
				record, err = store.confirmAction(record.Proposal.ID())
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.beginRestartApplicationExecution(record.Proposal); err != nil {
				t.Fatal(err)
			}
			if _, err := store.completeRestartApplicationSuccess(record.Proposal.ID()); err != nil {
				t.Fatal(err)
			}
		}
		// Fixture identity replacement is confined to this disposable test DB.
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`PRAGMA defer_foreign_keys=ON`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`UPDATE action_events SET action_id=? WHERE action_id=?`, id, record.Proposal.ID()); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`UPDATE action_proposals SET action_id=? WHERE action_id=?`, id, record.Proposal.ID()); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	// Legacy interrupted dispatch, definitive failure, and non-executable snapshot.
	interrupted := mustCreateRestart(t, store, ActionOriginDirectUser)
	if _, err := store.beginRestartApplicationExecution(interrupted.Proposal); err != nil {
		t.Fatal(err)
	}
	failed := mustCreateRestart(t, store, ActionOriginDirectUser)
	if _, err := store.beginRestartApplicationExecution(failed.Proposal); err != nil {
		t.Fatal(err)
	}
	if _, err := store.completeRestartApplicationFailure(failed.Proposal.ID(), ActionExecutionFailureUpstreamRejected); err != nil {
		t.Fatal(err)
	}
	legacy := mustCreateRestart(t, store, ActionOriginDirectUser)
	if _, err := store.db.Exec(`UPDATE action_proposals SET supports_execution=0 WHERE action_id=?`, legacy.Proposal.ID()); err != nil {
		t.Fatal(err)
	}
	before := actionAuditSnapshot(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reflect.DeepEqual(before, actionAuditSnapshot(t, reopened)) {
		t.Fatal("migration altered existing proposal/event/History bytes")
	}
	var version int
	if err := reopened.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 4 {
		t.Fatalf("version %d: %v", version, err)
	}
	for _, id := range append(ids, interrupted.Proposal.ID(), failed.Proposal.ID(), legacy.Proposal.ID()) {
		if _, err := reopened.getActionProposal(id); err != nil {
			t.Fatalf("legacy %s: %v", id, err)
		}
	}
	loaded, err := reopened.getActionProposal(interrupted.Proposal.ID())
	if err != nil || loaded.ExecutionOutcome != RestartApplicationExecutionOutcomeUnknown || loaded.FailureCategory != "" {
		t.Fatalf("legacy ambiguity: %+v %v", loaded, err)
	}
	if _, err := reopened.recordRestartApplicationUnknown(interrupted.Proposal.ID(), ActionExecutionFailureTimeout); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.recordRestartApplicationUnknown(interrupted.Proposal.ID(), "raw token /secret"); err == nil {
		t.Fatal("accepted open-ended category")
	}
}

func actionAuditSnapshot(t *testing.T, store *chatStore) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, table := range []string{"action_proposals", "action_events", "history_entries"} {
		rows, err := store.db.Query(`SELECT * FROM ` + table + ` ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var all [][]any
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			all = append(all, values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		encoded, err := json.Marshal(all)
		if err != nil {
			t.Fatal(err)
		}
		result[table] = string(encoded)
	}
	return result
}
