package main

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
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
	if diagnosticResponseContainsUnsupportedNegativeDeploymentCausality(response) {
		return diagnosticFinalResponse{}, fmt.Errorf("unsupported negative deployment causality")
	}
	if response.Confidence != "confirmed" && diagnosticResponseContainsUnsupportedCausalCertainty(response) {
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

func diagnosticResponseContainsUnsupportedCausalCertainty(response diagnosticFinalResponse) bool {
	for _, value := range diagnosticResponseValues(response) {
		if containsUnsupportedCausalCertainty(value) {
			return true
		}
	}
	return false
}

func diagnosticResponseContainsUnsupportedNegativeDeploymentCausality(response diagnosticFinalResponse) bool {
	for _, value := range diagnosticResponseValues(response) {
		if containsUnsupportedNegativeDeploymentCausality(value) {
			return true
		}
	}
	return false
}

func diagnosticResponseValues(response diagnosticFinalResponse) []string {
	values := []string{response.Conclusion, response.NextCheck}
	values = append(values, response.Working...)
	values = append(values, response.Failing...)
	values = append(values, response.RuledOut...)
	values = append(values, response.LessLikely...)
	values = append(values, response.PossibleCauses...)
	values = append(values, response.KeyEvidence...)
	values = append(values, response.Unknowns...)
	return values
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
	for _, clause := range normalizedCausalClauses(value) {
		for _, marker := range []string{
			"caused by",
			"deployment caused", "deploy caused", "release caused", "rollback caused",
			"deployment was responsible for", "deploy was responsible for",
			"deployment triggered the", "deploy triggered the", "release triggered the", "rollback triggered the",
			"deployment introduced the failure", "deploy introduced the failure",
			"deployment introduced the regression", "deploy introduced the regression",
		} {
			if containsUnqualifiedCausalMarker(clause, marker) {
				return true
			}
		}
		for _, marker := range []string{"the root cause is", "the root cause was", "the cause is", "the cause was"} {
			position := strings.Index(clause, marker)
			if position < 0 || causalClaimHasUncertaintyQualifier(clause, position) {
				continue
			}
			remainder := strings.TrimSpace(clause[position+len(marker):])
			if strings.HasPrefix(remainder, "not established") || strings.HasPrefix(remainder, "unknown") ||
				strings.HasPrefix(remainder, "unclear") || strings.HasPrefix(remainder, "undetermined") {
				continue
			}
			return true
		}
	}
	return false
}

var unsupportedNegativeDeploymentCausalityPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:deploy(?:ment)?|release|rollback|cutover)\b(?:\s+\w+){0,4}\s+(?:(?:did|does|do|is|was|are|were|has|have|had|can|could|would)\s+not|never)(?:\s+\w+){0,3}\s+(?:cause|causes|caused|causing|trigger|triggers|triggered|introduce|introduces|introduced|contribute|contributes|contributed|responsible|the\s+cause|to\s+blame)\b`),
	regexp.MustCompile(`\b(?:is|was|are|were)\s+not\s+caused\s+by(?:\s+\w+){0,4}\s+(?:deploy(?:ment)?|release|rollback|cutover)\b`),
	regexp.MustCompile(`\b(?:can|could)\s+not(?:\s+have)?(?:\s+been)?\s+caused\s+by(?:\s+\w+){0,4}\s+(?:deploy(?:ment)?|release|rollback|cutover)\b`),
	regexp.MustCompile(`\b(?:deploy(?:ment)?|release|rollback|cutover)\b(?:\s+\w+){0,4}\s+(?:cause|causes|caused|trigger|triggers|triggered|introduce|introduces|introduced|create|creates|created)\s+(?:no|zero)\s+(?:issue|issues|problem|problems|outage|outages|failure|failures|regression|regressions|error|errors|symptom|symptoms|harm)\b`),
	regexp.MustCompile(`\b(?:deploy(?:ment)?|release|rollback|cutover)\b(?:\s+\w+){0,3}\s+(?:(?:can|could|may)\s+be|is|was|has\s+been)\s+(?:ruled\s+out|excluded)\b`),
	regexp.MustCompile(`\b(?:we|i|evidence|data|logs|timing)\s+(?:can|could|does|do)\s+(?:rule\s+(?:the\s+)?(?:latest\s+|current\s+)?(?:deploy(?:ment)?|release|rollback|cutover)\s+out|exclude\s+(?:the\s+)?(?:latest\s+|current\s+)?(?:deploy(?:ment)?|release|rollback|cutover))\b`),
	regexp.MustCompile(`\b(?:deploy(?:ment)?|release|rollback|cutover)\b(?:\s+\w+){0,3}\s+(?:played|plays|had|has|bears|bore)\s+(?:no\s+(?:role|part|responsibility)|nothing\s+to\s+do)\b`),
	regexp.MustCompile(`\b(?:deploy(?:ment)?|release|rollback|cutover)\b(?:\s+\w+){0,3}\s+(?:is|was|are|were)\s+(?:entirely\s+|completely\s+)?unrelated\b`),
	regexp.MustCompile(`\b(?:issue|problem|outage|failure|regression|error|symptom|this|it)\b(?:\s+\w+){0,3}\s+(?:is|was|are|were)\s+not\s+due\s+to(?:\s+\w+){0,3}\s+(?:deploy(?:ment)?|release|rollback|cutover)\b`),
}

func containsUnsupportedNegativeDeploymentCausality(value string) bool {
	for _, clause := range normalizedCausalClauses(value) {
		for _, pattern := range unsupportedNegativeDeploymentCausalityPatterns {
			for _, match := range pattern.FindAllStringIndex(clause, -1) {
				if !causalClaimHasUncertaintyQualifier(clause, match[0]) {
					return true
				}
			}
		}
	}
	return false
}

func normalizedCausalClauses(value string) []string {
	lower := strings.ToLower(strings.ReplaceAll(value, "’", "'"))
	lower = strings.NewReplacer(
		"wasn't", "was not",
		"weren't", "were not",
		"isn't", "is not",
		"aren't", "are not",
		"didn't", "did not",
		"doesn't", "does not",
		"don't", "do not",
		"can't", "can not",
		"cannot", "can not",
		"couldn't", "could not",
		"hasn't", "has not",
		"haven't", "have not",
		"hadn't", "had not",
	).Replace(lower)
	rawClauses := strings.FieldsFunc(lower, func(r rune) bool {
		switch r {
		case '.', '!', '?', ';', ',', '\n', '\r':
			return true
		default:
			return false
		}
	})
	clauses := make([]string, 0, len(rawClauses))
	for _, clause := range rawClauses {
		if normalized := strings.Join(strings.Fields(clause), " "); normalized != "" {
			clauses = append(clauses, normalized)
		}
	}
	return clauses
}

func containsUnqualifiedCausalMarker(clause, marker string) bool {
	for offset := 0; offset < len(clause); {
		position := strings.Index(clause[offset:], marker)
		if position < 0 {
			return false
		}
		position += offset
		if !causalClaimHasUncertaintyQualifier(clause, position) {
			return true
		}
		offset = position + len(marker)
	}
	return false
}

func causalClaimHasUncertaintyQualifier(clause string, claimStart int) bool {
	if claimStart <= 0 {
		return false
	}
	prefix := clause[:claimStart]
	return containsAny(
		prefix,
		"does not establish", "do not establish", "did not establish", "not established",
		"can not establish", "could not establish", "unable to establish",
		"does not prove", "do not prove", "did not prove", "not proven",
		"can not prove", "could not prove", "unable to prove",
		"does not show", "do not show", "did not show", "can not show", "could not show",
		"no evidence", "insufficient evidence", "not enough evidence",
		"can not determine", "could not determine", "unable to determine",
		"unknown whether", "unclear whether", "undetermined whether",
		"can not conclude", "could not conclude", "unable to conclude",
		"can not say", "could not say", "would be speculation",
		"may have", "might have", "could have", "possibly", "possible that",
	)
}

func diagnosticResponseContainsNarration(response diagnosticFinalResponse) bool {
	for _, value := range diagnosticResponseValues(response) {
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
