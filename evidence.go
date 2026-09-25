package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const (
	investigationEvidenceSchemaVersion = "phase2a.v1"
	evidencePacketTargetBytes          = 4 << 10
	evidencePacketHardLimitBytes       = 6 << 10
)

// EvidenceKind separates direct observations from deterministic derivations
// and model-generated inferences. These Phase 2 contracts are in-memory only.
type EvidenceKind string

const (
	EvidenceObserved EvidenceKind = "observed"
	EvidenceDerived  EvidenceKind = "derived"
	EvidenceInferred EvidenceKind = "inferred"
)

type Availability string

const (
	AvailabilityAvailable   Availability = "available"
	AvailabilityPartial     Availability = "partial"
	AvailabilityUnavailable Availability = "unavailable"
	AvailabilityNotFound    Availability = "not_found"
	AvailabilityUnsupported Availability = "unsupported"
)

type FreshnessState string

const (
	FreshnessFresh   FreshnessState = "fresh"
	FreshnessStale   FreshnessState = "stale"
	FreshnessUnknown FreshnessState = "unknown"
)

type CapabilityFamily string

const (
	CapabilityFamilyPlatformCurrent        CapabilityFamily = "platform.current"
	CapabilityFamilyDeploymentCurrent      CapabilityFamily = "deployment.current"
	CapabilityFamilyHostHistory            CapabilityFamily = "host.history"
	CapabilityFamilyApplicationHistory     CapabilityFamily = "application.history"
	CapabilityFamilyInfrastructureTimeline CapabilityFamily = "infrastructure.timeline"
	CapabilityFamilyDatabaseState          CapabilityFamily = "database.state"
	CapabilityFamilyDeploymentHistory      CapabilityFamily = "deployment.history"
	CapabilityFamilyRuntimeEvidence        CapabilityFamily = "runtime.evidence"
	CapabilityFamilySourceRepository       CapabilityFamily = "source.repository"
)

type EvidenceType string

const (
	EvidenceTypeCurrentPlatformState  EvidenceType = "current_platform_state"
	EvidenceTypeCurrentDeploymentList EvidenceType = "current_deployment_list"
	EvidenceTypeCurrentDeployment     EvidenceType = "current_deployment_identity"
	EvidenceTypeCurrentApplication    EvidenceType = "current_application_state"
	EvidenceTypeHostHistory           EvidenceType = "host_history"
	EvidenceTypeThermalHistory        EvidenceType = "thermal_history"
	EvidenceTypeApplicationHistory    EvidenceType = "application_history"
	EvidenceTypeServiceHistory        EvidenceType = "service_history"
	EvidenceTypeInfrastructureEvents  EvidenceType = "infrastructure_events"
	EvidenceTypeCurrentDatabase       EvidenceType = "current_database_state"
	EvidenceTypeDatabaseBackups       EvidenceType = "database_backups"
	EvidenceTypeActivityTimeline      EvidenceType = "activity_timeline"
	EvidenceTypeRecoveryTimeline      EvidenceType = "recovery_timeline"
	EvidenceTypeRepositoryInventory   EvidenceType = "repository_inventory"
	EvidenceTypeRepositorySearch      EvidenceType = "repository_search"
	EvidenceTypeRepositoryContent     EvidenceType = "repository_content"
	EvidenceTypeRuntimeFailures       EvidenceType = "runtime_failures"
	EvidenceTypeDeploymentFailures    EvidenceType = "deployment_failures"
	EvidenceTypeDeploymentTimeline    EvidenceType = "deployment_timeline"
)

type SubjectKind string

const (
	SubjectHost        SubjectKind = "host"
	SubjectApplication SubjectKind = "application"
	SubjectDatabase    SubjectKind = "database"
	SubjectService     SubjectKind = "service"
	SubjectRepository  SubjectKind = "repository"
	SubjectPlatform    SubjectKind = "platform"
	SubjectUnknown     SubjectKind = "unknown"
)

