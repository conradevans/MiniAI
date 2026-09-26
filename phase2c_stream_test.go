package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type unavailablePhase2Executor struct {
	mu     sync.Mutex
	calls  []string
	onCall func(string)
}

func (e *unavailablePhase2Executor) Execute(_ context.Context, name string, _ map[string]any) (any, string, error) {
	e.mu.Lock()
	e.calls = append(e.calls, name)
	hook := e.onCall
	e.mu.Unlock()
	if hook != nil {
		hook(name)
	}
	return nil, "", errors.New("test evidence unavailable")
}

func (e *unavailablePhase2Executor) names() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

type phase2CTestHarness struct {
	app              *app
	executor         *unavailablePhase2Executor
	runner           *fakePowerHelperRunner
	reactor          *httptest.Server
	ollama           *httptest.Server
	mu               sync.Mutex
	chatRequests     []chatAPIRequest
	generateRequests []generateRequest
	reactorPaths     map[string]int
	deploymentPort   int
	responseContent  string
	responseStatus   int
	responseBuilder  func(chatAPIRequest) string
	onReasoner       func(*http.Request)
	onReasonerFinish func()
}

func newPhase2CTestHarness(t *testing.T, availableGiB float64) *phase2CTestHarness {
	t.Helper()
	h := &phase2CTestHarness{
		executor: &unavailablePhase2Executor{}, runner: &fakePowerHelperRunner{},
		reactorPaths: map[string]int{}, deploymentPort: 8080,
	}
	h.reactor = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.reactorPaths[r.URL.Path]++
		port := h.deploymentPort
		h.mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/deployments":
			_ = json.NewEncoder(w).Encode(map[string]any{"deployments": []any{
				map[string]any{
					"app": "myscheduler", "status": "healthy", "port": port,
					"database": map[string]any{"id": "database_123", "displayName": "MyScheduler Production"},
				},
				map[string]any{"app": "golf-mullet", "status": "healthy", "port": 9090},
			}})
		case "/api/v1/system":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"collectedAt": "2026-09-25T18:00:00Z", "services": []any{},
			})
		case "/api/v1/activity":
			_ = json.NewEncoder(w).Encode(map[string]any{"events": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	h.ollama = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/generate":
			var request generateRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			h.mu.Lock()
			h.generateRequests = append(h.generateRequests, request)
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(generateChunk{
				Response: "Direct response.", Done: true, EvalCount: 4, EvalDuration: int64(time.Second),
			})
		case "/api/chat":
			var request chatAPIRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			h.mu.Lock()
			h.chatRequests = append(h.chatRequests, request)
			content, status, builder := h.responseContent, h.responseStatus, h.responseBuilder
			hook, finish := h.onReasoner, h.onReasonerFinish
			h.mu.Unlock()
			if request.Format == nil {
				_ = json.NewEncoder(w).Encode(chatAPIResponse{
					Message: chatMessage{Role: "assistant", Content: "Planner response."}, Done: true,
				})
				return
			}
			if hook != nil {
				hook(r)
			}
			if status != 0 && status/100 != 2 {
				http.Error(w, "synthetic reasoner failure", status)
				if finish != nil {
					finish()
				}
				return
			}
			if builder != nil {
				content = builder(request)
			}
			if content == "" {
				packet, err := evidencePacketFromReasonerRequest(request)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				refs := sortedReasonerReferenceIDs(packet)
				if len(refs) == 0 {
					http.Error(w, "packet has no references", http.StatusBadRequest)
					return
				}
				draft := ReasonerDraft{
					Conclusion:         "The available evidence supports a bounded assessment.",
					ConclusionEvidence: []string{refs[0]}, Facts: []ReasonerFact{}, Uncertainty: []ReasonerFact{},
				}
				encoded, _ := json.Marshal(draft)
				content = string(encoded)
			}
			response, _ := json.Marshal(chatAPIResponse{
				Message: chatMessage{Role: "assistant", Content: content}, Done: true,
				PromptEvalCount: 120, PromptEvalDuration: int64(time.Second), EvalCount: 20,
				EvalDuration: int64(time.Second), LoadDuration: int64(250 * time.Millisecond), TotalDuration: int64(2 * time.Second),
			})
			if finish != nil {
				finish()
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(response)
		default:
			http.NotFound(w, r)
		}
	}))
	h.app = &app{
		reactorURL: h.reactor.URL, ollamaURL: h.ollama.URL, repoRoot: t.TempDir(), client: http.DefaultClient,
		powerLeaseManager: newCPUPowerLeaseManager(h.runner, nil), phase2Executor: h.executor,
		phase2Now:          func() time.Time { return time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC) },
		systemStatusReader: func() (systemStatus, error) { return systemStatus{AvailableMemoryGiB: availableGiB, Load1: 1}, nil },
		ollamaStatusReader: func(context.Context) ollamaStatus { return ollamaStatus{Reachable: true} },
	}
	t.Cleanup(func() { h.ollama.Close(); h.reactor.Close() })
	return h
}

