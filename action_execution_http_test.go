package main

import (
	"crypto"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

func TestAdminActionExecuteAuthenticatedSuccessAndExplicitSeparation(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	createdResponse := fixture.authenticatedRequest(
		http.MethodPost,
		testAdminOrigin+"/api/admin/actions",
		adminRestartRequest,
	)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	created := decodeAdminActionHTTPResponse(t, createdResponse)
	if created.State != ActionStateAuthorized || !created.SupportsExecution ||
		created.ExecutionOutcome != "" || created.FailureCategory != "" {

		t.Fatalf("created restart=%+v", created)
	}
	if strings.Contains(createdResponse.Body.String(), "execution_outcome") ||
		len(fixture.restarter.calls()) != 0 {

		t.Fatalf("create exposed an outcome or executed: body=%s calls=%v", createdResponse.Body.String(), fixture.restarter.calls())
	}

	executedResponse := fixture.authenticatedRequest(
		http.MethodPost,
		actionExecutionTestURL(created.ActionID),
		"",
	)
	if executedResponse.Code != http.StatusOK {
		t.Fatalf("execute status=%d body=%s", executedResponse.Code, executedResponse.Body.String())
	}
	executed := decodeAdminActionHTTPResponse(t, executedResponse)
	if executed.ActionID != created.ActionID || executed.State != ActionStateSucceeded ||
		!executed.SupportsExecution ||
		executed.ExecutionOutcome != RestartApplicationExecutionSucceeded ||
		executed.FailureCategory != "" {

		t.Fatalf("executed restart=%+v", executed)
	}
	if calls := fixture.restarter.calls(); len(calls) != 1 || calls[0] != "myscheduler" {
		t.Fatalf("restart calls=%v", calls)
	}

	getResponse := fixture.authenticatedRequest(
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions/"+string(created.ActionID),
		"",
	)
	loaded := decodeAdminActionHTTPResponse(t, getResponse)
	if getResponse.Code != http.StatusOK || loaded.State != ActionStateSucceeded ||
		loaded.ExecutionOutcome != "" || loaded.FailureCategory != "" {

		t.Fatalf("loaded restart status=%d action=%+v", getResponse.Code, loaded)
	}

	eventsResponse := fixture.authenticatedRequest(
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions/"+string(created.ActionID)+"/events",
		"",
	)
	events := decodeAdminActionEventsHTTPResponse(t, eventsResponse)
	assertAdminActionHTTPEventTypes(
		t,
		events,
		ActionEventPolicyAuthorizedOnCreation,
		ActionEventExecutionStarted,
		ActionEventExecutionSucceeded,
	)
	for _, event := range events.Events {
		if event.FailureCategory != "" {
			t.Fatalf("successful event exposed failure category: %+v", event)
		}
	}
}

func TestAdminActionExecuteDefinitiveMiniDeployRejectionIsSafe(t *testing.T) {
	var requests atomic.Int32
	miniDeploy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/deployments/myscheduler/restart" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "private upstream rejection", http.StatusConflict)
	}))
	defer miniDeploy.Close()

	store := newActionTestStore(t)
	boundary, token := cryptographicAdminTestBoundary(t)
	handler := newAdminActionTypedMiniDeployTestHandler(t, store, boundary, miniDeploy)
	created := mustCreateRestart(t, store, ActionOriginDirectUser)

	response := performAdminActionHTTPRequest(
		handler,
		http.MethodPost,
		actionExecutionTestURL(created.Proposal.ID()),
		"",
		token,
		testAdminOrigin,
	)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("execute status=%d body=%s", response.Code, response.Body.String())
	}
	result := decodeAdminActionHTTPResponse(t, response)
	if result.State != ActionStateFailed ||
		result.ExecutionOutcome != RestartApplicationExecutionFailed ||
		result.FailureCategory != ActionExecutionFailureUpstreamRejected {

		t.Fatalf("failed execution=%+v", result)
	}
	if requests.Load() != 1 {
		t.Fatalf("MiniDeploy requests=%d want=1", requests.Load())
	}
	if strings.Contains(response.Body.String(), "private upstream rejection") ||
		strings.Contains(response.Body.String(), miniDeploy.URL) {

		t.Fatalf("unsafe upstream detail in response: %s", response.Body.String())
	}
	getResponse := performAdminActionHTTPRequest(
		handler,
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions/"+string(created.Proposal.ID()),
		"",
		token,
		"",
	)
	loaded := decodeAdminActionHTTPResponse(t, getResponse)
	if getResponse.Code != http.StatusOK || loaded.State != ActionStateFailed ||
		loaded.ExecutionOutcome != "" || loaded.FailureCategory != "" {

		t.Fatalf("loaded failed action status=%d action=%+v", getResponse.Code, loaded)
	}

	retry := performAdminActionHTTPRequest(
		handler,
		http.MethodPost,
		actionExecutionTestURL(created.Proposal.ID()),
		"",
		token,
		testAdminOrigin,
	)
	if retry.Code != http.StatusConflict || requests.Load() != 1 {
		t.Fatalf("retry status=%d requests=%d body=%s", retry.Code, requests.Load(), retry.Body.String())
	}

	eventsResponse := performAdminActionHTTPRequest(
		handler,
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions/"+string(created.Proposal.ID())+"/events",
		"",
		token,
		"",
	)
	events := decodeAdminActionEventsHTTPResponse(t, eventsResponse)
	assertAdminActionHTTPEventTypes(
		t,
		events,
		ActionEventPolicyAuthorizedOnCreation,
		ActionEventExecutionStarted,
		ActionEventExecutionFailed,
	)
	if events.Events[2].FailureCategory != ActionExecutionFailureUpstreamRejected ||
		strings.Contains(eventsResponse.Body.String(), "private upstream rejection") {

		t.Fatalf("unsafe failure event response: %s", eventsResponse.Body.String())
	}
}

