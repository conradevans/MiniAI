package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const liveComparisonDiagnosticQuery = "Investigate whether MyScheduler has any hidden issues by comparing its current service health, database state, and recent runtime logs. If anything looks concerning, explain what and why."

func diagnosticResponseJSON(t *testing.T, response diagnosticFinalResponse) string {
	t.Helper()
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func newDiagnosticTestApp(t *testing.T, deployments []any, services []any, runtimeLogs, deploymentLogs string) (*app, func()) {
	t.Helper()
	reactor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/deployments":
			_ = json.NewEncoder(w).Encode(map[string]any{"deployments": deployments})
		case "/api/v1/system":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"services": services, "collectedAt": "2026-09-08T20:00:00Z",
			})
		case "/api/v1/activity":
			_ = json.NewEncoder(w).Encode(map[string]any{"events": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	mini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/deployments":
			current := []any{}
			for _, raw := range deployments {
				deployment, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				appName := stringField(deployment, "app")
				if appName == "" {
					continue
				}
				item := map[string]any{
					"app": appName, "image": appName + ":current",
					"port": deployment["port"], "strategy": deployment["strategy"],
				}
				containers, _ := deployment["containers"].([]any)
				if len(containers) > 0 {
					if container, ok := containers[0].(map[string]any); ok {
						item["container"] = container["container"]
						item["containerPort"] = container["containerPort"]
					}
				}
				current = append(current, item)
			}
			_ = json.NewEncoder(w).Encode(current)
		case strings.HasSuffix(r.URL.Path, "/history"):
			_ = json.NewEncoder(w).Encode(map[string]any{"app": "myscheduler", "versions": []any{}})
		case strings.HasSuffix(r.URL.Path, "/deploy-logs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"app": "myscheduler", "logs": deploymentLogs})
		case strings.HasSuffix(r.URL.Path, "/logs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"app": "myscheduler", "container": "backend", "logs": runtimeLogs})
		default:
			http.NotFound(w, r)
		}
	}))
	cleanup := func() {
		mini.Close()
		reactor.Close()
	}
	return &app{
		reactorURL: reactor.URL, minideployURL: mini.URL,
		repoRoot: t.TempDir(), client: http.DefaultClient,
	}, cleanup
}

func healthyMySchedulerDeployment() map[string]any {
	return map[string]any{
		"app": "myscheduler", "status": "healthy", "strategy": "fullstack-vite-node",
		"containers": []any{
			map[string]any{
				"service": "frontend", "container": "myscheduler-current-frontend", "strategy": "vite-static",
				"state": "running", "health": "healthy", "uptimeSeconds": 900.0, "restartCount": 0.0,
			},
			map[string]any{
				"service": "backend", "container": "myscheduler-current-backend", "strategy": "node-express",
				"state": "running", "health": "healthy", "uptimeSeconds": 850.0, "restartCount": 1.0,
			},
		},
		"database": map[string]any{"state": "linked", "displayName": "MyScheduler Production", "status": "ready"},
	}
}

func healthyNegativePremisePacket() diagnosticEvidencePacket {
	allRunning := true
	return diagnosticEvidencePacket{
		Application: appDiagnosticSnapshot{
			App:                "myscheduler",
			DeploymentState:    "healthy",
			Health:             "healthy",
			AllServicesRunning: &allRunning,
			Services: []diagnosticService{
				{Name: "frontend", State: "running", Health: "healthy"},
				{Name: "backend", State: "running", Health: "healthy"},
			},
			Database: &diagnosticDatabase{State: "linked", Status: "ready"},
			SourceStatus: map[string]string{
				"reactorlab_deployment": "ok",
				"reactorlab_system":     "ok",
				"reactorlab_activity":   "ok",
			},
		},
		RuntimeLogs: &diagnosticLogSummary{
			Kind:             "runtime",
			LinesExamined:    114,
			HTTPStatusCounts: map[string]int{"200": 114},
			Patterns:         []diagnosticLogPattern{},
		},
		Unavailable: []string{},
	}
}

func TestDeterministicRunningAnswerNeedsNoModel(t *testing.T) {
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, "", "")
	defer cleanup()
	recorder := httptest.NewRecorder()
	capture := newSSECaptureWriter(recorder)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	if !a.handleDeterministicDiagnosticLookup(capture, req, "Is MyScheduler running?") {
		t.Fatal("expected deterministic running answer")
	}
	if !strings.Contains(strings.ToLower(capture.answer.String()), "myscheduler is running") ||
		!strings.Contains(recorder.Body.String(), `"model_invoked":false`) {
		t.Fatalf("answer=%q stream=%s", capture.answer.String(), recorder.Body.String())
	}
	if len(capture.evidence) != 1 || capture.evidence[0].ToolName != "get_app_context" {
		t.Fatalf("evidence=%+v", capture.evidence)
	}
}

func TestDeterministicDiagnosticPrecedesOllamaChecks(t *testing.T) {
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, "", "")
	defer cleanup()
	modelCalls := 0
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		http.Error(w, "unexpected Ollama request", http.StatusInternalServerError)
	}))
	defer ollama.Close()
	a.ollamaURL = ollama.URL
	a.sessions = map[string]*session{
		"diagnostic-session": {
			ID:            "diagnostic-session",
			CreatedAt:     time.Now(),
			LastHeartbeat: time.Now(),
			LastChat:      time.Now(),
		},
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", strings.NewReader(
		`{"session_id":"diagnostic-session","message":"Is MyScheduler running?"}`,
	))
	a.handleChatStream(recorder, req)
	if modelCalls != 0 || !strings.Contains(recorder.Body.String(), `"model_invoked":false`) {
		t.Fatalf("model_calls=%d stream=%s", modelCalls, recorder.Body.String())
	}
}

func TestCleanVerificationPrecedesOllamaChecks(t *testing.T) {
	logs := `172.20.0.1 - - [08/Sep/2026:19:43:55 +0000] "GET /health HTTP/1.1" 200 42 "-" "client/2.0" "-"`
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, logs, "")
	defer cleanup()
	ollamaCalls := 0
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ollamaCalls++
		http.Error(w, "unexpected Ollama request", http.StatusInternalServerError)
	}))
	defer ollama.Close()
	a.ollamaURL = ollama.URL
	a.sessions = map[string]*session{
		"verification-session": {
			ID:            "verification-session",
			CreatedAt:     time.Now(),
			LastHeartbeat: time.Now(),
			LastChat:      time.Now(),
		},
	}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", strings.NewReader(
		`{"session_id":"verification-session","message":"Is MyScheduler okay?"}`,
	))
	a.handleChatStream(recorder, req)
	if ollamaCalls != 0 || !strings.Contains(recorder.Body.String(), `"model_invoked":false`) ||
		!strings.Contains(recorder.Body.String(), `"diagnostic_confidence":"strongly_supported"`) {
		t.Fatalf("ollama_calls=%d stream=%s", ollamaCalls, recorder.Body.String())
	}
}

