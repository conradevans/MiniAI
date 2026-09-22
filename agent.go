package main

import (
	"log"
	"net/http"
	"strings"
	"time"
)

type agentToolEvent struct {
	Phase     string         `json:"phase"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Summary   string         `json:"summary,omitempty"`
}

func (a *app) handleAgentChatStream(w http.ResponseWriter, r *http.Request, req chatRequest, policy modelPolicy, history []storedMessage) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	agentStarted := time.Now()
	budget := defaultInvestigationBudget()
	systemPrompt, contextApps := a.agentSystemPrompt(r.Context(), req.Message, history)
	sendSSE(w, "meta", a.withInferenceProfileMetadata(policy.Model, map[string]any{
		"model":          policy.Model,
		"mode":           policy.Mode,
		"context_apps":   contextApps,
		"agent":          true,
		"max_tool_calls": budget.MaxToolCalls,
	}))
	flusher.Flush()

	messages := []chatMessage{{Role: "system", Content: systemPrompt}}
	messages = append(messages, storedHistoryMessages(history)...)
	messages = append(messages, chatMessage{Role: "user", Content: req.Message})
	tools := agentToolDefinitions()
	executor := newCapabilityExecutor(a)
	toolCallsUsed := 0
	plannerCalls := 0
	usedAnyTool := false
	readPaths := map[string]bool{}
	var plannerContent string
	var plannerMetrics chatAPIResponse
	fastEvidence := make([]chatMessage, 0, agentEvidenceMaxFiles)
	sourceReads := 0

	// Exact deployment identity must come from the typed ReactorLab source
	// projection, never from a legacy browser/admin snapshot or local checkout.
	if requiresTypedDeploymentIdentity(req.Message) {
		for _, appName := range contextApps {
			if !budget.canCallTool(toolCallsUsed) {
				break
			}
			usedAnyTool = true
			toolCallsUsed++
			args := map[string]any{"app": appName}
			sendSSE(w, "tool", agentToolEvent{
				Phase: "start", Name: "get_app_context", Arguments: args,
				Summary: "typed deployment identity evidence floor",
			})
			flusher.Flush()

			result, summary, err := executor.Execute(
				r.Context(),
				"get_app_context",
				args,
			)
			if err != nil {
				result = map[string]any{"error": err.Error()}
				summary = "tool failed: " + err.Error()
			}
			messages = append(messages, chatMessage{
				Role:     "tool",
				ToolName: "get_app_context",
				Content: encodeAgentToolResult(compactAgentToolResult(
					"get_app_context",
					result,
				)),
			})
			sendSSE(w, "tool", agentToolEvent{
				Phase: "result", Name: "get_app_context",
				Summary: summary + " (typed deployment identity evidence floor)",
			})
			flusher.Flush()
		}
	}

	// Code-location questions must never terminate with a permission-seeking
	// answer such as "would you like me to search?". Seed one safe repository
	// search when exactly one app is already resolved. Simple location questions can
	// answer directly from that evidence; deeper questions continue to the planner.
	if requiresRepositoryEvidence(req.Message) && len(contextApps) == 1 {
		query := repositorySeedQuery(req.Message, contextApps)
		if query != "" && budget.canCallTool(toolCallsUsed) {
			args := map[string]any{"app": contextApps[0], "path": ".", "query": query}
			usedAnyTool = true
			toolCallsUsed++
			sendSSE(w, "tool", agentToolEvent{Phase: "start", Name: "search_repository", Arguments: args, Summary: "repository evidence floor"})
			flusher.Flush()

			result, summary, err := executor.Execute(r.Context(), "search_repository", args)
			if err != nil {
				result = map[string]any{"error": err.Error()}
				summary = "tool failed: " + err.Error()
			}
			searchMessage := chatMessage{Role: "tool", ToolName: "search_repository", Content: encodeAgentToolResult(compactAgentToolResult("search_repository", result))}
			messages = append(messages, searchMessage)
			sendSSE(w, "tool", agentToolEvent{Phase: "result", Name: "search_repository", Summary: summary + " (repository evidence floor)"})
			flusher.Flush()

			if search, ok := result.(repoSearchResponse); ok {
				candidates := repositoryLocationSourcePaths(search, readPaths, query)
				for _, candidate := range candidates {
					if !budget.canCallTool(toolCallsUsed) {
						break
					}
					toolCallsUsed++
					guardArgs := map[string]any{"app": search.App, "path": candidate}
					sendSSE(w, "tool", agentToolEvent{Phase: "start", Name: "read_repository_file", Arguments: guardArgs, Summary: "repository evidence floor"})
					flusher.Flush()
					guardResult, guardSummary, guardErr := executor.Execute(r.Context(), "read_repository_file", guardArgs)
					if guardErr != nil {
						guardResult = map[string]any{"error": guardErr.Error()}
						guardSummary = "tool failed: " + guardErr.Error()
					} else {
						readPaths[candidate] = true
					}
					readMessage := chatMessage{Role: "tool", ToolName: "read_repository_file", Content: encodeAgentToolResult(compactAgentToolResult("read_repository_file", guardResult))}
					messages = append(messages, readMessage)
					if file, ok := guardResult.(repoFileResponse); ok {
						if evidence, ok := compactRepositoryLocationEvidence(search, file); ok {
							fastEvidence = append(fastEvidence, chatMessage{
								Role: "tool", ToolName: "read_repository_file",
								Content: encodeAgentToolResult(evidence),
							})
							sourceReads++
						}
					}
					sendSSE(w, "tool", agentToolEvent{Phase: "result", Name: "read_repository_file", Summary: guardSummary + " (repository evidence floor)"})
					flusher.Flush()
				}
			}
		}
	}

	if isSimpleRepositoryLocationQuestion(req.Message) && len(contextApps) == 1 && sourceReads > 0 {
		fastMessages := repositoryLocationMessages(req.Message, contextApps[0], fastEvidence)
		promptRunes := repositoryMessageRunes(fastMessages)
		log.Printf("MiniAI repository fast path: app=%s evidence_files=%d prompt_runes=%d estimated_prompt_tokens=%d", contextApps[0], len(fastEvidence), promptRunes, (promptRunes+3)/4)
		if err := a.streamAgentFinal(r.Context(), w, flusher, policy, fastMessages, toolCallsUsed, 0, agentStarted); err != nil {
			sendSSE(w, "error", map[string]string{"error": err.Error()})
			flusher.Flush()
		}
		return
	}

	for round := 0; round < budget.MaxPlannerRounds && budget.canCallTool(toolCallsUsed); round++ {
		plannerCalls++
		planner, err := a.callAgentPlannerWithKeepalive(r.Context(), policy.Model, messages, tools, func() {
			writeSSEKeepalive(w, flusher)
		})
		if err != nil {
			sendSSE(w, "error", map[string]string{"error": err.Error()})
			flusher.Flush()
			return
		}
		plannerMetrics = planner
		messages = append(messages, planner.Message)
		calls := planner.Message.ToolCalls
		if len(calls) == 0 {
			plannerContent = strings.TrimSpace(planner.Message.Content)
			break
		}

		for _, call := range calls {
			if !budget.canCallTool(toolCallsUsed) {
				break
			}
			usedAnyTool = true
			toolCallsUsed++
			name := call.Function.Name
			args := call.Function.Arguments
			sendSSE(w, "tool", agentToolEvent{Phase: "start", Name: name, Arguments: args})
			flusher.Flush()

			result, summary, err := executor.Execute(r.Context(), name, args)
			if err != nil {
				result = map[string]any{"error": err.Error()}
				summary = "tool failed: " + err.Error()
			}
			if name == "read_repository_file" {
				if path := stringArg(args, "path"); path != "" {
					readPaths[path] = true
				}
			}
			content := encodeAgentToolResult(compactAgentToolResult(name, result))
			messages = append(messages, chatMessage{Role: "tool", ToolName: name, Content: content})
			sendSSE(w, "tool", agentToolEvent{Phase: "result", Name: name, Summary: summary})
			flusher.Flush()

			if name == "search_repository" && requiresRepositoryEvidence(req.Message) && budget.canCallTool(toolCallsUsed) {
				if search, ok := result.(repoSearchResponse); ok {
					query := stringArg(args, "query")
					candidates := bestUnreadSourcePaths(search, readPaths, query, agentEvidenceMaxFiles)
					for _, candidate := range candidates {
						if !budget.canCallTool(toolCallsUsed) {
							break
						}
						toolCallsUsed++
						guardArgs := map[string]any{"app": search.App, "path": candidate}
						sendSSE(w, "tool", agentToolEvent{Phase: "start", Name: "read_repository_file", Arguments: guardArgs, Summary: "evidence guardrail"})
						flusher.Flush()
						guardResult, guardSummary, guardErr := executor.Execute(r.Context(), "read_repository_file", guardArgs)
						if guardErr != nil {
							guardResult = map[string]any{"error": guardErr.Error()}
							guardSummary = "tool failed: " + guardErr.Error()
						} else {
							readPaths[candidate] = true
						}
						messages = append(messages, chatMessage{Role: "tool", ToolName: "read_repository_file", Content: encodeAgentToolResult(compactAgentToolResult("read_repository_file", guardResult))})
						sendSSE(w, "tool", agentToolEvent{Phase: "result", Name: "read_repository_file", Summary: guardSummary + " (evidence guardrail)"})
						flusher.Flush()
					}
				}
			}
		}
	}

	if !usedAnyTool {
		if plannerContent == "" {
			plannerContent = "I could not produce a supported answer from the available context."
		}
		emitBufferedAnswer(w, flusher, plannerContent)
		tokensPerSecond := 0.0
		if plannerMetrics.EvalDuration > 0 {
			tokensPerSecond = float64(plannerMetrics.EvalCount) / (float64(plannerMetrics.EvalDuration) / 1e9)
		}
		sendSSE(w, "done", map[string]any{
			"model":             policy.Model,
			"mode":              policy.Mode,
			"agent":             true,
			"tool_calls":        0,
			"planner_calls":     plannerCalls,
			"tokens":            plannerMetrics.EvalCount,
			"tokens_per_second": round2(tokensPerSecond),
			"load_seconds":      round2(float64(plannerMetrics.LoadDuration) / 1e9),
			"total_seconds":     round2(float64(plannerMetrics.TotalDuration) / 1e9),
			"agent_seconds":     round2(time.Since(agentStarted).Seconds()),
		})
		flusher.Flush()
		return
	}

	if plannerContent != "" {
		emitBufferedAnswer(w, flusher, plannerContent)
		tokensPerSecond := 0.0
		if plannerMetrics.EvalDuration > 0 {
			tokensPerSecond = float64(plannerMetrics.EvalCount) / (float64(plannerMetrics.EvalDuration) / 1e9)
		}
		sendSSE(w, "done", map[string]any{
			"model":             policy.Model,
			"mode":              policy.Mode,
			"agent":             true,
			"tool_calls":        toolCallsUsed,
			"planner_calls":     plannerCalls,
			"tokens":            plannerMetrics.EvalCount,
			"tokens_per_second": round2(tokensPerSecond),
			"load_seconds":      round2(float64(plannerMetrics.LoadDuration) / 1e9),
			"total_seconds":     round2(float64(plannerMetrics.TotalDuration) / 1e9),
			"agent_seconds":     round2(time.Since(agentStarted).Seconds()),
		})
		flusher.Flush()
		return
	}

	messages = append(messages, chatMessage{
		Role:    "system",
		Content: "Tool gathering is complete. Give the user the final answer now. Use only supported evidence, stay concise unless detail was requested, and do not request more tools.",
	})
	if err := a.streamAgentFinal(r.Context(), w, flusher, policy, messages, toolCallsUsed, plannerCalls, agentStarted); err != nil {
		sendSSE(w, "error", map[string]string{"error": err.Error()})
		flusher.Flush()
	}
}
