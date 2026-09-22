package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
		{"How was Dell memory during the last hour?", true},
		{"What happened around 3 PM?", true},
		{"Is automatic recovery armed?", true},
		{"Thanks, that helps.", false},
		{"Thanks for sharing the program overview.", false},
	}
	for _, tc := range cases {
		if got := shouldUseAgentTools(tc.message); got != tc.want {
			t.Fatalf("shouldUseAgentTools(%q)=%v want %v", tc.message, got, tc.want)
		}
	}
}

func TestExactDeploymentIdentityRequiresTypedEvidence(t *testing.T) {
	for _, message := range []string{
		"What commit is MyScheduler deployed on?",
		"Which deployment SHA is active?",
		"What branch was this release deployed from?",
		"Is the deployed app on the same version as the checkout?",
	} {
		if !requiresTypedDeploymentIdentity(message) {
			t.Fatalf("message did not require typed identity: %q", message)
		}
	}
	if requiresTypedDeploymentIdentity("Is MyScheduler healthy?") {
		t.Fatal("ordinary health question unexpectedly required exact identity")
	}
	if requiresTypedDeploymentIdentity("Please share deployment status.") {
		t.Fatal("sha trigger matched inside an ordinary word")
	}
	if requiresRepositoryEvidence(
		"What exact source commit is MyScheduler deployed from?",
	) {
		t.Fatal("exact deployed source question requested repository code evidence")
	}
}

func TestAgentPromptDescribesTypedEvidenceAndUnknownProvenance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/api/v1/deployments" {
			t.Fatalf("path=%q", request.URL.Path)
		}
		_, _ = response.Write([]byte(`{"deployments":[]}`))
	}))
	defer server.Close()
	a := &app{reactorURL: server.URL, client: server.Client()}
	prompt, _ := a.agentSystemPrompt(
		context.Background(),
		"What was deployed?",
		nil,
	)
	for _, required := range []string{
		"current and historical ReactorLab state",
		"source.commitSha is authoritative",
		"local repository commit is separate evidence",
		"exact deployed source is unknown",
		"archivedAt",
		"activatedAt",
		"Correlation is not causation",
		"read-only",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt lacks %q: %s", required, prompt)
		}
	}
	if strings.Contains(
		strings.ToLower(prompt),
		"does not contain an exact deployed git commit",
	) {
		t.Fatal("prompt retains obsolete deployment-history wording")
	}
}

type agentStreamCapture struct {
	answer string
	done   map[string]any
}

func decodeAgentStream(t *testing.T, body string) agentStreamCapture {
	t.Helper()
	var capture agentStreamCapture
	event := ""
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			payload := []byte(strings.TrimPrefix(line, "data: "))
			switch event {
			case "token":
				var token map[string]string
				if err := json.Unmarshal(payload, &token); err != nil {
					t.Fatalf("decode token event: %v", err)
				}
				capture.answer += token["content"]
			case "done":
				if err := json.Unmarshal(payload, &capture.done); err != nil {
					t.Fatalf("decode done event: %v", err)
				}
			}
			event = ""
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE stream: %v", err)
	}
	return capture
}

func newAgentHandoffReactorServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		if request.Method != http.MethodGet {
			t.Errorf("ReactorLab method=%s want GET", request.Method)
			http.Error(response, "unexpected method", http.StatusMethodNotAllowed)
			return
		}
		switch request.URL.Path {
		case "/api/v1/deployments":
			_, _ = response.Write([]byte(`{"deployments":[]}`))
		case "/internal/miniai/v1/overview":
			_ = json.NewEncoder(response).Encode(reactorLabOverview{
				CollectedAt: time.Date(
					2026, 9, 22, 18, 0, 0, 0, time.UTC,
				),
				System: reactorLabSection[reactorLabSystemSnapshot]{
					Available: false, Error: "system_metrics_unavailable",
				},
				Recovery: reactorLabSection[reactorLabRecovery]{
					Available: false, Error: "recovery_unavailable",
				},
				Deployments: reactorLabSection[reactorLabDeploymentList]{
					Available: false, Error: "deployment_source_unavailable",
				},
				Databases: reactorLabSection[reactorLabDatabaseList]{
					Available: false, Error: "database_source_unavailable",
				},
				Observability: reactorLabSection[reactorLabObservabilityState]{
					Available: false, Error: "observability_unavailable",
				},
			})
		default:
			t.Errorf("unexpected ReactorLab path %q", request.URL.Path)
			http.NotFound(response, request)
		}
	}))
}

