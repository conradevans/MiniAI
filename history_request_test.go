package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func allHistoryEntries(t *testing.T, store *chatStore) []HistoryEntry {
	t.Helper()
	listed, err := store.listHistory(HistoryListFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]HistoryEntry, 0, len(listed.Entries))
	for _, summary := range listed.Entries {
		entry, err := store.getHistoryEntry(summary.ID)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, entry)
	}
	return out
}

func TestDirectConversationCreatesOneLinkedHistoryEntry(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	chat, err := h.app.store.createChat("")
	if err != nil {
		t.Fatal(err)
	}
	stream := h.runChat(t, "history-direct", chat.ID, "Explain database normalization.")
	entries := allHistoryEntries(t, h.app.store)
	if len(entries) != 1 {
		t.Fatalf("history=%+v stream=%s", entries, stream)
	}
	entry := entries[0]
	if entry.Kind != HistoryKindConversation || entry.Status != HistoryStatusSucceeded || entry.ChatID != chat.ID ||
		entry.UserMessageID == 0 || entry.AssistantMessageID == 0 || entry.AnswerMode != "conversation" ||
		!entry.ModelInvoked || entry.PlannerCalls != 0 || entry.ReasonerCalls != 0 || len(entry.Targets) != 0 || len(entry.EvidenceReads) != 0 {
		t.Fatalf("direct history=%+v", entry)
	}
	detail, err := h.app.store.getChat(chat.ID)
	if err != nil || len(detail.Messages) != 2 || detail.Messages[0].HistoryEntryID != entry.ID || detail.Messages[1].HistoryEntryID != entry.ID {
		t.Fatalf("chat detail=%+v err=%v", detail, err)
	}
	if detail.Messages[0].Content != entry.RequestText || detail.Messages[1].Content != entry.ResultText {
		t.Fatalf("chat/history content drift: messages=%+v history=%+v", detail.Messages, entry)
	}

	h.runChat(t, "history-direct", chat.ID, "What is PostgreSQL?")
	entries = allHistoryEntries(t, h.app.store)
	if len(entries) != 2 || entries[0].ID == entries[1].ID || entries[0].ChatID != chat.ID || entries[1].ChatID != chat.ID {
		t.Fatalf("multiple requests did not create distinct linked history: %+v", entries)
	}
}

func TestOneAndTwoRoundInvestigationsCreateOneParentRecord(t *testing.T) {
	t.Run("one round", func(t *testing.T) {
		h := newPhase2CTestHarness(t, 12)
		executor := &platformPipelineExecutor{}
		h.app.phase2Executor = executor
		h.runChat(t, "history-one-round", "", "Is the Dell healthy right now?")
		entries := allHistoryEntries(t, h.app.store)
		if len(entries) != 1 {
			t.Fatalf("history=%+v", entries)
		}
		entry := entries[0]
		if entry.Kind != HistoryKindInvestigation || entry.Status != HistoryStatusSucceeded ||
			entry.ChatID != "" || entry.UserMessageID != 0 || entry.AssistantMessageID != 0 ||
			entry.Route != string(RouteCurrentPlatformHealth) || entry.EvidenceRounds != 1 || entry.SecondRoundReads != 0 ||
			entry.PlannerCalls != 0 || entry.ReasonerCalls != 1 || !entry.ModelInvoked || entry.Model != primaryModel ||
			entry.ToolCalls != 1 || entry.AnswerValidation != "valid" || entry.Confidence != string(ConfidenceHigh) ||
			len(entry.EvidenceReads) != 1 || entry.EvidenceReads[0].EvidenceRound != 1 ||
			entry.EvidenceReads[0].RequestID == "" || entry.EvidenceReads[0].StartedAt != nil || entry.EvidenceReads[0].CompletedAt != nil {
			t.Fatalf("one-round history=%+v", entry)
		}
	})

	t.Run("two rounds", func(t *testing.T) {
		h := newPhase2CTestHarness(t, 12)
		now := h.app.phase2CNow()
		executor := &phase2DTestExecutor{handler: phase2DDatabaseHandler(
			now, []reactorLabDatabase{{ID: "database_123", DisplayName: "Primary", Status: "ready"}}, nil,
		)}
		h.app.phase2Executor = executor
		h.runChat(t, "history-two-round", "", "Are the database backups healthy?")
		entries := allHistoryEntries(t, h.app.store)
		if len(entries) != 1 {
			t.Fatalf("history=%+v", entries)
		}
		entry := entries[0]
		if entry.EvidenceRounds != 2 || entry.SecondRoundReads != 1 || entry.PlannerCalls != 0 || entry.ReasonerCalls != 1 || len(entry.EvidenceReads) != 2 {
			t.Fatalf("two-round history=%+v", entry)
		}
		if entry.EvidenceReads[0].EvidenceRound != 1 || entry.EvidenceReads[1].EvidenceRound != 2 ||
			entry.EvidenceReads[0].RequestID == "" || entry.EvidenceReads[1].RequestID == "" ||
			entry.EvidenceReads[0].StartedAt != nil || entry.EvidenceReads[0].CompletedAt != nil ||
			entry.EvidenceReads[1].StartedAt != nil || entry.EvidenceReads[1].CompletedAt != nil {
			t.Fatalf("round children=%+v", entry.EvidenceReads)
		}
	})
}

