package main

import (
	"context"
	"fmt"
	"time"
)

// Phase2BResult is shadow-only internal output. No production chat, SSE,
// persistence, prompt, or model path calls this pipeline.
type Phase2BResult struct {
	Route      ShadowRouteDecision   `json:"route"`
	Plan       EvidencePlan          `json:"plan"`
	Results    []EvidenceResult      `json:"results"`
	Evidence   InvestigationEvidence `json:"evidence"`
	Packet     EvidencePacket        `json:"packet"`
	PacketJSON []byte                `json:"packetJson"`
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

func (pipeline Phase2BPipeline) Run(ctx context.Context, question string, routerContext ShadowRouterContext) (Phase2BResult, error) {
	if routerContext.Now.IsZero() {
		routerContext.Now = pipeline.now().UTC()
	}
	routeDecision := routePhase2BQuestion(question, routerContext)
	if routeDecision.Resolution != RouteResolutionSupported {
		return Phase2BResult{}, fmt.Errorf("phase2b route is %s: %s", routeDecision.Resolution, routeDecision.Reason)
	}
	plan, err := BuildEvidencePlanForQuestion(routeDecision.RouteDescriptor(), routeDecision.Requirements, pipeline.registry, routerContext.Now, normalizeQuestionText(question))
	if err != nil {
		return Phase2BResult{}, err
	}
	executor := NewEvidenceExecutor(pipeline.registry, pipeline.executor, pipeline.now)
	results, err := executor.Execute(ctx, plan)
	if err != nil {
		return Phase2BResult{}, err
	}
	normalizedQuestion := normalizeQuestionText(question)
	evidence, err := ReduceEvidenceResults(routeDecision.RouteDescriptor(), normalizedQuestion, routeDecision.Requirements, plan, results, pipeline.registry)
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
		Route: routeDecision, Plan: plan, Results: results,
		Evidence: evidence, Packet: packet, PacketJSON: packetJSON,
	}, nil
}
