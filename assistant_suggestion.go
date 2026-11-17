package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const reasonerRestartCurrentApplication = "restart_current_application"

var errInvalidReasonerActionSuggestion = errors.New("invalid reasoner action suggestion")

type reasonerActionSuggestion struct {
	Type        string   `json:"type"`
	Reason      string   `json:"reason"`
	EvidenceIDs []string `json:"evidence_ids"`
}

type validatedReasonerActionSuggestion struct {
	Target ActionTarget
	Reason string
}

type assistantActionSuggestionResponse struct {
	ActionID             ActionID     `json:"action_id"`
	Kind                 ActionKind   `json:"kind"`
	Target               ActionTarget `json:"target"`
	State                ActionState  `json:"state"`
	ConfirmationRequired bool         `json:"confirmation_required"`
	WarningRequired      bool         `json:"warning_required"`
	SupportsExecution    bool         `json:"supports_execution"`
	Reason               string       `json:"reason"`
}

func reasonerActionSuggestionSchema(packet EvidencePacket) map[string]any {
	if !reasonerActionSuggestionEligible(packet) {
		return map[string]any{"type": "null"}
	}
	references := reasonerActionEvidenceReferenceEnum(packet)
	return map[string]any{
		"anyOf": []any{
			map[string]any{"type": "null"},
			map[string]any{
				"type": "object", "additionalProperties": false,
				"required": []string{"type", "reason", "evidence_ids"},
				"properties": map[string]any{
					"type": map[string]any{"type": "string", "enum": []string{reasonerRestartCurrentApplication}},
					"reason": map[string]any{
						"type": "string", "minLength": 1, "maxLength": reasonerActionReasonMaxRunes, "pattern": `\S`,
					},
					"evidence_ids": map[string]any{
						"type": "array", "minItems": 1, "maxItems": 6, "uniqueItems": true,
						"items": map[string]any{"type": "string", "enum": references},
					},
				},
			},
		},
	}
}

func reasonerActionSuggestionInstruction(packet EvidencePacket) string {
	const triggerRule = "A restart suggestion is allowed only when the supplied trusted current application evidence establishes a restart-worthy current state. A currently running or healthy application must not receive a restart suggestion merely because the user asks what should happen next, historical failures exist, logs contain errors, or a restart might theoretically be harmless."
	if !reasonerActionSuggestionEligible(packet) {
		return "Do not suggest an action for this investigation; omit action_suggestion or set it to null. " + triggerRule
	}
	return "The normal evidence answer is primary. " + triggerRule + " action_suggestion is optional: omit it or use null unless that trusted current-state evidence directly supports a restart as a reasonable recovery step. The only allowed suggestion type is restart_current_application with one concise user-facing reason and supporting evidence_ids. It is only a proposal and cannot execute, authorize, or confirm anything. Never emit a target, action kind, arguments, command, shell, systemctl or Docker instruction, URL, endpoint, port, origin, state, safety metadata, confirmation setting, or execution setting. If evidence is incomplete, conflicted, too uncertain, or does not support restart, do not suggest one."
}

func reasonerActionSuggestionEligible(packet EvidencePacket) bool {
	if _, references, ok := reasonerRestartTriggerEvidence(packet); !ok || len(references) == 0 {
		return false
	}
	if packet.Confidence.Level != ConfidenceHigh && packet.Confidence.Level != ConfidenceMedium {
		return false
	}
	ceiling := packet.Confidence.SoftwareCeiling
	if ceiling == "" {
		ceiling = packet.Confidence.Level
	}
	if ceiling != ConfidenceHigh && ceiling != ConfidenceMedium {
		return false
	}
	return packet.Confidence.Inputs.CriticalCompleteness == CompletenessComplete
}

func trustedReasonerRestartTarget(packet EvidencePacket) (ActionTarget, bool) {
	switch packet.Route {
	case RouteApplicationCurrent, RouteApplicationPerformance:
	default:
		return ActionTarget{}, false
	}
	if len(packet.Subjects) != 1 {
		return ActionTarget{}, false
	}
	subject := packet.Subjects[0]
	if subject.Kind != SubjectApplication || subject.ID != strings.TrimSpace(subject.ID) ||
		!validActionApplicationTargetID(subject.ID) {

		return ActionTarget{}, false
	}
	return ActionTarget{Type: ActionTargetApplication, ID: subject.ID}, true
}

