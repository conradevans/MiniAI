package main

import (
	"database/sql"
	"fmt"
)

var actionSchemaV3TableStatements = []string{
	`CREATE TABLE action_proposals (
		action_id TEXT NOT NULL PRIMARY KEY
			CHECK(length(action_id) = 39
				AND substr(action_id, 1, 7) = 'action_'
				AND substr(action_id, 8) NOT GLOB '*[^0-9a-f]*'),
		action_kind TEXT NOT NULL CHECK(action_kind IN ('restart_application','rollback_deployment','deploy_repository')),
		origin TEXT NOT NULL CHECK(origin IN ('direct_user','assistant_suggested')),
		target_type TEXT NOT NULL CHECK(target_type IN ('application','repository')),
		target_id TEXT NOT NULL,
		arguments_json TEXT NOT NULL,
		safety_class TEXT NOT NULL CHECK(safety_class IN ('reversible','recoverable','compensatable','irreversible')),
		confirmation_required INTEGER NOT NULL CHECK(confirmation_required IN (0,1)),
		warning_required INTEGER NOT NULL CHECK(warning_required IN (0,1)),
		supports_execution INTEGER NOT NULL DEFAULT 0 CHECK(supports_execution IN (0,1)),
		policy_reason TEXT NOT NULL,
		current_state TEXT NOT NULL CHECK(current_state IN ('awaiting_confirmation','authorized','executing','succeeded','failed','cancelled')),
		history_entry_id TEXT REFERENCES history_entries(id) ON DELETE SET NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE action_events (
		event_id TEXT NOT NULL PRIMARY KEY
			CHECK(length(event_id) = 45
				AND substr(event_id, 1, 13) = 'action_event_'
				AND substr(event_id, 14) NOT GLOB '*[^0-9a-f]*'),
		action_id TEXT NOT NULL REFERENCES action_proposals(action_id) ON DELETE CASCADE,
		ordinal INTEGER NOT NULL CHECK(ordinal >= 0),
		event_type TEXT NOT NULL CHECK(event_type IN ('proposal_created','policy_authorized_on_creation','confirmation_granted','cancelled','execution_started','execution_succeeded','execution_failed')),
		from_state TEXT CHECK(from_state IS NULL OR from_state IN ('awaiting_confirmation','authorized','executing')),
		to_state TEXT NOT NULL CHECK(to_state IN ('awaiting_confirmation','authorized','executing','succeeded','failed','cancelled')),
		failure_category TEXT CHECK(failure_category IS NULL OR failure_category IN ('upstream_unavailable','upstream_rejected','timeout','invalid_response','internal_error')),
		created_at TEXT NOT NULL,
		CHECK(
			(event_type = 'proposal_created'
				AND from_state IS NULL AND to_state = 'awaiting_confirmation'
				AND failure_category IS NULL)
			OR (event_type = 'policy_authorized_on_creation'
				AND from_state IS NULL AND to_state = 'authorized'
				AND failure_category IS NULL)
			OR (event_type = 'confirmation_granted'
				AND from_state IS NOT NULL
				AND from_state = 'awaiting_confirmation' AND to_state = 'authorized'
				AND failure_category IS NULL)
			OR (event_type = 'cancelled'
				AND from_state IS NOT NULL
				AND from_state = 'awaiting_confirmation' AND to_state = 'cancelled'
				AND failure_category IS NULL)
			OR (event_type = 'execution_started'
				AND from_state IS NOT NULL
				AND from_state = 'authorized' AND to_state = 'executing'
				AND failure_category IS NULL)
			OR (event_type = 'execution_succeeded'
				AND from_state IS NOT NULL
				AND from_state = 'executing' AND to_state = 'succeeded'
				AND failure_category IS NULL)
			OR (event_type = 'execution_failed'
				AND from_state IS NOT NULL
				AND from_state = 'executing' AND to_state = 'failed'
				AND failure_category IS NOT NULL
				AND failure_category IN ('upstream_unavailable','upstream_rejected','timeout','invalid_response','internal_error'))
		)
	)`,
}

var actionSchemaV3IndexStatements = []string{
	`CREATE INDEX action_proposals_history_idx
	 ON action_proposals(history_entry_id, created_at, action_id)`,
	`CREATE UNIQUE INDEX action_events_action_ordinal_idx
	 ON action_events(action_id, ordinal)`,
	`CREATE UNIQUE INDEX action_events_action_type_idx
	 ON action_events(action_id, event_type)`,
}

