package main

import (
	"context"
	"fmt"
	"time"
)

type Phase2BResult struct {
	Route                        ShadowRouteDecision   `json:"route"`
	Plan                         EvidencePlan          `json:"plan"`
	Results                      []EvidenceResult      `json:"results"`
	Evidence                     InvestigationEvidence `json:"evidence"`
	Packet                       EvidencePacket        `json:"packet"`
	PacketJSON                   []byte                `json:"packetJson"`
	EvidenceRounds               int                   `json:"evidenceRounds"`
	SecondRoundReads             int                   `json:"secondRoundReads"`
	EvidenceProcessingLimitation string                `json:"evidenceProcessingLimitation,omitempty"`
	SecondRound                  SecondRoundDecision   `json:"-"`
}

// Phase2BPrepared captures one deterministic routing/planning instant. It lets
// production display and execute the exact plan that was routed without
// re-reading the clock or reclassifying the question.
type Phase2BPrepared struct {
	Question           string
	NormalizedQuestion string
	Now                time.Time
	Route              ShadowRouteDecision
	Plan               EvidencePlan
}

type Phase2BPipeline struct {
	registry capabilityRegistry
	executor capabilityExecutor
	now      func() time.Time
	reduce   evidenceReductionFunc
}

type evidenceReductionFunc func(
	InvestigationRoute, string, []EvidenceRequirement, EvidencePlan, []EvidenceResult, capabilityRegistry,
) (InvestigationEvidence, error)

func NewPhase2BPipeline(registry capabilityRegistry, executor capabilityExecutor, now func() time.Time) Phase2BPipeline {
	if now == nil {
		now = time.Now
	}
	return Phase2BPipeline{registry: registry, executor: executor, now: now, reduce: ReduceEvidenceResults}
}

func (pipeline Phase2BPipeline) reduceEvidence(route InvestigationRoute, question string, requirements []EvidenceRequirement, plan EvidencePlan, results []EvidenceResult) (InvestigationEvidence, error) {
	reduce := pipeline.reduce
	if reduce == nil {
		reduce = ReduceEvidenceResults
	}
	return reduce(route, question, requirements, plan, results, pipeline.registry)
}

func (pipeline Phase2BPipeline) Prepare(question string, routerContext ShadowRouterContext) (Phase2BPrepared, error) {
	if routerContext.Now.IsZero() {
		routerContext.Now = pipeline.now().UTC()
	}
	routerContext.Now = routerContext.Now.UTC()
	routeDecision := routePhase2BQuestion(question, routerContext)
	if routeDecision.Resolution != RouteResolutionSupported {
		return Phase2BPrepared{
			Question: question, NormalizedQuestion: normalizeQuestionText(question),
			Now: routerContext.Now, Route: routeDecision,
		}, nil
	}
	normalizedQuestion := normalizeQuestionText(question)
	plan, err := BuildEvidencePlanForQuestion(routeDecision.RouteDescriptor(), routeDecision.Requirements, pipeline.registry, routerContext.Now, normalizedQuestion)
	if err != nil {
		return Phase2BPrepared{
			Question: question, NormalizedQuestion: normalizedQuestion,
			Now: routerContext.Now, Route: routeDecision,
		}, err
	}
	return Phase2BPrepared{
		Question: question, NormalizedQuestion: normalizedQuestion,
		Now: routerContext.Now, Route: routeDecision, Plan: plan,
	}, nil
}

