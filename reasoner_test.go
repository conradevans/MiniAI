package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testReasonerPacket() EvidencePacket {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	return EvidencePacket{
		SchemaVersion: evidencePacketSchemaVersion,
		Route:         RouteCurrentPlatformHealth,
		Goal:          GoalCurrentAssessment,
		Subjects:      []EvidenceSubject{{Kind: SubjectHost, ID: "dell", Name: "Dell"}},
		Temporal:      TemporalScope{Kind: TemporalNow, At: &now, Valid: true},
		Sources: []EvidencePacketSource{{
			ID: "source-1", Capability: "get_platform_overview", CollectedAt: now,
			Availability: AvailabilityAvailable,
		}},
		Evidence: []EvidencePacketItem{{
			ID: "evidence-1", Kind: EvidenceObserved, Type: EvidenceTypeCurrentPlatformState,
			Subject:  EvidenceSubject{Kind: SubjectHost, ID: "dell", Name: "Dell"},
			SourceID: "source-1", RequirementID: "requirement-1", Criticality: CriticalityCritical,
			Fields: []EvidenceField{evidenceStringField("status", "healthy")},
		}},
		Confidence: EvidencePacketConfidence{Level: ConfidenceHigh, SoftwareCeiling: ConfidenceHigh},
	}
}

func validReasonerJSON() string {
	return `{"conclusion":"The Dell looks healthy right now.","conclusion_evidence":["evidence-1"],"facts":[{"text":"Current platform state reports healthy.","evidence_ids":["evidence-1"]}],"uncertainty":[]}`
}

func TestParseReasonerDraftValidAndRenderSoftwareConfidence(t *testing.T) {
	draft, err := parseReasonerDraft(validReasonerJSON(), testReasonerPacket())
	if err != nil {
		t.Fatal(err)
	}
	answer := renderReasonerAnswer(draft, ConfidenceMedium)
	if !strings.Contains(answer, "Confidence: Medium.") || strings.Count(answer, "Confidence:") != 1 || strings.Contains(answer, "%") {
		t.Fatalf("rendered answer=%q", answer)
	}
}

