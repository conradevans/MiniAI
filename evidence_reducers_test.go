package main

import (
	"strings"
	"testing"
	"time"
)

func TestAllPhase1ReadsExecuteRegisteredReducersBehaviorally(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 123456789, time.UTC)
	reducers := phase2BEvidenceReducers()
	registry := phase0CapabilityRegistry()
	readCount := 0
	for _, capability := range registry.capabilities {
		if capability.Kind != capabilityKindRead {
			continue
		}
		readCount++
		name := capability.Definition.Function.Name
		reducer := reducers[name]
		if reducer == nil {
			t.Errorf("read capability %q has no reducer", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			evidenceType := capability.Metadata.EvidenceTypes[0]
			subject := representativeReducerSubject(name)
			from := now.Add(-time.Hour)
			window := &TemporalScope{Kind: TemporalExplicitWindow, From: &from, To: &now, Valid: true}
			if isCurrentEvidenceType(evidenceType) {
				window = &TemporalScope{Kind: TemporalNow, At: &now, Valid: true}
			}
			requirement := testRequirement(RouteExactCurrentFact, evidenceType, CriticalityRelevant, subject, window)
			result := EvidenceResult{
				RequestID: "behavior-" + name, Capability: name, RequirementIDs: []string{requirement.ID},
				CompletedAt: now, Availability: AvailabilityAvailable, SafeResult: representativeReducerValue(name, now), CostUnits: 1, MaxFanout: 1,
			}
			firstItems, firstDerivations, err := reducer(result, requirement, "source:behavior", "irrelevant question prose")
			if err != nil {
				t.Fatal(err)
			}
			secondItems, secondDerivations, err := reducer(result, requirement, "source:behavior", "different irrelevant prose")
			if err != nil {
				t.Fatal(err)
			}
			if len(firstItems) == 0 || canonicalJSON(firstItems) != canonicalJSON(secondItems) || canonicalJSON(firstDerivations) != canonicalJSON(secondDerivations) {
				t.Fatalf("nondeterministic or empty reduction: items=%s derivations=%s", canonicalJSON(firstItems), canonicalJSON(firstDerivations))
			}
			for _, item := range firstItems {
				if item.SourceID != "source:behavior" || item.Kind == EvidenceInferred {
					t.Fatalf("provenance/classification lost: %+v", item)
				}
			}
			encoded := canonicalJSON(firstItems)
			for _, forbidden := range []string{"raw-secret-forbidden", "authorization: bearer raw-token"} {
				if strings.Contains(strings.ToLower(encoded), forbidden) {
					t.Fatalf("forbidden raw value survived: %s", encoded)
				}
			}

			unavailable := result
			unavailable.Availability = AvailabilityUnavailable
			unavailable.SafeResult = nil
			evidence := reduceTestResults(t, RouteExactCurrentFact, []EvidenceRequirement{requirement}, []EvidenceResult{unavailable})
			if len(evidence.Items) != 0 || len(evidence.Missing) != 1 || evidence.Missing[0].Availability != AvailabilityUnavailable {
				t.Fatalf("unavailable behavior=%+v", evidence)
			}
		})
	}
	if readCount != 18 || len(reducers) != 18 {
		t.Fatalf("read/reducer count=%d/%d", readCount, len(reducers))
	}
}

func representativeReducerSubject(capability string) EvidenceSubject {
	switch capability {
	case "get_platform_overview", "read_host_history", "read_temperature_history", "read_infrastructure_events", "read_activity", "read_recovery", "read_service_history", "list_apps":
		return EvidenceSubject{Kind: SubjectHost, ID: "dell"}
	case "list_databases", "read_database_backups":
		return EvidenceSubject{Kind: SubjectDatabase, ID: "db-1", Name: "primary"}
	case "list_repository_directory", "search_repository", "read_repository_file":
		return EvidenceSubject{Kind: SubjectRepository, ID: "myscheduler"}
	default:
		return EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler", Name: "MyScheduler"}
	}
}

