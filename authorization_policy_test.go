package main

import (
	"errors"
	"testing"
)

func TestPhase4ActionConfirmationPolicyMatrix(t *testing.T) {
	tests := []struct {
		name             string
		origin           ActionOrigin
		safetyClass      ActionSafetyClass
		wantConfirmation bool
		wantWarning      bool
		wantState        ActionState
	}{
		{
			name: "direct user reversible", origin: ActionOriginDirectUser,
			safetyClass: ActionSafetyReversible, wantState: ActionStateAuthorized,
		},
		{
			name: "direct user recoverable", origin: ActionOriginDirectUser,
			safetyClass: ActionSafetyRecoverable, wantState: ActionStateAuthorized,
		},
		{
			name: "direct user compensatable", origin: ActionOriginDirectUser,
			safetyClass: ActionSafetyCompensatable, wantConfirmation: true,
			wantState: ActionStateAwaitingConfirmation,
		},
		{
			name: "direct user irreversible", origin: ActionOriginDirectUser,
			safetyClass: ActionSafetyIrreversible, wantConfirmation: true,
			wantWarning: true, wantState: ActionStateAwaitingConfirmation,
		},
		{
			name:   "assistant suggested reversible",
			origin: ActionOriginAssistantSuggested, safetyClass: ActionSafetyReversible,
			wantConfirmation: true, wantState: ActionStateAwaitingConfirmation,
		},
		{
			name:   "assistant suggested recoverable",
			origin: ActionOriginAssistantSuggested, safetyClass: ActionSafetyRecoverable,
			wantConfirmation: true, wantState: ActionStateAwaitingConfirmation,
		},
		{
			name:   "assistant suggested compensatable",
			origin: ActionOriginAssistantSuggested, safetyClass: ActionSafetyCompensatable,
			wantConfirmation: true, wantState: ActionStateAwaitingConfirmation,
		},
		{
			name:   "assistant suggested irreversible",
			origin: ActionOriginAssistantSuggested, safetyClass: ActionSafetyIrreversible,
			wantConfirmation: true, wantWarning: true,
			wantState: ActionStateAwaitingConfirmation,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := evaluateActionConfirmationPolicy(
				test.origin,
				test.safetyClass,
			)
			if err != nil {
				t.Fatal(err)
			}
			if decision.ConfirmationRequired != test.wantConfirmation ||
				decision.WarningRequired != test.wantWarning ||
				decision.InitialState != test.wantState ||
				decision.Reason == "" {
				t.Fatalf("decision=%+v", decision)
			}
		})
	}
}

func TestPhase4ActionConfirmationPolicyRejectsInvalidEnums(t *testing.T) {
	if _, err := evaluateActionConfirmationPolicy(
		ActionOrigin("model_claimed"),
		ActionSafetyRecoverable,
	); !errors.Is(err, errInvalidActionOrigin) {
		t.Fatalf("invalid origin error=%v", err)
	}
	if _, err := evaluateActionConfirmationPolicy(
		ActionOriginDirectUser,
		ActionSafetyClass("low_risk"),
	); !errors.Is(err, errInvalidActionSafetyClass) {
		t.Fatalf("invalid safety class error=%v", err)
	}
}

func TestPhase4ActionConfirmationPolicyIsDeterministic(t *testing.T) {
	first, err := evaluateActionConfirmationPolicy(
		ActionOriginAssistantSuggested,
		ActionSafetyRecoverable,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := evaluateActionConfirmationPolicy(
		ActionOriginAssistantSuggested,
		ActionSafetyRecoverable,
	)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same inputs produced different decisions: first=%+v second=%+v", first, second)
	}
}
