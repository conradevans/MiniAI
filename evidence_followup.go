package main

import (
	"sort"
	"strings"
)

const (
	phase2DMaxSecondRounds           = 1
	phase2DMaxAdditionalLogicalReads = 2
	phase2DMaxAdditionalCostUnits    = 4
)

type EvidenceGapKind string

const (
	EvidenceGapMissingCritical  EvidenceGapKind = "missing_critical"
	EvidenceGapPartialCritical  EvidenceGapKind = "partial_critical"
	EvidenceGapMaterialConflict EvidenceGapKind = "material_conflict"
)

type FollowUpDecisionReason string

const (
	FollowUpPlanned           FollowUpDecisionReason = "follow_up_planned"
	FollowUpRouteUnsupported  FollowUpDecisionReason = "route_not_supported"
	FollowUpNoMaterialGap     FollowUpDecisionReason = "no_material_gap"
	FollowUpSubjectUnresolved FollowUpDecisionReason = "subject_unresolved"
	FollowUpNoSafeCapability  FollowUpDecisionReason = "no_safe_capability"
	FollowUpDuplicateRead     FollowUpDecisionReason = "equivalent_read_already_executed"
	FollowUpFanoutExceeded    FollowUpDecisionReason = "follow_up_fanout_exceeded"
	FollowUpBudgetExceeded    FollowUpDecisionReason = "follow_up_budget_exceeded"
	FollowUpExecutionFailed   FollowUpDecisionReason = "follow_up_execution_failed"
	FollowUpReductionFailed   FollowUpDecisionReason = "follow_up_reduction_failed"
)

type EvidenceGap struct {
	Kind              EvidenceGapKind
	RequirementID     string
	EvidenceType      EvidenceType
	Criticality       RequirementCriticality
	Subject           EvidenceSubject
	MissingCapability string
	ConflictID        string
}

type FollowUpEvidenceCandidate struct {
	Gap     EvidenceGap
	Subject EvidenceSubject
	Request CapabilityRequest
}

type FollowUpEvidencePlan struct {
	Plan       EvidencePlan
	Candidates []FollowUpEvidenceCandidate
	Rounds     int
}

type SecondRoundDecision struct {
	Needed bool
	Reason FollowUpDecisionReason
	Gaps   []EvidenceGap
	FollowUpEvidencePlan
}

func PlanSecondEvidenceRound(prepared Phase2BPrepared, evidence InvestigationEvidence, firstPlan EvidencePlan, firstResults []EvidenceResult, registry capabilityRegistry) SecondRoundDecision {
	decision := SecondRoundDecision{
		Reason: FollowUpNoMaterialGap,
		FollowUpEvidencePlan: FollowUpEvidencePlan{
			Plan: EvidencePlan{RouteID: string(prepared.Route.ID), Requests: []CapabilityRequest{}, Missing: []MissingEvidence{}},
		},
	}
	if prepared.Route.Resolution != RouteResolutionSupported || !isPhase2CReasonedRoute(prepared.Route.ID) {
		decision.Reason = FollowUpRouteUnsupported
		return decision
	}

	decision.Gaps = materialEvidenceGaps(evidence)
	if len(decision.Gaps) == 0 {
		return decision
	}

	requirements := make(map[string]EvidenceRequirement, len(evidence.Requirements))
	for _, requirement := range evidence.Requirements {
		requirements[requirement.ID] = requirement
	}
	executed := executedEvidenceReadIdentities(firstPlan, firstResults)
	selected := map[string]bool{}
	rejection := FollowUpNoSafeCapability

	for _, gap := range decision.Gaps {
		requirement, ok := requirements[gap.RequirementID]
		if !ok {
			continue
		}
		subjects, rejected := followUpSubjects(requirement, evidence)
		if rejected != "" {
			rejection = strongerFollowUpRejection(rejection, rejected)
			continue
		}
		for _, subject := range subjects {
			candidate, rejected := followUpCandidateForSubject(
				prepared, gap, requirement, subject, registry, executed, selected,
			)
			if rejected != "" {
				rejection = strongerFollowUpRejection(rejection, rejected)
				continue
			}
			request := candidate.Request
			if len(decision.Plan.Requests) >= phase2DMaxAdditionalLogicalReads ||
				decision.Plan.CostUnits+request.CostUnits > phase2DMaxAdditionalCostUnits {
				rejection = strongerFollowUpRejection(rejection, FollowUpBudgetExceeded)
				continue
			}
			identity := evidenceReadIdentity(request.Capability, request.Arguments)
			selected[identity] = true
			decision.Plan.Requests = append(decision.Plan.Requests, request)
			decision.Plan.CostUnits += request.CostUnits
			decision.Candidates = append(decision.Candidates, candidate)
		}
	}

	if len(decision.Plan.Requests) == 0 {
		decision.Reason = rejection
		return decision
	}
	sort.Slice(decision.Plan.Requests, func(i, j int) bool {
		left, right := decision.Plan.Requests[i], decision.Plan.Requests[j]
		if evidenceCapabilityOrder(left.Capability) != evidenceCapabilityOrder(right.Capability) {
			return evidenceCapabilityOrder(left.Capability) < evidenceCapabilityOrder(right.Capability)
		}
		if left.Capability != right.Capability {
			return left.Capability < right.Capability
		}
		return canonicalJSON(left.Arguments) < canonicalJSON(right.Arguments)
	})
	candidatesByIdentity := make(map[string]FollowUpEvidenceCandidate, len(decision.Candidates))
	for _, candidate := range decision.Candidates {
		candidatesByIdentity[evidenceReadIdentity(candidate.Request.Capability, candidate.Request.Arguments)] = candidate
	}
	decision.Candidates = decision.Candidates[:0]
	for index := range decision.Plan.Requests {
		decision.Plan.Requests[index].Order = index
		decision.Plan.Requests[index] = decision.Plan.Requests[index].withStableID()
		identity := evidenceReadIdentity(decision.Plan.Requests[index].Capability, decision.Plan.Requests[index].Arguments)
		candidate := candidatesByIdentity[identity]
		candidate.Request = decision.Plan.Requests[index]
		decision.Candidates = append(decision.Candidates, candidate)
	}
	decision.Plan.LogicalReads = len(decision.Plan.Requests)
	decision.Needed = true
	decision.Reason = FollowUpPlanned
	decision.Rounds = phase2DMaxSecondRounds
	return decision
}

