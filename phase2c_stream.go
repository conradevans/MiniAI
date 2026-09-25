package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

func (a *app) phase2CNow() time.Time {
	if a != nil && a.phase2Now != nil {
		return a.phase2Now().UTC()
	}
	return time.Now().UTC()
}

func (a *app) phase2CPipeline() Phase2BPipeline {
	executor := a.phase2Executor
	if executor == nil {
		executor = newCapabilityExecutor(a)
	}
	now := a.phase2Now
	if now == nil {
		now = time.Now
	}
	return NewPhase2BPipeline(phase0CapabilityRegistry(), executor, now)
}

func (a *app) buildProductionRouterContext(ctx context.Context, question string, history []storedMessage, now time.Time) ShadowRouterContext {
	routerContext := ShadowRouterContext{Now: now.UTC(), Location: time.UTC}
	deployments, err := a.fetchDeployments(ctx)
	if err != nil {
		return routerContext
	}
	for _, deployment := range deployments {
		name, _ := deployment["app"].(string)
		name = strings.TrimSpace(name)
		if !safeAppName(name) {
			continue
		}
		routerContext.Entities = append(routerContext.Entities, RouterEntity{
			Subject: EvidenceSubject{Kind: SubjectApplication, ID: name, Name: name},
			Aliases: []string{name},
		})
		for _, database := range routerDatabasesFromDeployment(deployment) {
			routerContext.Entities = append(routerContext.Entities, database)
		}
	}
	routerContext.ConversationSubjects = phase2CConversationSubjects(question, history, deployments)
	sort.Slice(routerContext.Entities, func(i, j int) bool {
		left := string(routerContext.Entities[i].Subject.Kind) + ":" + routerContext.Entities[i].Subject.ID
		right := string(routerContext.Entities[j].Subject.Kind) + ":" + routerContext.Entities[j].Subject.ID
		return left < right
	})
	return routerContext
}