func representativeReducerValue(capability string, now time.Time) any {
	from := now.Add(-time.Hour)
	window := reactorLabWindow{Range: "1h", From: from, To: now, BucketSeconds: 60, MaxPoints: 60}
	cpu := 10.0
	switch capability {
	case "get_platform_overview":
		return reactorLabOverview{
			CollectedAt: now,
			System:      reactorLabSection[reactorLabSystemSnapshot]{Available: true, Data: &reactorLabSystemSnapshot{CPU: reactorLabCPUState{UsagePercent: 10}, CollectedAt: now}},
			Recovery:    reactorLabSection[reactorLabRecovery]{Available: true}, Deployments: reactorLabSection[reactorLabDeploymentList]{Available: true},
			Databases: reactorLabSection[reactorLabDatabaseList]{Available: true}, Observability: reactorLabSection[reactorLabObservabilityState]{Available: true},
		}
	case "list_apps":
		return reactorLabAppListResult{Apps: []reactorLabAppSummary{{App: "myscheduler", Status: "running", ActivatedAt: &from}}, TotalApps: 1}
	case "get_app_context":
		return reactorLabAppContextResult{
			App: "myscheduler", Deployment: reactorLabDeployment{App: "myscheduler", Status: "running", ActivatedAt: &from},
			Overview: reactorLabContextOverview{Available: true, CollectedAt: &now, System: reactorLabSection[reactorLabSystemSnapshot]{Available: true}, Recovery: reactorLabSection[reactorLabRecovery]{Available: true}, Observability: reactorLabSection[reactorLabObservabilityState]{Available: true}},
		}
	case "read_host_history":
		return reactorLabHostHistory{Window: window, Points: []reactorLabHostPoint{{Timestamp: from, SampleCount: 1, CPUAverage: &cpu}}, TotalPoints: 1}
	case "read_temperature_history":
		return reactorLabTemperatureHistory{Window: window, Points: []reactorLabTemperaturePoint{{BucketStart: from, BucketEnd: now, SampleCount: 1, MinCelsius: 50, AvgCelsius: 55, MaxCelsius: 60, PeakAt: from}}, TotalPoints: 1}
	case "read_application_history":
		return reactorLabResolvedApplicationHistory{Application: reactorLabApplicationSummary{ID: "app-1", Name: "myscheduler", LatestStatus: "running", LastObservedAt: now}, History: reactorLabApplicationHistory{Window: window, ID: "app-1", Points: []reactorLabApplicationPoint{{Timestamp: from, SampleCount: 1, CPUAverage: 10, Status: "running"}}, TotalPoints: 1}}
	case "read_service_history":
		return reactorLabServiceHistory{Window: window, Services: []reactorLabServiceSeries{{ID: "svc-1", Name: "worker", Points: []reactorLabServicePoint{{Timestamp: from, SampleCount: 1, Available: true, Status: "running"}}}}, TotalServices: 1}
	case "read_infrastructure_events":
		return reactorLabEvents{Window: window, Events: []reactorLabEvent{{ID: "event-1", OccurredAt: from, Type: "restart", Summary: "safe event"}}, TotalEvents: 1}
	case "list_databases":
		return reactorLabDatabaseList{CollectedAt: now, Databases: []reactorLabDatabase{{ID: "db-1", DisplayName: "primary", Status: "ready"}}, TotalDatabases: 1}
	case "read_database_backups":
		return reactorLabBackupList{DatabaseID: "db-1", Backups: []reactorLabBackup{{ID: "backup-1", DatabaseID: "db-1", Status: "complete", CreatedAt: from}}, TotalBackups: 1}
	case "read_activity":
		return reactorLabActivity{Events: []reactorLabActivityEvent{{EventID: "activity-1", OccurredAt: from, Kind: "restart", Message: "safe"}}, TotalEvents: 1}
	case "read_recovery":
		return reactorLabRecovery{HistoryAvailable: true, RecentIncidents: []reactorLabRecoveryIncident{{EventID: "incident-1", LastKnownAliveAt: from, RecoveredAt: now, DowntimeSeconds: 3600, Status: "recovered"}}}
	case "list_repository_directory":
		return repoListResponse{App: "myscheduler", Path: ".", Entries: []repoEntry{{Name: "main.go", Path: "main.go", Type: "file"}}}
	case "search_repository":
		return repoSearchResponse{App: "myscheduler", Path: ".", Query: "schedule", Hits: []repoSearchHit{{Path: "main.go", Line: 1, Text: "safe match"}}, FilesScanned: 1}
	case "read_repository_file":
		return repoFileResponse{App: "myscheduler", Path: "main.go", Content: "password=raw-secret-forbidden\nsafe code", SizeBytes: 39}
	case "read_runtime_logs":
		return logToolResponse{App: "myscheduler", Kind: "runtime", Logs: from.Format(time.RFC3339Nano) + " ERROR password=raw-secret-forbidden full raw log payload", Lines: 1}
	case "read_deployment_logs":
		return logToolResponse{App: "myscheduler", Kind: "deployment", Logs: from.Format(time.RFC3339Nano) + " ERROR authorization: bearer raw-token full raw log payload", Lines: 1}
	case "read_deployment_history":
		return reactorLabDeploymentHistory{App: "myscheduler", Versions: []reactorLabDeploymentVersion{{App: "myscheduler", ActivatedAt: &from, ArchivedAt: now, ImageID: "sha256:safe"}}, TotalVersions: 1}
	default:
		panic("missing representative result for " + capability)
	}
}

func TestPlatformAndMetricReducersProduceTypedObservedAndDerivedEvidence(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	platformRequirement := testRequirement(RouteCurrentPlatformHealth, EvidenceTypeCurrentPlatformState, CriticalityCritical, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalNow, At: &now, Valid: true})
	overview := reactorLabOverview{CollectedAt: now, System: reactorLabSection[reactorLabSystemSnapshot]{Available: true, Data: &reactorLabSystemSnapshot{
		CPU: reactorLabCPUState{UsagePercent: 12.5, Load1: 0.25}, Memory: reactorLabMemoryState{UsagePercent: 40},
		Disk: reactorLabDiskState{UsagePercent: 55}, Temperature: reactorLabTemperatureState{Celsius: 62.25}, UptimeSeconds: 1234,
	}}, Recovery: reactorLabSection[reactorLabRecovery]{Available: true}}
	evidence := reduceTestResults(t, RouteCurrentPlatformHealth, []EvidenceRequirement{platformRequirement}, []EvidenceResult{{
		RequestID: "overview", Capability: "get_platform_overview", RequirementIDs: []string{platformRequirement.ID},
		CompletedAt: now, Availability: AvailabilityAvailable, SafeResult: overview, CostUnits: 1, MaxFanout: 1,
	}})
	if len(evidence.Items) != 1 || evidence.Items[0].Kind != EvidenceObserved || evidence.Items[0].Freshness.State != FreshnessFresh {
		t.Fatalf("platform evidence=%+v", evidence.Items)
	}
	if fieldText(evidence.Items[0], "temperature_celsius") != "62.25" {
		t.Fatalf("temperature field missing: %+v", evidence.Items[0].Payload.Fields)
	}

	from := now.Add(-2 * time.Hour)
	cpuA, cpuB, cpuMaxA, cpuMaxB := 10.0, 30.0, 20.0, 55.0
	hostRequirement := testRequirement(RouteThermalInvestigation, EvidenceTypeHostHistory, CriticalityCritical, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalExplicitWindow, From: &from, To: &now, Valid: true})
	host := reactorLabHostHistory{Window: reactorLabWindow{From: from, To: now, BucketSeconds: 60, MaxPoints: 240}, Points: []reactorLabHostPoint{
		{Timestamp: from, SampleCount: 1, CPUAverage: &cpuA, CPUMaximum: &cpuMaxA, MemoryUsedAverage: 100},
		{Timestamp: now, SampleCount: 1, CPUAverage: &cpuB, CPUMaximum: &cpuMaxB, MemoryUsedAverage: 300},
	}, TotalPoints: 2}
	evidence = reduceTestResults(t, RouteThermalInvestigation, []EvidenceRequirement{hostRequirement}, []EvidenceResult{{
		RequestID: "host", Capability: "read_host_history", RequirementIDs: []string{hostRequirement.ID},
		CompletedAt: now, Availability: AvailabilityAvailable, SafeResult: host, CostUnits: 1, MaxFanout: 1,
	}})
	if len(evidence.Items) != 2 || len(evidence.Derivations) != 1 {
		t.Fatalf("host reduction=%+v derivations=%+v", evidence.Items, evidence.Derivations)
	}
	derived := evidenceItemByKind(t, evidence.Items, EvidenceDerived)
	if fieldText(derived, "cpu_average") != "20" || fieldText(derived, "cpu_peak") != "55" || fieldText(derived, "cpu_delta") != "20" {
		t.Fatalf("host summary=%+v", derived.Payload.Fields)
	}
}

