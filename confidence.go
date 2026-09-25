package main

import "sort"

// DecideEvidenceConfidence is entirely software-owned. It evaluates evidence
// coverage and returns a ceiling for the future Phase 2C conclusion; no model
// input is accepted and a later model may only lower this value.
func DecideEvidenceConfidence(evidence InvestigationEvidence) ConfidenceDecision {
	itemsByRequirement := map[string][]EvidenceItem{}
	for _, item := range evidence.Items {
		itemsByRequirement[item.Relevance.RequirementID] = append(itemsByRequirement[item.Relevance.RequirementID], item)
	}
	missingByRequirement := map[string][]MissingEvidence{}
	for _, missing := range evidence.Missing {
		missingByRequirement[missing.RequirementID] = append(missingByRequirement[missing.RequirementID], missing)
	}

	inputs := ConfidenceInputs{
		CriticalCompleteness: CompletenessComplete,
		Freshness:            FreshnessFresh,
		Directness:           DirectnessDirect,
		Agreement:            AgreementConsistent,
		Coverage:             CoverageComplete,
		SubjectAmbiguity:     evidence.Route.Frame.Ambiguity.Ambiguous,
	}
	reasons := []string{}
	criticalMissing := false
	noncriticalMissing := false
	criticalFreshnessFailure := false
	criticalFreshnessUnknown := false
	criticalOnlyDerived := false
	criticalOnlyInferred := false
	criticalTruncation := false

	for _, requirement := range evidence.Requirements {
		items := itemsByRequirement[requirement.ID]
		missing := missingByRequirement[requirement.ID]
		available := len(items) > 0
		criticalUnavailable := false
		criticalPartial := false
		noncriticalLimitation := false
		for _, limitation := range missing {
			criticality := limitation.Criticality
			if criticality == "" {
				criticality = requirement.Criticality
			}
			if criticality != CriticalityCritical {
				noncriticalLimitation = true
				continue
			}
			if limitation.Availability == AvailabilityPartial {
				criticalPartial = true
			} else {
				criticalUnavailable = true
			}
		}
		if requirement.Criticality == CriticalityCritical {
			if !available || criticalUnavailable {
				criticalMissing = true
				inputs.CriticalCompleteness = CompletenessMissing
			}
			if criticalPartial {
				criticalTruncation = true
				if inputs.CriticalCompleteness == CompletenessComplete {
					inputs.CriticalCompleteness = CompletenessPartial
				}
			}
			if noncriticalLimitation {
				noncriticalMissing = true
			}
			hasObserved, hasDerived, hasInferred := false, false, false
			for _, item := range items {
				switch item.Kind {
				case EvidenceObserved:
					hasObserved = true
				case EvidenceDerived:
					hasDerived = true
				case EvidenceInferred:
					hasInferred = true
				}
			}
			criticalOnlyDerived = criticalOnlyDerived || (!hasObserved && hasDerived)
			criticalOnlyInferred = criticalOnlyInferred || (!hasObserved && hasInferred)

			if freshnessMatters(requirement) {
				state := criticalRequirementFreshness(items)
				switch state {
				case FreshnessStale:
					criticalFreshnessFailure = true
					inputs.Freshness = FreshnessStale
				case FreshnessUnknown:
					criticalFreshnessUnknown = true
					if inputs.Freshness != FreshnessStale {
						inputs.Freshness = FreshnessUnknown
					}
				}
			} else if criticalRequirementFreshness(items) == FreshnessUnknown && inputs.Freshness == FreshnessFresh {
				inputs.Freshness = FreshnessUnknown
			}
		} else if len(missing) > 0 || !available {
			noncriticalMissing = true
		}
	}

	if criticalOnlyInferred {
		inputs.Directness = DirectnessIndirect
	} else if criticalOnlyDerived {
		inputs.Directness = DirectnessMixed
	}
	materialConflict := false
	if len(evidence.Conflicts) > 0 {
		inputs.Agreement = AgreementMixed
	}
	for _, conflict := range evidence.Conflicts {
		if conflict.Material {
			materialConflict = true
			inputs.Agreement = AgreementConflicted
		}
	}
	inputs.RelevantTruncation = criticalTruncation
	for _, missing := range evidence.Missing {
		if missing.Availability == AvailabilityPartial && missing.Criticality != CriticalitySupporting {
			inputs.RelevantTruncation = true
		}
	}
	if criticalMissing {
		inputs.Coverage = CoverageNarrow
	} else if noncriticalMissing || inputs.RelevantTruncation {
		inputs.Coverage = CoveragePartial
	}

	level := ConfidenceHigh
	switch {
	case criticalMissing:
		level = ConfidenceLow
		reasons = append(reasons, "critical evidence is missing")
	case materialConflict:
		level = ConfidenceLow
		reasons = append(reasons, "material authoritative evidence conflicts")
	case criticalFreshnessFailure:
		level = ConfidenceLow
		reasons = append(reasons, "critical evidence is stale under an explicit policy")
	case criticalFreshnessUnknown:
		level = ConfidenceLow
		reasons = append(reasons, "critical freshness is unknown where freshness is required")
	case inputs.SubjectAmbiguity:
		level = ConfidenceLow
		reasons = append(reasons, "subject identity is ambiguous")
	case criticalTruncation:
		level = ConfidenceLow
		reasons = append(reasons, "critical evidence coverage is truncated")
	case inputs.Directness == DirectnessIndirect:
		level = ConfidenceLow
		reasons = append(reasons, "critical evidence is indirect")
	case noncriticalMissing || inputs.RelevantTruncation:
		level = ConfidenceMedium
		reasons = append(reasons, "noncritical evidence is missing or bounded")
	case inputs.Directness == DirectnessMixed:
		level = ConfidenceMedium
		reasons = append(reasons, "the evidence ceiling requires deterministic interpretation of aligned signals")
	default:
		reasons = append(reasons, "critical evidence is complete and consistent")
	}
	if len(evidence.Conflicts) > 0 && !materialConflict {
		if level == ConfidenceHigh {
			level = ConfidenceMedium
		}
		reasons = append(reasons, "nonmaterial evidence disagreement remains explicit")
	}
	sort.Strings(reasons)
	return ConfidenceDecision{Level: level, SoftwareCeiling: level, Determined: true, Inputs: inputs, Reasons: reasons}
}

func ApplyEvidenceConfidence(evidence InvestigationEvidence) InvestigationEvidence {
	evidence.Confidence = DecideEvidenceConfidence(evidence)
	return evidence
}

func freshnessMatters(requirement EvidenceRequirement) bool {
	if requirement.Freshness.Defined {
		return true
	}
	return requirement.Window != nil && requirement.Window.Kind == TemporalNow
}

func criticalRequirementFreshness(items []EvidenceItem) FreshnessState {
	if len(items) == 0 {
		return FreshnessUnknown
	}
	state := FreshnessFresh
	for _, item := range items {
		if item.Freshness.State == FreshnessStale {
			return FreshnessStale
		}
		if item.Freshness.State == FreshnessUnknown {
			state = FreshnessUnknown
		}
	}
	return state
}
