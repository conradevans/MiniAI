package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type concurrencyCapabilityExecutor struct {
	mu       sync.Mutex
	current  int
	maximum  int
	byKey    map[string]int
	maxByKey map[string]int
	calls    map[string]int
	delays   map[string]time.Duration
	failures map[string]error
	block    bool
}

func (executor *concurrencyCapabilityExecutor) Execute(ctx context.Context, name string, _ map[string]any) (any, string, error) {
	key := testBackendKey(name)
	executor.mu.Lock()
	if executor.byKey == nil {
		executor.byKey, executor.maxByKey, executor.calls = map[string]int{}, map[string]int{}, map[string]int{}
	}
	executor.current++
	executor.byKey[key]++
	executor.calls[name]++
	if executor.current > executor.maximum {
		executor.maximum = executor.current
	}
	if executor.byKey[key] > executor.maxByKey[key] {
		executor.maxByKey[key] = executor.byKey[key]
	}
	delay, failure, block := executor.delays[name], executor.failures[name], executor.block
	executor.mu.Unlock()

	if block {
		<-ctx.Done()
		failure = ctx.Err()
	} else if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			failure = ctx.Err()
		}
	}
	executor.mu.Lock()
	executor.current--
	executor.byKey[key]--
	executor.mu.Unlock()
	if failure != nil {
		return nil, "", failure
	}
	return map[string]any{"capability": name}, "safe " + name, nil
}

func testBackendKey(name string) string {
	switch name {
	case "read_runtime_logs", "read_deployment_logs", "list_apps":
		return "minideploy"
	case "list_repository_directory", "search_repository", "read_repository_file":
		return "repository"
	case "get_app_context":
		return "app_context"
	default:
		return "reactorlab"
	}
}

func TestEvidenceExecutorOverlapsIndependentReadsAndEnforcesLimits(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		caps       []string
		wantGlobal int
		key        string
		wantKey    int
	}{
		{name: "global and reactorlab", caps: []string{"get_platform_overview", "read_host_history", "read_temperature_history", "read_infrastructure_events", "read_activity", "read_recovery"}, wantGlobal: 4, key: "reactorlab", wantKey: 4},
		{name: "minideploy", caps: []string{"read_runtime_logs", "read_deployment_logs", "read_runtime_logs", "read_deployment_logs"}, wantGlobal: 2, key: "minideploy", wantKey: 2},
		{name: "repository", caps: []string{"list_repository_directory", "search_repository", "read_repository_file"}, wantGlobal: 1, key: "repository", wantKey: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &concurrencyCapabilityExecutor{delays: map[string]time.Duration{}, failures: map[string]error{}}
			for _, capability := range test.caps {
				fake.delays[capability] = 30 * time.Millisecond
			}
			plan := testEvidencePlan(test.caps)
			results, err := NewEvidenceExecutor(phase0CapabilityRegistry(), fake, func() time.Time { return now }).Execute(t.Context(), plan)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != len(test.caps) || fake.maximum != test.wantGlobal || fake.maxByKey[test.key] != test.wantKey {
				t.Fatalf("results=%d global=%d key=%d", len(results), fake.maximum, fake.maxByKey[test.key])
			}
			for index, result := range results {
				if result.PlanOrder != index {
					t.Fatalf("results not in plan order: %+v", results)
				}
			}
		})
	}
}

