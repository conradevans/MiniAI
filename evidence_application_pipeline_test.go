package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type applicationIncidentPipelineExecutor struct {
	values   map[string]any
	failures map[string]error
}

func (executor applicationIncidentPipelineExecutor) Execute(_ context.Context, name string, _ map[string]any) (any, string, error) {
	if err := executor.failures[name]; err != nil {
		return nil, "", err
	}
	value, ok := executor.values[name]
	if !ok {
		return nil, "", fmt.Errorf("unexpected capability %q", name)
	}
	return value, "safe " + name, nil
}

func productionApplicationIncidentExecutor(now time.Time) applicationIncidentPipelineExecutor {
	from := now.Add(-24 * time.Hour)
	window := reactorLabWindow{Range: "24h", From: from, To: now, BucketSeconds: 360, MaxPoints: 240}
	activated := now.Add(-72 * time.Hour)
	source := &reactorLabDeploymentSource{
		Provider: "github", Repository: "reactor-laboratory/myscheduler-production",
		Branch: "main", RequestedRef: "refs/heads/main", CommitSHA: strings.Repeat("a", 40),
	}
	contextResult := reactorLabAppContextResult{
		App: "myscheduler", SourceAvailable: true,
		Deployment: reactorLabDeployment{
			App: "myscheduler", Strategy: "systemd", Status: "running", Source: source,
			ActivatedAt: &activated, ImageID: "sha256:" + strings.Repeat("b", 64),
			Services: []reactorLabDeploymentService{
				{Name: "myscheduler-api.service", Strategy: "systemd", Status: "running"},
				{Name: "myscheduler-worker.service", Strategy: "systemd", Status: "failed"},
			},
			Databases: []reactorLabDeploymentDatabase{{ID: "database_762example", DisplayName: "MyScheduler Production", BindingName: "DATABASE_URL"}},
		},
		Databases:                []reactorLabDatabase{{ID: "database_762example", DisplayName: "MyScheduler Production", Status: "ready"}},
		DatabaseLookupsRequested: 1,
		Overview: reactorLabContextOverview{
			Available: true, CollectedAt: &now,
			System:        reactorLabSection[reactorLabSystemSnapshot]{Available: true},
			Recovery:      reactorLabSection[reactorLabRecovery]{Available: true},
			Observability: reactorLabSection[reactorLabObservabilityState]{Available: true},
		},
		LocalRepository: localRepositoryEvidence{Path: "/srv/myscheduler", Exists: true, Branch: "main", Commit: strings.Repeat("a", 40)},
	}

	hostPoints := make([]reactorLabHostPoint, 0, 240)
	applicationPoints := make([]reactorLabApplicationPoint, 0, 240)
	serviceSeries := make([]reactorLabServiceSeries, 0, 4)
	for index := 0; index < 240; index++ {
		at := from.Add(time.Duration(index) * 6 * time.Minute)
		cpuAverage := 18.0 + float64(index%17)
		cpuMaximum := cpuAverage + 12
		hostPoints = append(hostPoints, reactorLabHostPoint{
			Timestamp: at, SampleCount: 6, CPUAverage: &cpuAverage, CPUMaximum: &cpuMaximum,
			MemoryUsedAverage: 4_000_000_000 + float64(index*1_000_000),
		})
		status, restartCount := "running", 2
		if index >= 180 && index < 190 {
			status, restartCount = "failed", 3
		}
		applicationPoints = append(applicationPoints, reactorLabApplicationPoint{
			Timestamp: at, SampleCount: 6, CPUAverage: cpuAverage, CPUMaximum: cpuMaximum,
			MemoryUsedAverage: 750_000_000 + float64(index*100_000), MemoryUsedMaximum: 900_000_000,
			MemoryLimitAverage: 1_000_000_000, Status: status, RestartCount: restartCount,
		})
	}
	for serviceIndex := 0; serviceIndex < 4; serviceIndex++ {
		points := make([]reactorLabServicePoint, 0, 240)
		for index := 0; index < 240; index++ {
			status, available := "running", true
			if serviceIndex == 1 && index >= 180 && index < 190 {
				status, available = "failed", false
			}
			points = append(points, reactorLabServicePoint{
				Timestamp: from.Add(time.Duration(index) * 6 * time.Minute), SampleCount: 6,
				Available: available, Status: status,
			})
		}
		serviceSeries = append(serviceSeries, reactorLabServiceSeries{
			ID: fmt.Sprintf("service-%d", serviceIndex), Name: fmt.Sprintf("myscheduler-service-%d", serviceIndex), Points: points,
		})
	}
	logLines := make([]string, 0, 80)
	for index := 0; index < 80; index++ {
		at := now.Add(-time.Duration(80-index) * time.Minute)
		message := "INFO scheduler tick completed"
		if index%13 == 0 {
			message = "ERROR scheduled job failed with bounded internal error"
		}
		logLines = append(logLines, at.Format(time.RFC3339Nano)+" "+message)
	}

	return applicationIncidentPipelineExecutor{values: map[string]any{
		"get_app_context":   contextResult,
		"read_host_history": reactorLabHostHistory{Window: window, Points: hostPoints, TotalPoints: len(hostPoints)},
		"read_application_history": reactorLabResolvedApplicationHistory{
			Application: reactorLabApplicationSummary{ID: "myscheduler", Name: "MyScheduler", LatestStatus: "running", RestartCount: 3, LastObservedAt: now},
			History:     reactorLabApplicationHistory{Window: window, ID: "myscheduler", Points: applicationPoints, TotalPoints: len(applicationPoints)},
		},
		"read_service_history": reactorLabServiceHistory{Window: window, Services: serviceSeries, TotalServices: len(serviceSeries)},
		"read_runtime_logs": logToolResponse{
			App: "myscheduler", Kind: "runtime", Lines: len(logLines), Bytes: len(strings.Join(logLines, "\n")),
			Logs: strings.Join(logLines, "\n"), Truncated: true,
		},
	}, failures: map[string]error{}}
}

