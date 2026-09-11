package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	chatHistoryMaxMessages = 12
	chatHistoryMaxRunes    = 6000
	chatTitleMaxRunes      = 72
)

var errChatNotFound = errors.New("chat not found")

type chatStore struct {
	db *sql.DB
}

type chatRecord struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type storedMessage struct {
	ID        int64      `json:"id"`
	ChatID    string     `json:"chat_id,omitempty"`
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	CreatedAt time.Time  `json:"created_at"`
	Evidence  []evidence `json:"evidence,omitempty"`
}

type evidence struct {
	ID        int64          `json:"id,omitempty"`
	MessageID int64          `json:"message_id,omitempty"`
	ToolName  string         `json:"tool_name"`
	App       string         `json:"app,omitempty"`
	Source    string         `json:"source,omitempty"`
	Path      string         `json:"path,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Summary   string         `json:"summary,omitempty"`
	CreatedAt time.Time      `json:"created_at,omitempty"`
}

type chatDetail struct {
	Chat     chatRecord      `json:"chat"`
	Messages []storedMessage `json:"messages"`
}

type renameChatRequest struct {
	Title string `json:"title"`
}

func openChatStore(path string) (*chatStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("chat database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create chat database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open chat database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &chatStore{db: db}
	if err := store.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *chatStore) initialize() error {
	for _, statement := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = NORMAL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS chats (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id TEXT NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
			role TEXT NOT NULL CHECK(role IN ('user','assistant')),
			content TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS messages_chat_created_idx
		 ON messages(chat_id, id)`,
		`CREATE TABLE IF NOT EXISTS evidence (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			tool_name TEXT NOT NULL,
			app TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL DEFAULT '',
			arguments_json TEXT NOT NULL DEFAULT '{}',
			summary TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS evidence_message_idx
		 ON evidence(message_id, id)`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("initialize chat database: %w", err)
		}
	}
	return nil
}

