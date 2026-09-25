package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	phase2BMaxLogicalReads = 6
	phase2BMaxCostUnits    = 12
)

var (
	errEvidencePlanBudget           = errors.New("phase2b evidence plan exceeds budget")
	errEvidencePlanInvalidParameter = errors.New("phase2b evidence parameter policy rejected request")
)

// CapabilityRequest is a deterministic, read-only unit of Phase 2B work.
// Arguments are still validated by the existing capability handler at
// execution time; planning never replaces that safety boundary.
type CapabilityRequest struct {
	ID               string                      `json:"id"`
	Order            int                         `json:"order"`
	Capability       string                      `json:"capability"`
	Arguments        map[string]any              `json:"arguments"`
	RequirementIDs   []string                    `json:"requirementIds"`
	DependsOn        []string                    `json:"dependsOn,omitempty"`
	Criticality      RequirementCriticality      `json:"criticality"`
	ConcurrencyKey   string                      `json:"concurrencyKey,omitempty"`
	CostUnits        int                         `json:"costUnits"`
	MaxFanout        int                         `json:"maxFanout"`
	ArgumentBindings []CapabilityArgumentBinding `json:"argumentBindings,omitempty"`
}

type CapabilityArgumentBinding struct {
	Name          string `json:"name"`
	FromRequestID string `json:"fromRequestId"`
	Selector      string `json:"selector"`
}

func (request CapabilityRequest) withStableID() CapabilityRequest {
	request.Arguments = cloneCanonicalArguments(request.Arguments)
	request.RequirementIDs = sortedUniqueContractStrings(request.RequirementIDs)
	request.DependsOn = sortedUniqueContractStrings(request.DependsOn)
	request.ArgumentBindings = canonicalCapabilityBindings(request.ArgumentBindings)
	request.ID = stableContractID("request", struct {
		Capability       string
		Arguments        map[string]any
		RequirementIDs   []string
		DependsOn        []string
		ArgumentBindings []CapabilityArgumentBinding
	}{request.Capability, request.Arguments, request.RequirementIDs, request.DependsOn, request.ArgumentBindings})
	return request
}

type EvidencePlan struct {
	RouteID      string              `json:"routeId"`
	Requests     []CapabilityRequest `json:"requests"`
	Missing      []MissingEvidence   `json:"missing,omitempty"`
	LogicalReads int                 `json:"logicalReads"`
	CostUnits    int                 `json:"costUnits"`
}

type evidencePlanBudgetError struct {
	LogicalReads int
	CostUnits    int
}

func (err evidencePlanBudgetError) Error() string {
	return fmt.Sprintf("%v: logical reads %d/%d, cost units %d/%d", errEvidencePlanBudget, err.LogicalReads, phase2BMaxLogicalReads, err.CostUnits, phase2BMaxCostUnits)
}

func (err evidencePlanBudgetError) Unwrap() error { return errEvidencePlanBudget }

// Explicit provider order is intentionally independent of registry order.
// The first registered read provider wins.
var evidenceProviderPriority = map[EvidenceType][]string{
	EvidenceTypeCurrentPlatformState:  {"get_platform_overview", "get_app_context"},
	EvidenceTypeCurrentDeploymentList: {"list_apps"},
	EvidenceTypeCurrentDeployment:     {"list_apps", "get_app_context"},
	EvidenceTypeCurrentApplication:    {"get_app_context", "list_apps"},
	EvidenceTypeHostHistory:           {"read_host_history"},
	EvidenceTypeThermalHistory:        {"read_temperature_history"},
	EvidenceTypeApplicationHistory:    {"read_application_history"},
	EvidenceTypeServiceHistory:        {"read_service_history"},
	EvidenceTypeInfrastructureEvents:  {"read_infrastructure_events"},
	EvidenceTypeCurrentDatabase:       {"list_databases", "get_app_context"},
	EvidenceTypeDatabaseBackups:       {"read_database_backups"},
	EvidenceTypeActivityTimeline:      {"read_activity"},
	EvidenceTypeRecoveryTimeline:      {"read_recovery"},
	EvidenceTypeRepositoryInventory:   {"list_repository_directory"},
	EvidenceTypeRepositorySearch:      {"search_repository"},
	EvidenceTypeRepositoryContent:     {"read_repository_file"},
	EvidenceTypeRuntimeFailures:       {"read_runtime_logs"},
	EvidenceTypeDeploymentFailures:    {"read_deployment_logs"},
	EvidenceTypeDeploymentTimeline:    {"read_deployment_history"},
}

