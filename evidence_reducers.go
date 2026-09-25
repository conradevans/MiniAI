package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	phase2BTimelineLimit           = 20
	phase2BRepositoryFileRunes     = 1200
	currentEndpointFreshnessPolicy = "phase2b.current_endpoint_collection.v1"
)

type evidenceReducer func(EvidenceResult, EvidenceRequirement, string, string) ([]EvidenceItem, []Derivation, error)

func phase2BEvidenceReducers() map[string]evidenceReducer {
	return map[string]evidenceReducer{
		"get_platform_overview":      reducePlatformOverview,
		"list_apps":                  reduceApplicationList,
		"get_app_context":            reduceApplicationContext,
		"read_host_history":          reduceHostHistory,
		"read_temperature_history":   reduceTemperatureHistory,
		"read_application_history":   reduceApplicationHistory,
		"read_service_history":       reduceServiceHistory,
		"read_infrastructure_events": reduceInfrastructureEvents,
		"list_databases":             reduceDatabases,
		"read_database_backups":      reduceDatabaseBackups,
		"read_activity":              reduceActivity,
		"read_recovery":              reduceRecovery,
		"list_repository_directory":  reduceRepositoryDirectory,
		"search_repository":          reduceRepositorySearch,
		"read_repository_file":       reduceRepositoryFile,
		"read_runtime_logs":          reduceLogs,
		"read_deployment_logs":       reduceLogs,
		"read_deployment_history":    reduceDeploymentHistory,
	}
}

// ReduceEvidenceResults converts safe, typed result envelopes into the Phase
// 2A evidence graph. It never sees handler internals or unredacted source data.
func ReduceEvidenceResults(route InvestigationRoute, normalizedQuestion string, requirements []EvidenceRequirement, plan EvidencePlan, results []EvidenceResult, registry capabilityRegistry) (InvestigationEvidence, error) {
	canonicalRequirements := append([]EvidenceRequirement(nil), requirements...)
	requirementByID := map[string]EvidenceRequirement{}
	for index := range canonicalRequirements {
		canonicalRequirements[index] = canonicalRequirements[index].WithStableID(string(route.ID))
		requirementByID[canonicalRequirements[index].ID] = canonicalRequirements[index]
	}
	evidence := InvestigationEvidence{
		SchemaVersion: investigationEvidenceSchemaVersion,
		InvestigationID: stableContractID("investigation", struct {
			Question string
			RouteID  InvestigationRouteID
		}{normalizedQuestion, route.ID}),
		NormalizedQuestion: normalizedQuestion,
		Route:              route,
		Requirements:       canonicalRequirements,
		Sources:            []SourceRef{},
		Items:              []EvidenceItem{},
		Derivations:        []Derivation{},
		Missing:            append([]MissingEvidence(nil), plan.Missing...),
		Conflicts:          []EvidenceConflict{},
	}
	reducers := phase2BEvidenceReducers()
	ordered := append([]EvidenceResult(nil), results...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].PlanOrder != ordered[j].PlanOrder {
			return ordered[i].PlanOrder < ordered[j].PlanOrder
		}
		return ordered[i].RequestID < ordered[j].RequestID
	})
	for _, result := range ordered {
		localSourceID := "source:" + result.RequestID
		requirementIDs := sortedUniqueContractStrings(result.RequirementIDs)
		subject := EvidenceSubject{Kind: SubjectUnknown, ID: "unknown"}
		if len(requirementIDs) > 0 {
			if requirement, ok := requirementByID[requirementIDs[0]]; ok {
				subject = requirement.Subject
			}
		}
		metadata := CapabilityMetadata{}
		if capability, ok := registry.lookup(result.Capability); ok {
			metadata = capability.Metadata
		}
		family := CapabilityFamily("")
		if len(metadata.Families) > 0 {
			family = metadata.Families[0]
		}
		sourceAvailability, sourceTruncation := compoundSourceState(result)
		evidence.Sources = append(evidence.Sources, SourceRef{
			ID: localSourceID, Capability: result.Capability, Family: family,
			Resource: result.SafeSummary, Subject: subject, Arguments: safeArgumentsFromMap(result.Arguments),
			CollectedAt: result.CompletedAt.UTC(), Availability: sourceAvailability,
			SourceTimestamp: evidenceResultSourceTimestamp(result.SafeResult), ErrorCategory: result.ErrorCategory, Truncation: sourceTruncation,
		})

		if result.Availability != AvailabilityAvailable && result.Availability != AvailabilityPartial {
			for _, requirementID := range requirementIDs {
				requirement, ok := requirementByID[requirementID]
				if !ok {
					return InvestigationEvidence{}, fmt.Errorf("result %q has unknown requirement %q", result.RequestID, requirementID)
				}
				evidence.Missing = append(evidence.Missing, MissingEvidence{
					RequirementID: requirementID, Criticality: requirement.Criticality,
					Capability: result.Capability, Availability: result.Availability,
					ErrorCategory: result.ErrorCategory, Reason: unavailableEvidenceReason(result),
				})
			}
			continue
		}
		reducer := reducers[result.Capability]
		if reducer == nil {
			return InvestigationEvidence{}, fmt.Errorf("no evidence reducer registered for %q", result.Capability)
		}
		for _, requirementID := range requirementIDs {
			requirement, ok := requirementByID[requirementID]
			if !ok {
				return InvestigationEvidence{}, fmt.Errorf("result %q has unknown requirement %q", result.RequestID, requirementID)
			}
			items, derivations, err := reducer(result, requirement, localSourceID, normalizedQuestion)
			if err != nil {
				evidence.Missing = append(evidence.Missing, MissingEvidence{
					RequirementID: requirement.ID, Criticality: requirement.Criticality,
					Capability: result.Capability, Availability: AvailabilityUnavailable,
					ErrorCategory: EvidenceErrorMalformed, Reason: "typed result could not be reduced",
				})
				continue
			}
			evidence.Items = append(evidence.Items, items...)
			evidence.Derivations = append(evidence.Derivations, derivations...)
			evidence.Missing = append(evidence.Missing, compoundEvidenceLimitations(result, requirement)...)
			if result.Availability == AvailabilityPartial || result.Truncation.Truncated || evidenceItemsReportTruncation(items) {
				evidence.Missing = append(evidence.Missing, MissingEvidence{
					RequirementID: requirement.ID, Criticality: requirement.Criticality,
					Capability: result.Capability, Availability: AvailabilityPartial,
					Reason: "source reported partial or truncated coverage",
				})
			}
		}
	}
	evidence.Items, evidence.Derivations = appendTemporalAlignment(evidence.Items, evidence.Derivations)
	evidence.Conflicts = detectDeterministicEvidenceConflicts(evidence.Items)
	return NormalizeInvestigationEvidence(evidence)
}

func unavailableEvidenceReason(result EvidenceResult) string {
	if result.DependencyError {
		return "dependency unavailable; dependent arguments were not guessed"
	}
	if result.ErrorCategory != "" {
		return "capability returned " + string(result.ErrorCategory)
	}
	return "capability result unavailable"
}

func compoundSourceState(result EvidenceResult) (Availability, EvidenceTruncation) {
	availability, truncation := result.Availability, result.Truncation
	if availability != AvailabilityAvailable && availability != AvailabilityPartial {
		return availability, truncation
	}
	switch value := result.SafeResult.(type) {
	case reactorLabOverview:
		if !value.System.Available || !value.Recovery.Available || !value.Deployments.Available || !value.Databases.Available || !value.Observability.Available {
			availability = AvailabilityPartial
		}
	case reactorLabAppContextResult:
		if !value.Overview.Available || !value.Overview.System.Available || !value.Overview.Recovery.Available ||
			!value.Overview.Observability.Available || value.DatabaseLookupsTruncated || len(value.DatabaseLookupFailures) > 0 {
			availability = AvailabilityPartial
		}
		if value.DatabaseLookupsTruncated || len(value.DatabaseLookupFailures) > 0 {
			truncation = EvidenceTruncation{
				Truncated: true,
				Returned:  len(value.Databases),
				Total:     value.DatabaseLookupsRequested,
			}
		}
	}
	return availability, truncation
}