type EvidenceSubject struct {
	Kind SubjectKind `json:"kind"`
	ID   string      `json:"id"`
	Name string      `json:"name,omitempty"`
}

type EvidenceValueKind string

const (
	EvidenceValueString    EvidenceValueKind = "string"
	EvidenceValueInteger   EvidenceValueKind = "integer"
	EvidenceValueUnsigned  EvidenceValueKind = "unsigned"
	EvidenceValueDecimal   EvidenceValueKind = "decimal"
	EvidenceValueBoolean   EvidenceValueKind = "boolean"
	EvidenceValueTimestamp EvidenceValueKind = "timestamp"
	EvidenceValueDuration  EvidenceValueKind = "duration"
)

// EvidenceValue uses typed fields instead of free-form prose. Decimal retains
// the source lexical representation when exact decimal preservation matters.
type EvidenceValue struct {
	Kind      EvidenceValueKind `json:"kind"`
	String    string            `json:"string,omitempty"`
	Integer   *int64            `json:"integer,omitempty"`
	Unsigned  *uint64           `json:"unsigned,omitempty"`
	Decimal   string            `json:"decimal,omitempty"`
	Boolean   *bool             `json:"boolean,omitempty"`
	Timestamp *time.Time        `json:"timestamp,omitempty"`
	Duration  *time.Duration    `json:"duration,omitempty"`
}

type SafeArgument struct {
	Name  string        `json:"name"`
	Value EvidenceValue `json:"value"`
}

type EvidenceField struct {
	Name  string        `json:"name"`
	Value EvidenceValue `json:"value"`
}

type EvidencePayload struct {
	Fields []EvidenceField `json:"fields"`
}

type EvidenceTruncation struct {
	Truncated bool `json:"truncated"`
	Returned  int  `json:"returned,omitempty"`
	Total     int  `json:"total,omitempty"`
}

type EvidenceErrorCategory string

const (
	EvidenceErrorInvalidRequest EvidenceErrorCategory = "invalid_request"
	EvidenceErrorUnavailable    EvidenceErrorCategory = "unavailable"
	EvidenceErrorNotFound       EvidenceErrorCategory = "not_found"
	EvidenceErrorMalformed      EvidenceErrorCategory = "malformed"
	EvidenceErrorTooLarge       EvidenceErrorCategory = "too_large"
	EvidenceErrorUnauthorized   EvidenceErrorCategory = "unauthorized"
	EvidenceErrorTimeout        EvidenceErrorCategory = "timeout"
	EvidenceErrorInternal       EvidenceErrorCategory = "internal"
)

type SourceRef struct {
	ID              string                `json:"id"`
	Capability      string                `json:"capability"`
	Family          CapabilityFamily      `json:"family"`
	Resource        string                `json:"resource,omitempty"`
	Subject         EvidenceSubject       `json:"subject"`
	Arguments       []SafeArgument        `json:"arguments,omitempty"`
	CollectedAt     time.Time             `json:"collectedAt"`
	SourceTimestamp *time.Time            `json:"sourceTimestamp,omitempty"`
	Availability    Availability          `json:"availability"`
	ErrorCategory   EvidenceErrorCategory `json:"errorCategory,omitempty"`
	Truncation      EvidenceTruncation    `json:"truncation"`
}

func (source SourceRef) WithStableID() SourceRef {
	source.Arguments = canonicalSafeArguments(source.Arguments)
	source.ID = stableContractID("source", struct {
		Capability      string
		Family          CapabilityFamily
		Resource        string
		Subject         EvidenceSubject
		Arguments       []SafeArgument
		CollectedAt     time.Time
		SourceTimestamp *time.Time
		Availability    Availability
		ErrorCategory   EvidenceErrorCategory
		Truncation      EvidenceTruncation
	}{source.Capability, source.Family, source.Resource, source.Subject, source.Arguments, source.CollectedAt, source.SourceTimestamp, source.Availability, source.ErrorCategory, source.Truncation})
	return source
}

