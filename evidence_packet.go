package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

const evidencePacketSchemaVersion = "phase2b.packet.v1"

var errEvidencePacketTooLarge = errors.New("phase2b critical evidence exceeds packet hard limit")

type EvidencePacket struct {
	SchemaVersion   string                     `json:"schemaVersion"`
	Question        string                     `json:"question"`
	Route           InvestigationRouteID       `json:"route"`
	Goal            InvestigationGoal          `json:"goal"`
	Subjects        []EvidenceSubject          `json:"subjects"`
	Temporal        TemporalScope              `json:"temporal"`
	Sources         []EvidencePacketSource     `json:"sources"`
	Evidence        []EvidencePacketItem       `json:"evidence"`
	Derivations     []EvidencePacketDerivation `json:"derivations,omitempty"`
	Missing         []MissingEvidence          `json:"missing,omitempty"`
	Conflicts       []EvidenceConflict         `json:"conflicts,omitempty"`
	Confidence      ConfidenceDecision         `json:"confidence"`
	Compacted       bool                       `json:"compacted"`
	DroppedEvidence int                        `json:"droppedEvidence,omitempty"`
}

type EvidencePacketSource struct {
	ID           string             `json:"id"`
	Capability   string             `json:"capability"`
	Resource     string             `json:"resource,omitempty"`
	CollectedAt  time.Time          `json:"collectedAt"`
	Availability Availability       `json:"availability"`
	Truncation   EvidenceTruncation `json:"truncation"`
}

type EvidencePacketItem struct {
	ID            string                 `json:"id"`
	Kind          EvidenceKind           `json:"kind"`
	Type          EvidenceType           `json:"type"`
	Subject       EvidenceSubject        `json:"subject"`
	Timestamp     *time.Time             `json:"timestamp,omitempty"`
	Window        *TemporalScope         `json:"window,omitempty"`
	SourceID      string                 `json:"sourceId,omitempty"`
	RequirementID string                 `json:"requirementId"`
	Criticality   RequirementCriticality `json:"criticality"`
	Fields        []EvidenceField        `json:"fields"`
}

type EvidencePacketDerivation struct {
	Operation   string   `json:"operation"`
	PolicyID    string   `json:"policyId,omitempty"`
	Inputs      []string `json:"inputs"`
	OutputID    string   `json:"outputId"`
	Explanation string   `json:"explanation,omitempty"`
}

type EvidencePacketSizeError struct {
	Bytes int
	Limit int
}

func (err EvidencePacketSizeError) Error() string {
	return fmt.Sprintf("%v: %d bytes exceeds %d", errEvidencePacketTooLarge, err.Bytes, err.Limit)
}

func (err EvidencePacketSizeError) Unwrap() error { return errEvidencePacketTooLarge }