func compoundEvidenceLimitations(result EvidenceResult, requirement EvidenceRequirement) []MissingEvidence {
	if result.Availability != AvailabilityAvailable && result.Availability != AvailabilityPartial {
		return nil
	}
	limitation := func(criticality RequirementCriticality, reason string) MissingEvidence {
		return MissingEvidence{
			RequirementID: requirement.ID,
			Criticality:   criticality,
			Capability:    result.Capability,
			Availability:  AvailabilityPartial,
			Reason:        reason,
		}
	}
	limitations := []MissingEvidence{}
	switch value := result.SafeResult.(type) {
	case reactorLabOverview:
		sections := []struct {
			name        string
			available   bool
			criticality RequirementCriticality
		}{
			{"system", value.System.Available, requirement.Criticality},
			{"recovery", value.Recovery.Available, CriticalitySupporting},
			{"deployments", value.Deployments.Available, CriticalitySupporting},
			{"databases", value.Databases.Available, CriticalitySupporting},
			{"observability", value.Observability.Available, CriticalitySupporting},
		}
		for _, section := range sections {
			if !section.available {
				limitations = append(limitations, limitation(section.criticality, section.name+" section unavailable in compound platform overview"))
			}
		}
	case reactorLabAppContextResult:
		if !value.SourceAvailable {
			limitations = append(limitations, limitation(CriticalitySupporting, "deployment source identity unavailable in compound application context"))
		}
		if !value.Overview.Available {
			limitations = append(limitations, limitation(CriticalitySupporting, "platform overview unavailable in compound application context"))
		} else {
			for _, section := range []struct {
				name      string
				available bool
			}{
				{"system", value.Overview.System.Available},
				{"recovery", value.Overview.Recovery.Available},
				{"observability", value.Overview.Observability.Available},
			} {
				if !section.available {
					limitations = append(limitations, limitation(CriticalitySupporting, section.name+" overview facet unavailable in compound application context"))
				}
			}
		}
		failureIDs := make([]string, 0, len(value.DatabaseLookupFailures))
		for _, failure := range value.DatabaseLookupFailures {
			failureIDs = append(failureIDs, failure.DatabaseID)
		}
		failureIDs = sortedUniqueContractStrings(failureIDs)
		if len(failureIDs) > 0 {
			limitations = append(limitations, limitation(CriticalitySupporting, "database lookups failed for: "+strings.Join(failureIDs, ",")))
		}
		if value.DatabaseLookupsTruncated {
			limitations = append(limitations, limitation(CriticalitySupporting, "database lookup coverage truncated by compound application context bound"))
		}
	}
	return limitations
}

func observedItem(result EvidenceResult, requirement EvidenceRequirement, sourceID string, timestamp *time.Time, fields []EvidenceField) EvidenceItem {
	return EvidenceItem{
		ID:   "item:" + result.RequestID + ":" + requirement.ID + ":observed",
		Kind: EvidenceObserved, Type: requirement.Type, Subject: requirement.Subject,
		Timestamp: timestamp, Window: requirement.Window, SourceID: sourceID,
		Freshness: evidenceFreshness(requirement, timestamp, result.CompletedAt),
		Relevance: Relevance{RequirementID: requirement.ID, Criticality: requirement.Criticality},
		Payload:   EvidencePayload{Fields: fields},
	}
}

func derivedItem(result EvidenceResult, requirement EvidenceRequirement, sourceID, suffix, inputID string, timestamp *time.Time, fields []EvidenceField) (EvidenceItem, Derivation) {
	item := EvidenceItem{
		ID:   "item:" + result.RequestID + ":" + requirement.ID + ":derived:" + suffix,
		Kind: EvidenceDerived, Type: requirement.Type, Subject: requirement.Subject,
		Timestamp: timestamp, Window: requirement.Window, SourceID: sourceID,
		Freshness: evidenceFreshness(requirement, timestamp, result.CompletedAt),
		Relevance: Relevance{RequirementID: requirement.ID, Criticality: requirement.Criticality},
		Payload:   EvidencePayload{Fields: fields}, Inputs: []string{inputID},
	}
	derivation := Derivation{
		Operation: "deterministic_reduction", PolicyID: "phase2b.reducer.v1",
		Inputs: []string{inputID}, OutputID: item.ID,
	}
	return item, derivation
}

func evidenceFreshness(requirement EvidenceRequirement, sourceTimestamp *time.Time, collectedAt time.Time) Freshness {
	if isCurrentEvidenceType(requirement.Type) {
		return Freshness{State: FreshnessFresh, Reason: currentEndpointFreshnessPolicy + ": current endpoint collected for this investigation", PolicySet: true}
	}
	if !requirement.Freshness.Defined || requirement.Freshness.MaxAge == nil || sourceTimestamp == nil {
		return Freshness{State: FreshnessUnknown, Reason: "no explicit source freshness SLA", PolicySet: false}
	}
	age := collectedAt.UTC().Sub(sourceTimestamp.UTC())
	state := FreshnessFresh
	if age > *requirement.Freshness.MaxAge {
		state = FreshnessStale
	}
	maximum := *requirement.Freshness.MaxAge
	return Freshness{State: state, Age: &age, MaxAge: &maximum, Reason: requirement.Freshness.Reason, PolicySet: true}
}

func isCurrentEvidenceType(value EvidenceType) bool {
	switch value {
	case EvidenceTypeCurrentPlatformState, EvidenceTypeCurrentDeploymentList, EvidenceTypeCurrentDeployment, EvidenceTypeCurrentApplication, EvidenceTypeCurrentDatabase:
		return true
	default:
		return false
	}
}

func reducePlatformOverview(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[reactorLabOverview](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected overview result")
	}
	fields := []EvidenceField{timestampField("collected_at", value.CollectedAt)}
	fields = append(fields,
		boolFieldValue("system_available", value.System.Available),
		boolFieldValue("recovery_available", value.Recovery.Available),
		boolFieldValue("deployments_available", value.Deployments.Available),
		boolFieldValue("databases_available", value.Databases.Available),
		boolFieldValue("observability_available", value.Observability.Available),
	)
	if value.System.Data != nil {
		system := value.System.Data
		fields = append(fields,
			decimalField("cpu_usage_percent", system.CPU.UsagePercent),
			decimalField("load_1", system.CPU.Load1),
			decimalField("memory_usage_percent", system.Memory.UsagePercent),
			decimalField("disk_usage_percent", system.Disk.UsagePercent),
			decimalField("temperature_celsius", system.Temperature.Celsius),
			decimalField("uptime_seconds", system.UptimeSeconds),
			jsonField("services", compactServices(system.Services)),
		)
	}
	timestamp := value.CollectedAt.UTC()
	item := observedItem(result, requirement, sourceID, &timestamp, fields)
	return []EvidenceItem{item}, nil, nil
}

func reduceApplicationList(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[reactorLabAppListResult](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected app list result")
	}
	apps := append([]reactorLabAppSummary(nil), value.Apps...)
	sort.Slice(apps, func(i, j int) bool { return apps[i].App < apps[j].App })
	if requirement.Type != EvidenceTypeCurrentDeploymentList && requirement.Subject.ID != "" && requirement.Subject.ID != "all" && requirement.Subject.ID != "unresolved" {
		filtered := apps[:0]
		for _, application := range apps {
			if strings.EqualFold(application.App, requirement.Subject.ID) || strings.EqualFold(application.App, requirement.Subject.Name) {
				filtered = append(filtered, application)
			}
		}
		apps = filtered
	}
	items := make([]EvidenceItem, 0, len(apps))
	for index, application := range apps {
		subjectRequirement := requirement
		subjectRequirement.Subject = EvidenceSubject{Kind: SubjectApplication, ID: application.App, Name: application.App}
		fields := deploymentFields(application.App, application.Status, application.Strategy, application.ImageID, application.Source, application.ActivatedAt)
		item := observedItem(result, subjectRequirement, sourceID, application.ActivatedAt, fields)
		item.ID += ":" + strconv.Itoa(index)
		items = append(items, item)
	}
	if len(items) == 0 {
		items = append(items, observedItem(result, requirement, sourceID, nil, []EvidenceField{integerField("matched_applications", 0)}))
	}
	return items, nil, nil
}

func reduceApplicationContext(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[reactorLabAppContextResult](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected app context result")
	}
	fields := deploymentFields(value.App, value.Deployment.Status, value.Deployment.Strategy, value.Deployment.ImageID, value.Deployment.Source, value.Deployment.ActivatedAt)
	failureIDs := make([]string, 0, len(value.DatabaseLookupFailures))
	for _, failure := range value.DatabaseLookupFailures {
		failureIDs = append(failureIDs, failure.DatabaseID)
	}
	failureIDs = sortedUniqueContractStrings(failureIDs)
	fields = append(fields,
		boolFieldValue("source_available", value.SourceAvailable),
		boolFieldValue("overview_available", value.Overview.Available),
		boolFieldValue("overview_system_available", value.Overview.System.Available),
		boolFieldValue("overview_recovery_available", value.Overview.Recovery.Available),
		boolFieldValue("overview_observability_available", value.Overview.Observability.Available),
		integerField("database_count", int64(len(value.Databases))),
		integerField("database_lookups_requested", int64(value.DatabaseLookupsRequested)),
		integerField("database_lookup_failure_count", int64(len(failureIDs))),
		jsonField("database_lookup_failure_ids", failureIDs),
		boolFieldValue("database_lookups_truncated", value.DatabaseLookupsTruncated),
		boolFieldValue("database_coverage_complete", !value.DatabaseLookupsTruncated && len(failureIDs) == 0),
		evidenceStringField("repository_branch", value.LocalRepository.Branch),
		evidenceStringField("repository_commit", value.LocalRepository.Commit),
	)
	if !value.Overview.Available {
		fields = append(fields, evidenceStringField("overview_error_category", string(EvidenceErrorUnavailable)))
	}
	if value.Overview.CollectedAt != nil {
		fields = append(fields, timestampField("overview_collected_at", *value.Overview.CollectedAt))
	}
	item := observedItem(result, requirement, sourceID, value.Deployment.ActivatedAt, fields)
	return []EvidenceItem{item}, nil, nil
}

