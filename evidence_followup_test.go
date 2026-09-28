package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

type phase2DExecutorCall struct {
	Name      string
	Arguments map[string]any
}

type phase2DTestExecutor struct {
	mu      sync.Mutex
	calls   []phase2DExecutorCall
	handler func(context.Context, string, map[string]any) (any, string, error)
}

func (executor *phase2DTestExecutor) Execute(ctx context.Context, name string, arguments map[string]any) (any, string, error) {
	executor.mu.Lock()
	executor.calls = append(executor.calls, phase2DExecutorCall{Name: name, Arguments: cloneCanonicalArguments(arguments)})
	handler := executor.handler
	executor.mu.Unlock()
	if handler == nil {
		return nil, "", errors.New("unavailable")
	}
	return handler(ctx, name, cloneCanonicalArguments(arguments))
}

func (executor *phase2DTestExecutor) recordedCalls() []phase2DExecutorCall {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]phase2DExecutorCall(nil), executor.calls...)
}

func phase2DNow() time.Time {
	return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
}

func preparePhase2DDatabasePipeline(t *testing.T, executor capabilityExecutor) (Phase2BPipeline, Phase2BPrepared) {
	t.Helper()
	now := phase2DNow()
	pipeline := NewPhase2BPipeline(phase0CapabilityRegistry(), executor, func() time.Time { return now })
	prepared, err := pipeline.Prepare(
		"Are the database backups healthy?",
		ShadowRouterContext{Now: now, Location: time.UTC},
	)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Route.ID != RouteDatabaseInvestigation || len(prepared.Plan.Requests) != 1 || len(prepared.Plan.Missing) != 1 {
		t.Fatalf("unexpected database first round: %+v", prepared)
	}
	return pipeline, prepared
}

func phase2DDatabaseHandler(now time.Time, databases []reactorLabDatabase, backupError error) func(context.Context, string, map[string]any) (any, string, error) {
	return func(_ context.Context, name string, arguments map[string]any) (any, string, error) {
		switch name {
		case "list_databases":
			return reactorLabDatabaseList{CollectedAt: now, Databases: databases, TotalDatabases: len(databases)}, "databases", nil
		case "read_database_backups":
			if backupError != nil {
				return nil, "", backupError
			}
			databaseID := stringArg(arguments, "database_id")
			completed := now.Add(-time.Hour)
			return reactorLabBackupList{
				DatabaseID:   databaseID,
				Backups:      []reactorLabBackup{{ID: "backup-" + databaseID, DatabaseID: databaseID, Status: "complete", CreatedAt: completed, CompletedAt: &completed}},
				TotalBackups: 1,
			}, "backups", nil
		default:
			return nil, "", errors.New("unexpected capability")
		}
	}
}