func TestAgentNoToolPlannerContentRemainsDirectAnswer(t *testing.T) {
	reactor := newAgentHandoffReactorServer(t)
	defer reactor.Close()
	var requests []chatAPIRequest
	ollama := httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		var input chatAPIRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Errorf("decode Ollama request: %v", err)
			http.Error(response, "decode failed", http.StatusBadRequest)
			return
		}
		requests = append(requests, input)
		_ = json.NewEncoder(response).Encode(chatAPIResponse{
			Message: chatMessage{Content: "Direct planner answer."},
			Done:    true,
		})
	}))
	defer ollama.Close()

	a := &app{
		ollamaURL: ollama.URL, reactorURL: reactor.URL,
		client: ollama.Client(),
	}
	recorder := httptest.NewRecorder()
	a.handleAgentChatStream(
		recorder,
		httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil),
		chatRequest{Message: "Give me a direct answer."},
		modelPolicy{Model: fallbackModel, Mode: "fallback"},
		nil,
	)

	capture := decodeAgentStream(t, recorder.Body.String())
	if capture.answer != "Direct planner answer." {
		t.Fatalf("answer=%q", capture.answer)
	}
	if len(requests) != 1 || requests[0].Stream ||
		int(number(requests[0].Options["num_predict"])) != 512 {

		t.Fatalf("Ollama requests=%+v", requests)
	}
	if int(number(capture.done["tool_calls"])) != 0 ||
		int(number(capture.done["planner_calls"])) != 1 {

		t.Fatalf("done=%v", capture.done)
	}
}

type agentToolHandoffResult struct {
	stream   agentStreamCapture
	requests []chatAPIRequest
}