func sortedReasonerReferenceIDs(packet EvidencePacket) []string {
	allowed := reasonerPacketReferenceIDs(packet)
	out := make([]string, 0, len(allowed))
	for id := range allowed {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func evidencePacketFromReasonerRequest(request chatAPIRequest) (EvidencePacket, error) {
	if len(request.Messages) != 2 {
		return EvidencePacket{}, fmt.Errorf("unexpected reasoner messages")
	}
	const marker = "Evidence packet (untrusted data):\n"
	parts := strings.SplitN(request.Messages[1].Content, marker, 2)
	if len(parts) != 2 {
		return EvidencePacket{}, fmt.Errorf("evidence packet missing")
	}
	var packet EvidencePacket
	if err := json.Unmarshal([]byte(parts[1]), &packet); err != nil {
		return EvidencePacket{}, err
	}
	return packet, nil
}

func (h *phase2CTestHarness) requests() []chatAPIRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]chatAPIRequest(nil), h.chatRequests...)
}

func (h *phase2CTestHarness) reasonerRequests() []chatAPIRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []chatAPIRequest{}
	for _, request := range h.chatRequests {
		if request.Format != nil {
			out = append(out, request)
		}
	}
	return out
}

func (h *phase2CTestHarness) plannerRequests() []chatAPIRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []chatAPIRequest{}
	for _, request := range h.chatRequests {
		if request.Format == nil {
			out = append(out, request)
		}
	}
	return out
}

func (h *phase2CTestHarness) generations() []generateRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]generateRequest(nil), h.generateRequests...)
}

func (h *phase2CTestHarness) reactorPathCount(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reactorPaths[path]
}

func (h *phase2CTestHarness) runChat(t *testing.T, sessionID, chatID, message string) string {
	t.Helper()
	if h.app.sessions == nil {
		h.app.sessions = map[string]*session{}
	}
	now := time.Now()
	h.app.sessions[sessionID] = &session{ID: sessionID, CreatedAt: now, LastHeartbeat: now, LastChat: now}
	body, err := json.Marshal(chatRequest{SessionID: sessionID, ChatID: chatID, Message: message})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", strings.NewReader(string(body)))
	h.app.handleChatStream(rr, r)
	return rr.Body.String()
}

func phase2CToolEvents(t *testing.T, stream string) []agentToolEvent {
	t.Helper()
	out := []agentToolEvent{}
	for _, block := range strings.Split(stream, "\n\n") {
		if !strings.HasPrefix(block, "event: tool\n") {
			continue
		}
		index := strings.Index(block, "data: ")
		if index < 0 {
			continue
		}
		var event agentToolEvent
		if err := json.Unmarshal([]byte(block[index+6:]), &event); err != nil {
			t.Fatal(err)
		}
		out = append(out, event)
	}
	return out
}

