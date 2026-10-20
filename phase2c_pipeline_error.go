package main

import (
	"errors"
	"fmt"
	"log"
	"strings"
)

func phase2CPipelineLimitation(err error) string {
	var pipelineErr Phase2BPipelineError
	if errors.As(err, &pipelineErr) && pipelineErr.Category != "" {
		return string(pipelineErr.Category)
	}
	return "evidence_pipeline_failed"
}

func logPhase2CPipelineFailure(route InvestigationRouteID, results []EvidenceResult, err error) {
	stage := Phase2BPipelineStage("unknown")
	category := Phase2BPipelineErrorCategory("evidence_pipeline_failed")
	packetBytes, packetTarget, packetLimit := 0, 0, 0
	var pipelineErr Phase2BPipelineError
	if errors.As(err, &pipelineErr) {
		stage = pipelineErr.Stage
		category = pipelineErr.Category
		packetBytes = pipelineErr.PacketBytes
		packetTarget = pipelineErr.PacketTarget
		packetLimit = pipelineErr.PacketLimit
	}
	capabilities := make([]string, 0, len(results))
	for _, result := range results {
		availability := result.Availability
		if availability == "" {
			availability = AvailabilityUnavailable
		}
		capability := fmt.Sprintf("%s:%s", result.Capability, availability)
		if result.ErrorCategory != "" {
			capability += "/" + string(result.ErrorCategory)
		}
		capabilities = append(capabilities, capability)
	}
	log.Printf(
		"Phase 2C evidence pipeline failed: route=%s stage=%s category=%s packet_bytes=%d packet_target=%d packet_limit=%d capabilities=%s",
		route, stage, category, packetBytes, packetTarget, packetLimit, strings.Join(capabilities, ","),
	)
}
