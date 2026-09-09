package powerleasehelper

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testPolicy struct {
	epp     string
	maxFreq string
}

func newTestManager(t *testing.T, policies map[string]testPolicy) *Manager {
	t.Helper()
	root := filepath.Join(t.TempDir(), "cpufreq")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, policy := range policies {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, eppControl), []byte(policy.epp+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, maxFreqControl), []byte(policy.maxFreq+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runtimeDir := t.TempDir()
	return &Manager{
		sysfsRoot:    root,
		statePath:    filepath.Join(runtimeDir, "lease.json"),
		lockPath:     filepath.Join(runtimeDir, "lease.lock"),
		ownerUID:     uint32(os.Geteuid()),
		readControl:  readControlValue,
		writeControl: writeControlValue,
		wait:         func(time.Duration) {},
	}
}

func delaySuccessfulWriteVisibility(manager *Manager, staleReads int) *int {
	remaining := make(map[string]int)
	staleValues := make(map[string]string)
	waits := 0
	manager.writeControl = func(path, value string) error {
		previous, err := readControlValue(path)
		if err != nil {
			return err
		}
		if err := writeControlValue(path, value); err != nil {
			return err
		}
		staleValues[path] = previous
		remaining[path] = staleReads
		return nil
	}
	manager.readControl = func(path string) (string, error) {
		if remaining[path] > 0 {
			remaining[path]--
			return staleValues[path], nil
		}
		return readControlValue(path)
	}
	manager.wait = func(delay time.Duration) {
		if delay != verificationRetryInterval {
			panic("unexpected verification retry interval")
		}
		waits++
	}
	return &waits
}

func readTestPolicy(t *testing.T, manager *Manager, name string) testPolicy {
	t.Helper()
	path := filepath.Join(manager.sysfsRoot, name)
	epp, err := readControlValue(filepath.Join(path, eppControl))
	if err != nil {
		t.Fatal(err)
	}
	maxFreq, err := readControlValue(filepath.Join(path, maxFreqControl))
	if err != nil {
		t.Fatal(err)
	}
	return testPolicy{epp: epp, maxFreq: maxFreq}
}

func TestRuntimePathsUseDedicatedDirectory(t *testing.T) {
	if LeaseRuntimeDir != "/run/miniai-power" {
		t.Fatalf("runtime directory=%q", LeaseRuntimeDir)
	}
	if LeaseStatePath != filepath.Join(LeaseRuntimeDir, "lease.json") {
		t.Fatalf("lease state path=%q", LeaseStatePath)
	}
	if LeaseLockPath != filepath.Join(LeaseRuntimeDir, "lease.lock") {
		t.Fatalf("lease lock path=%q", LeaseLockPath)
	}
}

func TestEnterAndRestoreExactCPUState(t *testing.T) {
	original := map[string]testPolicy{
		"policy0": {epp: "performance", maxFreq: "4546000"},
		"policy1": {epp: "balance_performance", maxFreq: "4200000"},
	}
	manager := newTestManager(t, original)
	result, err := manager.Enter()
	if err != nil || result != ResultEntered {
		t.Fatalf("enter result=%q err=%v", result, err)
	}
	for name := range original {
		if got := readTestPolicy(t, manager, name); got != (testPolicy{epp: LowPowerEPP, maxFreq: LowPowerMaxFreq}) {
			t.Fatalf("%s low-power state=%+v", name, got)
		}
	}
	info, err := os.Stat(manager.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("lease mode=%o", info.Mode().Perm())
	}
	state, active, err := manager.loadState()
	if err != nil || !active || len(state.Policies) != 2 {
		t.Fatalf("state active=%v state=%+v err=%v", active, state, err)
	}
	for index, policy := range state.Policies {
		want := original[filepath.Base(policy.Path)]
		if policy.EnergyPerformancePreference != want.epp || policy.ScalingMaxFreq != want.maxFreq {
			t.Fatalf("saved policy %d=%+v want=%+v", index, policy, want)
		}
	}

	result, err = manager.Restore()
	if err != nil || result != ResultRestored {
		t.Fatalf("restore result=%q err=%v", result, err)
	}
	for name, want := range original {
		if got := readTestPolicy(t, manager, name); got != want {
			t.Fatalf("%s restored=%+v want=%+v", name, got, want)
		}
	}
	if _, err := os.Stat(manager.statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease state still exists: %v", err)
	}
	if result, err := manager.Restore(); err != nil || result != ResultNoActiveLease {
		t.Fatalf("idempotent restore result=%q err=%v", result, err)
	}
}

func TestPartialEnterFailureRollsBack(t *testing.T) {
	original := map[string]testPolicy{
		"policy0": {epp: "performance", maxFreq: "4546000"},
		"policy1": {epp: "balance_power", maxFreq: "3900000"},
	}
	manager := newTestManager(t, original)
	failed := false
	manager.writeControl = func(path, value string) error {
		if !failed && strings.HasSuffix(path, filepath.Join("policy1", maxFreqControl)) && value == LowPowerMaxFreq {
			failed = true
			return errors.New("injected write failure")
		}
		return writeControlValue(path, value)
	}
	if result, err := manager.Enter(); err == nil || result != "" {
		t.Fatalf("partial enter result=%q err=%v", result, err)
	}
	for name, want := range original {
		if got := readTestPolicy(t, manager, name); got != want {
			t.Fatalf("%s rollback=%+v want=%+v", name, got, want)
		}
	}
	if _, err := os.Stat(manager.statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rolled-back lease state remains: %v", err)
	}
}

func TestRestoreFailureKeepsLeaseState(t *testing.T) {
	manager := newTestManager(t, map[string]testPolicy{"policy0": {epp: "performance", maxFreq: "4546000"}})
	if _, err := manager.Enter(); err != nil {
		t.Fatal(err)
	}
	failed := false
	manager.writeControl = func(path, value string) error {
		if !failed && strings.HasSuffix(path, maxFreqControl) && value == "4546000" {
			failed = true
			return errors.New("injected restore failure")
		}
		return writeControlValue(path, value)
	}
	if result, err := manager.Restore(); err == nil || result != ResultNoActiveLease {
		t.Fatalf("restore result=%q err=%v", result, err)
	}
	if _, err := os.Stat(manager.statePath); err != nil {
		t.Fatalf("failed restoration deleted lease state: %v", err)
	}
	manager.writeControl = writeControlValue
	if result, err := manager.Recover(); err != nil || result != ResultRecovered {
		t.Fatalf("retry recovery result=%q err=%v", result, err)
	}
}

func TestStaleLeaseRecovery(t *testing.T) {
	original := map[string]testPolicy{"policy0": {epp: "performance", maxFreq: "4546000"}}
	manager := newTestManager(t, original)
	if _, err := manager.Enter(); err != nil {
		t.Fatal(err)
	}
	restarted := *manager
	result, err := restarted.Recover()
	if err != nil || result != ResultRecovered {
		t.Fatalf("recover result=%q err=%v", result, err)
	}
	if got := readTestPolicy(t, &restarted, "policy0"); got != original["policy0"] {
		t.Fatalf("recovered=%+v want=%+v", got, original["policy0"])
	}
	if result, err := restarted.Recover(); err != nil || result != ResultNoActiveLease {
		t.Fatalf("empty recover result=%q err=%v", result, err)
	}
}

func TestMalformedStatePathIsRejectedWithoutDeletion(t *testing.T) {
	manager := newTestManager(t, map[string]testPolicy{"policy0": {epp: "performance", maxFreq: "4546000"}})
	evil := filepath.Join(t.TempDir(), "policy99")
	if err := os.Mkdir(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evil, eppControl), []byte("performance"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evil, maxFreqControl), []byte("9999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := leaseState{Version: stateVersion, Policies: []policyState{{
		Path: evil, EnergyPerformancePreference: "power", ScalingMaxFreq: "3000000",
	}}}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.statePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if result, err := manager.Restore(); err == nil || result != ResultNoActiveLease {
		t.Fatalf("malformed restore result=%q err=%v", result, err)
	}
	if _, err := os.Stat(manager.statePath); err != nil {
		t.Fatalf("malformed lease state was deleted: %v", err)
	}
	if got, err := readControlValue(filepath.Join(evil, maxFreqControl)); err != nil || got != "9999999" {
		t.Fatalf("outside path was touched: value=%q err=%v", got, err)
	}
}

