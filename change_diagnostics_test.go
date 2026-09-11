package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type changeDiagnosticHarness struct {
	app             *app
	close           func()
	statuses        map[string]string
	deploymentCalls int
	currentCalls    int
	historyCalls    int
	runtimeLogCalls int
	deployLogCalls  int
	methods         []string
	historyStatus   int
	historyVersions []any
	runtimeLogs     string
	deploymentLogs  string
}

func newChangeDiagnosticHarness(t *testing.T) *changeDiagnosticHarness {
	t.Helper()
	harness := &changeDiagnosticHarness{
		statuses: map[string]string{
			"myscheduler": "healthy",
			"golfmullet":  "healthy",
		},
		historyStatus: http.StatusOK,
		historyVersions: []any{map[string]any{
			"app":           "myscheduler",
			"container":     "myscheduler-previous",
			"image":         "myscheduler:previous",
			"port":          8081,
			"containerPort": 3000,
			"healthPath":    "/health",
			"strategy":      "node-express",
			"deployedAt":    "2026-09-10T12:34:56Z",
		}},
		deploymentLogs: "2026-09-10T12:35:00Z INFO deployment completed",
	}

	reactor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		harness.methods = append(harness.methods, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/deployments":
			harness.deploymentCalls++
			deployments := []any{}
			for _, appName := range []string{"myscheduler", "golfmullet"} {
				status := harness.statuses[appName]
				health := status
				if health == "degraded" {
					health = "unhealthy"
				}
				deployments = append(deployments, map[string]any{
					"app": appName, "status": status, "strategy": "node-express", "port": 8082,
					"containers": []any{map[string]any{
						"service": "app", "container": appName + "-current", "strategy": "node-express",
						"state": "running", "health": health, "restartCount": 0.0,
					}},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"deployments": deployments})
		case "/api/v1/system":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"services": []any{}, "collectedAt": "2026-09-10T13:00:00Z",
			})
		case "/api/v1/activity":
			_ = json.NewEncoder(w).Encode(map[string]any{"events": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))

	mini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		harness.methods = append(harness.methods, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/deployments":
			harness.currentCalls++
			current := []any{}
			for _, appName := range []string{"myscheduler", "golfmullet"} {
				current = append(current, map[string]any{
					"app": appName, "container": appName + "-current",
					"image": appName + ":current", "port": 8082,
					"containerPort": 3000, "strategy": "node-express",
				})
			}
			_ = json.NewEncoder(w).Encode(current)
		case strings.HasSuffix(r.URL.Path, "/history"):
			harness.historyCalls++
			if harness.historyStatus != http.StatusOK {
				http.Error(w, "private upstream detail", harness.historyStatus)
				return
			}
			appName := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/deployments/"), "/history")
			versions := make([]any, len(harness.historyVersions))
			copy(versions, harness.historyVersions)
			for _, raw := range versions {
				if version, ok := raw.(map[string]any); ok {
					version["app"] = appName
					version["container"] = appName + "-previous"
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"app": appName, "versions": versions})
		case strings.HasSuffix(r.URL.Path, "/deploy-logs"):
			harness.deployLogCalls++
			appName := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/deployments/"), "/deploy-logs")
			_ = json.NewEncoder(w).Encode(map[string]any{"app": appName, "logs": harness.deploymentLogs})
		case strings.HasSuffix(r.URL.Path, "/logs"):
			harness.runtimeLogCalls++
			appName := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/deployments/"), "/logs")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"app": appName, "container": appName + "-current", "logs": harness.runtimeLogs,
			})
		default:
			http.NotFound(w, r)
		}
	}))

	harness.close = func() {
		mini.Close()
		reactor.Close()
	}
	harness.app = &app{
		reactorURL: reactor.URL, minideployURL: mini.URL,
		repoRoot: t.TempDir(), client: http.DefaultClient,
	}
	return harness
}

