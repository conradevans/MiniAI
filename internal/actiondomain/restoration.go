package actiondomain

import "fmt"

type RestorationEventType string

const (
	RestorationEventProposalCreated            RestorationEventType = "proposal_created"
	RestorationEventPolicyAuthorizedOnCreation RestorationEventType = "policy_authorized_on_creation"
	RestorationEventConfirmationGranted        RestorationEventType = "confirmation_granted"
	RestorationEventCancelled                  RestorationEventType = "cancelled"
	RestorationEventExecutionStarted           RestorationEventType = "execution_started"
	RestorationEventExecutionSucceeded         RestorationEventType = "execution_succeeded"
	RestorationEventExecutionFailed            RestorationEventType = "execution_failed"
)

type ExecutionFailureCategory string

const (
	ExecutionFailureUpstreamUnavailable ExecutionFailureCategory = "upstream_unavailable"
	ExecutionFailureUpstreamRejected    ExecutionFailureCategory = "upstream_rejected"
	ExecutionFailureTimeout             ExecutionFailureCategory = "timeout"
	ExecutionFailureInvalidResponse     ExecutionFailureCategory = "invalid_response"
	ExecutionFailureInternalError       ExecutionFailureCategory = "internal_error"
)

type RestorationEvent struct {
	Type            RestorationEventType
	FromState       ActionState
	ToState         ActionState
	FailureCategory ExecutionFailureCategory
}

var (
	ErrActionAuditMismatch        = fmt.Errorf("action audit trail does not match persisted state")
	ErrActionExecutionUnsupported = fmt.Errorf("persisted execution state is unsupported")
)

// RestoreProposal rebuilds an opaque proposal from durable data only after the
// current catalog, policy, lifecycle state, and complete audit transition chain
// agree. Phase 4B1 permits execution states only for restart_application and
// only when the complete execution event chain is valid.
func RestoreProposal(
	input ProposalInput,
	state ActionState,
	events []RestorationEvent,
) (Proposal, error) {
	definition, ok := LookupDefinition(input.Kind)
	if !ok {
		return Proposal{}, fmt.Errorf("%w: %q", ErrUnknownActionKind, input.Kind)
	}
	if !ValidActionOrigin(input.Origin) {
		return Proposal{}, fmt.Errorf("%w: %q", ErrInvalidActionOrigin, input.Origin)
	}
	if !validActionState(state) || state == ActionStateProposed {
		return Proposal{}, fmt.Errorf("%w: %q", ErrInvalidActionState, state)
	}
	if !supportsRestoredExecutionState(input.Kind) {
		switch state {
		case ActionStateExecuting, ActionStateSucceeded, ActionStateFailed:
			return Proposal{}, fmt.Errorf("%w: %q", ErrActionExecutionUnsupported, state)
		}
	}

	proposal := Proposal{
		id:        input.ID,
		kind:      input.Kind,
		origin:    input.Origin,
		target:    input.Target,
		arguments: input.Arguments,
		createdAt: input.CreatedAt.UTC(),
		state:     state,
	}
	if err := validateProposalIdentity(proposal, definition); err != nil {
		return Proposal{}, err
	}
	decision, err := EvaluateConfirmationPolicy(input.Origin, definition.SafetyClass)
	if err != nil {
		return Proposal{}, err
	}
	if err := validateRestorationEvents(decision.InitialState, state, events); err != nil {
		return Proposal{}, err
	}
	if _, err := EvaluateProposal(proposal); err != nil {
		return Proposal{}, err
	}
	return proposal, nil
}

func validateRestorationEvents(
	initialState ActionState,
	currentState ActionState,
	events []RestorationEvent,
) error {
	expected := make([]RestorationEvent, 0, 3)
	switch initialState {
	case ActionStateAuthorized:
		expected = append(expected, RestorationEvent{
			Type:    RestorationEventPolicyAuthorizedOnCreation,
			ToState: ActionStateAuthorized,
		})
	case ActionStateAwaitingConfirmation:
		expected = append(expected, RestorationEvent{
			Type:    RestorationEventProposalCreated,
			ToState: ActionStateAwaitingConfirmation,
		})
	default:
		return ErrActionAuditMismatch
	}

	if initialState == ActionStateAwaitingConfirmation {
		switch currentState {
		case ActionStateAwaitingConfirmation:
		case ActionStateCancelled:
			expected = append(expected, RestorationEvent{
				Type:      RestorationEventCancelled,
				FromState: ActionStateAwaitingConfirmation,
				ToState:   ActionStateCancelled,
			})
		default:
			expected = append(expected, RestorationEvent{
				Type:      RestorationEventConfirmationGranted,
				FromState: ActionStateAwaitingConfirmation,
				ToState:   ActionStateAuthorized,
			})
		}
	}

	switch currentState {
	case ActionStateAwaitingConfirmation:
		if initialState != ActionStateAwaitingConfirmation {
			return ErrActionAuditMismatch
		}
	case ActionStateCancelled:
		if initialState != ActionStateAwaitingConfirmation {
			return ErrActionAuditMismatch
		}
	case ActionStateAuthorized:
	case ActionStateExecuting, ActionStateSucceeded, ActionStateFailed:
		expected = append(expected, RestorationEvent{
			Type:      RestorationEventExecutionStarted,
			FromState: ActionStateAuthorized,
			ToState:   ActionStateExecuting,
		})
		if currentState == ActionStateSucceeded {
			expected = append(expected, RestorationEvent{
				Type:      RestorationEventExecutionSucceeded,
				FromState: ActionStateExecuting,
				ToState:   ActionStateSucceeded,
			})
		} else if currentState == ActionStateFailed {
			if len(events) != len(expected)+1 ||
				!ValidExecutionFailureCategory(events[len(expected)].FailureCategory) {

				return ErrActionAuditMismatch
			}
			expected = append(expected, RestorationEvent{
				Type:            RestorationEventExecutionFailed,
				FromState:       ActionStateExecuting,
				ToState:         ActionStateFailed,
				FailureCategory: events[len(expected)].FailureCategory,
			})
		}
	default:
		return ErrActionAuditMismatch
	}

	if len(events) != len(expected) {
		return ErrActionAuditMismatch
	}
	for index := range expected {
		if events[index] != expected[index] {
			return ErrActionAuditMismatch
		}
	}
	return nil
}

func ValidExecutionFailureCategory(category ExecutionFailureCategory) bool {
	switch category {
	case ExecutionFailureUpstreamUnavailable, ExecutionFailureUpstreamRejected,
		ExecutionFailureTimeout, ExecutionFailureInvalidResponse,
		ExecutionFailureInternalError:
		return true
	default:
		return false
	}
}
