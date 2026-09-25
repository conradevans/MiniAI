package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const phase2BGlobalWorkerLimit = 4

var errEvidenceExecutionInvalidPlan = errors.New("invalid phase2b evidence plan")

type EvidenceResult struct {
	RequestID       string                `json:"requestId"`
	PlanOrder       int                   `json:"planOrder"`
	Capability      string                `json:"capability"`
	Arguments       map[string]any        `json:"arguments"`
	RequirementIDs  []string              `json:"requirementIds"`
	StartedAt       time.Time             `json:"startedAt"`
	CompletedAt     time.Time             `json:"completedAt"`
	Availability    Availability          `json:"availability"`
	ErrorCategory   EvidenceErrorCategory `json:"errorCategory,omitempty"`
	SafeResult      any                   `json:"safeResult,omitempty"`
	SafeSummary     string                `json:"safeSummary,omitempty"`
	Truncation      EvidenceTruncation    `json:"truncation"`
	CostUnits       int                   `json:"costUnits"`
	MaxFanout       int                   `json:"maxFanout"`
	ConcurrencyKey  string                `json:"concurrencyKey,omitempty"`
	DependencyError bool                  `json:"dependencyError,omitempty"`
}

type EvidenceExecutor struct {
	registry capabilityRegistry
	executor capabilityExecutor
	now      func() time.Time
	limits   map[string]int
}

func NewEvidenceExecutor(registry capabilityRegistry, executor capabilityExecutor, now func() time.Time) EvidenceExecutor {
	if now == nil {
		now = time.Now
	}
	return EvidenceExecutor{
		registry: registry,
		executor: executor,
		now:      now,
		limits: map[string]int{
			"reactorlab":  4,
			"minideploy":  2,
			"repository":  1,
			"app_context": 1,
		},
	}
}

