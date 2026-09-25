package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestEvidencePlanningRouteMappingsAndCosts(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	application := EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler", Name: "MyScheduler"}
	tests := []struct {
		name  string
		route InvestigationRouteID
		frame QuestionFrame
		want  []string
		cost  int
	}{
		{
			name: "platform", route: RouteCurrentPlatformHealth,
			frame: QuestionFrame{Temporal: TemporalScope{Kind: TemporalNow, At: &now, Valid: true}},
			want:  []string{"get_platform_overview"}, cost: 1,
		},
		{
			name: "thermal", route: RouteThermalInvestigation,
			frame: QuestionFrame{Temporal: TemporalScope{Kind: TemporalNamedWindow, NamedRange: "24h", Valid: true}},
			want:  []string{"get_platform_overview", "read_host_history", "read_temperature_history"}, cost: 3,
		},
		{
			name: "restart", route: RouteRestartInvestigation,
			frame: QuestionFrame{Temporal: TemporalScope{Kind: TemporalNamedWindow, NamedRange: "6h", Valid: true}},
			want:  []string{"read_recovery", "read_infrastructure_events", "read_activity", "read_host_history", "read_temperature_history"}, cost: 5,
		},
		{
			name: "deployment correlation", route: RouteDeploymentCorrelation,
			frame: QuestionFrame{Subjects: []EvidenceSubject{application}, Temporal: TemporalScope{Kind: TemporalNamedWindow, NamedRange: "7d", Valid: true}},
			want:  []string{"get_app_context", "read_infrastructure_events", "read_activity", "read_application_history", "read_deployment_history"}, cost: 12,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requirements := requirementsForRoute(test.route, test.frame)
			plan, err := BuildEvidencePlan(InvestigationRoute{ID: test.route, Resolution: RouteResolutionSupported, Frame: test.frame}, requirements, phase0CapabilityRegistry(), now)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(plan.Requests))
			for _, request := range plan.Requests {
				got = append(got, request.Capability)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("capabilities=%v want %v", got, test.want)
			}
			if plan.LogicalReads != len(test.want) || plan.CostUnits != test.cost {
				t.Fatalf("budget=%d/%d want %d/%d", plan.LogicalReads, plan.CostUnits, len(test.want), test.cost)
			}
			for _, request := range plan.Requests {
				if request.ID == "" {
					t.Fatal("request ID is empty")
				}
			}
		})
	}
}

func TestEvidencePlanningDeterministicDedupAndMissing(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	route := InvestigationRoute{ID: RouteCurrentPlatformHealth, Resolution: RouteResolutionSupported}
	first := EvidenceRequirement{Type: EvidenceTypeCurrentPlatformState, Subject: EvidenceSubject{Kind: SubjectHost, ID: "dell"}, Criticality: CriticalityCritical, Cardinality: CardinalityOne, Round: InvestigationRoundInitial}
	second := first
	second.Criticality = CriticalityRelevant
	one, err := BuildEvidencePlan(route, []EvidenceRequirement{first, second}, phase0CapabilityRegistry(), now)
	if err != nil {
		t.Fatal(err)
	}
	two, err := BuildEvidencePlan(route, []EvidenceRequirement{second, first}, phase0CapabilityRegistry(), now)
	if err != nil {
		t.Fatal(err)
	}
	if canonicalJSON(one) != canonicalJSON(two) {
		t.Fatalf("permuted semantic input changed plan\n%s\n%s", canonicalJSON(one), canonicalJSON(two))
	}
	if len(one.Requests) != 1 || len(one.Requests[0].RequirementIDs) != 2 || one.Requests[0].Criticality != CriticalityCritical {
		t.Fatalf("deduplication failed: %+v", one)
	}

	registry := phase0CapabilityRegistry()
	filtered := registry.capabilities[:0]
	for _, capability := range registry.capabilities {
		if capability.Definition.Function.Name != "get_platform_overview" && capability.Definition.Function.Name != "get_app_context" {
			filtered = append(filtered, capability)
		}
	}
	registry.capabilities = filtered
	missing, err := BuildEvidencePlan(route, []EvidenceRequirement{first}, registry, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing.Requests) != 0 || len(missing.Missing) != 1 || missing.Missing[0].Criticality != CriticalityCritical {
		t.Fatalf("missing provider was not explicit: %+v", missing)
	}
}