func TestThermalTimelineDeploymentDatabaseLogAndRepositoryReducers(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	from := now.Add(-time.Hour)
	thermalRequirement := testRequirement(RouteThermalInvestigation, EvidenceTypeThermalHistory, CriticalityCritical, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalExplicitWindow, From: &from, To: &now, Valid: true})
	peakAt := from.Add(35 * time.Minute)
	thermal := reactorLabTemperatureHistory{Window: reactorLabWindow{From: from, To: now}, Points: []reactorLabTemperaturePoint{
		{BucketStart: from, BucketEnd: from.Add(30 * time.Minute), SampleCount: 2, MinCelsius: 50, AvgCelsius: 55, MaxCelsius: 60, PeakAt: from.Add(10 * time.Minute)},
		{BucketStart: from.Add(30 * time.Minute), BucketEnd: now, SampleCount: 2, MinCelsius: 52, AvgCelsius: 60, MaxCelsius: 72.125, PeakAt: peakAt},
	}, TotalPoints: 2}
	evidence := reduceTestResults(t, RouteThermalInvestigation, []EvidenceRequirement{thermalRequirement}, []EvidenceResult{{
		RequestID: "thermal", Capability: "read_temperature_history", RequirementIDs: []string{thermalRequirement.ID}, CompletedAt: now,
		Availability: AvailabilityAvailable, SafeResult: thermal, CostUnits: 1, MaxFanout: 1,
	}})
	derived := evidenceItemByKind(t, evidence.Items, EvidenceDerived)
	if fieldText(derived, "maximum_celsius") != "72.125" || fieldText(derived, "peak_at") != peakAt.Format(time.RFC3339Nano) {
		t.Fatalf("thermal peak not exact: %+v", derived.Payload.Fields)
	}

	app := EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler", Name: "MyScheduler"}
	deploymentRequirement := testRequirement(RouteDeploymentCorrelation, EvidenceTypeDeploymentTimeline, CriticalityCritical, app, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "7d", Valid: true})
	eventRequirement := testRequirement(RouteDeploymentCorrelation, EvidenceTypeInfrastructureEvents, CriticalityRelevant, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "7d", Valid: true})
	deployedAt, eventAt := now.Add(-2*time.Minute), now.Add(-73*time.Second)
	sha := "3c208ad5c485af8e6e46a77a0039dd32523e32fe"
	history := reactorLabDeploymentHistory{App: "myscheduler", Versions: []reactorLabDeploymentVersion{{
		App: "myscheduler", Source: &reactorLabDeploymentSource{Repository: "owner/myscheduler", Branch: "main", RequestedRef: "main", CommitSHA: sha},
		ActivatedAt: &deployedAt, ArchivedAt: now.Add(-time.Minute), ImageID: "sha256:exact",
	}}, TotalVersions: 1}
	events := reactorLabEvents{Window: reactorLabWindow{From: now.Add(-7 * 24 * time.Hour), To: now}, Events: []reactorLabEvent{{ID: "event-1", OccurredAt: eventAt, Type: "outage", Summary: "host outage"}}, TotalEvents: 1}
	evidence = reduceTestResults(t, RouteDeploymentCorrelation, []EvidenceRequirement{deploymentRequirement, eventRequirement}, []EvidenceResult{
		{RequestID: "deploy", Capability: "read_deployment_history", RequirementIDs: []string{deploymentRequirement.ID}, CompletedAt: now, Availability: AvailabilityAvailable, SafeResult: history, CostUnits: 1, MaxFanout: 1},
		{RequestID: "events", Capability: "read_infrastructure_events", RequirementIDs: []string{eventRequirement.ID}, CompletedAt: now, Availability: AvailabilityAvailable, SafeResult: events, CostUnits: 1, MaxFanout: 1},
	})
	encoded := canonicalJSON(evidence)
	if !strings.Contains(encoded, sha) || !strings.Contains(encoded, "owner/myscheduler") || !strings.Contains(encoded, deployedAt.Format(time.RFC3339)) {
		t.Fatalf("deployment identity lost: %s", encoded)
	}
	if !strings.Contains(encoded, "temporal_alignment") || strings.Contains(strings.ToLower(encoded), "caused outage") {
		t.Fatalf("temporal relationship missing or causal: %s", encoded)
	}

	databaseRequirement := testRequirement(RouteDatabaseInvestigation, EvidenceTypeCurrentDatabase, CriticalityCritical, EvidenceSubject{Kind: SubjectDatabase, ID: "db-1", Name: "primary"}, &TemporalScope{Kind: TemporalNow, At: &now, Valid: true})
	backupRequirement := testRequirement(RouteDatabaseInvestigation, EvidenceTypeDatabaseBackups, CriticalityRelevant, EvidenceSubject{Kind: SubjectDatabase, ID: "db-1", Name: "primary"}, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "7d", Valid: true})
	completed := now.Add(-time.Minute)
	evidence = reduceTestResults(t, RouteDatabaseInvestigation, []EvidenceRequirement{databaseRequirement, backupRequirement}, []EvidenceResult{
		{RequestID: "db", Capability: "list_databases", RequirementIDs: []string{databaseRequirement.ID}, CompletedAt: now, Availability: AvailabilityAvailable, SafeResult: reactorLabDatabaseList{CollectedAt: now, Databases: []reactorLabDatabase{{ID: "db-1", DisplayName: "primary", Status: "ready", LatestBackupAt: &completed}}, TotalDatabases: 1}, CostUnits: 1, MaxFanout: 1},
		{RequestID: "backups", Capability: "read_database_backups", RequirementIDs: []string{backupRequirement.ID}, CompletedAt: now, Availability: AvailabilityAvailable, SafeResult: reactorLabBackupList{DatabaseID: "db-1", Backups: []reactorLabBackup{{ID: "backup-exact", DatabaseID: "db-1", Status: "complete", CreatedAt: completed, CompletedAt: &completed}}, TotalBackups: 1}, CostUnits: 1, MaxFanout: 1},
	})
	encoded = canonicalJSON(evidence)
	for _, exact := range []string{"db-1", "primary", "backup-exact", completed.Format(time.RFC3339)} {
		if !strings.Contains(encoded, exact) {
			t.Fatalf("database exact value %q missing: %s", exact, encoded)
		}
	}

	logRequirement := testRequirement(RouteApplicationCurrent, EvidenceTypeRuntimeFailures, CriticalityRelevant, app, nil)
	logs := logToolResponse{App: "myscheduler", Kind: "runtime", Lines: 2, Logs: now.Format(time.RFC3339) + " ERROR password=super-secret failed\n" + now.Format(time.RFC3339) + " restarted", Redacted: false}
	evidence = reduceTestResults(t, RouteApplicationCurrent, []EvidenceRequirement{logRequirement}, []EvidenceResult{{RequestID: "logs", Capability: "read_runtime_logs", RequirementIDs: []string{logRequirement.ID}, CompletedAt: now, Availability: AvailabilityAvailable, SafeResult: logs, CostUnits: 1, MaxFanout: 1}})
	encoded = canonicalJSON(evidence)
	if strings.Contains(encoded, "super-secret") || strings.Contains(encoded, logs.Logs) || !strings.Contains(encoded, "error_patterns") {
		t.Fatalf("logs were not bounded/redacted: %s", encoded)
	}

	repositoryRequirement := testRequirement(RouteRepositoryInvestigation, EvidenceTypeRepositoryContent, CriticalityRelevant, EvidenceSubject{Kind: SubjectRepository, ID: "myscheduler"}, nil)
	longContent := "token=secret-value\n" + strings.Repeat("safe source line\n", 200)
	evidence = reduceTestResults(t, RouteRepositoryInvestigation, []EvidenceRequirement{repositoryRequirement}, []EvidenceResult{{RequestID: "repo", Capability: "read_repository_file", RequirementIDs: []string{repositoryRequirement.ID}, CompletedAt: now, Availability: AvailabilityAvailable, SafeResult: repoFileResponse{App: "myscheduler", Path: "internal/schedule.go", SizeBytes: len(longContent), Content: longContent}, CostUnits: 1, MaxFanout: 1}})
	encoded = canonicalJSON(evidence)
	if strings.Contains(encoded, "secret-value") || !strings.Contains(encoded, "internal/schedule.go") || len([]rune(fieldText(evidence.Items[0], "safe_snippet"))) > phase2BRepositoryFileRunes {
		t.Fatalf("repository reduction unsafe or unbounded: %s", encoded)
	}
}