// BuildEvidencePacket creates a deterministic model-ready representation. It
// excludes result envelopes, raw tool payloads, tool schemas, planner text,
// raw logs, and raw historical metric arrays by construction.
func BuildEvidencePacket(evidence InvestigationEvidence) (EvidencePacket, error) {
	normalized, err := NormalizeInvestigationEvidence(evidence)
	if err != nil {
		return EvidencePacket{}, err
	}
	if !normalized.Confidence.Determined {
		normalized.Confidence = DecideEvidenceConfidence(normalized)
	}
	packet := EvidencePacket{
		SchemaVersion: evidencePacketSchemaVersion,
		Question:      normalized.NormalizedQuestion,
		Route:         normalized.Route.ID,
		Goal:          normalized.Route.Frame.Goal,
		Subjects:      append([]EvidenceSubject(nil), normalized.Route.Frame.Subjects...),
		Temporal:      normalized.Route.Frame.Temporal,
		Sources:       make([]EvidencePacketSource, 0, len(normalized.Sources)),
		Evidence:      make([]EvidencePacketItem, 0, len(normalized.Items)),
		Derivations:   make([]EvidencePacketDerivation, 0, len(normalized.Derivations)),
		Missing:       append([]MissingEvidence(nil), normalized.Missing...),
		Conflicts:     append([]EvidenceConflict(nil), normalized.Conflicts...),
		Confidence:    normalized.Confidence,
	}
	for _, source := range normalized.Sources {
		packet.Sources = append(packet.Sources, EvidencePacketSource{
			ID: source.ID, Capability: source.Capability, Resource: source.Resource,
			CollectedAt: source.CollectedAt.UTC(), Availability: source.Availability, Truncation: source.Truncation,
		})
	}
	for _, item := range normalized.Items {
		packet.Evidence = append(packet.Evidence, EvidencePacketItem{
			ID: item.ID, Kind: item.Kind, Type: item.Type, Subject: item.Subject, Timestamp: item.Timestamp, Window: item.Window,
			SourceID: item.SourceID, RequirementID: item.Relevance.RequirementID,
			Criticality: item.Relevance.Criticality, Fields: canonicalEvidenceFields(item.Payload.Fields),
		})
	}
	for _, derivation := range normalized.Derivations {
		packet.Derivations = append(packet.Derivations, EvidencePacketDerivation{
			Operation: derivation.Operation, PolicyID: derivation.PolicyID, Inputs: append([]string(nil), derivation.Inputs...),
			OutputID: derivation.OutputID, Explanation: derivation.Explanation,
		})
	}
	canonicalizeEvidencePacket(&packet)
	encoded, err := json.Marshal(packet)
	if err != nil {
		return EvidencePacket{}, err
	}
	if len(encoded) <= evidencePacketTargetBytes {
		return packet, nil
	}

	packet.Compacted = true
	protected := packetProtectedEvidenceIDs(packet)
	for _, criticality := range []RequirementCriticality{CriticalitySupporting, CriticalityRelevant} {
		for {
			index := packetRemovalCandidate(packet.Evidence, protected, criticality)
			if index < 0 {
				break
			}
			packet.Evidence = append(packet.Evidence[:index], packet.Evidence[index+1:]...)
			packet.DroppedEvidence++
			pruneEvidencePacketReferences(&packet)
			encoded, err = json.Marshal(packet)
			if err != nil {
				return EvidencePacket{}, err
			}
			if len(encoded) <= evidencePacketTargetBytes {
				return packet, nil
			}
		}
	}
	pruneEvidencePacketReferences(&packet)
	encoded, err = json.Marshal(packet)
	if err != nil {
		return EvidencePacket{}, err
	}
	if len(encoded) > evidencePacketHardLimitBytes {
		return EvidencePacket{}, EvidencePacketSizeError{Bytes: len(encoded), Limit: evidencePacketHardLimitBytes}
	}
	return packet, nil
}

func MarshalEvidencePacket(packet EvidencePacket) ([]byte, error) {
	canonicalizeEvidencePacket(&packet)
	encoded, err := json.Marshal(packet)
	if err != nil {
		return nil, err
	}
	if len(encoded) > evidencePacketHardLimitBytes {
		return nil, EvidencePacketSizeError{Bytes: len(encoded), Limit: evidencePacketHardLimitBytes}
	}
	return encoded, nil
}

func canonicalizeEvidencePacket(packet *EvidencePacket) {
	packet.Subjects = append([]EvidenceSubject(nil), packet.Subjects...)
	sort.Slice(packet.Subjects, func(i, j int) bool { return canonicalJSON(packet.Subjects[i]) < canonicalJSON(packet.Subjects[j]) })
	sort.Slice(packet.Sources, func(i, j int) bool { return packet.Sources[i].ID < packet.Sources[j].ID })
	for index := range packet.Evidence {
		packet.Evidence[index].Fields = canonicalEvidenceFields(packet.Evidence[index].Fields)
	}
	sort.Slice(packet.Evidence, func(i, j int) bool { return packet.Evidence[i].ID < packet.Evidence[j].ID })
	for index := range packet.Derivations {
		packet.Derivations[index].Inputs = sortedUniqueContractStrings(packet.Derivations[index].Inputs)
	}
	sort.Slice(packet.Derivations, func(i, j int) bool {
		if packet.Derivations[i].OutputID != packet.Derivations[j].OutputID {
			return packet.Derivations[i].OutputID < packet.Derivations[j].OutputID
		}
		return packet.Derivations[i].Operation < packet.Derivations[j].Operation
	})
	sort.Slice(packet.Missing, func(i, j int) bool { return canonicalJSON(packet.Missing[i]) < canonicalJSON(packet.Missing[j]) })
	sort.Slice(packet.Conflicts, func(i, j int) bool { return packet.Conflicts[i].ID < packet.Conflicts[j].ID })
	packet.Confidence.Reasons = sortedUniqueContractStrings(packet.Confidence.Reasons)
}

