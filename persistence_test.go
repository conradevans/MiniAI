package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSECaptureWriterCapturesAnswerAndSafeToolMetadata(t *testing.T) {
	rr := httptest.NewRecorder()
	capture := newSSECaptureWriter(rr)

	sendSSE(capture, "tool", map[string]any{
		"phase": "start",
		"name":  "read_repository_file",
		"arguments": map[string]any{
			"app":  "myscheduler",
			"path": "backend/routes/scheduleRoutes.js",
		},
	})
	sendSSE(capture, "tool", map[string]any{
		"phase":   "result",
		"name":    "read_repository_file",
		"summary": "read backend/routes/scheduleRoutes.js (100 bytes)",
	})
	sendSSE(capture, "token", map[string]string{"content": "Hello"})
	sendSSE(capture, "token", map[string]string{"content": " world"})
	sendSSE(capture, "done", map[string]any{"tokens": 2})

	if capture.answer.String() != "Hello world" {
		t.Fatalf("answer=%q", capture.answer.String())
	}
	if !capture.done {
		t.Fatal("done event not captured")
	}
	if len(capture.evidence) != 1 {
		t.Fatalf("evidence=%+v", capture.evidence)
	}
	if capture.evidence[0].ToolName != "read_repository_file" || capture.evidence[0].Path != "backend/routes/scheduleRoutes.js" {
		t.Fatalf("unexpected evidence=%+v", capture.evidence[0])
	}
}

func TestChatContextQueryRetainsPriorAppReference(t *testing.T) {
	history := []storedMessage{
		{Role: "user", Content: "How is MyScheduler doing?"},
		{Role: "assistant", Content: "It is healthy."},
	}
	query := chatContextQuery(history, "What about its database?")
	if !strings.Contains(strings.ToLower(query), "myscheduler") {
		t.Fatalf("prior app reference missing from query: %q", query)
	}
}