func TestEvidenceExecutorDependenciesFailuresAndCancellation(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	parent := CapabilityRequest{ID: "parent", Order: 0, Capability: "read_host_history", ConcurrencyKey: "reactorlab", CostUnits: 1, MaxFanout: 1}
	child := CapabilityRequest{ID: "child", Order: 1, Capability: "read_temperature_history", ConcurrencyKey: "reactorlab", CostUnits: 1, MaxFanout: 1, DependsOn: []string{"parent"}}
	fake := &concurrencyCapabilityExecutor{delays: map[string]time.Duration{"read_host_history": 40 * time.Millisecond}, failures: map[string]error{}}
	results, err := NewEvidenceExecutor(phase0CapabilityRegistry(), fake, func() time.Time { return now }).Execute(t.Context(), EvidencePlan{Requests: []CapabilityRequest{parent, child}})
	if err != nil || len(results) != 2 || fake.calls["read_temperature_history"] != 1 {
		t.Fatalf("dependency execution err=%v results=%+v calls=%v", err, results, fake.calls)
	}

	fake = &concurrencyCapabilityExecutor{delays: map[string]time.Duration{}, failures: map[string]error{"read_host_history": errors.New("boom")}}
	results, err = NewEvidenceExecutor(phase0CapabilityRegistry(), fake, func() time.Time { return now }).Execute(t.Context(), EvidencePlan{Requests: []CapabilityRequest{parent, child}})
	if err != nil || fake.calls["read_temperature_history"] != 0 || !results[1].DependencyError || results[1].Availability != AvailabilityUnavailable {
		t.Fatalf("failed dependency fabricated child: err=%v results=%+v calls=%v", err, results, fake.calls)
	}

	sibling := CapabilityRequest{ID: "sibling", Order: 2, Capability: "read_recovery", ConcurrencyKey: "reactorlab", CostUnits: 1, MaxFanout: 1}
	fake = &concurrencyCapabilityExecutor{delays: map[string]time.Duration{}, failures: map[string]error{"read_host_history": errors.New("boom")}}
	results, err = NewEvidenceExecutor(phase0CapabilityRegistry(), fake, func() time.Time { return now }).Execute(t.Context(), EvidencePlan{Requests: []CapabilityRequest{parent, sibling}})
	if err != nil || results[1].Availability != AvailabilityAvailable {
		t.Fatalf("independent sibling was cancelled: err=%v results=%+v", err, results)
	}

	cancelContext, cancel := context.WithCancel(context.Background())
	fake = &concurrencyCapabilityExecutor{delays: map[string]time.Duration{}, failures: map[string]error{}, block: true}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	results, err = NewEvidenceExecutor(phase0CapabilityRegistry(), fake, func() time.Time { return now }).Execute(cancelContext, EvidencePlan{Requests: []CapabilityRequest{parent}})
	if err != nil || len(results) != 1 || results[0].Availability != AvailabilityUnavailable {
		t.Fatalf("cancellation result err=%v results=%+v", err, results)
	}
}