func TestChangeAwareDiagnosticClassification(t *testing.T) {
	for _, message := range []string{
		"What changed before MyScheduler started failing?",
		"Did this break after the latest deploy?",
		"What changed in MyScheduler?",
		"Was this working before the most recent deployment?",
		"Was this caused by a deploy?",
		"What was different before?",
		"Did the previous version work?",
		"When did this start?",
		"What happened after the latest deployment?",
		"Compare this deployment with the previous one.",
		"Did the deployment strategy change?",
		"Was there a regression after rollback?",
	} {
		if !isChangeAwareDiagnosticQuestion(message) || !isDiagnosticReasoningQuestion(message) {
			t.Fatalf("change-aware question routed incorrectly: %q", message)
		}
	}
	for _, message := range []string{
		"Is MyScheduler healthy?",
		"Which route handles schedules in MyScheduler?",
		"What is different between MyScheduler's frontend and backend?",
		"What is different between these two API responses?",
		"Tell me a joke.",
	} {
		if isChangeAwareDiagnosticQuestion(message) {
			t.Fatalf("ordinary query became change-aware: %q", message)
		}
	}
}

func TestRichChangeAwareQuestionsReachCuratedReasoning(t *testing.T) {
	packet := healthyNegativePremisePacket()
	packet.Application.DeploymentImage = "myscheduler:current"
	packet.Application.DeploymentContainer = "myscheduler-current"
	packet.Application.ContainerPort = 3000
	packet.DeploymentHistory = &deploymentHistoryToolResponse{
		App: "myscheduler",
		Versions: []deploymentHistoryVersion{{
			Relation: "immediately_previous", Container: "myscheduler-previous",
			Image: "myscheduler:previous", Port: 8081, ContainerPort: 3000,
			Strategy: "node-express", ArchivedAt: "2026-09-10T12:34:56Z",
		}},
	}
	for _, message := range []string{
		"What changed before this started failing?",
		"Did this happen after the latest deploy?",
		"When did this start?",
		"Was this caused by the latest deploy?",
		"Did the previous version work?",
		"Compare this deployment to the previous one",
	} {
		if _, deterministic := deterministicChangeAwareDiagnosticAnswer(packet, message); deterministic {
			t.Errorf("rich question was intercepted: %q", message)
		}
	}
}

func TestChangeAwareExplicitAppFetchesFreshStateAndHistoryWithoutUnneededLogs(t *testing.T) {
	harness := newChangeDiagnosticHarness(t)
	defer harness.close()

	prepared, ok := harness.app.prepareConversationDiagnosticInvestigation(
		context.Background(),
		"What changed in MyScheduler?",
		nil,
	)
	if !ok {
		t.Fatal("expected change-aware diagnostic")
	}
	if prepared.packet.Application.App != "myscheduler" ||
		prepared.packet.DeploymentHistory == nil ||
		len(prepared.packet.DeploymentHistory.Versions) != 1 ||
		prepared.packet.Application.DeploymentImage != "myscheduler:current" {
		t.Fatalf("change evidence missing: %+v", prepared.packet)
	}
	if prepared.packet.RuntimeLogs != nil || prepared.packet.DeploymentLogs != nil ||
		harness.runtimeLogCalls != 0 || harness.deployLogCalls != 0 {
		t.Fatal("metadata-only change lookup fetched unnecessary logs")
	}
	if countDiagnosticTool(prepared.evidence, "get_app_context") != 1 ||
		countDiagnosticTool(prepared.evidence, "read_current_deployment") != 1 ||
		countDiagnosticTool(prepared.evidence, "read_deployment_history") != 1 ||
		harness.currentCalls != 1 || harness.historyCalls != 1 {
		t.Fatalf("evidence provenance missing: %+v", prepared.evidence)
	}
}

func TestChangeAwareConversationInheritanceAndCurrentOverride(t *testing.T) {
	harness := newChangeDiagnosticHarness(t)
	defer harness.close()
	harness.runtimeLogs = "2026-09-10T12:36:00Z ERROR database connection refused"

	inherited, ok := harness.app.prepareConversationDiagnosticInvestigation(
		context.Background(),
		"Did this happen after the latest deploy?",
		conversationRoutingHistory("myscheduler"),
	)
	if !ok || inherited.packet.Application.App != "myscheduler" ||
		inherited.packet.DeploymentHistory == nil || inherited.packet.RuntimeLogs == nil {
		t.Fatalf("follow-up did not inherit MyScheduler with fresh evidence: %+v", inherited.packet)
	}

	overridden, ok := harness.app.prepareConversationDiagnosticInvestigation(
		context.Background(),
		"What changed in GolfMullet?",
		conversationRoutingHistory("myscheduler"),
	)
	if !ok || overridden.packet.Application.App != "golfmullet" ||
		overridden.packet.DeploymentHistory == nil ||
		overridden.packet.DeploymentHistory.App != "golfmullet" {
		t.Fatalf("current message did not override historical subject: %+v", overridden.packet)
	}
}

