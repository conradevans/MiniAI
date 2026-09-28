package main

import (
	"database/sql"
	"fmt"
	"strings"
)

type schemaColumn struct {
	Name    string
	Type    string
	NotNull bool
	Primary int
}

type schemaForeignKey struct {
	Table    string
	From     string
	To       string
	OnDelete string
}

type schemaIndex struct {
	Table   string
	Name    string
	Unique  bool
	Columns []string
	Desc    []bool
}

type schemaTable struct {
	Name        string
	Columns     []schemaColumn
	ForeignKeys []schemaForeignKey
}

func validateMiniAISchemaV1(tx *sql.Tx) error {
	tables := []schemaTable{
		{Name: "chats", Columns: []schemaColumn{
			{Name: "id", Type: "TEXT", Primary: 1},
			{Name: "title", Type: "TEXT", NotNull: true},
			{Name: "created_at", Type: "TEXT", NotNull: true},
			{Name: "updated_at", Type: "TEXT", NotNull: true},
		}},
		{Name: "messages", Columns: []schemaColumn{
			{Name: "id", Type: "INTEGER", Primary: 1},
			{Name: "chat_id", Type: "TEXT", NotNull: true},
			{Name: "role", Type: "TEXT", NotNull: true},
			{Name: "content", Type: "TEXT", NotNull: true},
			{Name: "created_at", Type: "TEXT", NotNull: true},
		}, ForeignKeys: []schemaForeignKey{
			{Table: "chats", From: "chat_id", To: "id", OnDelete: "CASCADE"},
		}},
		{Name: "evidence", Columns: []schemaColumn{
			{Name: "id", Type: "INTEGER", Primary: 1},
			{Name: "message_id", Type: "INTEGER", NotNull: true},
			{Name: "tool_name", Type: "TEXT", NotNull: true},
			{Name: "app", Type: "TEXT", NotNull: true},
			{Name: "source", Type: "TEXT", NotNull: true},
			{Name: "path", Type: "TEXT", NotNull: true},
			{Name: "arguments_json", Type: "TEXT", NotNull: true},
			{Name: "summary", Type: "TEXT", NotNull: true},
			{Name: "created_at", Type: "TEXT", NotNull: true},
		}, ForeignKeys: []schemaForeignKey{
			{Table: "messages", From: "message_id", To: "id", OnDelete: "CASCADE"},
		}},
		{Name: "history_entries", Columns: []schemaColumn{
			{Name: "id", Type: "TEXT", Primary: 1},
			{Name: "kind", Type: "TEXT", NotNull: true},
			{Name: "status", Type: "TEXT", NotNull: true},
			{Name: "chat_id", Type: "TEXT"},
			{Name: "user_message_id", Type: "INTEGER"},
			{Name: "assistant_message_id", Type: "INTEGER"},
			{Name: "request_text", Type: "TEXT", NotNull: true},
			{Name: "request_redacted", Type: "INTEGER", NotNull: true},
			{Name: "request_truncated", Type: "INTEGER", NotNull: true},
			{Name: "result_text", Type: "TEXT", NotNull: true},
			{Name: "result_redacted", Type: "INTEGER", NotNull: true},
			{Name: "result_truncated", Type: "INTEGER", NotNull: true},
			{Name: "started_at", Type: "TEXT", NotNull: true},
			{Name: "completed_at", Type: "TEXT"},
			{Name: "answer_mode", Type: "TEXT", NotNull: true},
			{Name: "route", Type: "TEXT", NotNull: true},
			{Name: "model", Type: "TEXT", NotNull: true},
			{Name: "model_mode", Type: "TEXT", NotNull: true},
			{Name: "inference_profile", Type: "TEXT", NotNull: true},
			{Name: "confidence", Type: "TEXT", NotNull: true},
			{Name: "model_invoked", Type: "INTEGER", NotNull: true},
			{Name: "planner_calls", Type: "INTEGER", NotNull: true},
			{Name: "reasoner_calls", Type: "INTEGER", NotNull: true},
			{Name: "evidence_rounds", Type: "INTEGER", NotNull: true},
			{Name: "second_round_reads", Type: "INTEGER", NotNull: true},
			{Name: "tool_calls", Type: "INTEGER", NotNull: true},
			{Name: "evidence_packet_bytes", Type: "INTEGER", NotNull: true},
			{Name: "answer_validation", Type: "TEXT", NotNull: true},
			{Name: "prompt_tokens", Type: "INTEGER", NotNull: true},
			{Name: "generated_tokens", Type: "INTEGER", NotNull: true},
			{Name: "load_seconds", Type: "REAL", NotNull: true},
			{Name: "prompt_seconds", Type: "REAL", NotNull: true},
			{Name: "total_seconds", Type: "REAL", NotNull: true},
			{Name: "investigation_seconds", Type: "REAL", NotNull: true},
			{Name: "error_category", Type: "TEXT", NotNull: true},
		}, ForeignKeys: []schemaForeignKey{
			{Table: "chats", From: "chat_id", To: "id", OnDelete: "SET NULL"},
			{Table: "messages", From: "user_message_id", To: "id", OnDelete: "SET NULL"},
			{Table: "messages", From: "assistant_message_id", To: "id", OnDelete: "SET NULL"},
		}},
		{Name: "history_targets", Columns: []schemaColumn{
			{Name: "history_entry_id", Type: "TEXT", NotNull: true, Primary: 1},
			{Name: "ordinal", Type: "INTEGER", NotNull: true, Primary: 2},
			{Name: "kind", Type: "TEXT", NotNull: true},
			{Name: "canonical_id", Type: "TEXT", NotNull: true},
			{Name: "display_name", Type: "TEXT", NotNull: true},
		}, ForeignKeys: []schemaForeignKey{
			{Table: "history_entries", From: "history_entry_id", To: "id", OnDelete: "CASCADE"},
		}},
		{Name: "history_evidence_reads", Columns: []schemaColumn{
			{Name: "id", Type: "INTEGER", Primary: 1},
			{Name: "history_entry_id", Type: "TEXT", NotNull: true},
			{Name: "request_id", Type: "TEXT", NotNull: true},
			{Name: "evidence_round", Type: "INTEGER", NotNull: true},
			{Name: "read_order", Type: "INTEGER", NotNull: true},
			{Name: "capability", Type: "TEXT", NotNull: true},
			{Name: "arguments_json", Type: "TEXT", NotNull: true},
			{Name: "safe_summary", Type: "TEXT", NotNull: true},
			{Name: "status", Type: "TEXT", NotNull: true},
			{Name: "availability", Type: "TEXT", NotNull: true},
			{Name: "started_at", Type: "TEXT"},
			{Name: "completed_at", Type: "TEXT"},
		}, ForeignKeys: []schemaForeignKey{
			{Table: "history_entries", From: "history_entry_id", To: "id", OnDelete: "CASCADE"},
		}},
		{Name: "history_links", Columns: []schemaColumn{
			{Name: "from_entry_id", Type: "TEXT", NotNull: true, Primary: 1},
			{Name: "to_entry_id", Type: "TEXT", NotNull: true, Primary: 2},
			{Name: "relation", Type: "TEXT", NotNull: true, Primary: 3},
			{Name: "created_at", Type: "TEXT", NotNull: true},
		}, ForeignKeys: []schemaForeignKey{
			{Table: "history_entries", From: "from_entry_id", To: "id", OnDelete: "RESTRICT"},
			{Table: "history_entries", From: "to_entry_id", To: "id", OnDelete: "RESTRICT"},
		}},
	}
	for _, table := range tables {
		if err := validateSchemaTable(tx, table); err != nil {
			return err
		}
	}
	indexes := []schemaIndex{
		{Table: "messages", Name: "messages_chat_created_idx", Columns: []string{"chat_id", "id"}, Desc: []bool{false, false}},
		{Table: "evidence", Name: "evidence_message_idx", Columns: []string{"message_id", "id"}, Desc: []bool{false, false}},
		{Table: "history_entries", Name: "history_entries_started_idx", Columns: []string{"started_at", "id"}, Desc: []bool{true, true}},
		{Table: "history_entries", Name: "history_entries_chat_idx", Columns: []string{"chat_id", "started_at", "id"}, Desc: []bool{false, true, true}},
		{Table: "history_entries", Name: "history_entries_kind_idx", Columns: []string{"kind", "started_at", "id"}, Desc: []bool{false, true, true}},
		{Table: "history_entries", Name: "history_entries_status_idx", Columns: []string{"status", "started_at", "id"}, Desc: []bool{false, true, true}},
		{Table: "history_entries", Name: "history_entries_route_idx", Columns: []string{"route", "started_at", "id"}, Desc: []bool{false, true, true}},
		{Table: "history_targets", Name: "history_targets_lookup_idx", Columns: []string{"kind", "canonical_id", "history_entry_id"}, Desc: []bool{false, false, false}},
		{Table: "history_evidence_reads", Name: "history_evidence_reads_entry_idx", Columns: []string{"history_entry_id", "evidence_round", "read_order", "id"}, Desc: []bool{false, false, false, false}},
		{Table: "history_links", Name: "history_links_to_idx", Columns: []string{"to_entry_id", "relation", "from_entry_id"}, Desc: []bool{false, false, false}},
	}
	for _, index := range indexes {
		if err := validateSchemaIndex(tx, index); err != nil {
			return err
		}
	}
	return nil
}

