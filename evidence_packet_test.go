package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"
)

func TestEvidencePacketCompactsAtTargetAndPreservesCriticalExactFacts(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 123456789, time.UTC)
	critical := testRequirement(RouteDeploymentCorrelation, EvidenceTypeDeploymentTimeline, CriticalityCritical, EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler", Name: "MyScheduler"}, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "7d", Valid: true})
	supporting := testRequirement(RouteDeploymentCorrelation, EvidenceTypeInfrastructureEvents, CriticalitySupporting, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "7d", Valid: true})
	missingRequirement := testRequirement(RouteDeploymentCorrelation, EvidenceTypeApplicationHistory, CriticalityCritical, EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler"}, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "7d", Valid: true})
	shaA := "3c208ad5c485af8e6e46a77a0039dd32523e32fe"
	shaB := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sources := []SourceRef{}
	items := []EvidenceItem{}
	for index, sha := range []string{shaA, shaB} {
		sourceID := fmt.Sprintf("source-%d", index)
		itemID := fmt.Sprintf("critical-%d", index)
		sources = append(sources, SourceRef{ID: sourceID, Capability: "read_deployment_history", Family: CapabilityFamilyDeploymentHistory, Resource: fmt.Sprintf("history-%d", index), Subject: critical.Subject, CollectedAt: now, Availability: AvailabilityAvailable})
		items = append(items, EvidenceItem{
			ID: itemID, Kind: EvidenceObserved, Type: critical.Type, Subject: critical.Subject, Timestamp: &now, SourceID: sourceID,
			Freshness: Freshness{State: FreshnessUnknown}, Relevance: Relevance{RequirementID: critical.ID, Criticality: CriticalityCritical},
			Payload: EvidencePayload{Fields: []EvidenceField{evidenceStringField("commit_sha", sha), timestampField("activated_at", now)}},
		})
	}
	for index := 0; index < 24; index++ {
		sourceID := fmt.Sprintf("support-source-%02d", index)
		sources = append(sources, SourceRef{ID: sourceID, Capability: "read_infrastructure_events", Family: CapabilityFamilyInfrastructureTimeline, Resource: fmt.Sprintf("events-%02d", index), Subject: supporting.Subject, CollectedAt: now, Availability: AvailabilityAvailable})
		items = append(items, EvidenceItem{
			ID: fmt.Sprintf("support-%02d", index), Kind: EvidenceObserved, Type: supporting.Type, Subject: supporting.Subject, Timestamp: &now, SourceID: sourceID,
			Freshness: Freshness{State: FreshnessUnknown}, Relevance: Relevance{RequirementID: supporting.ID, Criticality: CriticalitySupporting},
			Payload: EvidencePayload{Fields: []EvidenceField{evidenceStringField("summary", strings.Repeat(fmt.Sprintf("event-%02d ", index), 35))}},
		})
	}
	evidence := InvestigationEvidence{
		SchemaVersion: investigationEvidenceSchemaVersion, NormalizedQuestion: "did the deployment align with the outage",
		Route:        InvestigationRoute{ID: RouteDeploymentCorrelation, Resolution: RouteResolutionSupported, Frame: QuestionFrame{Goal: GoalCausalAssessment, Subjects: []EvidenceSubject{critical.Subject}, Temporal: *critical.Window}},
		Requirements: []EvidenceRequirement{critical, supporting, missingRequirement}, Sources: sources, Items: items,
		Missing:   []MissingEvidence{{RequirementID: missingRequirement.ID, Criticality: CriticalityCritical, Availability: AvailabilityUnavailable, Reason: "application history unavailable"}},
		Conflicts: []EvidenceConflict{{ID: "conflict", Subject: critical.Subject, Field: "commit_sha", EvidenceIDs: []string{"critical-0", "critical-1"}, Material: true, Explanation: "current commits disagree"}},
	}
	normalized, err := NormalizeInvestigationEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	normalized = ApplyEvidenceConfidence(normalized)
	packet, err := BuildEvidencePacket(normalized)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := MarshalEvidencePacket(packet)
	if err != nil {
		t.Fatal(err)
	}
	if !packet.Compacted || packet.DroppedEvidence == 0 || len(encoded) > evidencePacketHardLimitBytes {
		t.Fatalf("packet compaction=%t dropped=%d bytes=%d", packet.Compacted, packet.DroppedEvidence, len(encoded))
	}
	text := string(encoded)
	for _, exact := range []string{shaA, shaB, now.Format(time.RFC3339Nano), "application history unavailable", "current commits disagree"} {
		if !strings.Contains(text, exact) {
			t.Fatalf("critical exact value %q missing from %s", exact, text)
		}
	}
	for _, forbidden := range []string{"safeResult", "toolDefinitions", "plannerMessages", "raw_logs", "raw_metric_arrays"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("packet contains forbidden field %q: %s", forbidden, text)
		}
	}
}