type Freshness struct {
	State     FreshnessState `json:"state"`
	Age       *time.Duration `json:"age,omitempty"`
	MaxAge    *time.Duration `json:"maxAge,omitempty"`
	Reason    string         `json:"reason,omitempty"`
	PolicySet bool           `json:"policySet"`
}

type RequirementCriticality string

const (
	CriticalityCritical   RequirementCriticality = "critical"
	CriticalityRelevant   RequirementCriticality = "relevant"
	CriticalitySupporting RequirementCriticality = "supporting"
)

type Relevance struct {
	RequirementID string                 `json:"requirementId"`
	Criticality   RequirementCriticality `json:"criticality"`
	Reason        string                 `json:"reason,omitempty"`
}

type EvidenceItem struct {
	ID        string          `json:"id"`
	Kind      EvidenceKind    `json:"kind"`
	Type      EvidenceType    `json:"type"`
	Subject   EvidenceSubject `json:"subject"`
	Timestamp *time.Time      `json:"timestamp,omitempty"`
	Window    *TemporalScope  `json:"window,omitempty"`
	SourceID  string          `json:"sourceId"`
	Freshness Freshness       `json:"freshness"`
	Relevance Relevance       `json:"relevance"`
	Payload   EvidencePayload `json:"payload"`
	Inputs    []string        `json:"inputs,omitempty"`
}

func (item EvidenceItem) WithStableID() EvidenceItem {
	item.Payload.Fields = canonicalEvidenceFields(item.Payload.Fields)
	item.Inputs = sortedUniqueContractStrings(item.Inputs)
	item.ID = stableContractID("evidence", struct {
		Kind      EvidenceKind
		Type      EvidenceType
		Subject   EvidenceSubject
		Timestamp *time.Time
		Window    *TemporalScope
		SourceID  string
		Freshness Freshness
		Relevance Relevance
		Payload   EvidencePayload
		Inputs    []string
	}{item.Kind, item.Type, item.Subject, item.Timestamp, item.Window, item.SourceID, item.Freshness, item.Relevance, item.Payload, item.Inputs})
	return item
}

type Derivation struct {
	ID          string   `json:"id"`
	Operation   string   `json:"operation"`
	PolicyID    string   `json:"policyId,omitempty"`
	Inputs      []string `json:"inputs"`
	OutputID    string   `json:"outputId"`
	Explanation string   `json:"explanation,omitempty"`
}

func (derivation Derivation) WithStableID() Derivation {
	derivation.Inputs = sortedUniqueContractStrings(derivation.Inputs)
	derivation.ID = stableContractID("derivation", struct {
		Operation   string
		PolicyID    string
		Inputs      []string
		OutputID    string
		Explanation string
	}{derivation.Operation, derivation.PolicyID, derivation.Inputs, derivation.OutputID, derivation.Explanation})
	return derivation
}

type MissingEvidence struct {
	RequirementID string                 `json:"requirementId"`
	Criticality   RequirementCriticality `json:"criticality"`
	Capability    string                 `json:"capability,omitempty"`
	Availability  Availability           `json:"availability"`
	ErrorCategory EvidenceErrorCategory  `json:"errorCategory,omitempty"`
	Reason        string                 `json:"reason,omitempty"`
}

type EvidenceConflict struct {
	ID          string          `json:"id"`
	Subject     EvidenceSubject `json:"subject"`
	Field       string          `json:"field"`
	EvidenceIDs []string        `json:"evidenceIds"`
	Material    bool            `json:"material"`
	Explanation string          `json:"explanation,omitempty"`
}

func (conflict EvidenceConflict) WithStableID() EvidenceConflict {
	conflict.EvidenceIDs = sortedUniqueContractStrings(conflict.EvidenceIDs)
	conflict.ID = stableContractID("conflict", struct {
		Subject     EvidenceSubject
		Field       string
		EvidenceIDs []string
		Material    bool
		Explanation string
	}{conflict.Subject, conflict.Field, conflict.EvidenceIDs, conflict.Material, conflict.Explanation})
	return conflict
}