func TestChangeAwareConversationAmbiguityRemainsSafe(t *testing.T) {
	harness := newChangeDiagnosticHarness(t)
	defer harness.close()

	if prepared, ok := harness.app.prepareConversationDiagnosticInvestigation(
		context.Background(),
		"Compare what changed in MyScheduler and GolfMullet after deployment.",
		conversationRoutingHistory("myscheduler"),
	); ok {
		t.Fatalf("explicit multi-app message should remain ambiguous: %+v", prepared.packet)
	}

	ambiguousHistory := []storedMessage{{
		Role: "assistant",
		Evidence: []evidence{
			{App: "myscheduler", ToolName: "get_app_context"},
			{App: "golfmullet", ToolName: "get_app_context"},
		},
	}}
	if prepared, ok := harness.app.prepareConversationDiagnosticInvestigation(
		context.Background(),
		"Did this happen after the latest deploy?",
		ambiguousHistory,
	); ok {
		t.Fatalf("ambiguous structured history should not guess: %+v", prepared.packet)
	}
}

func TestChangeAwareHealthyAppDoesNotInventFailure(t *testing.T) {
	harness := newChangeDiagnosticHarness(t)
	defer harness.close()
	prepared, ok := harness.app.prepareDiagnosticInvestigation(
		context.Background(),
		"Did MyScheduler break after the latest deploy?",
	)
	if !ok {
		t.Fatal("expected change-aware diagnostic")
	}
	if _, deterministic := deterministicChangeAwareDiagnosticAnswer(
		prepared.packet,
		"Did MyScheduler break after the latest deploy?",
	); deterministic {
		t.Fatal("rich timing question was incorrectly intercepted by a deterministic answer")
	}
	if prepared.packet.Assessment.Status != "healthy" ||
		len(prepared.packet.Assessment.Failing) != 0 ||
		containsDiagnosticFact(prepared.packet.Assessment.PossibleCauses, "regression") {
		t.Fatalf("healthy app assessment invented a failure: %+v", prepared.packet.Assessment)
	}
}

func TestChangeAwareFailureWithoutTimestampOrderingDoesNotClaimCorrelation(t *testing.T) {
	harness := newChangeDiagnosticHarness(t)
	defer harness.close()
	harness.statuses["myscheduler"] = "degraded"
	harness.runtimeLogs = "ERROR database connection refused"
	harness.deploymentLogs = "INFO deployment completed"

	prepared, ok := harness.app.prepareDiagnosticInvestigation(
		context.Background(),
		"Did MyScheduler break after the latest deploy?",
	)
	if !ok {
		t.Fatal("expected change-aware diagnostic")
	}
	if _, deterministic := deterministicChangeAwareDiagnosticAnswer(
		prepared.packet,
		"Did MyScheduler break after the latest deploy?",
	); deterministic {
		t.Fatal("rich causal question was incorrectly intercepted by a deterministic answer")
	}
	encoded := strings.ToLower(diagnosticPacketJSON(prepared.packet.Assessment))
	if strings.Contains(encoded, "plausible correlation") ||
		strings.Contains(encoded, "temporal correlation") ||
		strings.Contains(encoded, "caused by") {
		t.Fatalf("untimestamped failure produced deployment correlation: %+v", prepared.packet.Assessment)
	}
	if !containsDiagnosticFact(prepared.packet.Assessment.Unknowns, "does not establish whether") {
		t.Fatalf("missing timing limitation: %+v", prepared.packet.Assessment)
	}
	if harness.deployLogCalls != 1 || harness.runtimeLogCalls != 1 {
		t.Fatalf("failure/timing evidence calls: deployment=%d runtime=%d", harness.deployLogCalls, harness.runtimeLogCalls)
	}
}

func TestChangeAwareTimestampedFailureAfterCutoverIsCorrelationNotCausation(t *testing.T) {
	harness := newChangeDiagnosticHarness(t)
	defer harness.close()
	harness.statuses["myscheduler"] = "degraded"
	harness.runtimeLogs = "2026-09-10T12:36:00Z ERROR database connection refused"

	prepared, ok := harness.app.prepareDiagnosticInvestigation(
		context.Background(),
		"Did MyScheduler break after the latest deploy?",
	)
	if !ok {
		t.Fatal("expected change-aware diagnostic")
	}
	encoded := strings.ToLower(diagnosticPacketJSON(prepared.packet.Assessment))
	if !strings.Contains(encoded, "temporal correlation only") ||
		!strings.Contains(encoded, "does not establish causation") ||
		strings.Contains(encoded, "caused by") {
		t.Fatalf("timestamp ordering was not bounded as correlation: %+v", prepared.packet.Assessment)
	}
}

