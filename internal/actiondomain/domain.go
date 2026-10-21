package actiondomain

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type ActionID string

type ActionKind string

const (
	ActionKindRestartApplication ActionKind = "restart_application"
	ActionKindRollbackDeployment ActionKind = "rollback_deployment"
	ActionKindDeployRepository   ActionKind = "deploy_repository"
)

type ActionTargetType string

const (
	ActionTargetApplication ActionTargetType = "application"
	ActionTargetRepository  ActionTargetType = "repository"
)

type ActionTarget struct {
	Type ActionTargetType `json:"type"`
	ID   string           `json:"id"`
}

type ActionState string

const (
	ActionStateProposed             ActionState = "proposed"
	ActionStateAwaitingConfirmation ActionState = "awaiting_confirmation"
	ActionStateAuthorized           ActionState = "authorized"
	ActionStateExecuting            ActionState = "executing"
	ActionStateSucceeded            ActionState = "succeeded"
	ActionStateFailed               ActionState = "failed"
	ActionStateCancelled            ActionState = "cancelled"
)

type ActionOrigin string

const (
	ActionOriginDirectUser         ActionOrigin = "direct_user"
	ActionOriginAssistantSuggested ActionOrigin = "assistant_suggested"
)

type ActionSafetyClass string

const (
	ActionSafetyReversible    ActionSafetyClass = "reversible"
	ActionSafetyRecoverable   ActionSafetyClass = "recoverable"
	ActionSafetyCompensatable ActionSafetyClass = "compensatable"
	ActionSafetyIrreversible  ActionSafetyClass = "irreversible"
)

type ActionPolicyReason string

const (
	ActionPolicyDirectUserNoConfirmation ActionPolicyReason = "direct_user_reversible_or_recoverable"
	ActionPolicyDirectUserConfirmation   ActionPolicyReason = "direct_user_compensatable_confirmation_required"
	ActionPolicyAssistantConfirmation    ActionPolicyReason = "assistant_suggested_confirmation_required"
	ActionPolicyIrreversibleWarning      ActionPolicyReason = "irreversible_warning_and_confirmation_required"
)

type AuthorizationDecision struct {
	ConfirmationRequired bool               `json:"confirmationRequired"`
	WarningRequired      bool               `json:"warningRequired"`
	InitialState         ActionState        `json:"initialState"`
	Reason               ActionPolicyReason `json:"reason"`
}

type RepositoryProvider string

const RepositoryProviderGitHub RepositoryProvider = "github"

// ActionArguments is sealed to this package. Each action kind accepts exactly
// one concrete argument value defined below.
type ActionArguments interface {
	actionArgumentsKind() ActionKind
}

type RestartApplicationArguments struct{}

func (RestartApplicationArguments) actionArgumentsKind() ActionKind {
	return ActionKindRestartApplication
}

type RollbackDeploymentArguments struct{}

func (RollbackDeploymentArguments) actionArgumentsKind() ActionKind {
	return ActionKindRollbackDeployment
}

// DeployRepositoryArguments accepts a fixed provider identity, not a URL or
// command string. A later executor can derive a provider URL from the validated
// repository target without accepting arbitrary transport input.
type DeployRepositoryArguments struct {
	Provider      RepositoryProvider `json:"provider"`
	ContainerPort int                `json:"containerPort,omitempty"`
	HealthPath    string             `json:"healthPath,omitempty"`
}

func (DeployRepositoryArguments) actionArgumentsKind() ActionKind {
	return ActionKindDeployRepository
}

type ActionDefinition struct {
	Kind                ActionKind
	SafetyClass         ActionSafetyClass
	TargetType          ActionTargetType
	SupportsExecution   bool
	executionFoundation actionExecutionFoundation
}

type actionExecutionFoundation uint8

const (
	actionExecutionFoundationNone actionExecutionFoundation = iota
	actionExecutionFoundationRestartApplication
)

type ProposalInput struct {
	ID        ActionID
	Kind      ActionKind
	Origin    ActionOrigin
	Target    ActionTarget
	Arguments ActionArguments
	CreatedAt time.Time
}

