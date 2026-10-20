package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestEvidencePacketBoundsMaximumStringsEscapesAndLongQuestion(t *testing.T) {
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	executor := productionApplicationIncidentExecutor(now)
	contextValue := executor.values["get_app_context"].(reactorLabAppContextResult)
	source := *contextValue.Deployment.Source
	source.Repository = strings.Repeat("r", 201)
	source.Branch = strings.Repeat(`<>&"\界`, 170)
	source.RequestedRef = strings.Repeat(`refs/<>&"\界/`, 90)
	source.CommitSHA = strings.Repeat("a", 40)
	contextValue.Deployment.Source = &source
	contextValue.Deployment.Status = strings.Repeat("界", 128)
	contextValue.Deployment.ImageID = "sha256:" + strings.Repeat("b", 64)
	contextValue.LocalRepository.Branch = strings.Repeat(`<>&"\界`, 170)
	executor.values["get_app_context"] = contextValue

	applicationHistory := executor.values["read_application_history"].(reactorLabResolvedApplicationHistory)
	applicationHistory.Application.LatestStatus = strings.Repeat("界", 128)
	for index := range applicationHistory.History.Points {
		if index%2 == 0 {
			applicationHistory.History.Points[index].Status = strings.Repeat(`<>&"\界`, 22)
		} else {
			applicationHistory.History.Points[index].Status = strings.Repeat("failed界", 18)
		}
		applicationHistory.History.Points[index].RestartCount = index
	}
	executor.values["read_application_history"] = applicationHistory

	serviceHistory := executor.values["read_service_history"].(reactorLabServiceHistory)
	for serviceIndex := range serviceHistory.Services {
		serviceHistory.Services[serviceIndex].ID = strings.Repeat("i", 96) + fmt.Sprint(serviceIndex)
		serviceHistory.Services[serviceIndex].Name = strings.Repeat("界", 96) + fmt.Sprint(serviceIndex)
		for pointIndex := range serviceHistory.Services[serviceIndex].Points {
			serviceHistory.Services[serviceIndex].Points[pointIndex].Status = strings.Repeat(`<>&"\界`, 22)
		}
	}
	executor.values["read_service_history"] = serviceHistory

	result, err := runProductionApplicationIncidentPipeline(t, executor, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.PacketJSON) >= evidencePacketHardLimitBytes {
		t.Fatalf("max-bound packet bytes=%d hard-limit=%d", len(result.PacketJSON), evidencePacketHardLimitBytes)
	}
	if !bytes.Contains(result.PacketJSON, []byte(strings.Repeat("a", 40))) || !bytes.Contains(result.PacketJSON, []byte("sha256:"+strings.Repeat("b", 64))) {
		t.Fatal("exact SHA identities were not retained")
	}
	for _, causal := range []string{"runtime_failures", "error_patterns", "service_history", "service_summary", "service_transitions"} {
		if !bytes.Contains(result.PacketJSON, []byte(causal)) {
			t.Fatalf("max-bound packet lost causal summary %q", causal)
		}
	}
	var decoded any
	if err := json.Unmarshal(result.PacketJSON, &decoded); err != nil {
		t.Fatalf("escape-heavy packet is invalid JSON: %v", err)
	}
	assertEvidencePacketBounds(t, result.Packet)
	foundServiceStates := false
	for _, item := range result.Packet.Evidence {
		if item.Type != EvidenceTypeServiceHistory || item.Kind != EvidenceObserved {
			continue
		}
		var states []serviceLatestStateFact
		if err := json.Unmarshal([]byte(packetFieldText(item, "service_states")), &states); err != nil {
			t.Fatal(err)
		}
		if len(states) != phase2BServiceSummaryLimit || states[0].ID == states[1].ID {
			t.Fatalf("bounded service identities collided: %+v", states)
		}
		for _, service := range states {
			assertBoundedUTF8(t, service.ID, phase2BServiceIdentityRunes)
		}
		foundServiceStates = true
	}
	if !foundServiceStates {
		t.Fatal("bounded service states were not retained")
	}

	longQuestionEvidence := result.Evidence
	longQuestionEvidence.NormalizedQuestion = "Why has MyScheduler been failing recently? " + strings.Repeat(`界<>&"\`, 20000)
	longPacket, err := BuildEvidencePacket(longQuestionEvidence)
	if err != nil {
		t.Fatal(err)
	}
	longJSON, err := MarshalEvidencePacket(longPacket)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.PacketJSON, longJSON) || bytes.Contains(longJSON, []byte(`"question"`)) {
		t.Fatalf("question affected packet: baseline=%d long=%d", len(result.PacketJSON), len(longJSON))
	}
	t.Logf("max-bound packet bytes=%d hard-limit headroom=%d", len(result.PacketJSON), evidencePacketHardLimitBytes-len(result.PacketJSON))
}

func TestCurrentApplicationMaximumIdentityFieldsAreBoundedAndExactSHAIsRetained(t *testing.T) {
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	value := productionApplicationIncidentExecutor(now).values["get_app_context"].(reactorLabAppContextResult)
	source := *value.Deployment.Source
	source.Repository = strings.Repeat("r", 201)
	source.Branch = strings.Repeat(`<>&"\界`, 170)
	source.RequestedRef = strings.Repeat(`refs/<>&"\界/`, 90)
	source.CommitSHA = strings.Repeat("c", 40)
	value.Deployment.Source = &source
	value.Deployment.Status = strings.Repeat("界", 128)
	value.Deployment.ImageID = "sha256:" + strings.Repeat("d", 64)
	value.LocalRepository.Branch = strings.Repeat(`<>&"\界`, 170)
	value.LocalRepository.Commit = strings.Repeat("e", 40)

	requirement := testRequirement(RouteApplicationCurrent, EvidenceTypeCurrentApplication, CriticalityCritical, EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler"}, &TemporalScope{Kind: TemporalNow, At: &now, Valid: true})
	evidence := reduceTestResults(t, RouteApplicationCurrent, []EvidenceRequirement{requirement}, []EvidenceResult{{
		RequestID: "context", Capability: "get_app_context", RequirementIDs: []string{requirement.ID}, CompletedAt: now,
		Availability: AvailabilityAvailable, SafeResult: value, CostUnits: 1, MaxFanout: 1,
	}})
	packet, err := BuildEvidencePacket(ApplyEvidenceConfidence(evidence))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := MarshalEvidencePacket(packet)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > evidencePacketTargetBytes || len(packet.Evidence) != 1 {
		t.Fatalf("current application packet bytes/items=%d/%d", len(encoded), len(packet.Evidence))
	}
	fields := map[string]string{}
	for _, field := range packet.Evidence[0].Fields {
		if field.Value.Kind == EvidenceValueString {
			fields[field.Name] = field.Value.String
		}
	}
	for _, name := range []string{"repository", "branch", "requested_ref", "status", "repository_branch"} {
		if fields[name] == "" {
			t.Fatalf("missing projected current application field %q: %s", name, encoded)
		}
		assertBoundedUTF8(t, fields[name], evidencePacketStringRunes)
	}
	for name, want := range map[string]string{
		"commit_sha": strings.Repeat("c", 40), "repository_commit": strings.Repeat("e", 40), "image_id": "sha256:" + strings.Repeat("d", 64),
	} {
		if fields[name] != want {
			t.Fatalf("exact identity %s=%q want %q", name, fields[name], want)
		}
	}
	assertEvidencePacketBounds(t, packet)
}

func TestApplicationFallbackWithOneHundredMatchesIsBoundedAndExplicit(t *testing.T) {
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	requirement := testRequirement(RouteApplicationCurrent, EvidenceTypeCurrentApplication, CriticalityCritical, EvidenceSubject{Kind: SubjectApplication, ID: "all"}, &TemporalScope{Kind: TemporalNow, At: &now, Valid: true})
	apps := make([]reactorLabAppSummary, 0, reactorLabMaxToolApps)
	const authoritativeTotal = 500
	for index := 0; index < reactorLabMaxToolApps; index++ {
		apps = append(apps, reactorLabAppSummary{App: fmt.Sprintf("candidate-%03d", index), Status: strings.Repeat("failed", 24)})
	}
	evidence := reduceTestResults(t, RouteApplicationCurrent, []EvidenceRequirement{requirement}, []EvidenceResult{{
		RequestID: "apps", Capability: "list_apps", RequirementIDs: []string{requirement.ID}, CompletedAt: now,
		Availability: AvailabilityAvailable, SafeResult: reactorLabAppListResult{Apps: apps, TotalApps: authoritativeTotal, Truncated: true}, CostUnits: 1, MaxFanout: reactorLabMaxToolApps,
	}})
	if len(evidence.Items) != 1 || evidenceIntegerValue(evidence.Items[0], "matched_applications") != authoritativeTotal ||
		evidenceIntegerValue(evidence.Items[0], "candidates_included") != phase2BApplicationCandidateLimit || !fieldBoolean(evidence.Items[0], "ambiguous") || !fieldBoolean(evidence.Items[0], "candidates_truncated") {
		t.Fatalf("fallback summary=%s", canonicalJSON(evidence.Items))
	}
	var candidates []map[string]any
	if err := json.Unmarshal([]byte(fieldText(evidence.Items[0], "application_candidates")), &candidates); err != nil {
		t.Fatal(err)
	}
	if len(candidates) != phase2BApplicationCandidateLimit {
		t.Fatalf("fallback candidates=%d", len(candidates))
	}
	packet, err := BuildEvidencePacket(ApplyEvidenceConfidence(evidence))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := MarshalEvidencePacket(packet)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > evidencePacketTargetBytes {
		t.Fatalf("fallback packet bytes=%d target=%d", len(encoded), evidencePacketTargetBytes)
	}
	t.Logf("fallback ambiguity packet bytes=%d", len(encoded))
	assertEvidencePacketBounds(t, packet)
}

func TestApplicationTransitionsRetainFirstAndMostRecentFailureRecovery(t *testing.T) {
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	from := now.Add(-10 * time.Hour)
	requirement := testRequirement(RouteApplicationPerformance, EvidenceTypeApplicationHistory, CriticalityCritical, EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler"}, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "24h", Valid: true})
	points := make([]reactorLabApplicationPoint, 10)
	for index := range points {
		status := "running"
		if index%2 == 1 {
			status = "failed"
		}
		points[index] = reactorLabApplicationPoint{Timestamp: from.Add(time.Duration(index) * time.Hour), Status: status, RestartCount: index}
	}
	items, derivations, err := reduceApplicationHistory(EvidenceResult{RequestID: "history", CompletedAt: now, SafeResult: reactorLabResolvedApplicationHistory{
		Application: reactorLabApplicationSummary{LatestStatus: "failed", RestartCount: 9},
		History:     reactorLabApplicationHistory{Window: reactorLabWindow{From: from, To: now, BucketSeconds: 3600}, Points: points, TotalPoints: len(points)},
	}}, requirement, "source:history", "")
	if err != nil {
		t.Fatal(err)
	}
	observed := evidenceItemByKind(t, items, EvidenceObserved)
	derived := evidenceItemByKind(t, items, EvidenceDerived)
	var transitions []applicationStateTransition
	if err := json.Unmarshal([]byte(fieldText(observed, "state_transitions")), &transitions); err != nil {
		t.Fatal(err)
	}
	wantTimes := []time.Time{points[1].Timestamp, points[2].Timestamp, points[8].Timestamp, points[9].Timestamp}
	if len(transitions) != phase2BApplicationTransitionLimit || evidenceIntegerValue(derived, "transition_count") != 9 || !fieldBoolean(derived, "transitions_truncated") ||
		hasEvidenceField(observed, "transition_count") || hasEvidenceField(observed, "cpu_average") || !hasEvidenceField(derived, "cpu_average") {
		t.Fatalf("transitions=%s", canonicalJSON(items))
	}
	if len(derivations) != 1 || len(derivations[0].Inputs) != 1 || derivations[0].Inputs[0] != observed.ID || derivations[0].OutputID != derived.ID {
		t.Fatalf("application history lineage=%+v", derivations)
	}
	for index := range wantTimes {
		if !transitions[index].Timestamp.Equal(wantTimes[index]) {
			t.Fatalf("transition %d at %s want %s", index, transitions[index].Timestamp, wantTimes[index])
		}
	}
}

func TestServiceHistoryCollapsesDuplicatesAndUsesGlobalTransitionOrder(t *testing.T) {
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	from := now.Add(-time.Hour)
	requirement := testRequirement(RouteApplicationPerformance, EvidenceTypeServiceHistory, CriticalityRelevant, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "1h", Valid: true})
	services := make([]reactorLabServiceSeries, 0, 21)
	for index := 0; index < 20; index++ {
		base := from.Add(time.Duration(index) * time.Minute)
		services = append(services, reactorLabServiceSeries{ID: fmt.Sprintf("service-%02d", index), Name: fmt.Sprintf("worker-%02d", index), Points: []reactorLabServicePoint{
			{Timestamp: base, Available: true, Status: "running"},
			{Timestamp: base.Add(10 * time.Minute), Available: false, Status: "failed"},
			{Timestamp: base.Add(20 * time.Minute), Available: true, Status: "running"},
		}})
	}
	services = append(services, services[0])
	items, derivations, err := reduceServiceHistory(EvidenceResult{RequestID: "services", CompletedAt: now, SafeResult: reactorLabServiceHistory{
		Window: reactorLabWindow{From: from, To: now}, Services: services, TotalServices: len(services),
	}}, requirement, "source:services", "")
	if err != nil {
		t.Fatal(err)
	}
	observed := evidenceItemByKind(t, items, EvidenceObserved)
	derived := evidenceItemByKind(t, items, EvidenceDerived)
	var states []serviceLatestStateFact
	if err := json.Unmarshal([]byte(fieldText(observed, "service_states")), &states); err != nil {
		t.Fatal(err)
	}
	var transitions []serviceTransitionFact
	if err := json.Unmarshal([]byte(fieldText(observed, "service_transitions")), &transitions); err != nil {
		t.Fatal(err)
	}
	var summary serviceEvidenceSummary
	if err := json.Unmarshal([]byte(fieldText(derived, "service_summary")), &summary); err != nil {
		t.Fatal(err)
	}
	latest := from.Add(39 * time.Minute)
	if len(states) != phase2BServiceSummaryLimit || len(transitions) != phase2BServiceTransitionLimit ||
		summary.ServiceCount != 21 || summary.UniqueServiceCount != 20 || summary.TransitionCount != 40 || summary.DeduplicatedPointCount == 0 ||
		transitions[len(transitions)-1].Timestamp != latest.Format(time.RFC3339Nano) || hasEvidenceField(observed, "service_summary") {
		t.Fatalf("service reduction=%s", canonicalJSON(items))
	}
	if len(derivations) != 1 || len(derivations[0].Inputs) != 1 || derivations[0].Inputs[0] != observed.ID || derivations[0].OutputID != derived.ID {
		t.Fatalf("service history lineage=%+v", derivations)
	}
}

func TestServiceIdentityProjectionIsCollisionResistantAndContextAware(t *testing.T) {
	first := "prefix-same-middle-one-suffix"
	second := "prefix-same-middle-two-suffix"
	firstProjected := compactServiceIdentity(first, phase2BServiceIdentityRunes)
	secondProjected := compactServiceIdentity(second, phase2BServiceIdentityRunes)
	if firstProjected == secondProjected || firstProjected != compactServiceIdentity(first, phase2BServiceIdentityRunes) {
		t.Fatalf("service identity projection is not distinct and deterministic: %q %q", firstProjected, secondProjected)
	}
	if compactServiceIdentity("short-id", phase2BServiceIdentityRunes) != "short-id" {
		t.Fatal("short service identity changed")
	}
	for _, projected := range []string{firstProjected, secondProjected, compactServiceIdentity(strings.Repeat("界", 20), phase2BServiceIdentityRunes)} {
		if !utf8.ValidString(projected) || len(projected) > phase2BServiceIdentityRunes || utf8.RuneCountInString(projected) > phase2BServiceIdentityRunes {
			t.Fatalf("service identity is not safely bounded: %q bytes=%d runes=%d", projected, len(projected), utf8.RuneCountInString(projected))
		}
	}
	const unrelatedID = "unrelated-event-identifier"
	var unrelated map[string]any
	if err := json.Unmarshal([]byte(projectEvidenceString("events", `{"id":"unrelated-event-identifier"}`)), &unrelated); err != nil {
		t.Fatal(err)
	}
	if unrelated["id"] != unrelatedID {
		t.Fatalf("generic nested id received service-specific projection: %+v", unrelated)
	}
}

func TestServiceHistoryEmptySeriesIsNotRepresentedAsDown(t *testing.T) {
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	from := now.Add(-time.Hour)
	requirement := testRequirement(RouteApplicationPerformance, EvidenceTypeServiceHistory, CriticalityRelevant, EvidenceSubject{Kind: SubjectHost, ID: "dell"}, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "1h", Valid: true})
	value := reactorLabServiceHistory{Window: reactorLabWindow{From: from, To: now}, Services: []reactorLabServiceSeries{
		{ID: "empty", Points: []reactorLabServicePoint{}},
		{ID: "down", Points: []reactorLabServicePoint{{Timestamp: now.Add(-2 * time.Minute), Available: false, Status: "failed"}}},
		{ID: "up", Points: []reactorLabServicePoint{{Timestamp: now.Add(-time.Minute), Available: true, Status: "running"}}},
	}}
	items, _, err := reduceServiceHistory(EvidenceResult{RequestID: "services", CompletedAt: now, SafeResult: value}, requirement, "source:services", "")
	if err != nil {
		t.Fatal(err)
	}
	observed := evidenceItemByKind(t, items, EvidenceObserved)
	derived := evidenceItemByKind(t, items, EvidenceDerived)
	var states []serviceLatestStateFact
	if err := json.Unmarshal([]byte(fieldText(observed, "service_states")), &states); err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[0].ID != "down" || states[0].Available || states[1].ID != "up" || !states[1].Available {
		t.Fatalf("empty/down/up service states=%+v", states)
	}
	for _, state := range states {
		if state.ID == "empty" {
			t.Fatalf("empty series was represented as a sampled state: %+v", states)
		}
	}
	var summary serviceEvidenceSummary
	if err := json.Unmarshal([]byte(fieldText(derived, "service_summary")), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.ServicesWithoutSamples != 1 || summary.ServiceCount != 3 || summary.UniqueServiceCount != 3 {
		t.Fatalf("empty series summary=%+v", summary)
	}
}

func TestRuntimeFailureSummaryKeepsOnlyBoundedTopPatterns(t *testing.T) {
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	from := now.Add(-time.Hour)
	requirement := testRequirement(RouteApplicationPerformance, EvidenceTypeRuntimeFailures, CriticalityRelevant, EvidenceSubject{Kind: SubjectApplication, ID: "myscheduler"}, &TemporalScope{Kind: TemporalNamedWindow, NamedRange: "1h", Valid: true})
	lines := make([]string, 0, 24)
	for index := 0; index < 12; index++ {
		label := strings.Repeat(string(rune('a'+index)), 3)
		for repeat := 0; repeat < index+1; repeat++ {
			lines = append(lines, from.Add(time.Duration(index)*time.Minute).Format(time.RFC3339Nano)+fmt.Sprintf(" ERROR failure-signature-%s %s", label, strings.Repeat("界", 200)))
		}
	}
	items, _, err := reduceLogs(EvidenceResult{RequestID: "logs", CompletedAt: now, SafeResult: logToolResponse{Kind: "runtime", Logs: strings.Join(lines, "\n")}}, requirement, "source:logs", "")
	if err != nil {
		t.Fatal(err)
	}
	derived := evidenceItemByKind(t, items, EvidenceDerived)
	var summary runtimeEvidenceSummary
	if err := json.Unmarshal([]byte(fieldText(derived, "runtime_summary")), &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Patterns) != phase2BRuntimePatternLimit {
		t.Fatalf("runtime patterns=%d", len(summary.Patterns))
	}
	for _, pattern := range summary.Patterns {
		if len([]rune(pattern.Signature)) > phase2BRuntimeSignatureRunes {
			t.Fatalf("signature runes=%d", len([]rune(pattern.Signature)))
		}
	}
}

func TestPacketMetadataCollectionsAndFreeFormStringsAreBounded(t *testing.T) {
	now := time.Date(2026, 9, 28, 5, 6, 47, 0, time.UTC)
	packet := EvidencePacket{
		SchemaVersion: evidencePacketSchemaVersion,
		Subjects:      []EvidenceSubject{{Kind: SubjectApplication, ID: strings.Repeat("i", 500), Name: strings.Repeat("界", 500)}},
		Sources:       []EvidencePacketSource{{ID: "source", Capability: strings.Repeat("cap", 100), Resource: strings.Repeat(`<>&"\界`, 100), CollectedAt: now, Availability: AvailabilityAvailable}},
		Evidence:      []EvidencePacketItem{{ID: "item", Kind: EvidenceObserved, Type: EvidenceTypeCurrentApplication, SourceID: "source", RequirementID: "requirement", Criticality: CriticalityCritical, Fields: []EvidenceField{evidenceStringField("status", strings.Repeat("界", 500))}}},
	}
	for index := 0; index < 30; index++ {
		packet.Missing = append(packet.Missing, MissingEvidence{RequirementID: "requirement", Criticality: CriticalityRelevant, Capability: strings.Repeat("c", 200), Availability: AvailabilityUnavailable, ErrorCategory: EvidenceErrorUnavailable, Reason: strings.Repeat("same reason ", 100)})
		packet.Conflicts = append(packet.Conflicts, EvidenceConflict{ID: fmt.Sprintf("conflict-%02d", index), Subject: packet.Subjects[0], Field: strings.Repeat("field", 50), EvidenceIDs: []string{"item"}, Material: true, Explanation: strings.Repeat("explanation", 100)})
		packet.Derivations = append(packet.Derivations, EvidencePacketDerivation{Operation: fmt.Sprintf("operation-%02d-%s", index, strings.Repeat("x", 200)), PolicyID: strings.Repeat("p", 200), Inputs: []string{"item"}, OutputID: "item", Explanation: strings.Repeat("derive", 100)})
	}
	if err := projectEvidencePacket(&packet); err != nil {
		t.Fatal(err)
	}
	if len(packet.Missing) != 1 || len(packet.Conflicts) != evidencePacketMaxConflicts || len(packet.Derivations) != evidencePacketMaxDerivations {
		t.Fatalf("metadata cardinality missing/conflicts/derivations=%d/%d/%d", len(packet.Missing), len(packet.Conflicts), len(packet.Derivations))
	}
	assertEvidencePacketBounds(t, packet)

	duplicate := MissingEvidence{RequirementID: "requirement", Criticality: CriticalityRelevant, Capability: "read_runtime_logs", Availability: AvailabilityUnavailable, ErrorCategory: EvidenceErrorUnavailable, Reason: strings.Repeat("safe reason ", 30)}
	duplicates := EvidencePacket{Missing: []MissingEvidence{duplicate, duplicate, duplicate}}
	if err := projectEvidencePacket(&duplicates); err != nil {
		t.Fatal(err)
	}
	if len(duplicates.Missing) != 1 {
		t.Fatalf("identical missing evidence was not deduplicated: %+v", duplicates.Missing)
	}
	assertBoundedUTF8(t, duplicates.Missing[0].Reason, evidencePacketMissingReasonRunes)
}

func TestMissingEvidencePreservesDistinctLimitationsAndReportsOverflow(t *testing.T) {
	distinct := []MissingEvidence{
		{RequirementID: "requirement", Criticality: CriticalitySupporting, Capability: "get_app_context", Availability: AvailabilityPartial, Reason: "recovery overview facet unavailable in compound application context"},
		{RequirementID: "requirement", Criticality: CriticalitySupporting, Capability: "get_app_context", Availability: AvailabilityPartial, Reason: "database lookups failed for: db-one"},
		{RequirementID: "requirement", Criticality: CriticalitySupporting, Capability: "get_app_context", Availability: AvailabilityPartial, Reason: "database lookup coverage truncated by compound application context bound"},
		{RequirementID: "requirement", Criticality: CriticalitySupporting, Capability: "get_app_context", Availability: AvailabilityPartial, Reason: "custom limitation alpha"},
		{RequirementID: "requirement", Criticality: CriticalitySupporting, Capability: "get_app_context", Availability: AvailabilityPartial, Reason: "custom limitation beta"},
	}
	first := EvidencePacket{Missing: append(append([]MissingEvidence{}, distinct...), distinct[0])}
	second := EvidencePacket{Missing: []MissingEvidence{distinct[4], distinct[2], distinct[0], distinct[3], distinct[1], distinct[0]}}
	if err := projectEvidencePacket(&first); err != nil {
		t.Fatal(err)
	}
	if err := projectEvidencePacket(&second); err != nil {
		t.Fatal(err)
	}
	firstJSON, err := MarshalEvidencePacket(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := MarshalEvidencePacket(second)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Missing) != 5 || !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("distinct limitations/determinism=%+v\n%s\n%s", first.Missing, firstJSON, secondJSON)
	}
	codes := map[string]bool{}
	for _, missing := range first.Missing {
		codes[missing.LimitationCode] = true
	}
	for _, code := range []string{"overview_recovery_unavailable", "database_lookup_failed", "database_lookup_coverage_truncated"} {
		if !codes[code] {
			t.Fatalf("missing typed limitation %q: %+v", code, first.Missing)
		}
	}

	large := EvidencePacket{}
	for index := 0; index < 9; index++ {
		large.Missing = append(large.Missing, MissingEvidence{RequirementID: "requirement", Criticality: CriticalityCritical, Capability: "read_application_history", Availability: AvailabilityUnavailable, LimitationCode: fmt.Sprintf("critical_%02d", index)})
	}
	for index := 0; index < 3; index++ {
		large.Missing = append(large.Missing, MissingEvidence{RequirementID: "requirement", Criticality: CriticalityRelevant, Capability: "read_runtime_logs", Availability: AvailabilityUnavailable, LimitationCode: fmt.Sprintf("relevant_%02d", index)})
	}
	for index := 0; index < 2; index++ {
		large.Missing = append(large.Missing, MissingEvidence{RequirementID: "requirement", Criticality: CriticalitySupporting, Capability: "get_app_context", Availability: AvailabilityPartial, LimitationCode: fmt.Sprintf("supporting_%02d", index)})
	}
	if err := projectEvidencePacket(&large); err != nil {
		t.Fatal(err)
	}
	critical := 0
	for _, missing := range large.Missing {
		if missing.Criticality == CriticalityCritical {
			critical++
		}
	}
	if len(large.Missing) != evidencePacketMaxMissing || critical != evidencePacketMaxMissing || large.MissingOmitted != (EvidencePacketMissingOmission{Critical: 1, Relevant: 3, Supporting: 2}) {
		t.Fatalf("missing evidence overflow=%+v omitted=%+v", large.Missing, large.MissingOmitted)
	}
	largeJSON, err := MarshalEvidencePacket(large)
	if err != nil {
		t.Fatal(err)
	}
	if len(largeJSON) >= evidencePacketHardLimitBytes {
		t.Fatalf("large missing-evidence packet bytes=%d hard-limit=%d", len(largeJSON), evidencePacketHardLimitBytes)
	}
	if !bytes.Contains(largeJSON, []byte(`"missingOmitted":{"critical":1,"relevant":3,"supporting":2}`)) {
		t.Fatalf("missing-evidence omission marker absent: %s", largeJSON)
	}
	t.Logf("large MissingEvidence packet bytes=%d", len(largeJSON))
	assertEvidencePacketBounds(t, large)
}

func assertEvidencePacketBounds(t *testing.T, packet EvidencePacket) {
	t.Helper()
	if len(packet.Subjects) > evidencePacketMaxSubjects || len(packet.Sources) > evidencePacketMaxSources || len(packet.Derivations) > evidencePacketMaxDerivations || len(packet.Missing) > evidencePacketMaxMissing || len(packet.Conflicts) > evidencePacketMaxConflicts {
		t.Fatalf("packet collection bound violated: %+v", packet)
	}
	critical, relevant, supporting := 0, 0, 0
	assertBoundedUTF8(t, packet.Temporal.NamedRange, 16)
	assertBoundedUTF8(t, packet.Temporal.Reason, evidencePacketTemporalReasonRunes)
	for _, subject := range packet.Subjects {
		assertBoundedUTF8(t, subject.ID, evidencePacketSubjectRunes)
		assertBoundedUTF8(t, subject.Name, evidencePacketSubjectRunes)
	}
	for _, source := range packet.Sources {
		assertBoundedUTF8(t, source.ID, evidencePacketIdentifierRunes)
		assertBoundedUTF8(t, source.Capability, evidencePacketIdentifierRunes)
		assertBoundedUTF8(t, source.Resource, evidencePacketResourceRunes)
	}
	for _, item := range packet.Evidence {
		assertBoundedUTF8(t, item.ID, evidencePacketIdentifierRunes)
		assertBoundedUTF8(t, item.SourceID, evidencePacketIdentifierRunes)
		assertBoundedUTF8(t, item.RequirementID, evidencePacketIdentifierRunes)
		switch item.Criticality {
		case CriticalityCritical:
			critical++
		case CriticalityRelevant:
			relevant++
		default:
			supporting++
		}
		if len(item.Fields) > evidencePacketMaxFieldsPerItem {
			t.Fatalf("field cardinality=%d", len(item.Fields))
		}
		assertBoundedUTF8(t, item.Subject.ID, evidencePacketSubjectRunes)
		assertBoundedUTF8(t, item.Subject.Name, evidencePacketSubjectRunes)
		for _, field := range item.Fields {
			assertBoundedUTF8(t, field.Name, evidencePacketFieldNameRunes)
			if field.Value.Kind != EvidenceValueString {
				continue
			}
			limit := evidencePacketStringRunes
			trimmed := strings.TrimSpace(field.Value.String)
			if json.Valid([]byte(trimmed)) && (strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{")) {
				limit = evidencePacketJSONFieldRunes
			}
			assertBoundedUTF8(t, field.Value.String, limit)
		}
	}
	if critical > evidencePacketMaxCriticalItems || relevant > evidencePacketMaxRelevantItems || supporting > evidencePacketMaxSupportingItems {
		t.Fatalf("evidence cardinality critical/relevant/supporting=%d/%d/%d", critical, relevant, supporting)
	}
	for _, missing := range packet.Missing {
		assertBoundedUTF8(t, missing.RequirementID, evidencePacketIdentifierRunes)
		assertBoundedUTF8(t, missing.Capability, evidencePacketIdentifierRunes)
		assertBoundedUTF8(t, missing.LimitationCode, evidencePacketLimitationCodeRunes)
		assertBoundedUTF8(t, missing.Reason, evidencePacketMissingReasonRunes)
	}
	if packet.MissingOmitted.Critical < 0 || packet.MissingOmitted.Relevant < 0 || packet.MissingOmitted.Supporting < 0 {
		t.Fatalf("negative missing-evidence omission count: %+v", packet.MissingOmitted)
	}
	for _, conflict := range packet.Conflicts {
		assertBoundedUTF8(t, conflict.ID, evidencePacketIdentifierRunes)
		assertBoundedUTF8(t, conflict.Subject.ID, evidencePacketSubjectRunes)
		assertBoundedUTF8(t, conflict.Subject.Name, evidencePacketSubjectRunes)
		assertBoundedUTF8(t, conflict.Field, evidencePacketFieldNameRunes)
		if len(conflict.EvidenceIDs) > evidencePacketMaxConflictEvidenceIDs {
			t.Fatalf("conflict references=%d", len(conflict.EvidenceIDs))
		}
		for _, evidenceID := range conflict.EvidenceIDs {
			assertBoundedUTF8(t, evidenceID, evidencePacketIdentifierRunes)
		}
		assertBoundedUTF8(t, conflict.Explanation, evidencePacketExplanationRunes)
	}
	for _, derivation := range packet.Derivations {
		assertBoundedUTF8(t, derivation.Operation, evidencePacketIdentifierRunes)
		assertBoundedUTF8(t, derivation.PolicyID, evidencePacketIdentifierRunes)
		assertBoundedUTF8(t, derivation.OutputID, evidencePacketIdentifierRunes)
		if len(derivation.Inputs) > evidencePacketMaxConflictEvidenceIDs {
			t.Fatalf("derivation inputs=%d", len(derivation.Inputs))
		}
		for _, input := range derivation.Inputs {
			assertBoundedUTF8(t, input, evidencePacketIdentifierRunes)
		}
		assertBoundedUTF8(t, derivation.Explanation, evidencePacketExplanationRunes)
	}
	for _, reason := range packet.Confidence.Reasons {
		assertBoundedUTF8(t, reason, evidencePacketConfidenceReasonRunes)
	}
}

func assertBoundedUTF8(t *testing.T, value string, limit int) {
	t.Helper()
	if !utf8.ValidString(value) || len([]rune(value)) > limit || len(value) > limit {
		t.Fatalf("invalid bounded string runes=%d bytes=%d limit=%d value=%q", len([]rune(value)), len(value), limit, value)
	}
}

func fieldBoolean(item EvidenceItem, name string) bool {
	for _, field := range item.Payload.Fields {
		if field.Name == name && field.Value.Boolean != nil {
			return *field.Value.Boolean
		}
	}
	return false
}

func packetFieldText(item EvidencePacketItem, name string) string {
	for _, field := range item.Fields {
		if field.Name == name && field.Value.Kind == EvidenceValueString {
			return field.Value.String
		}
	}
	return ""
}

func hasEvidenceField(item EvidenceItem, name string) bool {
	for _, field := range item.Payload.Fields {
		if field.Name == name {
			return true
		}
	}
	return false
}