func reasonerActionEvidenceReferenceEnum(packet EvidencePacket) []string {
	_, references, ok := reasonerRestartTriggerEvidence(packet)
	if !ok {
		return nil
	}
	return references
}

// Compaction intentionally elides a repeated item subject. In that case the closed
// application evidence type plus the packet single-target invariant retains binding.
func reasonerEvidenceBindsRestartTarget(item EvidencePacketItem, target ActionTarget) bool {
	switch item.Subject.Kind {
	case "", SubjectUnknown:
		return item.Subject.ID == ""
	case SubjectApplication:
		return item.Subject.ID == target.ID
	default:
		return false
	}
}

// reasonerRestartTriggerEvidence derives restart eligibility only from trusted,
// typed current-state evidence. Historical symptoms and model-authored text are
// intentionally excluded from this authorization input.
func reasonerRestartTriggerEvidence(packet EvidencePacket) (ActionTarget, []string, bool) {
	target, ok := trustedReasonerRestartTarget(packet)
	if !ok {
		return ActionTarget{}, nil, false
	}
	references := make([]string, 0, len(packet.Evidence))
	currentState := ""
	for _, item := range packet.Evidence {
		if item.Type != EvidenceTypeCurrentApplication {
			continue
		}
		if !reasonerEvidenceBindsRestartTarget(item, target) {
			return ActionTarget{}, nil, false
		}
		status, valid := reasonerCurrentApplicationStatus(item)
		if strings.TrimSpace(item.ID) == "" || item.Kind != EvidenceObserved || !valid || !reasonerRestartWorthyCurrentStatus(status) {
			return ActionTarget{}, nil, false
		}
		if currentState != "" && currentState != status {
			return ActionTarget{}, nil, false
		}
		currentState = status
		references = append(references, item.ID)
	}
	references = sortedUniqueContractStrings(references)
	if currentState == "" || len(references) == 0 {
		return ActionTarget{}, nil, false
	}
	return target, references, true
}

func reasonerCurrentApplicationStatus(item EvidencePacketItem) (string, bool) {
	status := ""
	found := false
	for _, field := range item.Fields {
		if field.Name != "status" {
			continue
		}
		if found || field.Value.Kind != EvidenceValueString {
			return "", false
		}
		status = strings.ToLower(strings.TrimSpace(field.Value.String))
		if status == "" || utf8.RuneCountInString(status) > evidencePacketStatusRunes {
			return "", false
		}
		found = true
	}
	return status, found
}

func reasonerRestartWorthyCurrentStatus(status string) bool {
	switch status {
	case "exited":
		return true
	default:
		return false
	}
}

func validateReasonerActionSuggestion(
	raw json.RawMessage,
	packet EvidencePacket,
) (*validatedReasonerActionSuggestion, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	if !reasonerActionSuggestionEligible(packet) {
		return nil, errInvalidReasonerActionSuggestion
	}
	if err := rejectDuplicateReasonerJSONKeys(trimmed); err != nil {
		return nil, errInvalidReasonerActionSuggestion
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errInvalidReasonerActionSuggestion
	}
	if err := requireExactReasonerKeys(object, "type", "reason", "evidence_ids"); err != nil {
		return nil, errInvalidReasonerActionSuggestion
	}
	var suggestion reasonerActionSuggestion
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&suggestion); err != nil {
		return nil, errInvalidReasonerActionSuggestion
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errInvalidReasonerActionSuggestion
	}
	if suggestion.Type != reasonerRestartCurrentApplication {
		return nil, errInvalidReasonerActionSuggestion
	}

	reason := strings.TrimSpace(suggestion.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > reasonerActionReasonMaxRunes {
		return nil, errInvalidReasonerActionSuggestion
	}
	for _, value := range reason {
		if unicode.IsControl(value) {
			return nil, errInvalidReasonerActionSuggestion
		}
	}
	if _, redacted := scrubSensitiveText(reason); redacted || reasonerActionReasonContainsExecutableText(reason) {
		return nil, errInvalidReasonerActionSuggestion
	}
	if err := validateReasonerText(reason, packet); err != nil {
		return nil, errInvalidReasonerActionSuggestion
	}

	target, triggerEvidence, ok := reasonerRestartTriggerEvidence(packet)
	if !ok {
		return nil, errInvalidReasonerActionSuggestion
	}
	allowed := make(map[string]bool, len(triggerEvidence))
	for _, evidenceID := range triggerEvidence {
		allowed[evidenceID] = true
	}
	if err := validateReasonerReferences(suggestion.EvidenceIDs, allowed, 6); err != nil {
		return nil, errInvalidReasonerActionSuggestion
	}
	return &validatedReasonerActionSuggestion{
		Target: target,
		Reason: strings.Join(strings.Fields(reason), " "),
	}, nil
}