func TestEvidenceParameterPolicyBoundsWindowsAndSubjects(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	from, to := now.Add(-2*time.Hour), now.Add(-time.Hour)
	requirement := EvidenceRequirement{
		Type: EvidenceTypeApplicationHistory, Subject: EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler"},
		Window:      &TemporalScope{Kind: TemporalExplicitWindow, From: &from, To: &to, Valid: true},
		Criticality: CriticalityCritical, Cardinality: CardinalityMany, Round: InvestigationRoundInitial,
	}
	args, err := evidenceArguments(requirement, "read_application_history", QuestionFrame{}, now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"app": "myscheduler", "from": from.Format(time.RFC3339Nano), "to": to.Format(time.RFC3339Nano)}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%v want %v", args, want)
	}
	future := now.Add(time.Minute)
	requirement.Window.To = &future
	if _, err := evidenceArguments(requirement, "read_application_history", QuestionFrame{}, now); !errors.Is(err, errEvidencePlanInvalidParameter) {
		t.Fatalf("future window error=%v", err)
	}
	requirement.Window = &TemporalScope{Kind: TemporalNow, At: &now, Valid: true}
	if _, err := evidenceArguments(requirement, "read_application_history", QuestionFrame{}, now); !errors.Is(err, errEvidencePlanInvalidParameter) {
		t.Fatalf("now must not widen to history: %v", err)
	}
	databaseRequirement := EvidenceRequirement{Type: EvidenceTypeDatabaseBackups, Subject: EvidenceSubject{Kind: SubjectDatabase, Name: "primary"}}
	if _, err := evidenceArguments(databaseRequirement, "read_database_backups", QuestionFrame{}, now); !errors.Is(err, errEvidencePlanInvalidParameter) {
		t.Fatalf("database display name must not become canonical ID: %v", err)
	}
}

