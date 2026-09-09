package powerleasehelper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	SystemCPUFreqRoot = "/sys/devices/system/cpu/cpufreq"
	LeaseRuntimeDir   = "/run/miniai-power"
	LeaseStatePath    = LeaseRuntimeDir + "/lease.json"
	LeaseLockPath     = LeaseRuntimeDir + "/lease.lock"
	LowPowerEPP       = "power"
	LowPowerMaxFreq   = "3000000"

	ResultEntered       = "entered"
	ResultRestored      = "restored"
	ResultRecovered     = "recovered"
	ResultNoActiveLease = "no_active_lease"
	ResultActiveLease   = "active_lease"

	stateVersion   = 1
	stateMaxBytes  = 64 * 1024
	maxPolicyCount = 512
	eppControl     = "energy_performance_preference"
	maxFreqControl = "scaling_max_freq"

	// One immediate read plus ten polls gives sysfs up to 200ms to expose a
	// successful write while keeping verification failure tightly bounded.
	verificationAttempts      = 11
	verificationRetryInterval = 20 * time.Millisecond
)

var (
	ErrActiveLease                = errors.New("a MiniAI CPU power lease is already active")
	ErrOperationInProgress        = errors.New("another MiniAI CPU power helper operation is in progress")
	errPolicyVerificationMismatch = errors.New("CPU policy verification failed")
)

type policyState struct {
	Path                        string `json:"path"`
	EnergyPerformancePreference string `json:"energy_performance_preference"`
	ScalingMaxFreq              string `json:"scaling_max_freq"`
}

type leaseState struct {
	Version  int           `json:"version"`
	Policies []policyState `json:"policies"`
}

type Manager struct {
	sysfsRoot    string
	statePath    string
	lockPath     string
	ownerUID     uint32
	readControl  func(string) (string, error)
	writeControl func(string, string) error
	wait         func(time.Duration)
}

func New() *Manager {
	return &Manager{
		sysfsRoot:    SystemCPUFreqRoot,
		statePath:    LeaseStatePath,
		lockPath:     LeaseLockPath,
		ownerUID:     0,
		readControl:  readControlValue,
		writeControl: writeControlValue,
		wait:         time.Sleep,
	}
}

func (m *Manager) Execute(args []string) (string, error) {
	if len(args) != 1 {
		return "", errors.New("exactly one fixed operation is required")
	}
	switch args[0] {
	case "enter":
		return m.Enter()
	case "restore":
		return m.Restore()
	case "recover":
		return m.Recover()
	case "status":
		return m.Status()
	default:
		return "", errors.New("unsupported operation")
	}
}

func (m *Manager) Enter() (string, error) {
	var result string
	err := m.withLock(func() error {
		if _, active, err := m.loadState(); err != nil {
			return err
		} else if active {
			return ErrActiveLease
		}

		policies, err := m.readCurrentPolicies()
		if err != nil {
			return err
		}
		state := leaseState{Version: stateVersion, Policies: policies}
		if err := m.persistState(state); err != nil {
			return err
		}
		if err := m.applyLowPower(policies); err != nil {
			rollbackErr := m.restoreSnapshot(policies)
			if rollbackErr == nil {
				if removeErr := m.removeState(); removeErr != nil {
					rollbackErr = removeErr
				}
			}
			return errors.Join(fmt.Errorf("apply low-power policy: %w", err), rollbackErr)
		}
		result = ResultEntered
		return nil
	})
	return result, err
}

func (m *Manager) Restore() (string, error) {
	return m.restore(ResultRestored)
}

func (m *Manager) Recover() (string, error) {
	return m.restore(ResultRecovered)
}

func (m *Manager) restore(successResult string) (string, error) {
	result := ResultNoActiveLease
	err := m.withLock(func() error {
		state, active, err := m.loadState()
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		if err := m.restoreSnapshot(state.Policies); err != nil {
			return err
		}
		if err := m.removeState(); err != nil {
			return err
		}
		result = successResult
		return nil
	})
	return result, err
}

func (m *Manager) Status() (string, error) {
	result := ResultNoActiveLease
	err := m.withLock(func() error {
		_, active, err := m.loadState()
		if err != nil {
			return err
		}
		if active {
			result = ResultActiveLease
		}
		return nil
	})
	return result, err
}