func deploymentFields(app, status, strategy, image string, source *reactorLabDeploymentSource, activatedAt *time.Time) []EvidenceField {
	fields := []EvidenceField{evidenceStringField("application", app), evidenceStringField("status", status), evidenceStringField("strategy", strategy), evidenceStringField("image_id", image)}
	if source != nil {
		fields = append(fields,
			evidenceStringField("source_provider", source.Provider), evidenceStringField("repository", source.Repository),
			evidenceStringField("branch", source.Branch), evidenceStringField("requested_ref", source.RequestedRef),
			evidenceStringField("commit_sha", source.CommitSHA),
		)
	}
	if activatedAt != nil {
		fields = append(fields, timestampField("activated_at", *activatedAt))
	}
	return fields
}

func reduceHostHistory(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[reactorLabHostHistory](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected host history result")
	}
	points, deduplicatedPoints, equalTimestampPoints := canonicalMetricPoints(value.Points, func(point reactorLabHostPoint) time.Time {
		return point.Timestamp
	})
	observedFields := windowFields(value.Window, len(points), value.TotalPoints)
	observedFields = append(observedFields, jsonField("salient_values", salientHostPoints(points)))
	var latest *time.Time
	if len(points) > 0 {
		timestamp := points[len(points)-1].Timestamp.UTC()
		latest = &timestamp
	}
	observed := observedItem(result, requirement, sourceID, latest, observedFields)
	derivedFields := summarizeHostPoints(points)
	derivedFields = append(derivedFields,
		integerField("coverage_gap_count", int64(timestampGapCount(hostPointTimes(points), value.Window.BucketSeconds))),
		integerField("deduplicated_point_count", int64(deduplicatedPoints)),
		boolFieldValue("equal_timestamp_values_canonically_ordered", equalTimestampPoints),
	)
	derived, derivation := derivedItem(result, requirement, sourceID, "metric_summary", observed.ID, latest, derivedFields)
	return []EvidenceItem{observed, derived}, []Derivation{derivation}, nil
}

func reduceTemperatureHistory(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[reactorLabTemperatureHistory](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected temperature history result")
	}
	points, deduplicatedPoints, equalTimestampPoints := canonicalMetricPoints(value.Points, func(point reactorLabTemperaturePoint) time.Time {
		return point.BucketStart
	})
	observedFields := windowFields(value.Window, len(points), value.TotalPoints)
	observedFields = append(observedFields, jsonField("salient_values", salientTemperaturePoints(points)))
	var latest *time.Time
	if len(points) > 0 {
		timestamp := points[len(points)-1].BucketEnd.UTC()
		latest = &timestamp
	}
	observed := observedItem(result, requirement, sourceID, latest, observedFields)
	derivedFields := summarizeTemperaturePoints(points)
	derivedFields = append(derivedFields,
		integerField("coverage_gap_count", int64(timestampGapCount(temperaturePointTimes(points), value.Window.BucketSeconds))),
		integerField("deduplicated_point_count", int64(deduplicatedPoints)),
		boolFieldValue("equal_timestamp_values_canonically_ordered", equalTimestampPoints),
	)
	derived, derivation := derivedItem(result, requirement, sourceID, "metric_summary", observed.ID, latest, derivedFields)
	return []EvidenceItem{observed, derived}, []Derivation{derivation}, nil
}

func reduceApplicationHistory(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[reactorLabResolvedApplicationHistory](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected application history result")
	}
	points, deduplicatedPoints, equalTimestampPoints := canonicalMetricPoints(value.History.Points, func(point reactorLabApplicationPoint) time.Time {
		return point.Timestamp
	})
	fields := windowFields(value.History.Window, len(points), value.History.TotalPoints)
	fields = append(fields,
		evidenceStringField("application_id", value.Application.ID), evidenceStringField("application_name", value.Application.Name),
		evidenceStringField("latest_status", value.Application.LatestStatus), integerField("restart_count", int64(value.Application.RestartCount)),
		jsonField("salient_values", salientApplicationPoints(points)),
	)
	latest := optionalLatestApplicationTimestamp(points)
	observed := observedItem(result, requirement, sourceID, latest, fields)
	derivedFields := summarizeApplicationPoints(points)
	derivedFields = append(derivedFields,
		integerField("coverage_gap_count", int64(timestampGapCount(applicationPointTimes(points), value.History.Window.BucketSeconds))),
		integerField("deduplicated_point_count", int64(deduplicatedPoints)),
		boolFieldValue("equal_timestamp_values_canonically_ordered", equalTimestampPoints),
	)
	derived, derivation := derivedItem(result, requirement, sourceID, "metric_summary", observed.ID, latest, derivedFields)
	return []EvidenceItem{observed, derived}, []Derivation{derivation}, nil
}

func reduceServiceHistory(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[reactorLabServiceHistory](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected service history result")
	}
	services := append([]reactorLabServiceSeries(nil), value.Services...)
	sort.Slice(services, func(i, j int) bool {
		if services[i].ID != services[j].ID {
			return services[i].ID < services[j].ID
		}
		if services[i].Name != services[j].Name {
			return services[i].Name < services[j].Name
		}
		return canonicalJSON(services[i]) < canonicalJSON(services[j])
	})
	type servicePointFact struct {
		ID          string    `json:"id"`
		Name        string    `json:"name"`
		Timestamp   time.Time `json:"timestamp"`
		SampleCount int       `json:"sampleCount"`
		Available   bool      `json:"available"`
		Status      string    `json:"status"`
	}
	type serviceSummary struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Samples     int    `json:"samples"`
		FirstStatus string `json:"firstStatus,omitempty"`
		LastStatus  string `json:"lastStatus,omitempty"`
		Transitions int    `json:"transitions"`
	}
	facts := []servicePointFact{}
	summaries := make([]serviceSummary, 0, len(services))
	var latest *time.Time
	deduplicatedPointCount := 0
	equalTimestampValues := false
	for _, service := range services {
		points, removed, equalTimestamps := canonicalMetricPoints(service.Points, func(point reactorLabServicePoint) time.Time {
			return point.Timestamp
		})
		deduplicatedPointCount += removed
		equalTimestampValues = equalTimestampValues || equalTimestamps
		summary := serviceSummary{ID: service.ID, Name: service.Name, Samples: len(points)}
		if len(points) > 0 {
			summary.FirstStatus, summary.LastStatus = points[0].Status, points[len(points)-1].Status
			for index, point := range points {
				facts = append(facts, servicePointFact{
					ID: service.ID, Name: service.Name, Timestamp: point.Timestamp.UTC(),
					SampleCount: point.SampleCount, Available: point.Available, Status: point.Status,
				})
				if index > 0 && (point.Available != points[index-1].Available || point.Status != points[index-1].Status) {
					summary.Transitions++
				}
			}
			timestamp := points[len(points)-1].Timestamp.UTC()
			if latest == nil || timestamp.After(*latest) {
				latest = &timestamp
			}
		}
		summaries = append(summaries, summary)
	}
	selectedFacts, omittedFacts := boundedSlice(facts, phase2BTimelineLimit)
	observedFields := []EvidenceField{
		evidenceStringField("requested_range", value.Window.Range),
		timestampField("coverage_from", value.Window.From),
		timestampField("coverage_to", value.Window.To),
		decimalField("bucket_seconds", value.Window.BucketSeconds),
		integerField("service_count", int64(len(services))),
		integerField("returned_service_points", int64(len(facts))),
		jsonField("service_points", selectedFacts),
		integerField("reducer_omitted", int64(omittedFacts)),
	}
	observed := observedItem(result, requirement, sourceID, latest, observedFields)
	derivedFields := []EvidenceField{
		jsonField("service_summaries", summaries),
		integerField("deduplicated_point_count", int64(deduplicatedPointCount)),
		boolFieldValue("equal_timestamp_values_canonically_ordered", equalTimestampValues),
	}
	derived, derivation := derivedItem(result, requirement, sourceID, "service_summary", observed.ID, latest, derivedFields)
	return []EvidenceItem{observed, derived}, []Derivation{derivation}, nil
}

func reduceInfrastructureEvents(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[reactorLabEvents](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected infrastructure events result")
	}
	events := append([]reactorLabEvent(nil), value.Events...)
	sort.Slice(events, func(i, j int) bool {
		if !events[i].OccurredAt.Equal(events[j].OccurredAt) {
			return events[i].OccurredAt.Before(events[j].OccurredAt)
		}
		return events[i].ID < events[j].ID
	})
	selected, omitted := boundedSlice(events, phase2BTimelineLimit)
	fields := windowFields(value.Window, len(events), value.TotalEvents)
	fields = append(fields, jsonField("events", selected), integerField("reducer_omitted", int64(omitted)))
	var timestamp *time.Time
	if len(events) > 0 {
		latest := events[len(events)-1].OccurredAt.UTC()
		timestamp = &latest
	}
	item := observedItem(result, requirement, sourceID, timestamp, fields)
	return []EvidenceItem{item}, nil, nil
}