// BuildEvidencePlan converts canonical Phase 2A requirements into a bounded,
// deterministic Phase 2B capability plan. It has no model dependency.
func BuildEvidencePlan(route InvestigationRoute, requirements []EvidenceRequirement, registry capabilityRegistry, now time.Time) (EvidencePlan, error) {
	return buildEvidencePlan(route, requirements, registry, now, "")
}

func BuildEvidencePlanForQuestion(route InvestigationRoute, requirements []EvidenceRequirement, registry capabilityRegistry, now time.Time, normalizedQuestion string) (EvidencePlan, error) {
	return buildEvidencePlan(route, requirements, registry, now, normalizedQuestion)
}

func buildEvidencePlan(route InvestigationRoute, requirements []EvidenceRequirement, registry capabilityRegistry, now time.Time, normalizedQuestion string) (EvidencePlan, error) {
	if now.IsZero() {
		now = time.Unix(0, 0).UTC()
	}
	now = now.UTC()
	canonicalRequirements := append([]EvidenceRequirement(nil), requirements...)
	for index := range canonicalRequirements {
		canonicalRequirements[index] = canonicalRequirements[index].WithStableID(string(route.ID))
	}
	sort.Slice(canonicalRequirements, func(i, j int) bool {
		return canonicalRequirements[i].ID < canonicalRequirements[j].ID
	})

	plan := EvidencePlan{RouteID: string(route.ID), Requests: []CapabilityRequest{}, Missing: []MissingEvidence{}}
	deduplicated := map[string]int{}
	deferredRepositoryContent := []EvidenceRequirement{}
	for _, requirement := range canonicalRequirements {
		provider, metadata, ok := selectEvidenceProvider(requirement, registry)
		if !ok {
			plan.Missing = append(plan.Missing, missingForPlanning(requirement, "no registered read provider"))
			continue
		}
		if provider == "read_repository_file" && normalizedQuestion != "" {
			deferredRepositoryContent = append(deferredRepositoryContent, requirement)
			continue
		}
		arguments, err := evidenceArgumentsForQuestion(requirement, provider, route.Frame, now, normalizedQuestion)
		if err != nil {
			plan.Missing = append(plan.Missing, MissingEvidence{
				RequirementID: requirement.ID,
				Criticality:   requirement.Criticality,
				Capability:    provider,
				Availability:  AvailabilityUnsupported,
				ErrorCategory: EvidenceErrorInvalidRequest,
				Reason:        err.Error(),
			})
			continue
		}
		request := CapabilityRequest{
			Capability:     provider,
			Arguments:      arguments,
			RequirementIDs: []string{requirement.ID},
			Criticality:    requirement.Criticality,
			ConcurrencyKey: metadata.ConcurrencyKey,
			CostUnits:      metadata.CostUnits,
			MaxFanout:      metadata.MaxFanout,
		}
		key := capabilityRequestDedupKey(request)
		if requestIndex, exists := deduplicated[key]; exists {
			existing := plan.Requests[requestIndex]
			existing.RequirementIDs = append(existing.RequirementIDs, requirement.ID)
			existing.Criticality = strongerCriticality(existing.Criticality, requirement.Criticality)
			plan.Requests[requestIndex] = existing
			continue
		}
		if request.CostUnits < 1 {
			request.CostUnits = 1
		}
		if request.MaxFanout < 1 {
			request.MaxFanout = 1
		}
		deduplicated[key] = len(plan.Requests)
		plan.Requests = append(plan.Requests, request)
	}
	for index := range plan.Requests {
		plan.Requests[index] = plan.Requests[index].withStableID()
	}
	deduplicated = make(map[string]int, len(plan.Requests)+len(deferredRepositoryContent))
	for index, request := range plan.Requests {
		deduplicated[capabilityRequestDedupKey(request)] = index
	}
	for _, requirement := range deferredRepositoryContent {
		parentIndex := -1
		for index, request := range plan.Requests {
			if request.Capability == "search_repository" && stringArg(request.Arguments, "app") == resolvedEvidenceSubject(requirement.Subject) {
				parentIndex = index
				break
			}
		}
		if parentIndex < 0 {
			plan.Missing = append(plan.Missing, MissingEvidence{RequirementID: requirement.ID, Criticality: requirement.Criticality, Capability: "read_repository_file", Availability: AvailabilityUnsupported, ErrorCategory: EvidenceErrorInvalidRequest, Reason: "repository content requires a planned safe search dependency"})
			continue
		}
		metadata := phase2CapabilityMetadata("read_repository_file")
		appName := resolvedEvidenceSubject(requirement.Subject)
		request := CapabilityRequest{
			Capability: "read_repository_file", Arguments: map[string]any{"app": appName}, RequirementIDs: []string{requirement.ID},
			DependsOn: []string{plan.Requests[parentIndex].ID}, Criticality: requirement.Criticality,
			ConcurrencyKey: metadata.ConcurrencyKey, CostUnits: metadata.CostUnits, MaxFanout: metadata.MaxFanout,
			ArgumentBindings: []CapabilityArgumentBinding{{Name: "path", FromRequestID: plan.Requests[parentIndex].ID, Selector: "first_repository_search_path"}},
		}
		key := capabilityRequestDedupKey(request)
		if requestIndex, exists := deduplicated[key]; exists {
			existing := plan.Requests[requestIndex]
			existing.RequirementIDs = append(existing.RequirementIDs, requirement.ID)
			existing.Criticality = strongerCriticality(existing.Criticality, requirement.Criticality)
			plan.Requests[requestIndex] = existing
			continue
		}
		deduplicated[key] = len(plan.Requests)
		plan.Requests = append(plan.Requests, request)
	}

	sort.Slice(plan.Requests, func(i, j int) bool {
		left, right := plan.Requests[i], plan.Requests[j]
		if evidenceCapabilityOrder(left.Capability) != evidenceCapabilityOrder(right.Capability) {
			return evidenceCapabilityOrder(left.Capability) < evidenceCapabilityOrder(right.Capability)
		}
		if left.Capability != right.Capability {
			return left.Capability < right.Capability
		}
		leftArguments, rightArguments := canonicalJSON(left.Arguments), canonicalJSON(right.Arguments)
		if leftArguments != rightArguments {
			return leftArguments < rightArguments
		}
		return capabilityRequestDedupKey(left) < capabilityRequestDedupKey(right)
	})
	for index := range plan.Requests {
		plan.Requests[index].Order = index
		plan.Requests[index] = plan.Requests[index].withStableID()
		plan.CostUnits += plan.Requests[index].CostUnits
	}
	plan.LogicalReads = len(plan.Requests)
	sort.Slice(plan.Missing, func(i, j int) bool { return canonicalJSON(plan.Missing[i]) < canonicalJSON(plan.Missing[j]) })
	if plan.LogicalReads > phase2BMaxLogicalReads || plan.CostUnits > phase2BMaxCostUnits {
		return EvidencePlan{}, evidencePlanBudgetError{LogicalReads: plan.LogicalReads, CostUnits: plan.CostUnits}
	}
	return plan, nil
}

