package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func productionExitedApplicationIncidentExecutor(now time.Time) applicationIncidentPipelineExecutor {
	executor := productionApplicationIncidentExecutor(now)
	contextResult := executor.values["get_app_context"].(reactorLabAppContextResult)
	contextResult.Deployment.Status = "exited"
	executor.values["get_app_context"] = contextResult
	return executor
}

func TestReasonerActionSuggestionRequiresRestartWorthyTypedCurrentState(t *testing.T) {
	validRaw := json.RawMessage(validAssistantSuggestionJSON("evidence-1"))
	history := func(packet *EvidencePacket) EvidencePacketItem {
		return EvidencePacketItem{
			ID: "history-1", Kind: EvidenceObserved, Type: EvidenceTypeApplicationHistory,
			Subject: packet.Subjects[0], Fields: []EvidenceField{evidenceStringField("latest_status", "failed")},
		}
	}
	runtime := func(packet *EvidencePacket) EvidencePacketItem {
		return EvidencePacketItem{
			ID: "runtime-1", Kind: EvidenceObserved, Type: EvidenceTypeRuntimeFailures,
			Subject: packet.Subjects[0], Fields: []EvidenceField{evidenceStringField("failure", "recorded")},
		}
	}
	tests := []struct {
		name   string
		mutate func(*EvidencePacket)
	}{
		{"current running", func(packet *EvidencePacket) {
			packet.Evidence[0].Fields = []EvidenceField{evidenceStringField("status", "running")}
		}},
		{"running with historical failures", func(packet *EvidencePacket) {
			packet.Evidence[0].Fields = []EvidenceField{evidenceStringField("status", "running")}
			packet.Evidence = append(packet.Evidence, history(packet))
		}},
		{"running with runtime failures", func(packet *EvidencePacket) {
			packet.Evidence[0].Fields = []EvidenceField{evidenceStringField("status", "running")}
			packet.Evidence = append(packet.Evidence, runtime(packet))
		}},
		{"missing current status", func(packet *EvidencePacket) {
			packet.Evidence = []EvidencePacketItem{history(packet), runtime(packet)}
		}},
		{"empty current status", func(packet *EvidencePacket) {
			packet.Evidence[0].Fields = []EvidenceField{evidenceStringField("status", "  ")}
		}},
		{"unknown current status", func(packet *EvidencePacket) {
			packet.Evidence[0].Fields = []EvidenceField{evidenceStringField("status", "degraded")}
		}},
		{"untyped current status", func(packet *EvidencePacket) {
			value := int64(1)
			packet.Evidence[0].Fields = []EvidenceField{{
				Name: "status", Value: EvidenceValue{Kind: EvidenceValueInteger, Integer: &value},
			}}
		}},
		{"conflicting current state", func(packet *EvidencePacket) {
			packet.Evidence = append(packet.Evidence, EvidencePacketItem{
				ID: "evidence-2", Kind: EvidenceObserved, Type: EvidenceTypeCurrentApplication,
				Subject: packet.Subjects[0], Fields: []EvidenceField{evidenceStringField("status", "running")},
			})
		}},
		{"wrong target current state", func(packet *EvidencePacket) {
			packet.Evidence[0].Subject = EvidenceSubject{Kind: SubjectApplication, ID: "golf-mullet"}
		}},
		{"additional wrong target current state", func(packet *EvidencePacket) {
			packet.Evidence = append(packet.Evidence, EvidencePacketItem{
				ID: "evidence-2", Kind: EvidenceObserved, Type: EvidenceTypeCurrentApplication,
				Subject: EvidenceSubject{Kind: SubjectApplication, ID: "golf-mullet"},
				Fields:  []EvidenceField{evidenceStringField("status", "exited")},
			})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packet := testAssistantSuggestionPacket()
			test.mutate(&packet)
			if reasonerActionSuggestionEligible(packet) {
				t.Fatalf("unsafe packet was suggestion-eligible: %+v", packet.Evidence)
			}
			if references := reasonerActionEvidenceReferenceEnum(packet); len(references) != 0 {
				t.Fatalf("unsafe packet exposed action references: %v", references)
			}
			if suggestion, err := validateReasonerActionSuggestion(validRaw, packet); !errors.Is(err, errInvalidReasonerActionSuggestion) || suggestion != nil {
				t.Fatalf("unsafe suggestion=%+v err=%v", suggestion, err)
			}
		})
	}
}

func TestReasonerActionSuggestionReferencesOnlyRestartTriggerEvidence(t *testing.T) {
	packet := testAssistantSuggestionPacket()
	packet.Evidence[0].Fields = []EvidenceField{evidenceStringField("status", " EXITED ")}
	packet.Evidence = append(packet.Evidence, EvidencePacketItem{
		ID: "history-1", Kind: EvidenceObserved, Type: EvidenceTypeApplicationHistory,
		Subject: packet.Subjects[0], Fields: []EvidenceField{evidenceStringField("latest_status", "failed")},
	})
	if !reasonerActionSuggestionEligible(packet) {
		t.Fatal("exact normalized exited state was not suggestion-eligible")
	}
	if references := reasonerActionEvidenceReferenceEnum(packet); len(references) != 1 || references[0] != "evidence-1" {
		t.Fatalf("action references=%v want only current-state trigger", references)
	}
	raw := json.RawMessage(validAssistantSuggestionJSON("history-1"))
	if suggestion, err := validateReasonerActionSuggestion(raw, packet); !errors.Is(err, errInvalidReasonerActionSuggestion) || suggestion != nil {
		t.Fatalf("historical-only suggestion=%+v err=%v", suggestion, err)
	}
}