func reasonerActionReasonContainsExecutableText(reason string) bool {
	normalized := strings.ToLower(reason)
	for _, forbidden := range []string{
		"systemctl", "docker ", "docker-compose", "kubectl", "curl ", "wget ",
		"http://", "https://", "/api/", "powershell", "cmd.exe", "sudo ",
	} {
		if strings.Contains(normalized, forbidden) {
			return true
		}
	}
	return false
}

func (a *app) persistReasonerActionSuggestion(
	historyEntryID string,
	draft ReasonerDraft,
	packet EvidencePacket,
) (*assistantActionSuggestionResponse, error) {
	suggestion, err := validateReasonerActionSuggestion(draft.ActionSuggestion, packet)
	if err != nil || suggestion == nil {
		return nil, err
	}
	if a == nil || a.store == nil || historyEntryID == "" {
		return nil, errActionHistoryLink
	}
	record, err := a.store.createAssistantRestartSuggestion(historyEntryID, suggestion.Target)
	if err != nil {
		return nil, err
	}
	response, err := newAssistantActionSuggestionResponse(record, suggestion.Reason)
	if err != nil {
		return nil, err
	}
	return &response, nil
}

func newAssistantActionSuggestionResponse(
	record storedActionProposal,
	reason string,
) (assistantActionSuggestionResponse, error) {
	proposal := record.Proposal
	definition, ok := phase4ActionCatalog().lookup(proposal.Kind())
	if !ok {
		return assistantActionSuggestionResponse{}, errInvalidPersistedAction
	}
	decision, err := phase4ActionCatalog().evaluate(proposal)
	if err != nil {
		return assistantActionSuggestionResponse{}, err
	}
	target := proposal.Target()
	if proposal.Kind() != ActionKindRestartApplication ||
		proposal.Origin() != ActionOriginAssistantSuggested ||
		target.Type != ActionTargetApplication ||
		!validActionApplicationTargetID(target.ID) ||
		proposal.State() != ActionStateAwaitingConfirmation ||
		definition.SafetyClass != ActionSafetyRecoverable ||
		!definition.SupportsExecution || !record.SupportsExecution || record.HistoryEntryID == "" ||
		!decision.ConfirmationRequired || decision.WarningRequired {

		return assistantActionSuggestionResponse{}, errInvalidPersistedAction
	}
	if _, ok := proposal.Arguments().(RestartApplicationArguments); !ok {
		return assistantActionSuggestionResponse{}, errInvalidPersistedAction
	}
	return assistantActionSuggestionResponse{
		ActionID: proposal.ID(), Kind: proposal.Kind(), Target: target,
		State: proposal.State(), ConfirmationRequired: decision.ConfirmationRequired,
		WarningRequired: decision.WarningRequired, SupportsExecution: record.SupportsExecution,
		Reason: reason,
	}, nil
}

func safeAssistantSuggestionError(err error) string {
	if errors.Is(err, errInvalidReasonerActionSuggestion) {
		return "invalid structured suggestion"
	}
	if errors.Is(err, errActionHistoryLink) || errors.Is(err, errActionPersistenceConflict) {
		return err.Error()
	}
	return fmt.Sprintf("persistence failure (%T)", err)
}
