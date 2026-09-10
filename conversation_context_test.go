package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func subjectTestDeployments() []map[string]any {
	return []map[string]any{
		{"app": "myscheduler"},
		{"app": "golfmullet"},
		{"app": "portfolio"},
	}
}

func TestConversationSubjectCurrentMessageWins(t *testing.T) {
	history := []storedMessage{
		{
			Role:    "assistant",
			Content: "MyScheduler is healthy.",
			Evidence: []evidence{
				{App: "myscheduler"},
			},
		},
	}

	got := resolveConversationSubjectFromDeployments(
		"What about golfmullet?",
		history,
		subjectTestDeployments(),
	)

	if got.App != "golfmullet" || got.Source != "current_message" || got.Ambiguous {
		t.Fatalf("subject=%+v", got)
	}
}

func TestConversationSubjectUsesLatestAssistantEvidence(t *testing.T) {
	history := []storedMessage{
		{Role: "user", Content: "Why is MyScheduler failing?"},
		{
			Role:    "assistant",
			Content: "The current server evidence is healthy.",
			Evidence: []evidence{
				{App: "myscheduler", ToolName: "get_app_context"},
				{App: "myscheduler", ToolName: "read_runtime_logs"},
			},
		},
	}

	got := resolveConversationSubjectFromDeployments(
		"Check its logs.",
		history,
		subjectTestDeployments(),
	)

	if got.App != "myscheduler" || got.Source != "assistant_evidence" || got.Ambiguous {
		t.Fatalf("subject=%+v", got)
	}
}

func TestConversationSubjectFallsBackToLatestPriorUserReference(t *testing.T) {
	history := []storedMessage{
		{Role: "user", Content: "How is MyScheduler doing?"},
		{Role: "assistant", Content: "It is healthy."},
	}

	got := resolveConversationSubjectFromDeployments(
		"What about its database?",
		history,
		subjectTestDeployments(),
	)

	if got.App != "myscheduler" || got.Source != "prior_user_message" || got.Ambiguous {
		t.Fatalf("subject=%+v", got)
	}
}

func TestConversationSubjectMostRecentEvidenceWins(t *testing.T) {
	history := []storedMessage{
		{
			Role:    "assistant",
			Content: "MyScheduler is healthy.",
			Evidence: []evidence{
				{App: "myscheduler"},
			},
		},
		{Role: "user", Content: "What about golfmullet?"},
		{
			Role:    "assistant",
			Content: "Golf Mullet is healthy.",
			Evidence: []evidence{
				{App: "golfmullet"},
			},
		},
	}

	got := resolveConversationSubjectFromDeployments(
		"Check its logs.",
		history,
		subjectTestDeployments(),
	)

	if got.App != "golfmullet" || got.Source != "assistant_evidence" || got.Ambiguous {
		t.Fatalf("subject=%+v", got)
	}
}

func TestConversationSubjectAmbiguousCurrentMessageDoesNotUseOldSubject(t *testing.T) {
	history := []storedMessage{
		{
			Role:    "assistant",
			Content: "MyScheduler is healthy.",
			Evidence: []evidence{
				{App: "myscheduler"},
			},
		},
	}

	got := resolveConversationSubjectFromDeployments(
		"Compare myscheduler and golfmullet.",
		history,
		subjectTestDeployments(),
	)

	if got.App != "" || !got.Ambiguous || got.Source != "current_message" {
		t.Fatalf("subject=%+v", got)
	}
}

func TestConversationSubjectAmbiguousEvidenceDoesNotGuess(t *testing.T) {
	history := []storedMessage{
		{
			Role:    "assistant",
			Content: "Comparison complete.",
			Evidence: []evidence{
				{App: "myscheduler"},
				{App: "golfmullet"},
			},
		},
	}

	got := resolveConversationSubjectFromDeployments(
		"Check its logs.",
		history,
		subjectTestDeployments(),
	)

	if got.App != "" || !got.Ambiguous || got.Source != "assistant_evidence" {
		t.Fatalf("subject=%+v", got)
	}
}

func TestConversationSubjectIgnoresUnknownEvidenceApp(t *testing.T) {
	history := []storedMessage{
		{Role: "user", Content: "How is MyScheduler doing?"},
		{
			Role:    "assistant",
			Content: "Done.",
			Evidence: []evidence{
				{App: "deleted-app"},
			},
		},
	}

	got := resolveConversationSubjectFromDeployments(
		"Check its logs.",
		history,
		subjectTestDeployments(),
	)

	if got.App != "myscheduler" || got.Source != "prior_user_message" || got.Ambiguous {
		t.Fatalf("subject=%+v", got)
	}
}

func TestConversationSubjectEmptyWhenNoAppCanBeResolved(t *testing.T) {
	got := resolveConversationSubjectFromDeployments(
		"How is everything?",
		nil,
		subjectTestDeployments(),
	)

	if got.App != "" || got.Ambiguous || got.Source != "" {
		t.Fatalf("subject=%+v", got)
	}
}

func newConversationRoutingTestServer(t *testing.T, deploymentStatus *string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/api/v1/deployments":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"deployments": []any{
					map[string]any{
						"app":        "myscheduler",
						"status":     *deploymentStatus,
						"containers": []any{},
					},
					map[string]any{
						"app":        "golfmullet",
						"status":     "healthy",
						"containers": []any{},
					},
				},
			})
		case "/api/v1/databases":
			_ = json.NewEncoder(w).Encode(map[string]any{"databases": []any{}})
		case "/api/v1/activity":
			_ = json.NewEncoder(w).Encode(map[string]any{"events": []any{}})
		case "/api/v1/system":
			_ = json.NewEncoder(w).Encode(map[string]any{"services": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))

	t.Cleanup(server.Close)
	return server
}