func reduceDatabases(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[reactorLabDatabaseList](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected database list result")
	}
	databases := append([]reactorLabDatabase(nil), value.Databases...)
	sort.Slice(databases, func(i, j int) bool { return databases[i].ID < databases[j].ID })
	items := make([]EvidenceItem, 0, len(databases))
	for index, database := range databases {
		if requirement.Subject.ID != "" && requirement.Subject.ID != "all" && requirement.Subject.ID != "unresolved" &&
			!strings.EqualFold(requirement.Subject.ID, database.ID) && !strings.EqualFold(requirement.Subject.Name, database.DisplayName) {
			continue
		}
		subjectRequirement := requirement
		subjectRequirement.Subject = EvidenceSubject{Kind: SubjectDatabase, ID: database.ID, Name: database.DisplayName}
		fields := []EvidenceField{
			evidenceStringField("database_id", database.ID), evidenceStringField("database_name", database.DisplayName), evidenceStringField("status", database.Status),
			integerField("size_bytes", database.SizeBytes), integerField("connections", database.Connections),
			integerField("active_connections", database.ActiveConnections), integerField("commits", database.Commits),
			integerField("rollbacks", database.Rollbacks), integerField("backup_count", int64(database.BackupCount)),
		}
		if database.LatestBackupAt != nil {
			fields = append(fields, timestampField("latest_backup_at", *database.LatestBackupAt))
		}
		item := observedItem(result, subjectRequirement, sourceID, &value.CollectedAt, fields)
		item.ID += ":" + strconv.Itoa(index)
		items = append(items, item)
	}
	if len(items) == 0 {
		items = append(items, observedItem(result, requirement, sourceID, &value.CollectedAt, []EvidenceField{integerField("matched_databases", 0)}))
	}
	return items, nil, nil
}

func reduceDatabaseBackups(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[reactorLabBackupList](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected backup list result")
	}
	backups := append([]reactorLabBackup(nil), value.Backups...)
	sort.Slice(backups, func(i, j int) bool {
		if !backups[i].CreatedAt.Equal(backups[j].CreatedAt) {
			return backups[i].CreatedAt.Before(backups[j].CreatedAt)
		}
		return backups[i].ID < backups[j].ID
	})
	backups, outsideWindow := filterBackupsToRequirementWindow(backups, requirement.Window, result.CompletedAt)
	selected, omitted := boundedSlice(backups, phase2BTimelineLimit)
	fields := []EvidenceField{
		evidenceStringField("database_id", value.DatabaseID), integerField("backup_count", int64(len(backups))),
		jsonField("backups", selected), integerField("reducer_omitted", int64(omitted)), integerField("outside_requested_window", int64(outsideWindow)),
	}
	var timestamp *time.Time
	if len(backups) > 0 {
		latest := backups[len(backups)-1].CreatedAt.UTC()
		timestamp = &latest
	}
	item := observedItem(result, requirement, sourceID, timestamp, fields)
	return []EvidenceItem{item}, nil, nil
}

func reduceActivity(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[reactorLabActivity](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected activity result")
	}
	events := append([]reactorLabActivityEvent(nil), value.Events...)
	sort.Slice(events, func(i, j int) bool {
		if !events[i].OccurredAt.Equal(events[j].OccurredAt) {
			return events[i].OccurredAt.Before(events[j].OccurredAt)
		}
		return events[i].EventID < events[j].EventID
	})
	events, outsideWindow := filterActivityToRequirementWindow(events, requirement.Window, result.CompletedAt)
	selected, omitted := boundedSlice(events, phase2BTimelineLimit)
	fields := []EvidenceField{jsonField("events", selected), integerField("event_count", int64(len(events))), integerField("reducer_omitted", int64(omitted)), integerField("outside_requested_window", int64(outsideWindow))}
	var timestamp *time.Time
	if len(events) > 0 {
		latest := events[len(events)-1].OccurredAt.UTC()
		timestamp = &latest
	}
	item := observedItem(result, requirement, sourceID, timestamp, fields)
	return []EvidenceItem{item}, nil, nil
}

func reduceRecovery(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[reactorLabRecovery](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected recovery result")
	}
	incidents := append([]reactorLabRecoveryIncident(nil), value.RecentIncidents...)
	sort.Slice(incidents, func(i, j int) bool {
		if !incidents[i].LastKnownAliveAt.Equal(incidents[j].LastKnownAliveAt) {
			return incidents[i].LastKnownAliveAt.Before(incidents[j].LastKnownAliveAt)
		}
		return incidents[i].EventID < incidents[j].EventID
	})
	incidents, outsideWindow := filterRecoveryToRequirementWindow(incidents, requirement.Window, result.CompletedAt)
	selected, omitted := boundedSlice(incidents, phase2BTimelineLimit)
	fields := []EvidenceField{
		evidenceStringField("protection_state", value.Protection.State), evidenceStringField("watchdog_state", value.Protection.HardwareWatchdog.State),
		evidenceStringField("rtc_state", value.Protection.RTC.State), boolFieldValue("history_available", value.HistoryAvailable),
		jsonField("incidents", selected), integerField("incident_count", int64(len(incidents))), integerField("reducer_omitted", int64(omitted)), integerField("outside_requested_window", int64(outsideWindow)),
	}
	var timestamp *time.Time
	if len(incidents) > 0 {
		latest := incidents[len(incidents)-1].RecoveredAt.UTC()
		timestamp = &latest
	}
	observed := observedItem(result, requirement, sourceID, timestamp, fields)
	items := []EvidenceItem{observed}
	derivations := []Derivation{}
	if len(incidents) > 0 {
		last := incidents[len(incidents)-1]
		derived, derivation := derivedItem(result, requirement, sourceID, "outage_interval", observed.ID, &last.RecoveredAt, []EvidenceField{
			timestampField("outage_start", last.LastKnownAliveAt), timestampField("recovered_at", last.RecoveredAt),
			integerField("downtime_seconds", last.DowntimeSeconds), evidenceStringField("temporal_relation", "recovery followed last-known-alive"),
		})
		derivation.Operation = "outage_interval"
		derivation.Explanation = "temporal ordering only; no causality inferred"
		items = append(items, derived)
		derivations = append(derivations, derivation)
	}
	return items, derivations, nil
}

func reduceRepositoryDirectory(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[repoListResponse](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected repository directory result")
	}
	entries := append([]repoEntry(nil), value.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	selected, omitted := boundedSlice(entries, 50)
	item := observedItem(result, requirement, sourceID, nil, []EvidenceField{
		evidenceStringField("application", value.App), evidenceStringField("path", value.Path), jsonField("entries", selected),
		integerField("reducer_omitted", int64(omitted)), boolFieldValue("source_truncated", value.Truncated),
	})
	return []EvidenceItem{item}, nil, nil
}

func reduceRepositorySearch(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[repoSearchResponse](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected repository search result")
	}
	hits := append([]repoSearchHit(nil), value.Hits...)
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Path != hits[j].Path {
			return hits[i].Path < hits[j].Path
		}
		return hits[i].Line < hits[j].Line
	})
	selected, omitted := boundedSlice(hits, agentSearchMaxHits)
	item := observedItem(result, requirement, sourceID, nil, []EvidenceField{
		evidenceStringField("application", value.App), evidenceStringField("query", value.Query), evidenceStringField("path", value.Path),
		integerField("files_scanned", int64(value.FilesScanned)), jsonField("matches", selected),
		integerField("reducer_omitted", int64(omitted)), boolFieldValue("source_truncated", value.Truncated),
	})
	return []EvidenceItem{item}, nil, nil
}

func reduceRepositoryFile(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[repoFileResponse](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected repository file result")
	}
	content, redacted := scrubSensitiveText(value.Content)
	snippet := truncateRunes(content, phase2BRepositoryFileRunes-1)
	item := observedItem(result, requirement, sourceID, nil, []EvidenceField{
		evidenceStringField("application", value.App), evidenceStringField("path", value.Path), integerField("size_bytes", int64(value.SizeBytes)),
		evidenceStringField("safe_snippet", snippet), boolFieldValue("redacted", value.Redacted || redacted),
		boolFieldValue("snippet_truncated", len([]rune(content)) > len([]rune(snippet))),
	})
	return []EvidenceItem{item}, nil, nil
}

type logWindowCoverage struct {
	FilterApplied     bool
	Complete          bool
	OriginalLines     int
	IncludedLines     int
	UnfilterableLines int
	From              *time.Time
	To                *time.Time
	Latest            *time.Time
}

