package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var reactorLabTestNow = time.Date(
	2026,
	9,
	22,
	18,
	0,
	0,
	0,
	time.UTC,
)

func reactorLabTestWindow() reactorLabWindow {
	return reactorLabWindow{
		Range:         "1h",
		From:          reactorLabTestNow.Add(-time.Hour),
		To:            reactorLabTestNow,
		BucketSeconds: 15,
		MaxPoints:     reactorLabMaxMetricPoints,
	}
}

func reactorLabTestDeployment(appName string) reactorLabDeployment {
	activatedAt := reactorLabTestNow.Add(-time.Hour)
	return reactorLabDeployment{
		App:      appName,
		Strategy: "fullstack-vite-node",
		Status:   "running",
		Source: &reactorLabDeploymentSource{
			Provider:     "github",
			Repository:   "owner/" + appName,
			Branch:       "main",
			RequestedRef: "refs/heads/main",
			CommitSHA:    strings.Repeat("a", 40),
		},
		ActivatedAt: &activatedAt,
		ImageID:     "sha256:" + strings.Repeat("b", 64),
		Services: []reactorLabDeploymentService{{
			Name:    "backend",
			Status:  "running",
			ImageID: "sha256:" + strings.Repeat("c", 64),
		}},
		Databases: []reactorLabDeploymentDatabase{{
			ID:          "database_11111111111111111111111111111111",
			DisplayName: "Production",
			BindingName: "primary",
		}},
	}
}

func reactorLabTestDatabase() reactorLabDatabase {
	latest := reactorLabTestNow.Add(-time.Hour)
	age := float64(time.Hour / time.Second)
	return reactorLabDatabase{
		ID:                "database_11111111111111111111111111111111",
		DisplayName:       "Production",
		Status:            "ready",
		SizeBytes:         4096,
		Connections:       3,
		ActiveConnections: 1,
		IdleConnections:   2,
		Commits:           10,
		Rollbacks:         1,
		BlockReads:        5,
		BlockHits:         50,
		RowsInserted:      7,
		RowsUpdated:       8,
		RowsDeleted:       2,
		BackupCount:       1,
		BackupBytes:       2048,
		LatestBackupAt:    &latest,
		BackupAgeSeconds:  &age,
		Deployments: []reactorLabDatabaseDeployment{{
			App: "myscheduler", BindingName: "primary",
		}},
	}
}

func reactorLabTestSystem() reactorLabSystemSnapshot {
	return reactorLabSystemSnapshot{
		CPU: reactorLabCPUState{LogicalCores: 4},
		Temperature: reactorLabTemperatureState{
			Celsius: 40, Source: "hwmon",
		},
		Network:     reactorLabNetworkState{Interface: "eno1"},
		Battery:     reactorLabBatteryState{Status: "unknown"},
		Services:    []reactorLabServiceState{},
		CollectedAt: reactorLabTestNow,
	}
}

func reactorLabTestRecovery() reactorLabRecovery {
	return reactorLabRecovery{
		Protection: reactorLabProtectionState{
			State: "armed",
			HardwareWatchdog: reactorLabHardwareWatchdog{
				State: "armed", Identity: "iTCO_wdt",
			},
			RTC: reactorLabRTCState{State: "armed"},
		},
		RecentIncidents: []reactorLabRecoveryIncident{},
	}
}

func TestReactorLabReadClientUsesGETFixedPathsAndEncodedQueries(t *testing.T) {
	databaseID := "database_11111111111111111111111111111111"
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		if request.Method != http.MethodGet {
			t.Fatalf("method=%s want GET", request.Method)
		}
		requests = append(
			requests,
			request.Method+" "+request.URL.RequestURI(),
		)
		switch request.URL.Path {
		case "/internal/miniai/v1/observability/host":
			_ = json.NewEncoder(response).Encode(
				reactorLabHostHistory{
					Window: reactorLabTestWindow(),
					Points: []reactorLabHostPoint{},
				},
			)
		case "/internal/miniai/v1/observability/events":
			_ = json.NewEncoder(response).Encode(
				reactorLabEvents{
					Window: reactorLabTestWindow(),
					Limit:  17,
					Events: []reactorLabEvent{},
				},
			)
		case "/internal/miniai/v1/databases/" + databaseID + "/backups":
			_ = json.NewEncoder(response).Encode(
				reactorLabBackupList{
					DatabaseID: databaseID,
					Backups:    []reactorLabBackup{},
					Limit:      23,
				},
			)
		default:
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
	}))
	defer server.Close()
	client := newReactorLabReadClient(server.URL, server.Client())

	if _, err := client.HostHistory(
		context.Background(),
		reactorLabWindowRequest{Range: "6h"},
	); err != nil {
		t.Fatal(err)
	}
	from := "2026-09-22T10:00:00-04:00"
	to := "2026-09-22T10:20:00-04:00"
	if _, err := client.HostHistory(
		context.Background(),
		reactorLabWindowRequest{From: from, To: to},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Events(
		context.Background(),
		reactorLabWindowRequest{Range: "1h"},
		17,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Backups(
		context.Background(),
		databaseID,
		23,
	); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"GET /internal/miniai/v1/observability/host?range=6h",
		"GET /internal/miniai/v1/observability/host?from=2026-09-22T10%3A00%3A00-04%3A00&to=2026-09-22T10%3A20%3A00-04%3A00",
		"GET /internal/miniai/v1/observability/events?limit=17&range=1h",
		"GET /internal/miniai/v1/databases/" + databaseID + "/backups?limit=23",
	}
	if !sameStrings(requests, want) {
		t.Fatalf("requests=%v want %v", requests, want)
	}
}

