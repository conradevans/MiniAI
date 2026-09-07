package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChoosePolicyPrimary(t *testing.T) {
	p := choosePolicy(systemStatus{AvailableMemoryGiB: 13.5, Load1: 0.2}, nil)
	if !p.Allowed || p.Model != primaryModel || p.Mode != "primary" {
		t.Fatalf("unexpected policy: %+v", p)
	}
}

func TestChoosePolicyFallback(t *testing.T) {
	p := choosePolicy(systemStatus{AvailableMemoryGiB: 8.0, Load1: 1.0}, nil)
	if !p.Allowed || p.Model != fallbackModel || p.Mode != "fallback" {
		t.Fatalf("unexpected policy: %+v", p)
	}
}

func TestChoosePolicyBlocked(t *testing.T) {
	p := choosePolicy(systemStatus{AvailableMemoryGiB: 5.0, Load1: 1.0}, nil)
	if p.Allowed || p.Mode != "blocked" {
		t.Fatalf("unexpected policy: %+v", p)
	}
}

func TestLoadedPrimaryStaysPrimary(t *testing.T) {
	p := choosePolicy(systemStatus{AvailableMemoryGiB: 7.5, Load1: 9.0}, []string{primaryModel})
	if !p.Allowed || p.Model != primaryModel {
		t.Fatalf("loaded primary should remain selected: %+v", p)
	}
}

func TestAggregateUsage(t *testing.T) {
	deployment := map[string]any{
		"containers": []any{
			map[string]any{
				"state": "running", "cpuPercent": 1.25, "memoryUsedBytes": float64(100),
				"networkRxBytes": float64(10), "networkTxBytes": float64(20),
				"blockReadBytes": float64(30), "blockWriteBytes": float64(40),
				"pids": float64(5), "writableBytes": float64(50), "restartCount": float64(1),
			},
			map[string]any{
				"state": "running", "cpuPercent": 0.75, "memoryUsedBytes": float64(200),
				"networkRxBytes": float64(11), "networkTxBytes": float64(21),
				"blockReadBytes": float64(31), "blockWriteBytes": float64(41),
				"pids": float64(6), "writableBytes": float64(51), "restartCount": float64(2),
			},
		},
	}
	got := aggregateUsage(deployment)
	if got.CPUPercent != 2 || got.MemoryUsedBytes != 300 || got.RunningContainers != 2 || got.Restarts != 3 {
		t.Fatalf("unexpected usage: %+v", got)
	}
}

func TestSelectDatabaseByDeploymentID(t *testing.T) {
	deployment := map[string]any{
		"database": map[string]any{"id": "database_123"},
	}
	databases := []map[string]any{
		{"id": "database_other"},
		{"id": "database_123", "displayName": "MyScheduler Production"},
	}
	got := selectDatabase("myscheduler", deployment, databases)
	if got == nil || got["id"] != "database_123" {
		t.Fatalf("unexpected database: %+v", got)
	}
}

func TestNormalizeMatchFindsSpacedAlias(t *testing.T) {
	message := normalizeMatch("How is My Scheduler doing?")
	if !strings.Contains(message, normalizeMatch("myscheduler")) {
		t.Fatalf("expected normalized app match, got %q", message)
	}
}

func TestSafeAppNameRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../myscheduler", "my/scheduler", "..", ""} {
		if safeAppName(name) {
			t.Fatalf("expected %q to be rejected", name)
		}
	}
	if !safeAppName("myscheduler") {
		t.Fatal("expected myscheduler to be accepted")
	}
}

func TestReadRepoContextExcludesSensitiveTopLevel(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "myscheduler")
	if err := os.MkdirAll(filepath.Join(repo, ".git", "refs", "heads"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"backend", "frontend", "node_modules"} {
		if err := os.MkdirAll(filepath.Join(repo, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("SECRET=x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "refs", "heads", "main"), []byte("e952ae5123456789\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &app{repoRoot: root}
	got := a.readRepoContext("myscheduler")
	if !got.Exists || got.Branch != "main" || got.Commit != "e952ae512345" {
		t.Fatalf("unexpected repo context: %+v", got)
	}
	for _, name := range got.TopLevel {
		if name == ".env" || name == ".git" || name == "node_modules" {
			t.Fatalf("sensitive/noisy entry leaked: %s", name)
		}
	}
}

func TestReapSessionsKeepsInFlightChat(t *testing.T) {
	now := time.Now()
	a := &app{sessions: map[string]*session{
		"busy": {
			ID:            "busy",
			CreatedAt:     now.Add(-2 * time.Minute),
			LastHeartbeat: now.Add(-2 * time.Minute),
			LastChat:      now.Add(-2 * time.Minute),
			InFlight:      1,
		},
	}}
	active, idle := a.reapSessions(now)
	if active != 1 || idle {
		t.Fatalf("in-flight chat should remain active and non-idle: active=%d idle=%v", active, idle)
	}
	if _, ok := a.sessions["busy"]; !ok {
		t.Fatal("in-flight session was incorrectly reaped")
	}
}

func TestReapSessionsExpiresMissedHeartbeatWhenIdle(t *testing.T) {
	now := time.Now()
	a := &app{sessions: map[string]*session{
		"stale": {
			ID:            "stale",
			CreatedAt:     now.Add(-2 * time.Minute),
			LastHeartbeat: now.Add(-2 * time.Minute),
			LastChat:      now.Add(-2 * time.Minute),
		},
	}}
	active, _ := a.reapSessions(now)
	if active != 0 {
		t.Fatalf("stale idle session should be reaped: active=%d", active)
	}
}

func TestCompactModelContextDropsVerboseContainerCounters(t *testing.T) {
	ctx := appContext{
		App: "myscheduler",
		Deployment: map[string]any{
			"app": "myscheduler", "status": "healthy", "strategy": "fullstack-vite-node",
			"containers": []any{map[string]any{
				"service": "backend", "state": "running", "health": "healthy",
				"memoryUsedBytes": float64(100), "restartCount": float64(0),
				"networkRxBytes": float64(999999),
			}},
		},
		Database: map[string]any{"displayName": "MyScheduler Production", "status": "ready"},
		Repo:     repoContext{Path: "/srv/myscheduler", Exists: true, Branch: "main"},
	}
	got := compactModelContext(ctx)
	dep := got["deployment"].(map[string]any)
	containers := dep["containers"].([]map[string]any)
	if _, ok := containers[0]["networkRxBytes"]; ok {
		t.Fatal("verbose network counter should not be copied into compact prompt context")
	}
	if containers[0]["service"] != "backend" {
		t.Fatalf("expected service identity to remain, got %+v", containers[0])
	}
}
