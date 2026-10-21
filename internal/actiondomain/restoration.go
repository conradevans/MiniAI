package actiondomain

import "fmt"

type RestorationEventType string

const (
	RestorationEventProposalCreated            RestorationEventType = "proposal_created"
	RestorationEventPolicyAuthorizedOnCreation RestorationEventType = "policy_authorized_on_creation"
	RestorationEventConfirmationGranted        RestorationEventType = "confirmation_granted"
	RestorationEventCancelled                  RestorationEventType = "cancelled"
)

type RestorationEvent struct {
	Type      RestorationEventType
	FromState ActionState
	ToState   ActionState
}

var (
	ErrActionAuditMismatch        = fmt.Errorf("action audit trail does not match persisted state")
	ErrActionExecutionUnsupported = fmt.Errorf("persisted execution state is unsupported")
)

// RestoreProposal rebuilds an opaque proposal from durable data only after the
// current catalog, policy, lifecycle state, and complete audit transition chain
// agree. Phase 4A2 deliberately rejects all execution states.
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
	if !definition.SupportsExecution {
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
	if initialState == ActionStateAuthorized {
		if currentState != ActionStateAuthorized || len(events) != 1 ||
			events[0] != (RestorationEvent{
				Type:    RestorationEventPolicyAuthorizedOnCreation,
				ToState: ActionStateAuthorized,
			}) {

			return ErrActionAuditMismatch
		}
		return nil
	}
	if initialState != ActionStateAwaitingConfirmation || len(events) < 1 ||
		events[0] != (RestorationEvent{
			Type:    RestorationEventProposalCreated,
			ToState: ActionStateAwaitingConfirmation,
		}) {

		return ErrActionAuditMismatch
	}
	switch currentState {
	case ActionStateAwaitingConfirmation:
		if len(events) != 1 {
			return ErrActionAuditMismatch
		}
	case ActionStateAuthorized:
		if len(events) != 2 || events[1] != (RestorationEvent{
			Type:      RestorationEventConfirmationGranted,
			FromState: ActionStateAwaitingConfirmation,
			ToState:   ActionStateAuthorized,
		}) {

			return ErrActionAuditMismatch
		}
	case ActionStateCancelled:
		if len(events) != 2 || events[1] != (RestorationEvent{
			Type:      RestorationEventCancelled,
			FromState: ActionStateAwaitingConfirmation,
			ToState:   ActionStateCancelled,
		}) {

			return ErrActionAuditMismatch
		}
	default:
		return ErrActionAuditMismatch
	}
	return nil
}
