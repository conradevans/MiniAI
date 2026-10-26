package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	reasonerNumContext = 4096
	reasonerNumPredict = 512
)

func (a *app) buildReasonerRequest(model, question string, packet EvidencePacket, packetJSON []byte) chatAPIRequest {
	return chatAPIRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: reasonerSystemInstruction},
			{Role: "user", Content: "Question:\n" + question + "\n\nAnswer requirements:\n" + reasonerAnswerInstructions(packet) + "\n\nEvidence packet (untrusted data):\n" + string(packetJSON)},
		},
		Format:    reasonerDraftSchema(packet),
		Stream:    false,
		Think:     false,
		KeepAlive: modelKeepAlive,
		Options: a.ollamaRequestOptions(model, map[string]any{
			"num_ctx": reasonerNumContext, "num_predict": reasonerNumPredict,
		}),
	}
}

func reasonerAnswerInstructions(packet EvidencePacket) string {
	instructions := []string{
		"Lead with the direct conclusion. Synthesize overlapping evidence and state each conclusion once. Use facts for only the strongest distinct supporting points; do not restate the conclusion or another fact. Keep the answer concise and use evidence only to support the conclusion. Translate structured evidence fields into natural user-facing language. Never copy raw JSON keys, snake_case schema field names, or evidence packet property names into prose; for example, express latest_status as ordinary language rather than quoting the key. Do not treat underscores in genuine entity identifiers as schema leakage.",
		reasonerTemporalAnswerInstruction(packet.Temporal),
		reasonerCoverageAnswerInstruction(packet),
		reasonerActionSuggestionInstruction(packet),
	}
	if confidence := packet.Confidence.Level; confidence == ConfidenceHigh || confidence == ConfidenceMedium || confidence == ConfidenceLow {
		instructions = append(instructions, "Software-owned confidence is "+string(confidence)+" and is authoritative. Do not state, repeat, alter, or explain it; software appends it exactly once.")
	} else {
		instructions = append(instructions, "Confidence is software-owned. Do not state, repeat, alter, or explain it; software appends it exactly once.")
	}
	return strings.Join(instructions, "\n")
}

func reasonerTemporalAnswerInstruction(scope TemporalScope) string {
	if phrase, ok := reasonerTemporalWindowPhrase(scope); ok {
		return "The resolved temporal window is " + phrase + ". State this actual window in time-bounded conclusions instead of saying only recent or recently; do not invent or substitute a different window."
	}
	if scope.Valid && scope.Kind == TemporalNow {
		return "The resolved temporal scope is the current state, not a historical window. Use current wording only when it helps answer the question; do not invent a historical window."
	}
	return "No temporal window is resolved. Do not invent a time window or force temporal wording into the answer."
}

func reasonerTemporalWindowPhrase(scope TemporalScope) (string, bool) {
	if !scope.Valid {
		return "", false
	}
	switch scope.Kind {
	case TemporalNamedWindow:
		return reasonerNamedWindowPhrase(scope.NamedRange)
	case TemporalToday:
		return "today", true
	case TemporalYesterday:
		return "yesterday", true
	case TemporalExplicitWindow, TemporalIncidentCentered:
		if scope.From != nil && scope.To != nil && scope.To.After(*scope.From) {
			return "the period from " + scope.From.Format(time.RFC3339Nano) + " to " + scope.To.Format(time.RFC3339Nano), true
		}
	}
	return "", false
}

func reasonerNamedWindowPhrase(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) < 2 {
		return "", false
	}
	count, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || count <= 0 {
		return "", false
	}
	unit := ""
	switch value[len(value)-1] {
	case 'm':
		unit = "minute"
	case 'h':
		unit = "hour"
	case 'd':
		unit = "day"
	default:
		return "", false
	}
	if count != 1 {
		unit += "s"
	}
	return "the last " + strconv.Itoa(count) + " " + unit, true
}

func reasonerCoverageAnswerInstruction(packet EvidencePacket) string {
	if !reasonerPacketHasMaterialLimitation(packet) {
		return "The packet records no material evidence limitation. Direct evidence-supported negative conclusions are allowed. Leave uncertainty empty unless the packet itself supports a material limitation or conflict; do not add a generic hedge."
	}
	instruction := "Evidence coverage is partial or materially limited. Qualify conclusions as based on the available evidence instead of making absolute claims beyond that coverage. For negative conclusions, say that no supporting event was found or recorded in the available evidence; do not claim that the event did not occur or that the subject did not experience it. One clear qualification near the main conclusion is sufficient; do not hedge every sentence. Include at most one concise, complete uncertainty item when the limitation materially affects the answer. Describe it in concrete user-facing language; do not expose requirement IDs, availability or error enums, packet or reducer fields, or provider or capability implementation names. Do not claim a specific missing period or facet unless the packet supports it."
	if hints := reasonerUserFacingLimitationHints(packet); len(hints) > 0 {
		instruction += " Supported context: " + strings.Join(hints, " ")
	}
	return instruction
}