func TestDeterministicMiniBaseServiceHealth(t *testing.T) {
	services := []any{map[string]any{"name": "MiniBase", "unit": "minibase.service", "status": "active", "active": true}}
	a, cleanup := newDiagnosticTestApp(t, nil, services, "", "")
	defer cleanup()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	if !a.handleDeterministicDiagnosticLookup(recorder, req, "Is MiniBase healthy?") {
		t.Fatal("expected deterministic MiniBase service answer")
	}
	if !strings.Contains(recorder.Body.String(), "active") || !strings.Contains(recorder.Body.String(), `"model_invoked":false`) {
		t.Fatalf("stream=%s", recorder.Body.String())
	}
}

func TestDeterministicDeployedCommitWhenPresent(t *testing.T) {
	deployment := healthyMySchedulerDeployment()
	deployment["commit"] = "abcdef123456"
	a, cleanup := newDiagnosticTestApp(t, []any{deployment}, nil, "", "")
	defer cleanup()
	recorder := httptest.NewRecorder()
	capture := newSSECaptureWriter(recorder)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	if !a.handleDeterministicDiagnosticLookup(capture, req, "What commit is MyScheduler deployed on?") {
		t.Fatal("expected deterministic deployed-commit answer")
	}
	if !strings.Contains(capture.answer.String(), "abcdef123456") ||
		!strings.Contains(recorder.Body.String(), `"model_invoked":false`) {
		t.Fatalf("answer=%q stream=%s", capture.answer.String(), recorder.Body.String())
	}
}

func TestAccessLogExtractsOnlyStructuredHTTPStatus(t *testing.T) {
	line := `172.20.0.1 - - [08/Sep/2026:19:43:55 +0000] "GET / HTTP/1.1" 200 567 "-" "Go-http-client/1.1" "-"`
	summary := summarizeDiagnosticLogs(logToolResponse{Kind: "runtime", Logs: line}, "", time.Now())
	if summary.HTTPStatusCounts["200"] != 1 || len(summary.HTTPStatusCounts) != 1 {
		t.Fatalf("status counts=%+v", summary.HTTPStatusCounts)
	}
	if summary.HTTPStatusCounts["172"] != 0 || summary.HTTPStatusCounts["567"] != 0 {
		t.Fatalf("IP octet or byte count became a status: %+v", summary.HTTPStatusCounts)
	}
	if summary.ErrorCount != 0 || len(summary.Patterns) != 0 {
		t.Fatalf("successful access line became an error: %+v", summary)
	}
}

func TestAccessLogRecognizesServerErrorButNotOtherNumbers(t *testing.T) {
	line := `172.20.0.1 - - [08/Sep/2026:19:43:55 +0000] "POST /api/schedules HTTP/1.1" 500 567 "-" "client/2.0" "-"`
	summary := summarizeDiagnosticLogs(logToolResponse{Kind: "runtime", Logs: line}, "", time.Now())
	if summary.HTTPStatusCounts["500"] != 1 || len(summary.HTTPStatusCounts) != 1 ||
		summary.ErrorCount != 1 || len(summary.Patterns) != 1 || summary.Patterns[0].Level != "error" {
		t.Fatalf("server error was not parsed conservatively: %+v", summary)
	}
}

func TestAccessLogTracksClientStatusWithoutCallingItServerError(t *testing.T) {
	line := `172.20.0.1 - - [08/Sep/2026:19:43:55 +0000] "GET /missing HTTP/1.1" 404 123 "-" "client/2.0" "-"`
	summary := summarizeDiagnosticLogs(logToolResponse{Kind: "runtime", Logs: line}, "", time.Now())
	if summary.HTTPStatusCounts["404"] != 1 || summary.ErrorCount != 0 || len(summary.Patterns) != 0 {
		t.Fatalf("client response was incorrectly treated as a server error: %+v", summary)
	}
}

func TestRepeatedAccessLogServerErrorsAreDeduplicated(t *testing.T) {
	logs := strings.Join([]string{
		`172.20.0.1 - - [08/Sep/2026:19:43:55 +0000] "GET /api/schedules HTTP/1.1" 500 567 "-" "client/2.0" "-"`,
		`172.20.0.1 - - [08/Sep/2026:19:44:55 +0000] "GET /api/schedules HTTP/1.1" 500 612 "-" "client/2.0" "-"`,
	}, "\n")
	summary := summarizeDiagnosticLogs(logToolResponse{Kind: "runtime", Logs: logs}, "", time.Now())
	if summary.HTTPStatusCounts["500"] != 2 || summary.ErrorCount != 2 ||
		len(summary.Patterns) != 1 || summary.Patterns[0].Occurrences != 2 {
		t.Fatalf("repeated server errors were not counted and deduplicated: %+v", summary)
	}
}

func TestDiagnosticLogSummaryDeduplicatesAndPreservesRange(t *testing.T) {
	logs := strings.Join([]string{
		"2026-09-08T14:37:52Z ERROR database timeout request 84",
		"2026-09-08T14:32:10Z ERROR database timeout request 12",
		"2026-09-08T14:35:00Z ERROR database timeout request 41",
		"2026-09-08T14:36:00Z WARN retrying connection",
	}, "\n")
	summary := summarizeDiagnosticLogs(logToolResponse{Kind: "runtime", Logs: logs}, "", time.Now())
	if summary.LinesExamined != 4 || summary.ErrorCount != 3 || summary.WarningCount != 1 {
		t.Fatalf("summary=%+v", summary)
	}
	if summary.FirstErrorTimestamp != "2026-09-08T14:32:10Z" || summary.LastErrorTimestamp != "2026-09-08T14:37:52Z" {
		t.Fatalf("range=%s..%s", summary.FirstErrorTimestamp, summary.LastErrorTimestamp)
	}
	if len(summary.Patterns) < 1 || summary.Patterns[0].Occurrences != 3 || summary.LinesSent > diagnosticPatternLimit {
		t.Fatalf("patterns=%+v lines_sent=%d", summary.Patterns, summary.LinesSent)
	}
}

func TestDiagnosticLogSummaryAndPacketAreBounded(t *testing.T) {
	var lines []string
	for i := 0; i < 1000; i++ {
		lines = append(lines, fmt.Sprintf("2026-09-08T14:32:%02dZ ERROR request %d failed with status 500 %s", i%60, i, strings.Repeat("x", 500)))
	}
	summary := summarizeDiagnosticLogs(logToolResponse{Kind: "runtime", Logs: strings.Join(lines, "\n")}, "", time.Now())
	if summary.LinesExamined != diagnosticLogLines || summary.LinesSent > diagnosticPatternLimit || !summary.Truncated {
		t.Fatalf("unbounded summary: %+v", summary)
	}
	packet := diagnosticEvidencePacket{
		Application: appDiagnosticSnapshot{App: "myscheduler", SourceStatus: map[string]string{}},
		RuntimeLogs: &summary,
	}
	encoded := boundedDiagnosticPacketJSON(&packet)
	if len([]rune(encoded)) > diagnosticPacketMaxRunes || !json.Valid([]byte(encoded)) {
		t.Fatalf("packet has %d runes or invalid JSON: %s", len([]rune(encoded)), encoded)
	}
}