func TestPhase2CSupportedRoutesUseBoundedEvidenceAndOneReasonerCall(t *testing.T) {
	tests := []struct {
		name, question string
		route          InvestigationRouteID
		capability     string
	}{
		{"platform health", "Is the Dell healthy right now?", RouteCurrentPlatformHealth, "get_platform_overview"},
		{"thermal", "Has the Dell been overheating today?", RouteThermalInvestigation, "read_temperature_history"},
		{"restart", "Why did the Dell restart yesterday?", RouteRestartInvestigation, "read_recovery"},
		{"application current", "Is My Scheduler healthy?", RouteApplicationCurrent, "get_app_context"},
		{"application performance", "Is My Scheduler slow today?", RouteApplicationPerformance, "read_application_history"},
		{"deployment correlation", "Did the My Scheduler deployment cause the outage?", RouteDeploymentCorrelation, "read_deployment_history"},
		{"database backup", "Are the database backups healthy?", RouteDatabaseInvestigation, "list_databases"},
		{"repository interpretation", "Explain where My Scheduler login is implemented in source code.", RouteRepositoryInvestigation, "search_repository"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newPhase2CTestHarness(t, 12)
			rr := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
			if !h.app.handlePhase2CStream(rr, r, test.question, nil) {
				t.Fatal("supported route was not handled")
			}
			requests := h.requests()
			if len(requests) != 1 {
				t.Fatalf("reasoner calls=%d stream=%s", len(requests), rr.Body.String())
			}
			stream := rr.Body.String()
			if !strings.Contains(stream, `"answer_mode":"phase2c"`) || !strings.Contains(stream, `"planner_calls":0`) || !strings.Contains(stream, `"reasoner_calls":1`) || !strings.Contains(stream, `"route":"`+string(test.route)+`"`) {
				t.Fatalf("stream=%s", stream)
			}
			events := phase2CToolEvents(t, stream)
			found, sawResult := false, false
			for _, event := range events {
				if event.Phase == "result" {
					sawResult = true
				}
				if event.Phase == "start" && sawResult {
					t.Fatalf("start followed result: %+v", events)
				}
				if event.Phase == "start" && event.Name == test.capability && event.RequestID != "" {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected capability %s events=%+v", test.capability, events)
			}
			if requests[0].Model != primaryModel || len(requests[0].Tools) != 0 || requests[0].Think || requests[0].Stream {
				t.Fatalf("request=%+v", requests[0])
			}
		})
	}
}

func TestPhase2CExactUnsupportedAndAmbiguousBoundaries(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	if h.app.handlePhase2CStream(httptest.NewRecorder(), r, "What commit is My Scheduler running?", nil) {
		t.Fatal("exact fact entered reasoner")
	}
	if h.app.handlePhase2CStream(httptest.NewRecorder(), r, "Compare My Scheduler versus another application.", nil) {
		t.Fatal("unsupported comparison entered Phase 2C")
	}
	if len(h.requests()) != 0 || len(h.executor.names()) != 0 {
		t.Fatalf("calls tools=%v model=%d", h.executor.names(), len(h.requests()))
	}
	rr := httptest.NewRecorder()
	capture := newSSECaptureWriter(rr)
	if !h.app.handlePhase2CStream(capture, r, "Is the app healthy?", nil) {
		t.Fatal("ambiguous route not handled")
	}
	if !strings.Contains(capture.answer.String(), "Which application") || len(h.requests()) != 0 || len(h.executor.names()) != 0 {
		t.Fatalf("answer=%q stream=%s", capture.answer.String(), rr.Body.String())
	}
}

func TestPhase2CRouterContextUsesBoundedConversationSubjectOnlyForIdentity(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	history := []storedMessage{{Role: "user", Content: "How is MyScheduler doing?"}, {Role: "assistant", Content: "Old state.", Evidence: []evidence{{App: "myscheduler"}}}}
	context := h.app.buildProductionRouterContext(t.Context(), "Is it slow today?", history, h.app.phase2CNow())
	decision := buildShadowInvestigationRoute("Is it slow today?", context)
	if decision.ID != RouteApplicationPerformance || len(decision.Frame.Subjects) != 1 || decision.Frame.Subjects[0].ID != "myscheduler" {
		t.Fatalf("decision=%+v", decision)
	}
	if strings.Contains(canonicalJSON(context), "Old state") {
		t.Fatal("history prose leaked into router context")
	}
}

func TestPhase2CToolRequestIDsAreStable(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	run := func() []string {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
		h.app.handlePhase2CStream(rr, r, "Why did the Dell restart yesterday?", nil)
		ids := []string{}
		for _, event := range phase2CToolEvents(t, rr.Body.String()) {
			if event.Phase == "start" {
				ids = append(ids, event.RequestID)
			}
		}
		return ids
	}
	if first, second := run(), run(); !reflect.DeepEqual(first, second) {
		t.Fatalf("unstable request IDs: %v %v", first, second)
	}
}

type concurrencyDetectWriter struct {
	*httptest.ResponseRecorder
	active     atomic.Int32
	concurrent atomic.Bool
}

