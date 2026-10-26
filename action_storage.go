package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	actiondomain "miniai/internal/actiondomain"
)

const actionArgumentsMaxBytes = 4096

type ActionEventType = actiondomain.RestorationEventType

const (
	ActionEventProposalCreated            = actiondomain.RestorationEventProposalCreated
	ActionEventPolicyAuthorizedOnCreation = actiondomain.RestorationEventPolicyAuthorizedOnCreation
	ActionEventConfirmationGranted        = actiondomain.RestorationEventConfirmationGranted
	ActionEventCancelled                  = actiondomain.RestorationEventCancelled
	ActionEventExecutionStarted           = actiondomain.RestorationEventExecutionStarted
	ActionEventExecutionSucceeded         = actiondomain.RestorationEventExecutionSucceeded
	ActionEventExecutionFailed            = actiondomain.RestorationEventExecutionFailed
)

type actionProposalCreateInput struct {
	Kind           ActionKind
	Origin         ActionOrigin
	Target         ActionTarget
	Arguments      ActionArguments
	HistoryEntryID string
	CreatedAt      time.Time
}

type storedActionProposal struct {
	Proposal          ActionProposal
	HistoryEntryID    string
	SupportsExecution bool
	UpdatedAt         time.Time
}

type ActionEvent struct {
	ID              string
	ActionID        ActionID
	Ordinal         int
	Type            ActionEventType
	FromState       *ActionState
	ToState         ActionState
	FailureCategory ActionExecutionFailureCategory
	CreatedAt       time.Time
}

var (
	errActionNotFound               = errors.New("action proposal not found")
	errInvalidPersistedAction       = errors.New("invalid persisted action proposal")
	errActionConfirmationNotAllowed = errors.New("action confirmation not allowed")
	errActionCancellationNotAllowed = errors.New("action cancellation not allowed")
	errActionPersistenceConflict    = errors.New("action persistence conflict")
	errActionHistoryLink            = errors.New("invalid action History linkage")
	actionEventIDPattern            = regexp.MustCompile(`^action_event_[0-9a-f]{32}$`)
)

const actionProposalColumns = `action_id, action_kind, origin, target_type, target_id,
	arguments_json, safety_class, confirmation_required, warning_required,
	supports_execution, policy_reason, current_state, history_entry_id, created_at, updated_at`

