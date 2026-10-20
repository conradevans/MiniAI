package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

const (
	evidencePacketMaxSubjects            = 8
	evidencePacketMaxCriticalItems       = 12
	evidencePacketMaxRelevantItems       = 12
	evidencePacketMaxSupportingItems     = 8
	evidencePacketMaxFieldsPerItem       = 16
	evidencePacketMaxSources             = 32
	evidencePacketMaxDerivations         = 12
	evidencePacketMaxMissing             = 8
	evidencePacketMaxConflicts           = 4
	evidencePacketMaxConflictEvidenceIDs = 6
	evidencePacketMaxJSONCollection      = 8
	evidencePacketSubjectRunes           = 64
	evidencePacketIdentifierRunes        = 96
	evidencePacketResourceRunes          = 120
	evidencePacketFieldNameRunes         = 64
	evidencePacketStringRunes            = 116
	evidencePacketStatusRunes            = 32
	evidencePacketRepositoryRunes        = 80
	evidencePacketJSONFieldRunes         = 512
	evidencePacketExplanationRunes       = 120
	evidencePacketLimitationCodeRunes    = 64
	evidencePacketMissingReasonRunes     = 96
	evidencePacketConfidenceReasonRunes  = 96
	evidencePacketTemporalReasonRunes    = 80
)

type evidenceFieldPriority uint8

const (
	evidenceFieldRequired evidenceFieldPriority = iota
	evidenceFieldSecondary
	evidenceFieldRemovable
)

func projectEvidencePacket(packet *EvidencePacket) error {
	packet.Subjects = boundPacketSubjects(packet.Subjects, evidencePacketMaxSubjects)
	packet.Temporal.NamedRange = boundPacketText(packet.Temporal.NamedRange, 16)
	packet.Temporal.Reason = boundPacketText(packet.Temporal.Reason, evidencePacketTemporalReasonRunes)

	for index := range packet.Sources {
		source := &packet.Sources[index]
		source.ID = boundPacketText(source.ID, evidencePacketIdentifierRunes)
		source.Capability = boundPacketText(source.Capability, evidencePacketIdentifierRunes)
		source.Resource = boundPacketText(source.Resource, evidencePacketResourceRunes)
		if source.Resource == "safe "+source.Capability {
			source.Resource = ""
		}
		if !source.Truncation.Truncated {
			source.Truncation = EvidenceTruncation{}
		}
	}

	critical, relevant, supporting := 0, 0, 0
	for index := range packet.Evidence {
		item := &packet.Evidence[index]
		item.ID = boundPacketText(item.ID, evidencePacketIdentifierRunes)
		item.SourceID = boundPacketText(item.SourceID, evidencePacketIdentifierRunes)
		item.RequirementID = boundPacketText(item.RequirementID, evidencePacketIdentifierRunes)
		item.Subject = boundPacketSubject(item.Subject)
		if len(packet.Subjects) == 1 && canonicalJSON(item.Subject) == canonicalJSON(packet.Subjects[0]) {
			item.Subject = EvidenceSubject{}
		}
		if item.Window != nil {
			item.Window.NamedRange = boundPacketText(item.Window.NamedRange, 16)
			item.Window.Reason = boundPacketText(item.Window.Reason, evidencePacketTemporalReasonRunes)
			if isCurrentEvidenceType(item.Type) || canonicalJSON(*item.Window) == canonicalJSON(packet.Temporal) {
				item.Window = nil
			}
		}
		projectedFields, irreducible := projectEvidenceFields(item, item.Fields)
		if irreducible {
			return EvidencePacketSizeError{Bytes: evidencePacketHardLimitBytes + 1, Limit: evidencePacketHardLimitBytes}
		}
		item.Fields = projectedFields
		switch item.Criticality {
		case CriticalityCritical:
			critical++
		case CriticalityRelevant:
			relevant++
		case CriticalitySupporting:
			supporting++
		}
	}
	if critical > evidencePacketMaxCriticalItems {
		return EvidencePacketSizeError{Bytes: evidencePacketHardLimitBytes + 1, Limit: evidencePacketHardLimitBytes}
	}
	packet.Evidence, packet.DroppedEvidence = boundPacketEvidence(packet.Evidence, packet.DroppedEvidence)
	if relevant > evidencePacketMaxRelevantItems || supporting > evidencePacketMaxSupportingItems {
		packet.Compacted = true
	}

	packet.Missing, packet.MissingOmitted = boundPacketMissing(packet.Missing)
	if packet.MissingOmitted != (EvidencePacketMissingOmission{}) {
		packet.Compacted = true
	}
	packet.Conflicts = boundPacketConflicts(packet.Conflicts)
	for index := range packet.Derivations {
		derivation := &packet.Derivations[index]
		derivation.Operation = boundPacketText(derivation.Operation, evidencePacketIdentifierRunes)
		derivation.PolicyID = boundPacketText(derivation.PolicyID, evidencePacketIdentifierRunes)
		derivation.OutputID = boundPacketText(derivation.OutputID, evidencePacketIdentifierRunes)
		derivation.Inputs = boundPacketStrings(derivation.Inputs, evidencePacketMaxConflictEvidenceIDs, evidencePacketIdentifierRunes)
		derivation.Explanation = boundPacketText(derivation.Explanation, evidencePacketExplanationRunes)
	}
	if len(packet.Derivations) > evidencePacketMaxDerivations {
		packet.Derivations = packet.Derivations[:evidencePacketMaxDerivations]
		packet.Compacted = true
	}
	packet.Confidence.Reasons = boundPacketStrings(packet.Confidence.Reasons, evidencePacketMaxMissing, evidencePacketConfidenceReasonRunes)
	if packet.Confidence.SoftwareCeiling == packet.Confidence.Level {
		packet.Confidence.SoftwareCeiling = ""
	}
	canonicalizeEvidencePacket(packet)
	pruneEvidencePacketReferences(packet)
	if len(packet.Sources) > evidencePacketMaxSources {
		return EvidencePacketSizeError{Bytes: evidencePacketHardLimitBytes + 1, Limit: evidencePacketHardLimitBytes}
	}
	compactPacketReferences(packet)
	return nil
}