func selectEvidenceProvider(requirement EvidenceRequirement, registry capabilityRegistry) (string, CapabilityMetadata, bool) {
	for _, name := range evidenceProviderPriority[requirement.Type] {
		item, ok := registry.lookup(name)
		if !ok || item.Kind != capabilityKindRead || item.handler == nil || !providerSupportsEvidenceSubject(name, requirement) {
			continue
		}
		for _, provided := range item.Metadata.EvidenceTypes {
			if provided == requirement.Type {
				return name, item.Metadata, true
			}
		}
	}
	return "", CapabilityMetadata{}, false
}

func providerSupportsEvidenceSubject(capability string, requirement EvidenceRequirement) bool {
	kind := requirement.Subject.Kind
	switch capability {
	case "get_platform_overview", "read_host_history", "read_temperature_history", "read_infrastructure_events", "read_activity", "read_recovery":
		return kind == SubjectHost || kind == SubjectPlatform
	case "list_apps":
		if requirement.Type == EvidenceTypeCurrentDeploymentList {
			return kind == SubjectHost || kind == SubjectPlatform || kind == SubjectUnknown
		}
		return kind == SubjectApplication
	case "get_app_context", "read_application_history", "read_runtime_logs", "read_deployment_logs", "read_deployment_history":
		return kind == SubjectApplication
	case "read_service_history":
		return kind == SubjectService || kind == SubjectHost || kind == SubjectPlatform
	case "list_databases", "read_database_backups":
		return kind == SubjectDatabase
	case "list_repository_directory", "search_repository", "read_repository_file":
		return kind == SubjectRepository || kind == SubjectApplication
	default:
		return false
	}
}