func TestOversizedDiagnosticPacketHasHardBoundAndKeepsCoreFacts(t *testing.T) {
	huge := strings.Repeat("oversized-detail-", 2000)
	mismatch := true
	running := false
	services := make([]diagnosticService, 20)
	for index := range services {
		services[index] = diagnosticService{Name: "backend-" + huge, State: "running", Health: "unhealthy", UptimeSeconds: 17, RestartCount: 3}
	}
	activity := make([]diagnosticActivity, 20)
	for index := range activity {
		activity[index] = diagnosticActivity{OccurredAt: "2026-09-08T14:31:00Z", Subject: huge, Message: huge}
	}
	patterns := make([]diagnosticLogPattern, 10)
	for index := range patterns {
		patterns[index] = diagnosticLogPattern{
			Signature: "database timeout " + huge, Level: "error", Occurrences: 84 - index,
			FirstTimestamp: "2026-09-08T14:32:10Z", LastTimestamp: "2026-09-08T14:37:52Z", Representative: huge,
		}
	}
	packet := diagnosticEvidencePacket{
		UserReportedSymptom: huge,
		Application: appDiagnosticSnapshot{
			App: "myscheduler", DeploymentState: "failing", Services: services, AllServicesRunning: &running,
			Health: "unhealthy", HealthSource: huge, DeployedCommit: "abc123", RepositoryCommit: "def456",
			VersionMismatch: &mismatch, RecentActivity: activity, SourceStatus: map[string]string{"oversized": huge},
		},
		RuntimeLogs: &diagnosticLogSummary{
			Kind: "runtime", LinesExamined: 160, ErrorCount: 84, WarningCount: 7,
			FirstErrorTimestamp: "2026-09-08T14:32:10Z", LastErrorTimestamp: "2026-09-08T14:37:52Z",
			HTTPStatusCounts: map[string]int{"500": 84, "503": 2, huge: 99}, Patterns: patterns,
		},
		Repository: []repositoryLocationEvidence{{Path: huge, StartLine: 1, EndLine: 20, Snippet: huge}},
		Investigation: diagnosticInvestigationSummary{
			Rounds: 99, EvidenceExpansions: []string{huge, huge, huge, huge}, BudgetExhausted: true,
		},
		Assessment: diagnosticEvidenceAssessment{
			Status: "unhealthy", ConfidenceCeiling: "strongly_supported",
			Working: []string{huge, huge, huge, huge, huge}, Failing: []string{huge, huge, huge, huge, huge},
			KeyEvidence: []string{huge, huge, huge, huge, huge}, Unknowns: []string{huge, huge, huge, huge, huge},
		},
		Unavailable: []string{huge},
	}

	result := boundedDiagnosticPacketJSON(&packet)
	if len([]rune(result)) > diagnosticPacketMaxRunes {
		t.Fatalf("packet has %d runes", len([]rune(result)))
	}
	if !json.Valid([]byte(result)) {
		t.Fatalf("packet is invalid JSON: %q", result)
	}
	var decoded diagnosticEvidencePacket
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Truncated || decoded.Application.App != "myscheduler" || decoded.Application.DeploymentState != "failing" ||
		decoded.Application.Health != "unhealthy" || decoded.Application.VersionMismatch == nil || !*decoded.Application.VersionMismatch ||
		decoded.Application.DeployedCommit != "abc123" || decoded.Application.RepositoryCommit != "def456" {
		t.Fatalf("important application facts were lost: %+v", decoded.Application)
	}
	if decoded.Assessment.Status != "unhealthy" || decoded.Assessment.ConfidenceCeiling != "strongly_supported" ||
		decoded.UserReportedSymptom == "" || len([]rune(decoded.UserReportedSymptom)) > diagnosticAssessmentItemMaxRunes ||
		decoded.Investigation.Rounds > diagnosticMaxEvidenceExpansionRounds || len(decoded.Investigation.EvidenceExpansions) > diagnosticMaxEvidenceExpansionRounds {
		t.Fatalf("important controller facts were lost or unbounded: assessment=%+v investigation=%+v", decoded.Assessment, decoded.Investigation)
	}
	if len(decoded.Application.Services) == 0 || decoded.Application.Services[0].State != "running" ||
		decoded.RuntimeLogs == nil || decoded.RuntimeLogs.ErrorCount != 84 ||
		decoded.RuntimeLogs.FirstErrorTimestamp != "2026-09-08T14:32:10Z" ||
		decoded.RuntimeLogs.LastErrorTimestamp != "2026-09-08T14:37:52Z" ||
		decoded.RuntimeLogs.HTTPStatusCounts["500"] != 84 || len(decoded.RuntimeLogs.Patterns) == 0 ||
		decoded.RuntimeLogs.Patterns[0].Occurrences != 84 {
		t.Fatalf("important service/log facts were lost: app=%+v logs=%+v", decoded.Application, decoded.RuntimeLogs)
	}
}

func TestDeterministicRecentErrorLookup(t *testing.T) {
	logs := strings.Join([]string{
		"2026-09-08T14:49:00Z INFO ready",
		"2026-09-08T14:55:00Z ERROR database timeout request 1",
		"2026-09-08T14:56:00Z ERROR database timeout request 2",
	}, "\n")
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, logs, "")
	defer cleanup()
	recorder := httptest.NewRecorder()
	capture := newSSECaptureWriter(recorder)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	if !a.handleDeterministicDiagnosticLookup(capture, req, "How many database timeout errors did MyScheduler log?") {
		t.Fatal("expected deterministic log answer")
	}
	body := recorder.Body.String()
	if !strings.Contains(capture.answer.String(), "2 database-timeout error occurrences") || !strings.Contains(body, `"model_invoked":false`) {
		t.Fatalf("answer=%q stream=%s", capture.answer.String(), body)
	}
}

func TestDeterministicRecentErrorsIgnoreSuccessfulAccessTraffic(t *testing.T) {
	logs := strings.Join([]string{
		`172.20.0.1 - - [08/Sep/2026:19:43:55 +0000] "GET / HTTP/1.1" 200 567 "-" "Go-http-client/1.1" "-"`,
		`172.20.0.1 - - [08/Sep/2026:19:44:55 +0000] "GET /health HTTP/1.1" 200 172 "-" "Go-http-client/1.1" "-"`,
	}, "\n")
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, logs, "")
	defer cleanup()
	recorder := httptest.NewRecorder()
	capture := newSSECaptureWriter(recorder)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	if !a.handleDeterministicDiagnosticLookup(capture, req, "Did MyScheduler log any errors recently?") {
		t.Fatal("expected reliable deterministic no-error answer")
	}
	if !strings.Contains(capture.answer.String(), "No error markers were found") ||
		strings.Contains(capture.answer.String(), "has 2 error lines") ||
		!strings.Contains(recorder.Body.String(), `"model_invoked":false`) {
		t.Fatalf("answer=%q stream=%s", capture.answer.String(), recorder.Body.String())
	}
}

