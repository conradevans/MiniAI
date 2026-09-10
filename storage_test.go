package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestChatStorePersistsConversationAndEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miniai.db")
	store, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	chat, err := store.createChat("")
	if err != nil {
		t.Fatal(err)
	}
	if chat.Title != "New chat" {
		t.Fatalf("title=%q", chat.Title)
	}
	if _, err := store.appendUserMessage(chat.ID, "Remember cobalt for this chat"); err != nil {
		t.Fatal(err)
	}
	items := []evidence{{
		ToolName: "read_repository_file",
		App:      "myscheduler",
		Source:   "repository",
		Path:     "backend/app.js",
		Arguments: map[string]any{
			"app":  "myscheduler",
			"path": "backend/app.js",
		},
		Summary: "read backend/app.js",
	}}
	if _, err := store.appendAssistantMessage(chat.ID, "OK", items); err != nil {
		t.Fatal(err)
	}
	detail, err := store.getChat(chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Chat.Title == "New chat" || detail.Chat.Title == "" {
		t.Fatalf("expected automatic title, got %q", detail.Chat.Title)
	}
	if len(detail.Messages) != 2 {
		t.Fatalf("messages=%d", len(detail.Messages))
	}
	if len(detail.Messages[1].Evidence) != 1 {
		t.Fatalf("evidence=%+v", detail.Messages[1].Evidence)
	}
	if detail.Messages[1].Evidence[0].Path != "backend/app.js" {
		t.Fatalf("unexpected evidence: %+v", detail.Messages[1].Evidence[0])
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	detail, err = reopened.getChat(chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Messages) != 2 || detail.Messages[0].Content != "Remember cobalt for this chat" {
		t.Fatalf("conversation did not persist: %+v", detail.Messages)
	}
	history, err := reopened.history(chat.ID, 12, 6000)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[1].Content != "OK" {
		t.Fatalf("history=%+v", history)
	}
	if len(history[1].Evidence) != 1 ||
		history[1].Evidence[0].App != "myscheduler" ||
		history[1].Evidence[0].ToolName != "read_repository_file" {
		t.Fatalf("history evidence=%+v", history[1].Evidence)
	}
}

func TestChatStoreDeleteCascades(t *testing.T) {
	store, err := openChatStore(filepath.Join(t.TempDir(), "miniai.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	chat, err := store.createChat("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.appendUserMessage(chat.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.appendAssistantMessage(chat.ID, "hi", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.deleteChat(chat.ID); err != nil {
		t.Fatal(err)
	}
	_, err = store.getChat(chat.ID)
	if !errors.Is(err, errChatNotFound) {
		t.Fatalf("expected chat not found, got %v", err)
	}
}

func TestSanitizeEvidenceDropsUnknownArgumentsAndScrubsSecrets(t *testing.T) {
	got := sanitizeEvidence(evidence{
		ToolName: "search_repository",
		Arguments: map[string]any{
			"app":     "myscheduler",
			"query":   "password=supersecret",
			"unknown": "do-not-store",
		},
		Summary: "Authorization: Bearer abcdef",
	})
	if _, ok := got.Arguments["unknown"]; ok {
		t.Fatal("unknown argument was persisted")
	}
	if got.Arguments["query"] == "password=supersecret" {
		t.Fatal("secret-like query was not scrubbed")
	}
	if got.Summary == "Authorization: Bearer abcdef" {
		t.Fatal("summary was not scrubbed")
	}
}