func TestReducersPreserveMissingPartialAndDeterministicConflicts(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	requirement := testRequirement(RouteThermalInvestigation, EvidenceTypeHostHistory, CriticalityCritical, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "24h", Valid: true})
	evidence := reduceTestResults(t, RouteThermalInvestigation, []EvidenceRequirement{requirement}, []EvidenceResult{{
		RequestID: "partial", Capability: "read_host_history", RequirementIDs: []string{requirement.ID}, CompletedAt: now,
		Availability: AvailabilityPartial, SafeResult: reactorLabHostHistory{Truncated: true, TotalPoints: 500}, Truncation: EvidenceTruncation{Truncated: true, Returned: 240, Total: 500}, CostUnits: 1, MaxFanout: 1,
	}})
	if len(evidence.Missing) != 1 || evidence.Missing[0].Availability != AvailabilityPartial || !evidence.Sources[0].Truncation.Truncated {
		t.Fatalf("partial state not explicit: %+v", evidence)
	}

	subject := EvidenceSubject{Kind: SubjectApplication, ID: "app"}
	items := []EvidenceItem{
		{ID: "one", Kind: EvidenceObserved, Type: EvidenceTypeCurrentApplication, Subject: subject, Payload: EvidencePayload{Fields: []EvidenceField{evidenceStringField("commit_sha", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}}},
		{ID: "two", Kind: EvidenceObserved, Type: EvidenceTypeCurrentApplication, Subject: subject, Payload: EvidencePayload{Fields: []EvidenceField{evidenceStringField("commit_sha", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")}}},
	}
	conflicts := detectDeterministicEvidenceConflicts(items)
	if len(conflicts) != 1 || !conflicts[0].Material || len(conflicts[0].EvidenceIDs) != 2 {
		t.Fatalf("conflict=%+v", conflicts)
	}
}

func TestReducersDoNotWidenWindowsForLimitOnlyTimelineAPIs(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	requirement := testRequirement(RouteRestartInvestigation, EvidenceTypeActivityTimeline, CriticalityRelevant, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "1h", Valid: true})
	recent, old := now.Add(-10*time.Minute), now.Add(-2*time.Hour)
	evidence := reduceTestResults(t, RouteRestartInvestigation, []EvidenceRequirement{requirement}, []EvidenceResult{{
		RequestID: "activity", Capability: "read_activity", RequirementIDs: []string{requirement.ID}, CompletedAt: now,
		Availability: AvailabilityAvailable, CostUnits: 1, MaxFanout: 1,
		SafeResult: reactorLabActivity{Events: []reactorLabActivityEvent{{EventID: "recent", OccurredAt: recent}, {EventID: "old-outside-window", OccurredAt: old}}, TotalEvents: 2},
	}})
	encoded := canonicalJSON(evidence)
	foundOutside := false
	for _, field := range evidence.Items[0].Payload.Fields {
		if field.Name == "outside_requested_window" && field.Value.Integer != nil && *field.Value.Integer == 1 {
			foundOutside = true
		}
	}
	if strings.Contains(encoded, "old-outside-window") || !strings.Contains(encoded, "recent") || !foundOutside {
		t.Fatalf("timeline window was widened: %s", encoded)
	}
}

func testRequirement(route InvestigationRouteID, evidenceType EvidenceType, criticality RequirementCriticality, subject EvidenceSubject, window *TemporalScope) EvidenceRequirement {
	return (EvidenceRequirement{Type: evidenceType, Subject: subject, Window: window, Criticality: criticality, Cardinality: CardinalityMany, Round: InvestigationRoundInitial}).WithStableID(string(route))
}

func reduceTestResults(t *testing.T, routeID InvestigationRouteID, requirements []EvidenceRequirement, results []EvidenceResult) InvestigationEvidence {
	t.Helper()
	frame := QuestionFrame{Subjects: []EvidenceSubject{}, Temporal: TemporalScope{Kind: TemporalUnspecified, Valid: true}}
	for _, requirement := range requirements {
		frame.Subjects = append(frame.Subjects, requirement.Subject)
	}
	evidence, err := ReduceEvidenceResults(InvestigationRoute{ID: routeID, Resolution: RouteResolutionSupported, Frame: frame}, "test question", requirements, EvidencePlan{}, results, phase0CapabilityRegistry())
	if err != nil {
		t.Fatal(err)
	}
	return evidence
}

func evidenceItemByKind(t *testing.T, items []EvidenceItem, kind EvidenceKind) EvidenceItem {
	t.Helper()
	for _, item := range items {
		if item.Kind == kind {
			return item
		}
	}
	t.Fatalf("no %s item in %+v", kind, items)
	return EvidenceItem{}
}

func fieldText(item EvidenceItem, name string) string {
	for _, field := range item.Payload.Fields {
		if field.Name != name {
			continue
		}
		switch field.Value.Kind {
		case EvidenceValueString:
			return field.Value.String
		case EvidenceValueDecimal:
			return field.Value.Decimal
		case EvidenceValueTimestamp:
			if field.Value.Timestamp != nil {
				return field.Value.Timestamp.UTC().Format(time.RFC3339Nano)
			}
		}
	}
	return ""
}

func TestCompoundPartialFacetsRemainVisibleAndBoundConfidence(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	host := EvidenceSubject{Kind: SubjectHost, ID: "dell"}
	requirement := testRequirement(RouteCurrentPlatformHealth, EvidenceTypeCurrentPlatformState, CriticalityCritical, host, &TemporalScope{Kind: TemporalNow, At: &now, Valid: true})
	completeOptional := reactorLabOverview{
		CollectedAt: now,
		Recovery:    reactorLabSection[reactorLabRecovery]{Available: true}, Deployments: reactorLabSection[reactorLabDeploymentList]{Available: true},
		Databases: reactorLabSection[reactorLabDatabaseList]{Available: true}, Observability: reactorLabSection[reactorLabObservabilityState]{Available: true},
	}
	criticalMissing := completeOptional
	criticalMissing.System = reactorLabSection[reactorLabSystemSnapshot]{Available: false, Error: "system unavailable"}
	evidence := reduceTestResults(t, RouteCurrentPlatformHealth, []EvidenceRequirement{requirement}, []EvidenceResult{{
		RequestID: "overview-critical", Capability: "get_platform_overview", RequirementIDs: []string{requirement.ID}, CompletedAt: now,
		Availability: AvailabilityAvailable, SafeResult: criticalMissing, CostUnits: 1, MaxFanout: 1,
	}})
	evidence = ApplyEvidenceConfidence(evidence)
	if evidence.Confidence.Level != ConfidenceLow || evidence.Sources[0].Availability != AvailabilityPartial || len(evidence.Missing) != 1 || evidence.Missing[0].Criticality != CriticalityCritical {
		t.Fatalf("critical compound partial=%+v", evidence)
	}

	optionalMissing := completeOptional
	optionalMissing.System = reactorLabSection[reactorLabSystemSnapshot]{Available: true, Data: &reactorLabSystemSnapshot{CollectedAt: now}}
	optionalMissing.Recovery = reactorLabSection[reactorLabRecovery]{Available: false, Error: "optional recovery unavailable"}
	evidence = reduceTestResults(t, RouteCurrentPlatformHealth, []EvidenceRequirement{requirement}, []EvidenceResult{{
		RequestID: "overview-optional", Capability: "get_platform_overview", RequirementIDs: []string{requirement.ID}, CompletedAt: now,
		Availability: AvailabilityAvailable, SafeResult: optionalMissing, CostUnits: 1, MaxFanout: 1,
	}})
	evidence = ApplyEvidenceConfidence(evidence)
	if evidence.Confidence.Level != ConfidenceMedium || len(evidence.Items) != 1 || len(evidence.Missing) != 1 || evidence.Missing[0].Criticality != CriticalitySupporting {
		t.Fatalf("optional compound partial=%+v", evidence)
	}
	packet, err := BuildEvidencePacket(evidence)
	if err != nil {
		t.Fatal(err)
	}
	packetJSON, err := MarshalEvidencePacket(packet)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(packetJSON), "recovery section unavailable") || !strings.Contains(string(packetJSON), `"availability":"partial"`) {
		t.Fatalf("partial facet absent from packet: %s", packetJSON)
	}

	app := EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler", Name: "MyScheduler"}
	appRequirement := testRequirement(RouteApplicationCurrent, EvidenceTypeCurrentApplication, CriticalityCritical, app, &TemporalScope{Kind: TemporalNow, At: &now, Valid: true})
	contextResult := reactorLabAppContextResult{
		App: "myscheduler", Deployment: reactorLabDeployment{App: "myscheduler", Status: "running"},
		Overview:  reactorLabContextOverview{Available: true, CollectedAt: &now, System: reactorLabSection[reactorLabSystemSnapshot]{Available: true}, Recovery: reactorLabSection[reactorLabRecovery]{Available: true}, Observability: reactorLabSection[reactorLabObservabilityState]{Available: true}},
		Databases: []reactorLabDatabase{{ID: "db-ok"}}, DatabaseLookupsRequested: 3, DatabaseLookupsTruncated: true,
		DatabaseLookupFailures: []reactorLabDatabaseLookupFailure{{DatabaseID: "db-failed", Error: "backend unavailable"}},
	}
	evidence = reduceTestResults(t, RouteApplicationCurrent, []EvidenceRequirement{appRequirement}, []EvidenceResult{{
		RequestID: "app-context", Capability: "get_app_context", RequirementIDs: []string{appRequirement.ID}, CompletedAt: now,
		Availability: AvailabilityAvailable, SafeResult: contextResult, CostUnits: 7, MaxFanout: 7,
	}})
	evidence = ApplyEvidenceConfidence(evidence)
	encoded := canonicalJSON(evidence)
	if evidence.Confidence.Level != ConfidenceMedium || evidence.Sources[0].Availability != AvailabilityPartial ||
		!strings.Contains(encoded, "db-failed") || !strings.Contains(encoded, "database_coverage_complete") || !strings.Contains(encoded, "database lookup coverage truncated") {
		t.Fatalf("app-context partial facets lost: %s", encoded)
	}
}

func TestDeploymentCorrelationUsesActivationRatherThanArchiveOrdering(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	app := EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler"}
	deploymentRequirement := testRequirement(RouteDeploymentCorrelation, EvidenceTypeDeploymentTimeline, CriticalityCritical, app, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "7d", Valid: true})
	eventRequirement := testRequirement(RouteDeploymentCorrelation, EvidenceTypeInfrastructureEvents, CriticalityRelevant, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "7d", Valid: true})
	olderActivation, correctActivation, eventAt := now.Add(-4*time.Hour), now.Add(-30*time.Minute), now.Add(-10*time.Minute)
	history := reactorLabDeploymentHistory{App: "myscheduler", Versions: []reactorLabDeploymentVersion{
		{App: "myscheduler", ActivatedAt: &olderActivation, ArchivedAt: now.Add(-time.Minute), Source: &reactorLabDeploymentSource{CommitSHA: "archive-newest-activation-old"}},
		{App: "myscheduler", ActivatedAt: &correctActivation, ArchivedAt: now.Add(-2 * time.Hour), Source: &reactorLabDeploymentSource{CommitSHA: "activation-correct-archive-old"}},
	}, TotalVersions: 2}
	events := reactorLabEvents{Events: []reactorLabEvent{{ID: "incident", OccurredAt: eventAt, Type: "outage"}}, TotalEvents: 1}
	evidence := reduceTestResults(t, RouteDeploymentCorrelation, []EvidenceRequirement{deploymentRequirement, eventRequirement}, []EvidenceResult{
		{RequestID: "deployments", PlanOrder: 0, Capability: "read_deployment_history", RequirementIDs: []string{deploymentRequirement.ID}, CompletedAt: now, Availability: AvailabilityAvailable, SafeResult: history, CostUnits: 1, MaxFanout: 1},
		{RequestID: "events", PlanOrder: 1, Capability: "read_infrastructure_events", RequirementIDs: []string{eventRequirement.ID}, CompletedAt: now, Availability: AvailabilityAvailable, SafeResult: events, CostUnits: 1, MaxFanout: 1},
	})
	var alignment EvidenceItem
	for _, item := range evidence.Items {
		if item.Kind == EvidenceDerived && fieldText(item, "temporal_relation") != "" {
			alignment = item
		}
	}
	if fieldText(alignment, "commit_sha") != "activation-correct-archive-old" || fieldText(alignment, "deployment_time") != correctActivation.Format(time.RFC3339Nano) {
		t.Fatalf("wrong deployment correlated: %+v", alignment.Payload.Fields)
	}
	for _, derivation := range evidence.Derivations {
		if derivation.Operation == "temporal_alignment" && !strings.Contains(strings.ToLower(derivation.Explanation), "no causality") {
			t.Fatalf("temporal derivation implies causality: %+v", derivation)
		}
	}

	ambiguousHistory := history
	ambiguousHistory.Versions = []reactorLabDeploymentVersion{
		{App: "myscheduler", ActivatedAt: &correctActivation, ArchivedAt: now.Add(-time.Hour), Source: &reactorLabDeploymentSource{CommitSHA: "one"}},
		{App: "myscheduler", ActivatedAt: &correctActivation, ArchivedAt: now.Add(-2 * time.Hour), Source: &reactorLabDeploymentSource{CommitSHA: "two"}},
	}
	evidence = reduceTestResults(t, RouteDeploymentCorrelation, []EvidenceRequirement{deploymentRequirement, eventRequirement}, []EvidenceResult{
		{RequestID: "deployments", PlanOrder: 0, Capability: "read_deployment_history", RequirementIDs: []string{deploymentRequirement.ID}, CompletedAt: now, Availability: AvailabilityAvailable, SafeResult: ambiguousHistory, CostUnits: 1, MaxFanout: 1},
		{RequestID: "events", PlanOrder: 1, Capability: "read_infrastructure_events", RequirementIDs: []string{eventRequirement.ID}, CompletedAt: now, Availability: AvailabilityAvailable, SafeResult: events, CostUnits: 1, MaxFanout: 1},
	})
	for _, item := range evidence.Items {
		if item.Kind == EvidenceDerived && strings.Contains(fieldText(item, "temporal_relation"), "ambiguous") {
			if fieldText(item, "commit_sha") != "" {
				t.Fatalf("ambiguous deployment fabricated commit: %+v", item)
			}
			return
		}
	}
	t.Fatalf("ambiguous activation was not preserved: %s", canonicalJSON(evidence))
}