func TestActiveLeaseAndUnsupportedArgumentsAreRejected(t *testing.T) {
	manager := newTestManager(t, map[string]testPolicy{"policy0": {epp: "performance", maxFreq: "4546000"}})
	if _, err := manager.Enter(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Enter(); !errors.Is(err, ErrActiveLease) {
		t.Fatalf("second enter err=%v", err)
	}
	for _, args := range [][]string{nil, {"enter", "extra"}, {"arbitrary"}, {"restore", "/tmp/path"}} {
		if result, err := manager.Execute(args); err == nil || result != "" {
			t.Fatalf("args=%v result=%q err=%v", args, result, err)
		}
	}
	if _, err := manager.Restore(); err != nil {
		t.Fatal(err)
	}
}

func TestEnterRetriesUntilSuccessfulWritesAreVisible(t *testing.T) {
	original := map[string]testPolicy{"policy0": {epp: "performance", maxFreq: "4546000"}}
	manager := newTestManager(t, original)
	waits := delaySuccessfulWriteVisibility(manager, 1)

	result, err := manager.Enter()
	if err != nil || result != ResultEntered {
		t.Fatalf("enter result=%q err=%v", result, err)
	}
	if *waits != 1 {
		t.Fatalf("verification waits=%d want=1", *waits)
	}
	if got := readTestPolicy(t, manager, "policy0"); got != (testPolicy{epp: LowPowerEPP, maxFreq: LowPowerMaxFreq}) {
		t.Fatalf("low-power state=%+v", got)
	}
}

func TestRestoreRetriesUntilSuccessfulWritesAreVisible(t *testing.T) {
	original := map[string]testPolicy{"policy0": {epp: "performance", maxFreq: "4546000"}}
	manager := newTestManager(t, original)
	if _, err := manager.Enter(); err != nil {
		t.Fatal(err)
	}
	waits := delaySuccessfulWriteVisibility(manager, 1)

	result, err := manager.Restore()
	if err != nil || result != ResultRestored {
		t.Fatalf("restore result=%q err=%v", result, err)
	}
	if *waits != 1 {
		t.Fatalf("verification waits=%d want=1", *waits)
	}
	if got := readTestPolicy(t, manager, "policy0"); got != original["policy0"] {
		t.Fatalf("restored=%+v want=%+v", got, original["policy0"])
	}
	if _, err := os.Stat(manager.statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease state still exists: %v", err)
	}
}

func TestVerificationFailsAfterBoundedAttemptsWithoutConvergence(t *testing.T) {
	manager := newTestManager(t, map[string]testPolicy{"policy0": {epp: "performance", maxFreq: "4546000"}})
	policyPath := filepath.Join(manager.sysfsRoot, "policy0")
	manager.readControl = func(path string) (string, error) {
		switch filepath.Base(path) {
		case eppControl:
			return "performance", nil
		case maxFreqControl:
			return "4546000", nil
		default:
			return "", errors.New("unexpected control path")
		}
	}
	waits := 0
	manager.wait = func(delay time.Duration) {
		if delay != verificationRetryInterval {
			t.Fatalf("retry interval=%v want=%v", delay, verificationRetryInterval)
		}
		waits++
	}

	err := manager.eventuallyVerifyPolicyStates([]policyState{{
		Path:                        policyPath,
		EnergyPerformancePreference: LowPowerEPP,
		ScalingMaxFreq:              LowPowerMaxFreq,
	}})
	if err == nil {
		t.Fatal("verification unexpectedly converged")
	}
	if waits != verificationAttempts-1 {
		t.Fatalf("verification waits=%d want=%d", waits, verificationAttempts-1)
	}
	if maximumWait := time.Duration(waits) * verificationRetryInterval; maximumWait != 200*time.Millisecond {
		t.Fatalf("maximum retry wait=%v want=200ms", maximumWait)
	}
}

func TestVerificationReadErrorFailsImmediately(t *testing.T) {
	manager := newTestManager(t, map[string]testPolicy{"policy0": {epp: "performance", maxFreq: "4546000"}})
	policyPath := filepath.Join(manager.sysfsRoot, "policy0")
	readErr := errors.New("injected read failure")
	reads := 0
	manager.readControl = func(string) (string, error) {
		reads++
		return "", readErr
	}
	waits := 0
	manager.wait = func(time.Duration) {
		waits++
	}

	err := manager.eventuallyVerifyPolicyStates([]policyState{{
		Path:                        policyPath,
		EnergyPerformancePreference: LowPowerEPP,
		ScalingMaxFreq:              LowPowerMaxFreq,
	}})
	if !errors.Is(err, readErr) {
		t.Fatalf("verification err=%v want=%v", err, readErr)
	}
	if reads != 1 {
		t.Fatalf("verification reads=%d want=1", reads)
	}
	if waits != 0 {
		t.Fatalf("verification waits=%d want=0", waits)
	}
}

func TestNonconvergingRestoreVerificationRetainsLeaseState(t *testing.T) {
	original := map[string]testPolicy{"policy0": {epp: "performance", maxFreq: "4546000"}}
	manager := newTestManager(t, original)
	if _, err := manager.Enter(); err != nil {
		t.Fatal(err)
	}
	manager.readControl = func(path string) (string, error) {
		value, err := readControlValue(path)
		if err != nil {
			return "", err
		}
		switch {
		case filepath.Base(path) == eppControl && value == original["policy0"].epp:
			return LowPowerEPP, nil
		case filepath.Base(path) == maxFreqControl && value == original["policy0"].maxFreq:
			return LowPowerMaxFreq, nil
		default:
			return value, nil
		}
	}

	if result, err := manager.Restore(); err == nil || result != ResultNoActiveLease {
		t.Fatalf("restore result=%q err=%v", result, err)
	}
	if got := readTestPolicy(t, manager, "policy0"); got != original["policy0"] {
		t.Fatalf("physical state=%+v want=%+v", got, original["policy0"])
	}
	if _, err := os.Stat(manager.statePath); err != nil {
		t.Fatalf("failed restore deleted lease state: %v", err)
	}
	manager.readControl = readControlValue
	if result, err := manager.Recover(); err != nil || result != ResultRecovered {
		t.Fatalf("recovery result=%q err=%v", result, err)
	}
}

func TestNonconvergingEnterVerificationRollsBackSafely(t *testing.T) {
	original := map[string]testPolicy{"policy0": {epp: "performance", maxFreq: "4546000"}}
	manager := newTestManager(t, original)
	manager.readControl = func(path string) (string, error) {
		value, err := readControlValue(path)
		if err != nil {
			return "", err
		}
		switch {
		case filepath.Base(path) == eppControl && value == LowPowerEPP:
			return original["policy0"].epp, nil
		case filepath.Base(path) == maxFreqControl && value == LowPowerMaxFreq:
			return original["policy0"].maxFreq, nil
		default:
			return value, nil
		}
	}

	if result, err := manager.Enter(); err == nil || result != "" {
		t.Fatalf("enter result=%q err=%v", result, err)
	}
	if got := readTestPolicy(t, manager, "policy0"); got != original["policy0"] {
		t.Fatalf("rollback=%+v want=%+v", got, original["policy0"])
	}
	if _, err := os.Stat(manager.statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rolled-back lease state remains: %v", err)
	}
}