func reduceLogs(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[logToolResponse](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected log result")
	}
	filtered, coverage := filterLogResponseToRequirementWindow(value, requirement.Window, result.CompletedAt)
	summary := summarizeDiagnosticLogs(filtered, "", result.CompletedAt)
	observedFields := []EvidenceField{
		evidenceStringField("application", value.App),
		evidenceStringField("log_kind", value.Kind),
		integerField("source_lines", int64(coverage.OriginalLines)),
		integerField("included_lines", int64(coverage.IncludedLines)),
		integerField("unfilterable_lines", int64(coverage.UnfilterableLines)),
		boolFieldValue("window_filter_applied", coverage.FilterApplied),
		boolFieldValue("coverage_complete", coverage.Complete),
		boolFieldValue("coverage_incomplete", !coverage.Complete),
		boolFieldValue("source_truncated", value.Truncated),
		boolFieldValue("redacted", summary.Redacted),
	}
	if coverage.From != nil && coverage.To != nil {
		observedFields = append(observedFields,
			timestampField("requested_from", *coverage.From),
			timestampField("requested_to", *coverage.To),
		)
	}
	observed := observedItem(result, requirement, sourceID, coverage.Latest, observedFields)
	derivedFields := []EvidenceField{
		integerField("lines_examined", int64(summary.LinesExamined)),
		integerField("error_count", int64(summary.ErrorCount)),
		integerField("warning_count", int64(summary.WarningCount)),
		integerField("restart_count", int64(summary.RestartCount)),
		evidenceStringField("first_error_timestamp", summary.FirstErrorTimestamp),
		evidenceStringField("last_error_timestamp", summary.LastErrorTimestamp),
		evidenceStringField("last_restart_timestamp", summary.LastRestartTimestamp),
		jsonField("error_patterns", summary.Patterns),
		boolFieldValue("truncated", summary.Truncated),
	}
	derived, derivation := derivedItem(result, requirement, sourceID, "log_summary", observed.ID, coverage.Latest, derivedFields)
	return []EvidenceItem{observed, derived}, []Derivation{derivation}, nil
}

func filterLogResponseToRequirementWindow(value logToolResponse, scope *TemporalScope, collectedAt time.Time) (logToolResponse, logWindowCoverage) {
	lines := strings.Split(strings.TrimSpace(value.Logs), "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		lines = nil
	}
	coverage := logWindowCoverage{
		Complete:      !value.Truncated,
		OriginalLines: len(lines),
		IncludedLines: len(lines),
	}
	from, to, bounded := requirementWindowBounds(scope, collectedAt)
	if !bounded {
		var latest time.Time
		for _, line := range lines {
			if parsed, ok := parseLogTimestamp(line, collectedAt); ok && parsed.After(latest) {
				latest = parsed
			}
		}
		if !latest.IsZero() {
			latest = latest.UTC()
			coverage.Latest = &latest
		}
		return value, coverage
	}
	from, to = from.UTC(), to.UTC()
	coverage.FilterApplied, coverage.From, coverage.To = true, &from, &to
	filtered := make([]string, 0, len(lines))
	var earliest, latest time.Time
	for _, line := range lines {
		parsed, ok := parseLogTimestamp(line, collectedAt)
		if !ok {
			coverage.UnfilterableLines++
			continue
		}
		parsed = parsed.UTC()
		if earliest.IsZero() || parsed.Before(earliest) {
			earliest = parsed
		}
		if timestampWithinRequirementWindow(parsed, from, to) {
			filtered = append(filtered, line)
			if parsed.After(latest) {
				latest = parsed
			}
		}
	}
	coverage.IncludedLines = len(filtered)
	coverage.Complete = coverage.UnfilterableLines == 0 &&
		(!value.Truncated || (!earliest.IsZero() && !earliest.After(from)))
	if !latest.IsZero() {
		latest = latest.UTC()
		coverage.Latest = &latest
	}
	value.Logs = strings.Join(filtered, "\n")
	value.Lines = len(filtered)
	return value, coverage
}

type deploymentVersionFact struct {
	CommitSHA    string     `json:"commitSha,omitempty"`
	Repository   string     `json:"repository,omitempty"`
	Branch       string     `json:"branch,omitempty"`
	RequestedRef string     `json:"requestedRef,omitempty"`
	ActivatedAt  *time.Time `json:"activatedAt,omitempty"`
	ArchivedAt   time.Time  `json:"archivedAt"`
	ImageID      string     `json:"imageId,omitempty"`
}

func reduceDeploymentHistory(result EvidenceResult, requirement EvidenceRequirement, sourceID, _ string) ([]EvidenceItem, []Derivation, error) {
	value, ok := evidenceValue[reactorLabDeploymentHistory](result.SafeResult)
	if !ok {
		return nil, nil, fmt.Errorf("unexpected deployment history result")
	}
	versions := append([]reactorLabDeploymentVersion(nil), value.Versions...)
	sort.Slice(versions, func(i, j int) bool {
		if !versions[i].ArchivedAt.Equal(versions[j].ArchivedAt) {
			return versions[i].ArchivedAt.Before(versions[j].ArchivedAt)
		}
		leftActivated, rightActivated := "", ""
		if versions[i].ActivatedAt != nil {
			leftActivated = versions[i].ActivatedAt.UTC().Format(time.RFC3339Nano)
		}
		if versions[j].ActivatedAt != nil {
			rightActivated = versions[j].ActivatedAt.UTC().Format(time.RFC3339Nano)
		}
		if leftActivated != rightActivated {
			return leftActivated < rightActivated
		}
		return canonicalJSON(versions[i]) < canonicalJSON(versions[j])
	})
	versions, outsideWindow := filterDeploymentsToRequirementWindow(versions, requirement.Window, result.CompletedAt)
	facts := make([]deploymentVersionFact, 0, len(versions))
	for _, version := range versions {
		fact := deploymentVersionFact{ActivatedAt: version.ActivatedAt, ArchivedAt: version.ArchivedAt, ImageID: version.ImageID}
		if fact.ActivatedAt != nil {
			activated := fact.ActivatedAt.UTC()
			fact.ActivatedAt = &activated
		}
		fact.ArchivedAt = fact.ArchivedAt.UTC()
		if version.Source != nil {
			fact.CommitSHA, fact.Repository, fact.Branch, fact.RequestedRef = version.Source.CommitSHA, version.Source.Repository, version.Source.Branch, version.Source.RequestedRef
		}
		facts = append(facts, fact)
	}
	selected, omitted := boundedSlice(facts, phase2BTimelineLimit)
	fields := []EvidenceField{
		evidenceStringField("application", value.App), jsonField("deployment_versions", selected),
		integerField("version_count", int64(len(versions))), integerField("reducer_omitted", int64(omitted)), integerField("outside_requested_window", int64(outsideWindow)),
	}
	var latestActivation *time.Time
	latestActivationFacts := []deploymentVersionFact{}
	var latestArchived *time.Time
	for _, fact := range facts {
		archived := fact.ArchivedAt.UTC()
		if latestArchived == nil || archived.After(*latestArchived) {
			latestArchived = &archived
		}
		if fact.ActivatedAt == nil {
			continue
		}
		activated := fact.ActivatedAt.UTC()
		switch {
		case latestActivation == nil || activated.After(*latestActivation):
			latestActivation = &activated
			latestActivationFacts = []deploymentVersionFact{fact}
		case activated.Equal(*latestActivation):
			latestActivationFacts = append(latestActivationFacts, fact)
		}
	}
	if latestArchived != nil {
		fields = append(fields, timestampField("latest_archived_at", *latestArchived))
	}
	if latestActivation != nil {
		fields = append(fields,
			timestampField("correlation_activated_at", *latestActivation),
			integerField("correlation_candidate_count", int64(len(latestActivationFacts))),
			boolFieldValue("correlation_ambiguous", len(latestActivationFacts) != 1),
		)
		if len(latestActivationFacts) == 1 {
			fields = append(fields,
				evidenceStringField("commit_sha", latestActivationFacts[0].CommitSHA),
				evidenceStringField("image_id", latestActivationFacts[0].ImageID),
			)
		}
	}
	item := observedItem(result, requirement, sourceID, latestActivation, fields)
	return []EvidenceItem{item}, nil, nil
}

func deploymentVersionSHA(version reactorLabDeploymentVersion) string {
	if version.Source == nil {
		return ""
	}
	return version.Source.CommitSHA
}