func TestDiagnosticOutputBudget(t *testing.T) {
	tests := []struct {
		name, message string
		want          int
	}{
		{name: "simple meaning", message: "What does this error mean?", want: diagnosticOutputSimple},
		{name: "simple interpretation", message: "Should I worry about this status?", want: diagnosticOutputSimple},
		{name: "normal diagnosis", message: "Why is MyScheduler failing?", want: diagnosticOutputNormal},
		{name: "comparison by alternatives", message: "Is this a database issue or backend issue, and why?", want: diagnosticOutputCompare},
		{name: "compare", message: "Compare the deployment state and runtime errors.", want: diagnosticOutputCompare},
		{name: "comparing live query", message: liveComparisonDiagnosticQuery, want: diagnosticOutputCompare},
		{name: "compared", message: "How does the backend look compared with the database?", want: diagnosticOutputCompare},
		{name: "comparison noun", message: "Give a comparison of service health and recent logs.", want: diagnosticOutputCompare},
		{name: "contrast", message: "Contrast current health with deployment errors.", want: diagnosticOutputCompare},
		{name: "contrasting", message: "Diagnose this by contrasting frontend and backend state.", want: diagnosticOutputCompare},
		{name: "versus", message: "Backend versus database: which looks unhealthy?", want: diagnosticOutputCompare},
		{name: "vs", message: "Backend vs. database: which is failing?", want: diagnosticOutputCompare},
		{name: "between and", message: "What differs between the frontend and backend?", want: diagnosticOutputCompare},
		{name: "deep root cause", message: "Analyze everything available and determine the most likely root cause.", want: diagnosticOutputDeep},
		{name: "deep overrides comparison", message: "Compare every signal and determine the most likely root cause.", want: diagnosticOutputDeep},
		{name: "deep correlation", message: "Correlate deployment timing, errors, service state, and repository state.", want: diagnosticOutputDeep},
		{name: "deep multistep", message: "Explain what changed, why it broke, and what I should investigate next.", want: diagnosticOutputDeep},
		{name: "ambiguous default", message: "Please take a closer look.", want: diagnosticOutputNormal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := diagnosticOutputBudget(test.message)
			if got != test.want {
				t.Fatalf("diagnosticOutputBudget(%q)=%d want %d", test.message, got, test.want)
			}
			if got > diagnosticOutputMaximum {
				t.Fatalf("diagnostic output budget %d exceeds maximum %d", got, diagnosticOutputMaximum)
			}
		})
	}
}

func TestCleanHiddenIssueInvestigationIsDeterministic(t *testing.T) {
	logs := `172.20.0.1 - - [08/Sep/2026:19:43:55 +0000] "GET /health HTTP/1.1" 200 42 "-" "client/2.0" "-"`
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, logs, "")
	defer cleanup()
	modelCalls := 0
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		http.Error(w, "unexpected Ollama request", http.StatusInternalServerError)
	}))
	defer ollama.Close()
	a.ollamaURL = ollama.URL

	recorder := httptest.NewRecorder()
	capture := newSSECaptureWriter(recorder)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	policy := modelPolicy{Allowed: true, Model: primaryModel, Mode: "primary"}
	if !a.handleCuratedDiagnosticStream(capture, req, liveComparisonDiagnosticQuery, policy) {
		t.Fatal("expected investigated diagnostic path")
	}
	if modelCalls != 0 {
		t.Fatalf("model_calls=%d want 0", modelCalls)
	}
	answer := strings.ToLower(capture.answer.String())
	if !strings.Contains(answer, "didn't find evidence of a current issue") ||
		!strings.Contains(answer, "confidence:** strongly supported") ||
		!strings.Contains(answer, "bounded recent runtime log lines contain no detected errors") {
		t.Fatalf("answer=%q", capture.answer.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"answer_mode":"deterministic"`) ||
		!strings.Contains(body, `"model_invoked":false`) ||
		!strings.Contains(body, `"diagnostic_confidence":"strongly_supported"`) {
		t.Fatalf("deterministic metadata missing: %s", body)
	}
	if len(capture.evidence) != 2 || capture.evidence[1].ToolName != "read_runtime_logs" {
		t.Fatalf("evidence=%+v", capture.evidence)
	}
}

func TestCausalDiagnosticUsesCuratedToolFreePacket(t *testing.T) {
	var rawLogs strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&rawLogs, "2026-09-08T14:32:%02dZ ERROR database timeout status 500 request %d\n", i%60, i)
	}
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, rawLogs.String(), "")
	defer cleanup()
	var received chatAPIRequest
	events := []string{}
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events = append(events, "model")
		_ = json.NewDecoder(r.Body).Decode(&received)
		_ = json.NewEncoder(w).Encode(chatAPIResponse{
			Message: chatMessage{Content: diagnosticResponseJSON(t, diagnosticFinalResponse{
				Status: "degraded", Confidence: "plausible",
				Conclusion:     "Repeated database timeouts are present, but the root cause is not established.",
				PossibleCauses: []string{"An intermittent database path problem."},
			})},
			Done: true, PromptEvalCount: 300, EvalCount: 24, EvalDuration: int64(time.Second),
		})
	}))
	defer ollama.Close()
	a.ollamaURL = ollama.URL
	runner := &fakePowerHelperRunner{handler: func(_ context.Context, command powerHelperCommand) (powerHelperResult, error) {
		events = append(events, string(command))
		if command == powerHelperEnter {
			return powerHelperEntered, nil
		}
		return powerHelperRestored, nil
	}}
	a.powerLeaseManager = newCPUPowerLeaseManager(runner, nil)

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	policy := modelPolicy{Allowed: true, Model: primaryModel, Mode: "primary"}
	ran, leaseErr := a.with8BPowerLease(req.Context(), policy.Model, func() error {
		if !a.handleCuratedDiagnosticStream(recorder, req, "Why is MyScheduler returning 500?", policy) {
			t.Fatal("expected curated diagnostic path")
		}
		return nil
	})
	if !ran || leaseErr != nil {
		t.Fatalf("curated lease ran=%v err=%v", ran, leaseErr)
	}
	if !reflect.DeepEqual(events, []string{"enter", "model", "restore"}) {
		t.Fatalf("curated lease sequence=%v", events)
	}
	if len(received.Tools) != 0 || len(received.Messages) != 3 {
		t.Fatalf("tools=%d messages=%d request=%+v", len(received.Tools), len(received.Messages), received)
	}
	if int(number(received.Options["num_predict"])) != diagnosticOutputNormal {
		t.Fatalf("num_predict=%v want %d", received.Options["num_predict"], diagnosticOutputNormal)
	}
	if int(number(received.Options["num_thread"])) != 4 || int(number(received.Options["num_batch"])) != 64 {
		t.Fatalf("cool profile options missing: %v", received.Options)
	}
	if number(received.Options["temperature"]) != 0 || received.Format == nil || received.KeepAlive == nil || number(received.KeepAlive) != 0 {
		t.Fatalf("structured deterministic options or keep_alive missing: %+v", received)
	}
	format, ok := received.Format.(map[string]any)
	if !ok || format["type"] != "object" || format["additionalProperties"] != false {
		t.Fatalf("structured format missing: %#v", received.Format)
	}
	prompt := received.Messages[0].Content
	if !strings.Contains(prompt, "Return only one JSON object") ||
		!strings.Contains(prompt, "Do not add fields outside the schema") ||
		!strings.Contains(prompt, "Never exceed the confidence_ceiling") ||
		!strings.Contains(prompt, "Never make a definitive negative deployment-causality claim") ||
		!strings.Contains(prompt, "A healthy current state proves only current health") ||
		!strings.Contains(prompt, "Timing alone is not causation") {
		t.Fatalf("curated structured-output instructions missing: %q", prompt)
	}
	packet := received.Messages[2].Content
	var evidence diagnosticEvidencePacket
	if err := json.Unmarshal([]byte(packet), &evidence); err != nil {
		t.Fatalf("decode packet: %v", err)
	}
	if len([]rune(packet)) > diagnosticPacketMaxRunes || evidence.RuntimeLogs == nil ||
		len(evidence.RuntimeLogs.Patterns) != 1 || evidence.RuntimeLogs.Patterns[0].Occurrences != 100 ||
		evidence.RuntimeLogs.LinesSent != 1 || evidence.Assessment.ConfidenceCeiling != "plausible" {
		t.Fatalf("packet was not compact or assessed: runes=%d evidence=%+v", len([]rune(packet)), evidence)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"answer_mode":"agent"`) || !strings.Contains(body, `"model_invoked":true`) ||
		!strings.Contains(body, `"planner_calls":0`) || !strings.Contains(body, "Repeated database timeouts are present") ||
		!strings.Contains(body, `"diagnostic_confidence":"plausible"`) ||
		!strings.Contains(body, `"inference_profile":"cool"`) || !strings.Contains(body, `"num_thread":4`) ||
		!strings.Contains(body, `"num_batch":64`) || !strings.Contains(body, `"cpu_power_limited":true`) ||
		!strings.Contains(body, `"cpu_power_profile":"3ghz_power"`) {
		t.Fatalf("execution metadata or rendered answer missing: %s", body)
	}
	if strings.Contains(body, `"status":"degraded"`) || strings.Contains(body, `"possible_causes"`) {
		t.Fatalf("raw structured model output leaked: %s", body)
	}
}

