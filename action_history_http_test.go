package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminHistoryActionsReadEmptyAndAuthenticated(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	history := startAdminActionHistory(t, fixture.store, "inspect an application")

	response := fixture.authenticatedRequest(
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions?history_id="+history.ID,
		"",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", response.Code, response.Body.String())
	}
	linked := decodeAdminHistoryActionsResponse(t, response)
	if linked.HistoryID != history.ID || linked.Actions == nil || len(linked.Actions) != 0 {
		t.Fatalf("linked actions=%+v", linked)
	}

	unauthenticated := performAdminActionHTTPRequest(
		fixture.handler,
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions?history_id="+history.ID,
		"",
		"",
		"",
	)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", unauthenticated.Code, unauthenticated.Body.String())
	}
}

func TestAdminHistoryActionsRestoreOnlyLinkedAuthoritativeState(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	firstHistory := startAdminActionHistory(t, fixture.store, "inspect MyScheduler")
	secondHistory := startAdminActionHistory(t, fixture.store, "inspect another application")
	first, err := fixture.store.createAssistantRestartSuggestion(
		firstHistory.ID,
		ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.store.createAssistantRestartSuggestion(
		secondHistory.ID,
		ActionTarget{Type: ActionTargetApplication, ID: "golf-mullet"},
	)
	if err != nil {
		t.Fatal(err)
	}

	linked := getAdminHistoryActions(t, fixture, firstHistory.ID)
	assertLinkedAssistantRestart(t, linked, firstHistory.ID, first.Proposal.ID(), "myscheduler", ActionStateAwaitingConfirmation)

	confirmedResponse := fixture.authenticatedRequest(
		http.MethodPost,
		testAdminOrigin+"/api/admin/actions/"+string(first.Proposal.ID())+"/confirm",
		"{\"confirm\":true}",
	)
	if confirmedResponse.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", confirmedResponse.Code, confirmedResponse.Body.String())
	}
	if len(fixture.restarter.calls()) != 0 {
		t.Fatalf("confirmation executed restart: %v", fixture.restarter.calls())
	}
	linked = getAdminHistoryActions(t, fixture, firstHistory.ID)
	assertLinkedAssistantRestart(t, linked, firstHistory.ID, first.Proposal.ID(), "myscheduler", ActionStateAuthorized)

	executedResponse := fixture.authenticatedRequest(
		http.MethodPost,
		testAdminOrigin+"/api/admin/actions/"+string(first.Proposal.ID())+"/execute",
		"",
	)
	if executedResponse.Code != http.StatusOK {
		t.Fatalf("execute status=%d body=%s", executedResponse.Code, executedResponse.Body.String())
	}
	linked = getAdminHistoryActions(t, fixture, firstHistory.ID)
	assertLinkedAssistantRestart(t, linked, firstHistory.ID, first.Proposal.ID(), "myscheduler", ActionStateSucceeded)
	if calls := fixture.restarter.calls(); len(calls) != 1 || calls[0] != "myscheduler" {
		t.Fatalf("restart calls=%v", calls)
	}

	other := getAdminHistoryActions(t, fixture, secondHistory.ID)
	assertLinkedAssistantRestart(t, other, secondHistory.ID, second.Proposal.ID(), "golf-mullet", ActionStateAwaitingConfirmation)
}

func TestAdminHistoryActionsRejectInvalidQueryShapes(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	tests := []struct {
		name  string
		query string
	}{
		{name: "missing", query: ""},
		{name: "empty", query: "?history_id="},
		{name: "duplicate", query: "?history_id=one&history_id=two"},
		{name: "unexpected only", query: "?other=value"},
		{name: "unexpected additional", query: "?history_id=one&other=value"},
		{name: "invalid slash", query: "?history_id=bad%2Fid"},
		{name: "invalid whitespace", query: "?history_id=%20history"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := fixture.authenticatedRequest(
				http.MethodGet,
				testAdminOrigin+"/api/admin/actions"+test.query,
				"",
			)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}

	unknown := fixture.authenticatedRequest(
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions?history_id=unknown_history",
		"",
	)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown History status=%d body=%s", unknown.Code, unknown.Body.String())
	}
}

func TestAdminHistoryActionsFailClosedForInvalidPersistedAction(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	history := startAdminActionHistory(t, fixture.store, "inspect MyScheduler")
	created, err := fixture.store.createAssistantRestartSuggestion(
		history.ID,
		ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
	)
	if err != nil {
		t.Fatal(err)
	}
	execActionTestSQL(
		t,
		fixture.store,
		"UPDATE action_proposals SET policy_reason = ? WHERE action_id = ?",
		ActionPolicyDirectUserNoConfirmation,
		created.Proposal.ID(),
	)

	response := fixture.authenticatedRequest(
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions?history_id="+history.ID,
		"",
	)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("invalid persisted action status=%d body=%s", response.Code, response.Body.String())
	}
}

func startAdminActionHistory(t *testing.T, store *chatStore, request string) HistoryEntry {
	t.Helper()
	entry, _, err := store.startHistoryEntry("", request, actionTestTime())
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func getAdminHistoryActions(
	t *testing.T,
	fixture adminActionHTTPFixture,
	historyID string,
) adminHistoryActionsResponse {
	t.Helper()
	response := fixture.authenticatedRequest(
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions?history_id="+historyID,
		"",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeAdminHistoryActionsResponse(t, response)
}

func decodeAdminHistoryActionsResponse(
	t *testing.T,
	response *httptest.ResponseRecorder,
) adminHistoryActionsResponse {
	t.Helper()
	var linked adminHistoryActionsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &linked); err != nil {
		t.Fatalf("decode linked actions: %v", err)
	}
	return linked
}

func assertLinkedAssistantRestart(
	t *testing.T,
	linked adminHistoryActionsResponse,
	historyID string,
	actionID ActionID,
	targetID string,
	state ActionState,
) {
	t.Helper()
	if linked.HistoryID != historyID || len(linked.Actions) != 1 {
		t.Fatalf("linked actions=%+v", linked)
	}
	action := linked.Actions[0]
	if action.ActionID != actionID ||
		action.Kind != ActionKindRestartApplication ||
		action.Origin != ActionOriginAssistantSuggested ||
		action.SafetyClass != ActionSafetyRecoverable ||
		action.Target != (adminActionTargetResponse{Type: ActionTargetApplication, ID: targetID}) ||
		action.State != state ||
		!action.ConfirmationRequired ||
		action.WarningRequired ||
		!action.SupportsExecution ||
		action.HistoryEntryID != historyID {

		t.Fatalf("linked action=%+v", action)
	}
}
