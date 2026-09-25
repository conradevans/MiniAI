package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestRepositoryEvidenceCarriesInternalObservedSourceContract(t *testing.T) {
	search := repoSearchResponse{Hits: []repoSearchHit{{Path: "backend/routes/schedule.go", Line: 1, Text: "handler"}}}
	file := repoFileResponse{Path: "backend/routes/schedule.go", Content: "handler"}
	evidence, ok := compactRepositoryLocationEvidence(search, file)
	if !ok {
		t.Fatal("expected repository evidence")
	}
	if evidence.Contract.Kind != evidenceObserved || evidence.Contract.Source.Capability != "read_repository_file" ||
		evidence.Contract.Source.Resource != file.Path {
		t.Fatalf("contract=%+v", evidence.Contract)
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "Contract") || strings.Contains(string(encoded), "observed") {
		t.Fatalf("internal evidence contract leaked into current JSON: %s", encoded)
	}
}

func TestEvidenceContractDistinguishesFactAndInferenceKinds(t *testing.T) {
	if evidenceObserved == evidenceDerived || evidenceObserved == evidenceInference || evidenceDerived == evidenceInference {
		t.Fatal("evidence kinds must remain distinct")
	}
}

func TestEvidenceSourceAndItemIDsAndOrderingAreDeterministic(t *testing.T) {
	collected := time.Date(2026, 9, 22, 15, 4, 5, 123456789, time.FixedZone("source", 2*60*60))
	subject := EvidenceSubject{Kind: SubjectApplication, ID: "golf-mullet", Name: "Golf Mullet"}
	firstSource := SourceRef{
		Capability: "read_deployment_history",
		Family:     CapabilityFamilyDeploymentHistory,
		Resource:   "/api/v1/deployments/golf-mullet/history",
		Subject:    subject,
		Arguments: []SafeArgument{
			{Name: "to", Value: EvidenceValue{Kind: EvidenceValueString, String: "2026-09-22T15:04:05Z"}},
			{Name: "app", Value: EvidenceValue{Kind: EvidenceValueString, String: "golf-mullet"}},
		},
		CollectedAt:  collected,
		Availability: AvailabilityAvailable,
	}.WithStableID()
	secondSource := firstSource
	secondSource.ID = ""
	secondSource.Arguments[0], secondSource.Arguments[1] = secondSource.Arguments[1], secondSource.Arguments[0]
	secondSource = secondSource.WithStableID()
	if firstSource.ID != secondSource.ID {
		t.Fatalf("canonical arguments produced different IDs: %q/%q", firstSource.ID, secondSource.ID)
	}

	otherSource := firstSource
	otherSource.ID = ""
	otherSource.Resource = "/api/v1/deployments/minideploy/history"
	otherSource = otherSource.WithStableID()
	first := mustNormalizeInvestigationEvidence(t, InvestigationEvidence{
		Route:   InvestigationRoute{ID: RouteDeploymentCorrelation},
		Sources: []SourceRef{otherSource, firstSource},
	})
	second := mustNormalizeInvestigationEvidence(t, InvestigationEvidence{
		Route:   InvestigationRoute{ID: RouteDeploymentCorrelation},
		Sources: []SourceRef{firstSource, otherSource},
	})
	if !reflect.DeepEqual(first.Sources, second.Sources) {
		t.Fatalf("source ordering differs:\n%+v\n%+v", first.Sources, second.Sources)
	}

	itemA := EvidenceItem{
		Kind:      EvidenceObserved,
		Type:      EvidenceTypeCurrentDeployment,
		Subject:   subject,
		Timestamp: &collected,
		SourceID:  firstSource.ID,
		Freshness: Freshness{State: FreshnessUnknown, Reason: "no source policy"},
		Payload: EvidencePayload{Fields: []EvidenceField{
			{Name: "status", Value: EvidenceValue{Kind: EvidenceValueString, String: "running"}},
			{Name: "commit_sha", Value: EvidenceValue{Kind: EvidenceValueString, String: "0123456789abcdef0123456789abcdef01234567"}},
		}},
	}.WithStableID()
	itemB := itemA
	itemB.ID = ""
	itemB.Payload.Fields[0], itemB.Payload.Fields[1] = itemB.Payload.Fields[1], itemB.Payload.Fields[0]
	itemB = itemB.WithStableID()
	if itemA.ID != itemB.ID {
		t.Fatalf("canonical payload produced different IDs: %q/%q", itemA.ID, itemB.ID)
	}
	otherItem := itemA
	otherItem.ID = ""
	otherItem.Type = EvidenceTypeDeploymentTimeline
	otherItem = otherItem.WithStableID()
	firstItems := mustNormalizeInvestigationEvidence(t, InvestigationEvidence{
		Route:   InvestigationRoute{ID: RouteDeploymentCorrelation},
		Sources: []SourceRef{firstSource},
		Items:   []EvidenceItem{otherItem, itemA},
	})
	secondItems := mustNormalizeInvestigationEvidence(t, InvestigationEvidence{
		Route:   InvestigationRoute{ID: RouteDeploymentCorrelation},
		Sources: []SourceRef{firstSource},
		Items:   []EvidenceItem{itemA, otherItem},
	})
	if !reflect.DeepEqual(firstItems.Items, secondItems.Items) {
		t.Fatalf("evidence ordering differs:\n%+v\n%+v", firstItems.Items, secondItems.Items)
	}
}

