package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	errHistoryNotFound         = errors.New("history entry not found")
	errHistoryAlreadyFinalized = errors.New("history entry already finalized")
	errHistoryLinkLimit        = errors.New("history entry link limit reached")
)

const historyEntryColumns = `id, kind, status, chat_id, user_message_id, assistant_message_id,
	request_text, request_redacted, request_truncated, result_text, result_redacted, result_truncated,
	started_at, completed_at, answer_mode, route, model, model_mode,
	inference_profile, confidence, model_invoked, planner_calls, reasoner_calls, evidence_rounds,
	second_round_reads, tool_calls, evidence_packet_bytes, answer_validation, prompt_tokens,
	generated_tokens, load_seconds, prompt_seconds, total_seconds, investigation_seconds, error_category`

type HistoryListFilter struct {
	Limit      int
	BeforeTime *time.Time
	BeforeID   string
	Kind       HistoryKind
	Status     HistoryStatus
	ChatID     string
	Route      string
	TargetKind string
	TargetID   string
}

type HistorySummary struct {
	ID               string          `json:"id"`
	Kind             HistoryKind     `json:"kind"`
	Status           HistoryStatus   `json:"status"`
	ChatID           string          `json:"chat_id,omitempty"`
	StartedAt        time.Time       `json:"started_at"`
	CompletedAt      *time.Time      `json:"completed_at,omitempty"`
	RequestPreview   string          `json:"request_preview"`
	ResultPreview    string          `json:"result_preview,omitempty"`
	AnswerMode       string          `json:"answer_mode,omitempty"`
	Route            string          `json:"route,omitempty"`
	Targets          []HistoryTarget `json:"targets"`
	Model            string          `json:"model,omitempty"`
	Confidence       string          `json:"confidence,omitempty"`
	EvidenceRounds   int             `json:"evidence_rounds"`
	SecondRoundReads int             `json:"second_round_reads"`
	ToolCalls        int             `json:"tool_calls"`
}

type HistoryListResult struct {
	Entries []HistorySummary
	HasMore bool
}