func packetProtectedEvidenceIDs(packet EvidencePacket) map[string]bool {
	protected := map[string]bool{}
	for _, conflict := range packet.Conflicts {
		for _, evidenceID := range conflict.EvidenceIDs {
			protected[evidenceID] = true
		}
	}
	for _, derivation := range packet.Derivations {
		if !packetMaterialDerivation(derivation.Operation) {
			continue
		}
		protected[derivation.OutputID] = true
		for _, inputID := range derivation.Inputs {
			protected[inputID] = true
		}
	}
	for _, item := range packet.Evidence {
		if packetConfidenceLimitingItem(item) || packetMaterialItem(packet.Route, item) {
			protected[item.ID] = true
		}
	}
	return protected
}

func packetMaterialDerivation(operation string) bool {
	switch operation {
	case "temporal_alignment", "outage_interval":
		return true
	default:
		return false
	}
}

func packetMaterialItem(route InvestigationRouteID, item EvidencePacketItem) bool {
	if item.Criticality != CriticalityRelevant {
		return false
	}
	fields := packetEvidenceFieldNames(item.Fields)
	hasExactField := func(names ...string) bool {
		for _, name := range names {
			if fields[name] {
				return true
			}
		}
		return false
	}
	switch route {
	case RouteDeploymentCorrelation:
		switch item.Type {
		case EvidenceTypeDeploymentTimeline, EvidenceTypeCurrentApplication, EvidenceTypeCurrentDeployment, EvidenceTypeCurrentDeploymentList:
			return hasExactField("commit_sha", "image_id", "activated_at", "correlation_activated_at", "deployment_time") || item.Timestamp != nil
		case EvidenceTypeInfrastructureEvents, EvidenceTypeActivityTimeline:
			return hasExactField("event_time", "outage_start", "recovered_at") || item.Timestamp != nil
		}
	case RouteRestartInvestigation:
		if item.Type == EvidenceTypeRecoveryTimeline || item.Type == EvidenceTypeInfrastructureEvents || item.Type == EvidenceTypeActivityTimeline {
			return hasExactField("event_time", "outage_start", "recovered_at") || item.Timestamp != nil
		}
	case RouteExactCurrentFact:
		return isCurrentEvidenceType(item.Type) && (hasExactField("commit_sha", "image_id", "activated_at", "status") || item.Timestamp != nil)
	}
	return false
}

func packetConfidenceLimitingItem(item EvidencePacketItem) bool {
	if item.Criticality == CriticalitySupporting {
		return false
	}
	for _, field := range item.Fields {
		switch field.Name {
		case "coverage_incomplete", "source_truncated", "snippet_truncated", "truncated", "correlation_ambiguous":
			if field.Value.Boolean != nil && *field.Value.Boolean {
				return true
			}
		case "database_coverage_complete":
			if field.Value.Boolean != nil && !*field.Value.Boolean {
				return true
			}
		case "reducer_omitted", "unfilterable_lines", "database_lookup_failure_count":
			if field.Value.Integer != nil && *field.Value.Integer > 0 {
				return true
			}
		}
	}
	return false
}

func packetEvidenceFieldNames(fields []EvidenceField) map[string]bool {
	names := make(map[string]bool, len(fields))
	for _, field := range fields {
		names[field.Name] = true
	}
	return names
}

func packetRemovalCandidate(items []EvidencePacketItem, protected map[string]bool, criticality RequirementCriticality) int {
	// Remove deterministic low-value derived facts before direct observations,
	// and remove from the stable tail so input permutations cannot affect it.
	for _, kind := range []EvidenceKind{EvidenceDerived, EvidenceObserved, EvidenceInferred} {
		for index := len(items) - 1; index >= 0; index-- {
			item := items[index]
			if item.Criticality == criticality && item.Kind == kind && !protected[item.ID] {
				return index
			}
		}
	}
	return -1
}

func pruneEvidencePacketReferences(packet *EvidencePacket) {
	presentItems := make(map[string]bool, len(packet.Evidence))
	usedSources := map[string]bool{}
	for _, item := range packet.Evidence {
		presentItems[item.ID] = true
		if item.SourceID != "" {
			usedSources[item.SourceID] = true
		}
	}
	derivations := packet.Derivations[:0]
	for _, derivation := range packet.Derivations {
		if !presentItems[derivation.OutputID] {
			continue
		}
		allInputsPresent := true
		for _, inputID := range derivation.Inputs {
			if !presentItems[inputID] {
				allInputsPresent = false
				break
			}
		}
		if allInputsPresent {
			derivations = append(derivations, derivation)
		}
	}
	packet.Derivations = derivations
	sources := packet.Sources[:0]
	for _, source := range packet.Sources {
		if usedSources[source.ID] {
			sources = append(sources, source)
		}
	}
	packet.Sources = sources
	canonicalizeEvidencePacket(packet)
}