func (s *chatStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *chatStore) createChat(title string) (chatRecord, error) {
	id, err := randomID(16)
	if err != nil {
		return chatRecord{}, err
	}
	now := time.Now().UTC()
	title = normalizeChatTitle(title)
	if title == "" {
		title = "New chat"
	}
	_, err = s.db.Exec(
		`INSERT INTO chats(id, title, created_at, updated_at) VALUES(?,?,?,?)`,
		id, title, encodeDBTime(now), encodeDBTime(now),
	)
	if err != nil {
		return chatRecord{}, err
	}
	return chatRecord{ID: id, Title: title, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *chatStore) chatExists(id string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM chats WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *chatStore) listChats(limit int) ([]chatRecord, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(
		`SELECT id, title, created_at, updated_at
		 FROM chats ORDER BY updated_at DESC, created_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []chatRecord{}
	for rows.Next() {
		var c chatRecord
		var created, updated string
		if err := rows.Scan(&c.ID, &c.Title, &created, &updated); err != nil {
			return nil, err
		}
		c.CreatedAt = decodeDBTime(created)
		c.UpdatedAt = decodeDBTime(updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *chatStore) getChat(id string) (chatDetail, error) {
	var out chatDetail
	var created, updated string
	err := s.db.QueryRow(
		`SELECT id, title, created_at, updated_at FROM chats WHERE id = ?`, id,
	).Scan(&out.Chat.ID, &out.Chat.Title, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return chatDetail{}, errChatNotFound
	}
	if err != nil {
		return chatDetail{}, err
	}
	out.Chat.CreatedAt = decodeDBTime(created)
	out.Chat.UpdatedAt = decodeDBTime(updated)

	rows, err := s.db.Query(
		`SELECT id, role, content, created_at FROM messages
		 WHERE chat_id = ? ORDER BY id ASC`, id,
	)
	if err != nil {
		return chatDetail{}, err
	}
	out.Messages = []storedMessage{}
	for rows.Next() {
		var m storedMessage
		var createdAt string
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &createdAt); err != nil {
			_ = rows.Close()
			return chatDetail{}, err
		}
		m.ChatID = id
		m.CreatedAt = decodeDBTime(createdAt)
		out.Messages = append(out.Messages, m)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return chatDetail{}, err
	}
	if err := rows.Close(); err != nil {
		return chatDetail{}, err
	}
	for i := range out.Messages {
		out.Messages[i].Evidence, err = s.messageEvidence(out.Messages[i].ID)
		if err != nil {
			return chatDetail{}, err
		}
	}
	return out, nil
}

func (s *chatStore) messageEvidence(messageID int64) ([]evidence, error) {
	rows, err := s.db.Query(
		`SELECT id, tool_name, app, source, path, arguments_json, summary, created_at
		 FROM evidence WHERE message_id = ? ORDER BY id ASC`, messageID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []evidence{}
	for rows.Next() {
		var e evidence
		var argsJSON, created string
		if err := rows.Scan(&e.ID, &e.ToolName, &e.App, &e.Source, &e.Path, &argsJSON, &e.Summary, &created); err != nil {
			return nil, err
		}
		e.MessageID = messageID
		e.CreatedAt = decodeDBTime(created)
		e.Arguments = map[string]any{}
		_ = json.Unmarshal([]byte(argsJSON), &e.Arguments)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *chatStore) deleteChat(id string) error {
	result, err := s.db.Exec(`DELETE FROM chats WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errChatNotFound
	}
	return nil
}

func (s *chatStore) renameChat(id, title string) (chatRecord, error) {
	title = normalizeChatTitle(title)
	if title == "" {
		return chatRecord{}, fmt.Errorf("title is required")
	}
	now := time.Now().UTC()
	result, err := s.db.Exec(
		`UPDATE chats SET title = ?, updated_at = ? WHERE id = ?`,
		title, encodeDBTime(now), id,
	)
	if err != nil {
		return chatRecord{}, err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return chatRecord{}, errChatNotFound
	}
	var out chatRecord
	var created, updated string
	err = s.db.QueryRow(
		`SELECT id, title, created_at, updated_at FROM chats WHERE id = ?`, id,
	).Scan(&out.ID, &out.Title, &created, &updated)
	if err != nil {
		return chatRecord{}, err
	}
	out.CreatedAt = decodeDBTime(created)
	out.UpdatedAt = decodeDBTime(updated)
	return out, nil
}

func (s *chatStore) appendUserMessage(chatID, content string) (storedMessage, error) {
	return s.appendMessage(chatID, "user", content, nil)
}

func (s *chatStore) appendAssistantMessage(chatID, content string, items []evidence) (storedMessage, error) {
	return s.appendMessage(chatID, "assistant", content, items)
}

func (s *chatStore) appendMessage(chatID, role, content string, items []evidence) (storedMessage, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return storedMessage{}, fmt.Errorf("message content is required")
	}
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return storedMessage{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var title string
	err = tx.QueryRow(`SELECT title FROM chats WHERE id = ?`, chatID).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return storedMessage{}, errChatNotFound
	}
	if err != nil {
		return storedMessage{}, err
	}
	result, err := tx.Exec(
		`INSERT INTO messages(chat_id, role, content, created_at) VALUES(?,?,?,?)`,
		chatID, role, content, encodeDBTime(now),
	)
	if err != nil {
		return storedMessage{}, err
	}
	messageID, err := result.LastInsertId()
	if err != nil {
		return storedMessage{}, err
	}

	if role == "user" && title == "New chat" {
		newTitle := autoChatTitle(content)
		if newTitle != "" {
			if _, err := tx.Exec(`UPDATE chats SET title = ? WHERE id = ?`, newTitle, chatID); err != nil {
				return storedMessage{}, err
			}
		}
	}
	if _, err := tx.Exec(`UPDATE chats SET updated_at = ? WHERE id = ?`, encodeDBTime(now), chatID); err != nil {
		return storedMessage{}, err
	}

	cleanEvidence := make([]evidence, 0, len(items))
	if role == "assistant" {
		for _, item := range items {
			item = sanitizeEvidence(item)
			args, _ := json.Marshal(item.Arguments)
			result, err := tx.Exec(
				`INSERT INTO evidence(message_id, tool_name, app, source, path, arguments_json, summary, created_at)
				 VALUES(?,?,?,?,?,?,?,?)`,
				messageID, item.ToolName, item.App, item.Source, item.Path, string(args), item.Summary, encodeDBTime(now),
			)
			if err != nil {
				return storedMessage{}, err
			}
			item.ID, _ = result.LastInsertId()
			item.MessageID = messageID
			item.CreatedAt = now
			cleanEvidence = append(cleanEvidence, item)
		}
	}
	if err := tx.Commit(); err != nil {
		return storedMessage{}, err
	}
	return storedMessage{ID: messageID, ChatID: chatID, Role: role, Content: content, CreatedAt: now, Evidence: cleanEvidence}, nil
}