func (w *concurrencyDetectWriter) Write(value []byte) (int, error) {
	if !w.active.CompareAndSwap(0, 1) {
		w.concurrent.Store(true)
	}
	defer w.active.Store(0)
	return w.ResponseRecorder.Write(value)
}
func (w *concurrencyDetectWriter) Flush() {}

func TestPhase2CDoesNotWriteResponseConcurrently(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	w := &concurrencyDetectWriter{ResponseRecorder: httptest.NewRecorder()}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	h.app.handlePhase2CStream(w, r, "Why did the Dell restart yesterday?", nil)
	if w.concurrent.Load() {
		t.Fatal("concurrent ResponseWriter use")
	}
}

func TestPhase2CInvalidReasonerOutputDoesNotRetryOrLeak(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	h.responseContent = `<think>private scratch</think>`
	rr := httptest.NewRecorder()
	capture := newSSECaptureWriter(rr)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	h.app.handlePhase2CStream(capture, r, "Is the Dell healthy right now?", nil)
	stream := rr.Body.String()
	if len(h.requests()) != 1 || capture.answer.String() != safeReasonerLimitation() || strings.Contains(stream, "private scratch") || !strings.Contains(stream, `"answer_validation":"invalid"`) {
		t.Fatalf("calls=%d answer=%q stream=%s", len(h.requests()), capture.answer.String(), stream)
	}
}

func TestPhase2CValidationFailureLogsSafeReasonWithoutRawModelContent(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	const privateModelContent = "DO_NOT_LOG_RAW_MODEL_CONTENT"
	h.responseBuilder = func(request chatAPIRequest) string {
		return phase2CTestDraftForRequest(request, "The Dell is healthy.", []string{privateModelContent})
	}

	var logs bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	previousPrefix := log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	})

	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	if !h.app.handlePhase2CStream(rr, r, "Is the Dell healthy right now?", nil) {
		t.Fatal("Phase 2C request was not handled")
	}
	logged := logs.String()
	if !strings.Contains(logged, "Phase 2C reasoner validation rejected: invalid evidence reference") {
		t.Fatalf("safe validation reason was not logged: %q", logged)
	}
	if strings.Contains(logged, privateModelContent) {
		t.Fatalf("validation log leaked raw model content: %q", logged)
	}
	if got := len(h.reasonerRequests()); got != 1 {
		t.Fatalf("invalid output triggered %d reasoner requests", got)
	}
	if phase2CStreamAnswer(t, rr.Body.String()) != safeReasonerLimitation() ||
		!strings.Contains(rr.Body.String(), `"answer_validation":"invalid"`) ||
		!strings.Contains(rr.Body.String(), `"confidence":"Low"`) {
		t.Fatalf("invalid output did not use the safe Low-confidence fallback: %s", rr.Body.String())
	}
}

type phase2COrderWriter struct {
	*httptest.ResponseRecorder
	onWrite func(string)
}

func (w *phase2COrderWriter) Write(value []byte) (int, error) {
	if w.onWrite != nil {
		w.onWrite(string(value))
	}
	return w.ResponseRecorder.Write(value)
}

func (w *phase2COrderWriter) Flush() {}

func phase2CTestDraftForRequest(request chatAPIRequest, conclusion string, references []string) string {
	if len(references) == 0 {
		packet, _ := evidencePacketFromReasonerRequest(request)
		references = sortedReasonerReferenceIDs(packet)
	}
	if len(references) == 0 {
		references = []string{"missing-reference"}
	}
	draft := ReasonerDraft{
		Conclusion: conclusion, ConclusionEvidence: []string{references[0]},
		Facts: []ReasonerFact{}, Uncertainty: []ReasonerFact{},
	}
	encoded, _ := json.Marshal(draft)
	return string(encoded)
}