func TestReactorLabOverviewPreservesPartialAvailability(t *testing.T) {
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

	overview, err := newReactorLabReadClient(
		server.URL,
		server.Client(),
	).Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !overview.System.Available || overview.System.Data == nil ||
		overview.Databases.Available ||
		overview.Databases.Error != "database_source_unavailable" ||
		overview.Databases.Data != nil {

		t.Fatalf("partial overview=%+v", overview)
	}
}

func TestReactorLabOverviewAllowsUnavailableTemperatureSensor(t *testing.T) {
	system := reactorLabTestSystem()
	system.Temperature = reactorLabTemperatureState{}
	server := httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		_ *http.Request,
	) {
		_ = json.NewEncoder(response).Encode(reactorLabOverview{
			CollectedAt: reactorLabTestNow,
			System: reactorLabSection[reactorLabSystemSnapshot]{
				Available: true,
				Data:      &system,
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

	if _, err := newReactorLabReadClient(
		server.URL,
		server.Client(),
	).Overview(context.Background()); err != nil {
		t.Fatalf("valid sensor-unavailable overview rejected: %v", err)
	}
}

func TestReactorLabReadClientRejectsMalformedAndUnsafeResponses(t *testing.T) {
	validDeployment := reactorLabTestDeployment("myscheduler")
	tests := []struct {
		name string
		body func(http.ResponseWriter)
		call func(reactorLabReadClient) error
		want error
	}{
		{
			name: "non 2xx hides body",
			body: func(response http.ResponseWriter) {
				http.Error(
					response,
					"PASSWORD=must-not-leak",
					http.StatusServiceUnavailable,
				)
			},
			call: func(client reactorLabReadClient) error {
				_, err := client.Deployments(context.Background())
				return err
			},
			want: errReactorLabUnavailable,
		},
		{
			name: "malformed json",
			body: func(response http.ResponseWriter) {
				_, _ = response.Write([]byte(`{"deployments":`))
			},
			call: func(client reactorLabReadClient) error {
				_, err := client.Deployments(context.Background())
				return err
			},
			want: errReactorLabMalformed,
		},
		{
			name: "trailing json",
			body: func(response http.ResponseWriter) {
				_, _ = response.Write([]byte(`{"deployments":[]} {}`))
			},
			call: func(client reactorLabReadClient) error {
				_, err := client.Deployments(context.Background())
				return err
			},
			want: errReactorLabMalformed,
		},
		{
			name: "oversized response",
			body: func(response http.ResponseWriter) {
				_, _ = response.Write([]byte(
					`{"deployments":[]}` +
						strings.Repeat(" ", reactorLabMaxResponseBytes),
				))
			},
			call: func(client reactorLabReadClient) error {
				_, err := client.Deployments(context.Background())
				return err
			},
			want: errReactorLabTooLarge,
		},
		{
			name: "missing required array",
			body: func(response http.ResponseWriter) {
				_, _ = response.Write([]byte(`{}`))
			},
			call: func(client reactorLabReadClient) error {
				_, err := client.Deployments(context.Background())
				return err
			},
			want: errReactorLabMalformed,
		},
		{
			name: "malformed deployment id",
			body: func(response http.ResponseWriter) {
				deployment := validDeployment
				deployment.App = "../escape"
				_ = json.NewEncoder(response).Encode(
					reactorLabDeploymentList{
						Deployments: []reactorLabDeployment{deployment},
					},
				)
			},
			call: func(client reactorLabReadClient) error {
				_, err := client.Deployments(context.Background())
				return err
			},
			want: errReactorLabMalformed,
		},
		{
			name: "malformed timestamp",
			body: func(response http.ResponseWriter) {
				out := reactorLabHostHistory{
					Window: reactorLabTestWindow(),
					Points: []reactorLabHostPoint{{
						SampleCount: 1,
					}},
				}
				_ = json.NewEncoder(response).Encode(out)
			},
			call: func(client reactorLabReadClient) error {
				_, err := client.HostHistory(
					context.Background(),
					reactorLabWindowRequest{},
				)
				return err
			},
			want: errReactorLabMalformed,
		},
		{
			name: "excessive metric array",
			body: func(response http.ResponseWriter) {
				points := make(
					[]reactorLabHostPoint,
					reactorLabMaxMetricPoints+1,
				)
				for index := range points {
					points[index] = reactorLabHostPoint{
						Timestamp:   reactorLabTestNow,
						SampleCount: 1,
					}
				}
				_ = json.NewEncoder(response).Encode(
					reactorLabHostHistory{
						Window: reactorLabTestWindow(),
						Points: points,
					},
				)
			},
			call: func(client reactorLabReadClient) error {
				_, err := client.HostHistory(
					context.Background(),
					reactorLabWindowRequest{},
				)
				return err
			},
			want: errReactorLabMalformed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(
				response http.ResponseWriter,
				_ *http.Request,
			) {
				test.body(response)
			}))
			defer server.Close()
			err := test.call(newReactorLabReadClient(
				server.URL,
				server.Client(),
			))
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want %v", err, test.want)
			}
			if err != nil && strings.Contains(err.Error(), "must-not-leak") {
				t.Fatalf("raw upstream body leaked: %v", err)
			}
		})
	}
}