func TestCanonicalLogWindowIgnoresQuestionProseAndComputedSummaryIsDerived(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 987654321, time.UTC)
	from, to := now.Add(-time.Hour), now.Add(-30*time.Minute)
	requirement := testRequirement(RouteApplicationCurrent, EvidenceTypeRuntimeFailures, CriticalityRelevant, EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler"}, &TemporalScope{Kind: TemporalExplicitWindow, From: &from, To: &to, Valid: true})
	inside := from.Add(5 * time.Minute)
	value := logToolResponse{App: "myscheduler", Kind: "runtime", Lines: 4, Logs: strings.Join([]string{
		from.Add(-time.Minute).Format(time.RFC3339Nano) + " ERROR outside-before",
		inside.Format(time.RFC3339Nano) + " ERROR inside-window",
		to.Add(time.Minute).Format(time.RFC3339Nano) + " ERROR outside-after",
		"unparseable ERROR cannot-be-bounded",
	}, "\n")}
	result := EvidenceResult{RequestID: "logs", Capability: "read_runtime_logs", CompletedAt: now, Availability: AvailabilityAvailable, SafeResult: value}
	firstItems, firstDerivations, err := reduceLogs(result, requirement, "source:logs", "show logs from yesterday")
	if err != nil {
		t.Fatal(err)
	}
	secondItems, secondDerivations, err := reduceLogs(result, requirement, "source:logs", "show only the last five seconds")
	if err != nil {
		t.Fatal(err)
	}
	if canonicalJSON(firstItems) != canonicalJSON(secondItems) || canonicalJSON(firstDerivations) != canonicalJSON(secondDerivations) {
		t.Fatal("question prose changed canonical TemporalScope filtering")
	}
	observed, derived := firstItems[0], firstItems[1]
	if observed.Kind != EvidenceObserved || derived.Kind != EvidenceDerived || fieldText(derived, "first_error_timestamp") != inside.Format(time.RFC3339Nano) ||
		evidenceIntegerValue(derived, "error_count") != 1 || evidenceIntegerValue(observed, "included_lines") != 1 || evidenceIntegerValue(observed, "unfilterable_lines") != 1 {
		t.Fatalf("log observed/derived split=%s", canonicalJSON(firstItems))
	}
	if len(firstDerivations) != 1 || firstDerivations[0].Inputs[0] != observed.ID || firstDerivations[0].OutputID != derived.ID {
		t.Fatalf("log lineage=%+v", firstDerivations)
	}
}