func TestPhase2CReasonerOutcomesAttemptExactlyOneRequest(t *testing.T) {
	tests := []struct {
		name       string
		question   string
		configure  func(*phase2CTestHarness)
		validation string
	}{
		{name: "valid", question: "Is the Dell healthy right now?", configure: func(*phase2CTestHarness) {}, validation: "valid"},
		{name: "malformed JSON", question: "Is the Dell healthy right now?", configure: func(h *phase2CTestHarness) {
			h.responseContent = "{bad"
		}, validation: "invalid"},
		{name: "schema invalid JSON", question: "Is the Dell healthy right now?", configure: func(h *phase2CTestHarness) {
			h.responseBuilder = func(request chatAPIRequest) string {
				packet, _ := evidencePacketFromReasonerRequest(request)
				refs := sortedReasonerReferenceIDs(packet)
				value := map[string]any{
					"conclusion": "Bounded result.", "conclusion_evidence": []string{refs[0]}, "uncertainty": []any{},
				}
				encoded, _ := json.Marshal(value)
				return string(encoded)
			}
		}, validation: "invalid"},
		{name: "invalid evidence reference", question: "Is the Dell healthy right now?", configure: func(h *phase2CTestHarness) {
			h.responseBuilder = func(request chatAPIRequest) string {
				return phase2CTestDraftForRequest(request, "Bounded result.", []string{"not-in-packet"})
			}
		}, validation: "invalid"},
		{name: "confidence language", question: "Is the Dell healthy right now?", configure: func(h *phase2CTestHarness) {
			h.responseBuilder = func(request chatAPIRequest) string {
				return phase2CTestDraftForRequest(request, "This is almost certainly healthy.", nil)
			}
		}, validation: "invalid"},
		{name: "causal certainty", question: "Did the MyScheduler deployment cause the outage?", configure: func(h *phase2CTestHarness) {
			h.responseBuilder = func(request chatAPIRequest) string {
				return phase2CTestDraftForRequest(request, "The rollout caused the outage.", nil)
			}
		}, validation: "invalid"},
		{name: "planning narration", question: "Is the Dell healthy right now?", configure: func(h *phase2CTestHarness) {
			h.responseBuilder = func(request chatAPIRequest) string {
				return phase2CTestDraftForRequest(request, "Let me analyze the evidence first.", nil)
			}
		}, validation: "invalid"},
		{name: "HTTP 500", question: "Is the Dell healthy right now?", configure: func(h *phase2CTestHarness) {
			h.responseStatus = http.StatusInternalServerError
		}, validation: "transport_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newPhase2CTestHarness(t, 12)
			test.configure(h)
			rr := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
			if !h.app.handlePhase2CStream(rr, r, test.question, nil) {
				t.Fatal("Phase 2C request was not handled")
			}
			if got := len(h.reasonerRequests()); got != 1 {
				t.Fatalf("reasoner attempts=%d stream=%s", got, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "\"answer_validation\":\""+test.validation+"\"") {
				t.Fatalf("validation=%s stream=%s", test.validation, rr.Body.String())
			}
			if test.validation != "valid" && phase2CStreamAnswer(t, rr.Body.String()) != safeReasonerLimitation() {
				t.Fatalf("invalid output was not replaced safely: %s", rr.Body.String())
			}
		})
	}
}

func TestPhase2CPowerLeaseOrderingAndFailureRestoration(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*phase2CTestHarness)
		validation string
	}{
		{"valid", func(*phase2CTestHarness) {}, "\"answer_validation\":\"valid\""},
		{"malformed", func(h *phase2CTestHarness) { h.responseContent = "{bad" }, "\"answer_validation\":\"invalid\""},
		{"http error", func(h *phase2CTestHarness) { h.responseStatus = http.StatusBadGateway }, "\"answer_validation\":\"transport_error\""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newPhase2CTestHarness(t, 12)
			test.configure(h)
			var mu sync.Mutex
			events := []string{}
			add := func(value string) {
				mu.Lock()
				events = append(events, value)
				mu.Unlock()
			}
			h.executor.onCall = func(string) { add("evidence") }
			h.runner.handler = func(_ context.Context, command powerHelperCommand) (powerHelperResult, error) {
				add(string(command))
				if command == powerHelperEnter {
					return powerHelperEntered, nil
				}
				return powerHelperRestored, nil
			}
			h.onReasoner = func(*http.Request) {
				if !h.app.powerLeaseManager.Active() {
					t.Error("reasoner outside lease")
				}
				add("reasoner_start")
			}
			h.onReasonerFinish = func() {
				if !h.app.powerLeaseManager.Active() {
					t.Error("reasoner finished outside lease")
				}
				add("reasoner_finish")
			}
			w := &phase2COrderWriter{ResponseRecorder: httptest.NewRecorder()}
			w.onWrite = func(value string) {
				var event string
				switch {
				case strings.Contains(value, "event: meta"):
					event = "meta"
				case strings.Contains(value, "event: tool"):
					event = "tool"
				case strings.Contains(value, "event: token"):
					event = "answer"
				case strings.Contains(value, "event: done"):
					event = "done"
				}
				if event == "" {
					return
				}
				if h.app.powerLeaseManager.Active() {
					t.Errorf("%s SSE emitted while lease active", event)
				}
				add(event)
			}
			r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
			h.app.handlePhase2CStream(w, r, "Is the Dell healthy right now?", nil)
			if h.app.powerLeaseManager.Active() {
				t.Fatal("lease remained active")
			}
			if got := len(h.reasonerRequests()); got != 1 {
				t.Fatalf("reasoner attempts=%d", got)
			}
			mu.Lock()
			got := append([]string(nil), events...)
			mu.Unlock()
			lastEvidence := lastIndexOf(got, "evidence")
			meta, tool := indexOf(got, "meta"), lastIndexOf(got, "tool")
			enter, reasonerStart := indexOf(got, "enter"), indexOf(got, "reasoner_start")
			reasonerFinish, restore := indexOf(got, "reasoner_finish"), indexOf(got, "restore")
			answer, done := indexOf(got, "answer"), indexOf(got, "done")
			if lastEvidence < 0 || meta <= lastEvidence || tool < meta || enter <= tool ||
				reasonerStart <= enter || reasonerFinish <= reasonerStart || restore <= reasonerFinish ||
				answer <= restore || done <= answer || !strings.Contains(w.Body.String(), test.validation) {
				t.Fatalf("events=%v stream=%s", got, w.Body.String())
			}
		})
	}
}

