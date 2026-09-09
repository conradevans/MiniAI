package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	diagnosticConclusionMaxRunes = 640
	diagnosticNextCheckMaxRunes  = 320
)

type diagnosticFinalResponse struct {
	Status         string   `json:"status"`
	Confidence     string   `json:"confidence"`
	Conclusion     string   `json:"conclusion"`
	Working        []string `json:"working,omitempty"`
	Failing        []string `json:"failing,omitempty"`
	RuledOut       []string `json:"ruled_out,omitempty"`
	LessLikely     []string `json:"less_likely,omitempty"`
	PossibleCauses []string `json:"possible_causes,omitempty"`
	KeyEvidence    []string `json:"key_evidence,omitempty"`
	Unknowns       []string `json:"unknowns,omitempty"`
	NextCheck      string   `json:"next_check,omitempty"`
}

func diagnosticFinalResponseSchema() map[string]any {
	arrayProperty := func() map[string]any {
		return map[string]any{
			"type": "array", "maxItems": diagnosticAssessmentMaxItems,
			"items": map[string]any{"type": "string", "maxLength": diagnosticAssessmentItemMaxRunes},
		}
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"status", "confidence", "conclusion"},
		"properties": map[string]any{
			"status": map[string]any{
				"type": "string", "enum": []string{"healthy", "unhealthy", "degraded", "unknown"},
			},
			"confidence": map[string]any{
				"type": "string", "enum": []string{"confirmed", "strongly_supported", "plausible", "unknown"},
			},
			"conclusion":      map[string]any{"type": "string", "minLength": 1, "maxLength": diagnosticConclusionMaxRunes},
			"working":         arrayProperty(),
			"failing":         arrayProperty(),
			"ruled_out":       arrayProperty(),
			"less_likely":     arrayProperty(),
			"possible_causes": arrayProperty(),
			"key_evidence":    arrayProperty(),
			"unknowns":        arrayProperty(),
			"next_check":      map[string]any{"type": "string", "maxLength": diagnosticNextCheckMaxRunes},
		},
	}
}

func parseDiagnosticFinalResponse(raw string, assessment diagnosticEvidenceAssessment, allowNextCheck bool) (diagnosticFinalResponse, error) {
	var response diagnosticFinalResponse
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return diagnosticFinalResponse{}, fmt.Errorf("invalid diagnostic JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return diagnosticFinalResponse{}, fmt.Errorf("invalid trailing diagnostic output")
	}

	response.Status = strings.ToLower(strings.TrimSpace(response.Status))
	response.Confidence = strings.ToLower(strings.TrimSpace(response.Confidence))
	response.Conclusion = strings.TrimSpace(truncateDiagnosticRunes(response.Conclusion, diagnosticConclusionMaxRunes))
	if response.Conclusion == "" {
		return diagnosticFinalResponse{}, fmt.Errorf("diagnostic conclusion is required")
	}
	if !validDiagnosticStatus(response.Status) {
		return diagnosticFinalResponse{}, fmt.Errorf("invalid diagnostic status")
	}
	if !validDiagnosticConfidence(response.Confidence) {
		return diagnosticFinalResponse{}, fmt.Errorf("invalid diagnostic confidence")
	}
	if assessment.Status == "unknown" && response.Status != "unknown" {
		return diagnosticFinalResponse{}, fmt.Errorf("model status exceeds unknown structured evidence")
	}
	if assessment.Status != "" && assessment.Status != "unknown" && response.Status != assessment.Status {
		return diagnosticFinalResponse{}, fmt.Errorf("model status conflicts with structured evidence")
	}
	response.Confidence = lowerDiagnosticConfidence(response.Confidence, assessment.ConfidenceCeiling)
	if response.Confidence != "confirmed" && containsUnsupportedCausalCertainty(response.Conclusion) {
		return diagnosticFinalResponse{}, fmt.Errorf("unsupported causal certainty")
	}

	modelPossibleCauses := response.PossibleCauses
	if len(assessment.Failing) == 0 && len(assessment.PossibleCauses) == 0 {
		modelPossibleCauses = nil
	}
	response.PossibleCauses = boundedDiagnosticStrings(
		append(append([]string{}, assessment.PossibleCauses...), modelPossibleCauses...),
		diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes,
	)
	response.Working = boundedDiagnosticStrings(assessment.Working, diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes)
	response.Failing = boundedDiagnosticStrings(assessment.Failing, diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes)
	response.RuledOut = boundedDiagnosticStrings(assessment.RuledOut, diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes)
	response.LessLikely = boundedDiagnosticStrings(assessment.LessLikely, diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes)
	response.KeyEvidence = boundedDiagnosticStrings(assessment.KeyEvidence, diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes)
	response.Unknowns = boundedDiagnosticStrings(assessment.Unknowns, diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes)
	response.NextCheck = strings.TrimSpace(truncateDiagnosticRunes(response.NextCheck, diagnosticNextCheckMaxRunes))
	if !allowNextCheck {
		response.NextCheck = ""
	}
	if diagnosticResponseContainsNarration(response) {
		return diagnosticFinalResponse{}, fmt.Errorf("diagnostic response contains internal narration")
	}
	return response, nil
}

