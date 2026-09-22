package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type reactorLabCapabilityFixture struct {
	server   *httptest.Server
	requests []string
}

func newReactorLabCapabilityFixture(
	t *testing.T,
) *reactorLabCapabilityFixture {
	t.Helper()
	fixture := &reactorLabCapabilityFixture{}
	deployment := reactorLabTestDeployment("myscheduler")
	database := reactorLabTestDatabase()
	application := reactorLabApplicationSummary{
		ID:             "app:myscheduler",
		Name:           "myscheduler",
		LatestStatus:   "healthy",
		RestartCount:   2,
		LastObservedAt: reactorLabTestNow,
	}
	service := reactorLabServiceSeries{
		ID:   "reactorlab",
		Name: "ReactorLab",
		Points: []reactorLabServicePoint{{
			Timestamp:   reactorLabTestNow,
			SampleCount: 1,
			Available:   true,
			Status:      "healthy",
		}},
	}
	overview := reactorLabOverview{
		CollectedAt: reactorLabTestNow,
		System: reactorLabSection[reactorLabSystemSnapshot]{
			Available: true,
			Data:      pointerTo(reactorLabTestSystem()),
		},
		Recovery: reactorLabSection[reactorLabRecovery]{
			Available: true,
			Data:      pointerTo(reactorLabTestRecovery()),
		},
		Deployments: reactorLabSection[reactorLabDeploymentList]{
			Available: true,
			Data: pointerTo(reactorLabDeploymentList{
				Deployments: []reactorLabDeployment{deployment},
			}),
		},
		Databases: reactorLabSection[reactorLabDatabaseList]{
			Available: true,
			Data: pointerTo(reactorLabDatabaseList{
				CollectedAt: reactorLabTestNow,
				Databases:   []reactorLabDatabase{database},
			}),
		},
		Observability: reactorLabSection[reactorLabObservabilityState]{
			Available: true,
			Data: pointerTo(reactorLabObservabilityState{
				Applications: []reactorLabApplicationSummary{application},
				Services:     []reactorLabServiceSeries{service},
			}),
		},
	}

	fixture.server = httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		if request.Method != http.MethodGet {
			t.Fatalf("mutating request: %s %s", request.Method, request.URL.Path)
		}
		fixture.requests = append(
			fixture.requests,
			request.Method+" "+request.URL.RequestURI(),
		)
		switch request.URL.Path {
		case "/internal/miniai/v1/overview":
			_ = json.NewEncoder(response).Encode(overview)
		case "/internal/miniai/v1/deployments":
			_ = json.NewEncoder(response).Encode(
				reactorLabDeploymentList{
					Deployments: []reactorLabDeployment{deployment, {
						App:       "legacy",
						Strategy:  "vite-static",
						Status:    "running",
						Services:  []reactorLabDeploymentService{},
						Databases: []reactorLabDeploymentDatabase{},
					}},
				},
			)
		case "/internal/miniai/v1/deployments/myscheduler":
			_ = json.NewEncoder(response).Encode(deployment)
		case "/internal/miniai/v1/deployments/myscheduler/history":
			firstActivated := reactorLabTestNow.Add(-2 * time.Hour)
			secondActivated := reactorLabTestNow.Add(-48 * time.Hour)
			_ = json.NewEncoder(response).Encode(
				reactorLabDeploymentHistory{
					App: "myscheduler",
					Versions: []reactorLabDeploymentVersion{
						{
							App:      "myscheduler",
							Strategy: "node-express",
							Source: &reactorLabDeploymentSource{
								Provider:     "github",
								Repository:   "owner/myscheduler",
								Branch:       "main",
								RequestedRef: "refs/heads/main",
								CommitSHA:    strings.Repeat("a", 40),
							},
							ActivatedAt: &firstActivated,
							ArchivedAt:  reactorLabTestNow.Add(-time.Hour),
							ImageID:     "sha256:" + strings.Repeat("b", 64),
							Services:    []reactorLabDeploymentService{},
						},
						{
							App:      "myscheduler",
							Strategy: "node-express",
							Source: &reactorLabDeploymentSource{
								Branch:    "release",
								CommitSHA: strings.Repeat("d", 40),
							},
							ActivatedAt: &secondActivated,
							ArchivedAt:  reactorLabTestNow.Add(-24 * time.Hour),
							Services: []reactorLabDeploymentService{{
								Name:    "backend",
								ImageID: "sha256:" + strings.Repeat("e", 64),
							}},
						},
						{
							App:        "myscheduler",
							Strategy:   "node-express",
							ArchivedAt: reactorLabTestNow.Add(-72 * time.Hour),
							Services:   []reactorLabDeploymentService{},
						},
					},
				},
			)
		case "/internal/miniai/v1/databases":
			_ = json.NewEncoder(response).Encode(
				reactorLabDatabaseList{
					CollectedAt: reactorLabTestNow,
					Databases:   []reactorLabDatabase{database},
				},
			)
		case "/internal/miniai/v1/databases/" + database.ID:
			_ = json.NewEncoder(response).Encode(database)
		case "/internal/miniai/v1/databases/" + database.ID + "/backups":
			limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
			completedAt := reactorLabTestNow.Add(-30 * time.Minute)
			_ = json.NewEncoder(response).Encode(
				reactorLabBackupList{
					DatabaseID: database.ID,
					Limit:      limit,
					Backups: []reactorLabBackup{{
						ID:          "backup_22222222222222222222222222222222",
						DatabaseID:  database.ID,
						Kind:        "automatic",
						Status:      "ready",
						SizeBytes:   2048,
						CreatedAt:   reactorLabTestNow.Add(-time.Hour),
						CompletedAt: &completedAt,
					}},
				},
			)
		case "/internal/miniai/v1/observability/host":
			_ = json.NewEncoder(response).Encode(
				reactorLabHostHistory{
					Window: reactorLabTestWindow(),
					Points: []reactorLabHostPoint{{
						Timestamp:   reactorLabTestNow,
						SampleCount: 1,
						CPUAverage:  pointerTo(25.5),
					}},
				},
			)
		case "/internal/miniai/v1/observability/temperature":
			_ = json.NewEncoder(response).Encode(
				reactorLabTemperatureHistory{
					Window: reactorLabTestWindow(),
					Points: []reactorLabTemperaturePoint{{
						BucketStart: reactorLabTestNow.Add(-time.Minute),
						BucketEnd:   reactorLabTestNow,
						SampleCount: 3,
						MinCelsius:  40,
						AvgCelsius:  42,
						MaxCelsius:  44,
						PeakAt:      reactorLabTestNow.Add(-30 * time.Second),
					}},
				},
			)
		case "/internal/miniai/v1/observability/applications":
			_ = json.NewEncoder(response).Encode(
				reactorLabApplications{
					Window:       reactorLabTestWindow(),
					Applications: []reactorLabApplicationSummary{application},
				},
			)
		case "/internal/miniai/v1/observability/applications/" + application.ID:
			_ = json.NewEncoder(response).Encode(
				reactorLabApplicationHistory{
					Window: reactorLabTestWindow(),
					ID:     application.ID,
					Points: []reactorLabApplicationPoint{{
						Timestamp:    reactorLabTestNow,
						SampleCount:  1,
						CPUAverage:   12,
						Status:       "healthy",
						RestartCount: 2,
					}},
				},
			)
		case "/internal/miniai/v1/observability/services":
			_ = json.NewEncoder(response).Encode(
				reactorLabServiceHistory{
					Window:   reactorLabTestWindow(),
					Services: []reactorLabServiceSeries{service},
				},
			)
		case "/internal/miniai/v1/observability/events":
			limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
			_ = json.NewEncoder(response).Encode(
				reactorLabEvents{
					Window: reactorLabTestWindow(),
					Limit:  limit,
					Events: []reactorLabEvent{{
						ID:           "event-1",
						Source:       "reactorlab",
						Type:         "service_unavailable",
						ResourceType: "service",
						ResourceID:   "reactorlab",
						ResourceName: "ReactorLab",
						OccurredAt:   reactorLabTestNow,
						Summary:      "Service unavailable",
					}},
				},
			)
		case "/internal/miniai/v1/activity":
			limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
			_ = json.NewEncoder(response).Encode(
				reactorLabActivity{
					Limit: limit,
					Events: []reactorLabActivityEvent{{
						OccurredAt: reactorLabTestNow,
						Source:     "minibase",
						Kind:       "backup_ready",
						Severity:   "info",
						Subject:    "Backup ready",
						Message:    "Database backup completed.",
					}},
				},
			)
		case "/internal/miniai/v1/recovery":
			_ = json.NewEncoder(response).Encode(
				reactorLabTestRecovery(),
			)
		default:
			http.NotFound(response, request)
		}
	}))
	return fixture
}

