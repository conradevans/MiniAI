package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticControllerIncludesDatabaseStateForTimeouts(t *testing.T) {
	logs := "2026-09-08T14:32:10Z ERROR database timeout while loading schedules"
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, logs, "")
	defer cleanup()
	snapshot := healthyNegativePremisePacket().Application
	packet, evidence := a.buildDiagnosticPacket(context.Background(), snapshot, "Diagnose the database timeout in MyScheduler")
	if packet.Application.Database == nil || packet.Application.Database.Status != "ready" {
		t.Fatalf("available database state was lost: %+v", packet.Application.Database)
	}
	if packet.RuntimeLogs == nil || packet.RuntimeLogs.ErrorCount != 1 || len(evidence) != 2 {
		t.Fatalf("runtime expansion missing: packet=%+v evidence=%+v", packet, evidence)
	}
	if !containsDiagnosticFact(packet.Assessment.PossibleCauses, "database-path timeout") {
		t.Fatalf("database timeout possibility missing: %+v", packet.Assessment)
	}
}

func TestDiagnosticControllerGathersDeploymentEvidenceByQuestion(t *testing.T) {
	runtimeLogs := "2026-09-08T14:32:10Z ERROR request failed"
	deploymentLogs := "2026-09-08T14:31:00Z INFO deployment completed"
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, runtimeLogs, deploymentLogs)
	defer cleanup()
	packet, _ := a.buildDiagnosticPacket(context.Background(), healthyNegativePremisePacket().Application, "Did MyScheduler failures begin after deployment?")
	if packet.DeploymentLogs == nil || packet.Investigation.Rounds != 2 ||
		!sameStrings(packet.Investigation.EvidenceExpansions, []string{diagnosticSourceRuntime, diagnosticSourceDeployment}) {
		t.Fatalf("deployment expansion missing: %+v", packet.Investigation)
	}
}

func TestDiagnosticControllerGathersDeploymentEvidenceFromTiming(t *testing.T) {
	runtimeLogs := "2026-09-08T14:32:10Z ERROR request failed"
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, runtimeLogs, "2026-09-08T14:31:30Z INFO deployed")
	defer cleanup()
	snapshot := healthyNegativePremisePacket().Application
	snapshot.DeployedAt = "2026-09-08T14:31:00Z"
	packet, _ := a.buildDiagnosticPacket(context.Background(), snapshot, "Diagnose MyScheduler errors")
	if packet.DeploymentLogs == nil || !containsDiagnosticFact(packet.Assessment.KeyEvidence, "timing alone does not prove causation") {
		t.Fatalf("timing-triggered deployment evidence missing: %+v", packet)
	}
}

func TestDiagnosticControllerRetainsBoundedRepositoryEvidence(t *testing.T) {
	_, indexed, _ := makeIndexedRepo(t)
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, "2026-09-08T14:32:10Z ERROR schedule template route failed", "")
	defer cleanup()
	a.repoRoot = indexed.repoRoot
	packet, evidence := a.buildDiagnosticPacket(context.Background(), healthyNegativePremisePacket().Application, "Why is the schedule template route failing in MyScheduler?")
	if len(packet.Repository) == 0 || packet.Repository[0].Path != "backend/routes/scheduleTemplateRoutes.js" ||
		len([]rune(packet.Repository[0].Snippet)) > agentEvidenceMaxRunesPerFile {
		t.Fatalf("bounded repository evidence missing: %+v", packet.Repository)
	}
	if countDiagnosticTool(evidence, "search_repository") != 1 || countDiagnosticTool(evidence, "read_repository_file") != 1 {
		t.Fatalf("repository evidence events missing or repeated: %+v", evidence)
	}
}

func TestDiagnosticControllerDoesNotRepeatSourcesAndStopsAtMaximum(t *testing.T) {
	_, indexed, _ := makeIndexedRepo(t)
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil,
		"2026-09-08T14:32:10Z ERROR schedule template route failed",
		"2026-09-08T14:31:00Z ERROR deployment route configuration failed")
	defer cleanup()
	a.repoRoot = indexed.repoRoot
	snapshot := healthyNegativePremisePacket().Application
	snapshot.DeployedAt = "2026-09-08T14:31:00Z"
	packet, evidence := a.buildDiagnosticPacket(context.Background(), snapshot, "Investigate the schedule template route after deployment in MyScheduler")
	if packet.Investigation.Rounds != diagnosticMaxEvidenceExpansionRounds || packet.Investigation.Rounds > diagnosticMaxEvidenceExpansionRounds {
		t.Fatalf("rounds=%d maximum=%d", packet.Investigation.Rounds, diagnosticMaxEvidenceExpansionRounds)
	}
	unique := sortedUniqueStrings(packet.Investigation.EvidenceExpansions)
	if len(unique) != len(packet.Investigation.EvidenceExpansions) {
		t.Fatalf("repeated expansion: %+v", packet.Investigation.EvidenceExpansions)
	}
	if countDiagnosticTool(evidence, "read_runtime_logs") != 1 || countDiagnosticTool(evidence, "read_deployment_logs") != 1 ||
		countDiagnosticTool(evidence, "search_repository") != 1 {
		t.Fatalf("evidence source repeated or missing: %+v", evidence)
	}
}