func TestZeroModelInvestigationOutcomesEachCreateOneRecord(t *testing.T) {
	tests := []struct {
		name, question, errorCategory string
	}{
		{name: "exact deterministic", question: "What port is MyScheduler listening on?"},
		{name: "clarification", question: "Is the app healthy?"},
		{name: "unsupported local", question: "Compare MyScheduler versus Golf Mullet right now?"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newPhase2CTestHarness(t, 12)
			h.runChat(t, "history-zero-model", "", test.question)
			entries := allHistoryEntries(t, h.app.store)
			if len(entries) != 1 || entries[0].Kind != HistoryKindInvestigation || entries[0].Status != HistoryStatusSucceeded ||
				entries[0].ModelInvoked || entries[0].PlannerCalls != 0 || entries[0].ReasonerCalls != 0 ||
				len(h.reasonerRequests()) != 0 || len(h.generations()) != 0 {
				t.Fatalf("entry=%+v reasoner=%d direct=%d", entries, len(h.reasonerRequests()), len(h.generations()))
			}
		})
	}
}

func TestPostEvidenceInferenceFailuresRetainHistoryReads(t *testing.T) {
	for _, test := range []struct {
		name, category string
		configure      func(*phase2CTestHarness)
	}{
		{name: "policy block", category: "model_policy_blocked", configure: func(h *phase2CTestHarness) {
			h.app.systemStatusReader = func() (systemStatus, error) { return systemStatus{AvailableMemoryGiB: 5, Load1: 1}, nil }
		}},
		{name: "ollama unavailable", category: "ollama_unavailable", configure: func(h *phase2CTestHarness) {
			h.app.ollamaStatusReader = func(context.Context) ollamaStatus { return ollamaStatus{Reachable: false} }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPhase2CTestHarness(t, 12)
			h.app.phase2Executor = &platformPipelineExecutor{}
			test.configure(h)
			h.runChat(t, "history-inference-failure", "", "Is the Dell healthy right now?")
			entries := allHistoryEntries(t, h.app.store)
			if len(entries) != 1 || entries[0].Status != HistoryStatusFailed || entries[0].ErrorCategory != test.category ||
				entries[0].ModelInvoked || entries[0].ReasonerCalls != 0 || entries[0].EvidenceRounds != 1 || len(entries[0].EvidenceReads) != 1 ||
				len(h.requests()) != 0 || len(h.runner.commands()) != 0 {
				t.Fatalf("entry=%+v requests=%d lease=%v", entries, len(h.requests()), h.runner.commands())
			}
		})
	}
}

func TestInvalidReasonerHistoryStoresOnlySafeFallback(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	h.app.phase2Executor = &platformPipelineExecutor{}
	h.responseContent = `{"conclusion":"password=REJECTED_SECRET","conclusion_evidence":["made-up"],"facts":[],"uncertainty":[]}`
	h.runChat(t, "history-invalid-reasoner", "", "Is the Dell healthy right now?")
	entries := allHistoryEntries(t, h.app.store)
	if len(entries) != 1 || entries[0].AnswerValidation != "invalid" || entries[0].ReasonerCalls != 1 ||
		entries[0].Confidence != string(ConfidenceLow) || strings.Contains(canonicalJSON(entries[0]), "REJECTED_SECRET") ||
		!strings.Contains(entries[0].ResultText, "validated answer") {
		t.Fatalf("invalid reasoner history=%+v", entries)
	}
}

func TestCanceledRequestFinalizesHistoryWithoutAssistantResult(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	h.app.phase2Executor = &platformPipelineExecutor{}
	started := make(chan struct{})
	h.onReasoner = func(request *http.Request) {
		close(started)
		<-request.Context().Done()
	}
	sessionID := "history-cancel"
	now := time.Now()
	h.app.sessions = map[string]*session{sessionID: {ID: sessionID, CreatedAt: now, LastHeartbeat: now, LastChat: now}}
	body, _ := json.Marshal(chatRequest{SessionID: sessionID, Message: "Is the Dell healthy right now?"})
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", strings.NewReader(string(body))).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.app.handleChatStream(httptest.NewRecorder(), request)
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled request did not return")
	}
	entries := allHistoryEntries(t, h.app.store)
	if len(entries) != 1 || entries[0].Status != HistoryStatusCanceled || entries[0].ErrorCategory != "client_canceled" ||
		entries[0].ResultText != "" || entries[0].AssistantMessageID != 0 {
		t.Fatalf("canceled history=%+v", entries)
	}
}

