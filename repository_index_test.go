package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRepositoryIndex() *repositoryIndex {
	routeFile := repoFileResponse{
		App: "myscheduler", Path: "backend/routes/scheduleTemplateRoutes.js",
		Content: "const router = express.Router();\nrouter.get('/', listTemplates);\nrouter.post('/', createTemplate);\n",
	}
	genericRouteFile := repoFileResponse{
		App: "myscheduler", Path: "backend/routes/scheduleRoutes.js",
		Content: "const router = express.Router();\nrouter.get('/', listSchedule);\nrouter.post('/shifts', createShift);\n",
	}
	mountFile := repoFileResponse{
		App: "myscheduler", Path: "backend/app.js",
		Content: "import scheduleRoutes from './routes/scheduleRoutes.js';\nimport scheduleTemplateRoutes from './routes/scheduleTemplateRoutes.js';\napp.use('/api/schedule', scheduleRoutes);\napp.use('/api/schedule-templates', scheduleTemplateRoutes);\n",
	}
	entries := append(extractRepositoryIndexEntries(routeFile), extractRepositoryIndexEntries(genericRouteFile)...)
	entries = append(entries, extractRepositoryIndexEntries(mountFile)...)
	return &repositoryIndex{
		App: "myscheduler", Version: "test",
		Entries: entries,
		Files:   []string{routeFile.Path, genericRouteFile.Path, mountFile.Path},
	}
}

func TestDeterministicScheduleTemplateRouteLookup(t *testing.T) {
	result, ok := lookupRepositoryIndex(testRepositoryIndex(), "Which backend route handles schedule templates in MyScheduler?")
	if !ok {
		t.Fatal("expected deterministic route result")
	}
	if !strings.Contains(result.Answer, "backend/routes/scheduleTemplateRoutes.js") ||
		!strings.Contains(result.Answer, "GET /") ||
		!strings.Contains(result.Answer, "POST /") {
		t.Fatalf("unsupported route answer: %q", result.Answer)
	}
	if len(result.Evidence) != 2 || result.Evidence[1].Path != "backend/app.js" ||
		!strings.Contains(result.Answer, "/api/schedule-templates") {
		t.Fatalf("mount evidence missing: %+v answer=%q", result.Evidence, result.Answer)
	}
}

func TestDeterministicGenericScheduleRouteLookup(t *testing.T) {
	result, ok := lookupRepositoryIndex(testRepositoryIndex(), "Which backend route handles schedule in MyScheduler?")
	if !ok {
		t.Fatal("expected deterministic generic schedule route")
	}
	if !strings.Contains(result.Answer, "backend/routes/scheduleRoutes.js") ||
		strings.Contains(result.Answer, "scheduleTemplateRoutes.js") ||
		!strings.Contains(result.Answer, "/api/schedule") {
		t.Fatalf("wrong generic schedule route: %q", result.Answer)
	}
}

func TestRepositoryRouteCoverageOutranksRepeatedGenericTerm(t *testing.T) {
	entries := []repositoryIndexEntry{{
		Kind: "route", Path: "backend/routes/scheduleTemplateRoutes.js",
		Line: 10, Method: "GET", RoutePath: "/",
	}}
	for line := 20; line < 40; line++ {
		entries = append(entries, repositoryIndexEntry{
			Kind: "route", Path: "backend/routes/scheduleRoutes.js",
			Line: line, Method: "GET", RoutePath: fmt.Sprintf("/item/%d", line),
		})
	}
	index := &repositoryIndex{App: "myscheduler", Entries: entries}
	result, ok := lookupRepositoryIndex(index, "Which route handles schedule templates in MyScheduler?")
	if !ok || !strings.Contains(result.Answer, "scheduleTemplateRoutes.js") {
		t.Fatalf("distinct-term coverage lost to repeated generic matches: result=%+v ok=%v", result, ok)
	}
}

func TestRepositoryIndexResolvesSimpleSymbol(t *testing.T) {
	file := repoFileResponse{App: "minideploy", Path: "app/deploy.go", Content: "package main\n\nfunc createDeployment(name string) error { return nil }\n"}
	index := &repositoryIndex{App: "minideploy", Entries: extractRepositoryIndexEntries(file)}
	result, ok := lookupRepositoryIndex(index, "Which file contains createDeployment in MiniDeploy?")
	if !ok || !strings.Contains(result.Answer, "app/deploy.go") || !strings.Contains(result.Answer, "line 3") {
		t.Fatalf("result=%+v ok=%v", result, ok)
	}
}