// Proposal is the authoritative action representation. Every field is private
// to this package so callers cannot rewrite origin, lifecycle state, identity,
// or the action kind after construction.
type Proposal struct {
	id        ActionID
	kind      ActionKind
	origin    ActionOrigin
	target    ActionTarget
	arguments ActionArguments
	createdAt time.Time
	state     ActionState
}

func (proposal Proposal) ID() ActionID {
	return proposal.id
}

func (proposal Proposal) Kind() ActionKind {
	return proposal.kind
}

func (proposal Proposal) Origin() ActionOrigin {
	return proposal.origin
}

func (proposal Proposal) State() ActionState {
	return proposal.state
}

func (proposal Proposal) SafetyClass() ActionSafetyClass {
	definition, ok := LookupDefinition(proposal.kind)
	if !ok {
		return ""
	}
	return definition.SafetyClass
}

func (proposal Proposal) Target() ActionTarget {
	return proposal.target
}

func (proposal Proposal) Arguments() ActionArguments {
	return proposal.arguments
}

func (proposal Proposal) CreatedAt() time.Time {
	return proposal.createdAt
}

func (proposal Proposal) ConfirmationRequired() bool {
	decision, err := EvaluateProposal(proposal)
	return err == nil && decision.ConfirmationRequired
}

func (proposal Proposal) WarningRequired() bool {
	decision, err := EvaluateProposal(proposal)
	return err == nil && decision.WarningRequired
}

func (proposal Proposal) PolicyReason() ActionPolicyReason {
	decision, err := EvaluateProposal(proposal)
	if err != nil {
		return ""
	}
	return decision.Reason
}

var (
	ErrUnknownActionKind            = errors.New("unknown action kind")
	ErrInvalidActionID              = errors.New("invalid action id")
	ErrInvalidActionTarget          = errors.New("invalid action target")
	ErrInvalidActionArguments       = errors.New("invalid action arguments")
	ErrInvalidActionCreatedAt       = errors.New("invalid action created timestamp")
	ErrInvalidActionProposal        = errors.New("invalid action proposal")
	ErrInvalidActionOrigin          = errors.New("invalid action origin")
	ErrInvalidActionSafetyClass     = errors.New("invalid action safety class")
	ErrInvalidActionState           = errors.New("invalid action state")
	ErrInvalidActionStateTransition = errors.New("invalid action state transition")
)