func phase2CConversationSubjects(question string, history []storedMessage, deployments []map[string]any) []EvidenceSubject {
	if !shouldInheritBareThisApplication(normalizeQuestionText(question)) {
		subject := resolveConversationSubjectFromDeployments(question, history, deployments)
		if subject.App == "" || subject.Ambiguous {
			return nil
		}
		return []EvidenceSubject{{Kind: SubjectApplication, ID: subject.App, Name: subject.App}}
	}

	seen := map[string]string{}
	for _, message := range history {
		var apps []string
		switch message.Role {
		case "assistant":
			apps = evidenceAppsForMessage(message, deployments)
		case "user":
			apps = mentionedAppNamesFromDeployments(message.Content, deployments)
		}
		for _, appName := range apps {
			if safeAppName(appName) {
				seen[strings.ToLower(appName)] = appName
			}
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	subjects := make([]EvidenceSubject, 0, len(keys))
	for _, key := range keys {
		name := seen[key]
		subjects = append(subjects, EvidenceSubject{Kind: SubjectApplication, ID: name, Name: name})
	}
	return subjects
}

func routerDatabasesFromDeployment(deployment map[string]any) []RouterEntity {
	seen := map[string]bool{}
	out := []RouterEntity{}
	add := func(id, name string) {
		id, name = strings.TrimSpace(id), strings.TrimSpace(name)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		aliases := []string{id}
		if name != "" {
			aliases = append(aliases, name)
		}
		out = append(out, RouterEntity{
			Subject: EvidenceSubject{Kind: SubjectDatabase, ID: id, Name: name},
			Aliases: aliases,
		})
	}
	if database, ok := deployment["database"].(map[string]any); ok {
		id, _ := database["id"].(string)
		name, _ := database["displayName"].(string)
		add(id, name)
	}
	if databases, ok := deployment["databases"].([]any); ok {
		for _, raw := range databases {
			if database, ok := raw.(map[string]any); ok {
				id, _ := database["id"].(string)
				name, _ := database["displayName"].(string)
				add(id, name)
			}
		}
	}
	return out
}

func isPhase2CReasonedRoute(route InvestigationRouteID) bool {
	switch route {
	case RouteCurrentPlatformHealth, RouteThermalInvestigation, RouteRestartInvestigation,
		RouteApplicationCurrent, RouteApplicationPerformance, RouteDeploymentCorrelation,
		RouteDatabaseInvestigation, RouteRepositoryInvestigation:
		return true
	default:
		return false
	}
}

func isRecognizedPhase2CAmbiguity(decision ShadowRouteDecision) bool {
	return decision.Resolution == RouteResolutionAmbiguous && len(decision.Frame.Domains) > 0
}

func phase2CClarification(decision ShadowRouteDecision) string {
	switch decision.Reason {
	case "unresolved_application_subject":
		return "Which application do you mean?"
	case "multiple_subjects":
		return "Which single application or system should I investigate?"
	default:
		return "Which specific application or system do you mean?"
	}
}

func isExactDeterministicDiagnosticIntent(message string) bool {
	if requiresTypedDeploymentIdentity(message) || isDiagnosticReasoningQuestion(message) ||
		isRepositoryCodeQuestion(message) || !isExplicitDiagnosticLookup(message) {
		return false
	}
	lower := strings.ToLower(message)
	if isExplicitLogLookup(message) {
		return true
	}
	for _, term := range []string{
		"running", "listening", "port", "repository commit", "same version", "last deployed",
	} {
		if messageContainsTerm(lower, term) {
			return true
		}
	}
	return false
}

func (a *app) handlePhase2CStream(w http.ResponseWriter, r *http.Request, question string, history []storedMessage) bool {
	if !isEvidenceSeekingQuestion(question) {
		return false
	}
	started := time.Now()
	now := a.phase2CNow()
	pipeline := a.phase2CPipeline()
	routerContext := a.buildProductionRouterContext(r.Context(), question, history, now)
	prepared, prepareErr := pipeline.Prepare(question, routerContext)
	if prepareErr != nil {
		if isPhase2CReasonedRoute(prepared.Route.ID) {
			a.emitPhase2CLimitation(w, prepared, nil, nil, started, "evidence_plan_invalid")
			return true
		}
		return false
	}
	if isRecognizedPhase2CAmbiguity(prepared.Route) {
		emitPhase2CClarification(w, prepared.Route, phase2CClarification(prepared.Route))
		return true
	}
	if prepared.Route.Resolution != RouteResolutionSupported || !isPhase2CReasonedRoute(prepared.Route.ID) {
		return false
	}

	result, err := pipeline.ExecutePrepared(r.Context(), prepared)
	if err != nil {
		a.emitPhase2CLimitation(w, prepared, nil, nil, started, "evidence_pipeline_failed")
		return true
	}

	sys, err := a.phase2CSystemStatus()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return true
	}
	ollama := a.phase2COllamaStatus(r.Context())
	if !ollama.Reachable {
		writeError(w, http.StatusServiceUnavailable, "ollama is unavailable")
		return true
	}
	policy := choosePolicy(sys, ollama.LoadedModels)
	if !policy.Allowed {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":  "MiniAI will not load a model while the Dell is under resource pressure",
			"policy": policy,
		})
		return true
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return true
	}
	setSSEHeaders(w)
	sendSSE(w, "meta", a.withInferenceProfileMetadata(policy.Model, map[string]any{
		"answer_mode": "phase2c", "route": prepared.Route.ID,
		"model": policy.Model, "mode": policy.Mode, "agent": true,
		"planner_calls": 0, "reasoner_calls": 1,
		"tool_calls":            result.Plan.LogicalReads,
		"evidence_packet_bytes": len(result.PacketJSON),
	}))
	emitPhase2CToolEvents(w, flusher, result.Plan, result.Results)
	flusher.Flush()

	var reasonerResponse chatAPIResponse
	var reasonerErr error
	reasonerCalls := 0
	ran, leaseErr := a.withProtectedModelPowerLease(r.Context(), policy.Model, func() error {
		reasonerCalls = 1
		reasonerResponse, reasonerErr = a.callOnePassReasonerWithKeepalive(
			r.Context(), policy.Model, question, result.PacketJSON, nil,
		)
		return nil
	})
	if !ran {
		reasonerErr = fmt.Errorf("protected model power lease unavailable")
	}
	logPowerLeaseError(leaseErr)

	answer := safeReasonerLimitation()
	validation := "transport_error"
	confidence := ConfidenceLow
	if reasonerErr == nil {
		draft, validationErr := parseReasonerDraft(reasonerResponse.Message.Content, result.Packet)
		if validationErr == nil {
			confidence = result.Evidence.Confidence.SoftwareCeiling
			if confidence != ConfidenceHigh && confidence != ConfidenceMedium && confidence != ConfidenceLow {
				confidence = ConfidenceLow
			}
			answer = renderReasonerAnswer(draft, confidence)
			validation = "valid"
		} else {
			validation = "invalid"
		}
	}
	emitBufferedAnswer(w, flusher, answer)
	emitPhase2CDone(w, flusher, policy, prepared.Route.ID, result.Plan.LogicalReads, len(result.PacketJSON), reasonerCalls, reasonerResponse, started, validation, confidence)
	return true
}

func (a *app) phase2CSystemStatus() (systemStatus, error) {
	if a != nil && a.systemStatusReader != nil {
		return a.systemStatusReader()
	}
	return readSystemStatus()
}

func (a *app) phase2COllamaStatus(ctx context.Context) ollamaStatus {
	if a != nil && a.ollamaStatusReader != nil {
		return a.ollamaStatusReader(ctx)
	}
	return a.getOllamaStatus(ctx)
}