func TestPhase2CCancellationRestoresLease(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	started := make(chan struct{})
	h.onReasoner = func(request *http.Request) { close(started); <-request.Context().Done() }
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.app.handlePhase2CStream(httptest.NewRecorder(), r, "Is the Dell healthy right now?", nil)
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled reasoner did not return")
	}
	if h.app.powerLeaseManager.Active() {
		t.Fatal("lease remained active after cancellation")
	}
	if got := len(h.reasonerRequests()); got != 1 {
		t.Fatalf("reasoner attempts after cancellation=%d", got)
	}
	if got := h.runner.commands(); !reflect.DeepEqual(got, []powerHelperCommand{powerHelperEnter, powerHelperRestore}) {
		t.Fatalf("commands=%v", got)
	}
}

func TestPhase2CBlockedPolicyNeverAcquiresLease(t *testing.T) {
	h := newPhase2CTestHarness(t, 5)
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	h.app.handlePhase2CStream(rr, r, "Is the Dell healthy right now?", nil)
	if len(h.executor.names()) == 0 || len(h.requests()) != 0 || len(h.runner.commands()) != 0 || rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("tools=%v model=%d lease=%v status=%d", h.executor.names(), len(h.requests()), h.runner.commands(), rr.Code)
	}
}

func TestPhase2CPrimaryAndFallbackUseSameArchitecture(t *testing.T) {
	requests := map[string]chatAPIRequest{}
	for _, test := range []struct {
		name      string
		available float64
		model     string
	}{{"primary", 12, primaryModel}, {"fallback", 8, fallbackModel}} {
		t.Run(test.name, func(t *testing.T) {
			h := newPhase2CTestHarness(t, test.available)
			rr := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
			h.app.handlePhase2CStream(rr, r, "Is the Dell healthy right now?", nil)
			captured := h.requests()
			if len(captured) != 1 || captured[0].Model != test.model {
				t.Fatalf("requests=%+v", captured)
			}
			requests[test.name] = captured[0]
		})
	}
	primary, fallback := requests["primary"], requests["fallback"]
	if !reflect.DeepEqual(primary.Messages, fallback.Messages) || canonicalJSON(primary.Format) != canonicalJSON(fallback.Format) || primary.Think != fallback.Think || primary.Options["num_ctx"] != fallback.Options["num_ctx"] || primary.Options["num_predict"] != fallback.Options["num_predict"] {
		t.Fatalf("architecture differs")
	}
}