func appendTemporalAlignment(items []EvidenceItem, derivations []Derivation) ([]EvidenceItem, []Derivation) {
	var deploymentIndex, eventIndex = -1, -1
	for index, item := range items {
		if item.Timestamp == nil || item.Kind != EvidenceObserved {
			continue
		}
		if item.Type == EvidenceTypeDeploymentTimeline && deploymentIndex < 0 {
			deploymentIndex = index
		}
		if (item.Type == EvidenceTypeInfrastructureEvents || item.Type == EvidenceTypeActivityTimeline) && eventIndex < 0 {
			eventIndex = index
		}
	}
	if deploymentIndex < 0 || eventIndex < 0 {
		return items, derivations
	}
	deployment, event := items[deploymentIndex], items[eventIndex]
	fields := []EvidenceField{}
	explanation := "temporal alignment only; no causality inferred"
	candidates := deploymentCandidatesForEvent(deployment, *event.Timestamp)
	if evidenceIntegerValue(deployment, "reducer_omitted") > 0 {
		fields = append(fields,
			evidenceStringField("temporal_relation", "deployment activation coverage is incomplete"),
			timestampField("event_time", *event.Timestamp),
			boolFieldValue("coverage_incomplete", true),
		)
		explanation = "bounded deployment history omitted activation candidates; no causal or active-version choice inferred"
	} else if len(candidates) == 0 {
		return items, derivations
	} else if len(candidates) > 1 {
		fields = append(fields,
			evidenceStringField("temporal_relation", "deployment activation is ambiguous at the event boundary"),
			integerField("deployment_candidate_count", int64(len(candidates))),
			timestampField("event_time", *event.Timestamp),
		)
		explanation = "multiple deployment records share the relevant activation time; no causal or active-version choice inferred"
	} else {
		deploymentTime := candidates[0].ActivatedAt.UTC()
		offset := event.Timestamp.Sub(deploymentTime)
		relation := "deployment and event occurred at the same time"
		if offset > 0 {
			relation = "deployment occurred before event"
		} else if offset < 0 {
			relation = "deployment occurred after event"
		}
		fields = append(fields,
			evidenceStringField("temporal_relation", relation), durationField("event_offset", offset),
			timestampField("deployment_time", deploymentTime), timestampField("event_time", *event.Timestamp),
			evidenceStringField("commit_sha", candidates[0].CommitSHA),
			evidenceStringField("image_id", candidates[0].ImageID),
		)
	}
	derived := EvidenceItem{
		ID:   "item:temporal_alignment:" + deployment.ID + ":" + event.ID,
		Kind: EvidenceDerived, Type: EvidenceTypeDeploymentTimeline, Subject: deployment.Subject,
		Timestamp: event.Timestamp, Window: deployment.Window, SourceID: deployment.SourceID,
		Freshness: deployment.Freshness, Relevance: deployment.Relevance,
		Payload: EvidencePayload{Fields: fields},
		Inputs:  []string{deployment.ID, event.ID},
	}
	derivation := Derivation{
		Operation: "temporal_alignment", PolicyID: "phase2b.temporal_alignment.v1",
		Inputs: []string{deployment.ID, event.ID}, OutputID: derived.ID,
		Explanation: explanation,
	}
	return append(items, derived), append(derivations, derivation)
}

func evidenceIntegerValue(item EvidenceItem, name string) int64 {
	for _, field := range item.Payload.Fields {
		if field.Name == name && field.Value.Integer != nil {
			return *field.Value.Integer
		}
	}
	return 0
}

func deploymentCandidatesForEvent(item EvidenceItem, eventTime time.Time) []deploymentVersionFact {
	encoded := ""
	for _, field := range item.Payload.Fields {
		if field.Name == "deployment_versions" && field.Value.Kind == EvidenceValueString {
			encoded = field.Value.String
			break
		}
	}
	if encoded == "" {
		return nil
	}
	var facts []deploymentVersionFact
	if err := json.Unmarshal([]byte(encoded), &facts); err != nil {
		return nil
	}
	unique := map[string]deploymentVersionFact{}
	for _, fact := range facts {
		if fact.ActivatedAt == nil {
			continue
		}
		activated := fact.ActivatedAt.UTC()
		fact.ActivatedAt = &activated
		unique[canonicalJSON(fact)] = fact
	}
	facts = facts[:0]
	for _, fact := range unique {
		facts = append(facts, fact)
	}
	sort.Slice(facts, func(i, j int) bool {
		if !facts[i].ActivatedAt.Equal(*facts[j].ActivatedAt) {
			return facts[i].ActivatedAt.Before(*facts[j].ActivatedAt)
		}
		return canonicalJSON(facts[i]) < canonicalJSON(facts[j])
	})
	if len(facts) == 0 {
		return nil
	}
	eventTime = eventTime.UTC()
	selectedTime := time.Time{}
	for _, fact := range facts {
		if fact.ActivatedAt.After(eventTime) {
			continue
		}
		if selectedTime.IsZero() || fact.ActivatedAt.After(selectedTime) {
			selectedTime = fact.ActivatedAt.UTC()
		}
	}
	if selectedTime.IsZero() {
		selectedTime = facts[0].ActivatedAt.UTC()
	}
	selected := []deploymentVersionFact{}
	for _, fact := range facts {
		if fact.ActivatedAt.Equal(selectedTime) {
			selected = append(selected, fact)
		}
	}
	return selected
}

func detectDeterministicEvidenceConflicts(items []EvidenceItem) []EvidenceConflict {
	conflicts := []EvidenceConflict{}
	materialFields := []string{"commit_sha", "image_id", "status"}
	for left := 0; left < len(items); left++ {
		if items[left].Kind != EvidenceObserved || !isCurrentEvidenceType(items[left].Type) {
			continue
		}
		for right := left + 1; right < len(items); right++ {
			if items[right].Kind != EvidenceObserved || !isCurrentEvidenceType(items[right].Type) ||
				!semanticEvidenceSubjectsMatch(items[left].Subject, items[right].Subject) ||
				!sameEffectiveEvidenceTime(items[left].Timestamp, items[right].Timestamp) {
				continue
			}
			leftFields, rightFields := evidenceFieldMap(items[left].Payload.Fields), evidenceFieldMap(items[right].Payload.Fields)
			for _, field := range materialFields {
				if !semanticallyComparableEvidenceField(items[left].Type, items[right].Type, field) {
					continue
				}
				leftValue, leftExists := leftFields[field]
				rightValue, rightExists := rightFields[field]
				if !leftExists || !rightExists || canonicalJSON(leftValue) == canonicalJSON(rightValue) {
					continue
				}
				subject := items[left].Subject
				if canonicalJSON(items[right].Subject) < canonicalJSON(subject) {
					subject = items[right].Subject
				}
				conflicts = append(conflicts, EvidenceConflict{
					Subject: subject, Field: field, EvidenceIDs: sortedUniqueContractStrings([]string{items[left].ID, items[right].ID}),
					Material: true, Explanation: "authoritative current observations disagree at the same effective time",
				})
			}
		}
	}
	sort.Slice(conflicts, func(i, j int) bool { return canonicalJSON(conflicts[i]) < canonicalJSON(conflicts[j]) })
	return conflicts
}

func semanticEvidenceSubjectsMatch(left, right EvidenceSubject) bool {
	if left.Kind != right.Kind {
		return false
	}
	if left.ID != "" && right.ID != "" {
		return strings.EqualFold(left.ID, right.ID)
	}
	return left.Name != "" && right.Name != "" && strings.EqualFold(left.Name, right.Name)
}

func semanticallyComparableEvidenceField(left, right EvidenceType, field string) bool {
	if left == right {
		return field == "commit_sha" || field == "image_id" || field == "status"
	}
	applicationCurrent := func(value EvidenceType) bool {
		switch value {
		case EvidenceTypeCurrentApplication, EvidenceTypeCurrentDeployment, EvidenceTypeCurrentDeploymentList:
			return true
		default:
			return false
		}
	}
	return applicationCurrent(left) && applicationCurrent(right) &&
		(field == "commit_sha" || field == "image_id" || field == "status")
}

func sameEffectiveEvidenceTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func evidenceFieldMap(fields []EvidenceField) map[string]EvidenceValue {
	out := make(map[string]EvidenceValue, len(fields))
	for _, field := range fields {
		out[field.Name] = field.Value
	}
	return out
}

func summarizeHostPoints(points []reactorLabHostPoint) []EvidenceField {
	fields := []EvidenceField{integerField("point_count", int64(len(points)))}
	if len(points) == 0 {
		return fields
	}
	cpuValues := make([]float64, 0, len(points))
	memoryValues := make([]float64, 0, len(points))
	peakValue := -math.MaxFloat64
	peakAt := points[0].Timestamp
	for _, point := range points {
		if point.CPUAverage != nil {
			cpuValues = append(cpuValues, *point.CPUAverage)
		}
		memoryValues = append(memoryValues, point.MemoryUsedAverage)
		candidate := -math.MaxFloat64
		if point.CPUMaximum != nil {
			candidate = *point.CPUMaximum
		} else if point.CPUAverage != nil {
			candidate = *point.CPUAverage
		}
		if candidate > peakValue {
			peakValue, peakAt = candidate, point.Timestamp
		}
	}
	fields = append(fields, numericSummaryFields("cpu", cpuValues)...)
	fields = append(fields, numericSummaryFields("memory_used_bytes", memoryValues)...)
	if peakValue != -math.MaxFloat64 {
		fields = append(fields, decimalField("cpu_peak", peakValue), timestampField("cpu_peak_at", peakAt))
	}
	if len(cpuValues) > 1 {
		fields = append(fields, decimalField("cpu_delta", cpuValues[len(cpuValues)-1]-cpuValues[0]))
	}
	return fields
}

