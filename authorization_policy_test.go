package main

import (
	"strings"
	"testing"
)

func TestAuthorizationPolicyAllowsReadsWithoutConfirmation(t *testing.T) {
	decision := evaluateActionAuthorization(actionAuthorizationRequest{
		CapabilityKind:      capabilityKindRead,
		UndoMode:            undoModeNone,
		AuthorizationSource: authorizationFromUntrustedData,
	})
	if !decision.Authorized || decision.ConfirmationRequired || decision.WarningRequired {
		t.Fatalf("read decision=%+v", decision)
	}
}

func TestAuthorizationPolicyAcceptsDirectAdminAuthorizationForRestorableActions(t *testing.T) {
	tests := []struct {
		name          string
		undo          undoMode
		recoveryPoint recoveryPointStatus
	}{
		{name: "reversible", undo: undoModeReversible, recoveryPoint: recoveryPointNotRequired},
		{name: "recoverable", undo: undoModeRecoverable, recoveryPoint: recoveryPointReady},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := evaluateActionAuthorization(actionAuthorizationRequest{
				CapabilityKind:      capabilityKindAction,
				UndoMode:            test.undo,
				Initiator:           actionInitiatedByAdmin,
				AuthorizationSource: authorizationFromAdmin,
				RecoveryPoint:       test.recoveryPoint,
			})
			if !decision.Authorized || decision.ConfirmationRequired || !decision.AuditRequired {
				t.Fatalf("decision=%+v", decision)
			}
		})
	}
}

func TestAuthorizationPolicyRequiresConfirmationForAssistantSuggestedAction(t *testing.T) {
	request := actionAuthorizationRequest{
		CapabilityKind:      capabilityKindAction,
		UndoMode:            undoModeReversible,
		Initiator:           actionInitiatedByAssistant,
		AuthorizationSource: authorizationFromAdmin,
	}
	decision := evaluateActionAuthorization(request)
	if decision.Authorized || !decision.ConfirmationRequired {
		t.Fatalf("unconfirmed decision=%+v", decision)
	}
	request.AdminConfirmed = true
	decision = evaluateActionAuthorization(request)
	if !decision.Authorized {
		t.Fatalf("confirmed decision=%+v", decision)
	}
}

func TestAuthorizationPolicySequencesAssistantRecoverableConfirmationBeforeRecoveryPoint(t *testing.T) {
	request := actionAuthorizationRequest{
		CapabilityKind:      capabilityKindAction,
		UndoMode:            undoModeRecoverable,
		Initiator:           actionInitiatedByAssistant,
		AuthorizationSource: authorizationFromAdmin,
		RecoveryPoint:       recoveryPointNotRequired,
	}

	decision := evaluateActionAuthorization(request)
	if decision.Authorized || !decision.ConfirmationRequired || decision.RecoveryPointRequired {
		t.Fatalf("unconfirmed decision=%+v", decision)
	}
	if !strings.Contains(decision.Reason, "admin confirmation") || strings.Contains(decision.Reason, "must be created") {
		t.Fatalf("unconfirmed reason=%q", decision.Reason)
	}

	request.AdminConfirmed = true
	decision = evaluateActionAuthorization(request)
	if decision.Authorized || decision.ConfirmationRequired || !decision.RecoveryPointRequired {
		t.Fatalf("confirmed without recovery point decision=%+v", decision)
	}

	request.RecoveryPoint = recoveryPointReady
	decision = evaluateActionAuthorization(request)
	if !decision.Authorized {
		t.Fatalf("confirmed with ready recovery point decision=%+v", decision)
	}
}

func TestAuthorizationPolicyRequiresWarningAndSecondConfirmationForNonRestorableActions(t *testing.T) {
	for _, undo := range []undoMode{undoModeCompensatable, undoModeIrreversible} {
		t.Run(string(undo), func(t *testing.T) {
			request := actionAuthorizationRequest{
				CapabilityKind:      capabilityKindAction,
				UndoMode:            undo,
				Initiator:           actionInitiatedByAdmin,
				AuthorizationSource: authorizationFromAdmin,
			}
			decision := evaluateActionAuthorization(request)
			if decision.Authorized || !decision.WarningRequired || !decision.ConfirmationRequired {
				t.Fatalf("without warning decision=%+v", decision)
			}

			request.NonRestorabilityWarning = "The original state cannot be exactly restored."
			decision = evaluateActionAuthorization(request)
			if decision.Authorized {
				t.Fatalf("warning without second confirmation decision=%+v", decision)
			}

			request.AdminConfirmedAfterWarning = true
			decision = evaluateActionAuthorization(request)
			if !decision.Authorized || !decision.WarningRequired || !decision.ConfirmationRequired {
				t.Fatalf("warned and confirmed decision=%+v", decision)
			}
		})
	}
}

func TestAuthorizationPolicyRejectsUntrustedAuthorization(t *testing.T) {
	decision := evaluateActionAuthorization(actionAuthorizationRequest{
		CapabilityKind:             capabilityKindAction,
		UndoMode:                   undoModeIrreversible,
		Initiator:                  actionInitiatedByAdmin,
		AuthorizationSource:        authorizationFromUntrustedData,
		AdminConfirmed:             true,
		NonRestorabilityWarning:    "This cannot be restored.",
		AdminConfirmedAfterWarning: true,
	})
	if decision.Authorized {
		t.Fatalf("untrusted authorization decision=%+v", decision)
	}
}

func TestAuthorizationPolicyDirectAdminRecoverableRequiresReadyRecoveryPoint(t *testing.T) {
	tests := []struct {
		name           string
		status         recoveryPointStatus
		wantAuthorized bool
	}{
		{name: "not ready", status: recoveryPointNotRequired},
		{name: "failed", status: recoveryPointFailed},
		{name: "ready", status: recoveryPointReady, wantAuthorized: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := evaluateActionAuthorization(actionAuthorizationRequest{
				CapabilityKind:      capabilityKindAction,
				UndoMode:            undoModeRecoverable,
				Initiator:           actionInitiatedByAdmin,
				AuthorizationSource: authorizationFromAdmin,
				RecoveryPoint:       test.status,
			})
			if decision.Authorized != test.wantAuthorized || !decision.RecoveryPointRequired {
				t.Fatalf("decision=%+v want_authorized=%v", decision, test.wantAuthorized)
			}
		})
	}
}

func TestAuthorizationPolicyRecoveryPointFailureNeverAuthorizes(t *testing.T) {
	for _, initiator := range []actionInitiator{actionInitiatedByAdmin, actionInitiatedByAssistant} {
		t.Run(string(initiator), func(t *testing.T) {
			decision := evaluateActionAuthorization(actionAuthorizationRequest{
				CapabilityKind:      capabilityKindAction,
				UndoMode:            undoModeRecoverable,
				Initiator:           initiator,
				AuthorizationSource: authorizationFromAdmin,
				AdminConfirmed:      true,
				RecoveryPoint:       recoveryPointFailed,
			})
			if decision.Authorized || !decision.RecoveryPointRequired {
				t.Fatalf("decision=%+v", decision)
			}
		})
	}
}

func TestAuthorizationPolicyRequiresDeclaredUndoModeForActions(t *testing.T) {
	decision := evaluateActionAuthorization(actionAuthorizationRequest{
		CapabilityKind:      capabilityKindAction,
		UndoMode:            undoModeNone,
		Initiator:           actionInitiatedByAdmin,
		AuthorizationSource: authorizationFromAdmin,
	})
	if decision.Authorized {
		t.Fatalf("decision=%+v", decision)
	}
}