func TestAdminActionExecuteAmbiguousMiniDeployResponseIsSafeAndNotRetried(t *testing.T) {
	var requests atomic.Int32
	miniDeploy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "private uncertain upstream failure", http.StatusInternalServerError)
	}))
	defer miniDeploy.Close()

	store := newActionTestStore(t)
	boundary, token := cryptographicAdminTestBoundary(t)
	handler := newAdminActionTypedMiniDeployTestHandler(t, store, boundary, miniDeploy)
	created := mustCreateRestart(t, store, ActionOriginDirectUser)

	response := performAdminActionHTTPRequest(
		handler,
		http.MethodPost,
		actionExecutionTestURL(created.Proposal.ID()),
		"",
		token,
		testAdminOrigin,
	)
	if response.Code != http.StatusAccepted {
		t.Fatalf("execute status=%d body=%s", response.Code, response.Body.String())
	}
	result := decodeAdminActionHTTPResponse(t, response)
	if result.State != ActionStateExecuting ||
		result.ExecutionOutcome != RestartApplicationExecutionOutcomeUnknown ||
		result.FailureCategory != ActionExecutionFailureUpstreamRejected {

		t.Fatalf("ambiguous execution=%+v", result)
	}
	if requests.Load() != 1 {
		t.Fatalf("MiniDeploy requests=%d want=1", requests.Load())
	}
	if strings.Contains(response.Body.String(), "private uncertain upstream failure") ||
		strings.Contains(response.Body.String(), miniDeploy.URL) {

		t.Fatalf("unsafe upstream detail in response: %s", response.Body.String())
	}
	getResponse := performAdminActionHTTPRequest(
		handler,
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions/"+string(created.Proposal.ID()),
		"",
		token,
		"",
	)
	loaded := decodeAdminActionHTTPResponse(t, getResponse)
	if getResponse.Code != http.StatusOK || loaded.State != ActionStateExecuting ||
		loaded.ExecutionOutcome != "" || loaded.FailureCategory != "" {

		t.Fatalf("loaded ambiguous action status=%d action=%+v", getResponse.Code, loaded)
	}

	second := performAdminActionHTTPRequest(
		handler,
		http.MethodPost,
		actionExecutionTestURL(created.Proposal.ID()),
		"",
		token,
		testAdminOrigin,
	)
	if second.Code != http.StatusConflict || requests.Load() != 1 {
		t.Fatalf("second execute status=%d requests=%d body=%s", second.Code, requests.Load(), second.Body.String())
	}
	eventsResponse := performAdminActionHTTPRequest(
		handler,
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions/"+string(created.Proposal.ID())+"/events",
		"",
		token,
		"",
	)
	events := decodeAdminActionEventsHTTPResponse(t, eventsResponse)
	assertAdminActionHTTPEventTypes(
		t,
		events,
		ActionEventPolicyAuthorizedOnCreation,
		ActionEventExecutionStarted,
	)
	if events.Events[1].FailureCategory != "" ||
		strings.Contains(eventsResponse.Body.String(), "private uncertain upstream failure") {

		t.Fatalf("unsafe ambiguous event response: %s", eventsResponse.Body.String())
	}
}

