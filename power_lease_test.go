package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePowerHelperRunner struct {
	mu      sync.Mutex
	calls   []powerHelperCommand
	handler func(context.Context, powerHelperCommand) (powerHelperResult, error)
}

func (f *fakePowerHelperRunner) Run(ctx context.Context, command powerHelperCommand) (powerHelperResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, command)
	handler := f.handler
	f.mu.Unlock()
	if handler != nil {
		return handler(ctx, command)
	}
	switch command {
	case powerHelperEnter:
		return powerHelperEntered, nil
	case powerHelperRestore:
		return powerHelperRestored, nil
	case powerHelperRecover:
		return powerHelperNoActiveLease, nil
	case powerHelperStatus:
		return powerHelperNoActiveLease, nil
	default:
		return "", errors.New("unexpected command")
	}
}

func (f *fakePowerHelperRunner) commands() []powerHelperCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]powerHelperCommand(nil), f.calls...)
}

func TestCurated8BPowerLeaseAcquiresBeforeModelAndRestores(t *testing.T) {
	runner := &fakePowerHelperRunner{}
	manager := newCPUPowerLeaseManager(runner, nil)
	a := &app{powerLeaseManager: manager}
	events := []string{}
	runner.handler = func(_ context.Context, command powerHelperCommand) (powerHelperResult, error) {
		events = append(events, string(command))
		if command == powerHelperEnter {
			return powerHelperEntered, nil
		}
		return powerHelperRestored, nil
	}
	ran, err := a.with8BPowerLease(context.Background(), primaryModel, func() error {
		if !manager.Active() {
			t.Fatal("8B model work ran without an active CPU power lease")
		}
		events = append(events, "curated_model")
		metadata := a.withInferenceProfileMetadata(primaryModel, map[string]any{})
		if metadata["cpu_power_limited"] != true || metadata["cpu_power_profile"] != cpuPowerProfileName {
			t.Fatalf("active lease metadata=%v", metadata)
		}
		return nil
	})
	if !ran || err != nil {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	if !reflect.DeepEqual(events, []string{"enter", "curated_model", "restore"}) {
		t.Fatalf("events=%v", events)
	}
	if manager.Active() {
		t.Fatal("lease remained active after curated model work")
	}
}

func TestGeneral8BLeaseCoversPlannerRoundsAndFinal(t *testing.T) {
	runner := &fakePowerHelperRunner{}
	a := &app{powerLeaseManager: newCPUPowerLeaseManager(runner, nil)}
	work := []string{}
	ran, err := a.with8BPowerLease(context.Background(), primaryModel, func() error {
		work = append(work, "planner_1", "planner_2", "final")
		return nil
	})
	if !ran || err != nil || !reflect.DeepEqual(work, []string{"planner_1", "planner_2", "final"}) {
		t.Fatalf("ran=%v work=%v err=%v", ran, work, err)
	}
	if got := runner.commands(); !reflect.DeepEqual(got, []powerHelperCommand{powerHelperEnter, powerHelperRestore}) {
		t.Fatalf("helper calls=%v", got)
	}
}

func Test4BModelWorkDoesNotUsePowerLease(t *testing.T) {
	runner := &fakePowerHelperRunner{}
	a := &app{powerLeaseManager: newCPUPowerLeaseManager(runner, nil)}
	modelCalled := false
	ran, err := a.with8BPowerLease(context.Background(), fallbackModel, func() error {
		modelCalled = true
		return nil
	})
	if !ran || err != nil || !modelCalled || len(runner.commands()) != 0 {
		t.Fatalf("ran=%v model_called=%v helper_calls=%v err=%v", ran, modelCalled, runner.commands(), err)
	}
}

func TestAcquireFailurePrevents8BModelWork(t *testing.T) {
	runner := &fakePowerHelperRunner{handler: func(_ context.Context, command powerHelperCommand) (powerHelperResult, error) {
		if command == powerHelperEnter {
			return "", errors.New("injected acquire failure")
		}
		return powerHelperRestored, nil
	}}
	a := &app{powerLeaseManager: newCPUPowerLeaseManager(runner, nil)}
	modelCalled := false
	ran, err := a.with8BPowerLease(context.Background(), primaryModel, func() error {
		modelCalled = true
		return nil
	})
	if ran || err == nil || modelCalled || !a.powerLeaseManager.isBlocked() {
		t.Fatalf("ran=%v model_called=%v blocked=%v err=%v", ran, modelCalled, a.powerLeaseManager.isBlocked(), err)
	}
	if got := runner.commands(); !reflect.DeepEqual(got, []powerHelperCommand{powerHelperEnter}) {
		t.Fatalf("helper calls=%v", got)
	}
}

func Test8BInferenceFailureStillRestores(t *testing.T) {
	runner := &fakePowerHelperRunner{}
	a := &app{powerLeaseManager: newCPUPowerLeaseManager(runner, nil)}
	modelErr := errors.New("injected Ollama failure")
	ran, err := a.with8BPowerLease(context.Background(), primaryModel, func() error { return modelErr })
	if !ran || !errors.Is(err, modelErr) {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	if got := runner.commands(); !reflect.DeepEqual(got, []powerHelperCommand{powerHelperEnter, powerHelperRestore}) {
		t.Fatalf("helper calls=%v", got)
	}
}

func TestRequestCancellationCannotCancelRestoration(t *testing.T) {
	requestContext, cancel := context.WithCancel(context.Background())
	restoreUsedIndependentContext := false
	runner := &fakePowerHelperRunner{handler: func(ctx context.Context, command powerHelperCommand) (powerHelperResult, error) {
		switch command {
		case powerHelperEnter:
			return powerHelperEntered, nil
		case powerHelperRestore:
			restoreUsedIndependentContext = ctx.Err() == nil
			return powerHelperRestored, nil
		default:
			return powerHelperNoActiveLease, nil
		}
	}}
	a := &app{powerLeaseManager: newCPUPowerLeaseManager(runner, nil)}
	ran, err := a.with8BPowerLease(requestContext, primaryModel, func() error {
		cancel()
		return requestContext.Err()
	})
	if !ran || !errors.Is(err, context.Canceled) || !restoreUsedIndependentContext {
		t.Fatalf("ran=%v independent_restore=%v err=%v", ran, restoreUsedIndependentContext, err)
	}
	if got := runner.commands(); !reflect.DeepEqual(got, []powerHelperCommand{powerHelperEnter, powerHelperRestore}) {
		t.Fatalf("helper calls=%v", got)
	}
}

func TestPanicUnwindingStillRestores(t *testing.T) {
	runner := &fakePowerHelperRunner{}
	a := &app{powerLeaseManager: newCPUPowerLeaseManager(runner, nil)}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected injected panic")
			}
		}()
		_, _ = a.with8BPowerLease(context.Background(), primaryModel, func() error {
			panic("injected model panic")
		})
	}()
	if got := runner.commands(); !reflect.DeepEqual(got, []powerHelperCommand{powerHelperEnter, powerHelperRestore}) {
		t.Fatalf("helper calls=%v", got)
	}
}