func (pipeline Phase2BPipeline) ExecutePrepared(ctx context.Context, prepared Phase2BPrepared) (Phase2BResult, error) {
	if prepared.Route.Resolution != RouteResolutionSupported {
		return Phase2BResult{}, fmt.Errorf("phase2b route is %s: %s", prepared.Route.Resolution, prepared.Route.Reason)
	}
	if err := ctx.Err(); err != nil {
		return Phase2BResult{}, err
	}
	executor := NewEvidenceExecutor(pipeline.registry, pipeline.executor, pipeline.now)
	firstResults, err := executor.Execute(ctx, prepared.Plan)
	if err != nil {
		return Phase2BResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return Phase2BResult{}, err
	}
	firstEvidence, err := pipeline.reduceEvidence(
		prepared.Route.RouteDescriptor(), prepared.NormalizedQuestion,
		prepared.Route.Requirements, prepared.Plan, firstResults,
	)
	if err != nil {
		return Phase2BResult{}, err
	}
	firstEvidence = ApplyEvidenceConfidence(firstEvidence)

	plan := prepared.Plan
	results := firstResults
	evidence := firstEvidence
	evidenceRounds := 1
	secondRoundReads := 0
	processingLimitation := ""
	secondRound := PlanSecondEvidenceRound(prepared, firstEvidence, prepared.Plan, firstResults, pipeline.registry)
	if secondRound.Needed {
		if err := ctx.Err(); err != nil {
			return Phase2BResult{}, err
		}
		followUpResults, executeErr := executor.Execute(ctx, secondRound.Plan)
		if err := ctx.Err(); err != nil {
			return Phase2BResult{}, err
		}
		if executeErr != nil {
			secondRound.Reason = FollowUpExecutionFailed
		} else {
			// Record successful execution before attempting any later reduction.
			// A reducer failure must not erase reads that already ran.
			plan = combineEvidencePlans(prepared.Plan, secondRound.Plan, nil)
			results = combineEvidenceResults(firstResults, followUpResults)
			evidenceRounds = 2
			secondRoundReads = len(followUpResults)

			followUpEvidence, reduceErr := pipeline.reduceEvidence(
				prepared.Route.RouteDescriptor(), prepared.NormalizedQuestion,
				prepared.Route.Requirements, secondRound.Plan, followUpResults,
			)
			if reduceErr != nil {
				secondRound.Reason = FollowUpReductionFailed
				processingLimitation = string(FollowUpReductionFailed)
			} else {
				resolved := resolvedFollowUpRequirements(followUpEvidence)
				combinedPlan := combineEvidencePlans(prepared.Plan, secondRound.Plan, resolved)
				combinedEvidence, combineErr := pipeline.reduceEvidence(
					prepared.Route.RouteDescriptor(), prepared.NormalizedQuestion,
					prepared.Route.Requirements, combinedPlan, results,
				)
				if combineErr != nil {
					secondRound.Reason = FollowUpReductionFailed
					processingLimitation = string(FollowUpReductionFailed)
				} else {
					combinedEvidence.Missing = filterResolvedPriorMissing(combinedEvidence.Missing, firstEvidence.Missing, resolved)
					combinedEvidence, combineErr = NormalizeInvestigationEvidence(combinedEvidence)
					if combineErr != nil {
						secondRound.Reason = FollowUpReductionFailed
						processingLimitation = string(FollowUpReductionFailed)
					} else {
						plan = combinedPlan
						evidence = combinedEvidence
					}
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return Phase2BResult{}, err
	}
	evidence = ApplyEvidenceConfidence(evidence)
	packet, err := BuildEvidencePacket(evidence)
	if err != nil {
		return Phase2BResult{}, err
	}
	packetJSON, err := MarshalEvidencePacket(packet)
	if err != nil {
		return Phase2BResult{}, err
	}
	return Phase2BResult{
		Route: prepared.Route, Plan: plan, Results: results,
		Evidence: evidence, Packet: packet, PacketJSON: packetJSON,
		EvidenceRounds: evidenceRounds, SecondRoundReads: secondRoundReads,
		EvidenceProcessingLimitation: processingLimitation, SecondRound: secondRound,
	}, nil
}

func (pipeline Phase2BPipeline) Run(ctx context.Context, question string, routerContext ShadowRouterContext) (Phase2BResult, error) {
	prepared, err := pipeline.Prepare(question, routerContext)
	if err != nil {
		return Phase2BResult{}, err
	}
	return pipeline.ExecutePrepared(ctx, prepared)
}
