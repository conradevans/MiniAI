package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeToolRepo(t *testing.T) (string, *app) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "myscheduler")
	if err := os.MkdirAll(filepath.Join(repo, "backend", "routes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "node_modules", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "backend", "routes", "schedule.js"), []byte("const schedule = true;\nconst API_TOKEN = supersecret;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("PASSWORD=hidden\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "node_modules", "x", "index.js"), []byte("schedule hidden noise\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, &app{repoRoot: root, client: http.DefaultClient}
}

func TestRepositoryPathBlocksTraversalAndSecrets(t *testing.T) {
	_, a := makeToolRepo(t)
	for _, p := range []string{"../other", ".env", ".git/HEAD", "node_modules/x/index.js"} {
		if _, _, _, err := a.resolveRepositoryPath("myscheduler", p); err == nil {
			t.Fatalf("expected %q to be blocked", p)
		}
	}
}

func TestReadRepositoryFileRedactsSecretAssignments(t *testing.T) {
	_, a := makeToolRepo(t)
	got, err := a.readRepositoryFile("myscheduler", "backend/routes/schedule.js")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Redacted || strings.Contains(got.Content, "supersecret") || !strings.Contains(got.Content, "[REDACTED]") {
		t.Fatalf("expected redaction, got %+v", got)
	}
}

func TestSearchRepositorySkipsBlockedDirectories(t *testing.T) {
	_, a := makeToolRepo(t)
	got, err := a.searchRepository(context.Background(), "myscheduler", ".", "schedule")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) != 1 || got.Hits[0].Path != "backend/routes/schedule.js" {
		t.Fatalf("unexpected search results: %+v", got)
	}
}

func TestTrimLogOutput(t *testing.T) {
	got, truncated := trimLogOutput("one\ntwo\nthree\nfour\n", 2, 1024)
	if !truncated || got != "three\nfour" {
		t.Fatalf("unexpected trimmed logs %q truncated=%v", got, truncated)
	}
}

func TestMiniDeployRuntimeLogsAreBoundedAndScrubbed(t *testing.T) {
	reactor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"deployments": []any{map[string]any{"app": "myscheduler"}}})
	}))
	defer reactor.Close()
	mini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"app": "myscheduler", "container": "frontend, backend",
			"logs": "one\nPASSWORD=hunter2\nthree\nfour\n",
		})
	}))
	defer mini.Close()
	a := &app{reactorURL: reactor.URL, minideployURL: mini.URL, client: http.DefaultClient}
	got, err := a.readMiniDeployLogs(context.Background(), "myscheduler", "runtime", 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.Lines != 3 || strings.Contains(got.Logs, "hunter2") || !got.Redacted {
		t.Fatalf("unexpected logs: %+v", got)
	}
}