func (m *Manager) withLock(work func() error) error {
	lock, err := os.OpenFile(m.lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open helper lock: %w", err)
	}
	defer lock.Close()
	if err := validateOwnedRegularFile(lock, m.ownerUID, 0o600); err != nil {
		return fmt.Errorf("validate helper lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return ErrOperationInProgress
		}
		return fmt.Errorf("lock helper state: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return work()
}

func (m *Manager) readCurrentPolicies() ([]policyState, error) {
	entries, err := os.ReadDir(m.sysfsRoot)
	if err != nil {
		return nil, fmt.Errorf("enumerate CPU policies: %w", err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		path := filepath.Join(m.sysfsRoot, entry.Name())
		if validPolicyPath(m.sysfsRoot, path) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, errors.New("no CPU frequency policies found")
	}
	if len(paths) > maxPolicyCount {
		return nil, errors.New("too many CPU frequency policies")
	}
	policies := make([]policyState, 0, len(paths))
	for _, path := range paths {
		state, err := m.readPolicy(path)
		if err != nil {
			return nil, err
		}
		policies = append(policies, state)
	}
	return policies, nil
}

func (m *Manager) readPolicy(path string) (policyState, error) {
	epp, err := m.readCPUControl(filepath.Join(path, eppControl))
	if err != nil {
		return policyState{}, fmt.Errorf("read EPP control: %w", err)
	}
	if !validEPP(epp) {
		return policyState{}, errors.New("CPU policy has an invalid EPP value")
	}
	maxFreq, err := m.readCPUControl(filepath.Join(path, maxFreqControl))
	if err != nil {
		return policyState{}, fmt.Errorf("read max-frequency control: %w", err)
	}
	if !validFrequency(maxFreq) {
		return policyState{}, errors.New("CPU policy has an invalid max-frequency value")
	}
	return policyState{Path: path, EnergyPerformancePreference: epp, ScalingMaxFreq: maxFreq}, nil
}

func (m *Manager) applyLowPower(policies []policyState) error {
	expected := make([]policyState, 0, len(policies))
	for _, policy := range policies {
		if err := m.writeControl(filepath.Join(policy.Path, eppControl), LowPowerEPP); err != nil {
			return err
		}
		if err := m.writeControl(filepath.Join(policy.Path, maxFreqControl), LowPowerMaxFreq); err != nil {
			return err
		}
		expected = append(expected, policyState{
			Path:                        policy.Path,
			EnergyPerformancePreference: LowPowerEPP,
			ScalingMaxFreq:              LowPowerMaxFreq,
		})
	}
	return m.eventuallyVerifyPolicyStates(expected)
}

func (m *Manager) restoreSnapshot(policies []policyState) error {
	var restoreErrors []error
	for _, policy := range policies {
		if err := m.writeControl(filepath.Join(policy.Path, maxFreqControl), policy.ScalingMaxFreq); err != nil {
			restoreErrors = append(restoreErrors, err)
		}
		if err := m.writeControl(filepath.Join(policy.Path, eppControl), policy.EnergyPerformancePreference); err != nil {
			restoreErrors = append(restoreErrors, err)
		}
	}
	if err := m.eventuallyVerifyPolicyStates(policies); err != nil {
		restoreErrors = append(restoreErrors, err)
	}
	return errors.Join(restoreErrors...)
}

func (m *Manager) eventuallyVerifyPolicyStates(policies []policyState) error {
	var lastErr error
	for attempt := 0; attempt < verificationAttempts; attempt++ {
		lastErr = m.verifyPolicyStates(policies)
		if lastErr == nil {
			return nil
		}
		if !errors.Is(lastErr, errPolicyVerificationMismatch) {
			return lastErr
		}
		if attempt+1 < verificationAttempts {
			m.waitForVerification(verificationRetryInterval)
		}
	}
	return fmt.Errorf("CPU policy verification did not converge after %d attempts: %w", verificationAttempts, lastErr)
}

func (m *Manager) verifyPolicyStates(policies []policyState) error {
	var verificationErrors []error
	for _, policy := range policies {
		if err := m.verifyPolicy(policy.Path, policy.EnergyPerformancePreference, policy.ScalingMaxFreq); err != nil {
			if !errors.Is(err, errPolicyVerificationMismatch) {
				return err
			}
			verificationErrors = append(verificationErrors, err)
		}
	}
	return errors.Join(verificationErrors...)
}

func (m *Manager) verifyPolicy(path, wantEPP, wantMaxFreq string) error {
	gotEPP, err := m.readCPUControl(filepath.Join(path, eppControl))
	if err != nil {
		return err
	}
	gotMaxFreq, err := m.readCPUControl(filepath.Join(path, maxFreqControl))
	if err != nil {
		return err
	}
	if gotEPP != wantEPP || gotMaxFreq != wantMaxFreq {
		return errPolicyVerificationMismatch
	}
	return nil
}

func (m *Manager) readCPUControl(path string) (string, error) {
	if m.readControl == nil {
		return readControlValue(path)
	}
	return m.readControl(path)
}

func (m *Manager) waitForVerification(delay time.Duration) {
	if m.wait == nil {
		time.Sleep(delay)
		return
	}
	m.wait(delay)
}

func (m *Manager) persistState(state leaseState) error {
	if err := validateLeaseState(m.sysfsRoot, state); err != nil {
		return err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode lease state: %w", err)
	}
	if len(encoded) > stateMaxBytes {
		return errors.New("lease state is too large")
	}
	dir := filepath.Dir(m.statePath)
	temp, err := os.CreateTemp(dir, ".lease-*")
	if err != nil {
		return fmt.Errorf("create temporary lease state: %w", err)
	}
	tempPath := temp.Name()
	keepTemp := true
	defer func() {
		temp.Close()
		if keepTemp {
			os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temp.Write(encoded); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Link(tempPath, m.statePath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrActiveLease
		}
		return fmt.Errorf("publish lease state: %w", err)
	}
	if err := os.Remove(tempPath); err != nil {
		return fmt.Errorf("remove temporary lease state: %w", err)
	}
	keepTemp = false
	return syncDirectory(dir)
}

func (m *Manager) loadState() (leaseState, bool, error) {
	info, err := os.Lstat(m.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return leaseState{}, false, nil
	}
	if err != nil {
		return leaseState{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || fileOwnerUID(info) != m.ownerUID {
		return leaseState{}, false, errors.New("lease state ownership or mode is invalid")
	}
	fd, err := syscall.Open(m.statePath, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return leaseState{}, false, err
	}
	file := os.NewFile(uintptr(fd), m.statePath)
	defer file.Close()
	if err := validateOwnedRegularFile(file, m.ownerUID, 0o600); err != nil {
		return leaseState{}, false, err
	}
	encoded, err := io.ReadAll(io.LimitReader(file, stateMaxBytes+1))
	if err != nil {
		return leaseState{}, false, err
	}
	if len(encoded) > stateMaxBytes {
		return leaseState{}, false, errors.New("lease state is too large")
	}
	var state leaseState
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return leaseState{}, false, errors.New("lease state JSON is invalid")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return leaseState{}, false, err
	}
	if err := validateLeaseState(m.sysfsRoot, state); err != nil {
		return leaseState{}, false, err
	}
	return state, true, nil
}

func validateLeaseState(sysfsRoot string, state leaseState) error {
	if state.Version != stateVersion || len(state.Policies) == 0 || len(state.Policies) > maxPolicyCount {
		return errors.New("lease state structure is invalid")
	}
	previous := ""
	for _, policy := range state.Policies {
		if !validPolicyPath(sysfsRoot, policy.Path) || !validEPP(policy.EnergyPerformancePreference) || !validFrequency(policy.ScalingMaxFreq) {
			return errors.New("lease state policy is invalid")
		}
		if previous != "" && policy.Path <= previous {
			return errors.New("lease state policies are not uniquely sorted")
		}
		previous = policy.Path
	}
	return nil
}

func validPolicyPath(sysfsRoot, path string) bool {
	root := filepath.Clean(sysfsRoot)
	clean := filepath.Clean(path)
	if sysfsRoot != root || path != clean || filepath.Dir(clean) != root {
		return false
	}
	name := filepath.Base(clean)
	if !strings.HasPrefix(name, "policy") || len(name) == len("policy") {
		return false
	}
	for _, char := range name[len("policy"):] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func validEPP(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func validFrequency(value string) bool {
	if value == "" || len(value) > 9 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	frequency, err := strconv.ParseUint(value, 10, 64)
	return err == nil && frequency > 0 && frequency <= 100_000_000
}

func readControlValue(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	value, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil {
		return "", err
	}
	if len(value) > 128 {
		return "", errors.New("CPU control value is too large")
	}
	return strings.TrimSpace(string(value)), nil
}

func writeControlValue(path, value string) error {
	return os.WriteFile(path, []byte(value), 0)
}

func validateOwnedRegularFile(file *os.File, ownerUID uint32, mode os.FileMode) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != mode || fileOwnerUID(info) != ownerUID {
		return errors.New("file ownership or mode is invalid")
	}
	return nil
}

func fileOwnerUID(info os.FileInfo) uint32 {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ^uint32(0)
	}
	return stat.Uid
}

func (m *Manager) removeState() error {
	if err := os.Remove(m.statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(m.statePath))
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("lease state JSON has trailing data")
	}
	return nil
}