func validDiagnosticStatus(value string) bool {
	switch value {
	case "healthy", "unhealthy", "degraded", "unknown":
		return true
	default:
		return false
	}
}

func validDiagnosticConfidence(value string) bool {
	switch value {
	case "confirmed", "strongly_supported", "plausible", "unknown":
		return true
	default:
		return false
	}
}

func lowerDiagnosticConfidence(model, ceiling string) string {
	rank := map[string]int{"unknown": 0, "plausible": 1, "strongly_supported": 2, "confirmed": 3}
	if !validDiagnosticConfidence(ceiling) {
		ceiling = "unknown"
	}
	if rank[model] > rank[ceiling] {
		return ceiling
	}
	return model
}

func containsUnsupportedCausalCertainty(value string) bool {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "caused by") {
		return true
	}
	for _, marker := range []string{"the root cause is", "the root cause was", "the cause is", "the cause was"} {
		position := strings.Index(lower, marker)
		if position < 0 {
			continue
		}
		remainder := strings.TrimSpace(lower[position+len(marker):])
		if strings.HasPrefix(remainder, "not established") || strings.HasPrefix(remainder, "unknown") ||
			strings.HasPrefix(remainder, "unclear") || strings.HasPrefix(remainder, "undetermined") {
			continue
		}
		return true
	}
	return false
}

func diagnosticResponseContainsNarration(response diagnosticFinalResponse) bool {
	values := []string{response.Conclusion, response.NextCheck}
	values = append(values, response.Working...)
	values = append(values, response.Failing...)
	values = append(values, response.RuledOut...)
	values = append(values, response.LessLikely...)
	values = append(values, response.PossibleCauses...)
	values = append(values, response.KeyEvidence...)
	values = append(values, response.Unknowns...)
	for _, value := range values {
		if hasObviousDiagnosticNarrationPrefix(value) {
			return true
		}
	}
	return false
}

func allowDiagnosticNextCheck(packet diagnosticEvidencePacket) bool {
	return len(packet.Unavailable) > 0 || packet.Investigation.BudgetExhausted ||
		(packet.Assessment.Status != "healthy" && packet.Assessment.ConfidenceCeiling == "unknown")
}

func renderDiagnosticFinalResponse(response diagnosticFinalResponse) string {
	var output strings.Builder
	output.WriteString(response.Conclusion)
	output.WriteString("\n\n**Confidence:** " + diagnosticConfidenceLabel(response.Confidence))
	appendDiagnosticMarkdownSection(&output, "What is working", response.Working)
	appendDiagnosticMarkdownSection(&output, "What is failing", response.Failing)
	appendDiagnosticMarkdownSection(&output, "What this rules out", response.RuledOut)
	appendDiagnosticMarkdownSection(&output, "What is less likely", response.LessLikely)
	appendDiagnosticMarkdownSection(&output, "What remains possible", response.PossibleCauses)
	appendDiagnosticMarkdownSection(&output, "Key evidence", response.KeyEvidence)
	appendDiagnosticMarkdownSection(&output, "What remains unknown", response.Unknowns)
	if response.NextCheck != "" {
		output.WriteString("\n\n### Next useful check\n\n")
		output.WriteString(response.NextCheck)
	}
	return output.String()
}

func appendDiagnosticMarkdownSection(output *strings.Builder, title string, values []string) {
	if len(values) == 0 {
		return
	}
	output.WriteString("\n\n### " + title + "\n")
	for _, value := range values {
		output.WriteString("\n- " + value)
	}
}

func diagnosticConfidenceLabel(value string) string {
	switch value {
	case "confirmed":
		return "Confirmed"
	case "strongly_supported":
		return "Strongly supported"
	case "plausible":
		return "Plausible"
	default:
		return "Unknown"
	}
}