func compactEvidencePacketToTarget(packet *EvidencePacket) error {
	encoded, err := marshalEvidencePacketJSON(*packet)
	if err != nil || len(encoded) <= evidencePacketTargetBytes {
		return err
	}
	packet.Compacted = true

	if done, compactErr := compactPacketFieldsToLimit(packet, evidenceFieldRemovable, evidencePacketTargetBytes); done || compactErr != nil {
		return compactErr
	}
	protected := packetProtectedEvidenceIDs(*packet)
	if done, compactErr := compactPacketItemsToLimit(packet, protected, CriticalityRelevant, false, evidencePacketTargetBytes); done || compactErr != nil {
		return compactErr
	}
	if done, compactErr := compactPacketFieldsToLimit(packet, evidenceFieldSecondary, evidencePacketTargetBytes); done || compactErr != nil {
		return compactErr
	}
	compactSecondaryPacketTimestamps(packet)
	compactDerivedPacketMetadata(packet)
	compactDeterministicDerivationMetadata(packet)
	packet.Confidence.Reasons = nil
	encoded, err = marshalEvidencePacketJSON(*packet)
	if err != nil || len(encoded) <= evidencePacketTargetBytes {
		return err
	}

	for _, causalOnly := range []bool{false, true} {
		if done, compactErr := compactPacketItemsToLimit(packet, protected, CriticalitySupporting, causalOnly, evidencePacketTargetBytes); done || compactErr != nil {
			return compactErr
		}
	}
	encoded, err = marshalEvidencePacketJSON(*packet)
	if err != nil || len(encoded) <= evidencePacketHardLimitBytes {
		return err
	}
	_, err = compactPacketItemsToLimit(packet, protected, CriticalityRelevant, true, evidencePacketHardLimitBytes)
	return err
}

func compactSecondaryPacketTimestamps(packet *EvidencePacket) {
	for index := range packet.Evidence {
		switch packet.Evidence[index].Type {
		case EvidenceTypeCurrentApplication, EvidenceTypeApplicationHistory, EvidenceTypeServiceHistory, EvidenceTypeRuntimeFailures:
			packet.Evidence[index].Timestamp = nil
		}
	}
}