func TestInvestigationEvidenceHasOneRequirementsAuthority(t *testing.T) {
	decision := buildShadowInvestigationRoute("Is the Dell healthy?", testShadowRouterContext())
	if _, exists := reflect.TypeOf(InvestigationRoute{}).FieldByName("Requirements"); exists {
		t.Fatal("stored route descriptor contains a second requirements authority")
	}
	evidence := mustNormalizeInvestigationEvidence(t, InvestigationEvidence{
		Route:        decision.RouteDescriptor(),
		Requirements: decision.Requirements,
	})
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(encoded), `"requirements"`); count != 1 {
		t.Fatalf("serialized requirements authorities=%d: %s", count, encoded)
	}
}

func TestEvidenceNormalizationRewritesAndValidatesProvenanceGraph(t *testing.T) {
	collected := time.Date(2026, 9, 22, 15, 4, 5, 123456789, time.UTC)
	subject := EvidenceSubject{Kind: SubjectApplication, ID: "golf-mullet", Name: "Golf Mullet"}
	in := InvestigationEvidence{
		Route: InvestigationRoute{ID: RouteDeploymentCorrelation, Resolution: RouteResolutionSupported},
		Requirements: []EvidenceRequirement{{
			Type: EvidenceTypeCurrentDeployment, Subject: subject,
			Criticality: CriticalityCritical, Cardinality: CardinalityOne,
			Round: InvestigationRoundInitial,
		}},
		Sources: []SourceRef{{
			Capability: "read_deployment_history", Family: CapabilityFamilyDeploymentHistory,
			Resource: "golf-mullet", Subject: subject, CollectedAt: collected,
			Availability: AvailabilityAvailable,
		}},
		Items: []EvidenceItem{
			{
				ID: "pre_observed", Kind: EvidenceObserved, Type: EvidenceTypeCurrentDeployment,
				Subject: subject, Timestamp: &collected, Freshness: Freshness{State: FreshnessUnknown},
				Payload: EvidencePayload{Fields: []EvidenceField{{
					Name: "commit_sha", Value: EvidenceValue{Kind: EvidenceValueString, String: "0123456789abcdef0123456789abcdef01234567"},
				}}},
			},
			{
				ID: "pre_derived", Kind: EvidenceDerived, Type: EvidenceTypeDeploymentTimeline,
				Subject: subject, Freshness: Freshness{State: FreshnessUnknown},
				Payload: EvidencePayload{Fields: []EvidenceField{{
					Name: "changed", Value: EvidenceValue{Kind: EvidenceValueBoolean, Boolean: boolPointer(true)},
				}}},
				Inputs: []string{"pre_observed"},
			},
		},
		Derivations: []Derivation{{
			Operation: "deployment_change", PolicyID: "deployment-change.v1",
			Inputs: []string{"pre_observed"}, OutputID: "pre_derived",
		}},
		Missing: []MissingEvidence{{
			Criticality: CriticalityRelevant, Capability: "read_application_history",
			Availability: AvailabilityUnavailable, ErrorCategory: EvidenceErrorUnavailable,
		}},
		Conflicts: []EvidenceConflict{{
			Subject: subject, Field: "deployment_state",
			EvidenceIDs: []string{"pre_derived", "pre_observed"}, Material: true,
		}},
	}

	normalized := mustNormalizeInvestigationEvidence(t, in)
	if err := validateInvestigationEvidenceGraph(normalized); err != nil {
		t.Fatalf("normalized graph invalid: %v", err)
	}
	if len(normalized.Requirements) != 1 || len(normalized.Sources) != 1 || len(normalized.Items) != 2 {
		t.Fatalf("normalized graph lost nodes: %+v", normalized)
	}
	requirementID := normalized.Requirements[0].ID
	sourceID := normalized.Sources[0].ID
	itemsByKind := map[EvidenceKind]EvidenceItem{}
	for _, item := range normalized.Items {
		itemsByKind[item.Kind] = item
		if item.Relevance.RequirementID != requirementID {
			t.Fatalf("item requirement reference=%q want %q", item.Relevance.RequirementID, requirementID)
		}
	}
	observed := itemsByKind[EvidenceObserved]
	derived := itemsByKind[EvidenceDerived]
	if observed.SourceID != sourceID || len(derived.Inputs) != 1 || derived.Inputs[0] != observed.ID {
		t.Fatalf("normalized item provenance observed=%+v derived=%+v", observed, derived)
	}
	if normalized.Derivations[0].OutputID != derived.ID ||
		!reflect.DeepEqual(normalized.Derivations[0].Inputs, []string{observed.ID}) {

		t.Fatalf("normalized derivation=%+v", normalized.Derivations[0])
	}
	if normalized.Missing[0].RequirementID != requirementID {
		t.Fatalf("normalized missing=%+v", normalized.Missing[0])
	}
	wantConflictIDs := []string{derived.ID, observed.ID}
	sort.Strings(wantConflictIDs)
	if !reflect.DeepEqual(normalized.Conflicts[0].EvidenceIDs, wantConflictIDs) {
		t.Fatalf("normalized conflict=%+v want IDs=%v", normalized.Conflicts[0], wantConflictIDs)
	}
}