func TestDifferentContainerIdentityAloneIsNotDeploymentCorrelation(t *testing.T) {
	packet := unhealthyDiagnosticPacket("")
	packet.Application.DeploymentContainer = "myscheduler-current"
	packet.Application.DeploymentImage = "myscheduler:same"
	packet.DeploymentHistory = &deploymentHistoryToolResponse{
		App: "myscheduler",
		Versions: []deploymentHistoryVersion{{
			Relation: "immediately_previous", Container: "myscheduler-previous",
			Image: "myscheduler:same", ArchivedAt: "2026-09-10T12:34:56Z",
		}},
	}
	packet.Assessment = assessDiagnosticEvidence(packet, "Did MyScheduler break after the latest deploy?")
	encoded := strings.ToLower(diagnosticPacketJSON(packet.Assessment))
	if !strings.Contains(encoded, "identity transition only") ||
		strings.Contains(encoded, "temporal correlation") ||
		strings.Contains(encoded, "plausible correlation") ||
		strings.Contains(encoded, "regression remains possible") {
		t.Fatalf("container churn was treated as deployment correlation: %+v", packet.Assessment)
	}
}

func TestChangeComparisonRetainsAllBoundedKnownFacts(t *testing.T) {
	packet := healthyNegativePremisePacket()
	packet.Application.DeploymentImage = "myscheduler:current"
	packet.Application.DeploymentContainer = "myscheduler-current"
	packet.Application.Strategy = "node-express"
	packet.Application.ListenerPort = 8082
	packet.Application.ContainerPort = 3000
	packet.Application.DeploymentServices = []deploymentHistoryService{{
		Name: "backend", Image: "backend:current", Strategy: "node-express", ContainerPort: 3000,
	}}
	previous := deploymentHistoryVersion{
		Relation: "immediately_previous", Container: "myscheduler-previous",
		Image: "myscheduler:previous", Strategy: "node-express",
		Port: 8081, ContainerPort: 3000,
		Services: []deploymentHistoryService{{
			Name: "backend", Image: "backend:previous", Strategy: "node-express", ContainerPort: 3000,
		}},
	}
	packet.DeploymentHistory = &deploymentHistoryToolResponse{
		App: "myscheduler", Versions: []deploymentHistoryVersion{previous},
	}
	facts := strings.ToLower(strings.Join(deploymentHistoryComparisonFacts(packet.Application, previous), "\n"))
	for _, expected := range []string{
		"image changed", "named service metadata changed", "backend image", "strategy is unchanged",
		"host port", "container ports both", "identity transition only",
	} {
		if !strings.Contains(facts, expected) {
			t.Errorf("missing comparison fact %q in %s", expected, facts)
		}
	}
	if _, deterministic := deterministicChangeAwareDiagnosticAnswer(
		packet,
		"Compare this deployment to the previous one",
	); deterministic {
		t.Fatal("multi-fact comparison should reach curated reasoning")
	}
	bounded := boundedDiagnosticPacketJSON(&packet)
	for _, value := range []string{"myscheduler:current", "myscheduler:previous", "8082", "8081", "3000"} {
		if !strings.Contains(bounded, value) {
			t.Errorf("curated packet lost comparison value %q: %s", value, bounded)
		}
	}
}

func TestTruncatedIdentifiersDoNotProduceEqualityClaims(t *testing.T) {
	prefix := strings.Repeat("x", deploymentHistoryIdentifierMaxRunes)
	current := appDiagnosticSnapshot{
		DeploymentImage:                prefix,
		DeploymentContainer:            prefix,
		Strategy:                       prefix,
		DeploymentIdentifiersTruncated: true,
	}
	previous := deploymentHistoryVersion{
		Image: prefix, Container: prefix, Strategy: prefix, IdentifiersTruncated: true,
	}
	facts := strings.ToLower(strings.Join(deploymentHistoryComparisonFacts(current, previous), "\n"))
	if strings.Contains(facts, "both record") || strings.Contains(facts, "unchanged") ||
		strings.Contains(facts, "same recorded identifiers") {
		t.Fatalf("bounded prefixes were treated as exact equality: %s", facts)
	}
	if !containsDiagnosticFact(deploymentHistoryComparisonUnknowns(current, previous), "identifier comparison is incomplete") {
		t.Fatal("identifier truncation limitation was not exposed")
	}
}