func (executor EvidenceExecutor) Execute(ctx context.Context, plan EvidencePlan) ([]EvidenceResult, error) {
	if err := validateExecutableEvidencePlan(plan, executor.registry); err != nil {
		return nil, err
	}
	if executor.executor == nil {
		return nil, fmt.Errorf("%w: capability executor is nil", errEvidenceExecutionInvalidPlan)
	}
	childContext, cancel := context.WithCancel(ctx)
	defer cancel()

	requests := append([]CapabilityRequest(nil), plan.Requests...)
	requestByID := make(map[string]CapabilityRequest, len(requests))
	done := make(map[string]chan struct{}, len(requests))
	for _, request := range requests {
		requestByID[request.ID] = request
		done[request.ID] = make(chan struct{})
	}
	global := make(chan struct{}, phase2BGlobalWorkerLimit)
	backend := map[string]chan struct{}{}
	for _, request := range requests {
		key := request.ConcurrencyKey
		if key == "" {
			key = "default"
		}
		if _, exists := backend[key]; exists {
			continue
		}
		limit := executor.limits[key]
		if limit < 1 {
			limit = 1
		}
		backend[key] = make(chan struct{}, limit)
	}

	results := make(map[string]EvidenceResult, len(requests))
	var resultMu sync.Mutex
	var wait sync.WaitGroup
	for _, request := range requests {
		request := request
		wait.Add(1)
		go func() {
			defer wait.Done()
			defer close(done[request.ID])
			storeCancelled := func() {
				resultMu.Lock()
				results[request.ID] = executor.cancelledResult(request, childContext.Err())
				resultMu.Unlock()
			}
			if childContext.Err() != nil {
				storeCancelled()
				return
			}
			executionArguments := cloneCanonicalArguments(request.Arguments)
			for _, dependencyID := range request.DependsOn {
				if childContext.Err() != nil {
					storeCancelled()
					return
				}
				select {
				case <-childContext.Done():
					storeCancelled()
					return
				case <-done[dependencyID]:
				}
				if childContext.Err() != nil {
					storeCancelled()
					return
				}
				resultMu.Lock()
				dependency := results[dependencyID]
				resultMu.Unlock()
				if dependency.Availability != AvailabilityAvailable && dependency.Availability != AvailabilityPartial {
					at := executor.now().UTC()
					resultMu.Lock()
					results[request.ID] = EvidenceResult{
						RequestID: request.ID, PlanOrder: request.Order, Capability: request.Capability,
						Arguments: cloneCanonicalArguments(request.Arguments), RequirementIDs: sortedUniqueContractStrings(request.RequirementIDs),
						StartedAt: at, CompletedAt: at, Availability: AvailabilityUnavailable,
						ErrorCategory: EvidenceErrorUnavailable, CostUnits: request.CostUnits, MaxFanout: request.MaxFanout,
						ConcurrencyKey: request.ConcurrencyKey, DependencyError: true,
					}
					resultMu.Unlock()
					return
				}
			}
			if childContext.Err() != nil {
				storeCancelled()
				return
			}
			resolvedArguments, resolveErr := resolveEvidenceArgumentBindings(executionArguments, request.ArgumentBindings, results, &resultMu)
			if resolveErr != nil {
				at := executor.now().UTC()
				resultMu.Lock()
				results[request.ID] = EvidenceResult{
					RequestID: request.ID, PlanOrder: request.Order, Capability: request.Capability,
					Arguments: executionArguments, RequirementIDs: sortedUniqueContractStrings(request.RequirementIDs),
					StartedAt: at, CompletedAt: at, Availability: AvailabilityNotFound,
					ErrorCategory: EvidenceErrorNotFound, CostUnits: request.CostUnits, MaxFanout: request.MaxFanout,
					ConcurrencyKey: request.ConcurrencyKey, DependencyError: true,
				}
				resultMu.Unlock()
				return
			}
			executionArguments = resolvedArguments
			if childContext.Err() != nil {
				storeCancelled()
				return
			}

			key := request.ConcurrencyKey
			if key == "" {
				key = "default"
			}
			if !acquireEvidenceSlot(childContext, backend[key]) {
				storeCancelled()
				return
			}
			defer func() { <-backend[key] }()
			if !acquireEvidenceSlot(childContext, global) {
				storeCancelled()
				return
			}
			defer func() { <-global }()
			if childContext.Err() != nil {
				storeCancelled()
				return
			}

			started := executor.now().UTC()
			value, summary, err := executor.executor.Execute(childContext, request.Capability, cloneCanonicalArguments(executionArguments))
			completed := executor.now().UTC()
			result := EvidenceResult{
				RequestID: request.ID, PlanOrder: request.Order, Capability: request.Capability,
				Arguments: cloneCanonicalArguments(executionArguments), RequirementIDs: sortedUniqueContractStrings(request.RequirementIDs),
				StartedAt: started, CompletedAt: completed, SafeSummary: strings.TrimSpace(summary),
				CostUnits: request.CostUnits, MaxFanout: request.MaxFanout, ConcurrencyKey: request.ConcurrencyKey,
			}
			if err != nil {
				result.Availability, result.ErrorCategory = evidenceErrorState(err, childContext)
			} else {
				result.SafeResult = value
				result.Truncation = evidenceResultTruncation(value)
				result.Availability = AvailabilityAvailable
				if result.Truncation.Truncated {
					result.Availability = AvailabilityPartial
				}
			}
			resultMu.Lock()
			results[request.ID] = result
			resultMu.Unlock()
		}()
	}
	wait.Wait()

	ordered := make([]EvidenceResult, 0, len(requests))
	for _, request := range requests {
		ordered = append(ordered, results[request.ID])
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].PlanOrder != ordered[j].PlanOrder {
			return ordered[i].PlanOrder < ordered[j].PlanOrder
		}
		return ordered[i].RequestID < ordered[j].RequestID
	})
	return ordered, nil
}

func acquireEvidenceSlot(ctx context.Context, slot chan struct{}) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case slot <- struct{}{}:
		if ctx.Err() != nil {
			<-slot
			return false
		}
		return true
	}
}

func (executor EvidenceExecutor) cancelledResult(request CapabilityRequest, err error) EvidenceResult {
	at := executor.now().UTC()
	_, category := evidenceErrorState(err, context.Background())
	return EvidenceResult{
		RequestID: request.ID, PlanOrder: request.Order, Capability: request.Capability,
		Arguments: cloneCanonicalArguments(request.Arguments), RequirementIDs: sortedUniqueContractStrings(request.RequirementIDs),
		StartedAt: at, CompletedAt: at, Availability: AvailabilityUnavailable, ErrorCategory: category,
		CostUnits: request.CostUnits, MaxFanout: request.MaxFanout, ConcurrencyKey: request.ConcurrencyKey,
	}
}