func TestEvidencePacketHardLimitFailsInsteadOfTruncatingCriticalValue(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	requirement := testRequirement(RouteCurrentPlatformHealth, EvidenceTypeCurrentPlatformState, CriticalityCritical, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalNow, At: &now, Valid: true})
	evidence := InvestigationEvidence{
		NormalizedQuestion: "health", Route: InvestigationRoute{ID: RouteCurrentPlatformHealth, Resolution: RouteResolutionSupported, Frame: QuestionFrame{Subjects: []EvidenceSubject{requirement.Subject}, Temporal: *requirement.Window}},
		Requirements: []EvidenceRequirement{requirement},
		Sources:      []SourceRef{{ID: "source", Capability: "get_platform_overview", Family: CapabilityFamilyPlatformCurrent, Resource: "overview", Subject: requirement.Subject, CollectedAt: now, Availability: AvailabilityAvailable}},
		Items:        []EvidenceItem{{ID: "item", Kind: EvidenceObserved, Type: requirement.Type, Subject: requirement.Subject, SourceID: "source", Freshness: Freshness{State: FreshnessFresh, PolicySet: true}, Relevance: Relevance{RequirementID: requirement.ID, Criticality: CriticalityCritical}, Payload: EvidencePayload{Fields: []EvidenceField{evidenceStringField("exact_critical_value", strings.Repeat("x", evidencePacketHardLimitBytes))}}}},
	}
	normalized, err := NormalizeInvestigationEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	normalized = ApplyEvidenceConfidence(normalized)
	if _, err := BuildEvidencePacket(normalized); !errors.Is(err, errEvidencePacketTooLarge) {
		t.Fatalf("hard-limit error=%v", err)
	}
}

func TestEvidencePacketIsByteStableAcrossInputPermutation(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	requirement := testRequirement(RouteCurrentPlatformHealth, EvidenceTypeCurrentPlatformState, CriticalityCritical, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalNow, At: &now, Valid: true})
	sources := []SourceRef{
		{ID: "source-a", Capability: "get_platform_overview", Family: CapabilityFamilyPlatformCurrent, Resource: "a", Subject: requirement.Subject, CollectedAt: now, Availability: AvailabilityAvailable},
		{ID: "source-b", Capability: "get_platform_overview", Family: CapabilityFamilyPlatformCurrent, Resource: "b", Subject: requirement.Subject, CollectedAt: now, Availability: AvailabilityAvailable},
	}
	items := []EvidenceItem{
		{ID: "item-a", Kind: EvidenceObserved, Type: requirement.Type, Subject: requirement.Subject, SourceID: "source-a", Freshness: Freshness{State: FreshnessFresh, PolicySet: true}, Relevance: Relevance{RequirementID: requirement.ID, Criticality: CriticalityCritical}, Payload: EvidencePayload{Fields: []EvidenceField{evidenceStringField("a", "1")}}},
		{ID: "item-b", Kind: EvidenceObserved, Type: requirement.Type, Subject: requirement.Subject, SourceID: "source-b", Freshness: Freshness{State: FreshnessFresh, PolicySet: true}, Relevance: Relevance{RequirementID: requirement.ID, Criticality: CriticalityCritical}, Payload: EvidencePayload{Fields: []EvidenceField{evidenceStringField("b", "2")}}},
	}
	build := func(sources []SourceRef, items []EvidenceItem) []byte {
		evidence := InvestigationEvidence{NormalizedQuestion: "health", Route: InvestigationRoute{ID: RouteCurrentPlatformHealth, Resolution: RouteResolutionSupported, Frame: QuestionFrame{Subjects: []EvidenceSubject{requirement.Subject}, Temporal: *requirement.Window}}, Requirements: []EvidenceRequirement{requirement}, Sources: sources, Items: items}
		normalized, err := NormalizeInvestigationEvidence(evidence)
		if err != nil {
			t.Fatal(err)
		}
		normalized = ApplyEvidenceConfidence(normalized)
		packet, err := BuildEvidencePacket(normalized)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := MarshalEvidencePacket(packet)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	first := build(sources, items)
	second := build([]SourceRef{sources[1], sources[0]}, []EvidenceItem{items[1], items[0]})
	if !bytes.Equal(first, second) {
		t.Fatalf("packets differ\n%s\n%s", first, second)
	}
}