func summarizeTemperaturePoints(points []reactorLabTemperaturePoint) []EvidenceField {
	fields := []EvidenceField{integerField("point_count", int64(len(points)))}
	if len(points) == 0 {
		return fields
	}
	minimum, maximum, weightedTotal, samples := points[0].MinCelsius, points[0].MaxCelsius, float64(0), 0
	peakAt := points[0].PeakAt
	for _, point := range points {
		if point.MinCelsius < minimum {
			minimum = point.MinCelsius
		}
		if point.MaxCelsius > maximum || (point.MaxCelsius == maximum && point.PeakAt.Before(peakAt)) {
			maximum, peakAt = point.MaxCelsius, point.PeakAt
		}
		weightedTotal += point.AvgCelsius * float64(point.SampleCount)
		samples += point.SampleCount
	}
	fields = append(fields, decimalField("minimum_celsius", minimum), decimalField("maximum_celsius", maximum), timestampField("peak_at", peakAt), integerField("sample_count", int64(samples)))
	if samples > 0 {
		fields = append(fields, decimalField("average_celsius", weightedTotal/float64(samples)))
	}
	fields = append(fields, decimalField("latest_celsius", points[len(points)-1].AvgCelsius), decimalField("delta_celsius", points[len(points)-1].AvgCelsius-points[0].AvgCelsius))
	return fields
}

func summarizeApplicationPoints(points []reactorLabApplicationPoint) []EvidenceField {
	fields := []EvidenceField{integerField("point_count", int64(len(points)))}
	if len(points) == 0 {
		return fields
	}
	cpu := make([]float64, 0, len(points))
	memory := make([]float64, 0, len(points))
	transitions := 0
	for index, point := range points {
		cpu = append(cpu, point.CPUAverage)
		memory = append(memory, point.MemoryUsedAverage)
		if index > 0 && (point.Status != points[index-1].Status || point.RestartCount != points[index-1].RestartCount) {
			transitions++
		}
	}
	fields = append(fields, numericSummaryFields("cpu", cpu)...)
	fields = append(fields, numericSummaryFields("memory_used_bytes", memory)...)
	fields = append(fields,
		evidenceStringField("first_status", points[0].Status), evidenceStringField("latest_status", points[len(points)-1].Status),
		integerField("first_restart_count", int64(points[0].RestartCount)), integerField("latest_restart_count", int64(points[len(points)-1].RestartCount)),
		integerField("transition_count", int64(transitions)),
	)
	return fields
}

func canonicalMetricPoints[T any](values []T, timestamp func(T) time.Time) ([]T, int, bool) {
	out := append([]T(nil), values...)
	sort.Slice(out, func(i, j int) bool {
		leftTime, rightTime := timestamp(out[i]), timestamp(out[j])
		if !leftTime.Equal(rightTime) {
			return leftTime.Before(rightTime)
		}
		return canonicalJSON(out[i]) < canonicalJSON(out[j])
	})
	deduplicated := make([]T, 0, len(out))
	removed := 0
	equalTimestampValues := false
	previousCanonical := ""
	var previousTime time.Time
	for index, value := range out {
		canonical := canonicalJSON(value)
		currentTime := timestamp(value)
		if index > 0 && currentTime.Equal(previousTime) {
			equalTimestampValues = true
		}
		if len(deduplicated) > 0 && canonical == previousCanonical {
			removed++
			previousTime = currentTime
			continue
		}
		deduplicated = append(deduplicated, value)
		previousCanonical = canonical
		previousTime = currentTime
	}
	return deduplicated, removed, equalTimestampValues
}

func numericSummaryFields(prefix string, values []float64) []EvidenceField {
	if len(values) == 0 {
		return nil
	}
	minimum, maximum, total := values[0], values[0], float64(0)
	for _, value := range values {
		minimum = math.Min(minimum, value)
		maximum = math.Max(maximum, value)
		total += value
	}
	return []EvidenceField{
		decimalField(prefix+"_first", values[0]), decimalField(prefix+"_latest", values[len(values)-1]),
		decimalField(prefix+"_minimum", minimum), decimalField(prefix+"_maximum", maximum), decimalField(prefix+"_average", total/float64(len(values))),
	}
}

func salientHostPoints(points []reactorLabHostPoint) any {
	type salient struct {
		Timestamp  time.Time `json:"timestamp"`
		CPUAverage *float64  `json:"cpuAverage,omitempty"`
		CPUMaximum *float64  `json:"cpuMaximum,omitempty"`
		MemoryUsed float64   `json:"memoryUsedAverage"`
	}
	indices := salientIndices(len(points), hostPeakIndex(points), hostTroughIndex(points))
	out := make([]salient, 0, len(indices))
	for _, index := range indices {
		out = append(out, salient{points[index].Timestamp, points[index].CPUAverage, points[index].CPUMaximum, points[index].MemoryUsedAverage})
	}
	return out
}

func salientTemperaturePoints(points []reactorLabTemperaturePoint) any {
	type salient struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
		Min  float64   `json:"minCelsius"`
		Avg  float64   `json:"avgCelsius"`
		Max  float64   `json:"maxCelsius"`
		Peak time.Time `json:"peakAt"`
	}
	peak, trough := 0, 0
	for index := range points {
		if points[index].MaxCelsius > points[peak].MaxCelsius {
			peak = index
		}
		if points[index].MinCelsius < points[trough].MinCelsius {
			trough = index
		}
	}
	indices := salientIndices(len(points), peak, trough)
	out := make([]salient, 0, len(indices))
	for _, index := range indices {
		point := points[index]
		out = append(out, salient{point.BucketStart, point.BucketEnd, point.MinCelsius, point.AvgCelsius, point.MaxCelsius, point.PeakAt})
	}
	return out
}

func salientApplicationPoints(points []reactorLabApplicationPoint) any {
	type salient struct {
		Timestamp time.Time `json:"timestamp"`
		CPU       float64   `json:"cpuAverage"`
		Memory    float64   `json:"memoryUsedAverage"`
		Status    string    `json:"status"`
		Restarts  int       `json:"restartCount"`
	}
	peak, trough := 0, 0
	transition := -1
	for index := range points {
		if points[index].CPUMaximum > points[peak].CPUMaximum {
			peak = index
		}
		if points[index].CPUAverage < points[trough].CPUAverage {
			trough = index
		}
		if index > 0 && transition < 0 && (points[index].Status != points[index-1].Status || points[index].RestartCount != points[index-1].RestartCount) {
			transition = index
		}
	}
	indices := salientIndices(len(points), peak, trough, transition)
	out := make([]salient, 0, len(indices))
	for _, index := range indices {
		point := points[index]
		out = append(out, salient{point.Timestamp, point.CPUAverage, point.MemoryUsedAverage, point.Status, point.RestartCount})
	}
	return out
}

func salientIndices(length int, candidates ...int) []int {
	if length == 0 {
		return nil
	}
	candidates = append([]int{0}, candidates...)
	candidates = append(candidates, length-1)
	seen := map[int]bool{}
	indices := []int{}
	for _, index := range candidates {
		if index < 0 || index >= length || seen[index] {
			continue
		}
		seen[index] = true
		indices = append(indices, index)
	}
	sort.Ints(indices)
	if len(indices) > 5 {
		indices = indices[:5]
	}
	return indices
}

func hostPeakIndex(points []reactorLabHostPoint) int {
	best, maximum := -1, -math.MaxFloat64
	for index, point := range points {
		if point.CPUMaximum != nil && *point.CPUMaximum > maximum {
			best, maximum = index, *point.CPUMaximum
		}
	}
	return best
}

func hostTroughIndex(points []reactorLabHostPoint) int {
	best, minimum := -1, math.MaxFloat64
	for index, point := range points {
		if point.CPUAverage != nil && *point.CPUAverage < minimum {
			best, minimum = index, *point.CPUAverage
		}
	}
	return best
}

func optionalLatestApplicationTimestamp(points []reactorLabApplicationPoint) *time.Time {
	if len(points) == 0 {
		return nil
	}
	value := points[len(points)-1].Timestamp.UTC()
	return &value
}