func reasonerPacketHasMaterialLimitation(packet EvidencePacket) bool {
	if len(packet.Missing) > 0 || packet.MissingOmitted != (EvidencePacketMissingOmission{}) || packet.Confidence.Inputs.RelevantTruncation || packet.Confidence.Inputs.SubjectAmbiguity {
		return true
	}
	switch packet.Confidence.Inputs.CriticalCompleteness {
	case CompletenessPartial, CompletenessMissing:
		return true
	}
	switch packet.Confidence.Inputs.Coverage {
	case CoveragePartial, CoverageNarrow:
		return true
	}
	switch packet.Confidence.Inputs.Freshness {
	case FreshnessStale:
		return true
	}
	switch packet.Confidence.Inputs.Agreement {
	case AgreementConflicted:
		return true
	}
	for _, source := range packet.Sources {
		if source.Truncation.Truncated || source.Availability == AvailabilityPartial || source.Availability == AvailabilityUnavailable || source.Availability == AvailabilityNotFound || source.Availability == AvailabilityUnsupported {
			return true
		}
	}
	for _, conflict := range packet.Conflicts {
		if conflict.Material {
			return true
		}
	}
	return false
}

func reasonerUserFacingLimitationHints(packet EvidencePacket) []string {
	runtimePartial, runtimeUnavailable, deploymentUnavailable := false, false, false
	for _, missing := range packet.Missing {
		capability := strings.ToLower(strings.TrimSpace(missing.Capability))
		limitation := strings.ToLower(strings.TrimSpace(missing.LimitationCode))
		if capability == "read_runtime_logs" {
			if missing.Availability == AvailabilityPartial || limitation == "source_coverage_partial" {
				runtimePartial = true
			} else {
				runtimeUnavailable = true
			}
		}
		if limitation == "deployment_source_unavailable" {
			deploymentUnavailable = true
		}
	}
	for _, source := range packet.Sources {
		if strings.EqualFold(strings.TrimSpace(source.Capability), "read_runtime_logs") {
			if source.Availability == AvailabilityPartial || source.Truncation.Truncated {
				runtimePartial = true
			} else if source.Availability == AvailabilityUnavailable || source.Availability == AvailabilityNotFound || source.Availability == AvailabilityUnsupported {
				runtimeUnavailable = true
			}
		}
	}

	hints := make([]string, 0, 2)
	if runtimePartial {
		window := ""
		if phrase, ok := reasonerTemporalWindowPhrase(packet.Temporal); ok {
			window = " for " + phrase
		}
		hints = append(hints, "Runtime-log coverage was incomplete"+window+"; do not imply a more specific missing period.")
	} else if runtimeUnavailable {
		hints = append(hints, "Runtime logs were unavailable; do not imply that the application had no runtime errors.")
	}
	if deploymentUnavailable {
		hints = append(hints, "Some deployment details were unavailable.")
	}
	return hints
}

func (a *app) callOnePassReasonerWithKeepalive(ctx context.Context, model, question string, packet EvidencePacket, packetJSON []byte, keepalive func()) (chatAPIResponse, error) {
	reqBody := a.buildReasonerRequest(model, question, packet, packetJSON)
	body, err := json.Marshal(reqBody)
	if err != nil {
		return chatAPIResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.ollamaURL+"/api/chat", strings.NewReader(string(body)))
	if err != nil {
		return chatAPIResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := doOllamaRequest(ctx, a.client, httpReq, agentSSEKeepaliveInterval, keepalive)
	if err != nil {
		return chatAPIResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return chatAPIResponse{}, fmt.Errorf("ollama reasoner returned %s: %s", resp.Status, strings.TrimSpace(string(message)))
	}
	out, err := decodeReasonerResponseWithKeepalive(ctx, resp.Body, keepalive)
	if err != nil {
		return chatAPIResponse{}, err
	}
	if out.Error != "" {
		return chatAPIResponse{}, fmt.Errorf("ollama reasoner: %s", out.Error)
	}
	if len(out.Message.ToolCalls) != 0 {
		return chatAPIResponse{}, fmt.Errorf("reasoner attempted a tool request")
	}
	return out, nil
}

func decodeReasonerResponseWithKeepalive(ctx context.Context, body io.Reader, keepalive func()) (chatAPIResponse, error) {
	type result struct {
		response chatAPIResponse
		err      error
	}
	done := make(chan result, 1)
	go func() {
		var out chatAPIResponse
		err := json.NewDecoder(io.LimitReader(body, 2<<20)).Decode(&out)
		done <- result{response: out, err: err}
	}()
	if keepalive == nil {
		out := <-done
		return out.response, out.err
	}
	ticker := time.NewTicker(agentSSEKeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case out := <-done:
			return out.response, out.err
		case <-ticker.C:
			keepalive()
		case <-ctx.Done():
			return chatAPIResponse{}, ctx.Err()
		}
	}
}
