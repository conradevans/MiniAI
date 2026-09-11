package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const changeComparisonMaxFacts = 8

func changeQuestionNeedsTimingOrFailureEvidence(message string) bool {
	m := strings.ToLower(message)
	if hasCurrentFailurePremise(m) {
		return true
	}
	return containsAny(
		m,
		"failure", "failed", "failing", "break", "broke", "broken",
		"working", "work before", "happen", "happened", "when did",
		"error", "crash", "restart", "unhealthy", "health check", "health-check",
		"regression", "caused", "cause", "started", "start failing",
		"investigate", "diagnose", "issue", "problem",
	)
}

func isDeploymentCausalityQuestion(message string) bool {
	if !isChangeAwareDiagnosticQuestion(message) {
		return false
	}
	m := strings.ToLower(message)
	return messageContainsTerm(m, "cause") ||
		messageContainsTerm(m, "caused") ||
		messageContainsTerm(m, "causing") ||
		containsAny(m, "responsible", "responsibility", "rule out", "ruled out", "blame")
}

func changeQuestionNeedsDeploymentLogs(message string) bool {
	return isChangeAwareDiagnosticQuestion(message) &&
		changeQuestionNeedsTimingOrFailureEvidence(message)
}

func diagnosticNeedsRuntimeEvidence(packet diagnosticEvidencePacket, message string) bool {
	if !isChangeAwareDiagnosticQuestion(message) {
		return true
	}
	if isDiagnosticUnhealthyState(packet.Application.Health) ||
		hasFailingService(packet.Application) {
		return true
	}
	if packet.Application.Health != "" &&
		!strings.EqualFold(packet.Application.Health, "healthy") {
		return true
	}
	return changeQuestionNeedsTimingOrFailureEvidence(message)
}

func addDeploymentHistoryAssessment(assessment *diagnosticEvidenceAssessment, packet diagnosticEvidencePacket, message string) {
	if !isChangeAwareDiagnosticQuestion(message) || packet.DeploymentHistory == nil {
		return
	}
	history := packet.DeploymentHistory
	if len(history.Versions) == 0 {
		assessment.Unknowns = append(
			assessment.Unknowns,
			"No previous deployment history is available for comparison.",
		)
		return
	}

	previous := history.Versions[0]
	temporalFact, temporalCorrelation := deploymentTemporalFact(packet)
	if temporalFact != "" {
		assessment.KeyEvidence = append(assessment.KeyEvidence, temporalFact)
	}
	facts := deploymentHistoryComparisonFacts(packet.Application, previous)
	assessment.KeyEvidence = append(assessment.KeyEvidence, facts...)
	if previous.ArchivedAt != "" {
		assessment.KeyEvidence = append(
			assessment.KeyEvidence,
			fmt.Sprintf("The immediately previous version entered MiniDeploy history at %s; this is an archive/cutover time, not its original deployment time.", previous.ArchivedAt),
		)
	}

	if changeQuestionNeedsTimingOrFailureEvidence(message) && temporalFact == "" {
		assessment.Unknowns = append(
			assessment.Unknowns,
			"Bounded timestamped evidence does not establish whether the observed symptom began before or after the latest cutover.",
		)
	}
	if currentDiagnosticHealthy(packet.Application) && isDeploymentCausalityQuestion(message) {
		assessment.Unknowns = append(
			assessment.Unknowns,
			"The application is currently healthy. That current state does not establish whether the latest deployment caused or did not cause an earlier issue.",
		)
	}
	assessment.Unknowns = append(
		assessment.Unknowns,
		"Deployment history does not provide an exact Git commit or an exact source-code diff between the active and previous versions.",
		"Previous runtime health is not persisted in deployment history.",
	)
	assessment.Unknowns = append(
		assessment.Unknowns,
		deploymentHistoryComparisonUnknowns(packet.Application, previous)...,
	)
	if strings.EqualFold(packet.Application.SourceStatus["repository"], "not-found") {
		assessment.Unknowns = append(
			assessment.Unknowns,
			"Current repository evidence is unavailable, so source implementation details cannot supplement the deployment comparison.",
		)
	}

	if temporalCorrelation &&
		(currentDiagnosticFailure(packet.Application) ||
			diagnosticLogHasFailure(packet.RuntimeLogs)) {
		assessment.PossibleCauses = append(
			assessment.PossibleCauses,
			"A deployment-related regression remains possible because a bounded error follows the cutover/archive time, but timing alone does not establish causation.",
		)
	}
}