func validateSchemaTable(tx *sql.Tx, expected schemaTable) error {
	rows, err := tx.Query(`SELECT name, type, "notnull", pk FROM pragma_table_info(?) ORDER BY cid`, expected.Name)
	if err != nil {
		return fmt.Errorf("inspect table %s: %w", expected.Name, err)
	}
	actual := []schemaColumn{}
	for rows.Next() {
		var column schemaColumn
		var notNull int
		if err := rows.Scan(&column.Name, &column.Type, &notNull, &column.Primary); err != nil {
			_ = rows.Close()
			return fmt.Errorf("inspect table %s: %w", expected.Name, err)
		}
		column.Type = strings.ToUpper(strings.TrimSpace(column.Type))
		column.NotNull = notNull == 1
		actual = append(actual, column)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("inspect table %s: %w", expected.Name, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("inspect table %s: %w", expected.Name, err)
	}
	if len(actual) == 0 {
		return fmt.Errorf("required table %s is missing", expected.Name)
	}
	if len(actual) != len(expected.Columns) {
		return fmt.Errorf("table %s has %d columns; want %d", expected.Name, len(actual), len(expected.Columns))
	}
	for index := range expected.Columns {
		want, got := expected.Columns[index], actual[index]
		if got.Name != want.Name || got.Type != want.Type || got.NotNull != want.NotNull || got.Primary != want.Primary {
			return fmt.Errorf("table %s column %d is %+v; want %+v", expected.Name, index, got, want)
		}
	}

	foreignRows, err := tx.Query(`SELECT "table", "from", "to", on_delete FROM pragma_foreign_key_list(?)`, expected.Name)
	if err != nil {
		return fmt.Errorf("inspect table %s foreign keys: %w", expected.Name, err)
	}
	actualForeignKeys := map[schemaForeignKey]int{}
	for foreignRows.Next() {
		var foreignKey schemaForeignKey
		if err := foreignRows.Scan(&foreignKey.Table, &foreignKey.From, &foreignKey.To, &foreignKey.OnDelete); err != nil {
			_ = foreignRows.Close()
			return fmt.Errorf("inspect table %s foreign keys: %w", expected.Name, err)
		}
		foreignKey.OnDelete = strings.ToUpper(foreignKey.OnDelete)
		actualForeignKeys[foreignKey]++
	}
	if err := foreignRows.Err(); err != nil {
		_ = foreignRows.Close()
		return fmt.Errorf("inspect table %s foreign keys: %w", expected.Name, err)
	}
	if err := foreignRows.Close(); err != nil {
		return fmt.Errorf("inspect table %s foreign keys: %w", expected.Name, err)
	}
	if len(actualForeignKeys) != len(expected.ForeignKeys) {
		return fmt.Errorf("table %s has %d foreign keys; want %d", expected.Name, len(actualForeignKeys), len(expected.ForeignKeys))
	}
	for _, want := range expected.ForeignKeys {
		if actualForeignKeys[want] != 1 {
			return fmt.Errorf("table %s is missing foreign key %+v", expected.Name, want)
		}
	}
	return nil
}

func validateSchemaIndex(tx *sql.Tx, expected schemaIndex) error {
	var unique int
	err := tx.QueryRow(`SELECT "unique" FROM pragma_index_list(?) WHERE name = ?`, expected.Table, expected.Name).Scan(&unique)
	if err == sql.ErrNoRows {
		return fmt.Errorf("required index %s is missing", expected.Name)
	}
	if err != nil {
		return fmt.Errorf("inspect index %s: %w", expected.Name, err)
	}
	if (unique == 1) != expected.Unique {
		return fmt.Errorf("index %s uniqueness does not match schema", expected.Name)
	}
	rows, err := tx.Query(`SELECT name, "desc" FROM pragma_index_xinfo(?) WHERE "key" = 1 ORDER BY seqno`, expected.Name)
	if err != nil {
		return fmt.Errorf("inspect index %s columns: %w", expected.Name, err)
	}
	columns := []string{}
	descending := []bool{}
	for rows.Next() {
		var column string
		var desc int
		if err := rows.Scan(&column, &desc); err != nil {
			_ = rows.Close()
			return fmt.Errorf("inspect index %s columns: %w", expected.Name, err)
		}
		columns = append(columns, column)
		descending = append(descending, desc == 1)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("inspect index %s columns: %w", expected.Name, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("inspect index %s columns: %w", expected.Name, err)
	}
	if len(columns) != len(expected.Columns) {
		return fmt.Errorf("index %s has columns %v; want %v", expected.Name, columns, expected.Columns)
	}
	for index := range columns {
		if columns[index] != expected.Columns[index] || descending[index] != expected.Desc[index] {
			return fmt.Errorf("index %s has columns %v descending %v; want %v descending %v", expected.Name, columns, descending, expected.Columns, expected.Desc)
		}
	}
	return nil
}