func TestEvidenceExecutorStableOrderingAndRegistryValidation(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	caps := []string{"read_recovery", "read_activity", "read_host_history", "read_temperature_history"}
	plan := testEvidencePlan(caps)
	var baseline string
	for iteration := 0; iteration < 5; iteration++ {
		fake := &concurrencyCapabilityExecutor{delays: map[string]time.Duration{}, failures: map[string]error{}}
		for index, capability := range caps {
			fake.delays[capability] = time.Duration((iteration+index*3)%5) * time.Millisecond
		}
		results, err := NewEvidenceExecutor(phase0CapabilityRegistry(), fake, func() time.Time { return now }).Execute(t.Context(), plan)
		if err != nil {
			t.Fatal(err)
		}
		encoded := canonicalJSON(results)
		if baseline == "" {
			baseline = encoded
		} else if encoded != baseline {
			t.Fatalf("completion order changed envelopes\n%s\n%s", baseline, encoded)
		}
	}

	fake := &concurrencyCapabilityExecutor{delays: map[string]time.Duration{}, failures: map[string]error{}}
	bad := EvidencePlan{Requests: []CapabilityRequest{{ID: "bad", Capability: "delete_everything", CostUnits: 1, MaxFanout: 1}}}
	if _, err := NewEvidenceExecutor(phase0CapabilityRegistry(), fake, func() time.Time { return now }).Execute(t.Context(), bad); !errors.Is(err, errEvidenceExecutionInvalidPlan) {
		t.Fatalf("unregistered capability error=%v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("invalid capability reached executor: %v", fake.calls)
	}
}

func testEvidencePlan(capabilities []string) EvidencePlan {
	registry := phase0CapabilityRegistry()
	requests := make([]CapabilityRequest, 0, len(capabilities))
	for index, name := range capabilities {
		capability, ok := registry.lookup(name)
		if !ok {
			panic(fmt.Sprintf("unknown test capability %q", name))
		}
		requests = append(requests, CapabilityRequest{
			ID: fmt.Sprintf("request-%02d", index), Order: index, Capability: name,
			ConcurrencyKey: capability.Metadata.ConcurrencyKey, CostUnits: capability.Metadata.CostUnits, MaxFanout: capability.Metadata.MaxFanout,
		})
	}
	return EvidencePlan{Requests: requests}
}

type cancellationStartProbe struct {
	mu        sync.Mutex
	starts    map[string]int
	started   chan string
	releases  map[string]<-chan struct{}
	completed map[string]chan struct{}
}

func newCancellationStartProbe() *cancellationStartProbe {
	return &cancellationStartProbe{
		starts: map[string]int{}, started: make(chan string, 16),
		releases: map[string]<-chan struct{}{}, completed: map[string]chan struct{}{},
	}
}

func (probe *cancellationStartProbe) Execute(ctx context.Context, name string, _ map[string]any) (any, string, error) {
	probe.mu.Lock()
	probe.starts[name]++
	release := probe.releases[name]
	completed := probe.completed[name]
	probe.mu.Unlock()
	probe.started <- name
	if release == nil {
		<-ctx.Done()
		return nil, "", ctx.Err()
	}
	select {
	case <-release:
		if completed != nil {
			close(completed)
		}
		return map[string]any{"complete": name}, "safe", nil
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
}

func (probe *cancellationStartProbe) totalStarts() int {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	total := 0
	for _, count := range probe.starts {
		total += count
	}
	return total
}

func (probe *cancellationStartProbe) startsFor(name string) int {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	return probe.starts[name]
}

func TestEvidenceExecutorCancellationAuthoritativelyPreventsHandlerStarts(t *testing.T) {
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	t.Run("already cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		probe := newCancellationStartProbe()
		results, err := NewEvidenceExecutor(phase0CapabilityRegistry(), probe, func() time.Time { return now }).Execute(ctx, testEvidencePlan([]string{"read_repository_file"}))
		if err != nil || len(results) != 1 || probe.totalStarts() != 0 {
			t.Fatalf("err=%v results=%+v starts=%d", err, results, probe.totalStarts())
		}
	})

	t.Run("queued for backend capacity", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		probe := newCancellationStartProbe()
		done := make(chan struct{})
		var results []EvidenceResult
		var err error
		go func() {
			results, err = NewEvidenceExecutor(phase0CapabilityRegistry(), probe, func() time.Time { return now }).Execute(ctx, testEvidencePlan([]string{"search_repository", "read_repository_file"}))
			close(done)
		}()
		<-probe.started
		cancel()
		<-done
		if err != nil || len(results) != 2 || probe.totalStarts() != 1 {
			t.Fatalf("err=%v results=%+v starts=%d", err, results, probe.totalStarts())
		}
	})

	t.Run("queued for global capacity", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		probe := newCancellationStartProbe()
		plan := testEvidencePlan([]string{"get_platform_overview", "read_host_history", "read_temperature_history", "read_infrastructure_events", "read_runtime_logs"})
		done := make(chan struct{})
		var results []EvidenceResult
		var err error
		go func() {
			results, err = NewEvidenceExecutor(phase0CapabilityRegistry(), probe, func() time.Time { return now }).Execute(ctx, plan)
			close(done)
		}()
		for index := 0; index < phase2BGlobalWorkerLimit; index++ {
			<-probe.started
		}
		cancel()
		<-done
		if err != nil || len(results) != 5 || probe.totalStarts() != phase2BGlobalWorkerLimit {
			t.Fatalf("err=%v results=%+v starts=%d", err, results, probe.totalStarts())
		}
	})

	t.Run("waiting for dependency", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		probe := newCancellationStartProbe()
		plan := testEvidencePlan([]string{"read_host_history", "read_temperature_history"})
		plan.Requests[1].DependsOn = []string{plan.Requests[0].ID}
		done := make(chan struct{})
		go func() {
			_, _ = NewEvidenceExecutor(phase0CapabilityRegistry(), probe, func() time.Time { return now }).Execute(ctx, plan)
			close(done)
		}()
		<-probe.started
		cancel()
		<-done
		if probe.totalStarts() != 1 || probe.startsFor("read_temperature_history") != 0 {
			t.Fatalf("starts=%v", probe.starts)
		}
	})

	t.Run("dependency completed before child execution", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		probe := newCancellationStartProbe()
		parentRelease := make(chan struct{})
		parentCompleted := make(chan struct{})
		probe.releases["read_host_history"] = parentRelease
		probe.completed["read_host_history"] = parentCompleted
		plan := testEvidencePlan([]string{"search_repository", "read_host_history", "read_repository_file"})
		plan.Requests[2].DependsOn = []string{plan.Requests[1].ID}
		done := make(chan struct{})
		go func() {
			_, _ = NewEvidenceExecutor(phase0CapabilityRegistry(), probe, func() time.Time { return now }).Execute(ctx, plan)
			close(done)
		}()
		seen := map[string]bool{}
		for len(seen) < 2 {
			seen[<-probe.started] = true
		}
		if !seen["search_repository"] || !seen["read_host_history"] {
			t.Fatalf("unexpected initial starts=%v", seen)
		}
		close(parentRelease)
		<-parentCompleted
		cancel()
		<-done
		if probe.startsFor("read_repository_file") != 0 || probe.totalStarts() != 2 {
			t.Fatalf("child started after dependency/cancellation: %v", probe.starts)
		}
	})
}