func compactDerivedPacketMetadata(packet *EvidencePacket) {
	for index := range packet.Evidence {
		item := &packet.Evidence[index]
		if item.Kind != EvidenceDerived {
			continue
		}
		item.Subject = EvidenceSubject{}
		item.Timestamp = nil
		item.Window = nil
		item.SourceID = ""
	}
}

func compactDeterministicDerivationMetadata(packet *EvidencePacket) {
	for index := range packet.Derivations {
		derivation := &packet.Derivations[index]
		if derivation.Operation == "deterministic_reduction" {
			derivation.PolicyID = ""
		}
	}
}

func compactPacketFieldsToLimit(packet *EvidencePacket, priority evidenceFieldPriority, limit int) (bool, error) {
	for {
		itemIndex, fieldIndex := packetFieldRemovalCandidate(packet.Evidence, priority)
		if itemIndex < 0 {
			return false, nil
		}
		fields := packet.Evidence[itemIndex].Fields
		packet.Evidence[itemIndex].Fields = append(fields[:fieldIndex], fields[fieldIndex+1:]...)
		encoded, err := marshalEvidencePacketJSON(*packet)
		if err != nil || len(encoded) <= limit {
			return len(encoded) <= limit, err
		}
	}
}

func packetFieldRemovalCandidate(items []EvidencePacketItem, priority evidenceFieldPriority) (int, int) {
	for itemIndex := len(items) - 1; itemIndex >= 0; itemIndex-- {
		for fieldIndex := len(items[itemIndex].Fields) - 1; fieldIndex >= 0; fieldIndex-- {
			if packetEvidenceFieldPriority(items[itemIndex], items[itemIndex].Fields[fieldIndex].Name) == priority {
				return itemIndex, fieldIndex
			}
		}
	}
	return -1, -1
}

func compactPacketItemsToLimit(packet *EvidencePacket, protected map[string]bool, criticality RequirementCriticality, causalOnly bool, limit int) (bool, error) {
	for {
		index := packetRemovalCandidateByCausality(packet.Evidence, protected, criticality, causalOnly)
		if index < 0 {
			return false, nil
		}
		packet.Evidence = append(packet.Evidence[:index], packet.Evidence[index+1:]...)
		packet.DroppedEvidence++
		pruneEvidencePacketReferences(packet)
		encoded, err := marshalEvidencePacketJSON(*packet)
		if err != nil || len(encoded) <= limit {
			return len(encoded) <= limit, err
		}
	}
}

func packetRemovalCandidateByCausality(items []EvidencePacketItem, protected map[string]bool, criticality RequirementCriticality, causalOnly bool) int {
	for rank := 4; rank >= 0; rank-- {
		for _, kind := range []EvidenceKind{EvidenceDerived, EvidenceObserved, EvidenceInferred} {
			for index := len(items) - 1; index >= 0; index-- {
				item := items[index]
				causal := packetHighValueCausalItem(item)
				if item.Criticality != criticality || item.Kind != kind || protected[item.ID] || causalOnly != causal {
					continue
				}
				if causalOnly && packetCausalRemovalRank(item.Type) != rank {
					continue
				}
				return index
			}
		}
	}
	return -1
}

func packetCausalRemovalRank(value EvidenceType) int {
	switch value {
	case EvidenceTypeServiceHistory:
		return 4
	case EvidenceTypeInfrastructureEvents, EvidenceTypeDeploymentTimeline:
		return 3
	case EvidenceTypeRecoveryTimeline:
		return 2
	default:
		return 0
	}
}

func packetHighValueCausalItem(item EvidencePacketItem) bool {
	if item.Type == EvidenceTypeRuntimeFailures {
		return item.Kind == EvidenceDerived
	}
	return item.Type == EvidenceTypeServiceHistory ||
		item.Type == EvidenceTypeInfrastructureEvents || item.Type == EvidenceTypeRecoveryTimeline ||
		item.Type == EvidenceTypeDeploymentTimeline
}

