package main

import (
	"context"
	"strings"
	"testing"
)

func TestShouldUseAgentTools(t *testing.T) {
	cases := []struct {
		message string
		want    bool
	}{
		{"Is MyScheduler healthy right now?", false},
		{"Where is the MyScheduler schedule route implemented?", true},
		{"Why is MyScheduler login failing?", true},
		{"Show me MyScheduler runtime logs", true},
	}
	for _, tc := range cases {
		if got := shouldUseAgentTools(tc.message); got != tc.want {
			t.Fatalf("shouldUseAgentTools(%q)=%v want %v", tc.message, got, tc.want)
		}
	}
}

func TestAgentToolsAreReadOnly(t *testing.T) {
	allowed := map[string]bool{
		"list_apps":                 true,
		"get_app_context":           true,
		"list_repository_directory": true,
		"search_repository":         true,
		"read_repository_file":      true,
		"read_runtime_logs":         true,
		"read_deployment_logs":      true,
	}
	defs := agentToolDefinitions()
	if len(defs) != len(allowed) {
		t.Fatalf("got %d tools want %d", len(defs), len(allowed))
	}
	for _, def := range defs {
		if !allowed[def.Function.Name] {
			t.Fatalf("unexpected agent tool %q", def.Function.Name)
		}
		lower := strings.ToLower(def.Function.Name + " " + def.Function.Description)
		for _, forbidden := range []string{"restart", "redeploy", "delete", "write file", "shell"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("tool %q contains forbidden capability term %q", def.Function.Name, forbidden)
			}
		}
	}
}

func TestExecuteAgentSearchUsesSafeRepositoryTool(t *testing.T) {
	_, a := makeToolRepo(t)
	result, summary, err := a.executeAgentTool(context.Background(), "search_repository", map[string]any{
		"app":   "myscheduler",
		"path":  ".",
		"query": "schedule",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.(repoSearchResponse)
	if !ok {
		t.Fatalf("unexpected result type %T", result)
	}
	if len(got.Hits) != 1 || got.Hits[0].Path != "backend/routes/schedule.js" {
		t.Fatalf("unexpected hits: %+v", got.Hits)
	}
	if !strings.Contains(summary, "1 matches") {
		t.Fatalf("unexpected summary %q", summary)
	}
}

func TestEncodeAgentToolResultCapsContext(t *testing.T) {
	input := map[string]any{"content": strings.Repeat("x", agentToolResultMaxRunes+1000)}
	got := encodeAgentToolResult(input)
	if len([]rune(got)) > agentToolResultMaxRunes+100 {
		t.Fatalf("encoded result too large: %d runes", len([]rune(got)))
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("expected truncation marker")
	}
}