type ConfidenceLevel string

const (
	ConfidenceHigh   ConfidenceLevel = "High"
	ConfidenceMedium ConfidenceLevel = "Medium"
	ConfidenceLow    ConfidenceLevel = "Low"
)

type CompletenessState string
type EvidenceDirectness string
type AgreementState string
type CoverageState string

const (
	CompletenessComplete CompletenessState = "complete"
	CompletenessPartial  CompletenessState = "partial"
	CompletenessMissing  CompletenessState = "missing"

	DirectnessDirect   EvidenceDirectness = "direct"
	DirectnessMixed    EvidenceDirectness = "mixed"
	DirectnessIndirect EvidenceDirectness = "indirect"

	AgreementConsistent AgreementState = "consistent"
	AgreementMixed      AgreementState = "mixed"
	AgreementConflicted AgreementState = "conflicted"

	CoverageComplete CoverageState = "complete"
	CoveragePartial  CoverageState = "partial"
	CoverageNarrow   CoverageState = "narrow"
)

type ConfidenceInputs struct {
	CriticalCompleteness CompletenessState  `json:"criticalCompleteness"`
	Freshness            FreshnessState     `json:"freshness"`
	Directness           EvidenceDirectness `json:"directness"`
	Agreement            AgreementState     `json:"agreement"`
	Coverage             CoverageState      `json:"coverage"`
	RelevantTruncation   bool               `json:"relevantTruncation"`
	SubjectAmbiguity     bool               `json:"subjectAmbiguity"`
}

type ConfidenceDecision struct {
	Level           ConfidenceLevel  `json:"level,omitempty"`
	SoftwareCeiling ConfidenceLevel  `json:"softwareCeiling,omitempty"`
	Determined      bool             `json:"determined"`
	Inputs          ConfidenceInputs `json:"inputs"`
	Reasons         []string         `json:"reasons,omitempty"`
}

type InvestigationEvidence struct {
	SchemaVersion      string                `json:"schemaVersion"`
	InvestigationID    string                `json:"investigationId"`
	NormalizedQuestion string                `json:"question"`
	Route              InvestigationRoute    `json:"route"`
	Requirements       []EvidenceRequirement `json:"requirements"`
	Sources            []SourceRef           `json:"sources"`
	Items              []EvidenceItem        `json:"items"`
	Derivations        []Derivation          `json:"derivations"`
	Missing            []MissingEvidence     `json:"missing"`
	Conflicts          []EvidenceConflict    `json:"conflicts"`
	Confidence         ConfidenceDecision    `json:"confidence"`
}