func TestParseDiagnosticFinalResponse(t *testing.T) {
	assessment := diagnosticEvidenceAssessment{
		Status: "degraded", ConfidenceCeiling: "plausible",
		Working:  []string{"The backend process is running."},
		Failing:  []string{"Runtime logs contain HTTP 500 responses."},
		RuledOut: []string{"A complete process outage."},
	}
	valid := diagnosticResponseJSON(t, diagnosticFinalResponse{
		Status: "degraded", Confidence: "confirmed", Conclusion: "The request path is failing.",
		Working:        []string{"unsupported model working claim"},
		PossibleCauses: []string{"A route-level failure."},
	})
	parsed, err := parseDiagnosticFinalResponse(valid, assessment, false)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Confidence != "plausible" || len(parsed.Working) != 1 || parsed.Working[0] != assessment.Working[0] ||
		len(parsed.RuledOut) != 1 || parsed.NextCheck != "" {
		t.Fatalf("structured response was not evidence-bounded: %+v", parsed)
	}

	invalid := []string{
		`{"status":"degraded","confidence":"certain","conclusion":"bad"}`,
		`{"status":"degraded","confidence":"unknown","conclusion":""}`,
		`{"status":"degraded","confidence":"unknown","conclusion":"bad","analysis":"hidden"}`,
		`We are given evidence. {"status":"degraded","confidence":"unknown","conclusion":"bad"}`,
	}
	for _, raw := range invalid {
		if response, err := parseDiagnosticFinalResponse(raw, assessment, false); err == nil {
			t.Fatalf("invalid response accepted: %+v from %q", response, raw)
		}
	}
	for _, field := range []string{"reasoning", "thought", "chain_of_thought", "plan", "analysis", "instructions", "prompt", "evidence_packet"} {
		raw := fmt.Sprintf(`{"status":"degraded","confidence":"unknown","conclusion":"safe",%q:"hidden"}`, field)
		if response, err := parseDiagnosticFinalResponse(raw, assessment, false); err == nil {
			t.Fatalf("forbidden field %q accepted: %+v", field, response)
		}
	}
	causal := `{"status":"degraded","confidence":"plausible","conclusion":"The outage was caused by the database."}`
	if response, err := parseDiagnosticFinalResponse(causal, assessment, false); err == nil {
		t.Fatalf("unsupported causal certainty accepted: %+v", response)
	}
	causalArray := `{"status":"degraded","confidence":"plausible","conclusion":"Timing overlaps.","possible_causes":["The latest deployment caused the outage."]}`
	if response, err := parseDiagnosticFinalResponse(causalArray, assessment, false); err == nil {
		t.Fatalf("unsupported causal certainty in response array accepted: %+v", response)
	}
	confirmedAssessment := assessment
	confirmedAssessment.ConfidenceCeiling = "confirmed"
	confirmed := `{"status":"degraded","confidence":"confirmed","conclusion":"The request failed because it was caused by the explicit configuration mismatch."}`
	if _, err := parseDiagnosticFinalResponse(confirmed, confirmedAssessment, false); err != nil {
		t.Fatalf("confirmed causal response rejected: %v", err)
	}
}

func TestParseDiagnosticFinalResponseRejectsUnsupportedNegativeDeploymentCausality(t *testing.T) {
	assessment := diagnosticEvidenceAssessment{
		Status: "healthy", ConfidenceCeiling: "unknown",
		Working: []string{"MyScheduler is currently healthy."},
	}
	cases := []string{
		"The deploy did not cause the issue.",
		"The deployment wasn't responsible for the problem.",
		"This was not caused by the deploy.",
		"The latest deployment caused no problem.",
		"The deploy can be ruled out.",
		"The deployment is not the cause.",
	}
	for _, conclusion := range cases {
		raw := diagnosticResponseJSON(t, diagnosticFinalResponse{Status: "healthy", Confidence: "unknown", Conclusion: conclusion})
		if response, err := parseDiagnosticFinalResponse(raw, assessment, false); err == nil {
			t.Fatalf("unsupported negative deployment causality accepted: %+v from %q", response, conclusion)
		}
	}

	confirmedAssessment := assessment
	confirmedAssessment.ConfidenceCeiling = "confirmed"
	confirmed := diagnosticResponseJSON(t, diagnosticFinalResponse{Status: "healthy", Confidence: "confirmed", Conclusion: "The deployment did not cause the issue."})
	if response, err := parseDiagnosticFinalResponse(confirmed, confirmedAssessment, false); err == nil {
		t.Fatalf("confirmed confidence bypassed negative-causality validation: %+v", response)
	}
}