func TestAdminActionExecuteRejectsWrongStateAndKindBeforeMiniDeploy(t *testing.T) {
	tests := []struct {
		name  string
		build func(*testing.T, *chatStore) storedActionProposal
	}{
		{
			name: "awaiting confirmation",
			build: func(t *testing.T, store *chatStore) storedActionProposal {
				return mustCreateRestart(t, store, ActionOriginAssistantSuggested)
			},
		},
		{
			name: "cancelled",
			build: func(t *testing.T, store *chatStore) storedActionProposal {
				created := mustCreateRestart(t, store, ActionOriginAssistantSuggested)
				cancelled, err := store.cancelAction(created.Proposal.ID())
				if err != nil {
					t.Fatal(err)
				}
				return cancelled
			},
		},
		{
			name: "executing",
			build: func(t *testing.T, store *chatStore) storedActionProposal {
				created := mustCreateRestart(t, store, ActionOriginDirectUser)
				executing, err := store.beginRestartApplicationExecution(created.Proposal)
				if err != nil {
					t.Fatal(err)
				}
				return executing
			},
		},
		{
			name: "succeeded",
			build: func(t *testing.T, store *chatStore) storedActionProposal {
				created := mustCreateRestart(t, store, ActionOriginDirectUser)
				executing, err := store.beginRestartApplicationExecution(created.Proposal)
				if err != nil {
					t.Fatal(err)
				}
				succeeded, err := store.completeRestartApplicationSuccess(executing.Proposal.ID())
				if err != nil {
					t.Fatal(err)
				}
				return succeeded
			},
		},
		{
			name: "failed",
			build: func(t *testing.T, store *chatStore) storedActionProposal {
				created := mustCreateRestart(t, store, ActionOriginDirectUser)
				executing, err := store.beginRestartApplicationExecution(created.Proposal)
				if err != nil {
					t.Fatal(err)
				}
				failed, err := store.completeRestartApplicationFailure(
					executing.Proposal.ID(),
					ActionExecutionFailureUpstreamRejected,
				)
				if err != nil {
					t.Fatal(err)
				}
				return failed
			},
		},
		{
			name: "rollback",
			build: func(t *testing.T, store *chatStore) storedActionProposal {
				created := mustCreateRollback(t, store, ActionOriginDirectUser, "")
				authorized, err := store.confirmAction(created.Proposal.ID())
				if err != nil {
					t.Fatal(err)
				}
				return authorized
			},
		},
		{
			name: "deploy",
			build: func(t *testing.T, store *chatStore) storedActionProposal {
				created := mustCreateAction(t, store, actionProposalCreateInput{
					Kind: ActionKindDeployRepository, Origin: ActionOriginDirectUser,
					Target: ActionTarget{Type: ActionTargetRepository, ID: "owner/repository"},
					Arguments: DeployRepositoryArguments{
						Provider: RepositoryProviderGitHub, ContainerPort: 8080, HealthPath: "/health",
					},
					CreatedAt: actionTestTime(),
				})
				authorized, err := store.confirmAction(created.Proposal.ID())
				if err != nil {
					t.Fatal(err)
				}
				return authorized
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newActionTestStore(t)
			record := test.build(t, store)
			restarter := &fakeMiniDeployRestarter{}
			boundary, token := cryptographicAdminTestBoundary(t)
			handler := newAdminActionProductionTestHandlerWithRestarter(
				store,
				boundary,
				restarter,
			)
			response := performAdminActionHTTPRequest(
				handler,
				http.MethodPost,
				actionExecutionTestURL(record.Proposal.ID()),
				"",
				token,
				testAdminOrigin,
			)
			if response.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if calls := restarter.calls(); len(calls) != 0 {
				t.Fatalf("ineligible action made restart calls=%v", calls)
			}
		})
	}
}