func deploymentTemporalFact(packet diagnosticEvidencePacket) (string, bool) {
	if packet.DeploymentHistory == nil ||
		len(packet.DeploymentHistory.Versions) == 0 {
		return "", false
	}
	archivedText := packet.DeploymentHistory.Versions[0].ArchivedAt
	archivedAt, err := time.Parse(time.RFC3339Nano, archivedText)
	if err != nil {
		return "", false
	}

	type timestampSource struct {
		label   string
		summary *diagnosticLogSummary
	}
	for _, source := range []timestampSource{
		{label: "runtime", summary: packet.RuntimeLogs},
		{label: "deployment", summary: packet.DeploymentLogs},
	} {
		if source.summary == nil || source.summary.FirstErrorTimestamp == "" {
			continue
		}
		errorAt, parseErr := time.Parse(
			time.RFC3339Nano,
			source.summary.FirstErrorTimestamp,
		)
		if parseErr != nil {
			continue
		}
		if !errorAt.Before(archivedAt) {
			return fmt.Sprintf(
				"The first error observed in the bounded %s logs at %s follows the cutover/archive time %s; this is temporal correlation only and does not establish causation.",
				source.label,
				source.summary.FirstErrorTimestamp,
				archivedText,
			), true
		}
		return fmt.Sprintf(
			"The first error observed in the bounded %s logs at %s precedes the cutover/archive time %s, so it does not support attributing the error to that transition.",
			source.label,
			source.summary.FirstErrorTimestamp,
			archivedText,
		), false
	}
	return "", false
}

func deploymentHistoryComparisonFacts(current appDiagnosticSnapshot, previous deploymentHistoryVersion) []string {
	facts := []string{}
	if current.DeploymentImage != "" && previous.Image != "" {
		if current.DeploymentImage == previous.Image {
			if !current.DeploymentIdentifiersTruncated && !previous.IdentifiersTruncated {
				facts = append(facts, fmt.Sprintf("The current and immediately previous deployment images both record %q.", current.DeploymentImage))
			}
		} else {
			facts = append(facts, fmt.Sprintf("The deployment image changed from %q to %q.", previous.Image, current.DeploymentImage))
		}
	}
	facts = append(facts, namedServiceComparisonFacts(current, previous)...)
	if current.Strategy != "" && previous.Strategy != "" {
		if strings.EqualFold(current.Strategy, previous.Strategy) {
			if !current.DeploymentIdentifiersTruncated && !previous.IdentifiersTruncated {
				facts = append(facts, fmt.Sprintf("The deployment strategy is unchanged at %q.", current.Strategy))
			}
		} else {
			facts = append(facts, fmt.Sprintf("The deployment strategy changed from %q to %q.", previous.Strategy, current.Strategy))
		}
	}
	if current.ListenerPort != 0 && previous.Port != 0 {
		if current.ListenerPort == previous.Port {
			facts = append(facts, fmt.Sprintf("The active and previous host ports both record %d.", current.ListenerPort))
		} else {
			facts = append(facts, fmt.Sprintf("The active host port %d differs from the previous recorded port %d.", current.ListenerPort, previous.Port))
		}
	}
	if current.ContainerPort != 0 && previous.ContainerPort != 0 {
		if current.ContainerPort == previous.ContainerPort {
			facts = append(facts, fmt.Sprintf("The active and previous container ports both record %d.", current.ContainerPort))
		} else {
			facts = append(facts, fmt.Sprintf("The container port changed from %d to %d.", previous.ContainerPort, current.ContainerPort))
		}
	}

	currentServices := deploymentServiceNames(current.DeploymentServices)
	previousServices := deploymentServiceNames(previous.Services)
	if len(currentServices) > 0 && len(previousServices) > 0 &&
		!current.DeploymentServicesTruncated && !previous.ServicesTruncated &&
		!current.DeploymentIdentifiersTruncated && !previous.IdentifiersTruncated {
		if sameStringSet(currentServices, previousServices) {
			facts = append(facts, fmt.Sprintf("The recorded service topology is unchanged: %s.", quotedIdentifiers(currentServices)))
		} else {
			facts = append(
				facts,
				fmt.Sprintf("The recorded service topology changed from %s to %s.", quotedIdentifiers(previousServices), quotedIdentifiers(currentServices)),
			)
		}
	}

	currentContainers := currentDeploymentContainers(current)
	previousContainers := previousDeploymentContainers(previous)
	if len(currentContainers) > 0 && len(previousContainers) > 0 &&
		!current.DeploymentServicesTruncated && !previous.ServicesTruncated &&
		!current.DeploymentIdentifiersTruncated && !previous.IdentifiersTruncated {
		if sameStringSet(currentContainers, previousContainers) {
			facts = append(facts, fmt.Sprintf("The current and previous container generations use the same recorded identifiers: %s.", quotedIdentifiers(currentContainers)))
		} else {
			facts = append(
				facts,
				fmt.Sprintf(
					"The current container/generation (%s) differs from the immediately previous recorded deployment (%s); this is an identity transition only.",
					quotedIdentifiers(currentContainers),
					quotedIdentifiers(previousContainers),
				),
			)
		}
	}
	return boundedDiagnosticStrings(
		facts,
		changeComparisonMaxFacts,
		diagnosticAssessmentItemMaxRunes,
	)
}

