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

func TestRepositoryEvidenceGuardrailPrefersRouteSource(t *testing.T) {
	search := repoSearchResponse{
		App: "myscheduler",
		Hits: []repoSearchHit{
			{Path: "README.md", Line: 1, Text: "schedule"},
			{Path: "backend/db/repository.js", Line: 10, Text: "schedule"},
			{Path: "backend/routes/scheduleRoutes.js", Line: 4, Text: "schedule"},
		},
	}
	got := bestUnreadSourcePaths(search, map[string]bool{}, "schedule", 1)
	if len(got) != 1 || got[0] != "backend/routes/scheduleRoutes.js" {
		t.Fatalf("bestUnreadSourcePaths=%v", got)
	}
}

func TestRepositoryEvidenceGuardrailUsesQueryAffinity(t *testing.T) {
	search := repoSearchResponse{
		App:   "myscheduler",
		Query: "schedule-template",
		Hits: []repoSearchHit{
			{Path: "backend/app.js", Line: 21, Text: `app.use("/api/schedule-templates", scheduleTemplateRoutes)`},
			{Path: "backend/routes/scheduleTemplateRoutes.js", Line: 8, Text: `router.get("/", async (req, res) => {`},
			{Path: "backend/routes/scheduleRoutes.js", Line: 4, Text: `router.get("/schedule", handler)`},
		},
	}
	got := bestUnreadSourcePaths(search, map[string]bool{}, "schedule-template", 2)
	if len(got) != 2 {
		t.Fatalf("bestUnreadSourcePaths=%v", got)
	}
	if got[0] != "backend/routes/scheduleTemplateRoutes.js" {
		t.Fatalf("query-specific route should rank first, got %v", got)
	}
}

func TestCompactAgentSearchResultLimitsHits(t *testing.T) {
	search := repoSearchResponse{App: "myscheduler", Query: "schedule"}
	for i := 0; i < 10; i++ {
		search.Hits = append(search.Hits, repoSearchHit{Path: "README.md", Line: i + 1, Text: strings.Repeat("x", 250)})
	}
	for i := 0; i < 10; i++ {
		search.Hits = append(search.Hits, repoSearchHit{Path: "backend/routes/scheduleRoutes.js", Line: i + 1, Text: strings.Repeat("y", 250)})
	}
	got, ok := compactAgentToolResult("search_repository", search).(repoSearchResponse)
	if !ok {
		t.Fatal("unexpected compact result type")
	}
	if len(got.Hits) > agentSearchMaxHits {
		t.Fatalf("hits=%d", len(got.Hits))
	}
	perFile := map[string]int{}
	for _, hit := range got.Hits {
		perFile[hit.Path]++
		if len([]rune(hit.Text)) > 181 {
			t.Fatalf("snippet too long: %d", len([]rune(hit.Text)))
		}
	}
	for path, n := range perFile {
		if n > agentSearchMaxPerFile {
			t.Fatalf("%s has %d hits", path, n)
		}
	}
}

func TestRequiresRepositoryEvidence(t *testing.T) {
	if !requiresRepositoryEvidence("Which route file defines the endpoint?") {
		t.Fatal("expected repository evidence requirement")
	}
	if requiresRepositoryEvidence("Is MyScheduler healthy?") {
		t.Fatal("unexpected repository evidence requirement")
	}
}
