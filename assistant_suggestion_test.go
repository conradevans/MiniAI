package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testAssistantSuggestionPacket() EvidencePacket {
	packet := testReasonerPacket()
	packet.Route = RouteApplicationPerformance
	packet.Goal = GoalIncidentExplanation
	packet.Subjects = []EvidenceSubject{{Kind: SubjectApplication, ID: "myscheduler", Name: "MyScheduler"}}
	packet.Evidence[0].Subject = packet.Subjects[0]
	packet.Evidence[0].Type = EvidenceTypeCurrentApplication
	packet.Confidence = EvidencePacketConfidence{
		Level: ConfidenceHigh, SoftwareCeiling: ConfidenceHigh,
		Inputs: EvidencePacketConfidenceInputs{
			CriticalCompleteness: CompletenessComplete,
			Freshness:            FreshnessFresh, Directness: DirectnessDirect,
			Agreement: AgreementConsistent, Coverage: CoverageComplete,
		},
	}
	return packet
}

func assistantReasonerJSON(suggestion string) string {
	return `{"conclusion":"MyScheduler recorded recent failures in the available evidence.","conclusion_evidence":["evidence-1"],"facts":[],"uncertainty":[],"action_suggestion":` + suggestion + `}`
}

func validAssistantSuggestionJSON(reference string) string {
	encoded, _ := json.Marshal(reasonerActionSuggestion{
		Type:        reasonerRestartCurrentApplication,
		Reason:      "MyScheduler is unhealthy and a restart may restore service.",
		EvidenceIDs: []string{reference},
	})
	return string(encoded)
}

