package main

import (
	"errors"
	"fmt"
	"time"

	actiondomain "miniai/internal/actiondomain"
)

var (
	errActionExecutionNotAllowed           = errors.New("restart action execution not allowed")
	errActionExecutionCompletionNotAllowed = errors.New("restart action execution completion not allowed")
)

func (s *chatStore) beginRestartApplicationExecution(
	proposal ActionProposal,
) (storedActionProposal, error) {
	if err := actiondomain.ValidateRestartExecutionFoundation(proposal); err != nil ||
		proposal.State() != ActionStateAuthorized {

		return storedActionProposal{}, fmt.Errorf(
			"%w: proposal is not an authorized restart action",
			errActionExecutionNotAllowed,
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
	record, events, err := loadActionProposal(tx, proposal.ID())
	if err != nil {
		return storedActionProposal{}, err
	}
	if err := actiondomain.ValidateRestartExecutionFoundation(record.Proposal); err != nil ||
		!record.SupportsExecution ||
		record.Proposal.State() != ActionStateAuthorized ||
		!sameRestartExecutionProposal(proposal, record.Proposal) {

		return storedActionProposal{}, fmt.Errorf(
			"%w: stored proposal is not the authorized action supplied",
			errActionExecutionNotAllowed,
		)
	}
	transitioned, err := transitionActionProposal(record.Proposal, ActionStateExecuting)
	if err != nil {
		return storedActionProposal{}, fmt.Errorf("%w: %v", errActionExecutionNotAllowed, err)
	}
	now := actionExecutionTimestamp(record.UpdatedAt)
	result, err := tx.Exec(
		`UPDATE action_proposals SET current_state = ?, updated_at = ?
		 WHERE action_id = ? AND current_state = ?`,
		ActionStateExecuting, encodeDBTime(now), proposal.ID(), ActionStateAuthorized,
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
		eventID, proposal.ID(), len(events), ActionEventExecutionStarted,
		ActionStateAuthorized, ActionStateExecuting, encodeDBTime(now),
	); err != nil {
		return storedActionProposal{}, fmt.Errorf(
			"%w: append execution-started event",
			errActionPersistenceConflict,
		)
	}
	if err := tx.Commit(); err != nil {
		return storedActionProposal{}, err
	}
	record.Proposal = transitioned
	record.UpdatedAt = now
	return record, nil
}

func (s *chatStore) completeRestartApplicationSuccess(
	id ActionID,
) (storedActionProposal, error) {
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
	if err := actiondomain.ValidateRestartExecutionFoundation(record.Proposal); err != nil ||
		!record.SupportsExecution ||
		record.Proposal.State() != ActionStateExecuting {

		return storedActionProposal{}, fmt.Errorf(
			"%w: action is not executing",
			errActionExecutionCompletionNotAllowed,
		)
	}
	transitioned, err := transitionActionProposal(record.Proposal, ActionStateSucceeded)
	if err != nil {
		return storedActionProposal{}, fmt.Errorf(
			"%w: %v",
			errActionExecutionCompletionNotAllowed,
			err,
		)
	}
	now := actionExecutionTimestamp(record.UpdatedAt)
	result, err := tx.Exec(
		`UPDATE action_proposals SET current_state = ?, updated_at = ?
		 WHERE action_id = ? AND current_state = ?`,
		ActionStateSucceeded, encodeDBTime(now), id, ActionStateExecuting,
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
		eventID, id, len(events), ActionEventExecutionSucceeded,
		ActionStateExecuting, ActionStateSucceeded, encodeDBTime(now),
	); err != nil {
		return storedActionProposal{}, fmt.Errorf(
			"%w: append execution-succeeded event",
			errActionPersistenceConflict,
		)
	}
	if err := tx.Commit(); err != nil {
		return storedActionProposal{}, err
	}
	record.Proposal = transitioned
	record.UpdatedAt = now
	return record, nil
}

func (s *chatStore) completeRestartApplicationFailure(
	id ActionID,
	category ActionExecutionFailureCategory,
) (storedActionProposal, error) {
	if !actiondomain.ValidExecutionFailureCategory(category) {
		return storedActionProposal{}, fmt.Errorf(
			"%w: invalid safe failure category",
			errActionExecutionCompletionNotAllowed,
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
	record, events, err := loadActionProposal(tx, id)
	if err != nil {
		return storedActionProposal{}, err
	}
	if err := actiondomain.ValidateRestartExecutionFoundation(record.Proposal); err != nil ||
		!record.SupportsExecution ||
		record.Proposal.State() != ActionStateExecuting {

		return storedActionProposal{}, fmt.Errorf(
			"%w: action is not executing",
			errActionExecutionCompletionNotAllowed,
		)
	}
	transitioned, err := transitionActionProposal(record.Proposal, ActionStateFailed)
	if err != nil {
		return storedActionProposal{}, fmt.Errorf(
			"%w: %v",
			errActionExecutionCompletionNotAllowed,
			err,
		)
	}
	now := actionExecutionTimestamp(record.UpdatedAt)
	result, err := tx.Exec(
		`UPDATE action_proposals SET current_state = ?, updated_at = ?
		 WHERE action_id = ? AND current_state = ?`,
		ActionStateFailed, encodeDBTime(now), id, ActionStateExecuting,
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
			event_id, action_id, ordinal, event_type, from_state, to_state,
			failure_category, created_at
		 ) VALUES(?,?,?,?,?,?,?,?)`,
		eventID, id, len(events), ActionEventExecutionFailed,
		ActionStateExecuting, ActionStateFailed, category, encodeDBTime(now),
	); err != nil {
		return storedActionProposal{}, fmt.Errorf(
			"%w: append execution-failed event",
			errActionPersistenceConflict,
		)
	}
	if err := tx.Commit(); err != nil {
		return storedActionProposal{}, err
	}
	record.Proposal = transitioned
	record.UpdatedAt = now
	return record, nil
}

func sameRestartExecutionProposal(left, right ActionProposal) bool {
	if left.ID() != right.ID() || left.Kind() != right.Kind() ||
		left.Origin() != right.Origin() || left.Target() != right.Target() ||
		left.State() != right.State() || !left.CreatedAt().Equal(right.CreatedAt()) {

		return false
	}
	_, leftOK := left.Arguments().(RestartApplicationArguments)
	_, rightOK := right.Arguments().(RestartApplicationArguments)
	return leftOK && rightOK
}

func actionExecutionTimestamp(previous time.Time) time.Time {
	now := time.Now().UTC()
	if now.Before(previous) {
		return previous
	}
	return now
}