func TestReasonerDraftValidationRejectsInvalidOutput(t *testing.T) {
	packet := testReasonerPacket()
	longConclusion := strings.Repeat("x", reasonerConclusionMaxRunes+1)
	tests := []struct {
		name string
		raw  string
	}{
		{"missing conclusion", `{"conclusion":"","conclusion_evidence":["evidence-1"],"facts":[],"uncertainty":[]}`},
		{"unexpected field", `{"conclusion":"ok","conclusion_evidence":["evidence-1"],"facts":[],"uncertainty":[],"confidence":"High"}`},
		{"invalid reference", `{"conclusion":"ok","conclusion_evidence":["made-up"],"facts":[],"uncertainty":[]}`},
		{"too many facts", `{"conclusion":"ok","conclusion_evidence":["evidence-1"],"facts":[{"text":"a","evidence_ids":["evidence-1"]},{"text":"b","evidence_ids":["evidence-1"]},{"text":"c","evidence_ids":["evidence-1"]},{"text":"d","evidence_ids":["evidence-1"]}],"uncertainty":[]}`},
		{"overlong conclusion", `{"conclusion":"` + longConclusion + `","conclusion_evidence":["evidence-1"],"facts":[],"uncertainty":[]}`},
		{"malformed", `{"conclusion":`},
		{"think", `{"conclusion":"<think>secret</think> Fine.","conclusion_evidence":["evidence-1"],"facts":[],"uncertainty":[]}`},
		{"confidence text", `{"conclusion":"Fine. Confidence: High","conclusion_evidence":["evidence-1"],"facts":[],"uncertainty":[]}`},
		{"confidence percentage", `{"conclusion":"I am 87% confident it is fine.","conclusion_evidence":["evidence-1"],"facts":[],"uncertainty":[]}`},
		{"tool request", `{"conclusion":"I will check with a tool.","conclusion_evidence":["evidence-1"],"facts":[],"uncertainty":[]}`},
		{"planning", `{"conclusion":"My plan is to inspect more evidence.","conclusion_evidence":["evidence-1"],"facts":[],"uncertainty":[]}`},
		{"capability narration", `{"conclusion":"get_platform_overview says healthy.","conclusion_evidence":["evidence-1"],"facts":[],"uncertainty":[]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if draft, err := parseReasonerDraft(test.raw, packet); err == nil {
				t.Fatalf("accepted invalid draft: %+v", draft)
			}
		})
	}
}

func TestReasonerCompletionValidation(t *testing.T) {
	if err := validateReasonerCompletion(chatAPIResponse{Done: true, DoneReason: "stop"}); err != nil {
		t.Fatalf("completed response rejected: %v", err)
	}
	if err := validateReasonerCompletion(chatAPIResponse{Done: true, DoneReason: "length"}); err != errReasonerOutputLimitReached ||
		reasonerCompletionValidationStatus(err) != "output_limit" || safeReasonerValidationError(err) != "output limit reached" {
		t.Fatalf("length completion classification=%q err=%v", reasonerCompletionValidationStatus(err), err)
	}
	if err := validateReasonerCompletion(chatAPIResponse{}); err != errReasonerResponseIncomplete ||
		reasonerCompletionValidationStatus(err) != "incomplete" || safeReasonerValidationError(err) != "incomplete response" {
		t.Fatalf("incomplete response classification=%q err=%v", reasonerCompletionValidationStatus(err), err)
	}
}

func TestReasonerAcceptsRepresentativeStructuredAnswerBeyondOldEnvelope(t *testing.T) {
	draft := ReasonerDraft{
		Conclusion:         strings.Repeat("The bounded evidence records the observed application state. ", 7),
		ConclusionEvidence: []string{"evidence-1"},
		Facts: []ReasonerFact{
			{Text: strings.Repeat("Recorded measurements remain within the observed bounds. ", 4), EvidenceIDs: []string{"evidence-1"}},
			{Text: strings.Repeat("The current state and historical samples are represented separately. ", 3), EvidenceIDs: []string{"evidence-1"}},
			{Text: strings.Repeat("The evidence packet preserves the bounded observation window. ", 3), EvidenceIDs: []string{"evidence-1"}},
		},
		Uncertainty: []ReasonerFact{{
			Text:        strings.Repeat("The packet does not include evidence outside the bounded window. ", 3),
			EvidenceIDs: []string{"evidence-1"},
		}},
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 1024 {
		t.Fatalf("representative structured response is too small to exercise added output headroom: %d bytes", len(raw))
	}
	if _, err := parseReasonerDraft(string(raw), testReasonerPacket()); err != nil {
		t.Fatalf("representative structured response rejected: %v", err)
	}
}

func TestReasonerRejectsUnsupportedDeploymentCausality(t *testing.T) {
	packet := testReasonerPacket()
	packet.Route = RouteDeploymentCorrelation
	raw := `{"conclusion":"The deployment caused the outage.","conclusion_evidence":["evidence-1"],"facts":[],"uncertainty":[]}`
	if _, err := parseReasonerDraft(raw, packet); err == nil {
		t.Fatal("accepted causal certainty from correlation evidence")
	}
}

func reasonerJSONWithConclusion(conclusion string) string {
	draft := ReasonerDraft{
		Conclusion: conclusion, ConclusionEvidence: []string{"evidence-1"},
		Facts: []ReasonerFact{}, Uncertainty: []ReasonerFact{},
	}
	encoded, _ := json.Marshal(draft)
	return string(encoded)
}

func TestReasonerRawShapeRequiresExactNonNullSchema(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"conclusion": "Bounded result.", "conclusion_evidence": []string{"evidence-1"},
			"facts": []any{}, "uncertainty": []any{},
		}
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing conclusion evidence", mutate: func(value map[string]any) { delete(value, "conclusion_evidence") }},
		{name: "null conclusion evidence", mutate: func(value map[string]any) { value["conclusion_evidence"] = []any(nil) }},
		{name: "missing facts", mutate: func(value map[string]any) { delete(value, "facts") }},
		{name: "null facts", mutate: func(value map[string]any) { value["facts"] = []any(nil) }},
		{name: "missing uncertainty", mutate: func(value map[string]any) { delete(value, "uncertainty") }},
		{name: "null uncertainty", mutate: func(value map[string]any) { value["uncertainty"] = []any(nil) }},
		{name: "extra top field", mutate: func(value map[string]any) { value["status"] = "healthy" }},
		{name: "nested missing evidence ids", mutate: func(value map[string]any) {
			value["facts"] = []any{map[string]any{"text": "Observed."}}
		}},
		{name: "nested null evidence ids", mutate: func(value map[string]any) {
			value["facts"] = []any{map[string]any{"text": "Observed.", "evidence_ids": []any(nil)}}
		}},
		{name: "nested extra field", mutate: func(value map[string]any) {
			value["uncertainty"] = []any{map[string]any{
				"text": "Unknown.", "evidence_ids": []string{"evidence-1"}, "status": "unknown",
			}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := base()
			test.mutate(value)
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if draft, err := parseReasonerDraft(string(raw), testReasonerPacket()); err == nil {
				t.Fatalf("accepted invalid raw shape: %+v", draft)
			}
		})
	}
	for _, raw := range []string{
		"{\"conclusion\":\"one\",\"conclusion\":\"two\",\"conclusion_evidence\":[\"evidence-1\"],\"facts\":[],\"uncertainty\":[]}",
		"{\"conclusion\":\"one\",\"conclusion_evidence\":[\"evidence-1\"],\"facts\":[{\"text\":\"a\",\"text\":\"b\",\"evidence_ids\":[\"evidence-1\"]}],\"uncertainty\":[]}",
	} {
		if draft, err := parseReasonerDraft(raw, testReasonerPacket()); err == nil {
			t.Fatalf("accepted duplicate-key raw shape: %+v", draft)
		}
	}
}

func TestReasonerRejectsModelAuthoredConfidenceParaphrases(t *testing.T) {
	for _, conclusion := range []string{
		"There is an 87% chance the host is healthy.",
		"The chance of health is 87%.",
		"There is an 87 percent probability of health.",
		"This is almost certainly healthy.",
		"This is definitely healthy.",
		"This is probably healthy.",
		"The probability is high.",
		"The odds strongly favor health.",
		"Health is highly likely.",
		"The certainty rating is high.",
		"I am quite sure the host is healthy.",
	} {
		t.Run(conclusion, func(t *testing.T) {
			if draft, err := parseReasonerDraft(reasonerJSONWithConclusion(conclusion), testReasonerPacket()); err == nil {
				t.Fatalf("accepted model-authored confidence: %+v", draft)
			}
		})
	}
	if _, err := parseReasonerDraft(
		reasonerJSONWithConclusion("CPU utilization is 87% in the observed sample."),
		testReasonerPacket(),
	); err != nil {
		t.Fatalf("ordinary metric percentage was rejected: %v", err)
	}
}

func TestReasonerRejectsBroadDefinitiveDeploymentCausality(t *testing.T) {
	packet := testReasonerPacket()
	packet.Route = RouteDeploymentCorrelation
	for _, conclusion := range []string{
		"The rollout caused the outage.",
		"The deployment led to the outage.",
		"The deployment resulted in the failure.",
		"The deployment is responsible for the outage.",
		"The release triggered the crash.",
		"The update produced the failure.",
		"The deployment created the outage.",
		"The release brought about the incident.",
		"The deployment made the service fail.",
		"The outage was caused by the rollout.",
	} {
		t.Run(conclusion, func(t *testing.T) {
			if draft, err := parseReasonerDraft(reasonerJSONWithConclusion(conclusion), packet); err == nil {
				t.Fatalf("accepted definitive causality: %+v", draft)
			}
		})
	}
	for _, conclusion := range []string{
		"The evidence does not establish that the deployment caused the outage.",
		"The deployment may have caused the outage, but the evidence does not establish it.",
		"It is possible that the release triggered the crash.",
	} {
		t.Run("allowed uncertainty "+conclusion, func(t *testing.T) {
			if _, err := parseReasonerDraft(reasonerJSONWithConclusion(conclusion), packet); err != nil {
				t.Fatalf("rejected bounded causal uncertainty: %v", err)
			}
		})
	}
	direct := true
	packet.Evidence[0].Fields = append(packet.Evidence[0].Fields, EvidenceField{
		Name:  "direct_causal_link",
		Value: EvidenceValue{Kind: EvidenceValueBoolean, Boolean: &direct},
	})
	if _, err := parseReasonerDraft(reasonerJSONWithConclusion("The rollout caused the outage."), packet); err != nil {
		t.Fatalf("explicit direct-causality evidence was ignored: %v", err)
	}
}

func TestReasonerRejectsNarrationAndWholeCodeContainers(t *testing.T) {
	for _, conclusion := range []string{
		"Let me analyze the evidence.",
		"Let me think about the evidence.",
		"Let me inspect the packet.",
		"I need to analyze the evidence.",
		"I need to inspect the source.",
		"I need to call a capability.",
		"The user is asking about health.",
		"The user wants a diagnosis.",
		"My plan is to inspect the host.",
		"First I will select a tool.",
		"{\"status\":\"healthy\"}",
		"[{\"fact\":\"healthy\"}]",
		string([]byte{96, 96, 96}) + "json\n{\"status\":\"healthy\"}\n" + string([]byte{96, 96, 96}),
	} {
		t.Run(conclusion, func(t *testing.T) {
			if draft, err := parseReasonerDraft(reasonerJSONWithConclusion(conclusion), testReasonerPacket()); err == nil {
				t.Fatalf("accepted narration/container: %+v", draft)
			}
		})
	}
	if _, err := parseReasonerDraft(
		reasonerJSONWithConclusion("The log line included {status} as an incidental placeholder."),
		testReasonerPacket(),
	); err != nil {
		t.Fatalf("ordinary incidental braces were rejected: %v", err)
	}
}

func TestReasonerRequestIsDeterministicCompactAndToolFree(t *testing.T) {
	packet := testReasonerPacket()
	packetJSON, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	a := &app{}
	first := a.buildReasonerRequest(primaryModel, "Is the Dell healthy right now?", packet, packetJSON)
	second := a.buildReasonerRequest(primaryModel, "Is the Dell healthy right now?", packet, packetJSON)
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("reasoner requests differ\n%s\n%s", firstJSON, secondJSON)
	}
	if reasonerNumPredict != 512 || len(first.Tools) != 0 || first.Think || first.Stream || first.Options["num_ctx"] != reasonerNumContext || first.Options["num_predict"] != reasonerNumPredict {
		t.Fatalf("request contract=%+v", first)
	}
	if len(first.Messages) != 2 || first.Messages[0].Role != "system" || first.Messages[1].Role != "user" {
		t.Fatalf("messages=%+v", first.Messages)
	}
	joined := first.Messages[0].Content + first.Messages[1].Content
	for _, absent := range []string{"CURRENT APP CONTEXT", "Tool gathering is complete", "read-only tools for current and historical", `"tools"`} {
		if strings.Contains(joined, absent) {
			t.Fatalf("reasoner input contains legacy planner material %q", absent)
		}
	}
	if !strings.Contains(first.Messages[1].Content, "Is the Dell healthy right now?") || !strings.Contains(first.Messages[1].Content, `"schemaVersion":"phase2b.packet.v1"`) {
		t.Fatalf("reasoner user input=%q", first.Messages[1].Content)
	}
	if len(first.Messages[0].Content) > 800 {
		t.Fatalf("system instruction is not short: %d bytes", len(first.Messages[0].Content))
	}
	lowerInstruction := strings.ToLower(first.Messages[0].Content)
	for _, required := range []string{
		"do not state confidence", "probability", "likelihood", "certainty", "likely", "probable", "probably",
		"certain", "certainly", "definite", "definitely", "confident", "sure", "odds",
		"percentage confidence", "put unresolved evidence limits in uncertainty", "software renders confidence separately",
	} {
		if !strings.Contains(lowerInstruction, required) {
			t.Fatalf("reasoner instruction does not make %q explicit: %q", required, first.Messages[0].Content)
		}
	}
	format := first.Format.(map[string]any)
	if format["additionalProperties"] != false {
		t.Fatalf("schema is not strict: %+v", format)
	}
}

func TestReasonerRequestInstructionsUseResolvedTemporalScope(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		question   string
		temporal   TemporalScope
		want       string
		mustNotSay string
	}{
		{
			name:     "inferred 24h",
			question: "Why has MyScheduler been failing recently?",
			temporal: parseTemporalScope("Why has MyScheduler been failing recently?", now, time.UTC),
			want:     "The resolved temporal window is the last 24 hours.",
		},
		{
			name:       "explicit 3-day window",
			question:   "Why has MyScheduler been failing in the last 3 days?",
			temporal:   TemporalScope{Kind: TemporalNamedWindow, NamedRange: "3d", Valid: true},
			want:       "The resolved temporal window is the last 3 days.",
			mustNotSay: "the last 24 hours",
		},
		{
			name:       "no temporal scope",
			question:   "Explain MyScheduler's configuration.",
			temporal:   TemporalScope{Kind: TemporalUnspecified, Valid: true},
			want:       "No temporal window is resolved. Do not invent a time window",
			mustNotSay: "the last 24 hours",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packet := testReasonerPacket()
			packet.Temporal = test.temporal
			packetJSON, err := json.Marshal(packet)
			if err != nil {
				t.Fatal(err)
			}
			request := (&app{}).buildReasonerRequest(primaryModel, test.question, packet, packetJSON)
			if len(request.Messages) != 2 || !strings.Contains(request.Messages[1].Content, test.want) {
				t.Fatalf("reasoner request does not expose resolved scope %q: %+v", test.want, request.Messages)
			}
			if test.mustNotSay != "" && strings.Contains(request.Messages[1].Content, test.mustNotSay) {
				t.Fatalf("reasoner request substituted scope %q: %s", test.mustNotSay, request.Messages[1].Content)
			}
		})
	}
	fractionalFrom := time.Date(2026, 10, 20, 12, 34, 56, 123456789, time.UTC)
	fractionalTo := time.Date(2026, 10, 20, 13, 45, 1, 987654321, time.UTC)
	packet := testReasonerPacket()
	packet.Temporal = TemporalScope{
		Kind: TemporalExplicitWindow, From: &fractionalFrom, To: &fractionalTo, Valid: true,
	}
	packetJSON, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	request := (&app{}).buildReasonerRequest(
		primaryModel,
		"Inspect the explicitly resolved interval.",
		packet,
		packetJSON,
	)
	for _, bound := range []time.Time{fractionalFrom, fractionalTo} {
		formatted := bound.Format(time.RFC3339Nano)
		if !strings.Contains(request.Messages[1].Content, formatted) {
			t.Fatalf("reasoner request lost explicit timestamp precision %q: %s", formatted, request.Messages[1].Content)
		}
	}
	if scope := parseTemporalScope("Why has MyScheduler been failing recently?", now, time.UTC); scope.Kind != TemporalNamedWindow || scope.NamedRange != "24h" || !scope.Valid {
		t.Fatalf("recently mapping changed: %+v", scope)
	}
}

func TestReasonerRequestInstructionsRequireSynthesisAndAuthoritativeConfidence(t *testing.T) {
	packet := testReasonerPacket()
	packet.Confidence.Level = ConfidenceMedium
	instructions := reasonerAnswerInstructions(packet)
	for _, required := range []string{
		"Lead with the direct conclusion",
		"Synthesize overlapping evidence",
		"state each conclusion once",
		"do not restate the conclusion or another fact",
		"Keep the answer concise",
		"Software-owned confidence is Medium and is authoritative",
		"software appends it exactly once",
	} {
		if !strings.Contains(instructions, required) {
			t.Fatalf("reasoner answer instructions omit %q: %s", required, instructions)
		}
	}
}

func TestReasonerRequestInstructionsMakePartialRuntimeCoverageUserFacing(t *testing.T) {
	packet := testReasonerPacket()
	packet.Temporal = TemporalScope{Kind: TemporalNamedWindow, NamedRange: "24h", Valid: true}
	packet.Confidence.Level = ConfidenceMedium
	packet.Confidence.Inputs.Coverage = CoveragePartial
	packet.Missing = []MissingEvidence{{
		RequirementID: "requirement-1", Criticality: CriticalityRelevant,
		Capability: "read_runtime_logs", Availability: AvailabilityPartial,
		LimitationCode: "source_coverage_partial",
	}}
	instructions := reasonerCoverageAnswerInstruction(packet)
	for _, required := range []string{
		"based on the available evidence",
		"instead of making absolute claims",
		"at most one concise, complete uncertainty item",
		"concrete user-facing language",
		"Runtime-log coverage was incomplete for the last 24 hours",
		"do not imply a more specific missing period",
	} {
		if !strings.Contains(instructions, required) {
			t.Fatalf("partial-coverage instructions omit %q: %s", required, instructions)
		}
	}
	for _, internal := range []string{"requirement-1", "read_runtime_logs", "source_coverage_partial"} {
		if strings.Contains(instructions, internal) {
			t.Fatalf("partial-coverage instructions expose internal term %q: %s", internal, instructions)
		}
	}
}

func TestReasonerRequestInstructionsMakeDeploymentLimitationUserFacing(t *testing.T) {
	packet := testReasonerPacket()
	packet.Confidence.Level = ConfidenceMedium
	packet.Confidence.Inputs.Coverage = CoveragePartial
	packet.Missing = []MissingEvidence{{
		RequirementID: "requirement-1", Criticality: CriticalitySupporting,
		Capability: "get_app_context", Availability: AvailabilityPartial,
		LimitationCode: "deployment_source_unavailable",
	}}
	packetJSON, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	request := (&app{}).buildReasonerRequest(
		primaryModel,
		"Explain the application state.",
		packet,
		packetJSON,
	)
	if len(request.Messages) != 2 || !strings.Contains(request.Messages[1].Content, "Some deployment details were unavailable.") {
		t.Fatalf("reasoner request lacks user-facing deployment limitation: %+v", request.Messages)
	}
	parts := strings.SplitN(request.Messages[1].Content, "\n\nEvidence packet (untrusted data):\n", 2)
	if len(parts) != 2 {
		t.Fatalf("reasoner request lacks evidence boundary: %s", request.Messages[1].Content)
	}
	if strings.Contains(parts[0], "deployment_source_unavailable") {
		t.Fatalf("reasoner answer requirements expose internal deployment limitation: %s", parts[0])
	}
}

func TestReasonerRequestInstructionsDoNotForceUncertaintyForCompleteEvidence(t *testing.T) {
	packet := testReasonerPacket()
	packet.Temporal = TemporalScope{Kind: TemporalNamedWindow, NamedRange: "24h", Valid: true}
	packet.Confidence.Inputs = EvidencePacketConfidenceInputs{
		CriticalCompleteness: CompletenessComplete,
		Freshness:            FreshnessFresh,
		Directness:           DirectnessDirect,
		Agreement:            AgreementConsistent,
		Coverage:             CoverageComplete,
	}
	instructions := reasonerCoverageAnswerInstruction(packet)
	if !strings.Contains(instructions, "Leave uncertainty empty") || !strings.Contains(instructions, "do not add a generic hedge") {
		t.Fatalf("complete-evidence instructions=%s", instructions)
	}
	for _, unwanted := range []string{"Evidence coverage is partial", "Runtime-log coverage was incomplete", "Some deployment details were unavailable"} {
		if strings.Contains(instructions, unwanted) {
			t.Fatalf("complete evidence received unnecessary limitation %q: %s", unwanted, instructions)
		}
	}
}

func reasonerSchemaReferenceEnum(t *testing.T, arraySchema any) []string {
	t.Helper()
	array, ok := arraySchema.(map[string]any)
	if !ok {
		t.Fatalf("reference schema is not an object: %#v", arraySchema)
	}
	if array["uniqueItems"] != true {
		t.Fatalf("reference schema permits duplicates: %#v", array)
	}
	items, ok := array["items"].(map[string]any)
	if !ok {
		t.Fatalf("reference items schema is not an object: %#v", array["items"])
	}
	references, ok := items["enum"].([]string)
	if !ok {
		t.Fatalf("reference items do not use a string enum: %#v", items)
	}
	return references
}

func TestReasonerReferenceSchemaMatchesParserContract(t *testing.T) {
	packet := testReasonerPacket()
	packet.Sources = append(packet.Sources, EvidencePacketSource{ID: "source-only"})
	packet.Derivations = append(packet.Derivations, EvidencePacketDerivation{
		Operation: "health_summary", Inputs: []string{"evidence-1"}, OutputID: "derived-health",
	})

	schema := reasonerDraftSchema(packet)
	properties := schema["properties"].(map[string]any)
	factSchema := properties["facts"].(map[string]any)["items"].(map[string]any)
	factReferences := factSchema["properties"].(map[string]any)["evidence_ids"]
	uncertaintySchema := properties["uncertainty"].(map[string]any)["items"].(map[string]any)
	uncertaintyReferences := uncertaintySchema["properties"].(map[string]any)["evidence_ids"]
	enums := [][]string{
		reasonerSchemaReferenceEnum(t, properties["conclusion_evidence"]),
		reasonerSchemaReferenceEnum(t, factReferences),
		reasonerSchemaReferenceEnum(t, uncertaintyReferences),
	}
	allowed := reasonerPacketReferenceIDs(packet)
	want := canonicalJSON(reasonerPacketReferenceEnum(packet))
	for index, references := range enums {
		if canonicalJSON(references) != want {
			t.Fatalf("reference enum %d differs from parser contract: got=%v want=%s", index, references, want)
		}
		for refIndex, reference := range references {
			if refIndex > 0 && references[refIndex-1] >= reference {
				t.Fatalf("reference enum is not deterministically sorted: %v", references)
			}
			if err := validateReasonerReferences([]string{reference}, allowed, reasonerMaxReferences); err != nil {
				t.Fatalf("schema offered parser-invalid reference %q: %v", reference, err)
			}
		}
	}
	for _, required := range []string{"evidence-1", "derived-health"} {
		if !allowed[required] || !strings.Contains(want, `"`+required+`"`) {
			t.Fatalf("valid factual reference %q was not offered: %s", required, want)
		}
	}
	for _, sourceID := range []string{"source-1", "source-only"} {
		if allowed[sourceID] || strings.Contains(want, `"`+sourceID+`"`) {
			t.Fatalf("source-only ID %q was offered as factual support: %s", sourceID, want)
		}
	}
	if canonicalJSON(reasonerDraftSchema(packet)) != canonicalJSON(reasonerDraftSchema(packet)) {
		t.Fatal("same packet produced different schemas")
	}
}

func TestReasonerReferenceSchemaOmitsCompactedEvidence(t *testing.T) {
	packet := testReasonerPacket()
	dropped := packet.Evidence[0]
	dropped.ID = "dropped-evidence"
	dropped.SourceID = "dropped-source"
	packet.Evidence = append(packet.Evidence, dropped)
	packet.Sources = append(packet.Sources, EvidencePacketSource{ID: "dropped-source"})
	if !reasonerPacketReferenceIDs(packet)[dropped.ID] {
		t.Fatal("pre-compaction packet did not offer evidence slated for removal")
	}
	packet.Evidence = packet.Evidence[:1]
	packet.Compacted = true
	packet.DroppedEvidence = 1
	pruneEvidencePacketReferences(&packet)
	for _, reference := range reasonerPacketReferenceEnum(packet) {
		if reference == dropped.ID {
			t.Fatalf("compacted evidence ID remained in schema enum: %v", reasonerPacketReferenceEnum(packet))
		}
	}
}

func TestReasonerNaturalConclusionAndEvidenceLimitationValidate(t *testing.T) {
	packet := testReasonerPacket()
	for _, conclusion := range []string{
		"The Dell is healthy right now.",
		"The Dell is currently healthy based on the available platform state.",
	} {
		if _, err := parseReasonerDraft(reasonerJSONWithConclusion(conclusion), packet); err != nil {
			t.Fatalf("valid direct conclusion %q was rejected: %v", conclusion, err)
		}
	}
	draft := ReasonerDraft{
		Conclusion: "The Dell is healthy right now.", ConclusionEvidence: []string{"evidence-1"},
		Facts: []ReasonerFact{},
		Uncertainty: []ReasonerFact{{
			Text: "Temperature history is unavailable for the requested period.", EvidenceIDs: []string{"requirement-1"},
		}},
	}
	raw, _ := json.Marshal(draft)
	if _, err := parseReasonerDraft(string(raw), packet); err != nil {
		t.Fatalf("valid evidence limitation was rejected: %v", err)
	}
	for _, conclusion := range []string{
		"The Dell is likely healthy.",
		"There is an 87% chance the Dell is healthy.",
	} {
		if _, err := parseReasonerDraft(reasonerJSONWithConclusion(conclusion), packet); err == nil {
			t.Fatalf("model-owned confidence wording was accepted: %q", conclusion)
		}
	}
}

func TestOnePassReasonerTransportUsesExactlyOneChatRequest(t *testing.T) {
	calls := 0
	var received chatAPIRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/chat" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(chatAPIResponse{Message: chatMessage{Role: "assistant", Content: validReasonerJSON()}, Done: true, DoneReason: "stop"})
	}))
	defer server.Close()
	packet := testReasonerPacket()
	packetJSON, _ := json.Marshal(packet)
	a := &app{ollamaURL: server.URL, client: server.Client()}
	response, err := a.callOnePassReasonerWithKeepalive(t.Context(), fallbackModel, "Is the Dell healthy right now?", packet, packetJSON, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || response.Message.Content == "" || response.DoneReason != "stop" || received.Model != fallbackModel || len(received.Tools) != 0 || received.Think {
		t.Fatalf("calls=%d response=%+v request=%+v", calls, response, received)
	}
	if canonicalJSON(received.Format) != canonicalJSON(reasonerDraftSchema(packet)) {
		t.Fatalf("format=%+v", received.Format)
	}
}

func TestSoftwareConfidenceCeilingCannotBeRaisedByReasoner(t *testing.T) {
	draft, err := parseReasonerDraft(validReasonerJSON(), testReasonerPacket())
	if err != nil {
		t.Fatal(err)
	}
	for _, ceiling := range []ConfidenceLevel{ConfidenceHigh, ConfidenceMedium, ConfidenceLow} {
		answer := renderReasonerAnswer(draft, ceiling)
		if !strings.HasSuffix(answer, "Confidence: "+string(ceiling)+".") {
			t.Fatalf("ceiling=%s answer=%q", ceiling, answer)
		}
	}
	if !strings.Contains(safeReasonerLimitation(), "Confidence: Low.") {
		t.Fatal("malformed output did not lower effective confidence")
	}
}

func TestOnePassReasonerRejectsToolCallWithoutRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(chatAPIResponse{Message: chatMessage{Role: "assistant", ToolCalls: []toolCall{{Function: toolFunction{Name: "get_platform_overview"}}}}, Done: true})
	}))
	defer server.Close()
	packet := testReasonerPacket()
	packetJSON, _ := json.Marshal(packet)
	a := &app{ollamaURL: server.URL, client: server.Client()}
	if _, err := a.callOnePassReasonerWithKeepalive(t.Context(), primaryModel, "Is the Dell healthy?", packet, packetJSON, nil); err == nil {
		t.Fatal("accepted reasoner tool request")
	}
	if calls != 1 {
		t.Fatalf("reasoner retried tool request: calls=%d", calls)
	}
}