func namedServiceComparisonFacts(current appDiagnosticSnapshot, previous deploymentHistoryVersion) []string {
	if current.DeploymentIdentifiersTruncated || previous.IdentifiersTruncated {
		return nil
	}
	currentByName := deploymentServicesByName(current.DeploymentServices)
	previousByName := deploymentServicesByName(previous.Services)
	changes := []string{}
	for _, name := range sortedDeploymentServiceNames(currentByName) {
		currentService := currentByName[name]
		previousService, ok := previousByName[name]
		if !ok {
			continue
		}
		if currentService.Image != "" && previousService.Image != "" &&
			currentService.Image != previousService.Image {
			changes = append(changes, fmt.Sprintf("%s image %q to %q", name, previousService.Image, currentService.Image))
		}
		if currentService.Strategy != "" && previousService.Strategy != "" &&
			!strings.EqualFold(currentService.Strategy, previousService.Strategy) {
			changes = append(changes, fmt.Sprintf("%s strategy %q to %q", name, previousService.Strategy, currentService.Strategy))
		}
		if currentService.ContainerPort != 0 && previousService.ContainerPort != 0 &&
			currentService.ContainerPort != previousService.ContainerPort {
			changes = append(changes, fmt.Sprintf("%s container port %d to %d", name, previousService.ContainerPort, currentService.ContainerPort))
		}
	}
	if len(changes) == 0 {
		return nil
	}
	return []string{"Named service metadata changed: " + strings.Join(
		boundedDiagnosticStrings(changes, 4, 120),
		"; ",
	) + "."}
}

func deploymentHistoryComparisonUnknowns(current appDiagnosticSnapshot, previous deploymentHistoryVersion) []string {
	unknowns := []string{}
	if current.DeploymentImage == "" &&
		!deploymentServicesHaveImages(current.DeploymentServices) {
		unknowns = append(unknowns, "The current deployed image is unavailable.")
	}
	if previous.Image == "" && !deploymentServicesHaveImages(previous.Services) {
		unknowns = append(unknowns, "The immediately previous deployed image is unavailable.")
	}
	if current.DeploymentServicesTruncated || previous.ServicesTruncated {
		unknowns = append(
			unknowns,
			"Service topology comparison is incomplete because at least one service list was truncated; no full-set topology difference is inferred.",
		)
	}
	if current.DeploymentIdentifiersTruncated || previous.IdentifiersTruncated {
		unknowns = append(
			unknowns,
			"Deployment identifier comparison is incomplete because at least one identifier was truncated; equality is not inferred from bounded prefixes.",
		)
	}
	return unknowns
}

func deploymentServicesHaveImages(services []deploymentHistoryService) bool {
	for _, service := range services {
		if service.Image != "" {
			return true
		}
	}
	return false
}

func deploymentServicesByName(services []deploymentHistoryService) map[string]deploymentHistoryService {
	out := map[string]deploymentHistoryService{}
	for _, service := range services {
		name := strings.TrimSpace(service.Name)
		if name != "" {
			out[name] = service
		}
	}
	return out
}

func sortedDeploymentServiceNames(services map[string]deploymentHistoryService) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func deploymentServiceNames(services []deploymentHistoryService) []string {
	names := make([]string, 0, len(services))
	for _, service := range services {
		names = append(names, service.Name)
	}
	return sortedUniqueNonempty(names)
}

