package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

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
			map[string]any{"service": "frontend", "state": "running", "health": "healthy", "uptimeSeconds": 900.0, "restartCount": 0.0},
			map[string]any{"service": "backend", "state": "running", "health": "healthy", "uptimeSeconds": 850.0, "restartCount": 1.0},
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
		{name: "comparison", message: "Is this a database issue or backend issue, and why?", want: diagnosticOutputCompare},
		{name: "explicit comparison", message: "Compare the deployment state and runtime errors.", want: diagnosticOutputCompare},
		{name: "deep root cause", message: "Analyze everything available and determine the most likely root cause.", want: diagnosticOutputDeep},
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

func TestCausalDiagnosticUsesCuratedToolFreePacket(t *testing.T) {
	var rawLogs strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&rawLogs, "2026-09-08T14:32:%02dZ ERROR database timeout status 500 request %d\n", i%60, i)
	}
	a, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, rawLogs.String(), "")
	defer cleanup()
	var mu sync.Mutex
	var received chatAPIRequest
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&received)
		mu.Unlock()
		_, _ = w.Write([]byte("{\"message\":{\"content\":\"The repeated timeouts are relevant evidence.\"},\"done\":true,\"prompt_eval_count\":300}\n"))
	}))
	defer ollama.Close()
	a.ollamaURL = ollama.URL

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	policy := modelPolicy{Allowed: true, Model: primaryModel, Mode: "primary"}
	if !a.handleCuratedDiagnosticStream(recorder, req, "Why is MyScheduler returning 500?", policy) {
		t.Fatal("expected curated diagnostic path")
	}
	mu.Lock()
	got := received
	mu.Unlock()
	if len(got.Tools) != 0 || len(got.Messages) != 3 {
		t.Fatalf("tools=%d messages=%d request=%+v", len(got.Tools), len(got.Messages), got)
	}
	if int(number(got.Options["num_predict"])) != diagnosticOutputNormal {
		t.Fatalf("num_predict=%v want %d", got.Options["num_predict"], diagnosticOutputNormal)
	}
	prompt := got.Messages[0].Content
	if !strings.Contains(prompt, "Challenge unsupported premises") ||
		!strings.Contains(prompt, "Mandatory output contract") ||
		!strings.Contains(prompt, "Never output planning, analysis narration, or meta-commentary") ||
		!strings.Contains(prompt, "Do not mention the evidence packet, prompt, or instructions") {
		t.Fatalf("curated final-answer instructions missing: %q", prompt)
	}
	packet := got.Messages[2].Content
	var evidence diagnosticEvidencePacket
	if err := json.Unmarshal([]byte(packet), &evidence); err != nil {
		t.Fatalf("decode packet: %v", err)
	}
	if len([]rune(packet)) > diagnosticPacketMaxRunes || evidence.RuntimeLogs == nil ||
		len(evidence.RuntimeLogs.Patterns) != 1 || evidence.RuntimeLogs.Patterns[0].Occurrences != 100 ||
		evidence.RuntimeLogs.LinesSent != 1 {
		t.Fatalf("packet was not compact: runes=%d evidence=%+v", len([]rune(packet)), evidence)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"answer_mode":"agent"`) || !strings.Contains(body, `"model_invoked":true`) ||
		!strings.Contains(body, `"planner_calls":0`) {
		t.Fatalf("execution metadata missing: %s", body)
	}
}

func TestHealthyCausalDiagnosticBypassesModelAndRetainsEvidence(t *testing.T) {
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
	if !a.handleCuratedDiagnosticStream(capture, req, "Why is MyScheduler failing?", policy) {
		t.Fatal("expected diagnostic path")
	}
	if modelCalls != 0 {
		t.Fatalf("model_calls=%d want 0", modelCalls)
	}
	answer := strings.ToLower(capture.answer.String())
	if !strings.Contains(answer, "does not currently appear to be failing") ||
		!strings.Contains(answer, "deployment healthy") ||
		!strings.Contains(answer, "database is linked and ready") ||
		!strings.Contains(answer, "no reliable errors or http 5xx") {
		t.Fatalf("answer=%q", capture.answer.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"answer_mode":"deterministic"`) ||
		!strings.Contains(body, `"model_invoked":false`) ||
		!strings.Contains(body, `"planner_calls":0`) {
		t.Fatalf("deterministic metadata missing: %s", body)
	}
	if len(capture.evidence) != 2 || capture.evidence[0].ToolName != "get_app_context" ||
		capture.evidence[1].ToolName != "read_runtime_logs" {
		t.Fatalf("evidence=%+v", capture.evidence)
	}
}

func TestNegativePremiseUnhealthyServiceFallsThrough(t *testing.T) {
	packet := healthyNegativePremisePacket()
	packet.Application.Services[1].State = "stopped"
	if answer, ok := deterministicNegativePremiseAnswer(packet, "Why is MyScheduler failing?"); ok {
		t.Fatalf("unexpected deterministic answer %q", answer)
	}
}

func TestNegativePremiseHTTP500FallsThrough(t *testing.T) {
	packet := healthyNegativePremisePacket()
	packet.RuntimeLogs.HTTPStatusCounts["500"] = 1
	if answer, ok := deterministicNegativePremiseAnswer(packet, "Why is MyScheduler failing?"); ok {
		t.Fatalf("unexpected deterministic answer %q", answer)
	}
}

func TestNegativePremiseExplicitErrorMarkersFallThrough(t *testing.T) {
	for _, marker := range []string{"ERROR", "FATAL", "PANIC"} {
		t.Run(marker, func(t *testing.T) {
			packet := healthyNegativePremisePacket()
			summary := summarizeDiagnosticLogs(logToolResponse{
				Kind: "runtime", Logs: "2026-09-08T20:00:00Z " + marker + " request failed",
			}, "", time.Now())
			packet.RuntimeLogs = &summary
			if answer, ok := deterministicNegativePremiseAnswer(packet, "Why is MyScheduler failing?"); ok {
				t.Fatalf("unexpected deterministic answer %q for summary %+v", answer, summary)
			}
		})
	}
}

func TestNegativePremiseMissingOrAmbiguousHealthFallsThrough(t *testing.T) {
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
			if answer, ok := deterministicNegativePremiseAnswer(packet, "Why is MyScheduler failing?"); ok {
				t.Fatalf("unexpected deterministic answer %q", answer)
			}
		})
	}
}

func TestNegativePremiseConflictingEvidenceFallsThrough(t *testing.T) {
	packet := healthyNegativePremisePacket()
	allRunning := false
	packet.Application.AllServicesRunning = &allRunning
	if answer, ok := deterministicNegativePremiseAnswer(packet, "Why is MyScheduler failing?"); ok {
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
