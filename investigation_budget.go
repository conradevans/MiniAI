package main

type investigationBudget struct {
	MaxPlannerRounds int
	MaxToolCalls     int
}

func defaultInvestigationBudget() investigationBudget {
	return investigationBudget{
		MaxPlannerRounds: 3,
		MaxToolCalls:     6,
	}
}

func (b investigationBudget) canCallTool(toolCallsUsed int) bool {
	return toolCallsUsed < b.MaxToolCalls
}