func packetEvidenceFieldPriority(item EvidencePacketItem, name string) evidenceFieldPriority {
	switch name {
	case "deduplicated_point_count", "equal_timestamp_values_canonically_ordered", "requested_range", "bucket_seconds",
		"point_count", "returned_points", "application", "log_kind", "database_count", "database_coverage_complete":
		return evidenceFieldRemovable
	case "repository_branch", "repository_commit", "requested_ref", "branch", "source_provider", "strategy", "total_points",
		"returned_service_points", "unique_service_count", "service_summaries_omitted", "transitions_omitted",
		"coverage_gap_count", "cpu_average", "cpu_maximum", "memory_used_bytes_average", "memory_used_bytes_maximum",
		"first_status", "first_restart_count", "latest_restart_count":
		return evidenceFieldSecondary
	}
	if item.Type == EvidenceTypeServiceHistory {
		switch name {
		case "coverage_from", "coverage_to":
			return evidenceFieldSecondary
		}
	}
	if item.Type == EvidenceTypeRuntimeFailures {
		switch name {
		case "source_lines", "included_lines", "unfilterable_lines", "requested_from", "requested_to", "truncated":
			return evidenceFieldSecondary
		}
	}
	return evidenceFieldRequired
}

func projectEvidenceFields(item *EvidencePacketItem, fields []EvidenceField) ([]EvidenceField, bool) {
	projected := make([]EvidenceField, 0, len(fields))
	for _, field := range fields {
		field.Name = boundPacketText(field.Name, evidencePacketFieldNameRunes)
		if field.Value.Kind == EvidenceValueString {
			field.Value.String = projectEvidenceString(field.Name, field.Value.String)
		}
		if field.Value.Kind == EvidenceValueDecimal {
			field.Value.Decimal = boundPacketText(field.Value.Decimal, 64)
		}
		projected = append(projected, field)
	}
	sort.Slice(projected, func(i, j int) bool {
		left, right := packetEvidenceFieldPriority(*item, projected[i].Name), packetEvidenceFieldPriority(*item, projected[j].Name)
		if left != right {
			return left < right
		}
		return projected[i].Name < projected[j].Name
	})
	required := 0
	for _, field := range projected {
		if packetEvidenceFieldPriority(*item, field.Name) == evidenceFieldRequired {
			required++
		}
	}
	if item.Criticality == CriticalityCritical && required > evidencePacketMaxFieldsPerItem {
		return nil, true
	}
	if len(projected) > evidencePacketMaxFieldsPerItem {
		projected = projected[:evidencePacketMaxFieldsPerItem]
	}
	return projected, false
}

func projectEvidenceString(name, value string) string {
	if exactPacketIdentity(name, value) {
		return value
	}
	trimmed := strings.TrimSpace(value)
	if len(trimmed) > 1 && ((trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']') || (trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}')) {
		var decoded any
		if json.Unmarshal([]byte(trimmed), &decoded) == nil {
			limits := []struct{ collection, text int }{{evidencePacketMaxJSONCollection, 64}, {4, 40}, {2, 24}}
			switch name {
			case "state_transitions", "service_summary", "service_states", "service_transitions":
				limits = []struct{ collection, text int }{{evidencePacketMaxJSONCollection, 7}, {evidencePacketMaxJSONCollection, 6}, {evidencePacketMaxJSONCollection, 4}}
			case "runtime_summary":
				limits = []struct{ collection, text int }{{evidencePacketMaxJSONCollection, 48}, {evidencePacketMaxJSONCollection, 32}, {evidencePacketMaxJSONCollection, 24}}
			case "application_candidates":
				limits = []struct{ collection, text int }{{evidencePacketMaxJSONCollection, 32}, {evidencePacketMaxJSONCollection, 20}, {evidencePacketMaxJSONCollection, 12}}
			}
			for _, limits := range limits {
				projected := projectPacketJSONValue(decoded, limits.collection, limits.text, 0, name)
				encoded, err := marshalJSONWithoutHTMLEscape(projected)
				if err == nil && len(encoded) <= evidencePacketJSONFieldRunes {
					return string(encoded)
				}
			}
			return `{"truncated":true,"representation":"bounded_summary"}`
		}
	}
	if name == "status" || name == "latest_status" || strings.HasSuffix(name, "_status") {
		return boundPacketText(value, evidencePacketStatusRunes)
	}
	if name == "repository" {
		return boundPacketText(value, evidencePacketRepositoryRunes)
	}
	return boundPacketText(value, evidencePacketStringRunes)
}