func capabilityRequestDedupKey(request CapabilityRequest) string {
	return canonicalJSON(struct {
		Capability       string
		Arguments        map[string]any
		DependsOn        []string
		ArgumentBindings []CapabilityArgumentBinding
	}{
		Capability:       request.Capability,
		Arguments:        cloneCanonicalArguments(request.Arguments),
		DependsOn:        sortedUniqueContractStrings(request.DependsOn),
		ArgumentBindings: canonicalCapabilityBindings(request.ArgumentBindings),
	})
}

func missingForPlanning(requirement EvidenceRequirement, reason string) MissingEvidence {
	return MissingEvidence{
		RequirementID: requirement.ID,
		Criticality:   requirement.Criticality,
		Availability:  AvailabilityUnsupported,
		ErrorCategory: EvidenceErrorInvalidRequest,
		Reason:        reason,
	}
}

func evidenceArguments(requirement EvidenceRequirement, capability string, frame QuestionFrame, now time.Time) (map[string]any, error) {
	return evidenceArgumentsForQuestion(requirement, capability, frame, now, "")
}

func evidenceArgumentsForQuestion(requirement EvidenceRequirement, capability string, frame QuestionFrame, now time.Time, normalizedQuestion string) (map[string]any, error) {
	arguments := map[string]any{}
	resolvedSubject := resolvedEvidenceSubject(requirement.Subject)

	switch capability {
	case "get_app_context", "read_application_history", "read_runtime_logs", "read_deployment_logs", "read_deployment_history":
		if resolvedSubject == "" || resolvedSubject == "unresolved" {
			return nil, fmt.Errorf("%w: resolved application subject required", errEvidencePlanInvalidParameter)
		}
		arguments["app"] = resolvedSubject
	case "read_service_history":
		if requirement.Subject.Kind == SubjectService && resolvedSubject != "" && resolvedSubject != "unresolved" {
			arguments["service"] = resolvedSubject
		}
	case "read_database_backups":
		databaseID := strings.TrimSpace(requirement.Subject.ID)
		if databaseID == "" || databaseID == "all" || databaseID == "unresolved" {
			return nil, fmt.Errorf("%w: canonical database ID required", errEvidencePlanInvalidParameter)
		}
		arguments["database_id"] = databaseID
		arguments["limit"] = 100
	case "read_infrastructure_events", "read_activity":
		arguments["limit"] = 100
	case "list_repository_directory":
		if resolvedSubject == "" || resolvedSubject == "unresolved" {
			return nil, fmt.Errorf("%w: resolved repository application required", errEvidencePlanInvalidParameter)
		}
		arguments["app"] = resolvedSubject
		arguments["path"] = "."
	case "search_repository":
		query := repositoryQuery(frame, normalizedQuestion)
		if resolvedSubject == "" || resolvedSubject == "unresolved" || query == "" {
			return nil, fmt.Errorf("%w: resolved repository application and bounded query required", errEvidencePlanInvalidParameter)
		}
		arguments["app"] = resolvedSubject
		arguments["path"] = "."
		arguments["query"] = query
	case "read_repository_file":
		return nil, fmt.Errorf("%w: repository path requires a prior safe search result", errEvidencePlanInvalidParameter)
	}

	if capability == "read_runtime_logs" || capability == "read_deployment_logs" {
		if err := validateLimitOnlyHistoricalWindow(requirement.Window, now); err != nil {
			return nil, err
		}
	}
	if capabilityUsesHistoricalWindow(capability) {
		windowArguments, err := evidenceWindowArguments(requirement.Window, now)
		if err != nil {
			return nil, err
		}
		for key, value := range windowArguments {
			arguments[key] = value
		}
	}
	return cloneCanonicalArguments(arguments), nil
}

func repositoryQuery(frame QuestionFrame, normalizedQuestion string) string {
	applications := []string{}
	for _, subject := range frame.Subjects {
		if subject.Kind == SubjectApplication || subject.Kind == SubjectRepository {
			applications = append(applications, subject.ID, subject.Name)
		}
	}
	return repositorySeedQuery(normalizedQuestion, applications)
}

func resolvedEvidenceSubject(subject EvidenceSubject) string {
	resolved := strings.TrimSpace(subject.ID)
	if resolved == "" || resolved == "unresolved" || resolved == "all" {
		resolved = strings.TrimSpace(subject.Name)
	}
	return resolved
}

func capabilityUsesHistoricalWindow(capability string) bool {
	switch capability {
	case "read_host_history", "read_temperature_history", "read_application_history", "read_service_history", "read_infrastructure_events":
		return true
	default:
		return false
	}
}

