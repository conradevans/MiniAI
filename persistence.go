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
	flusher  http.Flusher
	buffer   bytes.Buffer
	answer   strings.Builder
	evidence []evidence
	pending  []pendingEvidence
	done     bool
}

type pendingEvidence struct {
	Name      string
	Arguments map[string]any
	Summary   string
	Completed bool
}

func newSSECaptureWriter(w http.ResponseWriter) *sseCaptureWriter {
	flusher, _ := w.(http.Flusher)
	return &sseCaptureWriter{ResponseWriter: w, flusher: flusher}
}

func (w *sseCaptureWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		_, _ = w.buffer.Write(p[:n])
		w.consume()
	}
	return n, err
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
	case "done":
		w.done = true
	}
}

func (w *sseCaptureWriter) captureTool(obj map[string]any) {
	phase, _ := obj["phase"].(string)
	name, _ := obj["name"].(string)
	if name == "" {
		return
	}
	switch phase {
	case "start":
		args, _ := obj["arguments"].(map[string]any)
		summary, _ := obj["summary"].(string)
		w.pending = append(w.pending, pendingEvidence{Name: name, Arguments: args, Summary: summary})
	case "result":
		summary, _ := obj["summary"].(string)
		for i := len(w.pending) - 1; i >= 0; i-- {
			pending := &w.pending[i]
			if pending.Completed || pending.Name != name {
				continue
			}
			pending.Completed = true
			if summary == "" {
				summary = pending.Summary
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
