package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type createChatRequest struct {
	Title string `json:"title"`
}

type sseCaptureWriter struct {
	http.ResponseWriter
	flusher     http.Flusher
	buffer      bytes.Buffer
	answer      strings.Builder
	evidence    []evidence
	pending     []pendingEvidence
	metadata    HistoryExecutionMetadata
	done        bool
	streamError bool
	statusCode  int
}

type pendingEvidence struct {
	RequestID    string
	Name         string
	Arguments    map[string]any
	Summary      string
	Completed    bool
	Status       string
	Availability string
}

func newSSECaptureWriter(w http.ResponseWriter) *sseCaptureWriter {
	flusher, _ := w.(http.Flusher)
	return &sseCaptureWriter{ResponseWriter: w, flusher: flusher}
}

func (w *sseCaptureWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		_, _ = w.buffer.Write(p[:n])
		w.consume()
	}
	return n, err
}

func (w *sseCaptureWriter) WriteHeader(statusCode int) {
	if w.statusCode == 0 {
		w.statusCode = statusCode
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *sseCaptureWriter) Flush() {
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

func (w *sseCaptureWriter) consume() {
	for {
		data := w.buffer.Bytes()
		idx := bytes.Index(data, []byte("\n\n"))
		if idx < 0 {
			return
		}
		block := append([]byte(nil), data[:idx]...)
		w.buffer.Next(idx + 2)
		w.consumeBlock(string(block))
	}
}

func (w *sseCaptureWriter) consumeBlock(block string) {
	event := ""
	payload := ""
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		}
		if strings.HasPrefix(line, "data: ") {
			payload = strings.TrimPrefix(line, "data: ")
		}
	}
	if event == "" || payload == "" {
		return
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return
	}
	switch event {
	case "token":
		if content, _ := obj["content"].(string); content != "" {
			w.answer.WriteString(content)
		}
	case "tool":
		w.captureTool(obj)
	case "meta":
		w.metadata.mergeSSE(obj)
	case "done":
		w.metadata.mergeSSE(obj)
		w.done = true
	case "error":
		w.streamError = true
		if w.metadata.ErrorCategory == "" {
			w.metadata.ErrorCategory = "stream_error"
		}
	}
}

func (w *sseCaptureWriter) captureTool(obj map[string]any) {
	phase, _ := obj["phase"].(string)
	name, _ := obj["name"].(string)
	requestID, _ := obj["request_id"].(string)
	if name == "" {
		return
	}
	switch phase {
	case "start":
		args, _ := obj["arguments"].(map[string]any)
		summary, _ := obj["summary"].(string)
		w.pending = append(w.pending, pendingEvidence{
			RequestID: requestID, Name: name, Arguments: sanitizeHistoryArguments(args), Summary: summary,
			Status: "running",
		})
	case "result":
		summary, _ := obj["summary"].(string)
		resultArgs, _ := obj["arguments"].(map[string]any)
		for i := len(w.pending) - 1; i >= 0; i-- {
			pending := &w.pending[i]
			if pending.Completed {
				continue
			}
			if requestID != "" {
				if pending.RequestID != requestID {
					continue
				}
			} else if pending.Name != name {
				continue
			}
			pending.Completed = true
			pending.Status = "completed"
			if len(resultArgs) > 0 {
				if pending.Arguments == nil {
					pending.Arguments = map[string]any{}
				}
				for key, value := range resultArgs {
					pending.Arguments[key] = value
				}
			}
			if summary == "" {
				summary = pending.Summary
			}
			pending.Summary = summary
			if availability, _ := obj["availability"].(string); availability != "" {
				pending.Availability = availability
			}
			appName, _ := pending.Arguments["app"].(string)
			path, _ := pending.Arguments["path"].(string)
			w.evidence = append(w.evidence, evidence{
				ToolName:  name,
				App:       appName,
				Source:    evidenceSource(name),
				Path:      path,
				Arguments: pending.Arguments,
				Summary:   summary,
			})
			return
		}
	}
}