func TestPhase2COriginalPlatformHealthQuestionUsesOnePassArchitecture(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	h.app.sessions = map[string]*session{"phase2c-session": {ID: "phase2c-session", CreatedAt: time.Now(), LastHeartbeat: time.Now(), LastChat: time.Now()}}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", strings.NewReader(`{"session_id":"phase2c-session","message":"Check the current platform overview before answering. In one short sentence, tell me whether the Dell is healthy right now."}`))
	h.app.handleChatStream(rr, r)
	if len(h.requests()) != 1 || !strings.Contains(rr.Body.String(), `"answer_mode":"phase2c"`) || !strings.Contains(rr.Body.String(), `"planner_calls":0`) {
		t.Fatalf("requests=%d stream=%s", len(h.requests()), rr.Body.String())
	}
}

func TestPhase2CChatPersistenceStoresRenderedAnswerAndEvidenceOnly(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	store, err := openChatStore(t.TempDir() + "/chat.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h.app.store = store
	chat, err := store.createChat("Phase 2C")
	if err != nil {
		t.Fatal(err)
	}
	h.app.sessions = map[string]*session{"persist-session": {ID: "persist-session", CreatedAt: time.Now(), LastHeartbeat: time.Now(), LastChat: time.Now()}}
	body := fmt.Sprintf(`{"session_id":"persist-session","chat_id":%q,"message":"Is the Dell healthy right now?"}`, chat.ID)
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", strings.NewReader(body))
	h.app.handleChatStream(rr, r)
	detail, err := store.getChat(chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Messages) != 2 || detail.Messages[1].Role != "assistant" || !strings.Contains(detail.Messages[1].Content, "Confidence: Low.") {
		t.Fatalf("messages=%+v stream=%s", detail.Messages, rr.Body.String())
	}
	if strings.Contains(detail.Messages[1].Content, `"conclusion"`) || strings.Contains(detail.Messages[1].Content, `schemaVersion`) {
		t.Fatalf("raw model/packet persisted: %q", detail.Messages[1].Content)
	}
	if len(detail.Messages[1].Evidence) != 1 || detail.Messages[1].Evidence[0].ToolName != "get_platform_overview" {
		t.Fatalf("persisted evidence=%+v", detail.Messages[1].Evidence)
	}
}

func TestPhase2CProductionOrchestrationRoutesSupportedInvestigations(t *testing.T) {
	tests := []struct {
		name     string
		question string
		route    InvestigationRouteID
	}{
		{name: "platform health", question: "Is the Dell healthy right now?", route: RouteCurrentPlatformHealth},
		{name: "platform doing", question: "How is the Dell doing right now?", route: RouteCurrentPlatformHealth},
		{name: "thermal history", question: "Has the Dell been overheating today?", route: RouteThermalInvestigation},
		{name: "restart recovery", question: "Why did the Dell restart yesterday?", route: RouteRestartInvestigation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newPhase2CTestHarness(t, 12)
			stream := h.runChat(t, "production-route", "", test.question)
			if len(h.reasonerRequests()) != 1 || len(h.plannerRequests()) != 0 {
				t.Fatalf("reasoner=%d planner=%d stream=%s", len(h.reasonerRequests()), len(h.plannerRequests()), stream)
			}
			if !strings.Contains(stream, "\"answer_mode\":\"phase2c\"") ||
				!strings.Contains(stream, "\"route\":\""+string(test.route)+"\"") ||
				!strings.Contains(stream, "\"planner_calls\":0") {
				t.Fatalf("stream=%s", stream)
			}
		})
	}
}

func TestPhase2CProductionBareThisContinuity(t *testing.T) {
	tests := []struct {
		name          string
		prior         string
		wantReasoner  int
		wantSubstring string
	}{
		{
			name: "one safe application", prior: "MyScheduler went down after a deploy.",
			wantReasoner: 1, wantSubstring: "\"route\":\"deployment_correlation_investigation\"",
		},
		{
			name: "competing applications", prior: "MyScheduler and Golf Mullet went down after deploys.",
			wantReasoner: 0, wantSubstring: "Which single application",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newPhase2CTestHarness(t, 12)
			store, err := openChatStore(t.TempDir() + "/chat.db")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			h.app.store = store
			chat, err := store.createChat("continuity")
			if err != nil {
				t.Fatal(err)
			}
			h.runChat(t, "continuity-session", chat.ID, test.prior)
			stream := h.runChat(t, "continuity-session", chat.ID, "Did the last deploy cause this?")
			haystack := stream
			if test.wantReasoner == 0 {
				haystack = phase2CStreamAnswer(t, stream)
			}
			if len(h.reasonerRequests()) != test.wantReasoner || len(h.plannerRequests()) != 0 ||
				!strings.Contains(haystack, test.wantSubstring) {
				t.Fatalf("reasoner=%d planner=%d stream=%s", len(h.reasonerRequests()), len(h.plannerRequests()), stream)
			}
		})
	}
}

