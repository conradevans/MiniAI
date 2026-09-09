package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	powerHelperSudoPath       = "/usr/bin/sudo"
	powerHelperInstalledPath  = "/usr/local/libexec/miniai-power-helper"
	powerHelperCommandTimeout = 15 * time.Second
	powerLeaseCleanupTimeout  = 15 * time.Second
	cpuPowerProfileName       = "3ghz_power"
)

type powerHelperCommand string

const (
	powerHelperEnter   powerHelperCommand = "enter"
	powerHelperRestore powerHelperCommand = "restore"
	powerHelperRecover powerHelperCommand = "recover"
	powerHelperStatus  powerHelperCommand = "status"
)

type powerHelperResult string

const (
	powerHelperEntered       powerHelperResult = "entered"
	powerHelperRestored      powerHelperResult = "restored"
	powerHelperRecovered     powerHelperResult = "recovered"
	powerHelperNoActiveLease powerHelperResult = "no_active_lease"
	powerHelperActiveLease   powerHelperResult = "active_lease"
)

var (
	errPowerHelperFailed     = errors.New("MiniAI CPU power helper failed")
	errPowerLeaseUnavailable = errors.New("MiniAI 8B CPU power lease is unavailable")
	errPowerLeaseStateLost   = errors.New("MiniAI CPU power lease state was not available during restoration")
)

type powerHelperRunner interface {
	Run(context.Context, powerHelperCommand) (powerHelperResult, error)
}

type powerCommandExecutor func(context.Context, string, ...string) (string, error)

type sudoPowerHelperRunner struct {
	execute powerCommandExecutor
}

func newSudoPowerHelperRunner() sudoPowerHelperRunner {
	return sudoPowerHelperRunner{execute: executeFixedPowerHelper}
}

func (r sudoPowerHelperRunner) Run(ctx context.Context, command powerHelperCommand) (powerHelperResult, error) {
	if !validPowerHelperCommand(command) || r.execute == nil {
		return "", errPowerHelperFailed
	}
	output, err := r.execute(ctx, powerHelperSudoPath, "-n", powerHelperInstalledPath, string(command))
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errPowerHelperFailed
	}
	result := powerHelperResult(strings.TrimSpace(output))
	if !validPowerHelperResult(command, result) {
		return "", errPowerHelperFailed
	}
	return result, nil
}

func validPowerHelperCommand(command powerHelperCommand) bool {
	switch command {
	case powerHelperEnter, powerHelperRestore, powerHelperRecover, powerHelperStatus:
		return true
	default:
		return false
	}
}

func validPowerHelperResult(command powerHelperCommand, result powerHelperResult) bool {
	switch command {
	case powerHelperEnter:
		return result == powerHelperEntered
	case powerHelperRestore:
		return result == powerHelperRestored || result == powerHelperNoActiveLease
	case powerHelperRecover:
		return result == powerHelperRecovered || result == powerHelperNoActiveLease
	case powerHelperStatus:
		return result == powerHelperActiveLease || result == powerHelperNoActiveLease
	default:
		return false
	}
}

func executeFixedPowerHelper(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	var stdout boundedPowerHelperOutput
	command.Stdout = &stdout
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return "", err
	}
	if stdout.overflow {
		return "", errPowerHelperFailed
	}
	return stdout.buffer.String(), nil
}

type boundedPowerHelperOutput struct {
	buffer   bytes.Buffer
	overflow bool
}

func (w *boundedPowerHelperOutput) Write(value []byte) (int, error) {
	const limit = 256
	remaining := limit - w.buffer.Len()
	if remaining > 0 {
		toWrite := len(value)
		if toWrite > remaining {
			toWrite = remaining
		}
		_, _ = w.buffer.Write(value[:toWrite])
	}
	if len(value) > remaining {
		w.overflow = true
	}
	return len(value), nil
}

type cpuPowerLeaseManager struct {
	runner         powerHelperRunner
	logf           func(string, ...any)
	cleanupTimeout time.Duration
	slot           chan struct{}

	stateMu sync.RWMutex
	blocked bool
	active  bool
}

func newCPUPowerLeaseManager(runner powerHelperRunner, logf func(string, ...any)) *cpuPowerLeaseManager {
	manager := &cpuPowerLeaseManager{
		runner: runner, logf: logf, cleanupTimeout: powerLeaseCleanupTimeout,
		slot: make(chan struct{}, 1),
	}
	manager.slot <- struct{}{}
	return manager
}

func (m *cpuPowerLeaseManager) Recover(ctx context.Context) error {
	if m == nil || m.runner == nil {
		return errPowerLeaseUnavailable
	}
	select {
	case <-ctx.Done():
		m.setBlocked(true)
		return ctx.Err()
	case <-m.slot:
	}
	defer func() { m.slot <- struct{}{} }()
	result, err := m.runner.Run(ctx, powerHelperRecover)
	if err != nil || (result != powerHelperRecovered && result != powerHelperNoActiveLease) {
		m.setBlocked(true)
		m.safeLog("MiniAI CPU power recovery: cpu_power_recovery=failed 8b_inference=blocked")
		return errPowerLeaseUnavailable
	}
	m.setBlocked(false)
	if result == powerHelperRecovered {
		m.safeLog("MiniAI CPU power recovery: cpu_power_recovery=performed cpu_power_profile=restored")
	} else {
		m.safeLog("MiniAI CPU power recovery: cpu_power_recovery=not_needed")
	}
	return nil
}

