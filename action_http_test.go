package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const (
	adminRestartRequest  = `{"kind":"restart_application","target":{"type":"application","id":"myscheduler"},"arguments":{}}`
	adminRollbackRequest = `{"kind":"rollback_deployment","target":{"type":"application","id":"myscheduler"},"arguments":{}}`
	adminDeployRequest   = `{"kind":"deploy_repository","target":{"type":"repository","id":"ReactorLab/MyScheduler"},"arguments":{"provider":"github","containerPort":8080,"healthPath":"/healthz"}}`
)

type adminActionHTTPFixture struct {
	store   *chatStore
	handler http.Handler
	token   string
}

func TestAdminActionCreateUsesAuthoritativeDirectUserPolicy(t *testing.T) {
	tests := []struct {
		name                 string
		body                 string
		kind                 ActionKind
		safety               ActionSafetyClass
		state                ActionState
		confirmationRequired bool
		warningRequired      bool
		eventType            ActionEventType
	}{
		{
			name: "restart application", body: adminRestartRequest,
			kind: ActionKindRestartApplication, safety: ActionSafetyRecoverable,
			state: ActionStateAuthorized, eventType: ActionEventPolicyAuthorizedOnCreation,
		},
		{
			name: "rollback deployment", body: adminRollbackRequest,
			kind: ActionKindRollbackDeployment, safety: ActionSafetyIrreversible,
			state: ActionStateAwaitingConfirmation, confirmationRequired: true,
			warningRequired: true, eventType: ActionEventProposalCreated,
		},
		{
			name: "deploy repository", body: adminDeployRequest,
			kind: ActionKindDeployRepository, safety: ActionSafetyCompensatable,
			state: ActionStateAwaitingConfirmation, confirmationRequired: true,
			eventType: ActionEventProposalCreated,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAdminActionHTTPFixture(t)
			response := fixture.authenticatedRequest(
				http.MethodPost, testAdminOrigin+"/api/admin/actions", test.body,
			)
			if response.Code != http.StatusCreated {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			created := decodeAdminActionHTTPResponse(t, response)
			if created.ActionID == "" || created.Kind != test.kind ||
				created.Origin != ActionOriginDirectUser || created.SafetyClass != test.safety ||
				created.State != test.state ||
				created.ConfirmationRequired != test.confirmationRequired ||
				created.WarningRequired != test.warningRequired || created.SupportsExecution ||
				created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() ||
				created.HistoryEntryID != "" {

				t.Fatalf("unexpected action response: %+v", created)
			}
			if strings.Contains(response.Body.String(), `"history_id"`) {
				t.Fatalf("standalone HTTP action exposed a History link: %s", response.Body.String())
			}

			stored, err := fixture.store.getActionProposal(created.ActionID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Proposal.Origin() != ActionOriginDirectUser ||
				stored.Proposal.State() != test.state || stored.HistoryEntryID != "" {

				t.Fatalf("stored action=%+v", stored)
			}
			if test.kind == ActionKindDeployRepository {
				arguments, ok := stored.Proposal.Arguments().(DeployRepositoryArguments)
				if !ok || arguments.Provider != RepositoryProviderGitHub ||
					arguments.ContainerPort != 8080 || arguments.HealthPath != "/healthz" {

					t.Fatalf("stored deploy arguments=%#v", stored.Proposal.Arguments())
				}
			}
			events, err := fixture.store.listActionEvents(created.ActionID)
			if err != nil {
				t.Fatal(err)
			}
			assertActionEventTypes(t, events, test.eventType)
		})
	}
}

func TestAdminActionCreateRequiresAuthenticatedSameOriginBoundary(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	tests := []struct {
		name       string
		target     string
		token      string
		origin     string
		wantStatus int
	}{
		{
			name: "missing assertion", target: testAdminOrigin + "/api/admin/actions",
			origin: testAdminOrigin, wantStatus: http.StatusUnauthorized,
		},
		{
			name: "loopback is not authorization", target: "http://127.0.0.1:9300/api/admin/actions",
			origin: testAdminOrigin, wantStatus: http.StatusUnauthorized,
		},
		{
			name: "cross-site authenticated request", target: testAdminOrigin + "/api/admin/actions",
			token: fixture.token, origin: "https://attacker.example", wantStatus: http.StatusForbidden,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performAdminActionHTTPRequest(
				fixture.handler, http.MethodPost, test.target, adminRestartRequest,
				test.token, test.origin,
			)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
	if got := adminActionRowCount(t, fixture.store); got != 0 {
		t.Fatalf("unauthorized requests persisted %d actions", got)
	}
}

func TestAdminActionCreateRejectsClientControlledAuthorityFields(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	fields := []struct {
		name  string
		value string
	}{
		{name: "origin", value: `"direct_user"`},
		{name: "safety_class", value: `"recoverable"`},
		{name: "state", value: `"authorized"`},
		{name: "confirmation_required", value: `false`},
		{name: "warning_required", value: `false`},
		{name: "supports_execution", value: `false`},
		{name: "policy_reason", value: `"direct_user_reversible_or_recoverable"`},
		{name: "created_at", value: `"2026-10-21T00:00:00Z"`},
		{name: "updated_at", value: `"2026-10-21T00:00:00Z"`},
		{name: "history_id", value: `"history-forged"`},
		{name: "unexpected", value: `true`},
	}
	base := strings.TrimSuffix(adminRestartRequest, "}")
	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			body := fmt.Sprintf(`%s,%q:%s}`, base, field.name, field.value)
			response := fixture.authenticatedRequest(
				http.MethodPost, testAdminOrigin+"/api/admin/actions", body,
			)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if got := adminActionRowCount(t, fixture.store); got != 0 {
		t.Fatalf("rejected authority fields persisted %d actions", got)
	}
}

func TestAdminActionCreateRejectsInvalidJSONAndTypedInput(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{"kind":`},
		{name: "trailing JSON", body: adminRestartRequest + `{}`},
		{name: "oversized request", body: strings.Repeat(" ", adminActionRequestMaxBytes+1)},
		{name: "missing arguments", body: `{"kind":"restart_application","target":{"type":"application","id":"myscheduler"}}`},
		{name: "null arguments", body: `{"kind":"restart_application","target":{"type":"application","id":"myscheduler"},"arguments":null}`},
		{name: "unknown kind", body: `{"kind":"execute_shell","target":{"type":"application","id":"myscheduler"},"arguments":{}}`},
		{name: "cross-kind arguments", body: `{"kind":"restart_application","target":{"type":"application","id":"myscheduler"},"arguments":{"provider":"github","containerPort":8080,"healthPath":"/healthz"}}`},
		{name: "unknown target field", body: `{"kind":"restart_application","target":{"type":"application","id":"myscheduler","origin":"direct_user"},"arguments":{}}`},
		{name: "invalid repository", body: `{"kind":"deploy_repository","target":{"type":"repository","id":"https://github.com/ReactorLab/MyScheduler"},"arguments":{"provider":"github","containerPort":8080,"healthPath":"/healthz"}}`},
		{name: "port zero", body: `{"kind":"deploy_repository","target":{"type":"repository","id":"ReactorLab/MyScheduler"},"arguments":{"provider":"github","containerPort":0,"healthPath":"/healthz"}}`},
		{name: "port too large", body: `{"kind":"deploy_repository","target":{"type":"repository","id":"ReactorLab/MyScheduler"},"arguments":{"provider":"github","containerPort":65536,"healthPath":"/healthz"}}`},
		{name: "unsafe health path", body: `{"kind":"deploy_repository","target":{"type":"repository","id":"ReactorLab/MyScheduler"},"arguments":{"provider":"github","containerPort":8080,"healthPath":"https://attacker.example"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := fixture.authenticatedRequest(
				http.MethodPost, testAdminOrigin+"/api/admin/actions", test.body,
			)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if got := adminActionRowCount(t, fixture.store); got != 0 {
		t.Fatalf("invalid requests persisted %d actions", got)
	}
}

func TestAdminActionGetAndEventsUseHardenedRestoration(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	createdResponse := fixture.authenticatedRequest(
		http.MethodPost, testAdminOrigin+"/api/admin/actions", adminDeployRequest,
	)
	created := decodeAdminActionHTTPResponse(t, createdResponse)

	response := fixture.authenticatedRequest(
		http.MethodGet, testAdminOrigin+"/api/admin/actions/"+string(created.ActionID), "",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", response.Code, response.Body.String())
	}
	loaded := decodeAdminActionHTTPResponse(t, response)
	if loaded.ActionID != created.ActionID || loaded.State != ActionStateAwaitingConfirmation ||
		loaded.SupportsExecution {

		t.Fatalf("loaded action=%+v", loaded)
	}

	unauthenticated := performAdminActionHTTPRequest(
		fixture.handler, http.MethodGet,
		testAdminOrigin+"/api/admin/actions/"+string(created.ActionID), "", "", "",
	)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status=%d", unauthenticated.Code)
	}
	unknown := fixture.authenticatedRequest(
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions/action_00000000000000000000000000000000", "",
	)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown GET status=%d body=%s", unknown.Code, unknown.Body.String())
	}

	eventsResponse := fixture.authenticatedRequest(
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions/"+string(created.ActionID)+"/events", "",
	)
	events := decodeAdminActionEventsHTTPResponse(t, eventsResponse)
	if events.ActionID != created.ActionID || len(events.Events) != 1 ||
		events.Events[0].Ordinal != 0 || events.Events[0].EventType != ActionEventProposalCreated ||
		events.Events[0].FromState != nil ||
		events.Events[0].ToState != ActionStateAwaitingConfirmation {

		t.Fatalf("initial events=%+v", events)
	}

	confirmation := fixture.authenticatedRequest(
		http.MethodPost,
		testAdminOrigin+"/api/admin/actions/"+string(created.ActionID)+"/confirm",
		`{"confirm":true}`,
	)
	if confirmation.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", confirmation.Code, confirmation.Body.String())
	}
	eventsResponse = fixture.authenticatedRequest(
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions/"+string(created.ActionID)+"/events", "",
	)
	events = decodeAdminActionEventsHTTPResponse(t, eventsResponse)
	if len(events.Events) != 2 || events.Events[1].Ordinal != 1 ||
		events.Events[1].EventType != ActionEventConfirmationGranted ||
		events.Events[1].FromState == nil ||
		*events.Events[1].FromState != ActionStateAwaitingConfirmation ||
		events.Events[1].ToState != ActionStateAuthorized {

		t.Fatalf("confirmed events=%+v", events)
	}
}

func TestAdminActionGetRejectsCorruptPersistenceSafely(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	created := decodeAdminActionHTTPResponse(t, fixture.authenticatedRequest(
		http.MethodPost, testAdminOrigin+"/api/admin/actions", adminRollbackRequest,
	))
	execActionTestSQL(
		t, fixture.store,
		`UPDATE action_proposals SET safety_class = 'recoverable' WHERE action_id = ?`,
		created.ActionID,
	)
	for _, suffix := range []string{"", "/events"} {
		response := fixture.authenticatedRequest(
			http.MethodGet,
			testAdminOrigin+"/api/admin/actions/"+string(created.ActionID)+suffix,
			"",
		)
		if response.Code != http.StatusInternalServerError ||
			strings.Contains(response.Body.String(), "catalog metadata") ||
			strings.Contains(response.Body.String(), "recoverable") {

			t.Fatalf("unsafe corrupt-record response: status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestAdminActionConfirmationIsStrictAndNeverExecutes(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	created := decodeAdminActionHTTPResponse(t, fixture.authenticatedRequest(
		http.MethodPost, testAdminOrigin+"/api/admin/actions", adminDeployRequest,
	))
	path := testAdminOrigin + "/api/admin/actions/" + string(created.ActionID) + "/confirm"
	invalidBodies := []struct {
		name string
		body string
	}{
		{name: "empty body", body: ""},
		{name: "confirm false", body: `{"confirm":false}`},
		{name: "unknown state", body: `{"confirm":true,"state":"authorized"}`},
		{name: "malformed", body: `{"confirm":`},
		{name: "trailing document", body: `{"confirm":true}{}`},
		{name: "oversized", body: strings.Repeat(" ", adminActionRequestMaxBytes+1)},
	}
	for _, test := range invalidBodies {
		t.Run(test.name, func(t *testing.T) {
			response := fixture.authenticatedRequest(http.MethodPost, path, test.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			events, err := fixture.store.listActionEvents(created.ActionID)
			if err != nil {
				t.Fatal(err)
			}
			assertActionEventTypes(t, events, ActionEventProposalCreated)
		})
	}

	confirmedResponse := fixture.authenticatedRequest(http.MethodPost, path, `{"confirm":true}`)
	if confirmedResponse.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", confirmedResponse.Code, confirmedResponse.Body.String())
	}
	confirmed := decodeAdminActionHTTPResponse(t, confirmedResponse)
	if confirmed.State != ActionStateAuthorized || confirmed.SupportsExecution {
		t.Fatalf("confirmed action=%+v", confirmed)
	}
	stored, err := fixture.store.getActionProposal(created.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Proposal.State() != ActionStateAuthorized {
		t.Fatalf("stored state=%q", stored.Proposal.State())
	}
	events, err := fixture.store.listActionEvents(created.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	assertActionEventTypes(t, events, ActionEventProposalCreated, ActionEventConfirmationGranted)
}

func TestAdminActionRollbackRequiresExplicitWarningAcknowledgement(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	created := decodeAdminActionHTTPResponse(t, fixture.authenticatedRequest(
		http.MethodPost, testAdminOrigin+"/api/admin/actions", adminRollbackRequest,
	))
	path := testAdminOrigin + "/api/admin/actions/" + string(created.ActionID) + "/confirm"
	for _, body := range []string{
		`{"confirm":true}`,
		`{"confirm":true,"acknowledge_warning":false}`,
	} {
		response := fixture.authenticatedRequest(http.MethodPost, path, body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("missing warning acknowledgement status=%d body=%s", response.Code, response.Body.String())
		}
	}
	events, err := fixture.store.listActionEvents(created.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	assertActionEventTypes(t, events, ActionEventProposalCreated)

	response := fixture.authenticatedRequest(
		http.MethodPost, path, `{"confirm":true,"acknowledge_warning":true}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("acknowledged confirmation status=%d body=%s", response.Code, response.Body.String())
	}
	confirmed := decodeAdminActionHTTPResponse(t, response)
	if confirmed.State != ActionStateAuthorized || !confirmed.WarningRequired || confirmed.SupportsExecution {
		t.Fatalf("confirmed rollback=%+v", confirmed)
	}

	reconfirm := fixture.authenticatedRequest(
		http.MethodPost, path, `{"confirm":true,"acknowledge_warning":true}`,
	)
	if reconfirm.Code != http.StatusConflict {
		t.Fatalf("reconfirm status=%d body=%s", reconfirm.Code, reconfirm.Body.String())
	}
	events, err = fixture.store.listActionEvents(created.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	assertActionEventTypes(t, events, ActionEventProposalCreated, ActionEventConfirmationGranted)
}

func TestAdminActionCancellationOnlyCancelsPendingIntent(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	pending := decodeAdminActionHTTPResponse(t, fixture.authenticatedRequest(
		http.MethodPost, testAdminOrigin+"/api/admin/actions", adminDeployRequest,
	))
	path := testAdminOrigin + "/api/admin/actions/" + string(pending.ActionID) + "/cancel"

	for _, body := range []string{"{}", strings.Repeat("x", adminActionCancelMaxBytes+1)} {
		response := fixture.authenticatedRequest(http.MethodPost, path, body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("non-empty cancellation status=%d body=%s", response.Code, response.Body.String())
		}
	}
	response := fixture.authenticatedRequest(http.MethodPost, path, "")
	if response.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", response.Code, response.Body.String())
	}
	cancelled := decodeAdminActionHTTPResponse(t, response)
	if cancelled.State != ActionStateCancelled || cancelled.SupportsExecution {
		t.Fatalf("cancelled action=%+v", cancelled)
	}
	recancel := fixture.authenticatedRequest(http.MethodPost, path, "")
	if recancel.Code != http.StatusConflict {
		t.Fatalf("recancel status=%d body=%s", recancel.Code, recancel.Body.String())
	}
	events, err := fixture.store.listActionEvents(pending.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	assertActionEventTypes(t, events, ActionEventProposalCreated, ActionEventCancelled)

	authorized := decodeAdminActionHTTPResponse(t, fixture.authenticatedRequest(
		http.MethodPost, testAdminOrigin+"/api/admin/actions", adminRestartRequest,
	))
	response = fixture.authenticatedRequest(
		http.MethodPost,
		testAdminOrigin+"/api/admin/actions/"+string(authorized.ActionID)+"/cancel",
		"",
	)
	if response.Code != http.StatusConflict {
		t.Fatalf("authorized cancellation status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAdminActionPersistenceConflictsMapSafely(t *testing.T) {
	tests := []struct {
		name      string
		eventType ActionEventType
		suffix    string
		body      string
	}{
		{name: "confirmation", eventType: ActionEventConfirmationGranted, suffix: "/confirm", body: `{"confirm":true}`},
		{name: "cancellation", eventType: ActionEventCancelled, suffix: "/cancel"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAdminActionHTTPFixture(t)
			created := decodeAdminActionHTTPResponse(t, fixture.authenticatedRequest(
				http.MethodPost, testAdminOrigin+"/api/admin/actions", adminDeployRequest,
			))
			execActionTestSQL(t, fixture.store, fmt.Sprintf(`CREATE TRIGGER reject_http_%s_event
				BEFORE INSERT ON action_events
				WHEN NEW.event_type = %q
				BEGIN SELECT RAISE(ABORT, 'test rejection'); END`, test.name, test.eventType))
			response := fixture.authenticatedRequest(
				http.MethodPost,
				testAdminOrigin+"/api/admin/actions/"+string(created.ActionID)+test.suffix,
				test.body,
			)
			if response.Code != http.StatusConflict || strings.Contains(response.Body.String(), "test rejection") {
				t.Fatalf("conflict response status=%d body=%s", response.Code, response.Body.String())
			}
			stored, err := fixture.store.getActionProposal(created.ActionID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Proposal.State() != ActionStateAwaitingConfirmation {
				t.Fatalf("state escaped failed transaction: %q", stored.Proposal.State())
			}
			events, err := fixture.store.listActionEvents(created.ActionID)
			if err != nil {
				t.Fatal(err)
			}
			assertActionEventTypes(t, events, ActionEventProposalCreated)
		})
	}
}

func TestAdminActionExecutionAndGenericTransitionRoutesDoNotExist(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	created := decodeAdminActionHTTPResponse(t, fixture.authenticatedRequest(
		http.MethodPost, testAdminOrigin+"/api/admin/actions", adminRollbackRequest,
	))
	base := testAdminOrigin + "/api/admin/actions/" + string(created.ActionID)
	tests := []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: base + "/execute"},
		{method: http.MethodPost, path: base + "/run"},
		{method: http.MethodPost, path: base + "/transition"},
		{method: http.MethodPatch, path: base},
		{method: http.MethodPut, path: base},
		{method: http.MethodDelete, path: base},
	}
	for _, test := range tests {
		response := fixture.authenticatedRequest(test.method, test.path, "")
		if response.Code != http.StatusNotFound && response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s status=%d body=%s", test.method, test.path, response.Code, response.Body.String())
		}
	}
	stored, err := fixture.store.getActionProposal(created.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Proposal.State() != ActionStateAwaitingConfirmation {
		t.Fatalf("unsupported methods changed state to %q", stored.Proposal.State())
	}
	events, err := fixture.store.listActionEvents(created.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	assertActionEventTypes(t, events, ActionEventProposalCreated)
	var forbiddenStates int
	if err := fixture.store.db.QueryRow(
		`SELECT COUNT(*) FROM action_proposals WHERE current_state IN ('executing','succeeded','failed')`,
	).Scan(&forbiddenStates); err != nil {
		t.Fatal(err)
	}
	if forbiddenStates != 0 {
		t.Fatalf("HTTP made %d execution states reachable", forbiddenStates)
	}
}

func TestAdminActionPersistsAcrossReopenAndRemainsReadableThroughAPI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-actions.db")
	store, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	boundary, token := cryptographicAdminTestBoundary(t)
	handler := newAdminActionProductionTestHandler(store, boundary)
	createdResponse := performAdminActionHTTPRequest(
		handler, http.MethodPost, testAdminOrigin+"/api/admin/actions",
		adminDeployRequest, token, testAdminOrigin,
	)
	if createdResponse.Code != http.StatusCreated {
		_ = store.Close()
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	created := decodeAdminActionHTTPResponse(t, createdResponse)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	handler = newAdminActionProductionTestHandler(reopened, boundary)
	response := performAdminActionHTTPRequest(
		handler, http.MethodGet,
		testAdminOrigin+"/api/admin/actions/"+string(created.ActionID), "", token, "",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("reopened GET status=%d body=%s", response.Code, response.Body.String())
	}
	loaded := decodeAdminActionHTTPResponse(t, response)
	if loaded.ActionID != created.ActionID || loaded.State != ActionStateAwaitingConfirmation ||
		loaded.Kind != ActionKindDeployRepository || loaded.SupportsExecution {

		t.Fatalf("reopened action=%+v", loaded)
	}
}

func TestAdminActionRoutesLeaveHealthUnchanged(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	response := performAdminActionHTTPRequest(
		fixture.handler, http.MethodGet, "http://127.0.0.1:9300/health", "", "", "",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("health status=%d body=%s", response.Code, response.Body.String())
	}
	var health healthResponse
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if health.Service != "miniai" || health.Status != "ok" {
		t.Fatalf("health=%+v", health)
	}
}

func newAdminActionHTTPFixture(t *testing.T) adminActionHTTPFixture {
	t.Helper()
	store := newActionTestStore(t)
	boundary, token := cryptographicAdminTestBoundary(t)
	return adminActionHTTPFixture{
		store: store, handler: newAdminActionProductionTestHandler(store, boundary), token: token,
	}
}

func newAdminActionProductionTestHandler(
	store *chatStore,
	boundary adminSecurityBoundary,
) http.Handler {
	a := &app{store: store}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	a.registerAdminActionRoutes(mux)
	return withCommonHeaders(boundary.protectAdminRoutes(mux))
}

func (fixture adminActionHTTPFixture) authenticatedRequest(
	method string,
	target string,
	body string,
) *httptest.ResponseRecorder {
	return performAdminActionHTTPRequest(
		fixture.handler, method, target, body, fixture.token, testAdminOrigin,
	)
}

func performAdminActionHTTPRequest(
	handler http.Handler,
	method string,
	target string,
	body string,
	token string,
	origin string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set(adminAccessJWTHeader, token)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeAdminActionHTTPResponse(
	t *testing.T,
	response *httptest.ResponseRecorder,
) adminActionResponse {
	t.Helper()
	var value adminActionResponse
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatalf("decode action response: %v; body=%s", err, response.Body.String())
	}
	return value
}

func decodeAdminActionEventsHTTPResponse(
	t *testing.T,
	response *httptest.ResponseRecorder,
) adminActionEventsResponse {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("events status=%d body=%s", response.Code, response.Body.String())
	}
	var value adminActionEventsResponse
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatalf("decode event response: %v; body=%s", err, response.Body.String())
	}
	return value
}

func adminActionRowCount(t *testing.T, store *chatStore) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM action_proposals`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