func TestReasonerActionSuggestionUsesClosedEvidenceGroundedContract(t *testing.T) {
	packet := testAssistantSuggestionPacket()
	draft, err := parseReasonerDraft(
		assistantReasonerJSON(validAssistantSuggestionJSON("evidence-1")),
		packet,
	)
	if err != nil {
		t.Fatal(err)
	}
	suggestion, err := validateReasonerActionSuggestion(draft.ActionSuggestion, packet)
	if err != nil {
		t.Fatal(err)
	}
	if suggestion == nil || suggestion.Target != (ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"}) ||
		suggestion.Reason != "MyScheduler is unhealthy and a restart may restore service." {

		t.Fatalf("validated suggestion=%+v", suggestion)
	}

	schemaJSON := canonicalJSON(reasonerDraftSchema(packet))
	for _, required := range []string{`"action_suggestion"`, `"restart_current_application"`, `"evidence_ids"`} {
		if !strings.Contains(schemaJSON, required) {
			t.Fatalf("suggestion schema missing %s: %s", required, schemaJSON)
		}
	}
	for _, forbidden := range []string{`"target"`, `"kind"`, `"arguments"`, `"origin"`, `"state"`, `"supports_execution"`} {
		if strings.Contains(schemaJSON, forbidden) {
			t.Fatalf("suggestion schema exposes model-controlled field %s: %s", forbidden, schemaJSON)
		}
	}
	instruction := reasonerActionSuggestionInstruction(packet)
	for _, required := range []string{"optional", "cannot execute", "cannot", "Never emit a target", "do not suggest one"} {
		if !strings.Contains(instruction, required) {
			t.Fatalf("suggestion instruction missing %q: %s", required, instruction)
		}
	}

	withoutSuggestion, err := parseReasonerDraft(validReasonerJSON(), packet)
	if err != nil {
		t.Fatal(err)
	}
	if suggestion, err := validateReasonerActionSuggestion(withoutSuggestion.ActionSuggestion, packet); err != nil || suggestion != nil {
		t.Fatalf("omitted suggestion=%+v err=%v", suggestion, err)
	}
	nullSuggestion, err := parseReasonerDraft(assistantReasonerJSON("null"), packet)
	if err != nil {
		t.Fatal(err)
	}
	if suggestion, err := validateReasonerActionSuggestion(nullSuggestion.ActionSuggestion, packet); err != nil || suggestion != nil {
		t.Fatalf("null suggestion=%+v err=%v", suggestion, err)
	}
}

func TestDuplicateTopLevelReasonerActionSuggestionsAreDiscardedWithoutLosingAnswer(t *testing.T) {
	packet := testAssistantSuggestionPacket()
	suggestion := validAssistantSuggestionJSON("evidence-1")
	raw := fmt.Sprintf(
		`{"conclusion":"MyScheduler recorded recent failures in the available evidence.","conclusion_evidence":["evidence-1"],"facts":[],"uncertainty":[{"text":"Recovery duration is not established by the available evidence.","evidence_ids":["evidence-1"]}],"action_suggestion":%s,"action_suggestion":%s}`,
		suggestion,
		suggestion,
	)
	draft, err := parseReasonerDraft(raw, packet)
	if err != nil {
		t.Fatalf("duplicate optional suggestion invalidated the normal answer: %v", err)
	}
	if len(draft.ActionSuggestion) != 0 {
		t.Fatalf("duplicate optional suggestion was retained: %s", draft.ActionSuggestion)
	}
	if draft.Conclusion != "MyScheduler recorded recent failures in the available evidence." ||
		len(draft.Uncertainty) != 1 ||
		draft.Uncertainty[0].Text != "Recovery duration is not established by the available evidence." {

		t.Fatalf("base answer changed: %+v", draft)
	}
	if validated, err := validateReasonerActionSuggestion(draft.ActionSuggestion, packet); err != nil || validated != nil {
		t.Fatalf("duplicate optional suggestion remained actionable: suggestion=%+v err=%v", validated, err)
	}
	const expected = "MyScheduler recorded recent failures in the available evidence. Uncertainty: Recovery duration is not established by the available evidence. Confidence: Medium."
	if answer := renderReasonerAnswer(draft, ConfidenceMedium); answer != expected {
		t.Fatalf("rendered answer=%q want=%q", answer, expected)
	}
}

func TestReasonerActionSuggestionAdversarialShapesAreDiscardedWithoutLosingAnswer(t *testing.T) {
	packet := testAssistantSuggestionPacket()
	oversizedReason, _ := json.Marshal(map[string]any{
		"type":         reasonerRestartCurrentApplication,
		"reason":       strings.Repeat("x", reasonerActionReasonMaxRunes+1),
		"evidence_ids": []string{"evidence-1"},
	})
	tests := []struct {
		name       string
		suggestion string
	}{
		{"unsupported action kind", `{"type":"shutdown_application","reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"rollback suggestion", `{"type":"rollback_deployment","reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"deploy suggestion", `{"type":"deploy_repository","reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"model kind field", `{"type":"restart_current_application","kind":"restart_application","reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"arbitrary command field", `{"type":"restart_current_application","command":"systemctl restart myscheduler","reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"command in reason", `{"type":"restart_current_application","reason":"Run systemctl restart myscheduler.","evidence_ids":["evidence-1"]}`},
		{"arbitrary URL field", `{"type":"restart_current_application","url":"https://example.invalid/restart","reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"target mismatch", `{"type":"restart_current_application","target":{"type":"application","id":"another-app"},"reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"malformed structure", `"restart_current_application"`},
		{"oversized reason", string(oversizedReason)},
		{"extra safety class", `{"type":"restart_current_application","safety_class":"reversible","reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"model origin", `{"type":"restart_current_application","origin":"direct_user","reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"model state", `{"type":"restart_current_application","state":"authorized","reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"supports execution true", `{"type":"restart_current_application","supports_execution":true,"reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"supports execution false", `{"type":"restart_current_application","supports_execution":false,"reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"confirmation bypass", `{"type":"restart_current_application","confirmation_required":false,"reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"arbitrary arguments", `{"type":"restart_current_application","arguments":{"port":22},"reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"arbitrary history", `{"type":"restart_current_application","history_id":"other-history","reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
		{"unknown evidence", `{"type":"restart_current_application","reason":"Bounded reason.","evidence_ids":["invented"]}`},
		{"duplicate suggestion key", `{"type":"restart_current_application","type":"rollback_deployment","reason":"Bounded reason.","evidence_ids":["evidence-1"]}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			draft, err := parseReasonerDraft(assistantReasonerJSON(test.suggestion), packet)
			if err != nil {
				t.Fatalf("optional malformed suggestion invalidated the normal answer: %v", err)
			}
			if answer := renderReasonerAnswer(draft, ConfidenceHigh); !strings.Contains(answer, "MyScheduler recorded recent failures") {
				t.Fatalf("normal answer was lost: %q", answer)
			}
			if suggestion, err := validateReasonerActionSuggestion(draft.ActionSuggestion, packet); !errors.Is(err, errInvalidReasonerActionSuggestion) || suggestion != nil {

				t.Fatalf("suggestion=%+v err=%v", suggestion, err)
			}
		})
	}
}

func TestReasonerActionSuggestionRequiresEligibleTrustedApplicationEvidence(t *testing.T) {
	validRaw := json.RawMessage(validAssistantSuggestionJSON("evidence-1"))
	tests := []struct {
		name   string
		mutate func(*EvidencePacket)
	}{
		{"low confidence", func(packet *EvidencePacket) {
			packet.Confidence.Level, packet.Confidence.SoftwareCeiling = ConfidenceLow, ConfidenceLow
		}},
		{"critical evidence partial", func(packet *EvidencePacket) {
			packet.Confidence.Inputs.CriticalCompleteness = CompletenessPartial
		}},
		{"unrelated route", func(packet *EvidencePacket) { packet.Route = RouteCurrentPlatformHealth }},
		{"multi target", func(packet *EvidencePacket) {
			packet.Subjects = append(packet.Subjects, EvidenceSubject{Kind: SubjectApplication, ID: "golf-mullet"})
		}},
		{"untrusted target syntax", func(packet *EvidencePacket) { packet.Subjects[0].ID = "MyScheduler" }},
		{"no evidence", func(packet *EvidencePacket) { packet.Evidence = nil }},
		{"no target evidence", func(packet *EvidencePacket) {
			packet.Evidence[0].Subject = EvidenceSubject{Kind: SubjectHost, ID: "dell"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packet := testAssistantSuggestionPacket()
			test.mutate(&packet)
			if suggestion, err := validateReasonerActionSuggestion(validRaw, packet); err == nil || suggestion != nil {
				t.Fatalf("ineligible suggestion=%+v err=%v", suggestion, err)
			}
		})
	}
}

func TestAssistantRestartSuggestionPersistenceIsHistoryScopedIdempotentAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assistant-suggestions.db")
	store, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	history, _, err := store.startHistoryEntry("", "investigate MyScheduler", actionTestTime())
	if err != nil {
		t.Fatal(err)
	}
	target := ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"}

	const attempts = 12
	results := make(chan storedActionProposal, attempts)
	errorsFound := make(chan error, attempts)
	var wait sync.WaitGroup
	for range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			record, createErr := store.createAssistantRestartSuggestion(history.ID, target)
			if createErr != nil {
				errorsFound <- createErr
				return
			}
			results <- record
		}()
	}
	wait.Wait()
	close(results)
	close(errorsFound)
	for createErr := range errorsFound {
		t.Fatalf("idempotent create failed: %v", createErr)
	}
	var firstID ActionID
	for record := range results {
		if firstID == "" {
			firstID = record.Proposal.ID()
		}
		if record.Proposal.ID() != firstID {
			t.Fatalf("same History created duplicate IDs %q and %q", firstID, record.Proposal.ID())
		}
	}
	if got := assistantActionRowCount(t, store); got != 1 {
		t.Fatalf("same History persisted %d suggestions", got)
	}
	record, err := store.getActionProposal(firstID)
	if err != nil {
		t.Fatal(err)
	}
	assertAssistantRestartPolicy(t, record, history.ID)
	assertActionEventTypes(t, mustActionEvents(t, store, firstID), ActionEventProposalCreated)
	if _, err := store.createAssistantRestartSuggestion(
		history.ID,
		ActionTarget{Type: ActionTargetApplication, ID: "golf-mullet"},
	); !errors.Is(err, errActionPersistenceConflict) {
		t.Fatalf("same History accepted a different target: %v", err)
	}

	if _, err := store.finalizeHistoryEntry(history.ID, historyFinalizeInput{
		Kind: HistoryKindInvestigation, Status: HistoryStatusSucceeded,
		ResultText: "bounded result", CompletedAt: actionTestTime().Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	later, _, err := store.startHistoryEntry("", "investigate MyScheduler later", actionTestTime().Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	laterRecord, err := store.createAssistantRestartSuggestion(later.ID, target)
	if err != nil {
		t.Fatal(err)
	}
	if laterRecord.Proposal.ID() == firstID || assistantActionRowCount(t, store) != 2 {
		t.Fatalf("new investigation was incorrectly globally deduplicated: %+v", laterRecord)
	}
	if _, err := store.finalizeHistoryEntry(later.ID, historyFinalizeInput{
		Kind: HistoryKindInvestigation, Status: HistoryStatusSucceeded,
		ResultText: "later result", CompletedAt: actionTestTime().Add(3 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	loaded, err := reopened.getActionProposal(firstID)
	if err != nil {
		t.Fatal(err)
	}
	assertAssistantRestartPolicy(t, loaded, history.ID)
	ids, err := reopened.historyActionProposalIDs(history.ID)
	if err != nil || len(ids) != 1 || ids[0] != firstID {
		t.Fatalf("reopened History linkage ids=%v err=%v", ids, err)
	}
}

func TestPhase2CValidAssistantRestartSuggestionPersistsWithoutExecution(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	h.app.phase2Now = func() time.Time { return now }
	h.app.phase2Executor = productionApplicationIncidentExecutor(now)
	h.responseBuilder = assistantSuggestionResponseBuilder
	var miniDeployCalls atomic.Int64
	miniDeploy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		miniDeployCalls.Add(1)
		http.Error(w, "unexpected MiniDeploy call", http.StatusInternalServerError)
	}))
	defer miniDeploy.Close()
	h.app.minideployURL = miniDeploy.URL

	stream := h.runChat(t, "assistant-suggestion", "", "Why has MyScheduler been failing recently?")
	if got := len(h.reasonerRequests()); got != 1 {
		t.Fatalf("reasoner calls=%d stream=%s", got, stream)
	}
	request := h.reasonerRequests()[0]
	packet, packetErr := evidencePacketFromReasonerRequest(request)
	if packetErr != nil {
		t.Fatal(packetErr)
	}
	raw := assistantSuggestionResponseBuilder(request)
	if _, parseErr := parseReasonerDraft(raw, packet); parseErr != nil {
		t.Fatalf("fixture output rejected: %v raw=%s", parseErr, raw)
	}
	if answer := phase2CStreamAnswer(t, stream); !strings.Contains(answer, "MyScheduler recorded recent failures") {
		t.Fatalf("normal answer missing: %q", answer)
	}
	suggestion, ok := actionSuggestionFromStream(t, stream)
	if !ok {
		t.Fatalf("safe action suggestion missing: %s", stream)
	}
	if suggestion.Kind != ActionKindRestartApplication ||
		suggestion.Target != (ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"}) ||
		suggestion.State != ActionStateAwaitingConfirmation ||
		!suggestion.ConfirmationRequired || suggestion.WarningRequired || !suggestion.SupportsExecution ||
		suggestion.Reason == "" {

		t.Fatalf("response suggestion=%+v", suggestion)
	}
	suggestionJSON, marshalErr := json.Marshal(suggestion)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, forbidden := range []string{`"origin"`, `"safety_class"`, `"policy_reason"`, `"arguments"`, `"command"`, `"url"`} {
		if strings.Contains(string(suggestionJSON), forbidden) {
			t.Fatalf("safe suggestion response exposed %s: %s", forbidden, suggestionJSON)
		}
	}

	entries := allHistoryEntries(t, h.app.store)
	if len(entries) != 1 || entries[0].Status != HistoryStatusSucceeded {
		t.Fatalf("History entries=%+v", entries)
	}
	record, err := h.app.store.getActionProposal(suggestion.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	assertAssistantRestartPolicy(t, record, entries[0].ID)
	assertActionEventTypes(t, mustActionEvents(t, h.app.store, suggestion.ActionID), ActionEventProposalCreated)
	if miniDeployCalls.Load() != 0 {
		t.Fatalf("chat contacted MiniDeploy %d times", miniDeployCalls.Load())
	}

	boundary, token := cryptographicAdminTestBoundary(t)
	restarter := &fakeMiniDeployRestarter{}
	handler := newAdminActionProductionTestHandlerWithRestarter(h.app.store, boundary, restarter)
	confirmedResponse := performAdminActionHTTPRequest(
		handler,
		http.MethodPost,
		testAdminOrigin+"/api/admin/actions/"+string(suggestion.ActionID)+"/confirm",
		`{"confirm":true}`,
		token,
		testAdminOrigin,
	)
	if confirmedResponse.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", confirmedResponse.Code, confirmedResponse.Body.String())
	}
	confirmed := decodeAdminActionHTTPResponse(t, confirmedResponse)
	if confirmed.State != ActionStateAuthorized || len(restarter.calls()) != 0 || miniDeployCalls.Load() != 0 {
		t.Fatalf("confirmation crossed execution boundary: action=%+v restarts=%v MiniDeploy=%d", confirmed, restarter.calls(), miniDeployCalls.Load())
	}
	assertActionEventTypes(
		t,
		mustActionEvents(t, h.app.store, suggestion.ActionID),
		ActionEventProposalCreated,
		ActionEventConfirmationGranted,
	)
}

func TestPhase2CDuplicateTopLevelActionSuggestionsPreserveAnswerWithoutAction(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	h.app.phase2Now = func() time.Time { return now }
	h.app.phase2Executor = productionApplicationIncidentExecutor(now)
	h.responseBuilder = func(request chatAPIRequest) string {
		packet, err := evidencePacketFromReasonerRequest(request)
		if err != nil {
			return "{}"
		}
		references := reasonerActionEvidenceReferenceEnum(packet)
		if len(references) == 0 {
			return "{}"
		}
		suggestion := validAssistantSuggestionJSON(references[0])
		return fmt.Sprintf(
			`{"conclusion":"MyScheduler recorded recent failures in the available evidence.","conclusion_evidence":[%q],"facts":[],"uncertainty":[{"text":"Recovery duration is not established by the available evidence.","evidence_ids":[%q]}],"action_suggestion":%s,"action_suggestion":%s}`,
			references[0],
			references[0],
			suggestion,
			suggestion,
		)
	}
	var miniDeployCalls atomic.Int64
	miniDeploy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		miniDeployCalls.Add(1)
		http.Error(w, "unexpected MiniDeploy call", http.StatusInternalServerError)
	}))
	defer miniDeploy.Close()
	h.app.minideployURL = miniDeploy.URL

	stream := h.runChat(t, "assistant-duplicate-suggestion", "", "Why has MyScheduler been failing recently?")
	requests := h.reasonerRequests()
	if len(requests) != 1 {
		t.Fatalf("reasoner calls=%d stream=%s", len(requests), stream)
	}
	packet, err := evidencePacketFromReasonerRequest(requests[0])
	if err != nil {
		t.Fatal(err)
	}
	if !reasonerActionSuggestionEligible(packet) {
		t.Fatalf("production packet was not suggestion-eligible: %+v", packet)
	}
	references := reasonerActionEvidenceReferenceEnum(packet)
	if len(references) == 0 {
		t.Fatalf("eligible evidence reference missing: %+v", packet)
	}
	confidence := packet.Confidence.SoftwareCeiling
	if confidence != ConfidenceHigh && confidence != ConfidenceMedium && confidence != ConfidenceLow {
		confidence = packet.Confidence.Level
	}
	if confidence != ConfidenceHigh && confidence != ConfidenceMedium && confidence != ConfidenceLow {
		confidence = ConfidenceLow
	}
	expectedDraft := ReasonerDraft{
		Conclusion:         "MyScheduler recorded recent failures in the available evidence.",
		ConclusionEvidence: []string{references[0]},
		Facts:              []ReasonerFact{},
		Uncertainty: []ReasonerFact{{
			Text:        "Recovery duration is not established by the available evidence.",
			EvidenceIDs: []string{references[0]},
		}},
	}
	expectedAnswer := renderReasonerAnswer(expectedDraft, confidence)
	if answer := phase2CStreamAnswer(t, stream); answer != expectedAnswer ||
		answer == safeReasonerLimitation() ||
		!strings.Contains(answer, "Uncertainty: Recovery duration is not established by the available evidence.") {

		t.Fatalf("normal answer was not preserved: got=%q want=%q stream=%s", answer, expectedAnswer, stream)
	}

	var done map[string]any
	for _, block := range strings.Split(stream, "\n\n") {
		if !strings.HasPrefix(block, "event: done\n") {
			continue
		}
		index := strings.Index(block, "data: ")
		if index < 0 {
			continue
		}
		if err := json.Unmarshal([]byte(block[index+6:]), &done); err != nil {
			t.Fatal(err)
		}
	}
	if done == nil {
		t.Fatalf("done event missing: %s", stream)
	}
	if got, _ := done["confidence"].(string); got != string(confidence) {
		t.Fatalf("confidence=%q want=%q done=%v", got, confidence, done)
	}
	if got, _ := done["answer_validation"].(string); got != "valid" {
		t.Fatalf("answer validation=%q done=%v", got, done)
	}
	if got, _ := done["reasoner_calls"].(float64); got != 1 {
		t.Fatalf("reasoner calls=%v done=%v", got, done)
	}
	if _, exposed := done["action_suggestion"]; exposed {
		t.Fatalf("duplicate suggestion was exposed: %v", done["action_suggestion"])
	}
	if got := assistantActionRowCount(t, h.app.store); got != 0 {
		t.Fatalf("duplicate suggestion persisted %d assistant actions", got)
	}
	var eventCount int
	if err := h.app.store.db.QueryRow(`SELECT COUNT(*) FROM action_events`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("duplicate suggestion created %d confirmation/execution events", eventCount)
	}
	if miniDeployCalls.Load() != 0 {
		t.Fatalf("duplicate suggestion contacted MiniDeploy %d times", miniDeployCalls.Load())
	}
}

func TestPhase2CNoSuggestionPreservesNormalInvestigation(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	h.app.phase2Now = func() time.Time { return now }
	h.app.phase2Executor = productionApplicationIncidentExecutor(now)
	stream := h.runChat(t, "assistant-no-suggestion", "", "Why has MyScheduler been failing recently?")
	if len(h.reasonerRequests()) != 1 || phase2CStreamAnswer(t, stream) == "" ||
		strings.Contains(stream, `"action_suggestion"`) || assistantActionRowCount(t, h.app.store) != 0 {

		t.Fatalf("no-suggestion investigation changed: %s", stream)
	}
}

func productionApplicationRuntimeTrendExecutor(now time.Time) applicationIncidentPipelineExecutor {
	executor := productionApplicationIncidentExecutor(now)

	contextResult := executor.values["get_app_context"].(reactorLabAppContextResult)
	contextResult.Deployment.Status = "running"
	for index := range contextResult.Deployment.Services {
		contextResult.Deployment.Services[index].Status = "running"
	}
	executor.values["get_app_context"] = contextResult

	applicationHistory := executor.values["read_application_history"].(reactorLabResolvedApplicationHistory)
	applicationHistory.Application.LatestStatus = "running"
	applicationHistory.Application.RestartCount = 0
	for index := range applicationHistory.History.Points {
		applicationHistory.History.Points[index].Status = "running"
		applicationHistory.History.Points[index].RestartCount = 0
	}
	executor.values["read_application_history"] = applicationHistory

	serviceHistory := executor.values["read_service_history"].(reactorLabServiceHistory)
	for serviceIndex := range serviceHistory.Services {
		for pointIndex := range serviceHistory.Services[serviceIndex].Points {
			serviceHistory.Services[serviceIndex].Points[pointIndex].Status = "running"
			serviceHistory.Services[serviceIndex].Points[pointIndex].Available = true
		}
	}
	executor.values["read_service_history"] = serviceHistory

	logLines := make([]string, 0, 12)
	for index := 0; index < 12; index++ {
		at := now.Add(-time.Duration(12-index) * 5 * time.Minute)
		logLines = append(logLines, at.Format(time.RFC3339Nano)+" INFO health request completed with status 200")
	}
	logs := strings.Join(logLines, "\n")
	executor.values["read_runtime_logs"] = logToolResponse{
		App: "myscheduler", Kind: "runtime", Lines: len(logLines), Bytes: len(logs),
		Logs: logs, Truncated: true,
	}
	return executor
}

func applicationRuntimeTrendNoSuggestionResponse(request chatAPIRequest) string {
	packet, err := evidencePacketFromReasonerRequest(request)
	if err != nil {
		return "{}"
	}
	currentID, historyID, runtimeID := "", "", ""
	for _, item := range packet.Evidence {
		switch item.Type {
		case EvidenceTypeCurrentApplication:
			if currentID == "" {
				currentID = item.ID
			}
		case EvidenceTypeApplicationHistory:
			if historyID == "" {
				historyID = item.ID
			}
		case EvidenceTypeRuntimeFailures:
			if runtimeID == "" {
				runtimeID = item.ID
			}
		}
	}
	conclusion := "MyScheduler is running in the available current evidence."
	conclusionID := currentID
	if historyID != "" {
		conclusion = "Available application evidence records no MyScheduler failures or restarts in the last 24 hours."
		conclusionID = historyID
	}
	if conclusionID == "" || runtimeID == "" {
		return "{}"
	}
	draft := ReasonerDraft{
		Conclusion:         conclusion,
		ConclusionEvidence: []string{conclusionID},
		Facts:              []ReasonerFact{},
		Uncertainty: []ReasonerFact{{
			Text:        "Runtime-log coverage was incomplete for the last 24 hours.",
			EvidenceIDs: []string{runtimeID},
		}},
	}
	encoded, _ := json.Marshal(draft)
	return string(encoded)
}

func TestPhase2CApplicationRuntimeTrendPartialLogsReturnsNormalAnswerWithoutAction(t *testing.T) {
	const question = "Has MyScheduler had any failures in the last 24 hours?"
	h := newPhase2CTestHarness(t, 12)
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	h.app.phase2Now = func() time.Time { return now }
	h.app.phase2Executor = productionApplicationRuntimeTrendExecutor(now)
	h.responseBuilder = applicationRuntimeTrendNoSuggestionResponse

	var miniDeployMutationCalls atomic.Int64
	miniDeploy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		miniDeployMutationCalls.Add(1)
		http.Error(w, "unexpected MiniDeploy mutation", http.StatusInternalServerError)
	}))
	defer miniDeploy.Close()
	h.app.minideployURL = miniDeploy.URL

	stream := h.runChat(t, "application-runtime-trend", "", question)
	requests := h.reasonerRequests()
	if len(requests) != 1 || len(h.plannerRequests()) != 0 || len(h.generations()) != 0 {
		t.Fatalf("reasoner=%d planner=%d direct=%d stream=%s",
			len(requests), len(h.plannerRequests()), len(h.generations()), stream)
	}
	packet, err := evidencePacketFromReasonerRequest(requests[0])
	if err != nil {
		t.Fatal(err)
	}
	if packet.Route != RouteApplicationPerformance || packet.Goal != GoalTrend ||
		packet.Temporal.Kind != TemporalNamedWindow || packet.Temporal.NamedRange != "24h" ||
		packet.Confidence.Level != ConfidenceMedium || packet.Confidence.Inputs.Coverage != CoveragePartial {

		t.Fatalf("reasoner packet=%+v", packet)
	}
	if !strings.Contains(requests[0].Messages[1].Content, "Runtime-log coverage was incomplete for the last 24 hours") {
		t.Fatalf("reasoner request omitted runtime limitation: %s", requests[0].Messages[1].Content)
	}
	answer := phase2CStreamAnswer(t, stream)
	if answer == "" || answer == safeReasonerLimitation() ||
		strings.Contains(answer, "I couldn't safely gather the specific ReactorLab evidence needed") ||
		!strings.Contains(answer, "last 24 hours") ||
		!strings.Contains(answer, "Uncertainty: Runtime-log coverage was incomplete") {

		t.Fatalf("qualified normal answer=%q stream=%s", answer, stream)
	}
	for _, marker := range []string{
		"\"answer_mode\":\"phase2c\"",
		"\"route\":\"application_performance_investigation\"",
		"\"model_invoked\":true",
		"\"reasoner_calls\":1",
		"\"evidence_rounds\":1",
		"\"second_round_reads\":0",
		"\"confidence\":\"Medium\"",
		"\"answer_validation\":\"valid\"",
	} {
		if !strings.Contains(stream, marker) {
			t.Fatalf("missing %s in stream=%s", marker, stream)
		}
	}
	if strings.Contains(stream, "unsupported_local_evidence") || strings.Contains(stream, "\"action_suggestion\"") {
		t.Fatalf("normal answer entered unsupported or action path: %s", stream)
	}
	starts := map[string]bool{}
	for _, event := range phase2CToolEvents(t, stream) {
		if event.Phase == "start" {
			starts[event.Name] = true
		}
	}
	for _, capability := range []string{
		"get_app_context", "read_host_history", "read_application_history",
		"read_service_history", "read_runtime_logs",
	} {
		if !starts[capability] {
			t.Fatalf("missing %s read: %v", capability, starts)
		}
	}
	if len(starts) != 5 || starts["read_deployment_history"] {
		t.Fatalf("unexpected evidence reads: %v", starts)
	}
	if got := assistantActionRowCount(t, h.app.store); got != 0 {
		t.Fatalf("normal answer persisted %d assistant actions", got)
	}
	var eventCount int
	if err := h.app.store.db.QueryRow("SELECT COUNT(*) FROM action_events").Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("normal answer created %d confirmation/execution events", eventCount)
	}
	if miniDeployMutationCalls.Load() != 0 {
		t.Fatalf("normal answer contacted MiniDeploy %d times", miniDeployMutationCalls.Load())
	}
}

func TestPhase2CApplicationRuntimeTrendIneligibleSuggestionDoesNotSuppressAnswer(t *testing.T) {
	const question = "Has MyScheduler had any failures in the last 24 hours?"
	h := newPhase2CTestHarness(t, 12)
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	h.app.phase2Now = func() time.Time { return now }
	executor := productionApplicationRuntimeTrendExecutor(now)
	executor.failures["read_application_history"] = errReactorLabUnavailable
	h.app.phase2Executor = executor
	h.responseBuilder = applicationRuntimeTrendNoSuggestionResponse

	stream := h.runChat(t, "application-runtime-trend-ineligible-action", "", question)
	requests := h.reasonerRequests()
	if len(requests) != 1 {
		t.Fatalf("reasoner calls=%d stream=%s", len(requests), stream)
	}
	packet, err := evidencePacketFromReasonerRequest(requests[0])
	if err != nil {
		t.Fatal(err)
	}
	if packet.Confidence.Level != ConfidenceLow ||
		packet.Confidence.Inputs.CriticalCompleteness != CompletenessMissing ||
		reasonerActionSuggestionEligible(packet) {

		t.Fatalf("suggestion unexpectedly eligible: %+v", packet.Confidence)
	}
	if answer := phase2CStreamAnswer(t, stream); answer == "" || answer == safeReasonerLimitation() {
		t.Fatalf("ineligible suggestion suppressed normal answer: %q stream=%s", answer, stream)
	}
	if !strings.Contains(stream, "\"model_invoked\":true") ||
		!strings.Contains(stream, "\"answer_validation\":\"valid\"") ||
		strings.Contains(stream, "\"action_suggestion\"") ||
		assistantActionRowCount(t, h.app.store) != 0 {

		t.Fatalf("ineligible suggestion changed normal answer or persisted action: %s", stream)
	}
	var eventCount int
	if err := h.app.store.db.QueryRow("SELECT COUNT(*) FROM action_events").Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("ineligible suggestion created %d confirmation/execution events", eventCount)
	}
}

func TestStandaloneFreeFormActionLanguageCannotConfirmPendingSuggestion(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	history, _, err := h.app.store.startHistoryEntry("", "trusted investigation", actionTestTime())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := h.app.store.createAssistantRestartSuggestion(
		history.ID,
		ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var miniDeployCalls atomic.Int64
	miniDeploy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		miniDeployCalls.Add(1)
		http.Error(w, "unexpected MiniDeploy call", http.StatusInternalServerError)
	}))
	defer miniDeploy.Close()
	h.app.minideployURL = miniDeploy.URL

	for index, message := range []string{
		"Restart MyScheduler if needed.",
		"Fix it automatically.",
		"Do whatever you need.",
	} {
		stream := h.runChat(t, fmt.Sprintf("standalone-action-language-%d", index), "", message)
		if strings.Contains(stream, `"action_suggestion"`) {
			t.Fatalf("standalone action language created a suggestion: %s", stream)
		}
		loaded, err := h.app.store.getActionProposal(pending.Proposal.ID())
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Proposal.State() != ActionStateAwaitingConfirmation {
			t.Fatalf("free-form language confirmed action: %+v", loaded)
		}
		assertActionEventTypes(t, mustActionEvents(t, h.app.store, pending.Proposal.ID()), ActionEventProposalCreated)
		if got := assistantActionRowCount(t, h.app.store); got != 1 {
			t.Fatalf("standalone language persisted %d assistant actions", got)
		}
	}
	if miniDeployCalls.Load() != 0 {
		t.Fatalf("standalone action language contacted MiniDeploy %d times", miniDeployCalls.Load())
	}
}

func TestPhase2CFreeFormUserLanguageNeverConfirmsOrExecutesSuggestion(t *testing.T) {
	questions := []string{
		"Investigate why MyScheduler is unhealthy and start it again if needed.",
		"Investigate why MyScheduler is unhealthy and fix it automatically.",
		"Investigate why MyScheduler is unhealthy and do whatever you need.",
	}
	for _, question := range questions {
		t.Run(question, func(t *testing.T) {
			h := newPhase2CTestHarness(t, 12)
			now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
			h.app.phase2Now = func() time.Time { return now }
			h.app.phase2Executor = productionApplicationIncidentExecutor(now)
			h.responseBuilder = assistantSuggestionResponseBuilder
			stream := h.runChat(t, "assistant-free-form", "", question)
			suggestion, ok := actionSuggestionFromStream(t, stream)
			if !ok {
				t.Fatalf("suggestion missing: %s", stream)
			}
			record, err := h.app.store.getActionProposal(suggestion.ActionID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Proposal.State() != ActionStateAwaitingConfirmation {
				t.Fatalf("free-form text authorized action: %+v", record)
			}
			assertActionEventTypes(t, mustActionEvents(t, h.app.store, suggestion.ActionID), ActionEventProposalCreated)
		})
	}
}

func assistantSuggestionResponseBuilder(request chatAPIRequest) string {
	packet, err := evidencePacketFromReasonerRequest(request)
	if err != nil {
		return "{}"
	}
	reference := ""
	if references := reasonerActionEvidenceReferenceEnum(packet); len(references) > 0 {
		reference = references[0]
	}
	if reference == "" {
		return "{}"
	}
	draft := ReasonerDraft{
		Conclusion:         "MyScheduler recorded recent failures in the available evidence.",
		ConclusionEvidence: []string{reference}, Facts: []ReasonerFact{}, Uncertainty: []ReasonerFact{},
		ActionSuggestion: json.RawMessage(validAssistantSuggestionJSON(reference)),
	}
	encoded, _ := json.Marshal(draft)
	return string(encoded)
}

func actionSuggestionFromStream(t *testing.T, stream string) (assistantActionSuggestionResponse, bool) {
	t.Helper()
	for _, block := range strings.Split(stream, "\n\n") {
		if !strings.HasPrefix(block, "event: done\n") {
			continue
		}
		index := strings.Index(block, "data: ")
		if index < 0 {
			continue
		}
		var envelope struct {
			ActionSuggestion *assistantActionSuggestionResponse `json:"action_suggestion"`
		}
		if err := json.Unmarshal([]byte(block[index+6:]), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.ActionSuggestion != nil {
			return *envelope.ActionSuggestion, true
		}
	}
	return assistantActionSuggestionResponse{}, false
}

func assertAssistantRestartPolicy(t *testing.T, record storedActionProposal, historyID string) {
	t.Helper()
	proposal := record.Proposal
	definition, ok := phase4ActionCatalog().lookup(proposal.Kind())
	decision, err := phase4ActionCatalog().evaluate(proposal)
	if !ok || err != nil {
		t.Fatalf("action policy unavailable: found=%t err=%v", ok, err)
	}
	if proposal.Kind() != ActionKindRestartApplication ||
		proposal.Origin() != ActionOriginAssistantSuggested ||
		proposal.Target() != (ActionTarget{Type: ActionTargetApplication, ID: "myscheduler"}) ||
		proposal.State() != ActionStateAwaitingConfirmation ||
		definition.SafetyClass != ActionSafetyRecoverable ||
		!decision.ConfirmationRequired || decision.WarningRequired ||
		!record.SupportsExecution || record.HistoryEntryID != historyID {

		t.Fatalf("assistant restart policy mismatch: record=%+v definition=%+v decision=%+v", record, definition, decision)
	}
	if _, ok := proposal.Arguments().(RestartApplicationArguments); !ok {
		t.Fatalf("restart arguments=%T", proposal.Arguments())
	}
}

func assistantActionRowCount(t *testing.T, store *chatStore) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM action_proposals WHERE origin = ?`, ActionOriginAssistantSuggested).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func mustActionEvents(t *testing.T, store *chatStore, id ActionID) []ActionEvent {
	t.Helper()
	events, err := store.listActionEvents(id)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestInvalidSuggestionPersistenceNeverCreatesAction(t *testing.T) {
	packet := testAssistantSuggestionPacket()
	store := newActionTestStore(t)
	a := &app{store: store}
	for index, suggestionJSON := range []string{
		`{"type":"rollback_deployment","reason":"Bounded reason.","evidence_ids":["evidence-1"]}`,
		`{"type":"restart_current_application","target":{"type":"application","id":"another-app"},"reason":"Bounded reason.","evidence_ids":["evidence-1"]}`,
		`{"type":"restart_current_application","state":"authorized","reason":"Bounded reason.","evidence_ids":["evidence-1"]}`,
	} {
		history, _, err := store.startHistoryEntry("", fmt.Sprintf("invalid suggestion %d", index), actionTestTime().Add(time.Duration(index)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		draft, err := parseReasonerDraft(assistantReasonerJSON(suggestionJSON), packet)
		if err != nil {
			t.Fatal(err)
		}
		if response, err := a.persistReasonerActionSuggestion(history.ID, draft, packet); !errors.Is(err, errInvalidReasonerActionSuggestion) || response != nil {

			t.Fatalf("response=%+v err=%v", response, err)
		}
	}
	if got := assistantActionRowCount(t, store); got != 0 {
		t.Fatalf("invalid suggestions persisted %d actions", got)
	}
}