func (s *chatStore) startHistoryEntry(chatID, requestText string, startedAt time.Time) (HistoryEntry, storedMessage, error) {
	id, err := randomID(16)
	if err != nil {
		return HistoryEntry{}, storedMessage{}, err
	}
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	startedAt = startedAt.UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return HistoryEntry{}, storedMessage{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var userMessage storedMessage
	var chatValue any
	var userMessageValue any
	if chatID != "" {
		userMessage, err = appendMessageTx(tx, chatID, "user", requestText, nil, startedAt)
		if err != nil {
			return HistoryEntry{}, storedMessage{}, err
		}
		chatValue = chatID
		userMessageValue = userMessage.ID
	}
	requestText, requestRedacted, requestTruncated := sanitizeHistoryAuditText(requestText, historyRequestMaxRunes)
	_, err = tx.Exec(
		`INSERT INTO history_entries(
			id, kind, status, chat_id, user_message_id, request_text, request_redacted, request_truncated, started_at
		 ) VALUES(?,?,?,?,?,?,?,?,?)`,
		id, HistoryKindConversation, HistoryStatusRunning, chatValue, userMessageValue, requestText,
		requestRedacted, requestTruncated, encodeHistoryTime(startedAt),
	)
	if err != nil {
		return HistoryEntry{}, storedMessage{}, err
	}
	if err := tx.Commit(); err != nil {
		return HistoryEntry{}, storedMessage{}, err
	}
	return HistoryEntry{
		ID: id, Kind: HistoryKindConversation, Status: HistoryStatusRunning, ChatID: chatID,
		UserMessageID: userMessage.ID, RequestText: requestText, RequestRedacted: requestRedacted,
		RequestTruncated: requestTruncated, StartedAt: startedAt,
		Targets: []HistoryTarget{}, EvidenceReads: []HistoryEvidenceRead{}, Links: []HistoryLink{},
	}, userMessage, nil
}

func (s *chatStore) finalizeHistoryEntry(id string, input historyFinalizeInput) (HistoryEntry, error) {
	if !validHistoryStatus(input.Status) || input.Status == HistoryStatusRunning {
		return HistoryEntry{}, fmt.Errorf("invalid final history status %q", input.Status)
	}
	input.Kind = normalizeHistoryKind(input.Kind)
	if input.CompletedAt.IsZero() {
		input.CompletedAt = time.Now().UTC()
	}
	input.CompletedAt = input.CompletedAt.UTC()
	var resultRedacted, resultTruncated bool
	input.ResultText, resultRedacted, resultTruncated = sanitizeHistoryAuditText(input.ResultText, historyResultMaxRunes)
	input.Targets = sanitizeHistoryTargets(input.Targets)
	input.EvidenceReads = sanitizeHistoryEvidenceReads(input.EvidenceReads)
	input.Metadata = sanitizeHistoryMetadata(input.Metadata)

	tx, err := s.db.Begin()
	if err != nil {
		return HistoryEntry{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var currentStatus string
	var chatID sql.NullString
	err = tx.QueryRow(`SELECT status, chat_id FROM history_entries WHERE id = ?`, id).Scan(&currentStatus, &chatID)
	if errors.Is(err, sql.ErrNoRows) {
		return HistoryEntry{}, errHistoryNotFound
	}
	if err != nil {
		return HistoryEntry{}, err
	}
	if HistoryStatus(currentStatus) != HistoryStatusRunning {
		return HistoryEntry{}, errHistoryAlreadyFinalized
	}

	var assistantMessageID any
	if input.PersistAssistant && chatID.Valid && strings.TrimSpace(input.AssistantText) != "" {
		assistant, err := appendMessageTx(tx, chatID.String, "assistant", input.AssistantText, input.AssistantEvidence, input.CompletedAt)
		if err != nil {
			return HistoryEntry{}, err
		}
		assistantMessageID = assistant.ID
	}
	for _, target := range input.Targets {
		if _, err := tx.Exec(
			`INSERT INTO history_targets(history_entry_id, ordinal, kind, canonical_id, display_name) VALUES(?,?,?,?,?)`,
			id, target.Ordinal, target.Kind, target.CanonicalID, target.DisplayName,
		); err != nil {
			return HistoryEntry{}, err
		}
	}
	for _, read := range input.EvidenceReads {
		argumentsJSON, err := json.Marshal(read.Arguments)
		if err != nil {
			return HistoryEntry{}, err
		}
		var startedAt, completedAt any
		if read.StartedAt != nil {
			startedAt = encodeHistoryTime(*read.StartedAt)
		}
		if read.CompletedAt != nil {
			completedAt = encodeHistoryTime(*read.CompletedAt)
		}
		if _, err := tx.Exec(
			`INSERT INTO history_evidence_reads(
				history_entry_id, request_id, evidence_round, read_order, capability, arguments_json,
				safe_summary, status, availability, started_at, completed_at
			 ) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			id, read.RequestID, read.EvidenceRound, read.Order, read.Capability, string(argumentsJSON),
			read.SafeSummary, read.Status, read.Availability, startedAt, completedAt,
		); err != nil {
			return HistoryEntry{}, err
		}
	}
	metadata := input.Metadata
	result, err := tx.Exec(
		`UPDATE history_entries SET
			kind = ?, status = ?, assistant_message_id = ?, result_text = ?, result_redacted = ?, result_truncated = ?, completed_at = ?,
			answer_mode = ?, route = ?, model = ?, model_mode = ?, inference_profile = ?, confidence = ?,
			model_invoked = ?, planner_calls = ?, reasoner_calls = ?, evidence_rounds = ?,
			second_round_reads = ?, tool_calls = ?, evidence_packet_bytes = ?, answer_validation = ?,
			prompt_tokens = ?, generated_tokens = ?, load_seconds = ?, prompt_seconds = ?, total_seconds = ?,
			investigation_seconds = ?, error_category = ?
		 WHERE id = ? AND status = 'running'`,
		input.Kind, input.Status, assistantMessageID, input.ResultText, resultRedacted, resultTruncated, encodeHistoryTime(input.CompletedAt),
		metadata.AnswerMode, metadata.Route, metadata.Model, metadata.ModelMode, metadata.InferenceProfile, metadata.Confidence,
		metadata.ModelInvoked, metadata.PlannerCalls, metadata.ReasonerCalls, metadata.EvidenceRounds,
		metadata.SecondRoundReads, metadata.ToolCalls, metadata.EvidencePacketBytes, metadata.AnswerValidation,
		metadata.PromptTokens, metadata.GeneratedTokens, metadata.LoadSeconds, metadata.PromptSeconds, metadata.TotalSeconds,
		metadata.InvestigationSeconds, metadata.ErrorCategory, id,
	)
	if err != nil {
		return HistoryEntry{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return HistoryEntry{}, errHistoryAlreadyFinalized
	}
	if err := tx.Commit(); err != nil {
		return HistoryEntry{}, err
	}
	return s.getHistoryEntry(id)
}

func sanitizeHistoryMetadata(metadata HistoryExecutionMetadata) HistoryExecutionMetadata {
	metadata.AnswerMode = sanitizeHistoryText(metadata.AnswerMode, 80)
	metadata.Route = sanitizeHistoryText(metadata.Route, 120)
	metadata.Model = sanitizeHistoryText(metadata.Model, 120)
	metadata.ModelMode = sanitizeHistoryText(metadata.ModelMode, 80)
	metadata.InferenceProfile = sanitizeHistoryText(metadata.InferenceProfile, 80)
	metadata.Confidence = sanitizeHistoryText(metadata.Confidence, 80)
	metadata.AnswerValidation = sanitizeHistoryText(metadata.AnswerValidation, 80)
	metadata.ErrorCategory = sanitizeHistoryText(metadata.ErrorCategory, 120)
	metadata.PlannerCalls = boundedNonnegative(metadata.PlannerCalls, 100)
	metadata.ReasonerCalls = boundedNonnegative(metadata.ReasonerCalls, 100)
	metadata.EvidenceRounds = boundedNonnegative(metadata.EvidenceRounds, 10)
	metadata.SecondRoundReads = boundedNonnegative(metadata.SecondRoundReads, historyMaximumEvidenceReads)
	metadata.ToolCalls = boundedNonnegative(metadata.ToolCalls, historyMaximumEvidenceReads)
	metadata.EvidencePacketBytes = boundedNonnegative(metadata.EvidencePacketBytes, 1<<20)
	metadata.PromptTokens = boundedNonnegative(metadata.PromptTokens, 1<<24)
	metadata.GeneratedTokens = boundedNonnegative(metadata.GeneratedTokens, 1<<24)
	metadata.LoadSeconds = boundedNonnegativeFloat(metadata.LoadSeconds, 24*60*60)
	metadata.PromptSeconds = boundedNonnegativeFloat(metadata.PromptSeconds, 24*60*60)
	metadata.TotalSeconds = boundedNonnegativeFloat(metadata.TotalSeconds, 24*60*60)
	metadata.InvestigationSeconds = boundedNonnegativeFloat(metadata.InvestigationSeconds, 24*60*60)
	return metadata
}

func boundedNonnegative(value, maximum int) int {
	if value < 0 {
		return 0
	}
	if value > maximum {
		return maximum
	}
	return value
}

func boundedNonnegativeFloat(value, maximum float64) float64 {
	if value < 0 {
		return 0
	}
	if value > maximum {
		return maximum
	}
	return value
}

func (s *chatStore) cancelHistoryEntry(id string, completedAt time.Time, metadata HistoryExecutionMetadata, reads []HistoryEvidenceRead, targets []HistoryTarget) (HistoryEntry, error) {
	return s.finalizeHistoryEntry(id, historyFinalizeInput{
		Kind: historyKindForCapture(metadata, reads), Status: HistoryStatusCanceled, CompletedAt: completedAt,
		Metadata: metadata, EvidenceReads: reads, Targets: targets,
	})
}

func (s *chatStore) interruptRunningHistory(completedAt time.Time) (int64, error) {
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}
	result, err := s.db.Exec(
		`UPDATE history_entries
		 SET status = 'interrupted', completed_at = ?, error_category = 'process_restart'
		 WHERE status = 'running'`,
		encodeHistoryTime(completedAt),
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *chatStore) addHistoryLink(link HistoryLink) error {
	switch link.Relation {
	case "undo_of", "recovery_for", "preview_for", "code_change_for":
	default:
		return fmt.Errorf("invalid history relation")
	}
	if link.CreatedAt.IsZero() {
		link.CreatedAt = time.Now().UTC()
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	entryIDs := []string{link.FromEntryID}
	if link.ToEntryID != link.FromEntryID {
		entryIDs = append(entryIDs, link.ToEntryID)
	}
	for _, entryID := range entryIDs {
		var count int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM history_links WHERE from_entry_id = ? OR to_entry_id = ?`, entryID, entryID,
		).Scan(&count); err != nil {
			return err
		}
		if count >= historyMaximumLinks {
			return errHistoryLinkLimit
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO history_links(from_entry_id, to_entry_id, relation, created_at) VALUES(?,?,?,?)`,
		link.FromEntryID, link.ToEntryID, link.Relation, encodeHistoryTime(link.CreatedAt),
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *chatStore) chatMessageHistoryLinks(chatID string) (map[int64]string, error) {
	rows, err := s.db.Query(
		`SELECT id, user_message_id, assistant_message_id FROM history_entries
		 WHERE chat_id = ? ORDER BY started_at ASC, id ASC`, chatID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id string
		var userID, assistantID sql.NullInt64
		if err := rows.Scan(&id, &userID, &assistantID); err != nil {
			return nil, err
		}
		if userID.Valid {
			out[userID.Int64] = id
		}
		if assistantID.Valid {
			out[assistantID.Int64] = id
		}
	}
	return out, rows.Err()
}

func (s *chatStore) getHistoryEntry(id string) (HistoryEntry, error) {
	row := s.db.QueryRow(`SELECT `+historyEntryColumns+` FROM history_entries WHERE id = ?`, id)
	entry, err := scanHistoryEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return HistoryEntry{}, errHistoryNotFound
	}
	if err != nil {
		return HistoryEntry{}, err
	}
	entry.Targets, err = s.historyTargets(id)
	if err != nil {
		return HistoryEntry{}, err
	}
	entry.EvidenceReads, err = s.historyEvidenceReads(id)
	if err != nil {
		return HistoryEntry{}, err
	}
	entry.Links, entry.LinksTruncated, err = s.historyLinks(id)
	if err != nil {
		return HistoryEntry{}, err
	}
	return entry, nil
}

func (s *chatStore) listHistory(filter HistoryListFilter) (HistoryListResult, error) {
	if filter.Limit < 1 || filter.Limit > 100 {
		filter.Limit = 50
	}
	clauses := []string{}
	arguments := []any{}
	if filter.BeforeTime != nil && filter.BeforeID != "" {
		encoded := encodeHistoryTime(*filter.BeforeTime)
		clauses = append(clauses, `(started_at < ? OR (started_at = ? AND id < ?))`)
		arguments = append(arguments, encoded, encoded, filter.BeforeID)
	}
	if filter.Kind != "" {
		clauses = append(clauses, `kind = ?`)
		arguments = append(arguments, filter.Kind)
	}
	if filter.Status != "" {
		clauses = append(clauses, `status = ?`)
		arguments = append(arguments, filter.Status)
	}
	if filter.ChatID != "" {
		clauses = append(clauses, `chat_id = ?`)
		arguments = append(arguments, filter.ChatID)
	}
	if filter.Route != "" {
		clauses = append(clauses, `route = ?`)
		arguments = append(arguments, filter.Route)
	}
	if filter.TargetKind != "" || filter.TargetID != "" {
		targetClauses := []string{`target.history_entry_id = history_entries.id`}
		if filter.TargetKind != "" {
			targetClauses = append(targetClauses, `target.kind = ?`)
			arguments = append(arguments, filter.TargetKind)
		}
		if filter.TargetID != "" {
			targetClauses = append(targetClauses, `target.canonical_id = ?`)
			arguments = append(arguments, filter.TargetID)
		}
		clauses = append(clauses, `EXISTS (SELECT 1 FROM history_targets target WHERE `+strings.Join(targetClauses, " AND ")+`)`)
	}
	query := `SELECT ` + historyEntryColumns + ` FROM history_entries`
	if len(clauses) > 0 {
		query += ` WHERE ` + strings.Join(clauses, " AND ")
	}
	query += ` ORDER BY started_at DESC, id DESC LIMIT ?`
	arguments = append(arguments, filter.Limit+1)
	rows, err := s.db.Query(query, arguments...)
	if err != nil {
		return HistoryListResult{}, err
	}
	entries := []HistoryEntry{}
	for rows.Next() {
		entry, err := scanHistoryEntry(rows)
		if err != nil {
			_ = rows.Close()
			return HistoryListResult{}, err
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return HistoryListResult{}, err
	}
	if err := rows.Close(); err != nil {
		return HistoryListResult{}, err
	}
	hasMore := len(entries) > filter.Limit
	if hasMore {
		entries = entries[:filter.Limit]
	}
	out := make([]HistorySummary, 0, len(entries))
	for _, entry := range entries {
		targets, err := s.historyTargets(entry.ID)
		if err != nil {
			return HistoryListResult{}, err
		}
		out = append(out, HistorySummary{
			ID: entry.ID, Kind: entry.Kind, Status: entry.Status, ChatID: entry.ChatID,
			StartedAt: entry.StartedAt, CompletedAt: entry.CompletedAt,
			RequestPreview: truncateHistoryRunes(entry.RequestText, historyPreviewMaxRunes),
			ResultPreview:  truncateHistoryRunes(entry.ResultText, historyPreviewMaxRunes),
			AnswerMode:     entry.AnswerMode, Route: entry.Route, Targets: targets,
			Model: entry.Model, Confidence: entry.Confidence, EvidenceRounds: entry.EvidenceRounds,
			SecondRoundReads: entry.SecondRoundReads, ToolCalls: entry.ToolCalls,
		})
	}
	return HistoryListResult{Entries: out, HasMore: hasMore}, nil
}

func encodeHistoryTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

type historyRowScanner interface {
	Scan(...any) error
}

func scanHistoryEntry(row historyRowScanner) (HistoryEntry, error) {
	var entry HistoryEntry
	var kind, status string
	var chatID, completedAt sql.NullString
	var userMessageID, assistantMessageID sql.NullInt64
	var startedAt string
	var modelInvoked bool
	err := row.Scan(
		&entry.ID, &kind, &status, &chatID, &userMessageID, &assistantMessageID,
		&entry.RequestText, &entry.RequestRedacted, &entry.RequestTruncated,
		&entry.ResultText, &entry.ResultRedacted, &entry.ResultTruncated, &startedAt, &completedAt,
		&entry.AnswerMode, &entry.Route, &entry.Model, &entry.ModelMode, &entry.InferenceProfile, &entry.Confidence,
		&modelInvoked, &entry.PlannerCalls, &entry.ReasonerCalls, &entry.EvidenceRounds,
		&entry.SecondRoundReads, &entry.ToolCalls, &entry.EvidencePacketBytes, &entry.AnswerValidation,
		&entry.PromptTokens, &entry.GeneratedTokens, &entry.LoadSeconds, &entry.PromptSeconds,
		&entry.TotalSeconds, &entry.InvestigationSeconds, &entry.ErrorCategory,
	)
	if err != nil {
		return HistoryEntry{}, err
	}
	entry.Kind, entry.Status, entry.ModelInvoked = HistoryKind(kind), HistoryStatus(status), modelInvoked
	entry.ChatID = chatID.String
	entry.UserMessageID, entry.AssistantMessageID = userMessageID.Int64, assistantMessageID.Int64
	entry.StartedAt = decodeDBTime(startedAt)
	if completedAt.Valid {
		value := decodeDBTime(completedAt.String)
		entry.CompletedAt = &value
	}
	entry.Targets = []HistoryTarget{}
	entry.EvidenceReads = []HistoryEvidenceRead{}
	entry.Links = []HistoryLink{}
	return entry, nil
}

func (s *chatStore) historyTargets(id string) ([]HistoryTarget, error) {
	rows, err := s.db.Query(
		`SELECT ordinal, kind, canonical_id, display_name FROM history_targets
		 WHERE history_entry_id = ? ORDER BY ordinal ASC`, id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryTarget{}
	for rows.Next() {
		var target HistoryTarget
		if err := rows.Scan(&target.Ordinal, &target.Kind, &target.CanonicalID, &target.DisplayName); err != nil {
			return nil, err
		}
		out = append(out, target)
	}
	return out, rows.Err()
}

func (s *chatStore) historyEvidenceReads(id string) ([]HistoryEvidenceRead, error) {
	rows, err := s.db.Query(
		`SELECT id, request_id, evidence_round, read_order, capability, arguments_json, safe_summary,
			status, availability, started_at, completed_at
		 FROM history_evidence_reads WHERE history_entry_id = ?
		 ORDER BY evidence_round ASC, read_order ASC, id ASC`, id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryEvidenceRead{}
	for rows.Next() {
		var read HistoryEvidenceRead
		var argumentsJSON string
		var startedAt, completedAt sql.NullString
		if err := rows.Scan(
			&read.ID, &read.RequestID, &read.EvidenceRound, &read.Order, &read.Capability, &argumentsJSON,
			&read.SafeSummary, &read.Status, &read.Availability, &startedAt, &completedAt,
		); err != nil {
			return nil, err
		}
		read.Arguments = map[string]any{}
		_ = json.Unmarshal([]byte(argumentsJSON), &read.Arguments)
		if startedAt.Valid {
			value := decodeDBTime(startedAt.String)
			read.StartedAt = &value
		}
		if completedAt.Valid {
			value := decodeDBTime(completedAt.String)
			read.CompletedAt = &value
		}
		out = append(out, read)
	}
	return out, rows.Err()
}

func (s *chatStore) historyLinks(id string) ([]HistoryLink, bool, error) {
	rows, err := s.db.Query(
		`SELECT from_entry_id, to_entry_id, relation, created_at FROM history_links
		 WHERE from_entry_id = ? OR to_entry_id = ?
		 ORDER BY created_at ASC, from_entry_id ASC, to_entry_id ASC, relation ASC
		 LIMIT ?`, id, id, historyMaximumLinks+1,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []HistoryLink{}
	for rows.Next() {
		var link HistoryLink
		var createdAt string
		if err := rows.Scan(&link.FromEntryID, &link.ToEntryID, &link.Relation, &createdAt); err != nil {
			return nil, false, err
		}
		link.CreatedAt = decodeDBTime(createdAt)
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(out) > historyMaximumLinks
	if truncated {
		out = out[:historyMaximumLinks]
	}
	return out, truncated, nil
}