func currentDeploymentContainers(snapshot appDiagnosticSnapshot) []string {
	containers := []string{}
	for _, service := range snapshot.DeploymentServices {
		if service.Container != "" {
			containers = append(containers, service.Container)
		}
	}
	if len(containers) == 0 && snapshot.DeploymentContainer != "" {
		containers = append(containers, snapshot.DeploymentContainer)
	}
	if len(containers) == 0 {
		for _, service := range snapshot.Services {
			if service.Container != "" {
				containers = append(containers, service.Container)
			}
		}
	}
	return sortedUniqueNonempty(containers)
}

func previousDeploymentContainers(version deploymentHistoryVersion) []string {
	containers := []string{}
	for _, service := range version.Services {
		if service.Container != "" {
			containers = append(containers, service.Container)
		}
	}
	if len(containers) == 0 && version.Container != "" {
		containers = append(containers, version.Container)
	}
	return sortedUniqueNonempty(containers)
}

func sortedUniqueNonempty(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func quotedIdentifiers(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = fmt.Sprintf("%q", value)
	}
	return strings.Join(quoted, ", ")
}

func currentDiagnosticFailure(snapshot appDiagnosticSnapshot) bool {
	return isDiagnosticUnhealthyState(snapshot.Health) || hasFailingService(snapshot)
}

func currentDiagnosticHealthy(snapshot appDiagnosticSnapshot) bool {
	return strings.EqualFold(snapshot.DeploymentState, "healthy") &&
		strings.EqualFold(snapshot.Health, "healthy") &&
		verifiedAllServicesRunning(snapshot) &&
		verifiedAllServicesHealthy(snapshot)
}

func deterministicUncertainDeploymentCausalityAnswer(packet diagnosticEvidencePacket, message string) (string, bool) {
	if !isDeploymentCausalityQuestion(message) {
		return "", false
	}

	response := diagnosticFinalResponse{
		Status:         packet.Assessment.Status,
		Confidence:     "unknown",
		Working:        packet.Assessment.Working,
		Failing:        packet.Assessment.Failing,
		RuledOut:       packet.Assessment.RuledOut,
		LessLikely:     packet.Assessment.LessLikely,
		PossibleCauses: packet.Assessment.PossibleCauses,
		KeyEvidence:    packet.Assessment.KeyEvidence,
		Unknowns:       packet.Assessment.Unknowns,
	}
	response.Conclusion = fmt.Sprintf(
		"%s's current state is %s. The available evidence does not establish whether the latest deployment caused the reported issue. Current state alone cannot rule the deployment in or out.",
		packet.Application.App,
		currentDiagnosticState(packet.Application),
	)
	return renderDiagnosticFinalResponse(response), true
}

func deterministicChangeAwareDiagnosticAnswer(packet diagnosticEvidencePacket, message string) (string, bool) {
	if !isChangeAwareDiagnosticQuestion(message) {
		return "", false
	}

	response := diagnosticFinalResponse{
		Status:         packet.Assessment.Status,
		Confidence:     "unknown",
		Working:        packet.Assessment.Working,
		Failing:        packet.Assessment.Failing,
		RuledOut:       packet.Assessment.RuledOut,
		LessLikely:     packet.Assessment.LessLikely,
		PossibleCauses: packet.Assessment.PossibleCauses,
		KeyEvidence:    packet.Assessment.KeyEvidence,
		Unknowns:       packet.Assessment.Unknowns,
	}
	state := currentDiagnosticState(packet.Application)

	if packet.DeploymentHistory == nil {
		response.Conclusion = fmt.Sprintf(
			"%s's fresh current state is %s, but deployment history is unavailable, so I cannot compare it with the previous deployment.",
			packet.Application.App,
			state,
		)
		return renderDiagnosticFinalResponse(response), true
	}
	if len(packet.DeploymentHistory.Versions) == 0 {
		response.Conclusion = fmt.Sprintf(
			"There is no previous deployment history available for %s, so I cannot compare the current deployment with an earlier one. Its fresh current state is %s.",
			packet.Application.App,
			state,
		)
		return renderDiagnosticFinalResponse(response), true
	}
	if !isNarrowDeploymentMetadataQuestion(message) {
		return "", false
	}

	facts := deploymentHistoryComparisonFacts(
		packet.Application,
		packet.DeploymentHistory.Versions[0],
	)
	response.KeyEvidence = boundedDiagnosticStrings(
		append(append([]string{}, facts...), packet.Assessment.KeyEvidence...),
		changeComparisonMaxFacts,
		diagnosticAssessmentItemMaxRunes,
	)
	response.Unknowns = boundedDiagnosticStrings(
		append(
			deploymentHistoryComparisonUnknowns(
				packet.Application,
				packet.DeploymentHistory.Versions[0],
			),
			packet.Assessment.Unknowns...,
		),
		diagnosticAssessmentMaxItems,
		diagnosticAssessmentItemMaxRunes,
	)
	response.Conclusion = fmt.Sprintf(
		"%s's fresh current state is %s. The bounded current and immediately previous deployment metadata are compared below; container identity changes are generation facts only.",
		packet.Application.App,
		state,
	)
	return renderDiagnosticFinalResponse(response), true
}

