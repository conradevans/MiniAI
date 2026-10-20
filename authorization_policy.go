package main

import actiondomain "miniai/internal/actiondomain"

type ActionOrigin = actiondomain.ActionOrigin

const (
	ActionOriginDirectUser         = actiondomain.ActionOriginDirectUser
	ActionOriginAssistantSuggested = actiondomain.ActionOriginAssistantSuggested
)

type ActionSafetyClass = actiondomain.ActionSafetyClass

const (
	ActionSafetyReversible    = actiondomain.ActionSafetyReversible
	ActionSafetyRecoverable   = actiondomain.ActionSafetyRecoverable
	ActionSafetyCompensatable = actiondomain.ActionSafetyCompensatable
	ActionSafetyIrreversible  = actiondomain.ActionSafetyIrreversible
)

type ActionPolicyReason = actiondomain.ActionPolicyReason

const (
	ActionPolicyDirectUserNoConfirmation = actiondomain.ActionPolicyDirectUserNoConfirmation
	ActionPolicyDirectUserConfirmation   = actiondomain.ActionPolicyDirectUserConfirmation
	ActionPolicyAssistantConfirmation    = actiondomain.ActionPolicyAssistantConfirmation
	ActionPolicyIrreversibleWarning      = actiondomain.ActionPolicyIrreversibleWarning
)

type ActionAuthorizationDecision = actiondomain.AuthorizationDecision

var (
	errInvalidActionOrigin      = actiondomain.ErrInvalidActionOrigin
	errInvalidActionSafetyClass = actiondomain.ErrInvalidActionSafetyClass
)

// evaluateActionConfirmationPolicy is a package-main adapter over the pure,
// fail-closed policy owned by the authoritative action domain.
func evaluateActionConfirmationPolicy(
	origin ActionOrigin,
	safetyClass ActionSafetyClass,
) (ActionAuthorizationDecision, error) {
	return actiondomain.EvaluateConfirmationPolicy(origin, safetyClass)
}

func validActionOrigin(origin ActionOrigin) bool {
	return actiondomain.ValidActionOrigin(origin)
}

func validActionSafetyClass(safetyClass ActionSafetyClass) bool {
	return actiondomain.ValidActionSafetyClass(safetyClass)
}