func runAgentToolHandoff(
	t *testing.T,
	terminalPlannerContent string,
) agentToolHandoffResult {
	t.Helper()
	reactor := newAgentHandoffReactorServer(t)
	defer reactor.Close()
	var (
		mu       sync.Mutex
		requests []chatAPIRequest
	)
	ollama := httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		var input chatAPIRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Errorf("decode Ollama request: %v", err)
			http.Error(response, "decode failed", http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, input)
		requestNumber := len(requests)
		mu.Unlock()

		switch requestNumber {
		case 1:
			_ = json.NewEncoder(response).Encode(chatAPIResponse{
				Message: chatMessage{ToolCalls: []toolCall{{
					Type: "function",
					Function: toolFunction{
						Name:      "get_platform_overview",
						Arguments: map[string]any{},
					},
				}}},
				Done: true,
			})
		case 2:
			_ = json.NewEncoder(response).Encode(chatAPIResponse{
				Message: chatMessage{Content: terminalPlannerContent},
				Done:    true,
			})
		case 3:
			_ = json.NewEncoder(response).Encode(chatAPIResponse{
				Message: chatMessage{Content: "Final answer from evidence."},
				Done:    true,
			})
		default:
			t.Errorf("unexpected Ollama request %d", requestNumber)
			http.Error(response, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer ollama.Close()

	a := &app{
		ollamaURL: ollama.URL, reactorURL: reactor.URL,
		client: ollama.Client(),
	}
	recorder := httptest.NewRecorder()
	a.handleAgentChatStream(
		recorder,
		httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil),
		chatRequest{Message: "How is the Dell doing?"},
		modelPolicy{Model: fallbackModel, Mode: "fallback"},
		nil,
	)

	mu.Lock()
	requestCopy := append([]chatAPIRequest(nil), requests...)
	mu.Unlock()
	return agentToolHandoffResult{
		stream:   decodeAgentStream(t, recorder.Body.String()),
		requests: requestCopy,
	}
}

func TestAgentToolThenNoMoreToolsUsesDedicatedFinalAnswer(t *testing.T) {
	result := runAgentToolHandoff(
		t,
		"Planner synthesis that must remain internal.",
	)
	if result.stream.answer != "Final answer from evidence." {
		t.Fatalf("answer=%q", result.stream.answer)
	}
	if len(result.requests) != 3 ||
		result.requests[0].Stream || result.requests[1].Stream ||
		!result.requests[2].Stream {

		t.Fatalf("requests=%+v", result.requests)
	}
	if int(number(result.stream.done["tool_calls"])) != 1 ||
		int(number(result.stream.done["planner_calls"])) != 2 {

		t.Fatalf("done=%v", result.stream.done)
	}
	finalMessages := result.requests[2].Messages
	toolEvidenceFound := false
	finalInstructionFound := false
	for _, message := range finalMessages {
		if message.ToolName == "get_platform_overview" {
			toolEvidenceFound = true
		}
		if message.Role == "system" &&
			strings.Contains(message.Content, "Tool gathering is complete") {

			finalInstructionFound = true
		}
		if strings.Contains(message.Content, "Planner synthesis") {
			t.Fatalf("terminal planner content polluted final prompt: %+v", finalMessages)
		}
	}
	if !toolEvidenceFound || !finalInstructionFound {
		t.Fatalf("final messages missing evidence/instruction: %+v", finalMessages)
	}
}

func TestAgentPlannerReasoningIsNeverUserFacingAfterToolUse(t *testing.T) {
	plannerReasoning := "Okay, let me process the user question and tool response... </think>"
	result := runAgentToolHandoff(t, plannerReasoning)
	if strings.Contains(result.stream.answer, "Okay, let me process") ||
		strings.Contains(result.stream.answer, "</think>") ||
		strings.Contains(result.stream.answer, plannerReasoning) {

		t.Fatalf("planner reasoning leaked into answer: %q", result.stream.answer)
	}
	if result.stream.answer != "Final answer from evidence." {
		t.Fatalf("answer=%q", result.stream.answer)
	}
	for _, message := range result.requests[2].Messages {
		if strings.Contains(message.Content, "Okay, let me process") ||
			strings.Contains(message.Content, "</think>") {

			t.Fatalf("planner reasoning leaked into final prompt: %+v", result.requests[2].Messages)
		}
	}
}

func TestAgentToolsAreReadOnly(t *testing.T) {
	allowed := map[string]bool{
		"get_platform_overview":      true,
		"list_apps":                  true,
		"get_app_context":            true,
		"read_host_history":          true,
		"read_temperature_history":   true,
		"read_application_history":   true,
		"read_service_history":       true,
		"read_infrastructure_events": true,
		"list_databases":             true,
		"read_database_backups":      true,
		"read_activity":              true,
		"read_recovery":              true,
		"list_repository_directory":  true,
		"search_repository":          true,
		"read_repository_file":       true,
		"read_runtime_logs":          true,
		"read_deployment_logs":       true,
		"read_deployment_history":    true,
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
		for _, forbidden := range []string{"redeploy", "delete", "write file", "shell command"} {
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

func TestReactorLabMetricCompactionKeepsValidExactNewestPoints(t *testing.T) {
	points := make([]reactorLabHostPoint, reactorLabMaxMetricPoints)
	for index := range points {
		points[index] = reactorLabHostPoint{
			Timestamp:   reactorLabTestNow.Add(time.Duration(index) * time.Second),
			SampleCount: 1,
			CPUAverage:  pointerTo(float64(index) + 0.125),
		}
	}
	history := reactorLabHostHistory{
		Window:      reactorLabTestWindow(),
		Points:      points,
		TotalPoints: len(points),
	}
	compacted := compactAgentToolResult(
		"read_host_history",
		history,
	).(reactorLabHostHistory)
	if !compacted.Truncated || compacted.TotalPoints != len(points) ||
		len(compacted.Points) != 6 ||
		*compacted.Points[len(compacted.Points)-1].CPUAverage !=
			float64(reactorLabMaxMetricPoints-1)+0.125 {

		t.Fatalf("compacted=%+v", compacted)
	}
	encoded := encodeAgentToolResult(compacted)
	if !json.Valid([]byte(encoded)) ||
		len([]rune(encoded)) > agentToolResultMaxRunes {

		t.Fatalf("encoded compact result invalid or oversized: %s", encoded)
	}
}

func TestReactorLabAppCompactionPreservesPreexistingTotal(t *testing.T) {
	apps := make([]reactorLabAppSummary, reactorLabMaxToolApps)
	for index := range apps {
		apps[index].App = fmt.Sprintf("app-%03d", index)
	}
	compacted := compactAgentToolResult(
		"list_apps",
		reactorLabAppListResult{
			Apps: apps, TotalApps: 500, Truncated: true,
		},
	).(reactorLabAppListResult)
	if len(compacted.Apps) != 8 || compacted.TotalApps != 500 ||
		!compacted.Truncated {

		t.Fatalf("compacted=%+v", compacted)
	}
}

func TestAgentDeploymentHistoryCompactionStaysValidBoundedJSON(t *testing.T) {
	long := strings.Repeat("history-value-", 100)
	services := make([]deploymentHistoryService, deploymentHistoryMaxServices)
	for index := range services {
		services[index] = deploymentHistoryService{
			Name: long, Container: long, Image: long, Strategy: long,
		}
	}
	history := deploymentHistoryToolResponse{
		App: "myscheduler", TotalVersions: 12, Truncated: true, Redacted: true,
		ExactCommitAvailable: false,
		TimestampMeaning:     "archived_at is an archive/cutover time, not original deployment time",
		Source:               "MiniDeploy private deployment-history API",
		Versions: []deploymentHistoryVersion{{
			Position: 0, Relation: "immediately_previous", Container: long,
			Image: long, Strategy: long, ArchivedAt: "2026-09-10T12:34:56Z",
			Services: services, Truncated: true,
		}},
	}
	encoded := encodeAgentToolResult(compactAgentToolResult("read_deployment_history", history))
	if !json.Valid([]byte(encoded)) || len([]rune(encoded)) > agentToolResultMaxRunes {
		t.Fatalf("compacted history is invalid or oversized: runes=%d body=%s", len([]rune(encoded)), encoded)
	}
	for _, expected := range []string{
		`"source":"MiniDeploy deployment history"`,
		`"archived_at":"2026-09-10T12:34:56Z"`,
		`"services_truncated":true`,
		`"identifiers_truncated":true`,
		`"timestamp_meaning":"archive/cutover time, not original deployment time"`,
	} {
		if !strings.Contains(encoded, expected) {
			t.Errorf("compacted history lost %s: %s", expected, encoded)
		}
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
		{"Where is FooBar defined in MyScheduler?", true},
		{"Why is the schedule template route failing in MyScheduler?", false},
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

func TestMiniAIOllamaRequestsUseImmediateUnload(t *testing.T) {
	requests := []chatAPIRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatAPIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requests = append(requests, request)
		_ = json.NewEncoder(w).Encode(chatAPIResponse{Message: chatMessage{Content: "ok"}, Done: true})
	}))
	defer server.Close()
	ultraCool, _ := parse8BInferenceProfile(inferenceProfileUltraCool)
	a := &app{ollamaURL: server.URL, client: server.Client(), eightBProfile: ultraCool}
	if _, err := a.callAgentPlanner(context.Background(), primaryModel, []chatMessage{{Role: "user", Content: "test"}}, nil); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	if err := a.streamAgentFinalWithLimit(context.Background(), recorder, recorder, modelPolicy{Model: primaryModel, Mode: "primary"},
		[]chatMessage{{Role: "user", Content: "test"}}, 0, 0, time.Now(), 16); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests=%d want planner and final", len(requests))
	}
	for index, request := range requests {
		if request.KeepAlive == nil || number(request.KeepAlive) != 0 {
			t.Fatalf("request %d keep_alive=%v want 0", index, request.KeepAlive)
		}
		if int(number(request.Options["num_thread"])) != 3 || int(number(request.Options["num_batch"])) != 32 {
			t.Fatalf("request %d ultra_cool profile options=%v", index, request.Options)
		}
	}

	encoded, err := json.Marshal(generateRequest{Model: primaryModel, Stream: true, KeepAlive: modelKeepAlive})
	if err != nil {
		t.Fatal(err)
	}
	var generated map[string]any
	if err := json.Unmarshal(encoded, &generated); err != nil {
		t.Fatal(err)
	}
	if value, ok := generated["keep_alive"]; !ok || number(value) != 0 {
		t.Fatalf("generate keep_alive missing or nonzero: %s", encoded)
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

func TestRepositoryLocationEvidenceIsBoundedAroundMatch(t *testing.T) {
	lines := make([]string, 220)
	for i := range lines {
		lines[i] = "ordinary source line " + strings.Repeat("x", 120)
	}
	lines[109] = `router.get("/", scheduleTemplateHandler)`
	file := repoFileResponse{
		App: "myscheduler", Path: "backend/routes/scheduleTemplateRoutes.js",
		Content: strings.Join(lines, "\n"), Redacted: true,
	}
	search := repoSearchResponse{Hits: []repoSearchHit{{
		Path: file.Path, Line: 110, Text: lines[109],
	}}}
	evidence, ok := compactRepositoryLocationEvidence(search, file)
	if !ok {
		t.Fatal("expected compact evidence")
	}
	if evidence.StartLine != 102 || evidence.EndLine > 122 {
		t.Fatalf("unexpected evidence window %d-%d", evidence.StartLine, evidence.EndLine)
	}
	if !strings.Contains(evidence.Snippet, `110: router.get("/", scheduleTemplateHandler)`) {
		t.Fatalf("match missing from snippet: %q", evidence.Snippet)
	}
	if len([]rune(evidence.Snippet)) > agentEvidenceMaxRunesPerFile {
		t.Fatalf("snippet has %d runes", len([]rune(evidence.Snippet)))
	}
	if len(evidence.Snippet) >= len(file.Content)/2 {
		t.Fatalf("snippet was not substantially smaller: snippet=%d file=%d", len(evidence.Snippet), len(file.Content))
	}
	if !evidence.Redacted {
		t.Fatal("redaction metadata was not preserved")
	}
}

func TestRepositoryLocationSourcePathsPreferOneSufficientRouteFile(t *testing.T) {
	search := repoSearchResponse{Hits: []repoSearchHit{
		{Path: "backend/routes/scheduleTemplateRoutes.js", Line: 10, Text: `router.get("/", handler)`},
		{Path: "backend/services/scheduleTemplates.js", Line: 20, Text: "schedule template"},
	}}
	got := repositoryLocationSourcePaths(search, map[string]bool{}, "schedule")
	if len(got) != 1 || got[0] != "backend/routes/scheduleTemplateRoutes.js" {
		t.Fatalf("paths=%v", got)
	}
}

func TestRepositoryLocationSourcePathsIncludeRouteMountContext(t *testing.T) {
	search := repoSearchResponse{Hits: []repoSearchHit{
		{Path: "backend/routes/scheduleTemplateRoutes.js", Line: 10, Text: `router.get("/", handler)`},
		{Path: "backend/app.js", Line: 6, Text: `app.use("/api/schedule-templates", scheduleTemplateRoutes)`},
	}}
	got := repositoryLocationSourcePaths(search, map[string]bool{}, "schedule")
	if len(got) != 2 {
		t.Fatalf("paths=%v want route and mount files", got)
	}
	if got[0] != "backend/routes/scheduleTemplateRoutes.js" || got[1] != "backend/app.js" {
		t.Fatalf("paths=%v", got)
	}
}

func TestRepositoryLocationPromptStaysSmall(t *testing.T) {
	evidence := repositoryLocationEvidence{
		Path: "backend/routes/scheduleTemplateRoutes.js", StartLine: 10, EndLine: 20,
		Snippet: strings.Repeat("router.get('/', handler)\n", 11),
	}
	messages := repositoryLocationMessages(
		"Which backend route handles schedule templates in MyScheduler?",
		"myscheduler",
		[]chatMessage{{Role: "tool", ToolName: "read_repository_file", Content: encodeAgentToolResult(evidence)}},
	)
	if runes := repositoryMessageRunes(messages); runes >= 4000 {
		t.Fatalf("fast-path prompt too large: %d runes", runes)
	}
}