func TestPreviousVersionHealthRemainsUnknown(t *testing.T) {
	packet := healthyNegativePremisePacket()
	packet.Application.DeploymentImage = "myscheduler:current"
	packet.DeploymentHistory = &deploymentHistoryToolResponse{
		App: "myscheduler",
		Versions: []deploymentHistoryVersion{{
			Relation: "immediately_previous", Image: "myscheduler:previous",
		}},
	}
	packet.Assessment = assessDiagnosticEvidence(packet, "Did the previous version work?")
	if !containsDiagnosticFact(packet.Assessment.Unknowns, "previous runtime health") {
		t.Fatalf("historical health limitation missing: %+v", packet.Assessment)
	}
}

func TestWhenDidThisStartUsesOnlySupportedTimestampOrdering(t *testing.T) {
	t.Run("timestamped", func(t *testing.T) {
		harness := newChangeDiagnosticHarness(t)
		defer harness.close()
		harness.statuses["myscheduler"] = "degraded"
		harness.runtimeLogs = "2026-09-10T12:36:00Z ERROR request failed"
		prepared, ok := harness.app.prepareConversationDiagnosticInvestigation(
			context.Background(), "When did this start?", conversationRoutingHistory("myscheduler"),
		)
		if !ok || !containsDiagnosticFact(prepared.packet.Assessment.KeyEvidence, "temporal correlation only") {
			t.Fatalf("supported timestamp ordering missing: %+v", prepared.packet.Assessment)
		}
	})

	t.Run("untimestamped", func(t *testing.T) {
		harness := newChangeDiagnosticHarness(t)
		defer harness.close()
		harness.statuses["myscheduler"] = "degraded"
		harness.runtimeLogs = "ERROR request failed"
		prepared, ok := harness.app.prepareConversationDiagnosticInvestigation(
			context.Background(), "When did this start?", conversationRoutingHistory("myscheduler"),
		)
		if !ok || !containsDiagnosticFact(prepared.packet.Assessment.Unknowns, "does not establish whether") ||
			containsDiagnosticFact(prepared.packet.Assessment.KeyEvidence, "temporal correlation") {
			t.Fatalf("unsupported timestamp ordering was not kept unknown: %+v", prepared.packet.Assessment)
		}
	})
}