func TestMissingEvidenceNormalizationHasTotalDeterministicOrder(t *testing.T) {
	requirement := EvidenceRequirement{
		ID: "pre_requirement", Type: EvidenceTypeApplicationHistory,
		Subject:     EvidenceSubject{Kind: SubjectApplication, ID: "golf-mullet"},
		Criticality: CriticalityCritical, Cardinality: CardinalityMany,
		Round: InvestigationRoundInitial,
	}
	missing := []MissingEvidence{
		{RequirementID: "pre_requirement", Criticality: CriticalityRelevant, Capability: "read_application_history", Availability: AvailabilityPartial, ErrorCategory: EvidenceErrorTooLarge, Reason: "truncated"},
		{RequirementID: "pre_requirement", Criticality: CriticalityCritical, Capability: "read_application_history", Availability: AvailabilityUnavailable, ErrorCategory: EvidenceErrorTimeout, Reason: "timeout"},
		{RequirementID: "pre_requirement", Criticality: CriticalitySupporting, Capability: "read_application_history", Availability: AvailabilityNotFound, ErrorCategory: EvidenceErrorNotFound, Reason: "not found"},
	}
	reversed := []MissingEvidence{missing[2], missing[1], missing[0]}
	first := mustNormalizeInvestigationEvidence(t, InvestigationEvidence{
		Route:        InvestigationRoute{ID: RouteApplicationPerformance},
		Requirements: []EvidenceRequirement{requirement}, Missing: missing,
	})
	second := mustNormalizeInvestigationEvidence(t, InvestigationEvidence{
		Route:        InvestigationRoute{ID: RouteApplicationPerformance},
		Requirements: []EvidenceRequirement{requirement}, Missing: reversed,
	})
	if !reflect.DeepEqual(first.Missing, second.Missing) {
		t.Fatalf("missing evidence order differs:\n%+v\n%+v", first.Missing, second.Missing)
	}
}

func TestEvidenceContractsPreserveFullSHAAndTimestamp(t *testing.T) {
	const fullSHA = "0123456789abcdef0123456789abcdef01234567"
	timestamp, err := time.Parse(time.RFC3339Nano, "2026-09-22T15:04:05.123456789+02:30")
	if err != nil {
		t.Fatal(err)
	}
	item := EvidenceItem{
		Kind:      EvidenceObserved,
		Type:      EvidenceTypeCurrentDeployment,
		Subject:   EvidenceSubject{Kind: SubjectApplication, ID: "golf-mullet"},
		Timestamp: &timestamp,
		Freshness: Freshness{State: FreshnessUnknown},
		Payload: EvidencePayload{Fields: []EvidenceField{
			{Name: "commit_sha", Value: EvidenceValue{Kind: EvidenceValueString, String: fullSHA}},
			{Name: "activated_at", Value: EvidenceValue{Kind: EvidenceValueTimestamp, Timestamp: &timestamp}},
		}},
	}.WithStableID()
	if item.Payload.Fields[0].Value.Timestamp == nil && item.Payload.Fields[1].Value.Timestamp == nil {
		t.Fatal("typed timestamp was lost")
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), fullSHA) ||
		!strings.Contains(string(encoded), "2026-09-22T15:04:05.123456789+02:30") {
		t.Fatalf("exact SHA or timestamp was shortened: %s", encoded)
	}
}

func TestEvidencePacketBoundsAndConfidenceLevels(t *testing.T) {
	if evidencePacketTargetBytes != 4<<10 || evidencePacketHardLimitBytes != 6<<10 {
		t.Fatalf("packet bounds=%d/%d", evidencePacketTargetBytes, evidencePacketHardLimitBytes)
	}
	if ConfidenceHigh != "High" || ConfidenceMedium != "Medium" || ConfidenceLow != "Low" {
		t.Fatalf("confidence levels=%q/%q/%q", ConfidenceHigh, ConfidenceMedium, ConfidenceLow)
	}
}

func mustNormalizeInvestigationEvidence(t *testing.T, in InvestigationEvidence) InvestigationEvidence {
	t.Helper()
	normalized, err := NormalizeInvestigationEvidence(in)
	if err != nil {
		t.Fatal(err)
	}
	return normalized
}
