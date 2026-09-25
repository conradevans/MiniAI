package main

import (
	"testing"
	"time"
)

func TestSoftwareConfidenceRules(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	critical := testRequirement(RouteCurrentPlatformHealth, EvidenceTypeCurrentPlatformState, CriticalityCritical, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalNow, At: &now, Valid: true})
	supporting := testRequirement(RouteCurrentPlatformHealth, EvidenceTypeHostHistory, CriticalitySupporting, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "24h", Valid: true})
	base := confidenceEvidence(t, now, []EvidenceRequirement{critical}, []EvidenceItem{{
		ID: "critical", Kind: EvidenceObserved, Type: critical.Type, Subject: critical.Subject, SourceID: "source",
		Freshness: Freshness{State: FreshnessFresh, PolicySet: true}, Relevance: Relevance{RequirementID: critical.ID, Criticality: critical.Criticality},
		Payload: EvidencePayload{Fields: []EvidenceField{evidenceStringField("status", "healthy")}},
	}}, nil, nil)
	decision := DecideEvidenceConfidence(base)
	if decision.Level != ConfidenceHigh || decision.SoftwareCeiling != ConfidenceHigh || !decision.Determined {
		t.Fatalf("complete confidence=%+v", decision)
	}

	medium := base
	medium.Requirements = append(medium.Requirements, supporting)
	medium.Missing = []MissingEvidence{{RequirementID: supporting.ID, Criticality: CriticalitySupporting, Availability: AvailabilityUnavailable}}
	decision = DecideEvidenceConfidence(medium)
	if decision.Level != ConfidenceMedium {
		t.Fatalf("noncritical missing confidence=%+v", decision)
	}

	missingCritical := base
	missingCritical.Items = nil
	missingCritical.Missing = []MissingEvidence{{RequirementID: critical.ID, Criticality: CriticalityCritical, Availability: AvailabilityUnavailable}}
	if decision = DecideEvidenceConfidence(missingCritical); decision.Level != ConfidenceLow {
		t.Fatalf("critical missing confidence=%+v", decision)
	}

	conflicted := base
	conflicted.Conflicts = []EvidenceConflict{{ID: "conflict", Material: true, EvidenceIDs: []string{base.Items[0].ID, base.Items[0].ID}}}
	if decision = DecideEvidenceConfidence(conflicted); decision.Level != ConfidenceLow {
		t.Fatalf("material conflict confidence=%+v", decision)
	}

	unknownFreshness := base
	unknownFreshness.Items[0].Freshness = Freshness{State: FreshnessUnknown}
	if decision = DecideEvidenceConfidence(unknownFreshness); decision.Level != ConfidenceLow {
		t.Fatalf("unknown required freshness confidence=%+v", decision)
	}
}

func TestConfidenceHasNoModelInputAndCannotBeRaised(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	requirement := testRequirement(RouteCurrentPlatformHealth, EvidenceTypeCurrentPlatformState, CriticalityCritical, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalNow, At: &now, Valid: true})
	evidence := confidenceEvidence(t, now, []EvidenceRequirement{requirement}, nil, []MissingEvidence{{RequirementID: requirement.ID, Criticality: CriticalityCritical, Availability: AvailabilityUnavailable}}, nil)
	evidence.Confidence = ConfidenceDecision{Level: ConfidenceHigh, SoftwareCeiling: ConfidenceHigh, Determined: true, Reasons: []string{"pretend model opinion"}}
	decision := DecideEvidenceConfidence(evidence)
	if decision.Level != ConfidenceLow || decision.SoftwareCeiling != ConfidenceLow {
		t.Fatalf("preexisting/model-like value raised software result: %+v", decision)
	}
}

func confidenceEvidence(t *testing.T, now time.Time, requirements []EvidenceRequirement, items []EvidenceItem, missing []MissingEvidence, conflicts []EvidenceConflict) InvestigationEvidence {
	t.Helper()
	evidence := InvestigationEvidence{
		SchemaVersion: investigationEvidenceSchemaVersion, NormalizedQuestion: "is the dell healthy",
		Route:        InvestigationRoute{ID: RouteCurrentPlatformHealth, Resolution: RouteResolutionSupported, Frame: QuestionFrame{Temporal: TemporalScope{Kind: TemporalNow, At: &now, Valid: true}}},
		Requirements: requirements, Sources: []SourceRef{{ID: "source", Capability: "get_platform_overview", Family: CapabilityFamilyPlatformCurrent, Subject: EvidenceSubject{Kind: SubjectHost, ID: "dell"}, CollectedAt: now, Availability: AvailabilityAvailable}},
		Items: items, Missing: missing, Conflicts: conflicts,
	}
	normalized, err := NormalizeInvestigationEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	return normalized
}