func (metadata *HistoryExecutionMetadata) mergeSSE(object map[string]any) {
	mergeString := func(key string, target *string) {
		if value, ok := object[key].(string); ok {
			*target = value
		}
	}
	mergeInt := func(key string, target *int) {
		if value, ok := historyJSONInt(object[key]); ok {
			*target = value
		}
	}
	mergeFloat := func(key string, target *float64) {
		if value, ok := historyJSONFloat(object[key]); ok {
			*target = value
		}
	}
	mergeString("answer_mode", &metadata.AnswerMode)
	mergeString("route", &metadata.Route)
	mergeString("model", &metadata.Model)
	mergeString("mode", &metadata.ModelMode)
	mergeString("inference_profile", &metadata.InferenceProfile)
	mergeString("confidence", &metadata.Confidence)
	if metadata.Confidence == "" {
		mergeString("diagnostic_confidence", &metadata.Confidence)
	}
	mergeString("answer_validation", &metadata.AnswerValidation)
	if metadata.AnswerValidation == "transport_error" {
		metadata.ErrorCategory = "transport_error"
	}
	mergeString("error_category", &metadata.ErrorCategory)
	if limitation, ok := object["limitation"].(string); ok && limitation != "unsupported_local_evidence" {
		metadata.ErrorCategory = limitation
	}
	if value, ok := object["model_invoked"].(bool); ok {
		metadata.ModelInvoked = value
	}
	mergeInt("planner_calls", &metadata.PlannerCalls)
	mergeInt("reasoner_calls", &metadata.ReasonerCalls)
	mergeInt("evidence_rounds", &metadata.EvidenceRounds)
	if metadata.EvidenceRounds == 0 {
		mergeInt("investigation_rounds", &metadata.EvidenceRounds)
	}
	mergeInt("second_round_reads", &metadata.SecondRoundReads)
	mergeInt("tool_calls", &metadata.ToolCalls)
	mergeInt("evidence_packet_bytes", &metadata.EvidencePacketBytes)
	mergeInt("prompt_tokens", &metadata.PromptTokens)
	if _, exists := object["generated_tokens"]; exists {
		mergeInt("generated_tokens", &metadata.GeneratedTokens)
	} else {
		mergeInt("tokens", &metadata.GeneratedTokens)
	}
	mergeFloat("load_seconds", &metadata.LoadSeconds)
	mergeFloat("prompt_seconds", &metadata.PromptSeconds)
	mergeFloat("total_seconds", &metadata.TotalSeconds)
	if _, exists := object["investigation_seconds"]; exists {
		mergeFloat("investigation_seconds", &metadata.InvestigationSeconds)
	} else {
		mergeFloat("agent_seconds", &metadata.InvestigationSeconds)
	}
}

func historyJSONInt(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case json.Number:
		parsed, err := typed.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

func historyJSONFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func (w *sseCaptureWriter) capturedHistoryReads(finalStatus HistoryStatus) []HistoryEvidenceRead {
	secondRoundStart := len(w.pending) - w.metadata.SecondRoundReads
	if secondRoundStart < 0 {
		secondRoundStart = len(w.pending)
	}
	reads := make([]HistoryEvidenceRead, 0, len(w.pending))
	for index, pending := range w.pending {
		round := 1
		if w.metadata.EvidenceRounds >= 2 && index >= secondRoundStart {
			round = 2
		}
		status := pending.Status
		if !pending.Completed {
			if finalStatus == HistoryStatusCanceled {
				status = "canceled"
			} else {
				status = "failed"
			}
		}
		reads = append(reads, HistoryEvidenceRead{
			RequestID: pending.RequestID, EvidenceRound: round, Order: index, Capability: pending.Name,
			Arguments: pending.Arguments, SafeSummary: pending.Summary, Status: status,
			Availability: pending.Availability,
		})
	}
	return reads
}

func (a *app) handleListChats(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 500 {
			limit = n
		} else {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
	}
	chats, err := a.store.listChats(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not list chats")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": chats})
}

func (a *app) handleCreateChat(w http.ResponseWriter, r *http.Request) {
	var req createChatRequest
	if r.ContentLength != 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		if err := dec.Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
	}
	chat, err := a.store.createChat(req.Title)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create chat")
		return
	}
	writeJSON(w, http.StatusCreated, chat)
}

func (a *app) handleGetChat(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	detail, err := a.store.getChat(id)
	if errors.Is(err, errChatNotFound) {
		writeError(w, http.StatusNotFound, "chat not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read chat")
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (a *app) handleRenameChat(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	var req renameChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	chat, err := a.store.renameChat(id, req.Title)
	if errors.Is(err, errChatNotFound) {
		writeError(w, http.StatusNotFound, "chat not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, chat)
}

func (a *app) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	err := a.store.deleteChat(id)
	if errors.Is(err, errChatNotFound) {
		writeError(w, http.StatusNotFound, "chat not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not delete chat")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func chatHistoryText(history []storedMessage) string {
	if len(history) == 0 {
		return ""
	}
	var b strings.Builder
	for _, message := range history {
		role := strings.ToUpper(message.Role)
		if role != "USER" && role != "ASSISTANT" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", role, message.Content)
	}
	return strings.TrimSpace(b.String())
}

func chatContextQuery(history []storedMessage, current string) string {
	var b strings.Builder
	for _, message := range history {
		b.WriteString(message.Content)
		b.WriteByte('\n')
	}
	b.WriteString(current)
	return b.String()
}

func storedHistoryMessages(history []storedMessage) []chatMessage {
	out := make([]chatMessage, 0, len(history))
	for _, message := range history {
		if message.Role != "user" && message.Role != "assistant" {
			continue
		}
		out = append(out, chatMessage{Role: message.Role, Content: message.Content})
	}
	return out
}