func isNarrowDeploymentMetadataQuestion(message string) bool {
	m := strings.ToLower(message)
	if changeQuestionNeedsTimingOrFailureEvidence(m) ||
		messageContainsTerm(m, "compare") ||
		strings.Contains(m, "what changed") {
		return false
	}
	return containsAny(
		m,
		"deployment metadata", "image change", "image changed",
		"strategy change", "strategy changed", "port change", "port changed",
		"container change", "container changed", "topology change",
		"service change",
	)
}

func currentDiagnosticState(snapshot appDiagnosticSnapshot) string {
	if value := strings.TrimSpace(snapshot.DeploymentState); value != "" {
		return value
	}
	if value := strings.TrimSpace(snapshot.Health); value != "" {
		return value
	}
	return "unknown"
}

func unresolvedChangeAwareDiagnosticAnswer() string {
	return "I could not resolve exactly one currently deployed application from fresh state, so deployment-change diagnostics are unavailable. Name one current application and I will compare it without reusing prior runtime state."
}

func coreDeploymentHistory(history *deploymentHistoryToolResponse) map[string]any {
	core := map[string]any{
		"app":                    truncateDiagnosticRunes(history.App, 64),
		"total_versions":         history.TotalVersions,
		"truncated":              history.Truncated || len(history.Versions) > 1,
		"redacted":               history.Redacted,
		"exact_commit_available": false,
		"timestamp_meaning":      "archive/cutover time, not original deployment time",
		"source":                 "MiniDeploy deployment history",
	}
	if len(history.Versions) == 0 {
		core["versions"] = []any{}
		return core
	}

	version := history.Versions[0]
	identifiersTruncated := version.IdentifiersTruncated ||
		diagnosticStringExceeds(version.Container, 128) ||
		diagnosticStringExceeds(version.Image, 128) ||
		diagnosticStringExceeds(version.Strategy, 48)
	for index, service := range version.Services {
		if index >= 2 {
			break
		}
		identifiersTruncated = identifiersTruncated ||
			diagnosticStringExceeds(service.Name, 48) ||
			diagnosticStringExceeds(service.Container, 128) ||
			diagnosticStringExceeds(service.Image, 128) ||
			diagnosticStringExceeds(service.Strategy, 48)
	}
	previous := map[string]any{
		"position":              version.Position,
		"relation":              "immediately_previous",
		"truncated":             version.Truncated || len(version.Services) > 2 || identifiersTruncated,
		"services_truncated":    version.ServicesTruncated || len(version.Services) > 2,
		"identifiers_truncated": identifiersTruncated,
	}
	putBoundedDiagnosticString(previous, "container", version.Container, 128)
	putBoundedDiagnosticString(previous, "image", version.Image, 128)
	putBoundedDiagnosticString(previous, "strategy", version.Strategy, 48)
	putBoundedDiagnosticString(previous, "archived_at", version.ArchivedAt, 64)
	if version.Port != 0 {
		previous["port"] = version.Port
	}
	if version.ContainerPort != 0 {
		previous["container_port"] = version.ContainerPort
	}
	services := make([]map[string]any, 0, 2)
	for _, service := range version.Services {
		if len(services) == cap(services) {
			break
		}
		item := map[string]any{}
		putBoundedDiagnosticString(item, "name", service.Name, 48)
		putBoundedDiagnosticString(item, "container", service.Container, 128)
		putBoundedDiagnosticString(item, "image", service.Image, 128)
		putBoundedDiagnosticString(item, "strategy", service.Strategy, 48)
		if service.ContainerPort != 0 {
			item["container_port"] = service.ContainerPort
		}
		services = append(services, item)
	}
	if len(services) > 0 {
		previous["services"] = services
	}
	core["versions"] = []map[string]any{previous}
	return core
}

func diagnosticStringExceeds(value string, limit int) bool {
	return len([]rune(value)) > limit
}