func projectPacketJSONValue(value any, collectionLimit, textLimit, depth int, fieldName string) any {
	if depth >= 4 {
		return "bounded"
	}
	switch typed := value.(type) {
	case string:
		return boundPacketText(typed, textLimit)
	case []any:
		if len(typed) > collectionLimit {
			typed = typed[:collectionLimit]
		}
		out := make([]any, 0, len(typed))
		for _, child := range typed {
			out = append(out, projectPacketJSONValue(child, collectionLimit, textLimit, depth+1, fieldName))
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) > collectionLimit {
			keys = keys[:collectionLimit]
		}
		out := make(map[string]any, len(keys))
		for _, key := range keys {
			childTextLimit := textLimit
			if key == "id" && packetJSONServiceField(fieldName) {
				if text, ok := typed[key].(string); ok {
					out[boundPacketText(key, evidencePacketFieldNameRunes)] = compactServiceIdentity(text, phase2BServiceIdentityRunes)
					continue
				}
			} else if key == "application" && fieldName == "application_candidates" {
				if text, ok := typed[key].(string); ok {
					out[boundPacketText(key, evidencePacketFieldNameRunes)] = boundPacketIdentityText(text, phase2BApplicationIdentityRunes)
					continue
				}
			} else if packetJSONTimestampKey(key) {
				childTextLimit = 40
			}
			out[boundPacketText(key, evidencePacketFieldNameRunes)] = projectPacketJSONValue(typed[key], collectionLimit, childTextLimit, depth+1, fieldName)
		}
		return out
	default:
		return typed
	}
}

func packetJSONServiceField(name string) bool {
	switch name {
	case "service_summary", "service_states", "service_transitions":
		return true
	default:
		return false
	}
}

func packetJSONTimestampKey(value string) bool {
	switch value {
	case "at", "first", "last", "timestamp", "latestAt", "activatedAt", "occurredAt", "startedAt", "completedAt":
		return true
	default:
		return false
	}
}

func boundPacketEvidence(items []EvidencePacketItem, dropped int) ([]EvidencePacketItem, int) {
	critical := make([]EvidencePacketItem, 0)
	relevant := make([]EvidencePacketItem, 0)
	supporting := make([]EvidencePacketItem, 0)
	for _, item := range items {
		switch item.Criticality {
		case CriticalityCritical:
			critical = append(critical, item)
		case CriticalityRelevant:
			relevant = append(relevant, item)
		default:
			supporting = append(supporting, item)
		}
	}
	relevant = boundPacketItemsByCausality(relevant, evidencePacketMaxRelevantItems)
	if len(supporting) > evidencePacketMaxSupportingItems {
		supporting = supporting[:evidencePacketMaxSupportingItems]
	}
	out := append(append(critical, relevant...), supporting...)
	return out, dropped + len(items) - len(out)
}

func boundPacketItemsByCausality(items []EvidencePacketItem, limit int) []EvidencePacketItem {
	if len(items) <= limit {
		return items
	}
	sort.SliceStable(items, func(i, j int) bool {
		left, right := packetHighValueCausalItem(items[i]), packetHighValueCausalItem(items[j])
		if left != right {
			return left
		}
		return items[i].ID < items[j].ID
	})
	return items[:limit]
}

func boundPacketSubjects(subjects []EvidenceSubject, limit int) []EvidenceSubject {
	canonical := append([]EvidenceSubject(nil), subjects...)
	for index := range canonical {
		canonical[index] = boundPacketSubject(canonical[index])
	}
	sort.Slice(canonical, func(i, j int) bool { return canonicalJSON(canonical[i]) < canonicalJSON(canonical[j]) })
	if len(canonical) > limit {
		canonical = canonical[:limit]
	}
	return canonical
}

