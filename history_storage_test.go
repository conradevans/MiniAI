package main

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHistorySchemaFreshDatabaseAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miniai.db")
	store, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != miniAISchemaVersion {
		t.Fatalf("schema version=%d err=%v", version, err)
	}
	for _, table := range []string{"history_entries", "history_targets", "history_evidence_reads", "history_links"} {
		var name string
		if err := store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("missing table %s: %v", table, err)
		}
	}
	entry, _, err := store.startHistoryEntry("", "hello", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.finalizeHistoryEntry(entry.ID, historyFinalizeInput{
		Kind: HistoryKindConversation, Status: HistoryStatusSucceeded, ResultText: "hi", CompletedAt: time.Now().UTC(),
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
	defer reopened.Close()
	got, err := reopened.getHistoryEntry(entry.ID)
	if err != nil || got.Status != HistoryStatusSucceeded || got.ResultText != "hi" {
		t.Fatalf("reopened history=%+v err=%v", got, err)
	}
}

func TestHistoryMigrationPreservesPrePhase3ChatsMessagesAndEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`PRAGMA foreign_keys = ON`,
		`CREATE TABLE chats (id TEXT PRIMARY KEY, title TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, chat_id TEXT NOT NULL REFERENCES chats(id) ON DELETE CASCADE, role TEXT NOT NULL CHECK(role IN ('user','assistant')), content TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE TABLE evidence (id INTEGER PRIMARY KEY AUTOINCREMENT, message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE, tool_name TEXT NOT NULL, app TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '', arguments_json TEXT NOT NULL DEFAULT '{}', summary TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`,
		`INSERT INTO chats(id,title,created_at,updated_at) VALUES('legacy-chat','Legacy','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`,
		`INSERT INTO messages(id,chat_id,role,content,created_at) VALUES(1,'legacy-chat','user','old question','2026-09-01T00:00:00Z')`,
		`INSERT INTO messages(id,chat_id,role,content,created_at) VALUES(2,'legacy-chat','assistant','old answer','2026-09-01T00:00:01Z')`,
		`INSERT INTO evidence(message_id,tool_name,app,source,path,arguments_json,summary,created_at) VALUES(2,'get_app_context','myscheduler','app_context','','{"app":"myscheduler"}','safe summary','2026-09-01T00:00:01Z')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := store.getChat("legacy-chat")
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Messages) != 2 || detail.Messages[0].Content != "old question" ||
		len(detail.Messages[1].Evidence) != 1 || detail.Messages[1].Evidence[0].App != "myscheduler" {
		t.Fatalf("legacy content changed: %+v", detail)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.getChat("legacy-chat"); err != nil {
		t.Fatalf("idempotent reopen lost chat: %v", err)
	}
}

func TestHistoryTargetsEvidenceLinksAndSecretSanitizationPersist(t *testing.T) {
	store, err := openChatStore(filepath.Join(t.TempDir(), "miniai.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	one, _, err := store.startHistoryEntry("", strings.Join([]string{
		"Authorization: Bearer SECRET_TOKEN",
		"password=DB_PASSWORD",
		"CF_API_TOKEN=CLOUDFLARE_SECRET",
		"https://gituser:GIT_SECRET@example.test/repository.git",
		"-----BEGIN PRIVATE KEY-----\nPRIVATE_KEY_MATERIAL\n-----END PRIVATE KEY-----",
	}, "\n"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	started, completed := time.Now().UTC(), time.Now().UTC()
	one, err = store.finalizeHistoryEntry(one.ID, historyFinalizeInput{
		Kind: HistoryKindInvestigation, Status: HistoryStatusSucceeded,
		ResultText: "token=RESULT_SECRET\n<think>hidden reasoning</think>safe result", CompletedAt: completed,
		Metadata: HistoryExecutionMetadata{AnswerMode: "phase2c", Route: string(RouteDatabaseInvestigation), EvidenceRounds: 2, SecondRoundReads: 1, ToolCalls: 2},
		Targets:  []HistoryTarget{{Kind: "database", CanonicalID: "database_123", DisplayName: "Primary"}},
		EvidenceReads: []HistoryEvidenceRead{{
			RequestID: "request-1", EvidenceRound: 2, Capability: "read_database_backups",
			Arguments:   map[string]any{"database_id": "database_123", "query": "api_token=TOOL_SECRET", "unsafe": "DROP"},
			SafeSummary: "Bearer SUMMARY_SECRET", Status: "completed", StartedAt: &started, CompletedAt: &completed,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	two, _, err := store.startHistoryEntry("", "second", time.Now().UTC().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.finalizeHistoryEntry(two.ID, historyFinalizeInput{Kind: HistoryKindConversation, Status: HistoryStatusSucceeded, CompletedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.addHistoryLink(HistoryLink{FromEntryID: two.ID, ToEntryID: one.ID, Relation: "recovery_for"}); err != nil {
		t.Fatal(err)
	}

	got, err := store.getHistoryEntry(one.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded := canonicalJSON(got)
	for _, secret := range []string{
		"SECRET_TOKEN", "DB_PASSWORD", "CLOUDFLARE_SECRET", "GIT_SECRET", "PRIVATE_KEY_MATERIAL",
		"RESULT_SECRET", "hidden reasoning", "TOOL_SECRET", "SUMMARY_SECRET", "DROP",
	} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("secret %q persisted in %s", secret, encoded)
		}
	}
	if len(got.Targets) != 1 || len(got.EvidenceReads) != 1 || got.EvidenceReads[0].EvidenceRound != 2 {
		t.Fatalf("history children=%+v/%+v", got.Targets, got.EvidenceReads)
	}
	if !got.RequestRedacted || got.RequestTruncated || !got.ResultRedacted || got.ResultTruncated {
		t.Fatalf("history audit flags=%+v", got)
	}
	linked, err := store.getHistoryEntry(two.ID)
	if err != nil || len(linked.Links) != 1 || linked.Links[0].Relation != "recovery_for" {
		t.Fatalf("history links=%+v err=%v", linked.Links, err)
	}
}

func TestHistorySpecificSanitizerAddsProtectionWithoutChangingSharedScrubber(t *testing.T) {
	input := "Bearer HISTORY_TOKEN\n-----BEGIN RSA PRIVATE KEY-----\nHISTORY_KEY\n-----END RSA PRIVATE KEY-----"
	shared, _ := scrubSensitiveText(input)
	if shared != input {
		t.Fatalf("shared sanitizer unexpectedly changed additional History-only forms: %q", shared)
	}
	history, redacted, truncated := sanitizeHistoryAuditText(input, historyRequestMaxRunes)
	if !redacted || truncated || strings.Contains(history, "HISTORY_TOKEN") || strings.Contains(history, "HISTORY_KEY") {
		t.Fatalf("History sanitizer did not apply stronger protection: text=%q redacted=%t truncated=%t", history, redacted, truncated)
	}
}

func TestHistoryAuditCopyFlagsPreserveAuthoritativeChatBehavior(t *testing.T) {
	store, err := openChatStore(filepath.Join(t.TempDir(), "miniai.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	t.Run("safe short response matches", func(t *testing.T) {
		chat, err := store.createChat("safe")
		if err != nil {
			t.Fatal(err)
		}
		entry, _, err := store.startHistoryEntry(chat.ID, "ordinary request", time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		entry, err = store.finalizeHistoryEntry(entry.ID, historyFinalizeInput{
			Kind: HistoryKindConversation, Status: HistoryStatusSucceeded, ResultText: "ordinary response",
			AssistantText: "ordinary response", PersistAssistant: true, CompletedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		detail, err := store.getChat(chat.ID)
		if err != nil || len(detail.Messages) != 2 || entry.ResultText != detail.Messages[1].Content ||
			entry.RequestRedacted || entry.RequestTruncated || entry.ResultRedacted || entry.ResultTruncated {
			t.Fatalf("safe audit copy=%+v chat=%+v err=%v", entry, detail, err)
		}
	})

	t.Run("secret request and result retain raw chat", func(t *testing.T) {
		chat, err := store.createChat("secrets")
		if err != nil {
			t.Fatal(err)
		}
		requestText := "Bearer RAW_REQUEST_TOKEN"
		resultText := "password=RAW_RESULT_PASSWORD"
		entry, _, err := store.startHistoryEntry(chat.ID, requestText, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		entry, err = store.finalizeHistoryEntry(entry.ID, historyFinalizeInput{
			Kind: HistoryKindConversation, Status: HistoryStatusSucceeded, ResultText: resultText,
			AssistantText: resultText, PersistAssistant: true, CompletedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		detail, err := store.getChat(chat.ID)
		if err != nil || len(detail.Messages) != 2 {
			t.Fatalf("chat=%+v err=%v", detail, err)
		}
		if detail.Messages[0].Content != requestText || detail.Messages[1].Content != resultText {
			t.Fatalf("authoritative Chat messages changed: %+v", detail.Messages)
		}
		if !entry.RequestRedacted || entry.RequestTruncated || !entry.ResultRedacted || entry.ResultTruncated ||
			strings.Contains(entry.RequestText, "RAW_REQUEST_TOKEN") || strings.Contains(entry.ResultText, "RAW_RESULT_PASSWORD") {
			t.Fatalf("History audit policy not explicit: %+v", entry)
		}
	})

	t.Run("oversized result is explicitly truncated", func(t *testing.T) {
		chat, err := store.createChat("oversized")
		if err != nil {
			t.Fatal(err)
		}
		resultText := strings.Repeat("x", historyResultMaxRunes+50)
		entry, _, err := store.startHistoryEntry(chat.ID, "size request", time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		entry, err = store.finalizeHistoryEntry(entry.ID, historyFinalizeInput{
			Kind: HistoryKindConversation, Status: HistoryStatusSucceeded, ResultText: resultText,
			AssistantText: resultText, PersistAssistant: true, CompletedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		detail, err := store.getChat(chat.ID)
		if err != nil || len(detail.Messages) != 2 || detail.Messages[1].Content != resultText {
			t.Fatalf("authoritative oversized response changed: chat=%+v err=%v", detail, err)
		}
		if entry.ResultRedacted || !entry.ResultTruncated || len([]rune(entry.ResultText)) != historyResultMaxRunes {
			t.Fatalf("oversized audit copy=%+v", entry)
		}
	})

	t.Run("oversized request is explicitly truncated", func(t *testing.T) {
		chat, err := store.createChat("oversized request")
		if err != nil {
			t.Fatal(err)
		}
		requestText := strings.Repeat("q", historyRequestMaxRunes+50)
		entry, _, err := store.startHistoryEntry(chat.ID, requestText, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		detail, err := store.getChat(chat.ID)
		if err != nil || len(detail.Messages) != 1 || detail.Messages[0].Content != requestText {
			t.Fatalf("authoritative oversized request changed: chat=%+v err=%v", detail, err)
		}
		if entry.RequestRedacted || !entry.RequestTruncated || len([]rune(entry.RequestText)) != historyRequestMaxRunes {
			t.Fatalf("oversized request audit copy=%+v", entry)
		}
	})
}

func TestHistorySchemaRejectsMalformedPartialSchemaAndRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE chats (id TEXT PRIMARY KEY, title TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, chat_id TEXT NOT NULL REFERENCES chats(id) ON DELETE CASCADE, role TEXT NOT NULL, content TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE TABLE evidence (id INTEGER PRIMARY KEY AUTOINCREMENT, message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE, tool_name TEXT NOT NULL, app TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '', arguments_json TEXT NOT NULL DEFAULT '{}', summary TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`,
		`CREATE TABLE history_entries (id TEXT PRIMARY KEY, kind TEXT NOT NULL, status TEXT NOT NULL, chat_id TEXT, request_text TEXT NOT NULL, result_text TEXT NOT NULL DEFAULT '', started_at TEXT NOT NULL, route TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO chats(id,title,created_at,updated_at) VALUES('legacy-chat','Legacy','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`,
		`INSERT INTO messages(chat_id,role,content,created_at) VALUES('legacy-chat','user','still readable','2026-09-01T00:00:00Z')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if store, err := openChatStore(path); err == nil {
		_ = store.Close()
		t.Fatal("malformed partial schema was accepted")
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 0 {
		t.Fatalf("failed migration advanced version=%d err=%v", version, err)
	}
	var content string
	if err := db.QueryRow(`SELECT content FROM messages WHERE chat_id = 'legacy-chat'`).Scan(&content); err != nil || content != "still readable" {
		t.Fatalf("legacy data after rollback=%q err=%v", content, err)
	}
	var childTables int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='history_targets'`).Scan(&childTables); err != nil || childTables != 0 {
		t.Fatalf("migration objects were not rolled back count=%d err=%v", childTables, err)
	}
}

func TestHistorySchemaRejectsMalformedForeignKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "malformed-fk.db")
	store, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TABLE history_targets`,
		`CREATE TABLE history_targets (history_entry_id TEXT NOT NULL REFERENCES history_entries(id) ON DELETE RESTRICT, ordinal INTEGER NOT NULL, kind TEXT NOT NULL, canonical_id TEXT NOT NULL, display_name TEXT NOT NULL DEFAULT '', PRIMARY KEY(history_entry_id, ordinal))`,
		`CREATE INDEX history_targets_lookup_idx ON history_targets(kind, canonical_id, history_entry_id)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := openChatStore(path); err == nil {
		_ = reopened.Close()
		t.Fatal("malformed foreign key was accepted")
	}
}

func TestHistorySchemaRequiredIndexPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "indexes.db")
	store, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP INDEX history_entries_route_idx`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openChatStore(path)
	if err != nil {
		t.Fatalf("missing required index was not repaired: %v", err)
	}
	var indexName string
	if err := store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='history_entries_route_idx'`).Scan(&indexName); err != nil {
		t.Fatalf("repaired index missing: %v", err)
	}
	if _, err := store.db.Exec(`DROP INDEX history_entries_route_idx`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE INDEX history_entries_route_idx ON history_entries(route, id)`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := openChatStore(path); err == nil {
		_ = reopened.Close()
		t.Fatal("malformed required index was accepted")
	}
}

func TestHistorySchemaRejectsNewerVersionWithoutDowngrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "newer.db")
	store, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, miniAISchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := openChatStore(path); err == nil {
		_ = reopened.Close()
		t.Fatal("newer schema version was accepted")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != miniAISchemaVersion+1 {
		t.Fatalf("newer schema was changed version=%d err=%v", version, err)
	}
}

func TestSQLiteForeignKeysApplyToReplacementConnections(t *testing.T) {
	store, err := openChatStore(filepath.Join(t.TempDir(), "foreign-keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertEnabled := func() {
		var enabled int
		if err := store.db.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
			t.Fatalf("foreign_keys=%d err=%v", enabled, err)
		}
	}
	assertEnabled()
	store.db.SetMaxIdleConns(0)
	if err := store.db.Ping(); err != nil {
		t.Fatal(err)
	}
	assertEnabled()
	if _, err := store.db.Exec(`INSERT INTO history_targets(history_entry_id,ordinal,kind,canonical_id,display_name) VALUES('missing',0,'service','missing','')`); err == nil {
		t.Fatal("replacement connection accepted invalid history target foreign key")
	}
	if _, err := store.db.Exec(`INSERT INTO history_links(from_entry_id,to_entry_id,relation,created_at) VALUES('missing-a','missing-b','undo_of','2026-09-01T00:00:00.000000000Z')`); err == nil {
		t.Fatal("replacement connection accepted invalid history link foreign keys")
	}
}

func TestHistoryLinksAreBoundedAndDetailSignalsTruncation(t *testing.T) {
	store, err := openChatStore(filepath.Join(t.TempDir(), "links.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	hub, _, err := store.startHistoryEntry("", "hub", base)
	if err != nil {
		t.Fatal(err)
	}
	nodes := make([]HistoryEntry, 0, historyMaximumLinks+1)
	for index := 0; index <= historyMaximumLinks; index++ {
		node, _, err := store.startHistoryEntry("", fmt.Sprintf("node-%02d", index), base.Add(time.Duration(index+1)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	for index := 0; index < historyMaximumLinks; index++ {
		if err := store.addHistoryLink(HistoryLink{
			FromEntryID: hub.ID, ToEntryID: nodes[index].ID, Relation: "recovery_for", CreatedAt: base.Add(time.Duration(index) * time.Second),
		}); err != nil {
			t.Fatalf("link %d below bound: %v", index, err)
		}
	}
	if err := store.addHistoryLink(HistoryLink{FromEntryID: hub.ID, ToEntryID: nodes[historyMaximumLinks].ID, Relation: "recovery_for", CreatedAt: base.Add(historyMaximumLinks * time.Second)}); !errors.Is(err, errHistoryLinkLimit) {
		t.Fatalf("link above bound err=%v", err)
	}
	detail, err := store.getHistoryEntry(hub.ID)
	if err != nil || len(detail.Links) != historyMaximumLinks || detail.LinksTruncated {
		t.Fatalf("bounded detail=%+v err=%v", detail, err)
	}
	for index, link := range detail.Links {
		if link.ToEntryID != nodes[index].ID {
			t.Fatalf("links are not deterministic at %d: got=%s want=%s", index, link.ToEntryID, nodes[index].ID)
		}
	}
	if _, err := store.db.Exec(
		`INSERT INTO history_links(from_entry_id,to_entry_id,relation,created_at) VALUES(?,?,?,?)`,
		hub.ID, nodes[historyMaximumLinks].ID, "recovery_for", encodeHistoryTime(base.Add(historyMaximumLinks*time.Second)),
	); err != nil {
		t.Fatal(err)
	}
	detail, err = store.getHistoryEntry(hub.ID)
	if err != nil || len(detail.Links) != historyMaximumLinks || !detail.LinksTruncated {
		t.Fatalf("truncated detail count=%d flag=%t err=%v", len(detail.Links), detail.LinksTruncated, err)
	}
}

func TestChatDeletionPreservesHistoryAndNullsAssociations(t *testing.T) {
	store, err := openChatStore(filepath.Join(t.TempDir(), "miniai.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	chat, err := store.createChat("")
	if err != nil {
		t.Fatal(err)
	}
	entry, user, err := store.startHistoryEntry(chat.ID, "hello", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	entry, err = store.finalizeHistoryEntry(entry.ID, historyFinalizeInput{
		Kind: HistoryKindConversation, Status: HistoryStatusSucceeded, ResultText: "hi",
		AssistantText: "hi", PersistAssistant: true, CompletedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := store.getChat(chat.ID)
	if err != nil || len(detail.Messages) != 2 || detail.Messages[0].ID != user.ID ||
		detail.Messages[0].HistoryEntryID != entry.ID || detail.Messages[1].HistoryEntryID != entry.ID {
		t.Fatalf("chat history linkage=%+v err=%v", detail, err)
	}
	if err := store.deleteChat(chat.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.getChat(chat.ID); !errors.Is(err, errChatNotFound) {
		t.Fatalf("deleted chat err=%v", err)
	}
	preserved, err := store.getHistoryEntry(entry.ID)
	if err != nil || preserved.ChatID != "" || preserved.UserMessageID != 0 || preserved.AssistantMessageID != 0 || preserved.ResultText != "hi" {
		t.Fatalf("preserved history=%+v err=%v", preserved, err)
	}
}

func TestStartupRecoveryInterruptsOnlyStaleRunningHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miniai.db")
	store, err := openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	running, _, err := store.startHistoryEntry("", "running", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	complete, _, err := store.startHistoryEntry("", "complete", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.finalizeHistoryEntry(complete.ID, historyFinalizeInput{Kind: HistoryKindConversation, Status: HistoryStatusSucceeded, CompletedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = openChatStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	interrupted, err := store.getHistoryEntry(running.ID)
	if err != nil || interrupted.Status != HistoryStatusInterrupted || interrupted.ErrorCategory != "process_restart" || interrupted.CompletedAt == nil {
		t.Fatalf("interrupted=%+v err=%v", interrupted, err)
	}
	untouched, err := store.getHistoryEntry(complete.ID)
	if err != nil || untouched.Status != HistoryStatusSucceeded || untouched.ErrorCategory != "" {
		t.Fatalf("completed entry changed=%+v err=%v", untouched, err)
	}
}