func TestInitialHistoryWriteFailurePreventsExecution(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	if _, err := h.app.store.db.Exec(`CREATE TRIGGER fail_history_start BEFORE INSERT ON history_entries BEGIN SELECT RAISE(FAIL, 'synthetic start failure'); END`); err != nil {
		t.Fatal(err)
	}
	stream := h.runChat(t, "history-start-failure", "", "Is the Dell healthy right now?")
	if len(h.executor.names()) != 0 || len(h.requests()) != 0 || len(h.generations()) != 0 || len(h.runner.commands()) != 0 {
		t.Fatalf("work started after history failure: tools=%v chat=%d direct=%d lease=%v stream=%s",
			h.executor.names(), len(h.requests()), len(h.generations()), h.runner.commands(), stream)
	}
	if entries := allHistoryEntries(t, h.app.store); len(entries) != 0 {
		t.Fatalf("failed start left history=%+v", entries)
	}
}

func TestFinalizationFailureDoesNotRetryOrDuplicateExecution(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	chat, err := h.app.store.createChat("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.store.db.Exec(`CREATE TRIGGER fail_history_finalize BEFORE UPDATE ON history_entries BEGIN SELECT RAISE(FAIL, 'synthetic finalize failure'); END`); err != nil {
		t.Fatal(err)
	}
	var serverLog bytes.Buffer
	originalLogWriter := log.Writer()
	log.SetOutput(&serverLog)
	defer log.SetOutput(originalLogWriter)
	stream := h.runChat(t, "history-finalize-failure", chat.ID, "Explain database normalization.")
	entries := allHistoryEntries(t, h.app.store)
	if len(h.generations()) != 1 || len(entries) != 1 || entries[0].Status != HistoryStatusRunning {
		t.Fatalf("direct calls=%d history=%+v", len(h.generations()), entries)
	}
	detail, err := h.app.store.getChat(chat.ID)
	if err != nil || len(detail.Messages) != 1 || detail.Messages[0].Role != "user" {
		t.Fatalf("finalization failure persisted a second assistant response: chat=%+v err=%v", detail, err)
	}
	if strings.Count(stream, "Direct response.") != 1 || strings.Contains(stream, "synthetic finalize failure") ||
		strings.Contains(stream, "UPDATE history_entries") || strings.Contains(strings.ToLower(stream), "sqlite") {
		t.Fatalf("unsafe or duplicate client stream=%s", stream)
	}
	logged := serverLog.String()
	if !strings.Contains(logged, "persistence failure (") || strings.Contains(logged, "synthetic finalize failure") ||
		strings.Contains(logged, "UPDATE history_entries") {
		t.Fatalf("unsafe persistence log=%q", logged)
	}
}

func TestDirectConversationCancellationFinalizesOnceWithoutResult(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	chat, err := h.app.store.createChat("")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	h.onGenerate = func(request *http.Request) {
		close(started)
		<-request.Context().Done()
	}
	sessionID := "history-direct-cancel"
	now := time.Now()
	h.app.sessions = map[string]*session{sessionID: {ID: sessionID, CreatedAt: now, LastHeartbeat: now, LastChat: now}}
	body, _ := json.Marshal(chatRequest{SessionID: sessionID, ChatID: chat.ID, Message: "Explain database normalization."})
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", strings.NewReader(string(body))).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		h.app.handleChatStream(httptest.NewRecorder(), request)
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled direct request did not return")
	}
	entries := allHistoryEntries(t, h.app.store)
	if len(entries) != 1 || entries[0].Status != HistoryStatusCanceled || entries[0].ErrorCategory != "client_canceled" ||
		entries[0].ResultText != "" || entries[0].AssistantMessageID != 0 || len(h.generations()) != 1 {
		t.Fatalf("direct cancellation history=%+v generations=%d", entries, len(h.generations()))
	}
	detail, err := h.app.store.getChat(chat.ID)
	if err != nil || len(detail.Messages) != 1 || detail.Messages[0].Role != "user" {
		t.Fatalf("direct cancellation chat=%+v err=%v", detail, err)
	}
	again, err := h.app.store.getHistoryEntry(entries[0].ID)
	if err != nil || again.Status != HistoryStatusCanceled || again.ResultText != "" {
		t.Fatalf("canceled entry was overwritten: %+v err=%v", again, err)
	}
}