func TestAdminActionExecuteRequiresAuthenticatedSameOriginAdministrator(t *testing.T) {
	store := newActionTestStore(t)
	created := mustCreateRestart(t, store, ActionOriginDirectUser)
	boundary, validToken, wrongAdminToken := actionExecutionAdminTestBoundary(t)
	restarter := &fakeMiniDeployRestarter{}
	handler := newAdminActionProductionTestHandlerWithRestarter(store, boundary, restarter)
	path := actionExecutionTestURL(created.Proposal.ID())

	tests := []struct {
		name       string
		target     string
		token      string
		origin     string
		wantStatus int
	}{
		{
			name: "missing JWT", target: path, origin: testAdminOrigin,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:   "loopback without JWT",
			target: "http://127.0.0.1:9300/api/admin/actions/" + string(created.Proposal.ID()) + "/execute",
			origin: testAdminOrigin, wantStatus: http.StatusUnauthorized,
		},
		{
			name: "wrong administrator", target: path, token: wrongAdminToken,
			origin: testAdminOrigin, wantStatus: http.StatusForbidden,
		},
		{
			name: "cross-site", target: path, token: validToken,
			origin: "https://attacker.example", wantStatus: http.StatusForbidden,
		},
		{
			name: "malformed Origin", target: path, token: validToken,
			origin: "https://", wantStatus: http.StatusForbidden,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := performAdminActionHTTPRequest(
				handler,
				http.MethodPost,
				test.target,
				"",
				test.token,
				test.origin,
			)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.wantStatus, response.Body.String())
			}
			if calls := restarter.calls(); len(calls) != 0 {
				t.Fatalf("rejected request made restart calls=%v", calls)
			}
		})
	}

	allowed := performAdminActionHTTPRequest(
		handler,
		http.MethodPost,
		path,
		"",
		validToken,
		testAdminOrigin,
	)
	if allowed.Code != http.StatusOK || len(restarter.calls()) != 1 {
		t.Fatalf("allowed request status=%d calls=%v body=%s", allowed.Code, restarter.calls(), allowed.Body.String())
	}
}