func TestReasonerActionSuggestionAcceptsConsistentTargetCurrentEvidence(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*EvidencePacket)
		wantRefs int
	}{
		{
			name: "two matching exited observations",
			mutate: func(packet *EvidencePacket) {
				packet.Evidence = append(packet.Evidence, EvidencePacketItem{
					ID: "evidence-2", Kind: EvidenceObserved, Type: EvidenceTypeCurrentApplication,
					Subject: packet.Subjects[0], Fields: []EvidenceField{evidenceStringField("status", "exited")},
				})
			},
			wantRefs: 2,
		},
		{
			name: "compacted current subject",
			mutate: func(packet *EvidencePacket) {
				packet.Evidence[0].Subject = EvidenceSubject{}
			},
			wantRefs: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packet := testAssistantSuggestionPacket()
			test.mutate(&packet)
			if !reasonerActionSuggestionEligible(packet) {
				t.Fatalf("consistent packet was not suggestion-eligible: %+v", packet.Evidence)
			}
			references := reasonerActionEvidenceReferenceEnum(packet)
			if len(references) != test.wantRefs {
				t.Fatalf("action references=%v want %d", references, test.wantRefs)
			}
			raw := json.RawMessage(validAssistantSuggestionJSON(references[0]))
			suggestion, err := validateReasonerActionSuggestion(raw, packet)
			if err != nil || suggestion == nil {
				t.Fatalf("consistent suggestion=%+v err=%v", suggestion, err)
			}
		})
	}
}

func forcedAssistantSuggestionResponseBuilder(request chatAPIRequest) string {
	packet, err := evidencePacketFromReasonerRequest(request)
	if err != nil {
		return "{}"
	}
	reference := ""
	for _, item := range packet.Evidence {
		if item.Type == EvidenceTypeCurrentApplication {
			reference = item.ID
			break
		}
	}
	if reference == "" {
		return "{}"
	}
	draft := ReasonerDraft{
		Conclusion:         "MyScheduler is running in the available current evidence.",
		ConclusionEvidence: []string{reference},
		Facts:              []ReasonerFact{},
		Uncertainty:        []ReasonerFact{},
		ActionSuggestion:   json.RawMessage(validAssistantSuggestionJSON(reference)),
	}
	encoded, _ := json.Marshal(draft)
	return string(encoded)
}

func TestPhase2CCurrentRunningAppDiscardsRestartSuggestionWithoutLosingAnswer(t *testing.T) {
	h := newPhase2CTestHarness(t, 12)
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	h.app.phase2Now = func() time.Time { return now }
	h.app.phase2Executor = productionApplicationIncidentExecutor(now)
	h.responseBuilder = forcedAssistantSuggestionResponseBuilder
	var miniDeployCalls atomic.Int64
	miniDeploy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		miniDeployCalls.Add(1)
		http.Error(w, "unexpected MiniDeploy call", http.StatusInternalServerError)
	}))
	defer miniDeploy.Close()
	h.app.minideployURL = miniDeploy.URL

	stream := h.runChat(t, "running-app-suggestion", "", "Is MyScheduler healthy right now, and what should be done next?")
	if got := len(h.reasonerRequests()); got != 1 {
		t.Fatalf("reasoner calls=%d stream=%s", got, stream)
	}
	packet, err := evidencePacketFromReasonerRequest(h.reasonerRequests()[0])
	if err != nil {
		t.Fatal(err)
	}
	if reasonerActionSuggestionEligible(packet) {
		t.Fatalf("running application was suggestion-eligible: %+v", packet.Evidence)
	}
	answer := phase2CStreamAnswer(t, stream)
	if !strings.Contains(answer, "MyScheduler is running") || answer == safeReasonerLimitation() ||
		!strings.Contains(stream, `"answer_validation":"valid"`) {

		t.Fatalf("unsafe optional suggestion damaged the answer: answer=%q stream=%s", answer, stream)
	}
	if _, ok := actionSuggestionFromStream(t, stream); ok || strings.Contains(stream, `"action_suggestion"`) {
		t.Fatalf("running application exposed an action suggestion: %s", stream)
	}
	if got := assistantActionRowCount(t, h.app.store); got != 0 {
		t.Fatalf("running application persisted %d assistant actions", got)
	}
	var eventCount int
	if err := h.app.store.db.QueryRow("SELECT COUNT(*) FROM action_events").Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 || miniDeployCalls.Load() != 0 {
		t.Fatalf("running application events=%d MiniDeploy calls=%d", eventCount, miniDeployCalls.Load())
	}
}