func TestParseDiagnosticFinalResponseAllowsCurrentHealthAndCausalUncertainty(t *testing.T) {
	assessment := diagnosticEvidenceAssessment{
		Status: "healthy", ConfidenceCeiling: "unknown",
		Working: []string{"MyScheduler is currently healthy."},
	}
	cases := []string{
		"MyScheduler is currently healthy.",
		"The available evidence does not establish that the latest deployment caused the issue.",
		"MyScheduler is currently healthy, but that does not prove the latest deployment never caused an issue.",
	}
	for _, conclusion := range cases {
		raw := diagnosticResponseJSON(t, diagnosticFinalResponse{Status: "healthy", Confidence: "unknown", Conclusion: conclusion})
		if _, err := parseDiagnosticFinalResponse(raw, assessment, false); err != nil {
			t.Fatalf("safe factual or uncertainty statement rejected: %v from %q", err, conclusion)
		}
	}
}

func TestDiagnosticCausalityKeepsTimingScopedToObservedEvidence(t *testing.T) {
	assessment := diagnosticEvidenceAssessment{Status: "degraded", ConfidenceCeiling: "plausible"}
	scoped := diagnosticResponseJSON(t, diagnosticFinalResponse{Status: "degraded", Confidence: "plausible", Conclusion: "The observed error predates the cutover."})
	if _, err := parseDiagnosticFinalResponse(scoped, assessment, false); err != nil {
		t.Fatalf("scoped timestamp statement rejected: %v", err)
	}
	broad := diagnosticResponseJSON(t, diagnosticFinalResponse{Status: "degraded", Confidence: "plausible", Conclusion: "The deployment caused no issues."})
	if response, err := parseDiagnosticFinalResponse(broad, assessment, false); err == nil {
		t.Fatalf("broad negative causality accepted: %+v", response)
	}
	unknownOrdering := diagnosticResponseJSON(t, diagnosticFinalResponse{Status: "degraded", Confidence: "unknown", Conclusion: "Event ordering is unknown, so the deploy can be ruled out."})
	if response, err := parseDiagnosticFinalResponse(unknownOrdering, assessment, false); err == nil {
		t.Fatalf("unknown ordering ruled deployment out: %+v", response)
	}
}

func TestDiagnosticFinalResponseBoundsAndRendering(t *testing.T) {
	long := strings.Repeat("detail-", 100)
	assessment := diagnosticEvidenceAssessment{
		Status: "unhealthy", ConfidenceCeiling: "unknown",
		Working:  []string{"The service process is running."},
		Failing:  []string{"The health endpoint is failing."},
		RuledOut: []string{"A complete process outage."},
		Unknowns: []string{"The root cause is not established."},
	}
	raw := diagnosticResponseJSON(t, diagnosticFinalResponse{
		Status: "unhealthy", Confidence: "unknown", Conclusion: "MiniBase is unhealthy, but the root cause is unknown.",
		PossibleCauses: []string{long, "two", "three", "four", "five"},
		NextCheck:      strings.Repeat("next ", 200),
	})
	parsed, err := parseDiagnosticFinalResponse(raw, assessment, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.PossibleCauses) != diagnosticAssessmentMaxItems ||
		len([]rune(parsed.PossibleCauses[0])) > diagnosticAssessmentItemMaxRunes ||
		len([]rune(parsed.NextCheck)) > diagnosticNextCheckMaxRunes {
		t.Fatalf("response bounds failed: %+v", parsed)
	}
	rendered := renderDiagnosticFinalResponse(parsed)
	for _, expected := range []string{"MiniBase is unhealthy", "**Confidence:** Unknown", "### What is working", "### What is failing", "### What this rules out", "### What remains possible", "### What remains unknown", "### Next useful check"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("rendered response missing %q: %s", expected, rendered)
		}
	}
	clear := renderDiagnosticFinalResponse(diagnosticFinalResponse{Conclusion: "The issue is confirmed.", Confidence: "confirmed"})
	if strings.Contains(clear, "###") {
		t.Fatalf("clear diagnosis rendered empty sections: %s", clear)
	}
}

func TestCuratedDiagnosticInvalidStructuredOutputUsesSafeFallback(t *testing.T) {
	logs := `172.20.0.1 - - [08/Sep/2026:19:43:55 +0000] "GET /api/schedules HTTP/1.1" 500 567 "-" "client/2.0" "-"`
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, logs, "")
	defer cleanup()
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(chatAPIResponse{
			Message: chatMessage{Content: `We are given evidence. {"status":"degraded","confidence":"plausible","conclusion":"hidden"}`},
			Done:    true, PromptEvalCount: 210, EvalCount: 18, EvalDuration: int64(time.Second),
		})
	}))
	defer ollama.Close()
	a.ollamaURL = ollama.URL

	recorder := httptest.NewRecorder()
	capture := newSSECaptureWriter(recorder)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	policy := modelPolicy{Allowed: true, Model: primaryModel, Mode: "primary"}
	if !a.handleCuratedDiagnosticStream(capture, req, "Why is MyScheduler returning 500?", policy) {
		t.Fatal("expected curated diagnostic path")
	}
	if capture.answer.String() != curatedDiagnosticFormatFailureMessage {
		t.Fatalf("answer=%q want safe fallback", capture.answer.String())
	}
	body := recorder.Body.String()
	if strings.Contains(body, "We are given") || strings.Contains(body, `"conclusion":"hidden"`) {
		t.Fatalf("unsafe model output leaked: %s", body)
	}
	if !strings.Contains(body, `"model_invoked":true`) || !strings.Contains(body, `"planner_calls":0`) ||
		!strings.Contains(body, `"prompt_tokens":210`) || !strings.Contains(body, `"generated_tokens":18`) ||
		!strings.Contains(body, `"diagnostic_confidence":"unknown"`) {
		t.Fatalf("curated metrics missing: %s", body)
	}
	if len(capture.evidence) != 2 {
		t.Fatalf("evidence=%+v", capture.evidence)
	}
}