func conversationRoutingHistory(appName string) []storedMessage {
	return []storedMessage{
		{
			Role:    "assistant",
			Content: "Current evidence collected.",
			Evidence: []evidence{
				{App: appName, ToolName: "get_app_context"},
			},
		},
	}
}

func TestConversationContextsUsesHistorySubjectAndFreshState(t *testing.T) {
	status := "healthy"
	server := newConversationRoutingTestServer(t, &status)

	a := &app{
		reactorURL: server.URL,
		repoRoot:   t.TempDir(),
		client:     server.Client(),
	}

	history := conversationRoutingHistory("myscheduler")

	contexts, names := a.resolveConversationContexts(
		context.Background(),
		"What about its status?",
		history,
	)
	if len(contexts) != 1 || len(names) != 1 || names[0] != "myscheduler" {
		t.Fatalf("first contexts=%+v names=%v", contexts, names)
	}
	if got, _ := contexts[0].Deployment["status"].(string); got != "healthy" {
		t.Fatalf("first deployment status=%q", got)
	}

	status = "degraded"

	contexts, names = a.resolveConversationContexts(
		context.Background(),
		"What about its status now?",
		history,
	)
	if len(contexts) != 1 || len(names) != 1 || names[0] != "myscheduler" {
		t.Fatalf("second contexts=%+v names=%v", contexts, names)
	}
	if got, _ := contexts[0].Deployment["status"].(string); got != "degraded" {
		t.Fatalf("fresh deployment status=%q want degraded", got)
	}
}

func TestConversationContextsPreservesCurrentMultiAppComparison(t *testing.T) {
	status := "healthy"
	server := newConversationRoutingTestServer(t, &status)

	a := &app{
		reactorURL: server.URL,
		repoRoot:   t.TempDir(),
		client:     server.Client(),
	}

	contexts, names := a.resolveConversationContexts(
		context.Background(),
		"Compare myscheduler and golfmullet.",
		conversationRoutingHistory("myscheduler"),
	)

	if len(contexts) != 2 || len(names) != 2 {
		t.Fatalf("contexts=%d names=%v", len(contexts), names)
	}

	got := map[string]bool{}
	for _, name := range names {
		got[name] = true
	}
	if !got["myscheduler"] || !got["golfmullet"] {
		t.Fatalf("names=%v", names)
	}
}

func TestConversationDiagnosticLookupUsesHistorySubject(t *testing.T) {
	status := "healthy"
	server := newConversationRoutingTestServer(t, &status)

	a := &app{
		reactorURL: server.URL,
		repoRoot:   t.TempDir(),
		client:     server.Client(),
	}

	recorder := httptest.NewRecorder()
	capture := newSSECaptureWriter(recorder)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)

	if !a.handleConversationDiagnosticLookup(
		capture,
		req,
		"What is its status?",
		conversationRoutingHistory("myscheduler"),
	) {
		t.Fatal("expected conversation-aware deterministic diagnostic")
	}

	if !capture.done || !strings.Contains(strings.ToLower(capture.answer.String()), "myscheduler") {
		t.Fatalf("answer=%q done=%v", capture.answer.String(), capture.done)
	}
	if len(capture.evidence) != 1 || capture.evidence[0].App != "myscheduler" {
		t.Fatalf("evidence=%+v", capture.evidence)
	}
}

func TestConversationRepositoryLookupUsesHistorySubject(t *testing.T) {
	_, a, _ := makeIndexedRepo(t)

	status := "healthy"
	server := newConversationRoutingTestServer(t, &status)
	a.reactorURL = server.URL
	a.client = server.Client()

	recorder := httptest.NewRecorder()
	capture := newSSECaptureWriter(recorder)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)

	if !a.handleConversationRepositoryLookup(
		capture,
		req,
		"Which backend route handles schedule templates?",
		conversationRoutingHistory("myscheduler"),
	) {
		t.Fatal("expected conversation-aware deterministic repository lookup")
	}

	if !capture.done || !strings.Contains(capture.answer.String(), "scheduleTemplateRoutes.js") {
		t.Fatalf("answer=%q done=%v", capture.answer.String(), capture.done)
	}
	if len(capture.evidence) == 0 || capture.evidence[0].App != "myscheduler" {
		t.Fatalf("evidence=%+v", capture.evidence)
	}
}

func TestConversationReasoningDiagnosticUsesHistorySubject(t *testing.T) {
	status := "healthy"
	server := newConversationRoutingTestServer(t, &status)

	a := &app{
		reactorURL:    server.URL,
		minideployURL: server.URL,
		repoRoot:      t.TempDir(),
		client:        server.Client(),
	}

	prepared, ok := a.prepareConversationDiagnosticInvestigation(
		context.Background(),
		"Why is it failing?",
		conversationRoutingHistory("myscheduler"),
	)
	if !ok {
		t.Fatal("expected conversation-aware diagnostic investigation")
	}
	if prepared.packet.Application.App != "myscheduler" {
		t.Fatalf("application=%q", prepared.packet.Application.App)
	}
}