func TestServiceCalculationsAreDerivedFromObservedPoints(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	from := now.Add(-time.Hour)
	requirement := testRequirement(RouteApplicationPerformance, EvidenceTypeServiceHistory, CriticalityRelevant, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "1h", Valid: true})
	value := reactorLabServiceHistory{Window: reactorLabWindow{Range: "1h", From: from, To: now}, Services: []reactorLabServiceSeries{{ID: "svc", Name: "worker", Points: []reactorLabServicePoint{{Timestamp: from, Available: true, Status: "running"}, {Timestamp: now, Available: false, Status: "failed"}}}}}
	items, derivations, err := reduceServiceHistory(EvidenceResult{RequestID: "services", Capability: "read_service_history", CompletedAt: now, SafeResult: value}, requirement, "source:services", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Kind != EvidenceObserved || items[1].Kind != EvidenceDerived || evidenceFieldMap(items[0].Payload.Fields)["service_summaries"].Kind != "" || evidenceFieldMap(items[1].Payload.Fields)["service_summaries"].Kind == "" {
		t.Fatalf("service observed/derived classification=%s", canonicalJSON(items))
	}
	if len(derivations) != 1 || derivations[0].Inputs[0] != items[0].ID || derivations[0].OutputID != items[1].ID {
		t.Fatalf("service lineage=%+v", derivations)
	}
}