func TestRejectedNegativeDeploymentCausalityUsesUncertaintyFallback(t *testing.T) {
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, "", "")
	defer cleanup()
	unsafe := diagnosticResponseJSON(t, diagnosticFinalResponse{Status: "healthy", Confidence: "unknown", Conclusion: "The latest deploy did not cause an observable issue."})
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(chatAPIResponse{
			Message: chatMessage{Content: unsafe},
			Done:    true, PromptEvalCount: 210, EvalCount: 18, EvalDuration: int64(time.Second),
		})
	}))
	defer ollama.Close()
	a.ollamaURL = ollama.URL

	packet := healthyNegativePremisePacket()
	packet.DeploymentHistory = &deploymentHistoryToolResponse{
		App: "myscheduler",
		Versions: []deploymentHistoryVersion{{
			Relation: "immediately_previous", ArchivedAt: "2026-09-10T12:35:00Z",
		}},
	}
	question := "Was this caused by the latest deploy?"
	packet.Assessment = assessDiagnosticEvidence(packet, question)

	recorder := httptest.NewRecorder()
	capture := newSSECaptureWriter(recorder)
	policy := modelPolicy{Allowed: true, Model: primaryModel, Mode: "primary"}
	err := a.streamCuratedDiagnosticFinalWithLimit(
		context.Background(), capture, capture, policy, []chatMessage{{Role: "user", Content: question}},
		0, 0, time.Now(), diagnosticOutputNormal, packet, question,
	)
	if err != nil {
		t.Fatal(err)
	}
	answer := capture.answer.String()
	if containsUnsupportedNegativeDeploymentCausality(answer) {
		t.Fatalf("unsafe negative causality leaked through fallback: %s", answer)
	}
	if !strings.Contains(answer, "current state is healthy") ||
		!strings.Contains(answer, "does not establish whether") ||
		!strings.Contains(answer, "cannot rule the deployment in or out") {
		t.Fatalf("safe uncertainty fallback missing known or unknown facts: %s", answer)
	}
}

func TestCleanOkayVerificationBypassesModelAndRetainsEvidence(t *testing.T) {
	logs := `172.20.0.1 - - [08/Sep/2026:19:43:55 +0000] "GET / HTTP/1.1" 200 567 "-" "Go-http-client/1.1" "-"`
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, logs, "")
	defer cleanup()
	modelCalls := 0
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		http.Error(w, "unexpected Ollama request", http.StatusInternalServerError)
	}))
	defer ollama.Close()
	a.ollamaURL = ollama.URL

	recorder := httptest.NewRecorder()
	capture := newSSECaptureWriter(recorder)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	policy := modelPolicy{Allowed: true, Model: primaryModel, Mode: "primary"}
	if !a.handleCuratedDiagnosticStream(capture, req, "Is MyScheduler okay?", policy) {
		t.Fatal("expected diagnostic verification path")
	}
	if modelCalls != 0 {
		t.Fatalf("model_calls=%d want 0", modelCalls)
	}
	answer := strings.ToLower(capture.answer.String())
	if !strings.Contains(answer, "didn't find evidence of a current issue") ||
		!strings.Contains(answer, "confidence:** strongly supported") ||
		!strings.Contains(answer, "bounded recent runtime log lines contain no detected errors") {
		t.Fatalf("answer=%q", capture.answer.String())
	}
	if len(capture.evidence) != 2 || capture.evidence[0].ToolName != "get_app_context" ||
		capture.evidence[1].ToolName != "read_runtime_logs" {
		t.Fatalf("evidence=%+v", capture.evidence)
	}
}