var (
	actionIDPattern              = regexp.MustCompile(`^action_[0-9a-f]{32}$`)
	appIDPattern                 = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	githubRepositoryOwnerPattern = regexp.MustCompile(
		`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`,
	)
	githubRepositoryNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// LookupDefinition is the closed authoritative action catalog. It returns
// definitions by value and has no mutable map or slice behind it.
func LookupDefinition(kind ActionKind) (ActionDefinition, bool) {
	switch kind {
	case ActionKindRestartApplication:
		return ActionDefinition{
			Kind:                ActionKindRestartApplication,
			SafetyClass:         ActionSafetyRecoverable,
			TargetType:          ActionTargetApplication,
			SupportsExecution:   false,
			executionFoundation: actionExecutionFoundationRestartApplication,
		}, true
	case ActionKindRollbackDeployment:
		return ActionDefinition{
			Kind:              ActionKindRollbackDeployment,
			SafetyClass:       ActionSafetyIrreversible,
			TargetType:        ActionTargetApplication,
			SupportsExecution: false,
		}, true
	case ActionKindDeployRepository:
		return ActionDefinition{
			Kind:              ActionKindDeployRepository,
			SafetyClass:       ActionSafetyCompensatable,
			TargetType:        ActionTargetRepository,
			SupportsExecution: false,
		}, true
	default:
		return ActionDefinition{}, false
	}
}

func Definitions() []ActionDefinition {
	kinds := [...]ActionKind{
		ActionKindRestartApplication,
		ActionKindRollbackDeployment,
		ActionKindDeployRepository,
	}
	definitions := make([]ActionDefinition, 0, len(kinds))
	for _, kind := range kinds {
		definition, _ := LookupDefinition(kind)
		definitions = append(definitions, definition)
	}
	return definitions
}

func NewProposal(input ProposalInput) (Proposal, error) {
	definition, ok := LookupDefinition(input.Kind)
	if !ok {
		return Proposal{}, fmt.Errorf("%w: %q", ErrUnknownActionKind, input.Kind)
	}
	if !ValidActionOrigin(input.Origin) {
		return Proposal{}, fmt.Errorf("%w: %q", ErrInvalidActionOrigin, input.Origin)
	}
	proposal := Proposal{
		id:        input.ID,
		kind:      input.Kind,
		origin:    input.Origin,
		target:    input.Target,
		arguments: input.Arguments,
		createdAt: input.CreatedAt.UTC(),
		state:     ActionStateProposed,
	}
	if err := validateProposalIdentity(proposal, definition); err != nil {
		return Proposal{}, err
	}
	decision, err := EvaluateConfirmationPolicy(input.Origin, definition.SafetyClass)
	if err != nil {
		return Proposal{}, err
	}
	if err := ValidateStateTransition(ActionStateProposed, decision.InitialState); err != nil {
		return Proposal{}, err
	}
	proposal.state = decision.InitialState
	return proposal, nil
}

func EvaluateProposal(proposal Proposal) (AuthorizationDecision, error) {
	if proposal.zero() {
		return AuthorizationDecision{}, ErrInvalidActionProposal
	}
	definition, ok := LookupDefinition(proposal.kind)
	if !ok {
		return AuthorizationDecision{}, fmt.Errorf(
			"%w: %q",
			ErrUnknownActionKind,
			proposal.kind,
		)
	}
	if !ValidActionOrigin(proposal.origin) {
		return AuthorizationDecision{}, fmt.Errorf(
			"%w: %q",
			ErrInvalidActionOrigin,
			proposal.origin,
		)
	}
	if !validActionState(proposal.state) {
		return AuthorizationDecision{}, fmt.Errorf(
			"%w: %q",
			ErrInvalidActionState,
			proposal.state,
		)
	}
	if err := validateProposalIdentity(proposal, definition); err != nil {
		return AuthorizationDecision{}, err
	}
	decision, err := EvaluateConfirmationPolicy(proposal.origin, definition.SafetyClass)
	if err != nil {
		return AuthorizationDecision{}, err
	}
	if !stateReachableFromInitial(decision.InitialState, proposal.state) {
		return AuthorizationDecision{}, fmt.Errorf(
			"%w: %q is unreachable from %q",
			ErrInvalidActionState,
			proposal.state,
			decision.InitialState,
		)
	}
	return decision, nil
}

func TransitionProposal(proposal Proposal, to ActionState) (Proposal, error) {
	if _, err := EvaluateProposal(proposal); err != nil {
		return Proposal{}, err
	}
	if err := ValidateStateTransition(proposal.state, to); err != nil {
		return Proposal{}, err
	}
	proposal.state = to
	return proposal, nil
}

func EvaluateConfirmationPolicy(
	origin ActionOrigin,
	safetyClass ActionSafetyClass,
) (AuthorizationDecision, error) {
	if !ValidActionOrigin(origin) {
		return AuthorizationDecision{}, fmt.Errorf(
			"%w: %q",
			ErrInvalidActionOrigin,
			origin,
		)
	}
	if !ValidActionSafetyClass(safetyClass) {
		return AuthorizationDecision{}, fmt.Errorf(
			"%w: %q",
			ErrInvalidActionSafetyClass,
			safetyClass,
		)
	}

	if safetyClass == ActionSafetyIrreversible {
		return AuthorizationDecision{
			ConfirmationRequired: true,
			WarningRequired:      true,
			InitialState:         ActionStateAwaitingConfirmation,
			Reason:               ActionPolicyIrreversibleWarning,
		}, nil
	}
	if origin == ActionOriginAssistantSuggested {
		return AuthorizationDecision{
			ConfirmationRequired: true,
			InitialState:         ActionStateAwaitingConfirmation,
			Reason:               ActionPolicyAssistantConfirmation,
		}, nil
	}
	if safetyClass == ActionSafetyCompensatable {
		return AuthorizationDecision{
			ConfirmationRequired: true,
			InitialState:         ActionStateAwaitingConfirmation,
			Reason:               ActionPolicyDirectUserConfirmation,
		}, nil
	}
	return AuthorizationDecision{
		InitialState: ActionStateAuthorized,
		Reason:       ActionPolicyDirectUserNoConfirmation,
	}, nil
}

func ValidActionOrigin(origin ActionOrigin) bool {
	switch origin {
	case ActionOriginDirectUser, ActionOriginAssistantSuggested:
		return true
	default:
		return false
	}
}

func ValidActionSafetyClass(safetyClass ActionSafetyClass) bool {
	switch safetyClass {
	case ActionSafetyReversible, ActionSafetyRecoverable,
		ActionSafetyCompensatable, ActionSafetyIrreversible:
		return true
	default:
		return false
	}
}

// ValidateRestartExecutionFoundation revalidates the complete opaque proposal
// against the closed catalog before the restart-specific executor may begin.
// The foundation permit is intentionally separate from SupportsExecution:
// Phase 4B1 provides no API or automatic path that can execute an action.
func ValidateRestartExecutionFoundation(proposal Proposal) error {
	if _, err := EvaluateProposal(proposal); err != nil {
		return err
	}
	definition, ok := LookupDefinition(proposal.kind)
	if !ok || definition.executionFoundation != actionExecutionFoundationRestartApplication ||
		proposal.kind != ActionKindRestartApplication ||
		definition.TargetType != ActionTargetApplication ||
		proposal.target.Type != ActionTargetApplication {

		return ErrActionExecutionUnsupported
	}
	if _, ok := proposal.arguments.(RestartApplicationArguments); !ok {
		return ErrInvalidActionArguments
	}
	return nil
}

func supportsRestoredExecutionState(kind ActionKind) bool {
	definition, ok := LookupDefinition(kind)
	return ok && definition.executionFoundation == actionExecutionFoundationRestartApplication
}

func ValidApplicationTargetID(id string) bool {
	return appIDPattern.MatchString(id)
}

func ValidateStateTransition(from, to ActionState) error {
	if !validActionState(from) || !validActionState(to) {
		return fmt.Errorf(
			"%w: %q -> %q",
			ErrInvalidActionState,
			from,
			to,
		)
	}
	valid := false
	switch from {
	case ActionStateProposed:
		valid = to == ActionStateAwaitingConfirmation ||
			to == ActionStateAuthorized
	case ActionStateAwaitingConfirmation:
		valid = to == ActionStateAuthorized ||
			to == ActionStateCancelled
	case ActionStateAuthorized:
		valid = to == ActionStateExecuting
	case ActionStateExecuting:
		valid = to == ActionStateSucceeded ||
			to == ActionStateFailed
	}
	if !valid {
		return fmt.Errorf(
			"%w: %q -> %q",
			ErrInvalidActionStateTransition,
			from,
			to,
		)
	}
	return nil
}

func (proposal Proposal) zero() bool {
	return proposal.id == "" && proposal.kind == "" && proposal.origin == "" &&
		proposal.target == (ActionTarget{}) && proposal.arguments == nil &&
		proposal.createdAt.IsZero() && proposal.state == ""
}

func stateReachableFromInitial(initial, current ActionState) bool {
	switch initial {
	case ActionStateAuthorized:
		switch current {
		case ActionStateAuthorized, ActionStateExecuting,
			ActionStateSucceeded, ActionStateFailed:
			return true
		}
	case ActionStateAwaitingConfirmation:
		switch current {
		case ActionStateAwaitingConfirmation, ActionStateAuthorized,
			ActionStateCancelled, ActionStateExecuting,
			ActionStateSucceeded, ActionStateFailed:
			return true
		}
	}
	return false
}

func validateProposalIdentity(
	proposal Proposal,
	definition ActionDefinition,
) error {
	if !actionIDPattern.MatchString(string(proposal.id)) {
		return fmt.Errorf("%w: %q", ErrInvalidActionID, proposal.id)
	}
	if proposal.createdAt.IsZero() {
		return ErrInvalidActionCreatedAt
	}
	if err := validateActionTarget(proposal.target, definition.TargetType); err != nil {
		return err
	}
	if proposal.arguments == nil {
		return ErrInvalidActionArguments
	}
	if proposal.arguments.actionArgumentsKind() != proposal.kind {
		return fmt.Errorf(
			"%w: arguments for %q supplied to %q",
			ErrInvalidActionArguments,
			proposal.arguments.actionArgumentsKind(),
			proposal.kind,
		)
	}
	return validateArguments(proposal.kind, proposal.arguments)
}

func validateActionTarget(target ActionTarget, wantType ActionTargetType) error {
	if target.Type != wantType {
		return fmt.Errorf(
			"%w: type %q does not match %q",
			ErrInvalidActionTarget,
			target.Type,
			wantType,
		)
	}
	switch target.Type {
	case ActionTargetApplication:
		if !appIDPattern.MatchString(target.ID) {
			return fmt.Errorf("%w: invalid application id", ErrInvalidActionTarget)
		}
	case ActionTargetRepository:
		if !validGitHubRepositoryIdentity(target.ID) {
			return fmt.Errorf("%w: invalid repository id", ErrInvalidActionTarget)
		}
	default:
		return fmt.Errorf(
			"%w: unsupported type %q",
			ErrInvalidActionTarget,
			target.Type,
		)
	}
	return nil
}

func validateArguments(kind ActionKind, arguments ActionArguments) error {
	switch kind {
	case ActionKindRestartApplication:
		if _, ok := arguments.(RestartApplicationArguments); !ok {
			return fmt.Errorf("%w: restart_application", ErrInvalidActionArguments)
		}
	case ActionKindRollbackDeployment:
		if _, ok := arguments.(RollbackDeploymentArguments); !ok {
			return fmt.Errorf("%w: rollback_deployment", ErrInvalidActionArguments)
		}
	case ActionKindDeployRepository:
		value, ok := arguments.(DeployRepositoryArguments)
		if !ok {
			return fmt.Errorf("%w: deploy_repository", ErrInvalidActionArguments)
		}
		if value.Provider != RepositoryProviderGitHub {
			return fmt.Errorf(
				"%w: unsupported repository provider %q",
				ErrInvalidActionArguments,
				value.Provider,
			)
		}
		if value.ContainerPort < 1 || value.ContainerPort > 65535 {
			return fmt.Errorf(
				"%w: container port must be between 1 and 65535",
				ErrInvalidActionArguments,
			)
		}
		if !validDeployHealthPath(value.HealthPath) {
			return fmt.Errorf("%w: invalid health path", ErrInvalidActionArguments)
		}
	default:
		return fmt.Errorf("%w: %q", ErrUnknownActionKind, kind)
	}
	return nil
}

func validGitHubRepositoryIdentity(identity string) bool {
	if strings.Count(identity, "/") != 1 {
		return false
	}
	owner, repository, ok := strings.Cut(identity, "/")
	if !ok ||
		strings.Contains(owner, "--") ||
		!githubRepositoryOwnerPattern.MatchString(owner) ||
		!githubRepositoryNamePattern.MatchString(repository) {

		return false
	}
	return repository != "." && repository != ".."
}

func validDeployHealthPath(healthPath string) bool {
	if healthPath == "" ||
		healthPath != strings.TrimSpace(healthPath) ||
		len(healthPath) > 2048 ||
		!strings.HasPrefix(healthPath, "/") ||
		strings.HasPrefix(healthPath, "//") ||
		strings.Contains(healthPath, "\\") ||
		strings.Contains(strings.ToLower(healthPath), "://") ||
		strings.IndexFunc(healthPath, unicode.IsSpace) >= 0 ||
		strings.IndexFunc(healthPath, unicode.IsControl) >= 0 {

		return false
	}
	parsed, err := url.ParseRequestURI(healthPath)
	if err != nil || parsed.IsAbs() || parsed.Host != "" ||
		parsed.Opaque != "" || parsed.Fragment != "" || parsed.Path == "" ||
		!strings.HasPrefix(parsed.Path, "/") ||
		strings.HasPrefix(parsed.Path, "//") ||
		strings.Contains(strings.ToLower(parsed.Path), "://") ||
		strings.IndexFunc(parsed.Path, unicode.IsControl) >= 0 {

		return false
	}
	decodedQuery, err := url.QueryUnescape(parsed.RawQuery)
	return err == nil && strings.IndexFunc(decodedQuery, unicode.IsControl) < 0
}

func validActionState(state ActionState) bool {
	switch state {
	case ActionStateProposed, ActionStateAwaitingConfirmation,
		ActionStateAuthorized, ActionStateExecuting, ActionStateSucceeded,
		ActionStateFailed, ActionStateCancelled:
		return true
	default:
		return false
	}
}
