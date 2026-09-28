package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestHistoryListAndDetailAPIAreBoundedAndFilterable(t *testing.T) {
	store, err := openChatStore(filepath.Join(t.TempDir(), "miniai.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	for index, route := range []InvestigationRouteID{RouteCurrentPlatformHealth, RouteDatabaseInvestigation, RouteApplicationCurrent} {
		entry, _, err := store.startHistoryEntry("", "request "+string(route), now.Add(time.Duration(index)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		target := HistoryTarget{Kind: "platform", CanonicalID: "dell", DisplayName: "Dell"}
		if route == RouteDatabaseInvestigation {
			target = HistoryTarget{Kind: "database", CanonicalID: "database_123", DisplayName: "Primary"}
		}
		if _, err := store.finalizeHistoryEntry(entry.ID, historyFinalizeInput{
			Kind: HistoryKindInvestigation, Status: HistoryStatusSucceeded,
			ResultText: strings.Repeat("result ", 100), CompletedAt: now.Add(time.Duration(index)*time.Second + time.Millisecond),
			Metadata: HistoryExecutionMetadata{AnswerMode: "phase2c", Route: string(route), EvidenceRounds: 1, ToolCalls: 1},
			Targets:  []HistoryTarget{target},
		}); err != nil {
			t.Fatal(err)
		}
	}
	a := &app{store: store}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/history?limit=1&kind=investigation", nil)
	recorder := httptest.NewRecorder()
	a.handleListHistory(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var first struct {
		History    []HistorySummary `json:"history"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.History) != 1 || first.NextCursor == "" || len([]rune(first.History[0].ResultPreview)) > historyPreviewMaxRunes {
		t.Fatalf("first page=%+v", first)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/history?limit=1&cursor="+first.NextCursor, nil)
	recorder = httptest.NewRecorder()
	a.handleListHistory(recorder, request)
	var second struct {
		History []HistorySummary `json:"history"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &second); err != nil || len(second.History) != 1 || second.History[0].ID == first.History[0].ID {
		t.Fatalf("second page=%+v err=%v", second, err)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/v1/history?target_kind=database&target_id=database_123", nil)
	recorder = httptest.NewRecorder()
	a.handleListHistory(recorder, request)
	var filtered struct {
		History []HistorySummary `json:"history"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &filtered); err != nil || len(filtered.History) != 1 || filtered.History[0].Route != string(RouteDatabaseInvestigation) {
		t.Fatalf("filtered=%+v err=%v body=%s", filtered, err, recorder.Body.String())
	}

	detailRequest := httptest.NewRequest(http.MethodGet, "/api/v1/history/"+filtered.History[0].ID, nil)
	detailRequest.SetPathValue("id", filtered.History[0].ID)
	recorder = httptest.NewRecorder()
	a.handleGetHistory(recorder, detailRequest)
	var detail HistoryEntry
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil || detail.ID != filtered.History[0].ID || detail.RequestText == "" {
		t.Fatalf("detail=%+v err=%v body=%s", detail, err, recorder.Body.String())
	}
}

func TestHistoryPaginationWithEqualTimestampsHasNoDuplicatesOrSkips(t *testing.T) {
	store, err := openChatStore(filepath.Join(t.TempDir(), "equal-times.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	startedAt := time.Date(2026, 9, 28, 9, 30, 0, 123, time.UTC)
	want := make([]string, 0, 5)
	for index := 0; index < 5; index++ {
		entry, _, err := store.startHistoryEntry("", "same timestamp", startedAt)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, entry.ID)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(want)))
	a := &app{store: store}
	got := []string{}
	cursor := ""
	for {
		target := "/api/v1/history?limit=2"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		recorder := httptest.NewRecorder()
		a.handleListHistory(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var page struct {
			History    []HistorySummary `json:"history"`
			NextCursor string           `json:"next_cursor"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, entry := range page.History {
			got = append(got, entry.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(got) != len(want) {
		t.Fatalf("pagination count=%d want=%d ids=%v", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("pagination order/continuation got=%v want=%v", got, want)
		}
	}
}

func TestHistoryAPIRejectsInvalidFiltersAndUnknownIDs(t *testing.T) {
	store, err := openChatStore(filepath.Join(t.TempDir(), "miniai.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := &app{store: store}
	for _, target := range []string{
		"/api/v1/history?limit=101",
		"/api/v1/history?kind=anything",
		"/api/v1/history?status=done",
		"/api/v1/history?route=anything",
		"/api/v1/history?sql=DROP+TABLE+chats",
		"/api/v1/history?cursor=not-a-cursor",
	} {
		recorder := httptest.NewRecorder()
		a.handleListHistory(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("target=%s status=%d body=%s", target, recorder.Code, recorder.Body.String())
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/history/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
	request.SetPathValue("id", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	recorder := httptest.NewRecorder()
	a.handleGetHistory(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown history status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHistoryDetailAPIDoesNotExposeSecrets(t *testing.T) {
	store, err := openChatStore(filepath.Join(t.TempDir(), "miniai.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	requestText := strings.Join([]string{
		"Bearer SECRET_TOKEN",
		"password=DB_PASSWORD",
		"CF_API_TOKEN=CLOUDFLARE_SECRET",
		"https://gituser:GIT_SECRET@example.test/repository.git",
		"-----BEGIN RSA PRIVATE KEY-----\nPRIVATE_KEY_MATERIAL\n-----END RSA PRIVATE KEY-----",
	}, "\n")
	entry, _, err := store.startHistoryEntry("", requestText, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.finalizeHistoryEntry(entry.ID, historyFinalizeInput{
		Kind: HistoryKindInvestigation, Status: HistoryStatusSucceeded,
		ResultText:  "<think>hidden reasoning</think> token=RESULT_SECRET",
		CompletedAt: time.Now().UTC(),
		EvidenceReads: []HistoryEvidenceRead{{
			RequestID: "read-1", EvidenceRound: 1, Capability: "read_database_backups",
			Arguments: map[string]any{"query": "api_token=TOOL_SECRET"}, SafeSummary: "Bearer SUMMARY_SECRET",
		}},
	}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/history/"+entry.ID, nil)
	request.SetPathValue("id", entry.ID)
	recorder := httptest.NewRecorder()
	(&app{store: store}).handleGetHistory(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var detail HistoryEntry
	if err := json.Unmarshal(recorder.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if !detail.RequestRedacted || detail.RequestTruncated || !detail.ResultRedacted || detail.ResultTruncated {
		t.Fatalf("History API audit flags=%+v", detail)
	}
	for _, secret := range []string{
		"SECRET_TOKEN", "DB_PASSWORD", "CLOUDFLARE_SECRET", "GIT_SECRET", "PRIVATE_KEY_MATERIAL",
		"RESULT_SECRET", "hidden reasoning", "TOOL_SECRET", "SUMMARY_SECRET",
	} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("history API exposed %q: %s", secret, recorder.Body.String())
		}
	}
}

func TestExistingChatCRUDHandlersRemainCompatible(t *testing.T) {
	store, err := openChatStore(filepath.Join(t.TempDir(), "miniai.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := &app{store: store}

	createRecorder := httptest.NewRecorder()
	a.handleCreateChat(createRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/chats", strings.NewReader(`{"title":"Existing chat"}`)))
	if createRecorder.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createRecorder.Code, createRecorder.Body.String())
	}
	var chat chatRecord
	if err := json.Unmarshal(createRecorder.Body.Bytes(), &chat); err != nil || chat.ID == "" || chat.Title != "Existing chat" {
		t.Fatalf("created chat=%+v err=%v", chat, err)
	}
	if _, err := store.appendUserMessage(chat.ID, "existing question"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.appendAssistantMessage(chat.ID, "existing answer", nil); err != nil {
		t.Fatal(err)
	}

	listRecorder := httptest.NewRecorder()
	a.handleListChats(listRecorder, httptest.NewRequest(http.MethodGet, "/api/v1/chats", nil))
	var listed struct {
		Chats []chatRecord `json:"chats"`
	}
	if listRecorder.Code != http.StatusOK || json.Unmarshal(listRecorder.Body.Bytes(), &listed) != nil || len(listed.Chats) != 1 || listed.Chats[0].ID != chat.ID {
		t.Fatalf("list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}

	getRequest := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chat.ID, nil)
	getRequest.SetPathValue("id", chat.ID)
	getRecorder := httptest.NewRecorder()
	a.handleGetChat(getRecorder, getRequest)
	var detail chatDetail
	if getRecorder.Code != http.StatusOK || json.Unmarshal(getRecorder.Body.Bytes(), &detail) != nil || len(detail.Messages) != 2 ||
		detail.Messages[0].Content != "existing question" || detail.Messages[1].Content != "existing answer" ||
		detail.Messages[0].HistoryEntryID != "" || detail.Messages[1].HistoryEntryID != "" {
		t.Fatalf("get status=%d body=%s", getRecorder.Code, getRecorder.Body.String())
	}

	renameRequest := httptest.NewRequest(http.MethodPatch, "/api/v1/chats/"+chat.ID, strings.NewReader(`{"title":"Renamed chat"}`))
	renameRequest.SetPathValue("id", chat.ID)
	renameRecorder := httptest.NewRecorder()
	a.handleRenameChat(renameRecorder, renameRequest)
	var renamed chatRecord
	if renameRecorder.Code != http.StatusOK || json.Unmarshal(renameRecorder.Body.Bytes(), &renamed) != nil || renamed.Title != "Renamed chat" {
		t.Fatalf("rename status=%d body=%s", renameRecorder.Code, renameRecorder.Body.String())
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/chats/"+chat.ID, nil)
	deleteRequest.SetPathValue("id", chat.ID)
	deleteRecorder := httptest.NewRecorder()
	a.handleDeleteChat(deleteRecorder, deleteRequest)
	if deleteRecorder.Code != http.StatusNoContent {
		t.Fatalf("delete status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	missingRequest := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chat.ID, nil)
	missingRequest.SetPathValue("id", chat.ID)
	missingRecorder := httptest.NewRecorder()
	a.handleGetChat(missingRecorder, missingRequest)
	if missingRecorder.Code != http.StatusNotFound {
		t.Fatalf("deleted chat status=%d body=%s", missingRecorder.Code, missingRecorder.Body.String())
	}
}