func validateLimitOnlyHistoricalWindow(scope *TemporalScope, now time.Time) error {
	if scope == nil || scope.Kind == TemporalUnspecified {
		return nil
	}
	if !scope.Valid {
		return fmt.Errorf("%w: invalid temporal scope", errEvidencePlanInvalidParameter)
	}
	switch scope.Kind {
	case TemporalNow:
		return nil
	case TemporalNamedWindow:
		if !validReactorLabRange(scope.NamedRange) {
			return fmt.Errorf("%w: unsupported named historical range", errEvidencePlanInvalidParameter)
		}
		return nil
	case TemporalToday, TemporalYesterday, TemporalExplicitWindow, TemporalIncidentCentered:
		if scope.From == nil || scope.To == nil {
			return fmt.Errorf("%w: bounded from/to timestamps required", errEvidencePlanInvalidParameter)
		}
		from, to := scope.From.UTC(), scope.To.UTC()
		if !from.Before(to) || to.After(now.UTC()) || to.Sub(from) > maximumHistoricalWindow {
			return fmt.Errorf("%w: historical window outside retained past data", errEvidencePlanInvalidParameter)
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported temporal scope", errEvidencePlanInvalidParameter)
	}
}

func evidenceWindowArguments(scope *TemporalScope, now time.Time) (map[string]any, error) {
	if scope == nil || scope.Kind == TemporalUnspecified {
		return map[string]any{}, nil
	}
	if !scope.Valid {
		return nil, fmt.Errorf("%w: invalid temporal scope", errEvidencePlanInvalidParameter)
	}
	switch scope.Kind {
	case TemporalNamedWindow:
		if !validReactorLabRange(scope.NamedRange) {
			return nil, fmt.Errorf("%w: unsupported named historical range", errEvidencePlanInvalidParameter)
		}
		return map[string]any{"range": scope.NamedRange}, nil
	case TemporalToday, TemporalYesterday, TemporalExplicitWindow, TemporalIncidentCentered:
		if scope.From == nil || scope.To == nil {
			return nil, fmt.Errorf("%w: bounded from/to timestamps required", errEvidencePlanInvalidParameter)
		}
		from, to := scope.From.UTC(), scope.To.UTC()
		if !from.Before(to) || to.After(now) || to.Sub(from) > maximumHistoricalWindow {
			return nil, fmt.Errorf("%w: historical window outside retained past data", errEvidencePlanInvalidParameter)
		}
		return map[string]any{
			"from": from.Format(time.RFC3339Nano),
			"to":   to.Format(time.RFC3339Nano),
		}, nil
	case TemporalNow:
		return nil, fmt.Errorf("%w: current time is not silently widened to a historical window", errEvidencePlanInvalidParameter)
	default:
		return nil, fmt.Errorf("%w: unsupported temporal scope", errEvidencePlanInvalidParameter)
	}
}

func strongerCriticality(left, right RequirementCriticality) RequirementCriticality {
	rank := func(value RequirementCriticality) int {
		switch value {
		case CriticalityCritical:
			return 3
		case CriticalityRelevant:
			return 2
		default:
			return 1
		}
	}
	if rank(right) > rank(left) {
		return right
	}
	return left
}

func cloneCanonicalArguments(arguments map[string]any) map[string]any {
	if len(arguments) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(arguments))
	keys := make([]string, 0, len(arguments))
	for key := range arguments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out[key] = arguments[key]
	}
	return out
}

func canonicalCapabilityBindings(bindings []CapabilityArgumentBinding) []CapabilityArgumentBinding {
	out := append([]CapabilityArgumentBinding(nil), bindings...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].FromRequestID != out[j].FromRequestID {
			return out[i].FromRequestID < out[j].FromRequestID
		}
		return out[i].Selector < out[j].Selector
	})
	return out
}

func evidenceCapabilityOrder(name string) int {
	order := []string{
		"get_platform_overview", "get_app_context", "list_apps", "list_databases",
		"read_recovery", "read_infrastructure_events", "read_activity",
		"read_host_history", "read_temperature_history", "read_application_history", "read_service_history",
		"read_deployment_history", "read_database_backups", "read_runtime_logs", "read_deployment_logs",
		"list_repository_directory", "search_repository", "read_repository_file",
	}
	for index, candidate := range order {
		if candidate == name {
			return index
		}
	}
	return len(order)
}