func TestCrossTypeConflictDetectionUsesSemanticFactIdentity(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	later := now.Add(time.Minute)
	subject := EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler", Name: "MyScheduler"}
	item := func(id string, evidenceType EvidenceType, at *time.Time, fields ...EvidenceField) EvidenceItem {
		return EvidenceItem{ID: id, Kind: EvidenceObserved, Type: evidenceType, Subject: subject, Timestamp: at, Payload: EvidencePayload{Fields: fields}}
	}
	left := item("app", EvidenceTypeCurrentApplication, &now, evidenceStringField("commit_sha", "aaaa"), evidenceStringField("image_id", "sha256:a"), evidenceStringField("status", "healthy"))
	right := item("deploy", EvidenceTypeCurrentDeployment, &now, evidenceStringField("commit_sha", "bbbb"), evidenceStringField("image_id", "sha256:b"), evidenceStringField("status", "failed"))
	conflicts := detectDeterministicEvidenceConflicts([]EvidenceItem{left, right})
	if len(conflicts) != 3 || conflicts[0].EvidenceIDs[0] != "app" || conflicts[0].EvidenceIDs[1] != "deploy" {
		t.Fatalf("cross-type conflicts=%+v", conflicts)
	}
	reversed := detectDeterministicEvidenceConflicts([]EvidenceItem{right, left})
	if canonicalJSON(conflicts) != canonicalJSON(reversed) {
		t.Fatalf("conflicts changed with input order:\n%s\n%s", canonicalJSON(conflicts), canonicalJSON(reversed))
	}
	if got := detectDeterministicEvidenceConflicts([]EvidenceItem{left, item("later", EvidenceTypeCurrentDeployment, &later, evidenceStringField("commit_sha", "bbbb"))}); len(got) != 0 {
		t.Fatalf("different-time values conflicted: %+v", got)
	}
	historical := item("historical", EvidenceTypeApplicationHistory, &now, evidenceStringField("status", "down"))
	if got := detectDeterministicEvidenceConflicts([]EvidenceItem{left, historical}); len(got) != 0 {
		t.Fatalf("current and historical states conflicted: %+v", got)
	}
}