func compactServices(services []reactorLabServiceState) []reactorLabServiceState {
	out := append([]reactorLabServiceState(nil), services...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > phase2BTimelineLimit {
		out = out[:phase2BTimelineLimit]
	}
	return out
}

func windowFields(window reactorLabWindow, returned, total int) []EvidenceField {
	return []EvidenceField{
		evidenceStringField("requested_range", window.Range), timestampField("coverage_from", window.From), timestampField("coverage_to", window.To),
		decimalField("bucket_seconds", window.BucketSeconds), integerField("returned_points", int64(returned)), integerField("total_points", int64(total)),
	}
}

func boundedSlice[T any](values []T, limit int) ([]T, int) {
	if len(values) <= limit {
		return values, 0
	}
	return values[len(values)-limit:], len(values) - limit
}

func evidenceItemsReportTruncation(items []EvidenceItem) bool {
	for _, item := range items {
		for _, field := range item.Payload.Fields {
			switch field.Name {
			case "snippet_truncated", "source_truncated", "coverage_incomplete":
				if field.Value.Boolean != nil && *field.Value.Boolean {
					return true
				}
			case "reducer_omitted":
				if field.Value.Integer != nil && *field.Value.Integer > 0 {
					return true
				}
			}
		}
	}
	return false
}

func evidenceResultSourceTimestamp(value any) *time.Time {
	var timestamp time.Time
	switch result := value.(type) {
	case reactorLabOverview:
		timestamp = result.CollectedAt
	case reactorLabDatabaseList:
		timestamp = result.CollectedAt
	case reactorLabHostHistory:
		for _, point := range result.Points {
			if point.Timestamp.After(timestamp) {
				timestamp = point.Timestamp
			}
		}
	case reactorLabTemperatureHistory:
		for _, point := range result.Points {
			if point.BucketEnd.After(timestamp) {
				timestamp = point.BucketEnd
			}
		}
	case reactorLabResolvedApplicationHistory:
		for _, point := range result.History.Points {
			if point.Timestamp.After(timestamp) {
				timestamp = point.Timestamp
			}
		}
	case reactorLabEvents:
		for _, event := range result.Events {
			if event.OccurredAt.After(timestamp) {
				timestamp = event.OccurredAt
			}
		}
	case reactorLabActivity:
		for _, event := range result.Events {
			if event.OccurredAt.After(timestamp) {
				timestamp = event.OccurredAt
			}
		}
	case reactorLabRecovery:
		for _, incident := range result.RecentIncidents {
			if incident.RecoveredAt.After(timestamp) {
				timestamp = incident.RecoveredAt
			}
		}
	case reactorLabBackupList:
		for _, backup := range result.Backups {
			if backup.CreatedAt.After(timestamp) {
				timestamp = backup.CreatedAt
			}
		}
	case reactorLabDeploymentHistory:
		for _, version := range result.Versions {
			candidate := version.ArchivedAt
			if version.ActivatedAt != nil {
				candidate = *version.ActivatedAt
			}
			if candidate.After(timestamp) {
				timestamp = candidate
			}
		}
	}
	if timestamp.IsZero() {
		return nil
	}
	timestamp = timestamp.UTC()
	return &timestamp
}

func timestampGapCount(timestamps []time.Time, bucketSeconds float64) int {
	if len(timestamps) < 2 || bucketSeconds <= 0 {
		return 0
	}
	sort.Slice(timestamps, func(i, j int) bool { return timestamps[i].Before(timestamps[j]) })
	threshold := time.Duration(bucketSeconds * 1.5 * float64(time.Second))
	gaps := 0
	for index := 1; index < len(timestamps); index++ {
		if timestamps[index].Sub(timestamps[index-1]) > threshold {
			gaps++
		}
	}
	return gaps
}

func hostPointTimes(points []reactorLabHostPoint) []time.Time {
	out := make([]time.Time, 0, len(points))
	for _, point := range points {
		out = append(out, point.Timestamp)
	}
	return out
}

func temperaturePointTimes(points []reactorLabTemperaturePoint) []time.Time {
	out := make([]time.Time, 0, len(points))
	for _, point := range points {
		out = append(out, point.BucketStart)
	}
	return out
}

func applicationPointTimes(points []reactorLabApplicationPoint) []time.Time {
	out := make([]time.Time, 0, len(points))
	for _, point := range points {
		out = append(out, point.Timestamp)
	}
	return out
}

func requirementWindowBounds(scope *TemporalScope, collectedAt time.Time) (time.Time, time.Time, bool) {
	if scope == nil || !scope.Valid {
		return time.Time{}, time.Time{}, false
	}
	switch scope.Kind {
	case TemporalNamedWindow:
		durations := map[string]time.Duration{"15m": 15 * time.Minute, "1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}
		duration, ok := durations[scope.NamedRange]
		if !ok || collectedAt.IsZero() {
			return time.Time{}, time.Time{}, false
		}
		to := collectedAt.UTC()
		return to.Add(-duration), to, true
	case TemporalToday, TemporalYesterday, TemporalExplicitWindow, TemporalIncidentCentered:
		if scope.From == nil || scope.To == nil {
			return time.Time{}, time.Time{}, false
		}
		return scope.From.UTC(), scope.To.UTC(), true
	default:
		return time.Time{}, time.Time{}, false
	}
}

func timestampWithinRequirementWindow(timestamp time.Time, from, to time.Time) bool {
	value := timestamp.UTC()
	return !value.Before(from) && !value.After(to)
}

func filterBackupsToRequirementWindow(values []reactorLabBackup, scope *TemporalScope, collectedAt time.Time) ([]reactorLabBackup, int) {
	from, to, bounded := requirementWindowBounds(scope, collectedAt)
	if !bounded {
		return values, 0
	}
	out := make([]reactorLabBackup, 0, len(values))
	for _, value := range values {
		if timestampWithinRequirementWindow(value.CreatedAt, from, to) {
			out = append(out, value)
		}
	}
	return out, len(values) - len(out)
}

func filterActivityToRequirementWindow(values []reactorLabActivityEvent, scope *TemporalScope, collectedAt time.Time) ([]reactorLabActivityEvent, int) {
	from, to, bounded := requirementWindowBounds(scope, collectedAt)
	if !bounded {
		return values, 0
	}
	out := make([]reactorLabActivityEvent, 0, len(values))
	for _, value := range values {
		if timestampWithinRequirementWindow(value.OccurredAt, from, to) {
			out = append(out, value)
		}
	}
	return out, len(values) - len(out)
}

func filterRecoveryToRequirementWindow(values []reactorLabRecoveryIncident, scope *TemporalScope, collectedAt time.Time) ([]reactorLabRecoveryIncident, int) {
	from, to, bounded := requirementWindowBounds(scope, collectedAt)
	if !bounded {
		return values, 0
	}
	out := make([]reactorLabRecoveryIncident, 0, len(values))
	for _, value := range values {
		if !value.RecoveredAt.Before(from) && !value.LastKnownAliveAt.After(to) {
			out = append(out, value)
		}
	}
	return out, len(values) - len(out)
}

func filterDeploymentsToRequirementWindow(values []reactorLabDeploymentVersion, scope *TemporalScope, collectedAt time.Time) ([]reactorLabDeploymentVersion, int) {
	from, to, bounded := requirementWindowBounds(scope, collectedAt)
	if !bounded {
		return values, 0
	}
	out := make([]reactorLabDeploymentVersion, 0, len(values))
	for _, value := range values {
		timestamp := value.ArchivedAt
		if value.ActivatedAt != nil {
			timestamp = *value.ActivatedAt
		}
		if timestampWithinRequirementWindow(timestamp, from, to) {
			out = append(out, value)
		}
	}
	return out, len(values) - len(out)
}

func evidenceValue[T any](value any) (T, bool) {
	if typed, ok := value.(T); ok {
		return typed, true
	}
	if pointer, ok := value.(*T); ok && pointer != nil {
		return *pointer, true
	}
	var zero T
	return zero, false
}

func safeArgumentsFromMap(arguments map[string]any) []SafeArgument {
	keys := make([]string, 0, len(arguments))
	for key := range arguments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]SafeArgument, 0, len(keys))
	for _, key := range keys {
		out = append(out, SafeArgument{Name: key, Value: scalarEvidenceValue(arguments[key])})
	}
	return out
}

func scalarEvidenceValue(value any) EvidenceValue {
	switch typed := value.(type) {
	case string:
		return EvidenceValue{Kind: EvidenceValueString, String: typed}
	case int:
		converted := int64(typed)
		return EvidenceValue{Kind: EvidenceValueInteger, Integer: &converted}
	case int64:
		return EvidenceValue{Kind: EvidenceValueInteger, Integer: &typed}
	case uint64:
		return EvidenceValue{Kind: EvidenceValueUnsigned, Unsigned: &typed}
	case float64:
		return EvidenceValue{Kind: EvidenceValueDecimal, Decimal: strconv.FormatFloat(typed, 'g', -1, 64)}
	case bool:
		return EvidenceValue{Kind: EvidenceValueBoolean, Boolean: &typed}
	case time.Time:
		value := typed.UTC()
		return EvidenceValue{Kind: EvidenceValueTimestamp, Timestamp: &value}
	default:
		return EvidenceValue{Kind: EvidenceValueString, String: canonicalJSON(value)}
	}
}

func evidenceStringField(name, value string) EvidenceField {
	return EvidenceField{Name: name, Value: EvidenceValue{Kind: EvidenceValueString, String: value}}
}

func integerField(name string, value int64) EvidenceField {
	return EvidenceField{Name: name, Value: EvidenceValue{Kind: EvidenceValueInteger, Integer: &value}}
}

func decimalField(name string, value float64) EvidenceField {
	return EvidenceField{Name: name, Value: EvidenceValue{Kind: EvidenceValueDecimal, Decimal: strconv.FormatFloat(value, 'g', -1, 64)}}
}

func boolFieldValue(name string, value bool) EvidenceField {
	return EvidenceField{Name: name, Value: EvidenceValue{Kind: EvidenceValueBoolean, Boolean: &value}}
}

func timestampField(name string, value time.Time) EvidenceField {
	value = value.UTC()
	return EvidenceField{Name: name, Value: EvidenceValue{Kind: EvidenceValueTimestamp, Timestamp: &value}}
}

func durationField(name string, value time.Duration) EvidenceField {
	return EvidenceField{Name: name, Value: EvidenceValue{Kind: EvidenceValueDuration, Duration: &value}}
}

func jsonField(name string, value any) EvidenceField {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return evidenceStringField(name, string(encoded))
}