func TestEvidencePlanBudgetRejectsBeforeExecutionAndAccountsFanout(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	frame := QuestionFrame{Subjects: []EvidenceSubject{{Kind: SubjectApplication, ID: "myscheduler"}}, Temporal: TemporalScope{Kind: TemporalNow, At: &now, Valid: true}}
	requirements := requirementsForRoute(RouteApplicationCurrent, frame)
	plan, err := BuildEvidencePlan(InvestigationRoute{ID: RouteApplicationCurrent, Frame: frame}, requirements, phase0CapabilityRegistry(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Requests) != 1 || plan.Requests[0].Capability != "get_app_context" || plan.Requests[0].CostUnits != 7 || plan.Requests[0].MaxFanout != 7 || plan.CostUnits != 7 {
		t.Fatalf("app context fanout not accounted: %+v", plan)
	}

	fake := &countingCapabilityExecutor{}
	over := EvidencePlan{Requests: make([]CapabilityRequest, 7)}
	for index := range over.Requests {
		over.Requests[index] = CapabilityRequest{ID: stableContractID("test", index), Order: index, Capability: "get_platform_overview", CostUnits: 1, MaxFanout: 1}
	}
	_, err = NewEvidenceExecutor(phase0CapabilityRegistry(), fake, func() time.Time { return now }).Execute(t.Context(), over)
	if !errors.Is(err, errEvidencePlanBudget) || fake.calls != 0 {
		t.Fatalf("budget error=%v calls=%d", err, fake.calls)
	}
}

func TestRepositoryPlanningUsesDeterministicSearchDependency(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	frame := QuestionFrame{
		Goal:     GoalImplementationLocation,
		Subjects: []EvidenceSubject{{Kind: SubjectApplication, ID: "myscheduler", Name: "MyScheduler"}},
		Domains:  []EvidenceDomain{DomainRepository}, Temporal: TemporalScope{Kind: TemporalUnspecified, Valid: true},
	}
	route := InvestigationRoute{ID: RouteRepositoryInvestigation, Resolution: RouteResolutionSupported, Frame: frame}
	requirements := requirementsForRoute(route.ID, frame)
	plan, err := BuildEvidencePlanForQuestion(route, requirements, phase0CapabilityRegistry(), now, "where is schedule template implemented in myscheduler")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Requests) != 2 || len(plan.Missing) != 0 || plan.Requests[0].Capability != "search_repository" || plan.Requests[1].Capability != "read_repository_file" {
		t.Fatalf("repository plan=%+v", plan)
	}
	child := plan.Requests[1]
	if len(child.DependsOn) != 1 || child.DependsOn[0] != plan.Requests[0].ID || len(child.ArgumentBindings) != 1 || child.ArgumentBindings[0].Selector != "first_repository_search_path" {
		t.Fatalf("repository dependency=%+v", child)
	}

	fake := &repositoryBindingExecutor{}
	results, err := NewEvidenceExecutor(phase0CapabilityRegistry(), fake, func() time.Time { return now }).Execute(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || fake.filePath != "internal/a.go" || results[1].Arguments["path"] != "internal/a.go" {
		t.Fatalf("binding result=%+v executor=%+v", results, fake)
	}
}

type repositoryBindingExecutor struct {
	filePath string
}

func (executor *repositoryBindingExecutor) Execute(_ context.Context, name string, args map[string]any) (any, string, error) {
	switch name {
	case "search_repository":
		return repoSearchResponse{App: "myscheduler", Query: stringArg(args, "query"), Path: ".", Hits: []repoSearchHit{{Path: "internal/z.go", Line: 2}, {Path: "internal/a.go", Line: 10}}}, "safe search", nil
	case "read_repository_file":
		executor.filePath = stringArg(args, "path")
		return repoFileResponse{App: "myscheduler", Path: executor.filePath, Content: "safe"}, "safe file", nil
	default:
		return nil, "", errors.New("unexpected capability")
	}
}

type countingCapabilityExecutor struct{ calls int }

func (executor *countingCapabilityExecutor) Execute(_ context.Context, _ string, _ map[string]any) (any, string, error) {
	executor.calls++
	return struct{}{}, "", nil
}

func TestEvidenceProviderSelectionRejectsIncompatibleSubjects(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	without := func(names ...string) capabilityRegistry {
		blocked := map[string]bool{}
		for _, name := range names {
			blocked[name] = true
		}
		registry := phase0CapabilityRegistry()
		filtered := registry.capabilities[:0]
		for _, capability := range registry.capabilities {
			if !blocked[capability.Definition.Function.Name] {
				filtered = append(filtered, capability)
			}
		}
		registry.capabilities = filtered
		return registry
	}
	tests := []struct {
		name        string
		requirement EvidenceRequirement
		registry    capabilityRegistry
		want        string
	}{
		{
			name: "host does not become app", registry: without("get_platform_overview"),
			requirement: EvidenceRequirement{Type: EvidenceTypeCurrentPlatformState, Subject: EvidenceSubject{Kind: SubjectHost, ID: "dell"}, Criticality: CriticalityCritical, Cardinality: CardinalityOne},
		},
		{
			name: "database does not become app", registry: without("list_databases"),
			requirement: EvidenceRequirement{Type: EvidenceTypeCurrentDatabase, Subject: EvidenceSubject{Kind: SubjectDatabase, ID: "db-1"}, Criticality: CriticalityCritical, Cardinality: CardinalityOne},
		},
		{
			name: "application does not become platform", registry: without("get_app_context"),
			requirement: EvidenceRequirement{Type: EvidenceTypeCurrentPlatformState, Subject: EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler"}, Criticality: CriticalityCritical, Cardinality: CardinalityOne},
		},
		{
			name: "application cannot request platform service history", registry: phase0CapabilityRegistry(),
			requirement: EvidenceRequirement{Type: EvidenceTypeServiceHistory, Subject: EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler"}, Criticality: CriticalityRelevant, Cardinality: CardinalityMany},
		},
		{
			name: "host intentionally requests platform service history", registry: phase0CapabilityRegistry(), want: "read_service_history",
			requirement: EvidenceRequirement{Type: EvidenceTypeServiceHistory, Subject: EvidenceSubject{Kind: SubjectHost, ID: "dell"}, Criticality: CriticalityRelevant, Cardinality: CardinalityMany, Window: &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "24h", Valid: true}},
		},
		{
			name: "application context remains compatible", registry: phase0CapabilityRegistry(), want: "get_app_context",
			requirement: EvidenceRequirement{Type: EvidenceTypeCurrentApplication, Subject: EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler"}, Criticality: CriticalityCritical, Cardinality: CardinalityOne},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := BuildEvidencePlan(InvestigationRoute{ID: RouteApplicationPerformance}, []EvidenceRequirement{test.requirement}, test.registry, now)
			if err != nil {
				t.Fatal(err)
			}
			if test.want == "" {
				if len(plan.Requests) != 0 || len(plan.Missing) != 1 {
					t.Fatalf("incompatible provider was planned: %+v", plan)
				}
				return
			}
			if len(plan.Requests) != 1 || plan.Requests[0].Capability != test.want || len(plan.Missing) != 0 {
				t.Fatalf("compatible provider not planned: %+v", plan)
			}
		})
	}
}

func TestEvidenceWindowsPreserveSubsecondsAndValidateLimitOnlyLogs(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 987654321, time.UTC)
	from := now.Add(-2*time.Hour - 123456789*time.Nanosecond)
	to := now.Add(-time.Hour - 23456789*time.Nanosecond)
	requirement := EvidenceRequirement{
		Type: EvidenceTypeApplicationHistory, Subject: EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler"},
		Window: &TemporalScope{Kind: TemporalExplicitWindow, From: &from, To: &to, Valid: true}, Criticality: CriticalityCritical, Cardinality: CardinalityMany,
	}
	args, err := evidenceArguments(requirement, "read_application_history", QuestionFrame{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if args["from"] != from.Format(time.RFC3339Nano) || args["to"] != to.Format(time.RFC3339Nano) {
		t.Fatalf("subsecond window changed: %#v", args)
	}

	logRequirement := requirement
	logRequirement.Type = EvidenceTypeRuntimeFailures
	future := now.Add(time.Nanosecond)
	logRequirement.Window = &TemporalScope{Kind: TemporalExplicitWindow, From: &from, To: &future, Valid: true}
	if _, err := evidenceArguments(logRequirement, "read_runtime_logs", QuestionFrame{}, now); !errors.Is(err, errEvidencePlanInvalidParameter) {
		t.Fatalf("future log window error=%v", err)
	}
	tooOld := now.Add(-7*24*time.Hour - time.Nanosecond)
	logRequirement.Window = &TemporalScope{Kind: TemporalExplicitWindow, From: &tooOld, To: &now, Valid: true}
	if _, err := evidenceArguments(logRequirement, "read_runtime_logs", QuestionFrame{}, now); !errors.Is(err, errEvidencePlanInvalidParameter) {
		t.Fatalf("overlong log window error=%v", err)
	}
}

func TestDeferredRepositoryContentDeduplicatesAcrossRequirements(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	subject := EvidenceSubject{Kind: SubjectRepository, ID: "myscheduler", Name: "MyScheduler"}
	search := EvidenceRequirement{Type: EvidenceTypeRepositorySearch, Subject: subject, Criticality: CriticalityCritical, Cardinality: CardinalityMany}
	contentA := EvidenceRequirement{Type: EvidenceTypeRepositoryContent, Subject: subject, Criticality: CriticalityRelevant, Cardinality: CardinalityMany}
	contentB := contentA
	contentB.Criticality = CriticalitySupporting
	route := InvestigationRoute{ID: RouteRepositoryInvestigation, Frame: QuestionFrame{Goal: GoalImplementationLocation, Subjects: []EvidenceSubject{subject}}}
	plan, err := BuildEvidencePlanForQuestion(route, []EvidenceRequirement{contentB, search, contentA, contentA}, phase0CapabilityRegistry(), now, "where is the schedule template implemented")
	if err != nil {
		t.Fatal(err)
	}
	if plan.LogicalReads != 2 || plan.CostUnits != 2 || len(plan.Requests) != 2 {
		t.Fatalf("deduplicated budget=%+v", plan)
	}
	parent, child := plan.Requests[0], plan.Requests[1]
	if parent.Capability != "search_repository" || child.Capability != "read_repository_file" || len(child.DependsOn) != 1 || child.DependsOn[0] != parent.ID {
		t.Fatalf("repository DAG=%+v", plan.Requests)
	}
	if len(child.RequirementIDs) != 2 || child.Criticality != CriticalityRelevant || len(child.ArgumentBindings) != 1 {
		t.Fatalf("merged deferred request=%+v", child)
	}
}

func TestEvidenceCostBudgetRejectsBeforeAnyHandlerStarts(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	metadata := phase2CapabilityMetadata("get_app_context")
	plan := EvidencePlan{Requests: []CapabilityRequest{
		{ID: "one", Order: 0, Capability: "get_app_context", ConcurrencyKey: metadata.ConcurrencyKey, CostUnits: metadata.CostUnits, MaxFanout: metadata.MaxFanout},
		{ID: "two", Order: 1, Capability: "get_app_context", ConcurrencyKey: metadata.ConcurrencyKey, CostUnits: metadata.CostUnits, MaxFanout: metadata.MaxFanout},
	}}
	fake := &countingCapabilityExecutor{}
	_, err := NewEvidenceExecutor(phase0CapabilityRegistry(), fake, func() time.Time { return now }).Execute(t.Context(), plan)
	if !errors.Is(err, errEvidencePlanBudget) || fake.calls != 0 {
		t.Fatalf("cost-only overflow error=%v starts=%d", err, fake.calls)
	}
}
