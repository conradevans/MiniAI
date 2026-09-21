package main

import "testing"

func TestDefaultInvestigationBudgetRemainsBounded(t *testing.T) {
	budget := defaultInvestigationBudget()
	if budget.MaxPlannerRounds != 3 || budget.MaxToolCalls != 6 {
		t.Fatalf("default budget=%+v want rounds=3 tool_calls=6", budget)
	}
	if !budget.canCallTool(5) || budget.canCallTool(6) {
		t.Fatal("tool-call budget boundary changed")
	}
}