func TestDeploymentServiceTopologyRequiresCompleteSets(t *testing.T) {
	rawServices := make([]miniDeployHistoryService, 0, deploymentHistoryMaxServices+1)
	for index := 0; index < deploymentHistoryMaxServices+1; index++ {
		rawServices = append(rawServices, miniDeployHistoryService{
			Name: fmt.Sprintf("service-%d", index), Container: fmt.Sprintf("current-%d", index),
			Image: "app:same", ContainerPort: 3000,
		})
	}
	currentRaw, err := json.Marshal([]miniDeployHistoryVersion{{
		App: "myscheduler", Services: rawServices,
	}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := decodeCurrentDeployment("myscheduler", currentRaw)
	if err != nil {
		t.Fatal(err)
	}
	previous, _ := sanitizeDeploymentHistoryVersion(miniDeployHistoryVersion{
		App: "myscheduler", Services: rawServices,
	}, 0)
	snapshot := appDiagnosticSnapshot{}
	applyCurrentDeploymentMetadata(&snapshot, current)
	if len(snapshot.DeploymentServices) != deploymentHistoryMaxServices ||
		!snapshot.DeploymentServicesTruncated || !previous.ServicesTruncated {
		t.Fatalf("oversized services were not bounded and marked incomplete: current=%+v previous=%+v", snapshot, previous)
	}
	facts := strings.ToLower(strings.Join(deploymentHistoryComparisonFacts(snapshot, previous), "\n"))
	if strings.Contains(facts, "service topology") {
		t.Fatalf("truncated identical sets produced a topology assertion: %s", facts)
	}
	if !containsDiagnosticFact(deploymentHistoryComparisonUnknowns(snapshot, previous), "comparison is incomplete") {
		t.Fatal("truncated topology did not expose incompleteness")
	}

	completeCurrent := appDiagnosticSnapshot{DeploymentServices: []deploymentHistoryService{
		{Name: "frontend"}, {Name: "backend"},
	}}
	completePrevious := deploymentHistoryVersion{Services: []deploymentHistoryService{
		{Name: "frontend"}, {Name: "worker"},
	}}
	completeFacts := strings.ToLower(strings.Join(deploymentHistoryComparisonFacts(completeCurrent, completePrevious), "\n"))
	if !strings.Contains(completeFacts, "service topology changed") {
		t.Fatalf("complete topology difference was not reported: %s", completeFacts)
	}

	completePrevious.ServicesTruncated = true
	incompleteFacts := strings.ToLower(strings.Join(deploymentHistoryComparisonFacts(completeCurrent, completePrevious), "\n"))
	if strings.Contains(incompleteFacts, "service topology changed") {
		t.Fatalf("incomplete historical topology produced a difference: %s", incompleteFacts)
	}
}

func TestChangeAwareDirectRuntimeCauseDoesNotProveDeploymentCausality(t *testing.T) {
	packet := unhealthyDiagnosticPacket(
		"2026-09-10T12:36:00Z ERROR configuration error: required backend URL is missing",
	)
	packet.Application.Services[0].Container = "myscheduler-current"
	packet.DeploymentHistory = &deploymentHistoryToolResponse{
		App: "myscheduler",
		Versions: []deploymentHistoryVersion{{
			Relation: "immediately_previous", Container: "myscheduler-previous",
		}},
	}
	packet.Assessment = assessDiagnosticEvidence(packet, "Did MyScheduler break after the latest deploy?")
	if packet.Assessment.ConfidenceCeiling != "plausible" {
		t.Fatalf("runtime cause incorrectly proved deployment causality: %+v", packet.Assessment)
	}
}

func TestCurrentHealthyStateDoesNotExcludeDeploymentCausality(t *testing.T) {
	packet := healthyNegativePremisePacket()
	packet.DeploymentHistory = &deploymentHistoryToolResponse{
		App: "myscheduler",
		Versions: []deploymentHistoryVersion{{
			Relation: "immediately_previous", ArchivedAt: "2026-09-10T12:35:00Z",
		}},
	}
	packet.Assessment = assessDiagnosticEvidence(packet, "Was this caused by the latest deploy?")
	if containsDiagnosticFact(packet.Assessment.LessLikely, "deployment-caused") {
		t.Fatalf("current health was converted into negative deployment causality: %+v", packet.Assessment)
	}
	if !containsDiagnosticFact(packet.Assessment.Unknowns, "current state does not establish") ||
		!containsDiagnosticFact(packet.Assessment.Unknowns, "previous runtime health") ||
		!containsDiagnosticFact(packet.Assessment.Unknowns, "does not establish whether") {
		t.Fatalf("historical health or ordering uncertainty missing: %+v", packet.Assessment)
	}

	answer, ok := deterministicUncertainDeploymentCausalityAnswer(packet, "Was this caused by the latest deploy?")
	if !ok {
		t.Fatal("expected deterministic uncertainty fallback")
	}
	lower := strings.ToLower(answer)
	if !strings.Contains(lower, "current state is healthy") ||
		!strings.Contains(lower, "does not establish whether") ||
		!strings.Contains(lower, "cannot rule the deployment in or out") {
		t.Fatalf("fallback did not preserve known and unknown facts: %s", answer)
	}
	if containsUnsupportedNegativeDeploymentCausality(answer) {
		t.Fatalf("fallback contains unsupported negative causality: %s", answer)
	}
}

func TestChangeAwareEmptyAndUnavailableHistoryDegradeGracefully(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		harness := newChangeDiagnosticHarness(t)
		defer harness.close()
		harness.historyVersions = []any{}
		prepared, ok := harness.app.prepareDiagnosticInvestigation(
			context.Background(), "What changed in MyScheduler?",
		)
		if !ok || prepared.packet.DeploymentHistory == nil {
			t.Fatalf("empty history diagnostic missing: %+v", prepared.packet)
		}
		answer, _ := deterministicChangeAwareDiagnosticAnswer(prepared.packet, "What changed in MyScheduler?")
		if !strings.Contains(strings.ToLower(answer), "no previous deployment history") {
			t.Fatalf("empty history answer=%s", answer)
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		harness := newChangeDiagnosticHarness(t)
		defer harness.close()
		harness.historyStatus = http.StatusServiceUnavailable
		prepared, ok := harness.app.prepareDiagnosticInvestigation(
			context.Background(), "What changed in MyScheduler?",
		)
		if !ok || prepared.packet.DeploymentHistory != nil ||
			!containsDiagnosticFact(prepared.packet.Unavailable, "deployment history unavailable") {
			t.Fatalf("unavailable history was not isolated: %+v", prepared.packet)
		}
		answer, _ := deterministicChangeAwareDiagnosticAnswer(prepared.packet, "What changed in MyScheduler?")
		if !strings.Contains(strings.ToLower(answer), "history is unavailable") ||
			!strings.Contains(strings.ToLower(answer), "current state is healthy") {
			t.Fatalf("unavailable history answer=%s", answer)
		}
	})
}

func TestChangeAwareRepositoryEvidenceIsCurrentCheckoutNotDeployedDiff(t *testing.T) {
	packet := healthyNegativePremisePacket()
	packet.Application.SourceStatus["repository"] = "ok"
	packet.Application.Services[0].Container = "myscheduler-current"
	packet.DeploymentHistory = &deploymentHistoryToolResponse{
		App: "myscheduler",
		Versions: []deploymentHistoryVersion{{
			Relation: "immediately_previous", Container: "myscheduler-previous", Image: "myscheduler:previous",
		}},
	}
	packet.Repository = []repositoryLocationEvidence{{
		Path: "backend/config.go", StartLine: 10, EndLine: 18, Snippet: "current configuration",
	}}
	packet.Assessment = assessDiagnosticEvidence(packet, "What code changed after the latest deploy in MyScheduler?")

	if !containsDiagnosticFact(packet.Assessment.KeyEvidence, "current-checkout") ||
		!containsDiagnosticFact(packet.Assessment.KeyEvidence, "not an exact diff") ||
		!containsDiagnosticFact(packet.Assessment.Unknowns, "exact Git commit") {
		t.Fatalf("repository/deployment distinction missing: %+v", packet.Assessment)
	}
}

func TestChangeAwareAvailableRepositoryCanSupplementDeploymentEvidence(t *testing.T) {
	_, indexed, _ := makeIndexedRepo(t)
	harness := newChangeDiagnosticHarness(t)
	defer harness.close()
	harness.app.repoRoot = indexed.repoRoot

	prepared, ok := harness.app.prepareDiagnosticInvestigation(
		context.Background(),
		"What changed in the schedule template route after the latest deployment in MyScheduler?",
	)
	if !ok {
		t.Fatal("expected change-aware repository diagnostic")
	}
	if len(prepared.packet.Repository) == 0 ||
		prepared.packet.Repository[0].Path != "backend/routes/scheduleTemplateRoutes.js" ||
		countDiagnosticTool(prepared.evidence, "search_repository") != 1 ||
		countDiagnosticTool(prepared.evidence, "read_repository_file") != 1 ||
		!containsDiagnosticFact(prepared.packet.Assessment.KeyEvidence, "current-checkout") ||
		!containsDiagnosticFact(prepared.packet.Assessment.KeyEvidence, "not an exact diff") {
		t.Fatalf("repository did not safely supplement deployment evidence: packet=%+v evidence=%+v", prepared.packet, prepared.evidence)
	}
}

func TestChangeAwareTimestampAndCommitUnknownsAreExplicit(t *testing.T) {
	harness := newChangeDiagnosticHarness(t)
	defer harness.close()
	prepared, ok := harness.app.prepareDiagnosticInvestigation(
		context.Background(), "What changed in MyScheduler after the latest deployment?",
	)
	if !ok {
		t.Fatal("expected change-aware diagnostic")
	}
	encoded := boundedDiagnosticPacketJSON(&prepared.packet)
	if !strings.Contains(encoded, "archive/cutover") &&
		!strings.Contains(encoded, "not the original deployment time") {
		t.Fatalf("archive timestamp semantics missing: %s", encoded)
	}
	if !containsDiagnosticFact(prepared.packet.Assessment.Unknowns, "exact Git commit") {
		t.Fatalf("missing commit limitation: %+v", prepared.packet.Assessment)
	}
	answer, _ := deterministicChangeAwareDiagnosticAnswer(
		prepared.packet,
		"What changed in MyScheduler after the latest deployment?",
	)
	lower := strings.ToLower(answer)
	if strings.Contains(lower, "commit abc") ||
		strings.Contains(lower, "previous version was deployed at") {
		t.Fatalf("answer invented commit/original deployment time: %s", answer)
	}
}

func TestChangeAwareFollowUpRefetchesCurrentState(t *testing.T) {
	harness := newChangeDiagnosticHarness(t)
	defer harness.close()
	history := conversationRoutingHistory("myscheduler")

	first, ok := harness.app.prepareConversationDiagnosticInvestigation(
		context.Background(), "Did this happen after the latest deploy?", history,
	)
	if !ok || first.packet.Application.DeploymentState != "healthy" {
		t.Fatalf("first state=%+v", first.packet.Application)
	}
	callsAfterFirst := harness.deploymentCalls
	currentCallsAfterFirst := harness.currentCalls
	historyCallsAfterFirst := harness.historyCalls
	harness.statuses["myscheduler"] = "degraded"
	harness.runtimeLogs = "2026-09-10T12:36:00Z ERROR new failure"

	second, ok := harness.app.prepareConversationDiagnosticInvestigation(
		context.Background(), "Did this happen after the latest deploy?", history,
	)
	if !ok || second.packet.Application.DeploymentState != "degraded" ||
		harness.deploymentCalls <= callsAfterFirst ||
		harness.currentCalls != currentCallsAfterFirst+1 ||
		harness.historyCalls != historyCallsAfterFirst+1 {
		t.Fatalf("follow-up request counts: state=%+v reactor=%d/%d current=%d/%d history=%d/%d",
			second.packet.Application, harness.deploymentCalls, callsAfterFirst,
			harness.currentCalls, currentCallsAfterFirst, harness.historyCalls, historyCallsAfterFirst)
	}
}

func TestDeploymentHistoryIsNotFetchedForOrdinaryDiagnostics(t *testing.T) {
	harness := newChangeDiagnosticHarness(t)
	defer harness.close()
	harness.runtimeLogs = "2026-09-10T12:36:00Z ERROR request failed"

	if _, ok := harness.app.prepareDiagnosticInvestigation(
		context.Background(), "Why is MyScheduler failing?",
	); !ok {
		t.Fatal("expected ordinary diagnostic")
	}
	if harness.historyCalls != 0 {
		t.Fatalf("ordinary diagnostic fetched deployment history %d times", harness.historyCalls)
	}
}

func TestGenericDifferentComparisonDoesNotFetchChangeEvidence(t *testing.T) {
	harness := newChangeDiagnosticHarness(t)
	defer harness.close()

	if _, ok := harness.app.prepareDiagnosticInvestigation(
		context.Background(), "What is different between MyScheduler's frontend and backend?",
	); ok {
		t.Fatal("ordinary frontend/backend comparison became a deployment diagnostic")
	}
	if harness.currentCalls != 0 || harness.historyCalls != 0 || harness.deployLogCalls != 0 {
		t.Fatalf("ordinary comparison fetched change evidence: current=%d history=%d deploy_logs=%d",
			harness.currentCalls, harness.historyCalls, harness.deployLogCalls)
	}
}

func TestUnknownOrDeletedAppDoesNotFetchMiniDeployChangeEvidence(t *testing.T) {
	harness := newChangeDiagnosticHarness(t)
	defer harness.close()

	if prepared, ok := harness.app.prepareDiagnosticInvestigation(
		context.Background(), "What changed in DeletedApp after deployment?",
	); ok {
		t.Fatalf("deleted app unexpectedly resolved: %+v", prepared.packet)
	}
	if harness.currentCalls != 0 || harness.historyCalls != 0 {
		t.Fatalf("unresolved app fetched MiniDeploy evidence: current=%d history=%d", harness.currentCalls, harness.historyCalls)
	}
	if !strings.Contains(strings.ToLower(unresolvedChangeAwareDiagnosticAnswer()), "could not resolve exactly one") {
		t.Fatal("unresolved change-aware answer is not explicit")
	}
}

func TestChangeAwareInvestigationUsesOnlyReadRequests(t *testing.T) {
	harness := newChangeDiagnosticHarness(t)
	defer harness.close()
	if _, ok := harness.app.prepareDiagnosticInvestigation(
		context.Background(), "What changed in MyScheduler?",
	); !ok {
		t.Fatal("expected change-aware diagnostic")
	}
	for _, request := range harness.methods {
		if !strings.HasPrefix(request, http.MethodGet+" ") {
			t.Fatalf("change-aware diagnostics invoked a mutating request: %s", request)
		}
	}
}
