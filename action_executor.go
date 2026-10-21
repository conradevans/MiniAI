package main

import (
	"context"
	"errors"
	"fmt"

	actiondomain "miniai/internal/actiondomain"
)

type MiniDeployRestarter interface {
	RestartApplication(context.Context, string) error
}

type RestartApplicationExecutor struct {
	store     *chatStore
	restarter MiniDeployRestarter
}

type RestartApplicationExecutionOutcome string

const (
	RestartApplicationExecutionSucceeded      RestartApplicationExecutionOutcome = "succeeded"
	RestartApplicationExecutionFailed         RestartApplicationExecutionOutcome = "failed"
	RestartApplicationExecutionOutcomeUnknown RestartApplicationExecutionOutcome = "outcome_unknown"
)

type RestartApplicationExecutionResult struct {
	Action          storedActionProposal
	Outcome         RestartApplicationExecutionOutcome
	FailureCategory ActionExecutionFailureCategory
}

var (
	errRestartApplicationExecutionNotAllowed = errors.New("restart application execution not allowed")
	errRestartApplicationExecutionFailed     = errors.New("restart application execution failed")
	errRestartApplicationOutcomeUnknown      = errors.New("restart application outcome unknown")
)

func newRestartApplicationExecutor(
	store *chatStore,
	restarter MiniDeployRestarter,
) RestartApplicationExecutor {
	return RestartApplicationExecutor{store: store, restarter: restarter}
}

func (executor RestartApplicationExecutor) Execute(
	ctx context.Context,
	proposal ActionProposal,
) (RestartApplicationExecutionResult, error) {
	if ctx == nil || executor.store == nil || executor.restarter == nil {
		return RestartApplicationExecutionResult{}, fmt.Errorf(
			"%w: executor is unavailable",
			errRestartApplicationExecutionNotAllowed,
		)
	}
	if err := actiondomain.ValidateRestartExecutionFoundation(proposal); err != nil ||
		proposal.State() != ActionStateAuthorized {

		return RestartApplicationExecutionResult{}, fmt.Errorf(
			"%w: proposal is not an authorized restart action",
			errRestartApplicationExecutionNotAllowed,
		)
	}

	executing, err := executor.store.beginRestartApplicationExecution(proposal)
	if err != nil {
		return RestartApplicationExecutionResult{}, fmt.Errorf(
			"%w: %w",
			errRestartApplicationExecutionNotAllowed,
			err,
		)
	}

	// There is deliberately no retry after the durable executing transition.
	// A crash or ambiguous transport result leaves the action executing for a
	// later reconciliation phase, avoiding an accidental second restart.
	restartErr := executor.restarter.RestartApplication(
		ctx,
		executing.Proposal.Target().ID,
	)
	if restartErr == nil {
		succeeded, err := executor.store.completeRestartApplicationSuccess(
			executing.Proposal.ID(),
		)
		if err != nil {
			return RestartApplicationExecutionResult{
				Action:  executing,
				Outcome: RestartApplicationExecutionOutcomeUnknown,
			}, err
		}
		return RestartApplicationExecutionResult{
			Action:  succeeded,
			Outcome: RestartApplicationExecutionSucceeded,
		}, nil
	}

	failure := classifyMiniDeployRestartFailure(restartErr)
	if !failure.outcomeKnown {
		return RestartApplicationExecutionResult{
			Action:          executing,
			Outcome:         RestartApplicationExecutionOutcomeUnknown,
			FailureCategory: failure.category,
		}, fmt.Errorf(
			"%w: %s",
			errRestartApplicationOutcomeUnknown,
			failure.category,
		)
	}
	failed, err := executor.store.completeRestartApplicationFailure(
		executing.Proposal.ID(),
		failure.category,
	)
	if err != nil {
		return RestartApplicationExecutionResult{
			Action:          executing,
			Outcome:         RestartApplicationExecutionOutcomeUnknown,
			FailureCategory: failure.category,
		}, err
	}
	return RestartApplicationExecutionResult{
		Action:          failed,
		Outcome:         RestartApplicationExecutionFailed,
		FailureCategory: failure.category,
	}, fmt.Errorf(
		"%w: %s",
		errRestartApplicationExecutionFailed,
		failure.category,
	)
}