func TestPhase2CProductionOrdinaryConversationStaysDirect(t *testing.T) {
	for _, question := range []string{
		"Write a poem about MyScheduler.",
		"Tell me a joke about Golf Mullet.",
		"Explain database normalization.",
		"What is a repository?",
	} {
		t.Run(question, func(t *testing.T) {
			h := newPhase2CTestHarness(t, 12)
			stream := h.runChat(t, "ordinary-session", "", question)
			if len(h.reasonerRequests()) != 0 || len(h.plannerRequests()) != 0 || len(h.generations()) != 1 {
				t.Fatalf("reasoner=%d planner=%d direct=%d stream=%s",
					len(h.reasonerRequests()), len(h.plannerRequests()), len(h.generations()), stream)
			}
			if strings.Contains(stream, "\"answer_mode\":\"phase2c\"") {
				t.Fatalf("ordinary conversation entered Phase 2C: %s", stream)
			}
		})
	}
}

func TestPhase2CProductionExactDeterministicAndNoDuplicateProbes(t *testing.T) {
	t.Run("exact deterministic remains zero model", func(t *testing.T) {
		h := newPhase2CTestHarness(t, 12)
		stream := h.runChat(t, "exact-session", "", "What port is MyScheduler listening on?")
		if len(h.requests()) != 0 || len(h.generations()) != 0 ||
			!strings.Contains(phase2CStreamAnswer(t, stream), "port 8080") ||
			!strings.Contains(stream, "\"answer_mode\":\"deterministic\"") {
			t.Fatalf("chat=%d direct=%d stream=%s", len(h.requests()), len(h.generations()), stream)
		}
	})

	t.Run("supported assessment skips legacy collector", func(t *testing.T) {
		h := newPhase2CTestHarness(t, 12)
		stream := h.runChat(t, "assessment-session", "", "Is MyScheduler healthy?")
		if len(h.reasonerRequests()) != 1 || len(h.plannerRequests()) != 0 ||
			h.reactorPathCount("/api/v1/deployments") != 1 {
			t.Fatalf("reasoner=%d planner=%d deployment_reads=%d stream=%s",
				len(h.reasonerRequests()), len(h.plannerRequests()),
				h.reactorPathCount("/api/v1/deployments"), stream)
		}
	})

	t.Run("failed exact probe has one explicit fallback", func(t *testing.T) {
		h := newPhase2CTestHarness(t, 12)
		h.deploymentPort = 0
		stream := h.runChat(t, "failed-exact-session", "", "What port is MyScheduler listening on?")
		if len(h.requests()) != 0 || len(h.generations()) != 0 || len(h.executor.names()) != 0 ||
			h.reactorPathCount("/api/v1/deployments") != 1 {
			t.Fatalf("chat=%d direct=%d tools=%v deployment_reads=%d stream=%s",
				len(h.requests()), len(h.generations()), h.executor.names(),
				h.reactorPathCount("/api/v1/deployments"), stream)
		}
		if !strings.Contains(phase2CStreamAnswer(t, stream), "couldn't resolve that exact fact") {
			t.Fatalf("missing explicit deterministic fallback: %s", stream)
		}
	})
}

func phase2CStreamAnswer(t *testing.T, stream string) string {
	t.Helper()
	var answer strings.Builder
	for _, block := range strings.Split(stream, "\n\n") {
		if !strings.HasPrefix(block, "event: token\n") {
			continue
		}
		index := strings.Index(block, "data: ")
		if index < 0 {
			continue
		}
		var payload map[string]string
		if err := json.Unmarshal([]byte(block[index+6:]), &payload); err != nil {
			t.Fatal(err)
		}
		answer.WriteString(payload["content"])
	}
	return answer.String()
}

func lastIndexOf(values []string, wanted string) int {
	for index := len(values) - 1; index >= 0; index-- {
		if values[index] == wanted {
			return index
		}
	}
	return -1
}

func indexOf(values []string, wanted string) int {
	for index, value := range values {
		if value == wanted {
			return index
		}
	}
	return -1
}