func TestDiagnosticEvidenceConfidenceLevels(t *testing.T) {
	tests := []struct {
		name, want string
		packet     func() diagnosticEvidencePacket
	}{
		{name: "direct causal evidence", want: "confirmed", packet: func() diagnosticEvidencePacket {
			packet := unhealthyDiagnosticPacket("2026-09-08T14:32:10Z ERROR configuration error: required backend URL is missing")
			return packet
		}},
		{name: "multiple aligned signals", want: "strongly_supported", packet: func() diagnosticEvidencePacket {
			return unhealthyDiagnosticPacket("2026-09-08T14:32:10Z ERROR request failed")
		}},
		{name: "partial evidence", want: "plausible", packet: func() diagnosticEvidencePacket {
			packet := healthyNegativePremisePacket()
			summary := summarizeDiagnosticLogs(logToolResponse{Kind: "runtime", Logs: `172.20.0.1 - - [08/Sep/2026:14:32:10 +0000] "GET / HTTP/1.1" 500 12 "-" "client" "-"`}, "", time.Now())
			packet.RuntimeLogs = &summary
			return packet
		}},
		{name: "missing evidence", want: "unknown", packet: func() diagnosticEvidencePacket {
			packet := healthyNegativePremisePacket()
			packet.RuntimeLogs = nil
			packet.Unavailable = []string{"runtime logs unavailable"}
			return packet
		}},
		{name: "conflicting evidence", want: "unknown", packet: func() diagnosticEvidencePacket {
			packet := healthyNegativePremisePacket()
			packet.Application.Services[0].State = "stopped"
			return packet
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assessment := assessDiagnosticEvidence(test.packet(), "Why is MyScheduler failing?")
			if assessment.ConfidenceCeiling != test.want {
				t.Fatalf("confidence=%q want %q assessment=%+v", assessment.ConfidenceCeiling, test.want, assessment)
			}
		})
	}
}

func TestDiagnosticAssessmentPreservesEliminationFacts(t *testing.T) {
	packet := unhealthyDiagnosticPacket("2026-09-08T14:32:10Z ERROR database timeout while loading schedules")
	matches := false
	packet.Application.VersionMismatch = &matches
	packet.Application.DeployedCommit = "abc123"
	packet.Application.RepositoryCommit = "abc123"
	assessment := assessDiagnosticEvidence(packet, "Why is MyScheduler unhealthy?")
	for label, values := range map[string][]string{
		"working":     assessment.Working,
		"failing":     assessment.Failing,
		"ruled_out":   assessment.RuledOut,
		"less_likely": assessment.LessLikely,
		"possible":    assessment.PossibleCauses,
		"unknowns":    assessment.Unknowns,
	} {
		if len(values) == 0 {
			t.Fatalf("%s facts missing: %+v", label, assessment)
		}
	}
	if !containsDiagnosticFact(assessment.RuledOut, "complete service-process outage") ||
		!containsDiagnosticFact(assessment.RuledOut, "version mismatch") ||
		!containsDiagnosticFact(assessment.LessLikely, "general database outage") {
		t.Fatalf("safe elimination facts missing: %+v", assessment)
	}
}

func TestContradictoryEvidencePreventsFalseElimination(t *testing.T) {
	packet := healthyNegativePremisePacket()
	packet.Application.Services[0].State = "stopped"
	assessment := assessDiagnosticEvidence(packet, "Why is MyScheduler failing?")
	if !assessment.Contradictory || assessment.ConfidenceCeiling != "unknown" ||
		containsDiagnosticFact(assessment.RuledOut, "complete service-process outage") {
		t.Fatalf("contradiction was not preserved conservatively: %+v", assessment)
	}
}

func unhealthyDiagnosticPacket(logs string) diagnosticEvidencePacket {
	packet := healthyNegativePremisePacket()
	packet.Application.DeploymentState = "unhealthy"
	packet.Application.Health = "unhealthy"
	for index := range packet.Application.Services {
		packet.Application.Services[index].Health = "unhealthy"
	}
	summary := summarizeDiagnosticLogs(logToolResponse{Kind: "runtime", Logs: logs}, "", time.Now())
	packet.RuntimeLogs = &summary
	return packet
}

func containsDiagnosticFact(values []string, fragment string) bool {
	fragment = strings.ToLower(fragment)
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), fragment) {
			return true
		}
	}
	return false
}

func countDiagnosticTool(evidence []diagnosticEvidence, name string) int {
	count := 0
	for _, item := range evidence {
		if item.ToolName == name {
			count++
		}
	}
	return count
}

func sortedUniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
