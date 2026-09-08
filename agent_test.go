package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestRepositorySeedQueryUsesDomainTerm(t *testing.T) {
	got := repositorySeedQuery(
		"Which backend route handles schedule templates in MyScheduler?",
		[]string{"myscheduler"},
	)
	if got != "schedule" {
		t.Fatalf("repositorySeedQuery=%q want schedule", got)
	}
}

func TestRepositorySeedQuerySkipsGenericCodeWords(t *testing.T) {
	got := repositorySeedQuery(
		"Where is login auth implemented in MyScheduler?",
		[]string{"myscheduler"},
	)
	if got != "login" {
		t.Fatalf("repositorySeedQuery=%q want login", got)
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

func TestSimpleRepositoryLocationQuestionFastPath(t *testing.T) {
	cases := []struct {
		message string
		want    bool
	}{
		{"Which backend route handles schedule templates in MyScheduler?", true},
		{"Where is the login function defined in MyScheduler?", true},
		{"Why is the schedule route slow in MyScheduler?", false},
		{"Debug the schedule endpoint failure in MyScheduler", false},
		{"Check MyScheduler deployment logs for the route error", false},
		{"Which database query is slow in MyScheduler?", false},
	}
	for _, tc := range cases {
		if got := isSimpleRepositoryLocationQuestion(tc.message); got != tc.want {
			t.Errorf("isSimpleRepositoryLocationQuestion(%q)=%v want %v", tc.message, got, tc.want)
		}
	}
}

func TestRepositoryLocationMessagesAreCompactAndEvidenceOnly(t *testing.T) {
	evidence := []chatMessage{
		{Role: "tool", ToolName: "search_repository", Content: "{\"hits\":[{\"path\":\"backend/routes/schedule.js\",\"line\":8}]}"},
		{Role: "tool", ToolName: "read_repository_file", Content: "{\"path\":\"backend/routes/schedule.js\",\"content\":\"router.get(...)\"}"},
	}
	got := repositoryLocationMessages("Which route handles schedules?", "myscheduler", evidence)
	if len(got) != 4 {
		t.Fatalf("messages=%d want 4", len(got))
	}
	if !strings.Contains(got[0].Content, "read-only evidence") || strings.Contains(got[0].Content, "CURRENT APP CONTEXT") {
		t.Fatalf("unexpected fast-path system prompt: %q", got[0].Content)
	}
	if got[2].ToolName != "search_repository" || got[3].ToolName != "read_repository_file" {
		t.Fatalf("evidence not preserved: %+v", got)
	}
}

func TestDoOllamaRequestEmitsKeepaliveWhileWaiting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	var keepalives atomic.Int32
	resp, err := doOllamaRequest(context.Background(), server.Client(), req, 5*time.Millisecond, func() {
		keepalives.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if keepalives.Load() == 0 {
		t.Fatal("expected at least one keepalive while Ollama request was pending")
	}
}

func TestScanOllamaChatEmitsKeepaliveBetweenChunks(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		defer writer.Close()
		time.Sleep(30 * time.Millisecond)
		_, _ = writer.Write([]byte("{\"message\":{\"content\":\"ok\"},\"done\":true}\n"))
	}()
	var keepalives atomic.Int32
	var content string
	err := scanOllamaChat(context.Background(), reader, 5*time.Millisecond, func() {
		keepalives.Add(1)
	}, func(chunk chatAPIResponse) error {
		content += chunk.Message.Content
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if keepalives.Load() == 0 || content != "ok" {
		t.Fatalf("keepalives=%d content=%q", keepalives.Load(), content)
	}
}