func runProductionApplicationIncidentPipeline(t *testing.T, executor applicationIncidentPipelineExecutor, now time.Time) (Phase2BResult, error) {
	t.Helper()
	pipeline := NewPhase2BPipeline(phase0CapabilityRegistry(), executor, func() time.Time { return now })
	return pipeline.Run(t.Context(), "Why has MyScheduler been failing recently?", ShadowRouterContext{
		Now: now, Location: time.UTC,
		Entities: []RouterEntity{{Subject: EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler", Name: "MyScheduler"}, Aliases: []string{"myscheduler", "MyScheduler"}}},
	})
}

func TestProductionApplicationIncidentRunsFullFiveReadPipelineWithinPacketBudget(t *testing.T) {
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	result, err := runProductionApplicationIncidentPipeline(t, productionApplicationIncidentExecutor(now), now)
	if err != nil {
		var sizeErr EvidencePacketSizeError
		if errors.As(err, &sizeErr) {
			itemSizes := make([]string, 0, len(result.Evidence.Items))
			for _, item := range result.Evidence.Items {
				fieldSizes := make([]string, 0, len(item.Payload.Fields))
				for _, field := range item.Payload.Fields {
					fieldSizes = append(fieldSizes, fmt.Sprintf("%s=%d", field.Name, len(canonicalJSON(field))))
				}
				itemSizes = append(itemSizes, fmt.Sprintf("%s/%s=%d[%s]", item.Type, item.Kind, len(canonicalJSON(item)), strings.Join(fieldSizes, ",")))
			}
			t.Fatalf("production-shaped packet remained oversized: %d > %d; items: %s", sizeErr.Bytes, sizeErr.Limit, strings.Join(itemSizes, "; "))
		}
		t.Fatal(err)
	}
	if result.Route.ID != RouteApplicationPerformance || result.Plan.LogicalReads != 5 || result.Plan.CostUnits != 12 || len(result.Results) != 5 {
		t.Fatalf("route/plan/results=%s/%d/%d/%d", result.Route.ID, result.Plan.LogicalReads, result.Plan.CostUnits, len(result.Results))
	}
	if len(result.PacketJSON) == 0 || len(result.PacketJSON) > evidencePacketTargetBytes {
		t.Fatalf("normal packet bytes=%d target=%d", len(result.PacketJSON), evidencePacketTargetBytes)
	}
}

func TestProductionApplicationIncidentPacketIsStableAndRetainsCriticalFacts(t *testing.T) {
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	first, err := runProductionApplicationIncidentPipeline(t, productionApplicationIncidentExecutor(now), now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := runProductionApplicationIncidentPipeline(t, productionApplicationIncidentExecutor(now), now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.PacketJSON, second.PacketJSON) {
		t.Fatal("production-shaped packet is not byte stable")
	}
	criticalBefore, criticalAfter := 0, 0
	for _, item := range first.Evidence.Items {
		if item.Relevance.Criticality == CriticalityCritical {
			criticalBefore++
		}
	}
	for _, item := range first.Packet.Evidence {
		if item.Criticality == CriticalityCritical {
			criticalAfter++
		}
	}
	encoded := string(first.PacketJSON)
	for _, required := range []string{
		"myscheduler", strings.Repeat("a", 40), "sha256:" + strings.Repeat("b", 64),
		"reactor-laboratory/myscheduler-production", "activated_at", "latest_status",
		"restart_count", "state_transitions", "coverage_from", "coverage_to",
		"runtime_failures", "error_patterns", "service_history", "service_summary", "service_transitions",
	} {
		if !strings.Contains(encoded, required) {
			t.Fatalf("critical fact %q missing from packet: %s", required, encoded)
		}
	}
	if criticalBefore == 0 || criticalAfter != criticalBefore {
		t.Fatalf("critical evidence count changed: before=%d after=%d", criticalBefore, criticalAfter)
	}
	if len(first.PacketJSON) > evidencePacketTargetBytes {
		t.Fatalf("normal packet bytes=%d target=%d", len(first.PacketJSON), evidencePacketTargetBytes)
	}
	t.Logf("production-shaped packet bytes=%d target=%d hard_limit=%d", len(first.PacketJSON), evidencePacketTargetBytes, evidencePacketHardLimitBytes)
}

func TestProductionApplicationIncidentPartialReadsRemainNonfatal(t *testing.T) {
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	tests := []struct {
		name        string
		capability  string
		criticality RequirementCriticality
		confidence  ConfidenceLevel
		rounds      int
	}{
		{name: "Relevant runtime failures", capability: "read_runtime_logs", criticality: CriticalityRelevant, confidence: ConfidenceMedium, rounds: 1},
		{name: "Supporting host history", capability: "read_host_history", criticality: CriticalitySupporting, confidence: ConfidenceMedium, rounds: 1},
		{name: "Critical current application", capability: "get_app_context", criticality: CriticalityCritical, confidence: ConfidenceLow, rounds: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := productionApplicationIncidentExecutor(now)
			executor.failures[test.capability] = errReactorLabUnavailable
			result, err := runProductionApplicationIncidentPipeline(t, executor, now)
			if err != nil {
				t.Fatalf("partial evidence became fatal: %v", err)
			}
			if len(result.PacketJSON) == 0 || len(result.PacketJSON) >= evidencePacketHardLimitBytes {
				t.Fatalf("partial packet bytes=%d hard-limit=%d", len(result.PacketJSON), evidencePacketHardLimitBytes)
			}
			foundMissing := false
			for _, missing := range result.Evidence.Missing {
				if missing.Capability != test.capability {
					continue
				}
				foundMissing = true
				if missing.Criticality != test.criticality || missing.ErrorCategory != EvidenceErrorUnavailable {
					t.Fatalf("missing evidence=%+v", missing)
				}
			}
			if !foundMissing || result.Evidence.Confidence.Level != test.confidence || result.EvidenceRounds != test.rounds {
				t.Fatalf("missing/confidence/rounds=%t/%+v/%d", foundMissing, result.Evidence.Confidence, result.EvidenceRounds)
			}
			t.Logf("confidence=%s packet_bytes=%d evidence_rounds=%d", result.Evidence.Confidence.Level, len(result.PacketJSON), result.EvidenceRounds)
			for _, evidenceResult := range result.Results {
				if evidenceResult.Capability == test.capability && evidenceResult.ErrorCategory == EvidenceErrorInternal {
					t.Fatalf("provider failure was fabricated as internal: %+v", evidenceResult)
				}
			}
		})
	}
}

func TestExecutePreparedPreservesResultsOnIrreduciblyOversizedPacket(t *testing.T) {
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	executor := productionApplicationIncidentExecutor(now)
	pipeline := NewPhase2BPipeline(phase0CapabilityRegistry(), executor, func() time.Time { return now })
	pipeline.reduce = irreduciblyOversizedApplicationEvidence
	prepared, err := pipeline.Prepare("Why has MyScheduler been failing recently?", ShadowRouterContext{
		Now: now, Location: time.UTC,
		Entities: []RouterEntity{{Subject: EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler", Name: "MyScheduler"}, Aliases: []string{"myscheduler", "MyScheduler"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.ExecutePrepared(t.Context(), prepared)
	if err == nil {
		t.Fatal("irreducibly oversized packet unexpectedly succeeded")
	}
	var pipelineErr Phase2BPipelineError
	if !errors.As(err, &pipelineErr) || pipelineErr.Stage != Phase2BStagePacketBuild || pipelineErr.Category != Phase2BPipelineErrorPacketTooLarge || pipelineErr.PacketBytes <= pipelineErr.PacketLimit || pipelineErr.PacketTarget != evidencePacketTargetBytes {
		t.Fatalf("typed pipeline error=%+v raw=%v", pipelineErr, err)
	}
	if result.Route.ID != RouteApplicationPerformance || len(result.Plan.Requests) != 5 || len(result.Results) != 5 || len(result.Evidence.Items) == 0 || len(result.PacketJSON) != 0 {
		t.Fatalf("late error erased completed state: route=%s plan=%d results=%d evidence=%d packet=%d", result.Route.ID, len(result.Plan.Requests), len(result.Results), len(result.Evidence.Items), len(result.PacketJSON))
	}
	for _, evidenceResult := range result.Results {
		if evidenceResult.Availability != AvailabilityAvailable && evidenceResult.Availability != AvailabilityPartial {
			t.Fatalf("real result state lost: %+v", evidenceResult)
		}
	}
}

const irreducibleEvidenceMarker = "DO_NOT_LOG_RAW_EVIDENCE_PAYLOAD"

func irreduciblyOversizedApplicationEvidence(route InvestigationRoute, question string, requirements []EvidenceRequirement, plan EvidencePlan, results []EvidenceResult, registry capabilityRegistry) (InvestigationEvidence, error) {
	evidence, err := ReduceEvidenceResults(route, question, requirements, plan, results, registry)
	if err != nil {
		return InvestigationEvidence{}, err
	}
	var template EvidenceItem
	for _, item := range evidence.Items {
		if item.Relevance.Criticality == CriticalityCritical && item.Kind == EvidenceObserved {
			template = item
			break
		}
	}
	for index := 0; index < evidencePacketMaxCriticalItems; index++ {
		item := template
		item.ID = fmt.Sprintf("irreducible-%d-%d", len(results), index)
		item.Inputs = nil
		item.Payload.Fields = []EvidenceField{
			evidenceStringField("irreducible_marker", irreducibleEvidenceMarker),
			integerField("irreducible_index", int64(index)),
		}
		evidence.Items = append(evidence.Items, item)
	}
	return NormalizeInvestigationEvidence(evidence)
}