func boundPacketSubject(subject EvidenceSubject) EvidenceSubject {
	subject.ID = boundPacketIdentityText(subject.ID, evidencePacketSubjectRunes)
	subject.Name = boundPacketText(subject.Name, evidencePacketSubjectRunes)
	return subject
}

func boundPacketMissing(values []MissingEvidence) ([]MissingEvidence, EvidencePacketMissingOmission) {
	unique := make(map[string]MissingEvidence, len(values))
	for _, missing := range values {
		missing.RequirementID = boundPacketText(missing.RequirementID, evidencePacketIdentifierRunes)
		missing.Capability = boundPacketText(missing.Capability, evidencePacketIdentifierRunes)
		missing.LimitationCode = packetMissingLimitationCode(missing)
		missing.Reason = strings.Join(strings.Fields(missing.Reason), " ")
		if missing.LimitationCode != "" || strings.HasPrefix(missing.Reason, "capability returned ") {
			missing.Reason = ""
		} else {
			missing.Reason = boundPacketText(missing.Reason, evidencePacketMissingReasonRunes)
		}
		unique[canonicalJSON(missing)] = missing
	}
	out := make([]MissingEvidence, 0, len(unique))
	for _, missing := range unique {
		out = append(out, missing)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Criticality != out[j].Criticality {
			return packetCriticalityRank(out[i].Criticality) < packetCriticalityRank(out[j].Criticality)
		}
		return canonicalJSON(out[i]) < canonicalJSON(out[j])
	})
	if len(out) <= evidencePacketMaxMissing {
		return out, EvidencePacketMissingOmission{}
	}
	omitted := EvidencePacketMissingOmission{}
	for _, missing := range out[evidencePacketMaxMissing:] {
		switch missing.Criticality {
		case CriticalityCritical:
			omitted.Critical++
		case CriticalityRelevant:
			omitted.Relevant++
		default:
			omitted.Supporting++
		}
	}
	return out[:evidencePacketMaxMissing], omitted
}

func packetMissingLimitationCode(missing MissingEvidence) string {
	if missing.LimitationCode != "" {
		return boundPacketText(missing.LimitationCode, evidencePacketLimitationCodeRunes)
	}
	reason := strings.ToLower(strings.Join(strings.Fields(missing.Reason), " "))
	switch {
	case reason == "source reported partial or truncated coverage":
		return "source_coverage_partial"
	case strings.HasPrefix(reason, "dependency unavailable"):
		return "dependency_unavailable"
	case reason == "typed result could not be reduced":
		return "typed_result_malformed"
	case reason == "deployment source identity unavailable in compound application context":
		return "deployment_source_unavailable"
	case reason == "platform overview unavailable in compound application context":
		return "platform_overview_unavailable"
	case reason == "database lookup coverage truncated by compound application context bound":
		return "database_lookup_coverage_truncated"
	case reason == "no registered read provider":
		return "provider_unavailable"
	case reason == "repository content requires a planned safe search dependency":
		return "repository_search_dependency_required"
	case strings.HasPrefix(reason, "database lookups failed for:"):
		return "database_lookup_failed"
	}
	for _, facet := range []string{"system", "recovery", "deployments", "databases", "observability"} {
		if reason == facet+" section unavailable in compound platform overview" ||
			reason == facet+" overview facet unavailable in compound application context" {
			return "overview_" + facet + "_unavailable"
		}
	}
	return ""
}

func packetCriticalityRank(value RequirementCriticality) int {
	switch value {
	case CriticalityCritical:
		return 0
	case CriticalityRelevant:
		return 1
	default:
		return 2
	}
}