func TestDuplicateTimestampMetricPermutationsAreByteStable(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 123456789, time.UTC)
	from := now.Add(-time.Hour)
	window := reactorLabWindow{From: from, To: now, BucketSeconds: 60, MaxPoints: 240}
	scope := &TemporalScope{Kind: TemporalExplicitWindow, From: &from, To: &now, Valid: true}
	timestamp := from.Add(10 * time.Minute)

	cpuLow, cpuHigh := 10.0, 40.0
	hostA := reactorLabHostPoint{Timestamp: timestamp, SampleCount: 1, CPUAverage: &cpuLow, CPUMaximum: &cpuLow, MemoryUsedAverage: 100}
	hostB := reactorLabHostPoint{Timestamp: timestamp, SampleCount: 1, CPUAverage: &cpuHigh, CPUMaximum: &cpuHigh, MemoryUsedAverage: 200}
	assertMetricPermutationsStable(t, RouteThermalInvestigation, "read_host_history",
		testRequirement(RouteThermalInvestigation, EvidenceTypeHostHistory, CriticalityCritical, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, scope),
		[]reactorLabHostPoint{hostA, hostB, hostA}, func(points []reactorLabHostPoint) any {
			return reactorLabHostHistory{Window: window, Points: points, TotalPoints: len(points)}
		})

	temperatureA := reactorLabTemperaturePoint{BucketStart: timestamp, BucketEnd: timestamp.Add(time.Minute), SampleCount: 1, MinCelsius: 50, AvgCelsius: 55, MaxCelsius: 60, PeakAt: timestamp}
	temperatureB := reactorLabTemperaturePoint{BucketStart: timestamp, BucketEnd: timestamp.Add(time.Minute), SampleCount: 1, MinCelsius: 60, AvgCelsius: 65, MaxCelsius: 70, PeakAt: timestamp.Add(time.Second)}
	assertMetricPermutationsStable(t, RouteThermalInvestigation, "read_temperature_history",
		testRequirement(RouteThermalInvestigation, EvidenceTypeThermalHistory, CriticalityCritical, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, scope),
		[]reactorLabTemperaturePoint{temperatureA, temperatureB, temperatureA}, func(points []reactorLabTemperaturePoint) any {
			return reactorLabTemperatureHistory{Window: window, Points: points, TotalPoints: len(points)}
		})

	applicationA := reactorLabApplicationPoint{Timestamp: timestamp, SampleCount: 1, CPUAverage: 10, CPUMaximum: 20, MemoryUsedAverage: 100, Status: "running", RestartCount: 1}
	applicationB := reactorLabApplicationPoint{Timestamp: timestamp, SampleCount: 1, CPUAverage: 40, CPUMaximum: 50, MemoryUsedAverage: 200, Status: "failed", RestartCount: 2}
	assertMetricPermutationsStable(t, RouteApplicationPerformance, "read_application_history",
		testRequirement(RouteApplicationPerformance, EvidenceTypeApplicationHistory, CriticalityCritical, EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler"}, scope),
		[]reactorLabApplicationPoint{applicationA, applicationB, applicationA}, func(points []reactorLabApplicationPoint) any {
			return reactorLabResolvedApplicationHistory{Application: reactorLabApplicationSummary{ID: "app-1", Name: "myscheduler", LatestStatus: "running", LastObservedAt: now}, History: reactorLabApplicationHistory{Window: window, ID: "app-1", Points: points, TotalPoints: len(points)}}
		})
}

func assertMetricPermutationsStable[T any](t *testing.T, route InvestigationRouteID, capability string, requirement EvidenceRequirement, values []T, resultValue func([]T) any) {
	t.Helper()
	var evidenceBaseline, packetBaseline string
	for index, permutation := range allTestPermutations(values) {
		result := EvidenceResult{
			RequestID: "metric", Capability: capability, RequirementIDs: []string{requirement.ID}, PlanOrder: 0,
			CompletedAt: requirement.Window.To.UTC(), Availability: AvailabilityAvailable, SafeResult: resultValue(permutation), CostUnits: 1, MaxFanout: 1,
		}
		evidence := reduceTestResults(t, route, []EvidenceRequirement{requirement}, []EvidenceResult{result})
		evidence = ApplyEvidenceConfidence(evidence)
		packet, err := BuildEvidencePacket(evidence)
		if err != nil {
			t.Fatal(err)
		}
		packetBytes, err := MarshalEvidencePacket(packet)
		if err != nil {
			t.Fatal(err)
		}
		evidenceJSON := canonicalJSON(evidence)
		if index == 0 {
			evidenceBaseline, packetBaseline = evidenceJSON, string(packetBytes)
			continue
		}
		if evidenceJSON != evidenceBaseline || string(packetBytes) != packetBaseline {
			t.Fatalf("permutation %d changed evidence or packet\n%s\n%s\n%s\n%s", index, evidenceBaseline, evidenceJSON, packetBaseline, packetBytes)
		}
	}
	if !strings.Contains(evidenceBaseline, "equal_timestamp_values_canonically_ordered") || !strings.Contains(evidenceBaseline, "deduplicated_point_count") {
		t.Fatalf("equal-time semantics not explicit: %s", evidenceBaseline)
	}
}

func allTestPermutations[T any](values []T) [][]T {
	working := append([]T(nil), values...)
	out := [][]T{}
	var visit func(int)
	visit = func(index int) {
		if index == len(working) {
			out = append(out, append([]T(nil), working...))
			return
		}
		for candidate := index; candidate < len(working); candidate++ {
			working[index], working[candidate] = working[candidate], working[index]
			visit(index + 1)
			working[index], working[candidate] = working[candidate], working[index]
		}
	}
	visit(0)
	return out
}