func materialEvidenceGaps(evidence InvestigationEvidence) []EvidenceGap {
	requirements := make(map[string]EvidenceRequirement, len(evidence.Requirements))
	for _, requirement := range evidence.Requirements {
		requirements[requirement.ID] = requirement
	}
	missing := append([]MissingEvidence(nil), evidence.Missing...)
	sort.Slice(missing, func(i, j int) bool { return canonicalJSON(missing[i]) < canonicalJSON(missing[j]) })
	byKey := map[string]EvidenceGap{}
	for _, limitation := range missing {
		criticality := limitation.Criticality
		requirement, ok := requirements[limitation.RequirementID]
		if !ok {
			continue
		}
		if criticality == "" {
			criticality = requirement.Criticality
		}
		if !isMaterialFollowUpMissing(requirement, limitation, criticality) {
			continue
		}
		kind := EvidenceGapMissingCritical
		if limitation.Availability == AvailabilityPartial {
			kind = EvidenceGapPartialCritical
		}
		gap := EvidenceGap{
			Kind: kind, RequirementID: requirement.ID, EvidenceType: requirement.Type,
			Criticality: criticality, Subject: requirement.Subject, MissingCapability: limitation.Capability,
		}
		key := "missing:" + requirement.ID
		if existing, exists := byKey[key]; !exists || (existing.Kind == EvidenceGapPartialCritical && kind == EvidenceGapMissingCritical) {
			byKey[key] = gap
		}
	}

	itemRequirements := map[string]string{}
	for _, item := range evidence.Items {
		itemRequirements[item.ID] = item.Relevance.RequirementID
	}
	for _, conflict := range evidence.Conflicts {
		if !conflict.Material {
			continue
		}
		requirementIDs := map[string]bool{}
		for _, evidenceID := range conflict.EvidenceIDs {
			if requirementID := itemRequirements[evidenceID]; requirementID != "" {
				requirementIDs[requirementID] = true
			}
		}
		keys := make([]string, 0, len(requirementIDs))
		for requirementID := range requirementIDs {
			keys = append(keys, requirementID)
		}
		sort.Strings(keys)
		for _, requirementID := range keys {
			requirement, ok := requirements[requirementID]
			if !ok {
				continue
			}
			key := "conflict:" + requirementID
			byKey[key] = EvidenceGap{
				Kind: EvidenceGapMaterialConflict, RequirementID: requirement.ID, EvidenceType: requirement.Type,
				Criticality: requirement.Criticality, Subject: requirement.Subject, ConflictID: conflict.ID,
			}
		}
	}

	gaps := make([]EvidenceGap, 0, len(byKey))
	for _, gap := range byKey {
		gaps = append(gaps, gap)
	}
	sort.Slice(gaps, func(i, j int) bool {
		if gaps[i].Kind != gaps[j].Kind {
			return gaps[i].Kind < gaps[j].Kind
		}
		if gaps[i].RequirementID != gaps[j].RequirementID {
			return gaps[i].RequirementID < gaps[j].RequirementID
		}
		return gaps[i].ConflictID < gaps[j].ConflictID
	})
	return gaps
}

