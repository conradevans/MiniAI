package main

import (
	"context"
	"fmt"
	"time"
)

type Phase2BResult struct {
	Route      ShadowRouteDecision   `json:"route"`
	Plan       EvidencePlan          `json:"plan"`
	Results    []EvidenceResult      `json:"results"`
	Evidence   InvestigationEvidence `json:"evidence"`
	Packet     EvidencePacket        `json:"packet"`
	PacketJSON []byte                `json:"packetJson"`
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
}

func NewPhase2BPipeline(registry capabilityRegistry, executor capabilityExecutor, now func() time.Time) Phase2BPipeline {
	if now == nil {
		now = time.Now
	}
	return Phase2BPipeline{registry: registry, executor: executor, now: now}
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
	executor := NewEvidenceExecutor(pipeline.registry, pipeline.executor, pipeline.now)
	results, err := executor.Execute(ctx, prepared.Plan)
	if err != nil {
		return Phase2BResult{}, err
	}
	evidence, err := ReduceEvidenceResults(prepared.Route.RouteDescriptor(), prepared.NormalizedQuestion, prepared.Route.Requirements, prepared.Plan, results, pipeline.registry)
	if err != nil {
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
		Route: prepared.Route, Plan: prepared.Plan, Results: results,
		Evidence: evidence, Packet: packet, PacketJSON: packetJSON,
	}, nil
}

func (pipeline Phase2BPipeline) Run(ctx context.Context, question string, routerContext ShadowRouterContext) (Phase2BResult, error) {
	prepared, err := pipeline.Prepare(question, routerContext)
	if err != nil {
		return Phase2BResult{}, err
	}
	return pipeline.ExecutePrepared(ctx, prepared)
}