func (m *cpuPowerLeaseManager) Acquire(ctx context.Context, model string) (*cpuPowerLease, error) {
	if !sameModel(model, primaryModel) {
		return &cpuPowerLease{}, nil
	}
	if m == nil || m.runner == nil || m.isBlocked() {
		return nil, errPowerLeaseUnavailable
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.slot:
	}
	if m.isBlocked() {
		m.slot <- struct{}{}
		return nil, errPowerLeaseUnavailable
	}
	helperCtx, cancel := context.WithTimeout(context.Background(), powerHelperCommandTimeout)
	defer cancel()
	result, err := m.runner.Run(helperCtx, powerHelperEnter)
	if err != nil || result != powerHelperEntered {
		m.setBlocked(true)
		m.slot <- struct{}{}
		m.safeLog("MiniAI CPU power lease: cpu_power_lease=acquire_failed 8b_inference=blocked")
		return nil, errPowerLeaseUnavailable
	}
	m.stateMu.Lock()
	m.active = true
	m.stateMu.Unlock()
	m.safeLog("MiniAI CPU power lease: cpu_power_lease=acquired cpu_power_profile=3ghz_power")
	return &cpuPowerLease{manager: m, active: true}, nil
}

func (m *cpuPowerLeaseManager) Active() bool {
	if m == nil {
		return false
	}
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.active
}

func (m *cpuPowerLeaseManager) isBlocked() bool {
	m.stateMu.RLock()
	defer m.stateMu.RUnlock()
	return m.blocked
}

func (m *cpuPowerLeaseManager) setBlocked(blocked bool) {
	m.stateMu.Lock()
	m.blocked = blocked
	m.stateMu.Unlock()
}

func (m *cpuPowerLeaseManager) safeLog(format string, args ...any) {
	if m != nil && m.logf != nil {
		m.logf(format, args...)
	}
}

type cpuPowerLease struct {
	manager *cpuPowerLeaseManager
	active  bool
	once    sync.Once
	err     error
}

func (l *cpuPowerLease) Restore() error {
	if l == nil || !l.active || l.manager == nil {
		return nil
	}
	l.once.Do(func() {
		manager := l.manager
		timeout := manager.cleanupTimeout
		if timeout <= 0 {
			timeout = powerLeaseCleanupTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		result, err := manager.runner.Run(ctx, powerHelperRestore)
		if err == nil && result != powerHelperRestored {
			err = errPowerLeaseStateLost
		}
		manager.stateMu.Lock()
		manager.active = false
		if err != nil {
			manager.blocked = true
		}
		manager.stateMu.Unlock()
		if err != nil {
			manager.safeLog("MiniAI CPU power lease: cpu_power_lease=restore_failed 8b_inference=blocked")
			l.err = errPowerLeaseUnavailable
		} else {
			manager.safeLog("MiniAI CPU power lease: cpu_power_lease=restored cpu_power_profile=normal_restored")
		}
		manager.slot <- struct{}{}
	})
	return l.err
}

func (a *app) with8BPowerLease(ctx context.Context, model string, work func() error) (ran bool, err error) {
	if !sameModel(model, primaryModel) {
		return true, work()
	}
	if a == nil || a.powerLeaseManager == nil {
		return false, errPowerLeaseUnavailable
	}
	lease, err := a.powerLeaseManager.Acquire(ctx, model)
	if err != nil {
		return false, err
	}
	ran = true
	defer func() {
		err = errors.Join(err, lease.Restore())
	}()
	return true, work()
}

func (a *app) cpuPowerLimited() bool {
	return a != nil && a.powerLeaseManager != nil && a.powerLeaseManager.Active()
}

func initializeCPUPowerLeaseManager(runner powerHelperRunner, logf func(string, ...any)) *cpuPowerLeaseManager {
	manager := newCPUPowerLeaseManager(runner, logf)
	ctx, cancel := context.WithTimeout(context.Background(), powerHelperCommandTimeout)
	defer cancel()
	if err := manager.Recover(ctx); err != nil && logf != nil {
		logf("MiniAI CPU power recovery unavailable; Qwen 8B inference is blocked until recovery succeeds")
	}
	return manager
}

func logPowerLeaseError(err error) {
	if err != nil {
		log.Printf("MiniAI CPU power lease cleanup failed; Qwen 8B inference is blocked")
	}
}

func powerLeaseUnavailablePolicy(policy modelPolicy) modelPolicy {
	return modelPolicy{
		Allowed: false,
		Model:   policy.Model,
		Mode:    "blocked",
		Reason:  "Qwen 8B requires the temporary CPU low-power lease, which is unavailable",
	}
}

func powerLeaseUnavailableMessage() string {
	return fmt.Sprintf("MiniAI cannot run %s until its CPU low-power lease is available", primaryModel)
}