func (fixture *reactorLabCapabilityFixture) close() {
	fixture.server.Close()
}

func TestPhase1CCapabilitiesUseTypedReactorLabContract(t *testing.T) {
	fixture := newReactorLabCapabilityFixture(t)
	defer fixture.close()
	root := t.TempDir()
	repository := filepath.Join(root, "myscheduler")
	if err := os.MkdirAll(
		filepath.Join(repository, ".git", "refs", "heads"),
		0o755,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(repository, ".git", "HEAD"),
		[]byte("ref: refs/heads/main\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(repository, ".git", "refs", "heads", "main"),
		[]byte(strings.Repeat("f", 40)+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	a := &app{
		reactorURL:    fixture.server.URL,
		minideployURL: "http://127.0.0.1:1",
		repoRoot:      root,
		client:        fixture.server.Client(),
	}

	result, _, err := a.executeAgentTool(
		context.Background(),
		"list_apps",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	apps := result.(reactorLabAppListResult)
	if len(apps.Apps) != 2 || apps.Apps[0].App != "legacy" ||
		apps.Apps[0].SourceAvailable ||
		apps.Apps[0].Source != nil ||
		!apps.Apps[1].SourceAvailable ||
		apps.Apps[1].Source == nil ||
		apps.Apps[1].Source.CommitSHA != strings.Repeat("a", 40) ||
		apps.Apps[1].LocalRepository.Commit != strings.Repeat("f", 12) ||
		apps.Apps[1].LocalRepository.Commit ==
			apps.Apps[1].Source.CommitSHA {

		t.Fatalf("apps=%+v", apps)
	}

	result, _, err = a.executeAgentTool(
		context.Background(),
		"get_app_context",
		map[string]any{"app": "myscheduler"},
	)
	if err != nil {
		t.Fatal(err)
	}
	appContext := result.(reactorLabAppContextResult)
	if !appContext.SourceAvailable ||
		appContext.Deployment.Source == nil ||
		appContext.Deployment.Source.Provider != "github" ||
		appContext.Deployment.Source.Repository != "owner/myscheduler" ||
		appContext.Deployment.Source.Branch != "main" ||
		appContext.Deployment.Source.RequestedRef != "refs/heads/main" ||
		appContext.Deployment.ActivatedAt == nil ||
		appContext.Deployment.ImageID == "" ||
		appContext.Deployment.Services[0].ImageID == "" ||
		len(appContext.Databases) != 1 ||
		!appContext.Overview.Available ||
		!appContext.Overview.System.Available {

		t.Fatalf("app context=%+v", appContext)
	}

	result, _, err = a.executeAgentTool(
		context.Background(),
		"read_deployment_history",
		map[string]any{"app": "myscheduler"},
	)
	if err != nil {
		t.Fatal(err)
	}
	history := result.(reactorLabDeploymentHistory)
	if len(history.Versions) != 3 ||
		history.Versions[0].Source.CommitSHA ==
			history.Versions[1].Source.CommitSHA ||
		history.Versions[0].ActivatedAt == nil ||
		history.Versions[0].ActivatedAt.Equal(
			history.Versions[0].ArchivedAt,
		) ||
		history.Versions[2].Source != nil ||
		history.Versions[2].ActivatedAt != nil ||
		history.Versions[1].Services[0].ImageID !=
			"sha256:"+strings.Repeat("e", 64) {

		t.Fatalf("history=%+v", history)
	}

	hostResult, _, err := a.executeAgentTool(
		context.Background(),
		"read_host_history",
		map[string]any{"range": "1h"},
	)
	if err != nil || len(hostResult.(reactorLabHostHistory).Points) != 1 {
		t.Fatalf("host=%+v error=%v", hostResult, err)
	}

	temperatureResult, _, err := a.executeAgentTool(
		context.Background(),
		"read_temperature_history",
		map[string]any{
			"from": "2026-09-22T17:00:00Z",
			"to":   "2026-09-22T18:00:00Z",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	temperature := temperatureResult.(reactorLabTemperatureHistory)
	if temperature.Points[0].MinCelsius != 40 ||
		temperature.Points[0].AvgCelsius != 42 ||
		temperature.Points[0].MaxCelsius != 44 ||
		temperature.Points[0].PeakAt.IsZero() {

		t.Fatalf("temperature=%+v", temperature)
	}

	applicationResult, _, err := a.executeAgentTool(
		context.Background(),
		"read_application_history",
		map[string]any{"app": "MyScheduler", "range": "1h"},
	)
	if err != nil {
		t.Fatal(err)
	}
	applicationHistory := applicationResult.(reactorLabResolvedApplicationHistory)
	if applicationHistory.Application.ID != "app:myscheduler" ||
		applicationHistory.History.ID != "app:myscheduler" {

		t.Fatalf("application history=%+v", applicationHistory)
	}

	serviceResult, _, err := a.executeAgentTool(
		context.Background(),
		"read_service_history",
		map[string]any{"service": "REACTORLAB", "range": "1h"},
	)
	if err != nil ||
		len(serviceResult.(reactorLabServiceHistory).Services) != 1 {

		t.Fatalf("service=%+v error=%v", serviceResult, err)
	}

	eventResult, _, err := a.executeAgentTool(
		context.Background(),
		"read_infrastructure_events",
		map[string]any{"range": "1h", "limit": 25},
	)
	if err != nil ||
		eventResult.(reactorLabEvents).Limit != 25 {

		t.Fatalf("events=%+v error=%v", eventResult, err)
	}

	databaseResult, _, err := a.executeAgentTool(
		context.Background(),
		"list_databases",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	databases := databaseResult.(reactorLabDatabaseList)
	if databases.Databases[0].Commits != 10 ||
		databases.Databases[0].RowsInserted != 7 ||
		databases.Databases[0].BackupCount != 1 {

		t.Fatalf("databases=%+v", databases)
	}

	backupResult, _, err := a.executeAgentTool(
		context.Background(),
		"read_database_backups",
		map[string]any{
			"database_id": "database_11111111111111111111111111111111",
			"limit":       20,
		},
	)
	if err != nil ||
		backupResult.(reactorLabBackupList).Limit != 20 {

		t.Fatalf("backups=%+v error=%v", backupResult, err)
	}

	activityResult, _, err := a.executeAgentTool(
		context.Background(),
		"read_activity",
		map[string]any{"limit": 20},
	)
	if err != nil ||
		activityResult.(reactorLabActivity).Limit != 20 {

		t.Fatalf("activity=%+v error=%v", activityResult, err)
	}

	recoveryResult, _, err := a.executeAgentTool(
		context.Background(),
		"read_recovery",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	recoveryJSON, err := json.Marshal(recoveryResult)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(recoveryJSON), "iTCO_wdt") ||
		strings.Contains(string(recoveryJSON), "previousBootId") ||
		strings.Contains(string(recoveryJSON), "recoveryBootId") {

		t.Fatalf("recovery=%s", recoveryJSON)
	}
}

func TestApplicationHistoryAmbiguityFailsBeforePathRequest(t *testing.T) {
	detailCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/internal/miniai/v1/observability/applications":
			_ = json.NewEncoder(response).Encode(
				reactorLabApplications{
					Window: reactorLabTestWindow(),
					Applications: []reactorLabApplicationSummary{
						{
							ID: "app:first", Name: "shared",
							LatestStatus:   "healthy",
							LastObservedAt: reactorLabTestNow,
						},
						{
							ID: "app:second", Name: "shared",
							LatestStatus:   "healthy",
							LastObservedAt: reactorLabTestNow,
						},
					},
				},
			)
		default:
			detailCalled = true
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	a := &app{reactorURL: server.URL, client: server.Client()}
	_, _, err := a.executeAgentTool(
		context.Background(),
		"read_application_history",
		map[string]any{"app": "shared", "range": "1h"},
	)
	if !errors.Is(err, errReactorLabApplicationAmbiguous) {
		t.Fatalf("error=%v", err)
	}
	if detailCalled {
		t.Fatal("ambiguous application was used in a detail path")
	}
}

func TestServiceHistoryAmbiguityFailsSafely(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		calls++
		if request.URL.Path != "/internal/miniai/v1/observability/services" {
			t.Fatalf("path=%q", request.URL.Path)
		}
		_ = json.NewEncoder(response).Encode(reactorLabServiceHistory{
			Window: reactorLabTestWindow(),
			Services: []reactorLabServiceSeries{
				{ID: "service:first", Name: "shared", Points: []reactorLabServicePoint{}},
				{ID: "service:second", Name: "shared", Points: []reactorLabServicePoint{}},
			},
		})
	}))
	defer server.Close()
	a := &app{reactorURL: server.URL, client: server.Client()}
	_, _, err := a.executeAgentTool(
		context.Background(),
		"read_service_history",
		map[string]any{"service": "shared", "range": "1h"},
	)
	if !errors.Is(err, errReactorLabServiceAmbiguous) {
		t.Fatalf("error=%v", err)
	}
	if calls != 1 {
		t.Fatalf("service ambiguity made %d requests, want one fixed collection read", calls)
	}
}

func TestCapabilityLimitsRejectBeforeReactorLabRequest(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		calls++
	}))
	defer server.Close()
	a := &app{reactorURL: server.URL, client: server.Client()}
	tests := []struct {
		name string
		args map[string]any
	}{
		{
			name: "read_infrastructure_events",
			args: map[string]any{"limit": 501},
		},
		{
			name: "read_database_backups",
			args: map[string]any{
				"database_id": "database_11111111111111111111111111111111",
				"limit":       201,
			},
		},
		{name: "read_activity", args: map[string]any{"limit": 201}},
		{name: "read_host_history", args: map[string]any{"range": 1}},
		{name: "read_service_history", args: map[string]any{"service": 1}},
	}
	for _, test := range tests {
		if _, _, err := a.executeAgentTool(
			context.Background(),
			test.name,
			test.args,
		); !errors.Is(err, errReactorLabInvalidRequest) {

			t.Fatalf("%s error=%v", test.name, err)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid limits reached ReactorLab %d times", calls)
	}
}

func TestPlatformOverviewCapabilityPreservesUnavailableSection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		if request.URL.Path != "/internal/miniai/v1/overview" {
			t.Fatalf("path=%q", request.URL.Path)
		}
		_ = json.NewEncoder(response).Encode(reactorLabOverview{
			CollectedAt: reactorLabTestNow,
			System: reactorLabSection[reactorLabSystemSnapshot]{
				Available: true,
				Data:      pointerTo(reactorLabTestSystem()),
			},
			Recovery: reactorLabSection[reactorLabRecovery]{
				Available: false, Error: "recovery_unavailable",
			},
			Deployments: reactorLabSection[reactorLabDeploymentList]{
				Available: false, Error: "deployment_source_unavailable",
			},
			Databases: reactorLabSection[reactorLabDatabaseList]{
				Available: false, Error: "database_source_unavailable",
			},
			Observability: reactorLabSection[reactorLabObservabilityState]{
				Available: false, Error: "observability_unavailable",
			},
		})
	}))
	defer server.Close()
	a := &app{reactorURL: server.URL, client: server.Client()}
	result, _, err := a.executeAgentTool(
		context.Background(),
		"get_platform_overview",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	overview := result.(reactorLabOverview)
	if overview.Databases.Available ||
		overview.Databases.Error != "database_source_unavailable" ||
		overview.Databases.Data != nil {

		t.Fatalf("overview=%+v", overview)
	}
}