func TestRestoreFailureBlocksFurther8BWork(t *testing.T) {
	runner := &fakePowerHelperRunner{handler: func(_ context.Context, command powerHelperCommand) (powerHelperResult, error) {
		if command == powerHelperEnter {
			return powerHelperEntered, nil
		}
		return "", errors.New("injected restore failure")
	}}
	manager := newCPUPowerLeaseManager(runner, nil)
	a := &app{powerLeaseManager: manager}
	ran, err := a.with8BPowerLease(context.Background(), primaryModel, func() error { return nil })
	if !ran || err == nil || !manager.isBlocked() {
		t.Fatalf("ran=%v blocked=%v err=%v", ran, manager.isBlocked(), err)
	}
	modelCalled := false
	ran, err = a.with8BPowerLease(context.Background(), primaryModel, func() error {
		modelCalled = true
		return nil
	})
	if ran || err == nil || modelCalled {
		t.Fatalf("second ran=%v model_called=%v err=%v", ran, modelCalled, err)
	}
	if got := runner.commands(); !reflect.DeepEqual(got, []powerHelperCommand{powerHelperEnter, powerHelperRestore}) {
		t.Fatalf("helper calls=%v", got)
	}
}

func TestStructuredResponseFailureStillRestores(t *testing.T) {
	runner := &fakePowerHelperRunner{}
	a := &app{powerLeaseManager: newCPUPowerLeaseManager(runner, nil)}
	formatErr := errors.New("invalid structured response")
	ran, err := a.with8BPowerLease(context.Background(), primaryModel, func() error { return formatErr })
	if !ran || !errors.Is(err, formatErr) {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	if got := runner.commands(); !reflect.DeepEqual(got, []powerHelperCommand{powerHelperEnter, powerHelperRestore}) {
		t.Fatalf("helper calls=%v", got)
	}
}

func TestConcurrent8BLeasesAreSerialized(t *testing.T) {
	runner := &fakePowerHelperRunner{}
	manager := newCPUPowerLeaseManager(runner, nil)
	first, err := manager.Acquire(context.Background(), primaryModel)
	if err != nil {
		t.Fatal(err)
	}
	type acquireResult struct {
		lease *cpuPowerLease
		err   error
	}
	secondResult := make(chan acquireResult, 1)
	go func() {
		lease, err := manager.Acquire(context.Background(), primaryModel)
		secondResult <- acquireResult{lease: lease, err: err}
	}()
	select {
	case result := <-secondResult:
		t.Fatalf("second lease was not serialized: %+v", result)
	case <-time.After(40 * time.Millisecond):
	}
	if got := runner.commands(); !reflect.DeepEqual(got, []powerHelperCommand{powerHelperEnter}) {
		t.Fatalf("helper calls while first lease active=%v", got)
	}
	if err := first.Restore(); err != nil {
		t.Fatal(err)
	}
	var second acquireResult
	select {
	case second = <-secondResult:
	case <-time.After(time.Second):
		t.Fatal("second lease did not resume after restoration")
	}
	if second.err != nil {
		t.Fatal(second.err)
	}
	if err := second.lease.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := runner.commands(); !reflect.DeepEqual(got, []powerHelperCommand{
		powerHelperEnter, powerHelperRestore, powerHelperEnter, powerHelperRestore,
	}) {
		t.Fatalf("serialized helper calls=%v", got)
	}
}

func TestStartupRecoveryIsAttempted(t *testing.T) {
	runner := &fakePowerHelperRunner{handler: func(_ context.Context, command powerHelperCommand) (powerHelperResult, error) {
		if command != powerHelperRecover {
			return "", fmt.Errorf("unexpected command %q", command)
		}
		return powerHelperRecovered, nil
	}}
	manager := initializeCPUPowerLeaseManager(runner, nil)
	if manager.isBlocked() {
		t.Fatal("successful recovery left 8B blocked")
	}
	if got := runner.commands(); !reflect.DeepEqual(got, []powerHelperCommand{powerHelperRecover}) {
		t.Fatalf("startup helper calls=%v", got)
	}
}

func TestRecoveryFailureBlocks8BButNotDeterministicDiagnostics(t *testing.T) {
	runner := &fakePowerHelperRunner{handler: func(_ context.Context, command powerHelperCommand) (powerHelperResult, error) {
		return "", errors.New("injected recovery failure")
	}}
	manager := initializeCPUPowerLeaseManager(runner, nil)
	if !manager.isBlocked() {
		t.Fatal("recovery failure did not block 8B")
	}
	modelCalled := false
	a := &app{powerLeaseManager: manager}
	ran, err := a.with8BPowerLease(context.Background(), primaryModel, func() error {
		modelCalled = true
		return nil
	})
	if ran || err == nil || modelCalled {
		t.Fatalf("ran=%v model_called=%v err=%v", ran, modelCalled, err)
	}

	diagnosticApp, cleanup := newDiagnosticTestApp(t, []any{healthyMySchedulerDeployment()}, nil, "", "")
	defer cleanup()
	diagnosticApp.powerLeaseManager = manager
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/chat/stream", nil)
	if !diagnosticApp.handleDeterministicDiagnosticLookup(recorder, request, "Is MyScheduler running?") ||
		!strings.Contains(recorder.Body.String(), `"model_invoked":false`) {
		t.Fatalf("deterministic response unavailable: %s", recorder.Body.String())
	}
	if got := runner.commands(); !reflect.DeepEqual(got, []powerHelperCommand{powerHelperRecover}) {
		t.Fatalf("deterministic path invoked helper: %v", got)
	}
}

func TestSudoRunnerUsesOnlyFixedHelperArguments(t *testing.T) {
	var gotName string
	var gotArgs []string
	runner := sudoPowerHelperRunner{execute: func(_ context.Context, name string, args ...string) (string, error) {
		gotName = name
		gotArgs = append([]string(nil), args...)
		return "entered\n", nil
	}}
	result, err := runner.Run(context.Background(), powerHelperEnter)
	if err != nil || result != powerHelperEntered {
		t.Fatalf("result=%q err=%v", result, err)
	}
	if gotName != powerHelperSudoPath || !reflect.DeepEqual(gotArgs, []string{"-n", powerHelperInstalledPath, "enter"}) {
		t.Fatalf("command=%q args=%v", gotName, gotArgs)
	}
	gotName, gotArgs = "", nil
	if _, err := runner.Run(context.Background(), powerHelperCommand("user-controlled")); err == nil || gotName != "" || gotArgs != nil {
		t.Fatalf("unsupported command reached executor: name=%q args=%v err=%v", gotName, gotArgs, err)
	}
}

func TestKeepAliveRemainsImmediateUnload(t *testing.T) {
	if number(modelKeepAlive) != 0 {
		t.Fatalf("keep_alive=%v want 0", modelKeepAlive)
	}
}

func TestRemovalStopsMiniAIBeforeFinalRecovery(t *testing.T) {
	documentation, err := os.ReadFile("POWER_LEASE.md")
	if err != nil {
		t.Fatal(err)
	}
	removalAt := strings.Index(string(documentation), "## Removal")
	if removalAt < 0 {
		t.Fatal("removal instructions are missing")
	}
	commands := string(documentation)[removalAt:]
	failClosedAt := strings.Index(commands, "set -e")
	stopAt := strings.Index(commands, "sudo systemctl stop miniai.service")
	recoverAt := strings.Index(commands, "sudo /usr/local/libexec/miniai-power-helper recover")
	removeAt := strings.Index(commands, "sudo rm -f /usr/local/libexec/miniai-power-helper")
	if failClosedAt < 0 || stopAt <= failClosedAt || recoverAt <= stopAt || removeAt <= recoverAt {
		t.Fatalf("unsafe helper removal order: fail_closed=%d stop=%d recover=%d remove=%d", failClosedAt, stopAt, recoverAt, removeAt)
	}
}

func TestInstallationRecoversLegacyLeaseBeforeReplacingHelper(t *testing.T) {
	documentation, err := os.ReadFile("POWER_LEASE.md")
	if err != nil {
		t.Fatal(err)
	}
	commands := string(documentation)
	failClosedAt := strings.Index(commands, "set -e")
	stopAt := strings.Index(commands, "sudo systemctl stop miniai.service")
	recoverAt := strings.Index(commands, "sudo /usr/local/libexec/miniai-power-helper recover")
	replaceAt := strings.Index(commands, "sudo install -o root -g root -m 0755 /tmp/miniai-power-helper /usr/local/libexec/miniai-power-helper")
	if failClosedAt < 0 || stopAt <= failClosedAt || recoverAt <= stopAt || replaceAt <= recoverAt {
		t.Fatalf("unsafe legacy-helper transition order: fail_closed=%d stop=%d recover=%d replace=%d", failClosedAt, stopAt, recoverAt, replaceAt)
	}
}