func isMaterialFollowUpMissing(requirement EvidenceRequirement, limitation MissingEvidence, criticality RequirementCriticality) bool {
	if criticality == CriticalityCritical {
		return true
	}
	return criticality == CriticalityRelevant &&
		requirement.Type == EvidenceTypeDatabaseBackups &&
		limitation.Capability == "read_database_backups" &&
		!isResolvedFollowUpSubject(requirement.Subject)
}

func followUpSubjects(requirement EvidenceRequirement, evidence InvestigationEvidence) ([]EvidenceSubject, FollowUpDecisionReason) {
	if isResolvedFollowUpSubject(requirement.Subject) {
		return []EvidenceSubject{requirement.Subject}, ""
	}
	candidates := map[string]EvidenceSubject{}
	for _, item := range evidence.Items {
		if !followUpItemResolvesSubject(requirement.Subject.Kind, item) || !isResolvedFollowUpSubject(item.Subject) {
			continue
		}
		key := string(item.Subject.Kind) + ":" + item.Subject.ID
		candidates[key] = item.Subject
	}
	keys := make([]string, 0, len(candidates))
	for key := range candidates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return nil, FollowUpSubjectUnresolved
	}
	if len(keys) > phase2DMaxAdditionalLogicalReads || (requirement.Cardinality != CardinalityMany && len(keys) != 1) {
		return nil, FollowUpFanoutExceeded
	}
	subjects := make([]EvidenceSubject, 0, len(keys))
	for _, key := range keys {
		subjects = append(subjects, candidates[key])
	}
	return subjects, ""
}

func isResolvedFollowUpSubject(subject EvidenceSubject) bool {
	id := strings.TrimSpace(subject.ID)
	return subject.Kind != SubjectUnknown && id != "" && id != "all" && id != "unresolved"
}

func followUpItemResolvesSubject(kind SubjectKind, item EvidenceItem) bool {
	switch kind {
	case SubjectDatabase:
		return item.Type == EvidenceTypeCurrentDatabase && item.Subject.Kind == SubjectDatabase
	case SubjectApplication, SubjectService:
		return (item.Type == EvidenceTypeCurrentApplication || item.Type == EvidenceTypeCurrentDeployment || item.Type == EvidenceTypeCurrentDeploymentList) &&
			(item.Subject.Kind == SubjectApplication || item.Subject.Kind == SubjectService)
	case SubjectRepository:
		return (item.Type == EvidenceTypeRepositoryInventory || item.Type == EvidenceTypeRepositorySearch) &&
			(item.Subject.Kind == SubjectRepository || item.Subject.Kind == SubjectApplication)
	default:
		return false
	}
}

func followUpCandidateForSubject(prepared Phase2BPrepared, gap EvidenceGap, requirement EvidenceRequirement, subject EvidenceSubject, registry capabilityRegistry, executed, selected map[string]bool) (FollowUpEvidenceCandidate, FollowUpDecisionReason) {
	requirement.Subject = subject
	providers := followUpProviderOrder(gap, requirement.Type)
	sawDuplicate, sawBudget := false, false
	for _, provider := range providers {
		capability, ok := registry.lookup(provider)
		if !ok || capability.Kind != capabilityKindRead || capability.handler == nil || !providerSupportsEvidenceSubject(provider, requirement) || !capabilityProvidesEvidenceType(capability.Metadata, requirement.Type) {
			continue
		}
		arguments, err := evidenceArgumentsForQuestion(requirement, provider, prepared.Route.Frame, prepared.Now, prepared.NormalizedQuestion)
		if err != nil {
			continue
		}
		identity := evidenceReadIdentity(provider, arguments)
		if executed[identity] || selected[identity] {
			sawDuplicate = true
			continue
		}
		if capability.Metadata.CostUnits < 1 || capability.Metadata.CostUnits > phase2DMaxAdditionalCostUnits ||
			capability.Metadata.MaxFanout < 1 || capability.Metadata.MaxFanout > phase2DMaxAdditionalLogicalReads {
			sawBudget = true
			continue
		}
		request := CapabilityRequest{
			Capability: provider, Arguments: arguments, RequirementIDs: []string{gap.RequirementID},
			Criticality: gap.Criticality, ConcurrencyKey: capability.Metadata.ConcurrencyKey,
			CostUnits: capability.Metadata.CostUnits, MaxFanout: capability.Metadata.MaxFanout,
		}
		request = request.withStableID()
		return FollowUpEvidenceCandidate{Gap: gap, Subject: subject, Request: request}, ""
	}
	if sawBudget {
		return FollowUpEvidenceCandidate{}, FollowUpBudgetExceeded
	}
	if sawDuplicate {
		return FollowUpEvidenceCandidate{}, FollowUpDuplicateRead
	}
	return FollowUpEvidenceCandidate{}, FollowUpNoSafeCapability
}