func TestEvidenceReductionFailureRetainsCompletedReadsWithoutRetry(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	now := h.app.phase2CNow()
	executor := &phase2DTestExecutor{handler: phase2DDatabaseHandler(
		now, []reactorLabDatabase{{ID: "database_123", DisplayName: "Primary", Status: "ready"}}, nil,
	)}
	h.app.phase2Executor = executor
	reductions := 0
	h.app.phase2Reducer = func(route InvestigationRoute, question string, requirements []EvidenceRequirement, plan EvidencePlan, results []EvidenceResult, registry capabilityRegistry) (InvestigationEvidence, error) {
		reductions++
		if reductions == 3 {
			return InvestigationEvidence{}, fmt.Errorf("synthetic combined reduction failure")
		}
		return ReduceEvidenceResults(route, question, requirements, plan, results, registry)
	}
	h.runChat(t, "history-reduction-failure", "", "Are the database backups healthy?")
	entries := allHistoryEntries(t, h.app.store)
	if len(entries) != 1 || entries[0].Status != HistoryStatusFailed ||
		entries[0].ErrorCategory != string(FollowUpReductionFailed) || entries[0].EvidenceRounds != 2 ||
		entries[0].SecondRoundReads != 1 || len(entries[0].EvidenceReads) != 2 {
		t.Fatalf("reduction failure history=%+v", entries)
	}
	for _, read := range entries[0].EvidenceReads {
		if read.Status != "completed" || read.StartedAt != nil || read.CompletedAt != nil {
			t.Fatalf("completed evidence was not retained safely: %+v", entries[0].EvidenceReads)
		}
	}
	if calls := executor.recordedCalls(); len(calls) != 2 || reductions != 3 || len(h.reasonerRequests()) != 0 ||
		len(h.plannerRequests()) != 0 || len(h.generations()) != 0 || len(h.runner.commands()) != 0 {
		t.Fatalf("failure retried work: calls=%+v reductions=%d reasoner=%d planner=%d direct=%d lease=%v",
			calls, reductions, len(h.reasonerRequests()), len(h.plannerRequests()), len(h.generations()), h.runner.commands())
	}
}

func TestConcurrentInvestigationsKeepHistoryEvidenceIsolated(t *testing.T) {
	h := newPhase2CTestHarness(t, 5)
	now := h.app.phase2CNow()
	h.app.phase2Executor = &phase2DTestExecutor{handler: phase2DDatabaseHandler(
		now, []reactorLabDatabase{{ID: "database_123", DisplayName: "Primary", Status: "ready"}}, nil,
	)}
	const requests = 6
	h.app.sessions = map[string]*session{}
	chatIDs := map[string]bool{}
	requestTexts := map[string]bool{}
	var wait sync.WaitGroup
	for index := 0; index < requests; index++ {
		sessionID := fmt.Sprintf("history-concurrent-%d", index)
		chat, err := h.app.store.createChat(fmt.Sprintf("Concurrent %d", index))
		if err != nil {
			t.Fatal(err)
		}
		chatIDs[chat.ID] = true
		message := fmt.Sprintf("Are the database backups healthy? request-marker-%d", index)
		requestTexts[message] = true
		stamp := time.Now()
		h.app.mu.Lock()
		h.app.sessions[sessionID] = &session{ID: sessionID, CreatedAt: stamp, LastHeartbeat: stamp, LastChat: stamp}
		h.app.mu.Unlock()
		body, _ := json.Marshal(chatRequest{SessionID: sessionID, ChatID: chat.ID, Message: message})
		wait.Add(1)
		go func() {
			defer wait.Done()
			h.app.handleChatStream(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", strings.NewReader(string(body))))
		}()
	}
	wait.Wait()
	entries := allHistoryEntries(t, h.app.store)
	if len(entries) != requests {
		t.Fatalf("history count=%d want=%d", len(entries), requests)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
		if !chatIDs[entry.ChatID] || !requestTexts[entry.RequestText] || entry.UserMessageID == 0 {
			t.Fatalf("cross-request chat/request contamination: %+v", entry)
		}
		delete(chatIDs, entry.ChatID)
		delete(requestTexts, entry.RequestText)
		if entry.EvidenceRounds != 2 || entry.SecondRoundReads != 1 || len(entry.EvidenceReads) != 2 ||
			entry.EvidenceReads[0].EvidenceRound != 1 || entry.EvidenceReads[1].EvidenceRound != 2 {
			t.Fatalf("cross-request evidence contamination: %+v", entry)
		}
	}
	if len(chatIDs) != 0 || len(requestTexts) != 0 {
		t.Fatalf("missing chat/request associations: chats=%v requests=%v", chatIDs, requestTexts)
	}
	sort.Strings(ids)
	for index := 1; index < len(ids); index++ {
		if ids[index] == ids[index-1] {
			t.Fatalf("duplicate history id %s", ids[index])
		}
	}
}