func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
}

func emitPhase2CToolEvents(w http.ResponseWriter, flusher http.Flusher, plan EvidencePlan, results []EvidenceResult) {
	for _, request := range plan.Requests {
		sendSSE(w, "tool", agentToolEvent{
			RequestID: request.ID, Phase: "start", Name: request.Capability,
			Arguments: cloneCanonicalArguments(request.Arguments),
		})
	}
	flusher.Flush()
	for _, result := range results {
		sendSSE(w, "tool", agentToolEvent{
			RequestID: result.RequestID, Phase: "result", Name: result.Capability,
			Arguments: cloneCanonicalArguments(result.Arguments), Summary: phase2CResultSummary(result),
		})
	}
	flusher.Flush()
}

func phase2CResultSummary(result EvidenceResult) string {
	if summary := strings.TrimSpace(result.SafeSummary); summary != "" {
		return summary
	}
	if result.DependencyError {
		return "evidence dependency was unavailable"
	}
	if result.ErrorCategory != "" {
		return fmt.Sprintf("evidence unavailable (%s)", result.ErrorCategory)
	}
	return "evidence read completed"
}

func emitPhase2CDone(w http.ResponseWriter, flusher http.Flusher, policy modelPolicy, route InvestigationRouteID, toolCalls, packetBytes, reasonerCalls int, response chatAPIResponse, started time.Time, validation string, confidence ConfidenceLevel) {
	tokensPerSecond := 0.0
	if response.EvalDuration > 0 {
		tokensPerSecond = float64(response.EvalCount) / (float64(response.EvalDuration) / 1e9)
	}
	sendSSE(w, "done", map[string]any{
		"answer_mode": "phase2c", "route": route,
		"model": policy.Model, "mode": policy.Mode, "agent": true, "model_invoked": reasonerCalls == 1,
		"planner_calls": 0, "reasoner_calls": reasonerCalls, "tool_calls": toolCalls,
		"evidence_packet_bytes": packetBytes, "answer_validation": validation,
		"confidence":            confidence,
		"prompt_tokens":         response.PromptEvalCount,
		"prompt_seconds":        round2(float64(response.PromptEvalDuration) / 1e9),
		"tokens":                response.EvalCount,
		"tokens_per_second":     round2(tokensPerSecond),
		"load_seconds":          round2(float64(response.LoadDuration) / 1e9),
		"total_seconds":         round2(float64(response.TotalDuration) / 1e9),
		"investigation_seconds": round2(time.Since(started).Seconds()),
	})
	flusher.Flush()
}

func (a *app) emitPhase2CLimitation(w http.ResponseWriter, prepared Phase2BPrepared, results []EvidenceResult, packet []byte, started time.Time, reason string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	setSSEHeaders(w)
	sendSSE(w, "meta", map[string]any{
		"answer_mode": "phase2c", "route": prepared.Route.ID, "agent": true,
		"planner_calls": 0, "reasoner_calls": 0, "tool_calls": prepared.Plan.LogicalReads,
		"evidence_packet_bytes": len(packet), "limitation": reason,
	})
	if len(prepared.Plan.Requests) > 0 {
		if len(results) == 0 {
			results = phase2CUnavailableResults(prepared.Plan)
		}
		emitPhase2CToolEvents(w, flusher, prepared.Plan, results)
	}
	emitBufferedAnswer(w, flusher, safeReasonerLimitation())
	emitPhase2CDone(w, flusher, modelPolicy{}, prepared.Route.ID, prepared.Plan.LogicalReads, len(packet), 0, chatAPIResponse{}, started, reason, ConfidenceLow)
}

func phase2CUnavailableResults(plan EvidencePlan) []EvidenceResult {
	results := make([]EvidenceResult, 0, len(plan.Requests))
	for _, request := range plan.Requests {
		results = append(results, EvidenceResult{
			RequestID: request.ID, PlanOrder: request.Order, Capability: request.Capability,
			Arguments:    cloneCanonicalArguments(request.Arguments),
			Availability: AvailabilityUnavailable, ErrorCategory: EvidenceErrorInternal,
		})
	}
	return results
}

func emitPhase2CClarification(w http.ResponseWriter, decision ShadowRouteDecision, answer string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	setSSEHeaders(w)
	sendSSE(w, "meta", map[string]any{
		"answer_mode": "deterministic", "route": decision.ID, "agent": false,
		"model_invoked": false, "planner_calls": 0, "reasoner_calls": 0, "tool_calls": 0,
	})
	emitBufferedAnswer(w, flusher, answer)
	sendSSE(w, "done", map[string]any{
		"answer_mode": "deterministic", "route": decision.ID,
		"model_invoked": false, "planner_calls": 0, "reasoner_calls": 0, "tool_calls": 0, "tokens": 0,
	})
	flusher.Flush()
}