func TestAdminActionExecuteRejectsMethodsPathsAndInputBeforeMiniDeploy(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	created := mustCreateRestart(t, fixture.store, ActionOriginDirectUser)
	base := actionExecutionTestURL(created.Proposal.ID())
	escapedID := "%61" + string(created.Proposal.ID())[1:]
	tests := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{name: "GET", method: http.MethodGet, target: base},
		{name: "PATCH", method: http.MethodPatch, target: base},
		{name: "PUT", method: http.MethodPut, target: base},
		{name: "DELETE", method: http.MethodDelete, target: base},
		{name: "extra path", method: http.MethodPost, target: base + "/extra"},
		{name: "empty action ID", method: http.MethodPost, target: testAdminOrigin + "/api/admin/actions//execute"},
		{
			name: "escaped alias", method: http.MethodPost,
			target: testAdminOrigin + "/api/admin/actions/" + escapedID + "/execute",
		},
		{name: "query options", method: http.MethodPost, target: base + "?retry=1"},
		{name: "body metadata", method: http.MethodPost, target: base, body: `{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := fixture.authenticatedRequest(test.method, test.target, test.body)
			if response.Code >= http.StatusOK && response.Code < http.StatusMultipleChoices {
				t.Fatalf("unsafe request succeeded: status=%d body=%s", response.Code, response.Body.String())
			}
			if calls := fixture.restarter.calls(); len(calls) != 0 {
				t.Fatalf("unsafe request made restart calls=%v", calls)
			}
		})
	}

	loaded, err := fixture.store.getActionProposal(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Proposal.State() != ActionStateAuthorized {
		t.Fatalf("rejected input changed state to %q", loaded.Proposal.State())
	}
}

func TestAdminActionExecuteConcurrencyCallsMiniDeployOnce(t *testing.T) {
	store := newActionTestStore(t)
	created := mustCreateRestart(t, store, ActionOriginDirectUser)
	boundary, token := cryptographicAdminTestBoundary(t)
	restarter := &fakeMiniDeployRestarter{
		firstEntered: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	handler := newAdminActionProductionTestHandlerWithRestarter(store, boundary, restarter)
	path := actionExecutionTestURL(created.Proposal.ID())
	firstResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstResponse <- performAdminActionHTTPRequest(
			handler,
			http.MethodPost,
			path,
			"",
			token,
			testAdminOrigin,
		)
	}()

	select {
	case <-restarter.firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first execution did not reach MiniDeploy")
	}
	secondResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		secondResponse <- performAdminActionHTTPRequest(
			handler,
			http.MethodPost,
			path,
			"",
			token,
			testAdminOrigin,
		)
	}()

	var losing *httptest.ResponseRecorder
	select {
	case losing = <-secondResponse:
	case <-time.After(5 * time.Second):
		close(restarter.releaseFirst)
		t.Fatal("concurrent loser did not fail before MiniDeploy completed")
	}
	if losing.Code != http.StatusConflict {
		close(restarter.releaseFirst)
		t.Fatalf("concurrent loser status=%d body=%s", losing.Code, losing.Body.String())
	}
	close(restarter.releaseFirst)

	var winning *httptest.ResponseRecorder
	select {
	case winning = <-firstResponse:
	case <-time.After(5 * time.Second):
		t.Fatal("winning execution did not complete")
	}
	if winning.Code != http.StatusOK {
		t.Fatalf("winning status=%d body=%s", winning.Code, winning.Body.String())
	}
	if calls := restarter.calls(); len(calls) != 1 || calls[0] != "myscheduler" {
		t.Fatalf("concurrent restart calls=%v", calls)
	}
	events, err := store.listActionEvents(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	assertActionEventTypes(
		t,
		events,
		ActionEventPolicyAuthorizedOnCreation,
		ActionEventExecutionStarted,
		ActionEventExecutionSucceeded,
	)
}

func TestAdminActionConfirmDoesNotExecuteAssistantRestart(t *testing.T) {
	fixture := newAdminActionHTTPFixture(t)
	created := mustCreateRestart(t, fixture.store, ActionOriginAssistantSuggested)
	confirm := fixture.authenticatedRequest(
		http.MethodPost,
		testAdminOrigin+"/api/admin/actions/"+string(created.Proposal.ID())+"/confirm",
		`{"confirm":true}`,
	)
	if confirm.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", confirm.Code, confirm.Body.String())
	}
	confirmed := decodeAdminActionHTTPResponse(t, confirm)
	if confirmed.State != ActionStateAuthorized || !confirmed.SupportsExecution ||
		len(fixture.restarter.calls()) != 0 {

		t.Fatalf("confirmation executed or returned wrong policy: action=%+v calls=%v", confirmed, fixture.restarter.calls())
	}

	execute := fixture.authenticatedRequest(
		http.MethodPost,
		actionExecutionTestURL(created.Proposal.ID()),
		"",
	)
	if execute.Code != http.StatusOK || len(fixture.restarter.calls()) != 1 {
		t.Fatalf("execute status=%d calls=%v body=%s", execute.Code, fixture.restarter.calls(), execute.Body.String())
	}
}

func TestPre4B2RestartSnapshotRemainsReadableAndNonExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-4b2-actions.db")
	store, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	created := mustCreateRestart(t, store, ActionOriginDirectUser)
	execActionTestSQL(
		t,
		store,
		`UPDATE action_proposals SET supports_execution = 0 WHERE action_id = ?`,
		created.Proposal.ID(),
	)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var version int
	if err := reopened.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != miniAISchemaVersion {
		t.Fatalf("schema version=%d want=%d", version, miniAISchemaVersion)
	}
	loaded, err := reopened.getActionProposal(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Proposal.State() != ActionStateAuthorized || loaded.SupportsExecution {
		t.Fatalf("legacy restart snapshot=%+v", loaded)
	}
	assertActionSupportsExecution(t, ActionKindRestartApplication, true)

	boundary, token := cryptographicAdminTestBoundary(t)
	restarter := &fakeMiniDeployRestarter{}
	handler := newAdminActionProductionTestHandlerWithRestarter(
		reopened,
		boundary,
		restarter,
	)
	getResponse := performAdminActionHTTPRequest(
		handler,
		http.MethodGet,
		testAdminOrigin+"/api/admin/actions/"+string(created.Proposal.ID()),
		"",
		token,
		"",
	)
	got := decodeAdminActionHTTPResponse(t, getResponse)
	if getResponse.Code != http.StatusOK || got.SupportsExecution {
		t.Fatalf("legacy GET status=%d action=%+v", getResponse.Code, got)
	}
	executeResponse := performAdminActionHTTPRequest(
		handler,
		http.MethodPost,
		actionExecutionTestURL(created.Proposal.ID()),
		"",
		token,
		testAdminOrigin,
	)
	if executeResponse.Code != http.StatusConflict || len(restarter.calls()) != 0 {
		t.Fatalf("legacy execute status=%d calls=%v body=%s", executeResponse.Code, restarter.calls(), executeResponse.Body.String())
	}
	events, err := reopened.listActionEvents(created.Proposal.ID())
	if err != nil {
		t.Fatal(err)
	}
	assertActionEventTypes(t, events, ActionEventPolicyAuthorizedOnCreation)
}

func actionExecutionTestURL(id ActionID) string {
	return testAdminOrigin + "/api/admin/actions/" + string(id) + "/execute"
}

func assertAdminActionHTTPEventTypes(
	t *testing.T,
	response adminActionEventsResponse,
	want ...ActionEventType,
) {
	t.Helper()
	if len(response.Events) != len(want) {
		t.Fatalf("events=%+v want types=%v", response.Events, want)
	}
	for index, eventType := range want {
		if response.Events[index].EventType != eventType || response.Events[index].Ordinal != index {
			t.Fatalf("event[%d]=%+v want type=%q ordinal=%d", index, response.Events[index], eventType, index)
		}
	}
}

func newAdminActionTypedMiniDeployTestHandler(
	t *testing.T,
	store *chatStore,
	boundary adminSecurityBoundary,
	server *httptest.Server,
) http.Handler {
	t.Helper()
	client, err := newMiniDeployRestartClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return newAdminActionProductionTestHandlerWithRestarter(store, boundary, client)
}

func actionExecutionAdminTestBoundary(
	t *testing.T,
) (adminSecurityBoundary, string, string) {
	t.Helper()
	privateKey := generateAdminTestKey(t)
	validator := newCloudflareAdminValidatorWithKeySet(
		testAdminIssuer,
		testAdminAudience,
		testAdminEmail,
		&oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&privateKey.PublicKey}},
	)
	now := time.Now().UTC()
	claims := adminAccessTestClaims{
		issuer: testAdminIssuer, audience: testAdminAudience, email: testAdminEmail,
		expiresAt: now.Add(time.Hour), notBefore: now.Add(-time.Minute),
	}
	validToken := signAdminTestToken(t, privateKey, "", claims)
	claims.email = "other@example.com"
	wrongAdminToken := signAdminTestToken(t, privateKey, "", claims)
	return adminSecurityBoundary{
		validator: validator, trustedOrigin: testAdminOrigin,
	}, validToken, wrongAdminToken
}