func (s *chatStore) history(chatID string, maxMessages, maxRunes int) ([]storedMessage, error) {
	if maxMessages < 1 {
		maxMessages = chatHistoryMaxMessages
	}
	if maxRunes < 1 {
		maxRunes = chatHistoryMaxRunes
	}
	rows, err := s.db.Query(
		`SELECT id, role, content, created_at FROM messages
		 WHERE chat_id = ? ORDER BY id DESC LIMIT ?`, chatID, maxMessages,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reversed := []storedMessage{}
	for rows.Next() {
		var m storedMessage
		var created string
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &created); err != nil {
			return nil, err
		}
		m.ChatID = chatID
		m.CreatedAt = decodeDBTime(created)
		reversed = append(reversed, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	selected := make([]storedMessage, 0, len(reversed))
	used := 0
	for _, m := range reversed {
		runes := len([]rune(m.Content))
		if used+runes > maxRunes {
			remaining := maxRunes - used
			if remaining <= 0 {
				break
			}
			m.Content = truncateRunes(m.Content, remaining)
			runes = len([]rune(m.Content))
		}
		selected = append(selected, m)
		used += runes
		if used >= maxRunes {
			break
		}
	}
	out := make([]storedMessage, 0, len(selected))
	for i := len(selected) - 1; i >= 0; i-- {
		out = append(out, selected[i])
	}
	for i := range out {
		if out[i].Role != "assistant" {
			continue
		}
		out[i].Evidence, err = s.messageEvidence(out[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func autoChatTitle(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if message == "" {
		return ""
	}
	return truncateRunes(message, chatTitleMaxRunes)
}

func normalizeChatTitle(title string) string {
	title = strings.Join(strings.Fields(title), " ")
	return truncateRunes(title, chatTitleMaxRunes)
}

func sanitizeEvidence(in evidence) evidence {
	out := evidence{
		ToolName:  truncateRunes(strings.TrimSpace(in.ToolName), 80),
		App:       truncateRunes(strings.TrimSpace(in.App), 120),
		Source:    truncateRunes(strings.TrimSpace(in.Source), 120),
		Path:      truncateRunes(strings.TrimSpace(in.Path), 500),
		Summary:   truncateRunes(strings.TrimSpace(in.Summary), 500),
		Arguments: map[string]any{},
	}
	out.Summary, _ = scrubSensitiveText(out.Summary)
	for _, key := range []string{"app", "path", "query", "lines"} {
		value, ok := in.Arguments[key]
		if !ok {
			continue
		}
		switch v := value.(type) {
		case string:
			v, _ = scrubSensitiveText(truncateRunes(strings.TrimSpace(v), 500))
			out.Arguments[key] = v
		case int, int64, float64, json.Number:
			out.Arguments[key] = v
		}
	}
	return out
}

func evidenceSource(toolName string) string {
	switch toolName {
	case "search_repository", "read_repository_file", "list_repository_directory":
		return "repository"
	case "read_runtime_logs":
		return "runtime_logs"
	case "read_deployment_logs":
		return "deployment_logs"
	case "read_deployment_history":
		return "deployment_history"
	case "read_current_deployment":
		return "current_deployment"
	case "get_app_context":
		return "app_context"
	case "list_apps":
		return "reactorlab"
	default:
		return "tool"
	}
}

func encodeDBTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func decodeDBTime(value string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, value)
	return t.UTC()
}