func validateExecutableEvidencePlan(plan EvidencePlan, registry capabilityRegistry) error {
	logicalReads, costUnits := len(plan.Requests), 0
	for _, request := range plan.Requests {
		costUnits += request.CostUnits
	}
	if logicalReads > phase2BMaxLogicalReads || costUnits > phase2BMaxCostUnits {
		return evidencePlanBudgetError{LogicalReads: logicalReads, CostUnits: costUnits}
	}
	requestIDs := make(map[string]bool, len(plan.Requests))
	orders := make(map[int]bool, len(plan.Requests))
	for _, request := range plan.Requests {
		if request.ID == "" || requestIDs[request.ID] {
			return fmt.Errorf("%w: invalid or duplicate request ID %q", errEvidenceExecutionInvalidPlan, request.ID)
		}
		requestIDs[request.ID] = true
		if request.Order < 0 || orders[request.Order] {
			return fmt.Errorf("%w: invalid or duplicate plan order %d", errEvidenceExecutionInvalidPlan, request.Order)
		}
		orders[request.Order] = true
		item, ok := registry.lookup(request.Capability)
		if !ok || item.Kind != capabilityKindRead || item.handler == nil {
			return fmt.Errorf("%w: %q is not a registered read", errEvidenceExecutionInvalidPlan, request.Capability)
		}
		if request.CostUnits < 1 || request.MaxFanout < 1 {
			return fmt.Errorf("%w: invalid cost for %q", errEvidenceExecutionInvalidPlan, request.ID)
		}
		if request.CostUnits != item.Metadata.CostUnits || request.MaxFanout != item.Metadata.MaxFanout || request.ConcurrencyKey != item.Metadata.ConcurrencyKey {
			return fmt.Errorf("%w: request %q does not match registered cost/concurrency metadata", errEvidenceExecutionInvalidPlan, request.ID)
		}
	}
	for _, request := range plan.Requests {
		for _, dependencyID := range request.DependsOn {
			if !requestIDs[dependencyID] || dependencyID == request.ID {
				return fmt.Errorf("%w: bad dependency %q", errEvidenceExecutionInvalidPlan, dependencyID)
			}
		}
		for _, binding := range request.ArgumentBindings {
			if binding.Name == "" || binding.FromRequestID == "" || binding.Selector != "first_repository_search_path" || !requestIDs[binding.FromRequestID] {
				return fmt.Errorf("%w: invalid argument binding on %q", errEvidenceExecutionInvalidPlan, request.ID)
			}
			foundDependency := false
			for _, dependencyID := range request.DependsOn {
				foundDependency = foundDependency || dependencyID == binding.FromRequestID
			}
			if !foundDependency {
				return fmt.Errorf("%w: binding source is not a dependency on %q", errEvidenceExecutionInvalidPlan, request.ID)
			}
		}
	}
	state := map[string]uint8{}
	byID := map[string]CapabilityRequest{}
	for _, request := range plan.Requests {
		byID[request.ID] = request
	}
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("%w: dependency cycle", errEvidenceExecutionInvalidPlan)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, dependencyID := range byID[id].DependsOn {
			if err := visit(dependencyID); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range byID {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func resolveEvidenceArgumentBindings(arguments map[string]any, bindings []CapabilityArgumentBinding, results map[string]EvidenceResult, mu *sync.Mutex) (map[string]any, error) {
	resolved := cloneCanonicalArguments(arguments)
	for _, binding := range canonicalCapabilityBindings(bindings) {
		mu.Lock()
		dependency := results[binding.FromRequestID]
		mu.Unlock()
		switch binding.Selector {
		case "first_repository_search_path":
			search, ok := evidenceValue[repoSearchResponse](dependency.SafeResult)
			if !ok || len(search.Hits) == 0 {
				return nil, fmt.Errorf("repository search returned no safe path")
			}
			hits := append([]repoSearchHit(nil), search.Hits...)
			sort.Slice(hits, func(i, j int) bool {
				if hits[i].Path != hits[j].Path {
					return hits[i].Path < hits[j].Path
				}
				if hits[i].Line != hits[j].Line {
					return hits[i].Line < hits[j].Line
				}
				return hits[i].Text < hits[j].Text
			})
			resolved[binding.Name] = hits[0].Path
		default:
			return nil, fmt.Errorf("unsupported argument binding")
		}
	}
	return cloneCanonicalArguments(resolved), nil
}

func evidenceErrorState(err error, ctx context.Context) (Availability, EvidenceErrorCategory) {
	switch {
	case err == nil:
		return AvailabilityAvailable, ""
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return AvailabilityUnavailable, EvidenceErrorTimeout
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return AvailabilityUnavailable, EvidenceErrorUnavailable
	case errors.Is(err, errReactorLabInvalidRequest):
		return AvailabilityUnsupported, EvidenceErrorInvalidRequest
	case errors.Is(err, errReactorLabNotFound), errors.Is(err, os.ErrNotExist), errors.Is(err, errReactorLabApplicationNotFound), errors.Is(err, errReactorLabServiceNotFound):
		return AvailabilityNotFound, EvidenceErrorNotFound
	case errors.Is(err, errReactorLabMalformed):
		return AvailabilityUnavailable, EvidenceErrorMalformed
	case errors.Is(err, errReactorLabTooLarge), errors.Is(err, errRepoTooLarge):
		return AvailabilityUnavailable, EvidenceErrorTooLarge
	case errors.Is(err, errRepoForbidden), errors.Is(err, errRepoBinary):
		return AvailabilityUnavailable, EvidenceErrorUnauthorized
	case errors.Is(err, errReactorLabUnavailable):
		return AvailabilityUnavailable, EvidenceErrorUnavailable
	default:
		return AvailabilityUnavailable, EvidenceErrorInternal
	}
}

func evidenceResultTruncation(value any) EvidenceTruncation {
	switch result := value.(type) {
	case reactorLabAppListResult:
		return EvidenceTruncation{Truncated: result.Truncated, Returned: len(result.Apps), Total: result.TotalApps}
	case reactorLabAppContextResult:
		// Database lookup coverage is a supporting facet of this compound
		// response and is represented explicitly by the reducer.
		return EvidenceTruncation{}
	case reactorLabHostHistory:
		return EvidenceTruncation{Truncated: result.Truncated, Returned: len(result.Points), Total: result.TotalPoints}
	case reactorLabTemperatureHistory:
		return EvidenceTruncation{Truncated: result.Truncated, Returned: len(result.Points), Total: result.TotalPoints}
	case reactorLabResolvedApplicationHistory:
		return EvidenceTruncation{Truncated: result.History.Truncated, Returned: len(result.History.Points), Total: result.History.TotalPoints}
	case reactorLabServiceHistory:
		return EvidenceTruncation{Truncated: result.Truncated, Returned: len(result.Services), Total: result.TotalServices}
	case reactorLabEvents:
		return EvidenceTruncation{Truncated: result.Truncated, Returned: len(result.Events), Total: result.TotalEvents}
	case reactorLabDatabaseList:
		return EvidenceTruncation{Truncated: result.Truncated, Returned: len(result.Databases), Total: result.TotalDatabases}
	case reactorLabBackupList:
		return EvidenceTruncation{Truncated: result.Truncated, Returned: len(result.Backups), Total: result.TotalBackups}
	case reactorLabActivity:
		return EvidenceTruncation{Truncated: result.Truncated, Returned: len(result.Events), Total: result.TotalEvents}
	case reactorLabDeploymentHistory:
		return EvidenceTruncation{Truncated: result.Truncated, Returned: len(result.Versions), Total: result.TotalVersions}
	case repoListResponse:
		return EvidenceTruncation{Truncated: result.Truncated, Returned: len(result.Entries)}
	case repoSearchResponse:
		return EvidenceTruncation{Truncated: result.Truncated, Returned: len(result.Hits)}
	case logToolResponse:
		return EvidenceTruncation{Truncated: result.Truncated, Returned: result.Lines}
	default:
		return EvidenceTruncation{}
	}
}