func TestReactorLabReadClientRejectsUnsafeArgumentsBeforeRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		calls.Add(1)
	}))
	defer server.Close()
	client := newReactorLabReadClient(server.URL, server.Client())

	cases := []func() error{
		func() error {
			_, err := client.Events(
				context.Background(),
				reactorLabWindowRequest{},
				501,
			)
			return err
		},
		func() error {
			_, err := client.Backups(
				context.Background(),
				"../database",
				100,
			)
			return err
		},
		func() error {
			_, err := client.ApplicationHistory(
				context.Background(),
				"../application",
				reactorLabWindowRequest{},
			)
			return err
		},
		func() error {
			_, err := client.HostHistory(
				context.Background(),
				reactorLabWindowRequest{Range: "30d"},
			)
			return err
		},
		func() error {
			_, err := client.HostHistory(
				context.Background(),
				reactorLabWindowRequest{
					From: "2026-09-22T10:00:00Z",
				},
			)
			return err
		},
		func() error {
			_, err := client.HostHistory(
				context.Background(),
				reactorLabWindowRequest{
					From: "not-time", To: "also-not-time",
				},
			)
			return err
		},
		func() error {
			_, err := client.HostHistory(
				context.Background(),
				reactorLabWindowRequest{
					From: "2026-09-22T11:00:00Z",
					To:   "2026-09-22T10:00:00Z",
				},
			)
			return err
		},
	}
	for index, call := range cases {
		if err := call(); !errors.Is(err, errReactorLabInvalidRequest) {
			t.Fatalf("case %d error=%v", index, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("unsafe arguments reached upstream %d times", calls.Load())
	}
}

func TestReactorLabTypedProjectionDropsSecretBearingUnknownFields(t *testing.T) {
	databaseID := "database_11111111111111111111111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		switch request.URL.Path {
		case "/internal/miniai/v1/deployments":
			_, _ = response.Write([]byte(`{"deployments":[{
				"app":"myscheduler",
				"strategy":"node-express",
				"status":"running",
				"source":{"branch":"main","commitSha":"` +
				strings.Repeat("a", 40) + `"},
				"services":[],
				"databases":[],
				"repoUrl":"https://user:repo-secret@example.invalid/repo.git",
				"environment":{"TOKEN":"environment-secret"}
			}]}`))
		case "/internal/miniai/v1/databases/" + databaseID + "/backups":
			_, _ = response.Write([]byte(`{
				"databaseId":"` + databaseID + `",
				"limit":100,
				"truncated":false,
				"backups":[{
					"id":"backup_22222222222222222222222222222222",
					"databaseId":"` + databaseID + `",
					"kind":"automatic",
					"status":"ready",
					"sizeBytes":10,
					"createdAt":"2026-09-22T10:00:00Z",
					"completedAt":"2026-09-22T10:01:00Z",
					"path":"/private/backups/archive.tar",
					"password":"backup-secret"
				}]
			}`))
		case "/internal/miniai/v1/databases":
			_, _ = response.Write([]byte(`{
				"collectedAt":"2026-09-22T10:00:00Z",
				"databases":[{
					"id":"` + databaseID + `",
					"displayName":"Production",
					"status":"ready",
					"deployments":[],
					"connectionString":"postgres://admin:database-secret@database.invalid/app",
					"password":"database-secret"
				}]
			}`))
		case "/internal/miniai/v1/recovery":
			_, _ = response.Write([]byte(`{
				"protection":{
					"state":"armed",
					"hardwareWatchdog":{"state":"armed"},
					"rtc":{"state":"armed"}
				},
				"historyAvailable":false,
				"recentIncidents":[],
				"previousBootId":"private-previous-boot",
				"recoveryBootId":"private-recovery-boot"
			}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	client := newReactorLabReadClient(server.URL, server.Client())

	deployments, err := client.Deployments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	backups, err := client.Backups(
		context.Background(),
		databaseID,
		100,
	)
	if err != nil {
		t.Fatal(err)
	}
	databases, err := client.Databases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := client.Recovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]any{
		"deployments": deployments,
		"databases":   databases,
		"backups":     backups,
		"recovery":    recovery,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"repoUrl",
		"repo-secret",
		"environment-secret",
		"/private/backups",
		"backup-secret",
		"database-secret",
		"connectionString",
		"previousBootId",
		"recoveryBootId",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("typed projection leaked %q: %s", forbidden, encoded)
		}
	}
}

func pointerTo[T any](value T) *T {
	return &value
}