func TestPhase2DCompletePlatformEvidenceStaysOneRound(t *testing.T) {
	now := phase2DNow()
	executor := &platformPipelineExecutor{}
	pipeline := NewPhase2BPipeline(phase0CapabilityRegistry(), executor, func() time.Time { return now })
	result, err := pipeline.Run(t.Context(), "Is the Dell healthy right now?", ShadowRouterContext{Now: now, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	if result.EvidenceRounds != 1 || result.SecondRoundReads != 0 || result.SecondRound.Needed || executor.calls != 1 {
		t.Fatalf("complete platform evidence triggered follow-up: %+v calls=%d", result.SecondRound, executor.calls)
	}
}

func TestPhase2DCompleteApplicationIncidentEvidenceStaysOneRound(t *testing.T) {
	now := phase2DNow()
	pipeline := NewPhase2BPipeline(phase0CapabilityRegistry(), nil, func() time.Time { return now })
	prepared, err := pipeline.Prepare(
		"Why has MyScheduler been failing recently?",
		ShadowRouterContext{Now: now, Location: time.UTC, Entities: testShadowRouterContext().Entities},
	)
	if err != nil {
		t.Fatal(err)
	}
	decision := PlanSecondEvidenceRound(
		prepared,
		InvestigationEvidence{Requirements: prepared.Route.Requirements},
		prepared.Plan,
		nil,
		pipeline.registry,
	)
	if prepared.Route.ID != RouteApplicationPerformance || prepared.Plan.LogicalReads != 5 ||
		prepared.Plan.CostUnits != 12 || decision.Needed || decision.Reason != FollowUpNoMaterialGap {
		t.Fatalf("complete application incident triggered follow-up: prepared=%+v decision=%+v", prepared, decision)
	}
}

func TestPhase2DTypedDatabaseGapRunsOneBoundedFollowUpRound(t *testing.T) {
	now := phase2DNow()
	executor := &phase2DTestExecutor{}
	executor.handler = phase2DDatabaseHandler(now, []reactorLabDatabase{{ID: "database_123", DisplayName: "Primary", Status: "ready"}}, nil)
	pipeline, prepared := preparePhase2DDatabasePipeline(t, executor)
	result, err := pipeline.ExecutePrepared(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	calls := executor.recordedCalls()
	if result.EvidenceRounds != 2 || result.SecondRoundReads != 1 || len(calls) != 2 ||
		calls[0].Name != "list_databases" || calls[1].Name != "read_database_backups" ||
		stringArg(calls[1].Arguments, "database_id") != "database_123" {
		t.Fatalf("unexpected bounded follow-up: result=%+v calls=%+v", result.SecondRound, calls)
	}
	for _, missing := range result.Evidence.Missing {
		if missing.Capability == "read_database_backups" {
			t.Fatalf("resolved backup gap remained missing: %+v", result.Evidence.Missing)
		}
	}
	if result.Evidence.Confidence.Level != ConfidenceHigh {
		t.Fatalf("resolved material gap did not recompute confidence: %+v", result.Evidence.Confidence)
	}
}

func TestPhase2DNoncriticalGapDoesNotTriggerFollowUp(t *testing.T) {
	now := phase2DNow()
	executor := &platformPipelineExecutor{}
	pipeline := NewPhase2BPipeline(phase0CapabilityRegistry(), executor, func() time.Time { return now })
	prepared, err := pipeline.Prepare("Is the Dell healthy?", ShadowRouterContext{Now: now, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	results, err := NewEvidenceExecutor(pipeline.registry, executor, pipeline.now).Execute(t.Context(), prepared.Plan)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := ReduceEvidenceResults(prepared.Route.RouteDescriptor(), prepared.NormalizedQuestion, prepared.Route.Requirements, prepared.Plan, results, pipeline.registry)
	if err != nil {
		t.Fatal(err)
	}
	evidence.Missing = append(evidence.Missing, MissingEvidence{
		RequirementID: evidence.Requirements[0].ID, Criticality: CriticalitySupporting,
		Capability: "read_recovery", Availability: AvailabilityUnavailable,
	})
	decision := PlanSecondEvidenceRound(prepared, evidence, prepared.Plan, results, pipeline.registry)
	if decision.Needed || decision.Reason != FollowUpNoMaterialGap {
		t.Fatalf("noncritical limitation triggered follow-up: %+v", decision)
	}
}

func TestPhase2DGapWithoutResolvedSubjectDoesNotFollowUp(t *testing.T) {
	now := phase2DNow()
	requirement := EvidenceRequirement{
		Type: EvidenceTypeCurrentApplication, Subject: EvidenceSubject{Kind: SubjectApplication, ID: "unresolved"},
		Criticality: CriticalityCritical, Cardinality: CardinalityOne, Round: InvestigationRoundInitial,
	}.WithStableID(string(RouteApplicationCurrent))
	prepared := Phase2BPrepared{
		Now: now, NormalizedQuestion: "is the application healthy",
		Route: ShadowRouteDecision{ID: RouteApplicationCurrent, Resolution: RouteResolutionSupported, Requirements: []EvidenceRequirement{requirement}},
		Plan:  EvidencePlan{RouteID: string(RouteApplicationCurrent)},
	}
	evidence := InvestigationEvidence{
		Route: prepared.Route.RouteDescriptor(), Requirements: []EvidenceRequirement{requirement},
		Missing: []MissingEvidence{{RequirementID: requirement.ID, Criticality: CriticalityCritical, Capability: "get_app_context", Availability: AvailabilityUnavailable}},
	}
	decision := PlanSecondEvidenceRound(prepared, evidence, prepared.Plan, nil, phase0CapabilityRegistry())
	if decision.Needed || decision.Reason != FollowUpSubjectUnresolved {
		t.Fatalf("unresolved subject follow-up=%+v", decision)
	}
}

func TestPhase2DDuplicateLogicalReadIsNotRepeated(t *testing.T) {
	now := phase2DNow()
	app := EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler"}
	requirement := EvidenceRequirement{
		Type: EvidenceTypeApplicationHistory, Subject: app, Window: &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "24h", Valid: true},
		Criticality: CriticalityCritical, Cardinality: CardinalityMany, Round: InvestigationRoundInitial,
	}.WithStableID(string(RouteApplicationPerformance))
	arguments := map[string]any{"app": "myscheduler", "range": "24h"}
	metadata := phase2CapabilityMetadata("read_application_history")
	request := CapabilityRequest{
		Capability: "read_application_history", Arguments: arguments, RequirementIDs: []string{requirement.ID},
		Criticality: CriticalityCritical, CostUnits: metadata.CostUnits, MaxFanout: metadata.MaxFanout, ConcurrencyKey: metadata.ConcurrencyKey,
	}.withStableID()
	prepared := Phase2BPrepared{
		Now: now, NormalizedQuestion: "is myscheduler slow",
		Route: ShadowRouteDecision{ID: RouteApplicationPerformance, Resolution: RouteResolutionSupported, Frame: QuestionFrame{Subjects: []EvidenceSubject{app}}, Requirements: []EvidenceRequirement{requirement}},
		Plan:  EvidencePlan{RouteID: string(RouteApplicationPerformance), Requests: []CapabilityRequest{request}, LogicalReads: 1, CostUnits: request.CostUnits},
	}
	evidence := InvestigationEvidence{
		Route: prepared.Route.RouteDescriptor(), Requirements: []EvidenceRequirement{requirement},
		Missing: []MissingEvidence{{RequirementID: requirement.ID, Criticality: CriticalityCritical, Capability: request.Capability, Availability: AvailabilityUnavailable}},
	}
	decision := PlanSecondEvidenceRound(prepared, evidence, prepared.Plan, nil, phase0CapabilityRegistry())
	if decision.Needed || decision.Reason != FollowUpDuplicateRead {
		t.Fatalf("equivalent read was repeated: %+v", decision)
	}
}

func TestPhase2DSecondRoundFanoutIsStrictlyBoundedAndDeterministic(t *testing.T) {
	for _, test := range []struct {
		name      string
		databases []reactorLabDatabase
		want      []string
		reason    FollowUpDecisionReason
	}{
		{"two databases", []reactorLabDatabase{{ID: "database_b"}, {ID: "database_a"}}, []string{"database_a", "database_b"}, FollowUpPlanned},
		{"three databases", []reactorLabDatabase{{ID: "database_c"}, {ID: "database_a"}, {ID: "database_b"}}, nil, FollowUpFanoutExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := phase2DNow()
			executor := &phase2DTestExecutor{handler: phase2DDatabaseHandler(now, test.databases, nil)}
			pipeline, prepared := preparePhase2DDatabasePipeline(t, executor)
			firstResults, err := NewEvidenceExecutor(pipeline.registry, executor, pipeline.now).Execute(t.Context(), prepared.Plan)
			if err != nil {
				t.Fatal(err)
			}
			firstEvidence, err := ReduceEvidenceResults(prepared.Route.RouteDescriptor(), prepared.NormalizedQuestion, prepared.Route.Requirements, prepared.Plan, firstResults, pipeline.registry)
			if err != nil {
				t.Fatal(err)
			}
			decision := PlanSecondEvidenceRound(prepared, firstEvidence, prepared.Plan, firstResults, pipeline.registry)
			if decision.Reason != test.reason || decision.Plan.LogicalReads != len(test.want) {
				t.Fatalf("decision=%+v", decision)
			}
			got := []string{}
			for index, request := range decision.Plan.Requests {
				got = append(got, stringArg(request.Arguments, "database_id"))
				if index >= len(decision.Candidates) || decision.Candidates[index].Request.ID != request.ID || decision.Candidates[index].Request.Order != index {
					t.Fatalf("typed candidates do not match finalized plan: %+v", decision)
				}
			}
			if len(decision.Candidates) != len(decision.Plan.Requests) {
				t.Fatalf("candidate count=%d requests=%d", len(decision.Candidates), len(decision.Plan.Requests))
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("follow-up order=%v want=%v", got, test.want)
			}
		})
	}
}

func TestPhase2DFollowUpBudgetRejectsExpensiveRead(t *testing.T) {
	now := phase2DNow()
	app := EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler"}
	requirement := EvidenceRequirement{
		Type: EvidenceTypeCurrentApplication, Subject: app, Criticality: CriticalityCritical,
		Cardinality: CardinalityOne, Round: InvestigationRoundInitial,
	}.WithStableID(string(RouteApplicationCurrent))
	prepared := Phase2BPrepared{
		Now: now, Route: ShadowRouteDecision{ID: RouteApplicationCurrent, Resolution: RouteResolutionSupported, Frame: QuestionFrame{Subjects: []EvidenceSubject{app}}, Requirements: []EvidenceRequirement{requirement}},
		Plan: EvidencePlan{RouteID: string(RouteApplicationCurrent)},
	}
	evidence := InvestigationEvidence{
		Route: prepared.Route.RouteDescriptor(), Requirements: []EvidenceRequirement{requirement},
		Missing: []MissingEvidence{{RequirementID: requirement.ID, Criticality: CriticalityCritical, Capability: "get_app_context", Availability: AvailabilityUnavailable}},
	}
	registry := phase0CapabilityRegistry()
	filtered := registry.capabilities[:0]
	for _, capability := range registry.capabilities {
		if capability.Definition.Function.Name != "list_apps" {
			filtered = append(filtered, capability)
		}
	}
	registry.capabilities = filtered
	decision := PlanSecondEvidenceRound(prepared, evidence, prepared.Plan, nil, registry)
	if decision.Needed || decision.Reason != FollowUpBudgetExceeded {
		t.Fatalf("expensive follow-up escaped budget: %+v", decision)
	}
}

func TestPhase2DFailedFollowUpIsPreservedWithoutRetryOrRoundThree(t *testing.T) {
	now := phase2DNow()
	executor := &phase2DTestExecutor{handler: phase2DDatabaseHandler(now, []reactorLabDatabase{{ID: "database_123"}}, errReactorLabUnavailable)}
	pipeline, prepared := preparePhase2DDatabasePipeline(t, executor)
	result, err := pipeline.ExecutePrepared(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	calls := executor.recordedCalls()
	if result.EvidenceRounds != 2 || result.SecondRoundReads != 1 || len(calls) != 2 {
		t.Fatalf("failed follow-up retried or changed rounds: result=%+v calls=%+v", result, calls)
	}
	found := false
	for _, missing := range result.Evidence.Missing {
		found = found || (missing.Capability == "read_database_backups" && missing.Availability == AvailabilityUnavailable)
	}
	if !found || result.Evidence.Confidence.Level == ConfidenceHigh {
		t.Fatalf("failed follow-up was hidden or confidence increased: %+v", result.Evidence)
	}
}

func TestPhase2DSecondRoundConflictIsPreservedAndLowersConfidence(t *testing.T) {
	now := phase2DNow()
	application := EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler", Name: "MyScheduler"}
	requirement := EvidenceRequirement{
		Type: EvidenceTypeCurrentApplication, Subject: application,
		Window:      &TemporalScope{Kind: TemporalNow, At: &now, Valid: true},
		Criticality: CriticalityCritical, Cardinality: CardinalityOne, Round: InvestigationRoundInitial,
	}.WithStableID(string(RouteApplicationCurrent))
	metadata := phase2CapabilityMetadata("get_app_context")
	request := CapabilityRequest{
		Order: 0, Capability: "get_app_context", Arguments: map[string]any{"app": "myscheduler"},
		RequirementIDs: []string{requirement.ID}, Criticality: CriticalityCritical,
		CostUnits: metadata.CostUnits, MaxFanout: metadata.MaxFanout, ConcurrencyKey: metadata.ConcurrencyKey,
	}.withStableID()
	prepared := Phase2BPrepared{
		Question: "Is MyScheduler healthy?", NormalizedQuestion: "is myscheduler healthy",
		Now: now,
		Route: ShadowRouteDecision{
			ID: RouteApplicationCurrent, Resolution: RouteResolutionSupported,
			Frame: QuestionFrame{Subjects: []EvidenceSubject{application}}, Requirements: []EvidenceRequirement{requirement},
		},
		Plan: EvidencePlan{
			RouteID: string(RouteApplicationCurrent), Requests: []CapabilityRequest{request},
			Missing: []MissingEvidence{{
				RequirementID: requirement.ID, Criticality: CriticalityCritical,
				Capability: "list_apps", Availability: AvailabilityPartial,
				Reason: "authoritative corroboration unavailable in the initial plan",
			}},
			LogicalReads: 1, CostUnits: request.CostUnits,
		},
	}
	executor := &phase2DTestExecutor{handler: func(_ context.Context, name string, _ map[string]any) (any, string, error) {
		switch name {
		case "get_app_context":
			return reactorLabAppContextResult{
				App: "myscheduler", SourceAvailable: true,
				Deployment: reactorLabDeployment{App: "myscheduler", Status: "running"},
				Overview: reactorLabContextOverview{
					Available:     true,
					System:        reactorLabSection[reactorLabSystemSnapshot]{Available: true},
					Recovery:      reactorLabSection[reactorLabRecovery]{Available: true},
					Observability: reactorLabSection[reactorLabObservabilityState]{Available: true},
				},
			}, "application context", nil
		case "list_apps":
			return reactorLabAppListResult{
				Apps: []reactorLabAppSummary{{App: "myscheduler", Status: "stopped"}}, TotalApps: 1,
			}, "applications", nil
		default:
			return nil, "", errors.New("unexpected capability")
		}
	}}
	pipeline := NewPhase2BPipeline(phase0CapabilityRegistry(), executor, func() time.Time { return now })
	result, err := pipeline.ExecutePrepared(t.Context(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	if result.EvidenceRounds != 2 || result.SecondRoundReads != 1 {
		t.Fatalf("second round did not run: %+v", result.SecondRound)
	}
	if calls := executor.recordedCalls(); len(calls) != 2 || calls[1].Name != "list_apps" {
		t.Fatalf("unexpected reads: %+v", calls)
	}
	if len(result.Evidence.Conflicts) == 0 || !result.Evidence.Conflicts[0].Material {
		t.Fatalf("new material conflict was hidden: %+v", result.Evidence.Conflicts)
	}
	if result.Evidence.Confidence.Level != ConfidenceLow {
		t.Fatalf("conflicted evidence falsely raised confidence: %+v", result.Evidence.Confidence)
	}
}

func TestPhase2DCancellationStopsBeforeOrDuringFollowUp(t *testing.T) {
	t.Run("before follow-up", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		now := phase2DNow()
		executor := &phase2DTestExecutor{}
		executor.handler = func(_ context.Context, name string, _ map[string]any) (any, string, error) {
			if name != "list_databases" {
				t.Fatalf("follow-up launched after cancellation: %s", name)
			}
			cancel()
			return reactorLabDatabaseList{CollectedAt: now, Databases: []reactorLabDatabase{{ID: "database_123"}}, TotalDatabases: 1}, "databases", nil
		}
		pipeline, prepared := preparePhase2DDatabasePipeline(t, executor)
		if _, err := pipeline.ExecutePrepared(ctx, prepared); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation err=%v", err)
		}
		if calls := executor.recordedCalls(); len(calls) != 1 {
			t.Fatalf("calls after pre-follow-up cancellation=%+v", calls)
		}
	})

	t.Run("during follow-up", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		now := phase2DNow()
		started := make(chan struct{})
		executor := &phase2DTestExecutor{}
		executor.handler = func(ctx context.Context, name string, _ map[string]any) (any, string, error) {
			switch name {
			case "list_databases":
				return reactorLabDatabaseList{CollectedAt: now, Databases: []reactorLabDatabase{{ID: "database_123"}}, TotalDatabases: 1}, "databases", nil
			case "read_database_backups":
				close(started)
				<-ctx.Done()
				return nil, "", ctx.Err()
			default:
				return nil, "", errors.New("unexpected capability")
			}
		}
		pipeline, prepared := preparePhase2DDatabasePipeline(t, executor)
		done := make(chan error, 1)
		go func() {
			_, err := pipeline.ExecutePrepared(ctx, prepared)
			done <- err
		}()
		<-started
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation err=%v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("follow-up cancellation leaked a goroutine")
		}
		if calls := executor.recordedCalls(); len(calls) != 2 {
			t.Fatalf("unexpected calls during cancellation=%+v", calls)
		}
	})
}