func TestRepositoryIndexAmbiguityFallsBack(t *testing.T) {
	index := &repositoryIndex{App: "myscheduler", Entries: []repositoryIndexEntry{
		{Kind: "route", Path: "backend/routes/fooRoutes.js", Line: 2, Method: "GET", RoutePath: "/"},
		{Kind: "route", Path: "backend/routes/foo_routes.js", Line: 2, Method: "GET", RoutePath: "/"},
	}}
	if result, ok := lookupRepositoryIndex(index, "Which route handles foo in MyScheduler?"); ok {
		t.Fatalf("ambiguous lookup should fall back, got %+v", result)
	}
}

func makeIndexedRepo(t *testing.T) (string, *app, string) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "myscheduler")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "backend", "routes"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := "const router = express.Router();\nrouter.get('/', handler);\n"
	if err := os.WriteFile(filepath.Join(repo, "backend", "routes", "scheduleTemplateRoutes.js"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("1", 40)
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte(head+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, &app{repoRoot: root, client: http.DefaultClient}, head
}

func TestRepositoryIndexReuseAndGitInvalidation(t *testing.T) {
	repo, a, _ := makeIndexedRepo(t)
	first, hit, err := a.repositoryIndex(context.Background(), "myscheduler")
	if err != nil || hit {
		t.Fatalf("first index: hit=%v err=%v", hit, err)
	}
	second, hit, err := a.repositoryIndex(context.Background(), "myscheduler")
	if err != nil || !hit || first != second {
		t.Fatalf("cached index: hit=%v same=%v err=%v", hit, first == second, err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte(strings.Repeat("2", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	third, hit, err := a.repositoryIndex(context.Background(), "myscheduler")
	if err != nil || hit || third == second || third.Version == second.Version {
		t.Fatalf("refreshed index: hit=%v same=%v old=%q new=%q err=%v", hit, third == second, second.Version, third.Version, err)
	}
}

func TestRepositoryIndexExcludesSecretsAndBlockedPaths(t *testing.T) {
	repo, a, _ := makeIndexedRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("PASSWORD=hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "private.key"), []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "node_modules", "pkg", "secret.js"), []byte("const leaked = 'hunter2';\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	index, _, err := a.repositoryIndex(context.Background(), "myscheduler")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range index.Files {
		if strings.Contains(path, ".env") || strings.Contains(path, ".key") || strings.Contains(path, "node_modules") {
			t.Fatalf("blocked path indexed: %q", path)
		}
	}
	for _, entry := range index.Entries {
		encoded := entry.Kind + entry.Name + entry.Path + entry.Method + entry.RoutePath + entry.MountTarget
		if strings.Contains(encoded, "hunter2") {
			t.Fatalf("secret content indexed: %+v", entry)
		}
	}
}

func TestDeterministicHandlerRetainsEvidenceMetadata(t *testing.T) {
	_, indexed, _ := makeIndexedRepo(t)
	reactor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"deployments":[{"app":"myscheduler"}]}`))
	}))
	defer reactor.Close()
	indexed.reactorURL = reactor.URL

	recorder := httptest.NewRecorder()
	capture := newSSECaptureWriter(recorder)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	if !indexed.handleDeterministicRepositoryLookup(capture, req, "Which backend route handles schedule templates in MyScheduler?") {
		t.Fatal("expected deterministic handler")
	}
	if !capture.done || !strings.Contains(capture.answer.String(), "scheduleTemplateRoutes.js") {
		t.Fatalf("answer=%q done=%v", capture.answer.String(), capture.done)
	}
	if len(capture.evidence) != 1 || capture.evidence[0].Path != "backend/routes/scheduleTemplateRoutes.js" {
		t.Fatalf("evidence=%+v", capture.evidence)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"answer_mode":"deterministic"`) ||
		!strings.Contains(body, `"model_invoked":false`) {
		t.Fatalf("execution metadata missing: %s", body)
	}
}

func TestDiagnosticQuestionDoesNotBuildDeterministicIndex(t *testing.T) {
	_, a, _ := makeIndexedRepo(t)
	result, hit, ok := a.deterministicRepositoryLookup(context.Background(), "Why is the schedule template route failing in MyScheduler?")
	if ok || hit || result.Answer != "" || len(a.repoIndexes) != 0 {
		t.Fatalf("diagnostic lookup result=%+v hit=%v ok=%v cache=%d", result, hit, ok, len(a.repoIndexes))
	}
}

func TestRepositoryVersionTracksSafeSourceMetadata(t *testing.T) {
	repo, a, _ := makeIndexedRepo(t)
	first, firstFiles, err := a.repositoryVersion("myscheduler")
	if err != nil {
		t.Fatal(err)
	}
	unchanged, unchangedFiles, err := a.repositoryVersion("myscheduler")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != first || len(unchangedFiles) != len(firstFiles) {
		t.Fatalf("unchanged version mismatch: %q != %q", unchanged, first)
	}

	source := filepath.Join(repo, "backend", "routes", "scheduleTemplateRoutes.js")
	fixedTime := time.Unix(1_700_000_000, 123)
	if err := os.Chtimes(source, fixedTime, fixedTime); err != nil {
		t.Fatal(err)
	}
	mtimeVersion, _, err := a.repositoryVersion("myscheduler")
	if err != nil {
		t.Fatal(err)
	}
	if mtimeVersion == first {
		t.Fatal("mtime change did not change repository version")
	}

	if err := os.WriteFile(source, []byte("const router = express.Router();\nrouter.get('/', changedHandler);\nrouter.post('/', handler);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sizeVersion, _, err := a.repositoryVersion("myscheduler")
	if err != nil {
		t.Fatal(err)
	}
	if sizeVersion == mtimeVersion {
		t.Fatal("size change did not change repository version")
	}
}

func TestRepositoryVersionTracksAddedAndRemovedSafeSources(t *testing.T) {
	repo, a, _ := makeIndexedRepo(t)
	initial, _, err := a.repositoryVersion("myscheduler")
	if err != nil {
		t.Fatal(err)
	}
	addedPath := filepath.Join(repo, "backend", "routes", "extraRoutes.js")
	if err := os.WriteFile(addedPath, []byte("router.get('/extra', handler);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	added, _, err := a.repositoryVersion("myscheduler")
	if err != nil {
		t.Fatal(err)
	}
	if added == initial {
		t.Fatal("adding a safe source did not change repository version")
	}
	if err := os.Remove(addedPath); err != nil {
		t.Fatal(err)
	}
	removed, _, err := a.repositoryVersion("myscheduler")
	if err != nil {
		t.Fatal(err)
	}
	if removed == added || removed != initial {
		t.Fatalf("removal version=%q initial=%q added=%q", removed, initial, added)
	}
}

func TestRepositoryVersionIncludesGitCommit(t *testing.T) {
	repo, a, _ := makeIndexedRepo(t)
	first, _, err := a.repositoryVersion("myscheduler")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte(strings.Repeat("2", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, _, err := a.repositoryVersion("myscheduler")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("Git commit change did not change repository version")
	}
}

func TestRepositoryCacheInvalidatesForUncommittedSafeSourceChange(t *testing.T) {
	repo, a, _ := makeIndexedRepo(t)
	first, hit, err := a.repositoryIndex(context.Background(), "myscheduler")
	if err != nil || hit {
		t.Fatalf("first index: hit=%v err=%v", hit, err)
	}
	source := filepath.Join(repo, "backend", "routes", "scheduleTemplateRoutes.js")
	if err := os.WriteFile(source, []byte("router.get('/', changedHandler);\nrouter.post('/', handler);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, hit, err := a.repositoryIndex(context.Background(), "myscheduler")
	if err != nil || hit || second == first || second.Version == first.Version {
		t.Fatalf("dirty refresh: hit=%v same=%v old=%q new=%q err=%v", hit, second == first, first.Version, second.Version, err)
	}
}

func TestExcludedFilesDoNotChangeRepositoryVersion(t *testing.T) {
	repo, a, _ := makeIndexedRepo(t)
	initial, _, err := a.repositoryVersion("myscheduler")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("PASSWORD=hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "node_modules", "pkg", "ignored.js"), []byte("const ignored = true;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _, err := a.repositoryVersion("myscheduler")
	if err != nil {
		t.Fatal(err)
	}
	if after != initial {
		t.Fatalf("excluded files changed version: before=%q after=%q", initial, after)
	}
}