// NormalizeInvestigationEvidence supplies stable IDs, rewrites provenance
// references graph-wide, validates them, and applies canonical ordering. It
// preserves source values; it does not summarize or shorten identifiers.
func NormalizeInvestigationEvidence(in InvestigationEvidence) (InvestigationEvidence, error) {
	if in.SchemaVersion == "" {
		in.SchemaVersion = investigationEvidenceSchemaVersion
	}
	in.Requirements = append([]EvidenceRequirement(nil), in.Requirements...)
	in.Sources = append([]SourceRef(nil), in.Sources...)
	in.Items = append([]EvidenceItem(nil), in.Items...)
	in.Derivations = append([]Derivation(nil), in.Derivations...)
	in.Missing = append([]MissingEvidence(nil), in.Missing...)
	in.Conflicts = append([]EvidenceConflict(nil), in.Conflicts...)
	for index := range in.Items {
		in.Items[index].Inputs = append([]string(nil), in.Items[index].Inputs...)
	}
	for index := range in.Derivations {
		in.Derivations[index].Inputs = append([]string(nil), in.Derivations[index].Inputs...)
	}
	for index := range in.Conflicts {
		in.Conflicts[index].EvidenceIDs = append([]string(nil), in.Conflicts[index].EvidenceIDs...)
	}

	requirementIDs := map[string]string{}
	canonicalRequirementIDs := map[string]bool{}
	for index := range in.Requirements {
		oldID := in.Requirements[index].ID
		in.Requirements[index] = in.Requirements[index].WithStableID(string(in.Route.ID))
		newID := in.Requirements[index].ID
		if canonicalRequirementIDs[newID] {
			return InvestigationEvidence{}, fmt.Errorf("duplicate canonical requirement ID %q", newID)
		}
		canonicalRequirementIDs[newID] = true
		if err := addEvidenceIDMapping(requirementIDs, oldID, newID, "requirement"); err != nil {
			return InvestigationEvidence{}, err
		}
		requirementIDs[newID] = newID
	}

	sourceIDs := map[string]string{}
	canonicalSourceIDs := map[string]bool{}
	for index := range in.Sources {
		oldID := in.Sources[index].ID
		in.Sources[index] = in.Sources[index].WithStableID()
		newID := in.Sources[index].ID
		if canonicalSourceIDs[newID] {
			return InvestigationEvidence{}, fmt.Errorf("duplicate canonical source ID %q", newID)
		}
		canonicalSourceIDs[newID] = true
		if err := addEvidenceIDMapping(sourceIDs, oldID, newID, "source"); err != nil {
			return InvestigationEvidence{}, err
		}
		sourceIDs[newID] = newID
	}

	itemByOldID := map[string]int{}
	for index, item := range in.Items {
		if item.ID == "" {
			continue
		}
		if _, exists := itemByOldID[item.ID]; exists {
			return InvestigationEvidence{}, fmt.Errorf("duplicate pre-normalized evidence ID %q", item.ID)
		}
		itemByOldID[item.ID] = index
	}
	itemIDs := map[string]string{}
	canonicalItemIDs := map[string]bool{}
	itemState := make([]uint8, len(in.Items))
	var normalizeItem func(int) error
	normalizeItem = func(index int) error {
		switch itemState[index] {
		case 1:
			return fmt.Errorf("evidence input cycle at index %d", index)
		case 2:
			return nil
		}
		itemState[index] = 1
		item := in.Items[index]
		oldID := item.ID
		item.Relevance.RequirementID = remapEvidenceID(item.Relevance.RequirementID, requirementIDs)
		if item.Relevance.RequirementID == "" && len(in.Requirements) == 1 {
			item.Relevance.RequirementID = in.Requirements[0].ID
		}
		item.SourceID = remapEvidenceID(item.SourceID, sourceIDs)
		if item.SourceID == "" && item.Kind == EvidenceObserved && len(in.Sources) == 1 {
			item.SourceID = in.Sources[0].ID
		}
		for inputIndex, inputID := range item.Inputs {
			if dependencyIndex, ok := itemByOldID[inputID]; ok {
				if err := normalizeItem(dependencyIndex); err != nil {
					return err
				}
				item.Inputs[inputIndex] = in.Items[dependencyIndex].ID
				continue
			}
			item.Inputs[inputIndex] = remapEvidenceID(inputID, itemIDs)
		}
		item = item.WithStableID()
		if canonicalItemIDs[item.ID] {
			return fmt.Errorf("duplicate canonical evidence ID %q", item.ID)
		}
		canonicalItemIDs[item.ID] = true
		if err := addEvidenceIDMapping(itemIDs, oldID, item.ID, "evidence"); err != nil {
			return err
		}
		itemIDs[item.ID] = item.ID
		in.Items[index] = item
		itemState[index] = 2
		return nil
	}
	for index := range in.Items {
		if err := normalizeItem(index); err != nil {
			return InvestigationEvidence{}, err
		}
	}

	canonicalDerivationIDs := map[string]bool{}
	for index := range in.Derivations {
		for inputIndex, inputID := range in.Derivations[index].Inputs {
			in.Derivations[index].Inputs[inputIndex] = remapEvidenceID(inputID, itemIDs)
		}
		in.Derivations[index].OutputID = remapEvidenceID(in.Derivations[index].OutputID, itemIDs)
		in.Derivations[index] = in.Derivations[index].WithStableID()
		if canonicalDerivationIDs[in.Derivations[index].ID] {
			return InvestigationEvidence{}, fmt.Errorf("duplicate canonical derivation ID %q", in.Derivations[index].ID)
		}
		canonicalDerivationIDs[in.Derivations[index].ID] = true
	}
	canonicalConflictIDs := map[string]bool{}
	for index := range in.Conflicts {
		for evidenceIndex, evidenceID := range in.Conflicts[index].EvidenceIDs {
			in.Conflicts[index].EvidenceIDs[evidenceIndex] = remapEvidenceID(evidenceID, itemIDs)
		}
		in.Conflicts[index] = in.Conflicts[index].WithStableID()
		if canonicalConflictIDs[in.Conflicts[index].ID] {
			return InvestigationEvidence{}, fmt.Errorf("duplicate canonical conflict ID %q", in.Conflicts[index].ID)
		}
		canonicalConflictIDs[in.Conflicts[index].ID] = true
	}
	for index := range in.Missing {
		in.Missing[index].RequirementID = remapEvidenceID(in.Missing[index].RequirementID, requirementIDs)
		if in.Missing[index].RequirementID == "" && len(in.Requirements) == 1 {
			in.Missing[index].RequirementID = in.Requirements[0].ID
		}
	}

	if err := validateInvestigationEvidenceGraph(in); err != nil {
		return InvestigationEvidence{}, err
	}

	sort.Slice(in.Requirements, func(i, j int) bool { return in.Requirements[i].ID < in.Requirements[j].ID })
	sort.Slice(in.Sources, func(i, j int) bool { return in.Sources[i].ID < in.Sources[j].ID })
	sort.Slice(in.Items, func(i, j int) bool { return in.Items[i].ID < in.Items[j].ID })
	sort.Slice(in.Derivations, func(i, j int) bool { return in.Derivations[i].ID < in.Derivations[j].ID })
	sort.Slice(in.Missing, func(i, j int) bool {
		return canonicalJSON(in.Missing[i]) < canonicalJSON(in.Missing[j])
	})
	sort.Slice(in.Conflicts, func(i, j int) bool { return in.Conflicts[i].ID < in.Conflicts[j].ID })
	return in, nil
}

