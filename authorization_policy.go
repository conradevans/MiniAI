package main

import "strings"

type actionInitiator string

const (
	actionInitiatedByAdmin     actionInitiator = "authenticated_admin"
	actionInitiatedByAssistant actionInitiator = "assistant"
)

type authorizationSource string

const (
	authorizationFromAdmin         authorizationSource = "authenticated_admin_message"
	authorizationFromUntrustedData authorizationSource = "untrusted_data"
)

type recoveryPointStatus string

const (
	recoveryPointNotRequired recoveryPointStatus = "not_required"
	recoveryPointReady       recoveryPointStatus = "created_and_verified"
	recoveryPointFailed      recoveryPointStatus = "creation_or_verification_failed"
)

type actionAuthorizationRequest struct {
	CapabilityKind             capabilityKind
	UndoMode                   undoMode
	Initiator                  actionInitiator
	AuthorizationSource        authorizationSource
	AdminConfirmed             bool
	NonRestorabilityWarning    string
	AdminConfirmedAfterWarning bool
	RecoveryPoint              recoveryPointStatus
}

type authorizationDecision struct {
	Authorized            bool
	ConfirmationRequired  bool
	WarningRequired       bool
	RecoveryPointRequired bool
	AuditRequired         bool
	Reason                string
}

// evaluateActionAuthorization is deliberately fail-closed. It defines future
// action policy only; Phase 0 registers no action capabilities and does not call
// this from the current request flow.
func evaluateActionAuthorization(req actionAuthorizationRequest) authorizationDecision {
	if req.CapabilityKind == capabilityKindRead {
		return authorizationDecision{Authorized: true, Reason: "read-only capability"}
	}
	if req.CapabilityKind != capabilityKindAction {
		return authorizationDecision{Reason: "unknown capability kind"}
	}

	decision := authorizationDecision{AuditRequired: true}
	if req.AuthorizationSource != authorizationFromAdmin {
		decision.ConfirmationRequired = true
		decision.Reason = "only an authenticated admin message can authorize an action"
		return decision
	}
	if req.Initiator != actionInitiatedByAdmin && req.Initiator != actionInitiatedByAssistant {
		decision.Reason = "unknown action initiator"
		return decision
	}

	switch req.UndoMode {
	case undoModeReversible:
		if req.Initiator == actionInitiatedByAssistant && !req.AdminConfirmed {
			decision.ConfirmationRequired = true
			decision.Reason = "assistant-suggested actions require explicit admin confirmation"
			return decision
		}
	case undoModeRecoverable:
		if req.Initiator == actionInitiatedByAssistant && !req.AdminConfirmed {
			decision.ConfirmationRequired = true
			decision.Reason = "assistant-suggested recoverable actions require explicit admin confirmation before recovery-point preparation"
			return decision
		}
		decision.RecoveryPointRequired = true
		if req.RecoveryPoint != recoveryPointReady {
			decision.Reason = "a recovery point must be created and verified before the action"
			return decision
		}
	case undoModeCompensatable, undoModeIrreversible:
		decision.WarningRequired = true
		decision.ConfirmationRequired = true
		if strings.TrimSpace(req.NonRestorabilityWarning) == "" || !req.AdminConfirmedAfterWarning {
			decision.Reason = "the admin must receive a non-restorability warning and explicitly confirm afterward"
			return decision
		}
	default:
		decision.Reason = "actions must declare undo semantics"
		return decision
	}

	decision.Authorized = true
	decision.Reason = "authorized by authenticated admin"
	return decision
}