func followUpProviderOrder(gap EvidenceGap, evidenceType EvidenceType) []string {
	providers := []string{}
	if gap.MissingCapability != "" {
		providers = append(providers, gap.MissingCapability)
	}
	providers = append(providers, evidenceProviderPriority[evidenceType]...)
	seen := map[string]bool{}
	out := make([]string, 0, len(providers))
	for _, provider := range providers {
		if provider == "" || seen[provider] {
			continue
		}
		seen[provider] = true
		out = append(out, provider)
	}
	return out
}

func capabilityProvidesEvidenceType(metadata CapabilityMetadata, evidenceType EvidenceType) bool {
	for _, provided := range metadata.EvidenceTypes {
		if provided == evidenceType {
			return true
		}
	}
	return false
}

func executedEvidenceReadIdentities(plan EvidencePlan, results []EvidenceResult) map[string]bool {
	executed := map[string]bool{}
	for _, request := range plan.Requests {
		executed[evidenceReadIdentity(request.Capability, request.Arguments)] = true
	}
	for _, result := range results {
		executed[evidenceReadIdentity(result.Capability, result.Arguments)] = true
	}
	return executed
}

func evidenceReadIdentity(capability string, arguments map[string]any) string {
	return capability + ":" + canonicalJSON(cloneCanonicalArguments(arguments))
}

func strongerFollowUpRejection(left, right FollowUpDecisionReason) FollowUpDecisionReason {
	rank := map[FollowUpDecisionReason]int{
		FollowUpNoSafeCapability: 1, FollowUpSubjectUnresolved: 2, FollowUpDuplicateRead: 3,
		FollowUpFanoutExceeded: 4, FollowUpBudgetExceeded: 5,
	}
	if rank[right] > rank[left] {
		return right
	}
	return left
}

func resolvedFollowUpRequirements(evidence InvestigationEvidence) map[string]bool {
	items := map[string]bool{}
	for _, item := range evidence.Items {
		items[item.Relevance.RequirementID] = true
	}
	blocked := map[string]bool{}
	for _, missing := range evidence.Missing {
		if missing.Criticality == CriticalityCritical {
			blocked[missing.RequirementID] = true
		}
	}
	resolved := map[string]bool{}
	for requirementID := range items {
		resolved[requirementID] = !blocked[requirementID]
	}
	return resolved
}

func filterResolvedPriorMissing(current []MissingEvidence, prior []MissingEvidence, resolved map[string]bool) []MissingEvidence {
	priorKeys := map[string]bool{}
	for _, missing := range prior {
		if resolved[missing.RequirementID] {
			priorKeys[canonicalJSON(missing)] = true
		}
	}
	out := make([]MissingEvidence, 0, len(current))
	for _, missing := range current {
		if resolved[missing.RequirementID] && priorKeys[canonicalJSON(missing)] {
			continue
		}
		out = append(out, missing)
	}
	return out
}

func combineEvidencePlans(first, second EvidencePlan, resolved map[string]bool) EvidencePlan {
	combined := EvidencePlan{RouteID: first.RouteID, Requests: append([]CapabilityRequest(nil), first.Requests...)}
	for _, request := range second.Requests {
		request.Order = len(combined.Requests)
		combined.Requests = append(combined.Requests, request)
	}
	combined.Missing = filterResolvedPriorMissing(first.Missing, first.Missing, resolved)
	combined.LogicalReads = len(combined.Requests)
	combined.CostUnits = first.CostUnits + second.CostUnits
	return combined
}

func combineEvidenceResults(first, second []EvidenceResult) []EvidenceResult {
	combined := append([]EvidenceResult(nil), first...)
	for _, result := range second {
		result.PlanOrder = len(combined)
		combined = append(combined, result)
	}
	return combined
}