func TestPhase2BPipelineHasNoProductionCallSites(t *testing.T) {
	implementationFiles := map[string]bool{
		"investigation_router.go": true,
		"evidence_plan.go":        true,
		"evidence_executor.go":    true,
		"evidence_reducers.go":    true,
		"confidence.go":           true,
		"evidence_packet.go":      true,
		"evidence_pipeline.go":    true,
	}
	phase2BNames := map[string]bool{
		"buildShadowInvestigationRoute": true,
		"routePhase2BQuestion":          true,
		"requirementsForRoute":          true,
		"phase2SemanticRouteProfiles":   true,
	}
	for path := range implementationFiles {
		if path == "investigation_router.go" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if declaration.Recv == nil {
					phase2BNames[declaration.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, specification := range declaration.Specs {
					switch specification := specification.(type) {
					case *ast.TypeSpec:
						phase2BNames[specification.Name.Name] = true
					case *ast.ValueSpec:
						for _, name := range specification.Names {
							phase2BNames[name.Name] = true
						}
					}
				}
			}
		}
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		path := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || implementationFiles[path] {
			continue
		}
		checked++
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if ok && phase2BNames[identifier.Name] {
				t.Errorf("production root file %s references Phase 2B identifier %s", path, identifier.Name)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("production isolation guard checked no root Go files")
	}
}

func TestPacketCompactionPreservesRelevantMaterialExactValues(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 987654321, time.UTC)
	subject := EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler", Name: "MyScheduler"}
	relevant := testRequirement(RouteDeploymentCorrelation, EvidenceTypeDeploymentTimeline, CriticalityRelevant, subject, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "7d", Valid: true})
	supporting := testRequirement(RouteDeploymentCorrelation, EvidenceTypeDeploymentTimeline, CriticalitySupporting, subject, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "7d", Valid: true})
	relevantSHA := "3c208ad5c485af8e6e46a77a0039dd32523e32fe"
	sources := []SourceRef{{ID: "material-source", Capability: "read_deployment_history", Family: CapabilityFamilyDeploymentHistory, Subject: subject, CollectedAt: now, Availability: AvailabilityAvailable}}
	items := []EvidenceItem{{
		ID: "material", Kind: EvidenceObserved, Type: EvidenceTypeDeploymentTimeline, Subject: subject, Timestamp: &now, SourceID: "material-source",
		Freshness: Freshness{State: FreshnessUnknown}, Relevance: Relevance{RequirementID: relevant.ID, Criticality: CriticalityRelevant},
		Payload: EvidencePayload{Fields: []EvidenceField{evidenceStringField("commit_sha", relevantSHA), timestampField("activated_at", now)}},
	}}
	irrelevantSHAs := []string{}
	for index := 0; index < 30; index++ {
		sha := fmt.Sprintf("irrelevant-%02d-%040d", index, index)
		irrelevantSHAs = append(irrelevantSHAs, sha)
		sourceID := fmt.Sprintf("irrelevant-source-%02d", index)
		sources = append(sources, SourceRef{ID: sourceID, Capability: "read_deployment_history", Family: CapabilityFamilyDeploymentHistory, Resource: sourceID, Subject: subject, CollectedAt: now, Availability: AvailabilityAvailable})
		items = append(items, EvidenceItem{
			ID: fmt.Sprintf("irrelevant-%02d", index), Kind: EvidenceObserved, Type: EvidenceTypeDeploymentTimeline, Subject: subject, Timestamp: &now, SourceID: sourceID,
			Freshness: Freshness{State: FreshnessUnknown}, Relevance: Relevance{RequirementID: supporting.ID, Criticality: CriticalitySupporting},
			Payload: EvidencePayload{Fields: []EvidenceField{evidenceStringField("commit_sha", sha), evidenceStringField("historical_detail", strings.Repeat("nonmaterial history ", 20))}},
		})
	}
	evidence := InvestigationEvidence{
		SchemaVersion: investigationEvidenceSchemaVersion, NormalizedQuestion: "which deployment preceded the incident",
		Route:        InvestigationRoute{ID: RouteDeploymentCorrelation, Resolution: RouteResolutionSupported, Frame: QuestionFrame{Goal: GoalCausalAssessment, Subjects: []EvidenceSubject{subject}, Temporal: *relevant.Window}},
		Requirements: []EvidenceRequirement{relevant, supporting}, Sources: sources, Items: items,
	}
	normalized, err := NormalizeInvestigationEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	normalized = ApplyEvidenceConfidence(normalized)
	build := func() (EvidencePacket, []byte) {
		packet, err := BuildEvidencePacket(normalized)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := MarshalEvidencePacket(packet)
		if err != nil {
			t.Fatal(err)
		}
		return packet, encoded
	}
	packet, first := build()
	_, second := build()
	if !packet.Compacted || packet.DroppedEvidence == 0 || len(first) > evidencePacketHardLimitBytes || !bytes.Equal(first, second) {
		t.Fatalf("compacted=%t dropped=%d bytes=%d stable=%t", packet.Compacted, packet.DroppedEvidence, len(first), bytes.Equal(first, second))
	}
	encoded := string(first)
	if !strings.Contains(encoded, relevantSHA) || !strings.Contains(encoded, now.Format(time.RFC3339Nano)) || !strings.Contains(encoded, "myscheduler") {
		t.Fatalf("material exact value removed: %s", encoded)
	}
	remainingIrrelevant := 0
	for _, sha := range irrelevantSHAs {
		if strings.Contains(encoded, sha) {
			remainingIrrelevant++
		}
	}
	if remainingIrrelevant == len(irrelevantSHAs) {
		t.Fatalf("irrelevant SHAs were globally protected: %d remain", remainingIrrelevant)
	}
}