func boundPacketConflicts(values []EvidenceConflict) []EvidenceConflict {
	out := append([]EvidenceConflict(nil), values...)
	for index := range out {
		out[index].ID = boundPacketText(out[index].ID, evidencePacketIdentifierRunes)
		out[index].Subject = boundPacketSubject(out[index].Subject)
		out[index].Field = boundPacketText(out[index].Field, evidencePacketFieldNameRunes)
		out[index].EvidenceIDs = boundPacketStrings(out[index].EvidenceIDs, evidencePacketMaxConflictEvidenceIDs, evidencePacketIdentifierRunes)
		out[index].Explanation = boundPacketText(out[index].Explanation, evidencePacketExplanationRunes)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Material != out[j].Material {
			return out[i].Material
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > evidencePacketMaxConflicts {
		out = out[:evidencePacketMaxConflicts]
	}
	return out
}

func boundPacketStrings(values []string, limit, runeLimit int) []string {
	out := make([]string, 0, min(len(values), limit))
	seen := map[string]bool{}
	for _, value := range values {
		value = boundPacketText(value, runeLimit)
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func boundPacketText(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit && len(value) <= limit {
		return value
	}
	maximumRunes := min(len(runes), limit-1)
	for maximumRunes > 0 && len(string(runes[:maximumRunes]))+len("…") > limit {
		maximumRunes--
	}
	if maximumRunes == 0 {
		return "…"
	}
	return string(runes[:maximumRunes]) + "…"
}

func boundPacketIdentityText(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit && len(value) <= limit {
		return value
	}
	if limit <= len("…") {
		return boundPacketText(value, limit)
	}
	retained := min(len(runes), limit-1)
	head := retained / 2
	tail := retained - head
	for head+tail > 0 && len(string(runes[:head]))+len("…")+len(string(runes[len(runes)-tail:])) > limit {
		if head >= tail && head > 0 {
			head--
		} else {
			tail--
		}
	}
	return string(runes[:head]) + "…" + string(runes[len(runes)-tail:])
}

func exactPacketIdentity(name, value string) bool {
	if name != "commit_sha" && name != "repository_commit" {
		return false
	}
	digest := value
	if strings.HasPrefix(digest, "sha256:") {
		digest = strings.TrimPrefix(digest, "sha256:")
	}
	if len(digest) != 40 && len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func compactPacketReferences(packet *EvidencePacket) {
	canonicalizeEvidencePacket(packet)
	sourceIDs := make(map[string]string, len(packet.Sources))
	for index := range packet.Sources {
		oldID := packet.Sources[index].ID
		newID := "s" + strconv.Itoa(index+1)
		sourceIDs[oldID] = newID
		packet.Sources[index].ID = newID
	}
	requirements := make([]string, 0, len(packet.Evidence)+len(packet.Missing))
	for _, item := range packet.Evidence {
		requirements = append(requirements, item.RequirementID)
	}
	for _, missing := range packet.Missing {
		requirements = append(requirements, missing.RequirementID)
	}
	requirements = sortedUniqueContractStrings(requirements)
	requirementIDs := make(map[string]string, len(requirements))
	for index, requirementID := range requirements {
		requirementIDs[requirementID] = "r" + strconv.Itoa(index+1)
	}
	evidenceIDs := make(map[string]string, len(packet.Evidence))
	for index := range packet.Evidence {
		oldID := packet.Evidence[index].ID
		newID := "e" + strconv.Itoa(index+1)
		evidenceIDs[oldID] = newID
		packet.Evidence[index].ID = newID
	}
	for index := range packet.Evidence {
		item := &packet.Evidence[index]
		item.SourceID = sourceIDs[item.SourceID]
		item.RequirementID = requirementIDs[item.RequirementID]
	}
	for index := range packet.Missing {
		packet.Missing[index].RequirementID = requirementIDs[packet.Missing[index].RequirementID]
	}
	for index := range packet.Derivations {
		derivation := &packet.Derivations[index]
		derivation.OutputID = evidenceIDs[derivation.OutputID]
		for inputIndex := range derivation.Inputs {
			derivation.Inputs[inputIndex] = evidenceIDs[derivation.Inputs[inputIndex]]
		}
	}
	for index := range packet.Conflicts {
		for evidenceIndex := range packet.Conflicts[index].EvidenceIDs {
			packet.Conflicts[index].EvidenceIDs[evidenceIndex] = evidenceIDs[packet.Conflicts[index].EvidenceIDs[evidenceIndex]]
		}
	}
	canonicalizeEvidencePacket(packet)
}

func marshalEvidencePacketJSON(packet EvidencePacket) ([]byte, error) {
	return marshalJSONWithoutHTMLEscape(packet)
}

func marshalJSONWithoutHTMLEscape(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte{'\n'}), nil
}