type actionQueryer interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func (s *chatStore) createActionProposal(
	input actionProposalCreateInput,
) (storedActionProposal, error) {
	idValue, err := randomID(16)
	if err != nil {
		return storedActionProposal{}, err
	}
	if input.CreatedAt.IsZero() {
		input.CreatedAt = time.Now().UTC()
	}
	input.CreatedAt = input.CreatedAt.UTC()
	if input.HistoryEntryID != strings.TrimSpace(input.HistoryEntryID) {
		return storedActionProposal{}, errActionHistoryLink
	}
	proposal, err := phase4ActionCatalog().propose(ActionProposalInput{
		ID:        ActionID("action_" + idValue),
		Kind:      input.Kind,
		Origin:    input.Origin,
		Target:    input.Target,
		Arguments: input.Arguments,
		CreatedAt: input.CreatedAt,
	})
	if err != nil {
		return storedActionProposal{}, err
	}
	definition, ok := phase4ActionCatalog().lookup(proposal.Kind())
	if !ok {
		return storedActionProposal{}, errUnknownActionKind
	}
	decision, err := phase4ActionCatalog().evaluate(proposal)
	if err != nil {
		return storedActionProposal{}, err
	}
	argumentsJSON, err := encodeActionArguments(proposal.Kind(), proposal.Arguments())
	if err != nil {
		return storedActionProposal{}, err
	}
	eventType := ActionEventProposalCreated
	switch proposal.State() {
	case ActionStateAwaitingConfirmation:
	case ActionStateAuthorized:
		eventType = ActionEventPolicyAuthorizedOnCreation
	default:
		return storedActionProposal{}, fmt.Errorf(
			"%w: unsupported initial state %q",
			errInvalidPersistedAction,
			proposal.State(),
		)
	}
	eventID, err := newActionEventID()
	if err != nil {
		return storedActionProposal{}, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return storedActionProposal{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var historyValue any
	if input.HistoryEntryID != "" {
		var one int
		err := tx.QueryRow(
			`SELECT 1 FROM history_entries WHERE id = ?`,
			input.HistoryEntryID,
		).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return storedActionProposal{}, errActionHistoryLink
		}
		if err != nil {
			return storedActionProposal{}, err
		}
		historyValue = input.HistoryEntryID
	}
	if _, err := tx.Exec(
		`INSERT INTO action_proposals(
			action_id, action_kind, origin, target_type, target_id, arguments_json,
			safety_class, confirmation_required, warning_required, supports_execution,
			policy_reason, current_state, history_entry_id, created_at, updated_at
		 ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		proposal.ID(), proposal.Kind(), proposal.Origin(), proposal.Target().Type,
		proposal.Target().ID, argumentsJSON, definition.SafetyClass,
		decision.ConfirmationRequired, decision.WarningRequired,
		definition.SupportsExecution, decision.Reason, proposal.State(), historyValue,
		encodeDBTime(input.CreatedAt), encodeDBTime(input.CreatedAt),
	); err != nil {
		return storedActionProposal{}, err
	}
	if _, err := tx.Exec(
		`INSERT INTO action_events(
			event_id, action_id, ordinal, event_type, from_state, to_state, created_at
		 ) VALUES(?,?,?,?,NULL,?,?)`,
		eventID, proposal.ID(), 0, eventType, proposal.State(),
		encodeDBTime(input.CreatedAt),
	); err != nil {
		return storedActionProposal{}, err
	}
	if err := tx.Commit(); err != nil {
		return storedActionProposal{}, err
	}
	return storedActionProposal{
		Proposal:          proposal,
		HistoryEntryID:    input.HistoryEntryID,
		SupportsExecution: definition.SupportsExecution,
		UpdatedAt:         input.CreatedAt,
	}, nil
}

// createAssistantRestartSuggestion is idempotent only within one trusted History request.
// The store mutex serializes same-process response retries; the durable History lookup
// preserves the same identity after a process reopen without suppressing later investigations.
func (s *chatStore) createAssistantRestartSuggestion(
	historyEntryID string,
	target ActionTarget,
) (storedActionProposal, error) {
	if s == nil || s.db == nil || historyEntryID == "" ||
		historyEntryID != strings.TrimSpace(historyEntryID) ||
		target.Type != ActionTargetApplication ||
		!validActionApplicationTargetID(target.ID) {

		return storedActionProposal{}, errActionHistoryLink
	}
	s.assistantSuggestionMu.Lock()
	defer s.assistantSuggestionMu.Unlock()

	rows, err := s.db.Query(
		`SELECT action_id FROM action_proposals
		 WHERE history_entry_id = ? AND action_kind = ? AND origin = ?
		 ORDER BY created_at ASC, action_id ASC`,
		historyEntryID,
		ActionKindRestartApplication,
		ActionOriginAssistantSuggested,
	)
	if err != nil {
		return storedActionProposal{}, err
	}
	ids := []ActionID{}
	for rows.Next() {
		var id ActionID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return storedActionProposal{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return storedActionProposal{}, err
	}
	if err := rows.Close(); err != nil {
		return storedActionProposal{}, err
	}
	if len(ids) > 1 {
		return storedActionProposal{}, errActionPersistenceConflict
	}
	if len(ids) == 1 {
		record, err := s.getActionProposal(ids[0])
		if err != nil {
			return storedActionProposal{}, err
		}
		proposalTarget := record.Proposal.Target()
		if record.HistoryEntryID != historyEntryID ||
			record.Proposal.Kind() != ActionKindRestartApplication ||
			record.Proposal.Origin() != ActionOriginAssistantSuggested ||
			proposalTarget != target {

			return storedActionProposal{}, errActionPersistenceConflict
		}
		if _, ok := record.Proposal.Arguments().(RestartApplicationArguments); !ok {
			return storedActionProposal{}, errActionPersistenceConflict
		}
		return record, nil
	}

	return s.createActionProposal(actionProposalCreateInput{
		Kind: ActionKindRestartApplication, Origin: ActionOriginAssistantSuggested,
		Target: target, Arguments: RestartApplicationArguments{},
		HistoryEntryID: historyEntryID,
	})
}

func (s *chatStore) getActionProposal(
	id ActionID,
) (storedActionProposal, error) {
	record, _, err := loadActionProposal(s.db, id)
	return record, err
}

func (s *chatStore) listActionEvents(id ActionID) ([]ActionEvent, error) {
	_, events, err := loadActionProposal(s.db, id)
	return events, err
}

func (s *chatStore) confirmAction(id ActionID) (storedActionProposal, error) {
	eventID, err := newActionEventID()
	if err != nil {
		return storedActionProposal{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return storedActionProposal{}, err
	}
	defer func() { _ = tx.Rollback() }()
	record, events, err := loadActionProposal(tx, id)
	if err != nil {
		return storedActionProposal{}, err
	}
	if record.Proposal.State() != ActionStateAwaitingConfirmation {
		return storedActionProposal{}, fmt.Errorf(
			"%w: state is %q",
			errActionConfirmationNotAllowed,
			record.Proposal.State(),
		)
	}
	transitioned, err := transitionActionProposal(
		record.Proposal,
		ActionStateAuthorized,
	)
	if err != nil {
		return storedActionProposal{}, fmt.Errorf(
			"%w: %v",
			errActionConfirmationNotAllowed,
			err,
		)
	}
	now := time.Now().UTC()
	if now.Before(record.UpdatedAt) {
		now = record.UpdatedAt
	}
	result, err := tx.Exec(
		`UPDATE action_proposals SET current_state = ?, updated_at = ?
		 WHERE action_id = ? AND current_state = ?`,
		ActionStateAuthorized, encodeDBTime(now), id,
		ActionStateAwaitingConfirmation,
	)
	if err != nil {
		return storedActionProposal{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return storedActionProposal{}, err
	}
	if affected != 1 {
		return storedActionProposal{}, errActionPersistenceConflict
	}
	if _, err := tx.Exec(
		`INSERT INTO action_events(
			event_id, action_id, ordinal, event_type, from_state, to_state, created_at
		 ) VALUES(?,?,?,?,?,?,?)`,
		eventID, id, len(events), ActionEventConfirmationGranted,
		ActionStateAwaitingConfirmation, ActionStateAuthorized, encodeDBTime(now),
	); err != nil {
		return storedActionProposal{}, fmt.Errorf(
			"%w: append confirmation event: %v",
			errActionPersistenceConflict,
			err,
		)
	}
	if err := tx.Commit(); err != nil {
		return storedActionProposal{}, err
	}
	record.Proposal = transitioned
	record.UpdatedAt = now
	return record, nil
}

func (s *chatStore) cancelAction(id ActionID) (storedActionProposal, error) {
	eventID, err := newActionEventID()
	if err != nil {
		return storedActionProposal{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return storedActionProposal{}, err
	}
	defer func() { _ = tx.Rollback() }()
	record, events, err := loadActionProposal(tx, id)
	if err != nil {
		return storedActionProposal{}, err
	}
	if record.Proposal.State() != ActionStateAwaitingConfirmation {
		return storedActionProposal{}, fmt.Errorf(
			"%w: state is %q",
			errActionCancellationNotAllowed,
			record.Proposal.State(),
		)
	}
	transitioned, err := transitionActionProposal(
		record.Proposal,
		ActionStateCancelled,
	)
	if err != nil {
		return storedActionProposal{}, fmt.Errorf(
			"%w: %v",
			errActionCancellationNotAllowed,
			err,
		)
	}
	now := time.Now().UTC()
	if now.Before(record.UpdatedAt) {
		now = record.UpdatedAt
	}
	result, err := tx.Exec(
		`UPDATE action_proposals SET current_state = ?, updated_at = ?
		 WHERE action_id = ? AND current_state = ?`,
		ActionStateCancelled, encodeDBTime(now), id,
		ActionStateAwaitingConfirmation,
	)
	if err != nil {
		return storedActionProposal{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return storedActionProposal{}, err
	}
	if affected != 1 {
		return storedActionProposal{}, errActionPersistenceConflict
	}
	if _, err := tx.Exec(
		`INSERT INTO action_events(
			event_id, action_id, ordinal, event_type, from_state, to_state, created_at
		 ) VALUES(?,?,?,?,?,?,?)`,
		eventID, id, len(events), ActionEventCancelled,
		ActionStateAwaitingConfirmation, ActionStateCancelled, encodeDBTime(now),
	); err != nil {
		return storedActionProposal{}, fmt.Errorf(
			"%w: append cancellation event: %v",
			errActionPersistenceConflict,
			err,
		)
	}
	if err := tx.Commit(); err != nil {
		return storedActionProposal{}, err
	}
	record.Proposal = transitioned
	record.UpdatedAt = now
	return record, nil
}

func (s *chatStore) historyActionProposalIDs(
	historyEntryID string,
) ([]ActionID, error) {
	var one int
	err := s.db.QueryRow(
		`SELECT 1 FROM history_entries WHERE id = ?`,
		historyEntryID,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errActionHistoryLink
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(
		`SELECT action_id FROM action_proposals
		 WHERE history_entry_id = ? ORDER BY created_at ASC, action_id ASC`,
		historyEntryID,
	)
	if err != nil {
		return nil, err
	}
	ids := []ActionID{}
	for rows.Next() {
		var id ActionID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := s.getActionProposal(id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func loadActionProposal(
	queryer actionQueryer,
	id ActionID,
) (storedActionProposal, []ActionEvent, error) {
	var actionID ActionID
	var kind, origin, targetType, targetID, argumentsJSON string
	var safetyClass, policyReason, currentState string
	var confirmationRequired, warningRequired, supportsExecution bool
	var historyEntryID sql.NullString
	var createdAtText, updatedAtText string
	err := queryer.QueryRow(
		`SELECT `+actionProposalColumns+` FROM action_proposals WHERE action_id = ?`,
		id,
	).Scan(
		&actionID, &kind, &origin, &targetType, &targetID, &argumentsJSON,
		&safetyClass, &confirmationRequired, &warningRequired,
		&supportsExecution, &policyReason, &currentState, &historyEntryID,
		&createdAtText, &updatedAtText,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return storedActionProposal{}, nil, errActionNotFound
	}
	if err != nil {
		return storedActionProposal{}, nil, err
	}
	createdAt, err := decodeCanonicalActionTime(createdAtText)
	if err != nil {
		return storedActionProposal{}, nil, invalidPersistedAction(err)
	}
	updatedAt, err := decodeCanonicalActionTime(updatedAtText)
	if err != nil || updatedAt.Before(createdAt) {
		if err == nil {
			err = errors.New("updated timestamp precedes creation")
		}
		return storedActionProposal{}, nil, invalidPersistedAction(err)
	}
	definition, ok := phase4ActionCatalog().lookup(ActionKind(kind))
	if !ok {
		return storedActionProposal{}, nil, invalidPersistedAction(errUnknownActionKind)
	}
	if ActionSafetyClass(safetyClass) != definition.SafetyClass ||
		ActionTargetType(targetType) != definition.TargetType ||
		!validStoredActionExecutionPolicy(ActionKind(kind), supportsExecution, definition) {

		return storedActionProposal{}, nil, invalidPersistedAction(
			errors.New("stored catalog metadata does not match authoritative definition"),
		)
	}
	arguments, err := decodeActionArguments(ActionKind(kind), argumentsJSON)
	if err != nil {
		return storedActionProposal{}, nil, invalidPersistedAction(err)
	}
	if historyEntryID.Valid {
		var one int
		err := queryer.QueryRow(
			`SELECT 1 FROM history_entries WHERE id = ?`,
			historyEntryID.String,
		).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return storedActionProposal{}, nil, invalidPersistedAction(errActionHistoryLink)
		}
		if err != nil {
			return storedActionProposal{}, nil, err
		}
	}
	events, err := loadActionEvents(queryer, actionID)
	if err != nil {
		return storedActionProposal{}, nil, err
	}
	if len(events) == 0 || !events[0].CreatedAt.Equal(createdAt) ||
		!events[len(events)-1].CreatedAt.Equal(updatedAt) {

		return storedActionProposal{}, nil, invalidPersistedAction(
			errors.New("stored timestamps do not match action audit events"),
		)
	}
	restorationEvents := make([]actiondomain.RestorationEvent, 0, len(events))
	for _, event := range events {
		var fromState ActionState
		if event.FromState != nil {
			fromState = *event.FromState
		}
		restorationEvents = append(restorationEvents, actiondomain.RestorationEvent{
			Type:            event.Type,
			FromState:       fromState,
			ToState:         event.ToState,
			FailureCategory: event.FailureCategory,
		})
	}
	proposal, err := actiondomain.RestoreProposal(actiondomain.ProposalInput{
		ID:        actionID,
		Kind:      ActionKind(kind),
		Origin:    ActionOrigin(origin),
		Target:    ActionTarget{Type: ActionTargetType(targetType), ID: targetID},
		Arguments: arguments,
		CreatedAt: createdAt,
	}, ActionState(currentState), restorationEvents)
	if err != nil {
		return storedActionProposal{}, nil, invalidPersistedAction(err)
	}
	decision, err := phase4ActionCatalog().evaluate(proposal)
	if err != nil {
		return storedActionProposal{}, nil, invalidPersistedAction(err)
	}
	if confirmationRequired != decision.ConfirmationRequired ||
		warningRequired != decision.WarningRequired ||
		ActionPolicyReason(policyReason) != decision.Reason {

		return storedActionProposal{}, nil, invalidPersistedAction(
			errors.New("stored policy metadata does not match authoritative policy"),
		)
	}
	return storedActionProposal{
		Proposal:          proposal,
		HistoryEntryID:    historyEntryID.String,
		SupportsExecution: supportsExecution,
		UpdatedAt:         updatedAt,
	}, events, nil
}

// validStoredActionExecutionPolicy keeps pre-4B2 restart proposals readable
// without granting them the execution authority added in 4B2. A persisted
// true value can never override an authoritative catalog denial.
func validStoredActionExecutionPolicy(
	kind ActionKind,
	stored bool,
	definition ActionDefinition,
) bool {
	if stored {
		return definition.SupportsExecution
	}
	return !definition.SupportsExecution ||
		(kind == ActionKindRestartApplication && definition.SupportsExecution)
}

func loadActionEvents(
	queryer actionQueryer,
	actionID ActionID,
) ([]ActionEvent, error) {
	rows, err := queryer.Query(
		`SELECT event_id, ordinal, event_type, from_state, to_state, failure_category, created_at
		 FROM action_events WHERE action_id = ? ORDER BY ordinal ASC`,
		actionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []ActionEvent{}
	var previous time.Time
	for rows.Next() {
		var event ActionEvent
		var eventType, toState, createdAtText string
		var failureCategory sql.NullString
		var fromState sql.NullString
		if err := rows.Scan(
			&event.ID,
			&event.Ordinal,
			&eventType,
			&fromState,
			&toState,
			&failureCategory,
			&createdAtText,
		); err != nil {
			return nil, err
		}
		if !actionEventIDPattern.MatchString(event.ID) || event.Ordinal != len(events) {
			return nil, invalidPersistedAction(
				errors.New("invalid action event identity or ordinal"),
			)
		}
		event.ActionID = actionID
		event.Type = ActionEventType(eventType)
		if failureCategory.Valid {
			event.FailureCategory = ActionExecutionFailureCategory(failureCategory.String)
		}
		event.ToState = ActionState(toState)
		if fromState.Valid {
			value := ActionState(fromState.String)
			event.FromState = &value
		}
		event.CreatedAt, err = decodeCanonicalActionTime(createdAtText)
		if err != nil || (!previous.IsZero() && event.CreatedAt.Before(previous)) {
			if err == nil {
				err = errors.New("action event timestamps are out of order")
			}
			return nil, invalidPersistedAction(err)
		}
		previous = event.CreatedAt
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func encodeActionArguments(
	kind ActionKind,
	arguments ActionArguments,
) (string, error) {
	var value any
	switch kind {
	case ActionKindRestartApplication:
		typed, ok := arguments.(RestartApplicationArguments)
		if !ok {
			return "", errInvalidActionArguments
		}
		value = typed
	case ActionKindRollbackDeployment:
		typed, ok := arguments.(RollbackDeploymentArguments)
		if !ok {
			return "", errInvalidActionArguments
		}
		value = typed
	case ActionKindDeployRepository:
		typed, ok := arguments.(DeployRepositoryArguments)
		if !ok {
			return "", errInvalidActionArguments
		}
		value = typed
	default:
		return "", errUnknownActionKind
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(encoded) > actionArgumentsMaxBytes {
		return "", errInvalidActionArguments
	}
	return string(encoded), nil
}

func decodeActionArguments(
	kind ActionKind,
	encoded string,
) (ActionArguments, error) {
	if encoded == "" || len(encoded) > actionArgumentsMaxBytes {
		return nil, errInvalidActionArguments
	}
	var pointer any
	switch kind {
	case ActionKindRestartApplication:
		pointer = &RestartApplicationArguments{}
	case ActionKindRollbackDeployment:
		pointer = &RollbackDeploymentArguments{}
	case ActionKindDeployRepository:
		pointer = &DeployRepositoryArguments{}
	default:
		return nil, errUnknownActionKind
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(pointer); err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidActionArguments, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidActionArguments, err)
	}
	var value ActionArguments
	switch typed := pointer.(type) {
	case *RestartApplicationArguments:
		value = *typed
	case *RollbackDeploymentArguments:
		value = *typed
	case *DeployRepositoryArguments:
		value = *typed
	default:
		return nil, errInvalidActionArguments
	}
	canonical, err := encodeActionArguments(kind, value)
	if err != nil || canonical != encoded {
		return nil, errInvalidActionArguments
	}
	return value, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func decodeCanonicalActionTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.IsZero() {
		return time.Time{}, errors.New("invalid action timestamp")
	}
	parsed = parsed.UTC()
	if encodeDBTime(parsed) != value {
		return time.Time{}, errors.New("non-canonical action timestamp")
	}
	return parsed, nil
}

func invalidPersistedAction(err error) error {
	return fmt.Errorf("%w: %w", errInvalidPersistedAction, err)
}

func newActionEventID() (string, error) {
	value, err := randomID(16)
	if err != nil {
		return "", err
	}
	return "action_event_" + value, nil
}