func migrateMiniAIActionSchemaV3(tx *sql.Tx) error {
	for _, statement := range []string{
		`DROP INDEX action_events_action_type_idx`,
		`DROP INDEX action_events_action_ordinal_idx`,
		`DROP INDEX action_proposals_history_idx`,
		`ALTER TABLE action_events RENAME TO action_events_v2`,
		`ALTER TABLE action_proposals RENAME TO action_proposals_v2`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("prepare action schema version 3 migration: %w", err)
		}
	}
	for _, statement := range actionSchemaV3TableStatements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("create action schema version 3: %w", err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO action_proposals(
			action_id, action_kind, origin, target_type, target_id, arguments_json,
			safety_class, confirmation_required, warning_required, supports_execution,
			policy_reason, current_state, history_entry_id, created_at, updated_at
		 ) SELECT
			action_id, action_kind, origin, target_type, target_id, arguments_json,
			safety_class, confirmation_required, warning_required, supports_execution,
			policy_reason, current_state, history_entry_id, created_at, updated_at
		 FROM action_proposals_v2`,
		`INSERT INTO action_events(
			event_id, action_id, ordinal, event_type, from_state, to_state,
			failure_category, created_at
		 ) SELECT
			event_id, action_id, ordinal, event_type, from_state, to_state,
			NULL, created_at
		 FROM action_events_v2`,
		`DROP TABLE action_events_v2`,
		`DROP TABLE action_proposals_v2`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("copy action schema version 3 data: %w", err)
		}
	}
	for _, statement := range actionSchemaV3IndexStatements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("index action schema version 3: %w", err)
		}
	}
	return nil
}

func validateMiniAISchemaV3(tx *sql.Tx) error {
	if err := validateMiniAISchemaV1(tx); err != nil {
		return err
	}
	tables := []schemaTable{
		{Name: "action_proposals", Columns: []schemaColumn{
			{Name: "action_id", Type: "TEXT", NotNull: true, Primary: 1},
			{Name: "action_kind", Type: "TEXT", NotNull: true},
			{Name: "origin", Type: "TEXT", NotNull: true},
			{Name: "target_type", Type: "TEXT", NotNull: true},
			{Name: "target_id", Type: "TEXT", NotNull: true},
			{Name: "arguments_json", Type: "TEXT", NotNull: true},
			{Name: "safety_class", Type: "TEXT", NotNull: true},
			{Name: "confirmation_required", Type: "INTEGER", NotNull: true},
			{Name: "warning_required", Type: "INTEGER", NotNull: true},
			{Name: "supports_execution", Type: "INTEGER", NotNull: true},
			{Name: "policy_reason", Type: "TEXT", NotNull: true},
			{Name: "current_state", Type: "TEXT", NotNull: true},
			{Name: "history_entry_id", Type: "TEXT"},
			{Name: "created_at", Type: "TEXT", NotNull: true},
			{Name: "updated_at", Type: "TEXT", NotNull: true},
		}, ForeignKeys: []schemaForeignKey{
			{Table: "history_entries", From: "history_entry_id", To: "id", OnDelete: "SET NULL"},
		}},
		{Name: "action_events", Columns: []schemaColumn{
			{Name: "event_id", Type: "TEXT", NotNull: true, Primary: 1},
			{Name: "action_id", Type: "TEXT", NotNull: true},
			{Name: "ordinal", Type: "INTEGER", NotNull: true},
			{Name: "event_type", Type: "TEXT", NotNull: true},
			{Name: "from_state", Type: "TEXT"},
			{Name: "to_state", Type: "TEXT", NotNull: true},
			{Name: "failure_category", Type: "TEXT"},
			{Name: "created_at", Type: "TEXT", NotNull: true},
		}, ForeignKeys: []schemaForeignKey{
			{Table: "action_proposals", From: "action_id", To: "action_id", OnDelete: "CASCADE"},
		}},
	}
	for _, table := range tables {
		if err := validateSchemaTable(tx, table); err != nil {
			return err
		}
	}
	indexes := []schemaIndex{
		{Table: "action_proposals", Name: "action_proposals_history_idx", Columns: []string{"history_entry_id", "created_at", "action_id"}, Desc: []bool{false, false, false}},
		{Table: "action_events", Name: "action_events_action_ordinal_idx", Unique: true, Columns: []string{"action_id", "ordinal"}, Desc: []bool{false, false}},
		{Table: "action_events", Name: "action_events_action_type_idx", Unique: true, Columns: []string{"action_id", "event_type"}, Desc: []bool{false, false}},
	}
	for _, index := range indexes {
		if err := validateSchemaIndex(tx, index); err != nil {
			return err
		}
	}
	rows, err := tx.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("validate action schema foreign keys: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("validate action schema foreign keys: violation detected")
	}
	return rows.Err()
}