func addEvidenceIDMapping(mapping map[string]string, oldID, newID, kind string) error {
	if oldID == "" {
		return nil
	}
	if existing, ok := mapping[oldID]; ok && existing != newID {
		return fmt.Errorf("pre-normalized %s ID %q maps to multiple canonical IDs", kind, oldID)
	}
	mapping[oldID] = newID
	return nil
}

func remapEvidenceID(value string, mapping map[string]string) string {
	if canonical, ok := mapping[value]; ok {
		return canonical
	}
	return value
}

func validateInvestigationEvidenceGraph(in InvestigationEvidence) error {
	requirementIDs := map[string]bool{}
	for _, requirement := range in.Requirements {
		if requirement.ID == "" || requirementIDs[requirement.ID] {
			return fmt.Errorf("invalid or duplicate requirement ID %q", requirement.ID)
		}
		requirementIDs[requirement.ID] = true
	}
	sourceIDs := map[string]bool{}
	for _, source := range in.Sources {
		if source.ID == "" || sourceIDs[source.ID] {
			return fmt.Errorf("invalid or duplicate source ID %q", source.ID)
		}
		sourceIDs[source.ID] = true
	}
	evidenceIDs := map[string]bool{}
	for _, item := range in.Items {
		if item.ID == "" || evidenceIDs[item.ID] {
			return fmt.Errorf("invalid or duplicate evidence ID %q", item.ID)
		}
		evidenceIDs[item.ID] = true
	}
	for _, item := range in.Items {
		if len(requirementIDs) > 0 && item.Relevance.RequirementID == "" {
			return fmt.Errorf("evidence %q has no requirement ID", item.ID)
		}
		if item.Relevance.RequirementID != "" && !requirementIDs[item.Relevance.RequirementID] {
			return fmt.Errorf("evidence %q has dangling requirement ID %q", item.ID, item.Relevance.RequirementID)
		}
		if item.Kind == EvidenceObserved && item.SourceID == "" {
			return fmt.Errorf("observed evidence %q has no source ID", item.ID)
		}
		if item.SourceID != "" && !sourceIDs[item.SourceID] {
			return fmt.Errorf("evidence %q has dangling source ID %q", item.ID, item.SourceID)
		}
		if (item.Kind == EvidenceDerived || item.Kind == EvidenceInferred) && len(item.Inputs) == 0 {
			return fmt.Errorf("%s evidence %q has no inputs", item.Kind, item.ID)
		}
		for _, inputID := range item.Inputs {
			if !evidenceIDs[inputID] {
				return fmt.Errorf("evidence %q has dangling input ID %q", item.ID, inputID)
			}
		}
	}
	derivationIDs := map[string]bool{}
	for _, derivation := range in.Derivations {
		if derivation.ID == "" || derivationIDs[derivation.ID] {
			return fmt.Errorf("invalid or duplicate derivation ID %q", derivation.ID)
		}
		derivationIDs[derivation.ID] = true
		if len(derivation.Inputs) == 0 {
			return fmt.Errorf("derivation %q has no inputs", derivation.ID)
		}
		for _, inputID := range derivation.Inputs {
			if !evidenceIDs[inputID] {
				return fmt.Errorf("derivation %q has dangling input ID %q", derivation.ID, inputID)
			}
		}
		if !evidenceIDs[derivation.OutputID] {
			return fmt.Errorf("derivation %q has dangling output ID %q", derivation.ID, derivation.OutputID)
		}
	}
	conflictIDs := map[string]bool{}
	for _, conflict := range in.Conflicts {
		if conflict.ID == "" || conflictIDs[conflict.ID] {
			return fmt.Errorf("invalid or duplicate conflict ID %q", conflict.ID)
		}
		conflictIDs[conflict.ID] = true
		if len(conflict.EvidenceIDs) < 2 {
			return fmt.Errorf("conflict %q has fewer than two evidence IDs", conflict.ID)
		}
		for _, evidenceID := range conflict.EvidenceIDs {
			if !evidenceIDs[evidenceID] {
				return fmt.Errorf("conflict %q has dangling evidence ID %q", conflict.ID, evidenceID)
			}
		}
	}
	for _, missing := range in.Missing {
		if !requirementIDs[missing.RequirementID] {
			return fmt.Errorf("missing evidence has dangling requirement ID %q", missing.RequirementID)
		}
	}
	return nil
}

func canonicalSafeArguments(values []SafeArgument) []SafeArgument {
	out := append([]SafeArgument(nil), values...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return canonicalJSON(out[i].Value) < canonicalJSON(out[j].Value)
	})
	return out
}

func canonicalEvidenceFields(values []EvidenceField) []EvidenceField {
	out := append([]EvidenceField(nil), values...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return canonicalJSON(out[i].Value) < canonicalJSON(out[j].Value)
	})
	return out
}

func sortedUniqueContractStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func stableContractID(prefix string, value any) string {
	sum := sha256.Sum256([]byte(canonicalJSON(value)))
	return prefix + "_" + hex.EncodeToString(sum[:])
}

func canonicalJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// Legacy Phase 1 repository-location evidence remains internal and JSON-omitted.
type evidenceKind = EvidenceKind

const (
	evidenceObserved  evidenceKind = EvidenceObserved
	evidenceDerived   evidenceKind = EvidenceDerived
	evidenceInference evidenceKind = "inference"
)

type evidenceSourceReference struct {
	Capability string
	Resource   string
}

type evidenceRecord struct {
	Kind      evidenceKind
	Statement string
	Source    evidenceSourceReference
}
