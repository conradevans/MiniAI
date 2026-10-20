package main

import actiondomain "miniai/internal/actiondomain"

type ActionID = actiondomain.ActionID

type ActionKind = actiondomain.ActionKind

const (
	ActionKindRestartApplication = actiondomain.ActionKindRestartApplication
	ActionKindRollbackDeployment = actiondomain.ActionKindRollbackDeployment
	ActionKindDeployRepository   = actiondomain.ActionKindDeployRepository
)

type ActionTargetType = actiondomain.ActionTargetType

const (
	ActionTargetApplication = actiondomain.ActionTargetApplication
	ActionTargetRepository  = actiondomain.ActionTargetRepository
)

type ActionTarget = actiondomain.ActionTarget

type ActionState = actiondomain.ActionState

const (
	ActionStateProposed             = actiondomain.ActionStateProposed
	ActionStateAwaitingConfirmation = actiondomain.ActionStateAwaitingConfirmation
	ActionStateAuthorized           = actiondomain.ActionStateAuthorized
	ActionStateExecuting            = actiondomain.ActionStateExecuting
	ActionStateSucceeded            = actiondomain.ActionStateSucceeded
	ActionStateFailed               = actiondomain.ActionStateFailed
	ActionStateCancelled            = actiondomain.ActionStateCancelled
)

type RepositoryProvider = actiondomain.RepositoryProvider

const RepositoryProviderGitHub = actiondomain.RepositoryProviderGitHub

type ActionArguments = actiondomain.ActionArguments

type RestartApplicationArguments = actiondomain.RestartApplicationArguments

type RollbackDeploymentArguments = actiondomain.RollbackDeploymentArguments

type DeployRepositoryArguments = actiondomain.DeployRepositoryArguments

type ActionProposal = actiondomain.Proposal

type ActionProposalInput = actiondomain.ProposalInput

type ActionDefinition = actiondomain.ActionDefinition

// actionCatalog intentionally has no fields. All security metadata is resolved
// by the closed lookup in internal/actiondomain for every operation.
type actionCatalog struct{}

var (
	errUnknownActionKind            = actiondomain.ErrUnknownActionKind
	errInvalidActionID              = actiondomain.ErrInvalidActionID
	errInvalidActionTarget          = actiondomain.ErrInvalidActionTarget
	errInvalidActionArguments       = actiondomain.ErrInvalidActionArguments
	errInvalidActionCreatedAt       = actiondomain.ErrInvalidActionCreatedAt
	errInvalidActionProposal        = actiondomain.ErrInvalidActionProposal
	errInvalidActionState           = actiondomain.ErrInvalidActionState
	errInvalidActionStateTransition = actiondomain.ErrInvalidActionStateTransition
)

func phase4ActionCatalog() actionCatalog {
	return actionCatalog{}
}

func (actionCatalog) definitionsCopy() []ActionDefinition {
	return actiondomain.Definitions()
}

func (actionCatalog) lookup(kind ActionKind) (ActionDefinition, bool) {
	return actiondomain.LookupDefinition(kind)
}

func (actionCatalog) propose(input ActionProposalInput) (ActionProposal, error) {
	return actiondomain.NewProposal(input)
}

func (actionCatalog) evaluate(
	proposal ActionProposal,
) (ActionAuthorizationDecision, error) {
	return actiondomain.EvaluateProposal(proposal)
}

func validateActionStateTransition(from, to ActionState) error {
	return actiondomain.ValidateStateTransition(from, to)
}

func transitionActionProposal(
	proposal ActionProposal,
	to ActionState,
) (ActionProposal, error) {
	return actiondomain.TransitionProposal(proposal, to)
}