func TestHealthyFailurePremiseUsesOneCuratedPass(t *testing.T) {
	logs := `172.20.0.1 - - [08/Sep/2026:19:43:55 +0000] "GET / HTTP/1.1" 200 567 "-" "Go-http-client/1.1" "-"`
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, logs, "")
	defer cleanup()
	modelCalls := 0
	var received chatAPIRequest
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		_ = json.NewDecoder(r.Body).Decode(&received)
		_ = json.NewEncoder(w).Encode(chatAPIResponse{
			Message: chatMessage{Content: diagnosticResponseJSON(t, diagnosticFinalResponse{
				Status: "healthy", Confidence: "confirmed",
				Conclusion:     "I can't confirm a current server-side failure from the available evidence.",
				PossibleCauses: []string{"An intermittent request-specific problem outside the captured log window."},
			})},
			Done: true, EvalCount: 18,
		})
	}))
	defer ollama.Close()
	a.ollamaURL = ollama.URL

	recorder := httptest.NewRecorder()
	capture := newSSECaptureWriter(recorder)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	policy := modelPolicy{Allowed: true, Model: primaryModel, Mode: "primary"}
	if !a.handleCuratedDiagnosticStream(capture, req, "Why is MyScheduler failing?", policy) {
		t.Fatal("expected curated diagnostic path")
	}
	if modelCalls != 1 {
		t.Fatalf("model_calls=%d want 1", modelCalls)
	}
	if received.KeepAlive == nil || number(received.KeepAlive) != 0 {
		t.Fatalf("keep_alive=%v want 0", received.KeepAlive)
	}
	var packet diagnosticEvidencePacket
	if err := json.Unmarshal([]byte(received.Messages[2].Content), &packet); err != nil {
		t.Fatal(err)
	}
	if packet.UserReportedSymptom != "Why is MyScheduler failing?" || packet.Assessment.ConfidenceCeiling != "plausible" ||
		len(packet.Assessment.Failing) != 0 || len(packet.Assessment.PossibleCauses) == 0 ||
		len(packet.Assessment.Working) == 0 || len(packet.Assessment.RuledOut) == 0 || len(packet.Assessment.LessLikely) == 0 ||
		packet.RuntimeLogs == nil || !logSummarySupportsNoCurrentFailure(packet.RuntimeLogs) {
		t.Fatalf("reported premise packet not preserved conservatively: %+v", packet)
	}
	if !strings.Contains(received.Messages[0].Content, "user_reported_symptom") ||
		!strings.Contains(received.Messages[0].Content, "do not manufacture a confirmed failure") {
		t.Fatalf("reported-premise instructions missing: %q", received.Messages[0].Content)
	}
	answer := capture.answer.String()
	if !strings.Contains(answer, "**Confidence:** Plausible") || strings.Contains(answer, "**Confidence:** Confirmed") ||
		!strings.Contains(answer, "### What remains possible") {
		t.Fatalf("answer=%q", answer)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"model_invoked":true`) || !strings.Contains(body, `"planner_calls":0`) {
		t.Fatalf("curated metrics missing: %s", body)
	}
}

func TestDirectDiagnosticCauseBypassesModel(t *testing.T) {
	logs := "2026-09-08T20:00:00Z ERROR configuration error: required backend URL is missing"
	deployment := healthyMySchedulerDeployment()
	deployment["status"] = "unhealthy"
	a, cleanup := newDiagnosticTestApp(t, []any{deployment}, nil, logs, "")
	defer cleanup()
	modelCalls := 0
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		http.Error(w, "unexpected Ollama request", http.StatusInternalServerError)
	}))
	defer ollama.Close()
	a.ollamaURL = ollama.URL

	recorder := httptest.NewRecorder()
	capture := newSSECaptureWriter(recorder)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	policy := modelPolicy{Allowed: true, Model: primaryModel, Mode: "primary"}
	if !a.handleCuratedDiagnosticStream(capture, req, "Why is MyScheduler failing?", policy) {
		t.Fatal("expected diagnostic path")
	}
	if modelCalls != 0 || !strings.Contains(capture.answer.String(), "**Confidence:** Confirmed") ||
		!strings.Contains(strings.ToLower(capture.answer.String()), "configuration error") {
		t.Fatalf("model_calls=%d answer=%q", modelCalls, capture.answer.String())
	}
}

func TestCleanVerificationUnhealthyServiceFallsThrough(t *testing.T) {
	packet := healthyNegativePremisePacket()
	packet.Application.Services[1].State = "stopped"
	if answer, ok := deterministicInvestigatedDiagnosticAnswer(packet, "Is MyScheduler okay?"); ok {
		t.Fatalf("unexpected deterministic answer %q", answer)
	}
}

func TestCleanVerificationHTTP500FallsThrough(t *testing.T) {
	packet := healthyNegativePremisePacket()
	packet.RuntimeLogs.HTTPStatusCounts["500"] = 1
	if answer, ok := deterministicInvestigatedDiagnosticAnswer(packet, "Is MyScheduler okay?"); ok {
		t.Fatalf("unexpected deterministic answer %q", answer)
	}
}

func TestCleanVerificationExplicitErrorMarkersFallThrough(t *testing.T) {
	for _, marker := range []string{"ERROR", "FATAL", "PANIC"} {
		t.Run(marker, func(t *testing.T) {
			packet := healthyNegativePremisePacket()
			summary := summarizeDiagnosticLogs(logToolResponse{
				Kind: "runtime", Logs: "2026-09-08T20:00:00Z " + marker + " request failed",
			}, "", time.Now())
			packet.RuntimeLogs = &summary
			if answer, ok := deterministicInvestigatedDiagnosticAnswer(packet, "Is MyScheduler okay?"); ok {
				t.Fatalf("unexpected deterministic answer %q for summary %+v", answer, summary)
			}
		})
	}
}

func TestCleanVerificationMissingOrAmbiguousHealthFallsThrough(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*diagnosticEvidencePacket)
	}{
		{name: "missing application health", mutate: func(packet *diagnosticEvidencePacket) {
			packet.Application.Health = ""
		}},
		{name: "missing service health", mutate: func(packet *diagnosticEvidencePacket) {
			packet.Application.Services[0].Health = ""
		}},
		{name: "activity source unavailable", mutate: func(packet *diagnosticEvidencePacket) {
			packet.Application.SourceStatus["reactorlab_activity"] = "unavailable"
		}},
		{name: "runtime logs unavailable", mutate: func(packet *diagnosticEvidencePacket) {
			packet.RuntimeLogs = nil
			packet.Unavailable = []string{"runtime logs unavailable"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packet := healthyNegativePremisePacket()
			test.mutate(&packet)
			if answer, ok := deterministicInvestigatedDiagnosticAnswer(packet, "Is MyScheduler okay?"); ok {
				t.Fatalf("unexpected deterministic answer %q", answer)
			}
		})
	}
}

func TestCleanVerificationConflictingEvidenceFallsThrough(t *testing.T) {
	packet := healthyNegativePremisePacket()
	allRunning := false
	packet.Application.AllServicesRunning = &allRunning
	if answer, ok := deterministicInvestigatedDiagnosticAnswer(packet, "Is MyScheduler okay?"); ok {
		t.Fatalf("unexpected deterministic answer %q", answer)
	}
}

func TestDiagnosticLogTimestampOrderingUsesInstant(t *testing.T) {
	logs := strings.Join([]string{
		"2026-09-08T15:00:00+02:00 ERROR earlier",
		"2026-09-08T14:00:00Z ERROR later",
	}, "\n")
	summary := summarizeDiagnosticLogs(logToolResponse{Kind: "runtime", Logs: logs}, "", time.Now())
	if summary.FirstErrorTimestamp != "2026-09-08T15:00:00+02:00" || summary.LastErrorTimestamp != "2026-09-08T14:00:00Z" {
		t.Fatalf("range=%s..%s", summary.FirstErrorTimestamp, summary.LastErrorTimestamp)
	}
}

func TestDiagnosticLogSummaryTracksLatestRestart(t *testing.T) {
	logs := strings.Join([]string{
		"2026-09-08T14:40:00Z WARN service restarted",
		"2026-09-08T14:35:00Z WARN service restarted",
	}, "\n")
	summary := summarizeDiagnosticLogs(logToolResponse{Kind: "runtime", Logs: logs}, "", time.Now())
	answer, ok := deterministicLogAnswer(appDiagnosticSnapshot{App: "miniai"}, summary, "When did MiniAI last restart?")
	if !ok || !strings.Contains(answer, "2026-09-08T14:40:00Z") {
		t.Fatalf("answer=%q ok=%v summary=%+v", answer, ok, summary)
	}
}

func TestDiagnosticLogSummaryPreservesRedaction(t *testing.T) {
	summary := summarizeDiagnosticLogs(logToolResponse{
		Kind: "runtime", Logs: "2026-09-08T14:40:00Z ERROR api_token=super-secret failure",
	}, "", time.Now())
	encoded := diagnosticPacketJSON(summary)
	if !summary.Redacted || strings.Contains(encoded, "super-secret") || !strings.Contains(encoded, "[REDACTED]") {
		t.Fatalf("redaction failed: %s", encoded)
	}
}

func TestDiagnosticVerificationQuestionClassification(t *testing.T) {
	for _, message := range []string{
		"Is MyScheduler okay?",
		"Does MyScheduler have any issues?",
		"Check whether MyScheduler is healthy.",
		"Investigate whether MyScheduler has hidden issues.",
	} {
		if !isDiagnosticVerificationQuestion(message) || !isDiagnosticReasoningQuestion(message) || isExplicitDiagnosticLookup(message) {
			t.Fatalf("verification question routed incorrectly: %q", message)
		}
	}
	for _, message := range []string{
		"Why is MyScheduler failing?",
		"Why is MiniBase broken?",
		"What is causing this failure?",
	} {
		if isDiagnosticVerificationQuestion(message) || !hasCurrentFailurePremise(message) {
			t.Fatalf("failure premise routed incorrectly: %q", message)
		}
	}
}

func TestDiagnosticRoutingPreservesRepositoryAndVersionQuestions(t *testing.T) {
	if isExplicitDiagnosticLookup("Which health endpoint handles MyScheduler status?") ||
		!isRepositoryCodeQuestion("Explain how MyScheduler errors relate to this code") {
		t.Fatal("repository code questions must retain the repository/agent path")
	}
	if !isExplicitDiagnosticLookup("Is MyScheduler's deployed version the same as the repository version?") {
		t.Fatal("explicit deployed/repository version comparison should remain deterministic")
	}
	if !commitValuesMatch("abcdef123456", "abcdef1") || commitValuesMatch("abcdef1", "fedcba2") {
		t.Fatal("commit comparison must support abbreviated hashes without conflating different commits")
	}
}

func TestCausalDiagnosticDoesNotUseDeterministicStatusAnswer(t *testing.T) {
	message := "Why is MyScheduler returning 500?"
	if !isDiagnosticReasoningQuestion(message) || isExplicitDiagnosticLookup(message) {
		t.Fatalf("wrong diagnostic classification for %q", message)
	}
}
