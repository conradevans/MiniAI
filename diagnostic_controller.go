package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	diagnosticMaxEvidenceExpansionRounds = 3
	diagnosticMaxRepositorySnippets      = 2
	diagnosticAssessmentMaxItems         = 4
	diagnosticAssessmentItemMaxRunes     = 240
)

const (
	diagnosticSourceRuntime    = "runtime_logs"
	diagnosticSourceDeployment = "deployment_logs"
	diagnosticSourceRepository = "repository"
)

type diagnosticInvestigationSummary struct {
	Rounds             int      `json:"rounds"`
	EvidenceExpansions []string `json:"evidence_expansions,omitempty"`
	BudgetExhausted    bool     `json:"budget_exhausted,omitempty"`
}

type diagnosticEvidenceAssessment struct {
	Status            string   `json:"status"`
	ConfidenceCeiling string   `json:"confidence_ceiling"`
	Working           []string `json:"working,omitempty"`
	Failing           []string `json:"failing,omitempty"`
	RuledOut          []string `json:"ruled_out,omitempty"`
	LessLikely        []string `json:"less_likely,omitempty"`
	PossibleCauses    []string `json:"possible_causes,omitempty"`
	KeyEvidence       []string `json:"key_evidence,omitempty"`
	Unknowns          []string `json:"unknowns,omitempty"`
	Contradictory     bool     `json:"contradictory,omitempty"`
}

func (a *app) runDiagnosticInvestigation(ctx context.Context, snapshot appDiagnosticSnapshot, message string) (diagnosticEvidencePacket, []diagnosticEvidence) {
	packet := diagnosticEvidencePacket{
		Application: snapshot,
		Unavailable: []string{},
		Repository:  []repositoryLocationEvidence{},
	}
	if hasCurrentFailurePremise(message) {
		packet.UserReportedSymptom = scrubbedDiagnosticText(message, diagnosticAssessmentItemMaxRunes)
	}
	evidence := []diagnosticEvidence{{
		ToolName: "get_app_context", Args: map[string]any{"app": snapshot.App},
		Summary: "resolved structured application, service, database, activity, and version state",
	}}
	attempted := map[string]bool{}
	for packet.Investigation.Rounds < diagnosticMaxEvidenceExpansionRounds {
		source := nextDiagnosticEvidenceSource(packet, message, attempted)
		if source == "" {
			break
		}
		attempted[source] = true
		packet.Investigation.Rounds++
		packet.Investigation.EvidenceExpansions = append(packet.Investigation.EvidenceExpansions, source)
		switch source {
		case diagnosticSourceRuntime:
			logs, err := a.readMiniDeployLogs(ctx, snapshot.App, "runtime", diagnosticLogLines)
			if err != nil {
				appendDiagnosticUnavailable(&packet, "runtime logs unavailable")
				continue
			}
			summary := summarizeDiagnosticLogs(logs, message, time.Now())
			packet.RuntimeLogs = &summary
			evidence = append(evidence, diagnosticEvidence{
				ToolName: "read_runtime_logs",
				Args:     map[string]any{"app": snapshot.App, "lines": diagnosticLogLines},
				Summary:  fmt.Sprintf("summarized %d bounded runtime log lines", summary.LinesExamined),
			})
		case diagnosticSourceDeployment:
			logs, err := a.readMiniDeployLogs(ctx, snapshot.App, "deployment", diagnosticLogLines)
			if err != nil {
				appendDiagnosticUnavailable(&packet, "deployment logs unavailable")
				continue
			}
			summary := summarizeDiagnosticLogs(logs, message, time.Now())
			packet.DeploymentLogs = &summary
			evidence = append(evidence, diagnosticEvidence{
				ToolName: "read_deployment_logs",
				Args:     map[string]any{"app": snapshot.App, "lines": diagnosticLogLines},
				Summary:  fmt.Sprintf("summarized %d bounded deployment log lines", summary.LinesExamined),
			})
		case diagnosticSourceRepository:
			snippets, repositoryEvidence, err := a.gatherDiagnosticRepositoryEvidence(ctx, snapshot.App, message)
			evidence = append(evidence, repositoryEvidence...)
			if err != nil || len(snippets) == 0 {
				appendDiagnosticUnavailable(&packet, "relevant repository evidence unavailable")
				continue
			}
			packet.Repository = snippets
		}
	}
	packet.Investigation.BudgetExhausted = nextDiagnosticEvidenceSource(packet, message, attempted) != ""
	packet.Assessment = assessDiagnosticEvidence(packet, message)
	return packet, evidence
}

func nextDiagnosticEvidenceSource(packet diagnosticEvidencePacket, message string, attempted map[string]bool) string {
	if !attempted[diagnosticSourceRuntime] {
		return diagnosticSourceRuntime
	}
	if !attempted[diagnosticSourceDeployment] && diagnosticNeedsDeploymentEvidence(packet, message) {
		return diagnosticSourceDeployment
	}
	if !attempted[diagnosticSourceRepository] && diagnosticNeedsRepositoryEvidence(packet, message) {
		return diagnosticSourceRepository
	}
	return ""
}

func diagnosticNeedsDeploymentEvidence(packet diagnosticEvidencePacket, message string) bool {
	lower := strings.ToLower(message)
	if containsAny(lower, "deploy", "release", "rollout", "after", "what changed") ||
		messageContainsTerm(lower, "version") || messageContainsTerm(lower, "commit") {
		return true
	}
	if packet.Application.VersionMismatch != nil && *packet.Application.VersionMismatch {
		return true
	}
	if diagnosticErrorsFollowDeployment(packet.Application, packet.RuntimeLogs) {
		return true
	}
	return diagnosticLogContains(packet.RuntimeLogs, "configuration error", "invalid configuration", "missing required configuration")
}

func diagnosticNeedsRepositoryEvidence(packet diagnosticEvidencePacket, message string) bool {
	if isRepositoryCodeQuestion(message) || containsAny(strings.ToLower(message), "code path", "stack trace") {
		return true
	}
	terms := []string{"route", "endpoint", "handler", "function", "stack trace", ".go:", ".js:", ".ts:"}
	return diagnosticLogContains(packet.RuntimeLogs, terms...) || diagnosticLogContains(packet.DeploymentLogs, terms...)
}

func diagnosticLogContains(summary *diagnosticLogSummary, terms ...string) bool {
	if summary == nil {
		return false
	}
	for _, pattern := range summary.Patterns {
		haystack := strings.ToLower(pattern.Signature + " " + pattern.Representative)
		if containsAny(haystack, terms...) {
			return true
		}
	}
	return false
}

func diagnosticErrorsFollowDeployment(snapshot appDiagnosticSnapshot, summary *diagnosticLogSummary) bool {
	if snapshot.DeployedAt == "" || summary == nil || summary.FirstErrorTimestamp == "" {
		return false
	}
	deployedAt, err := time.Parse(time.RFC3339Nano, snapshot.DeployedAt)
	if err != nil {
		return false
	}
	firstError, err := time.Parse(time.RFC3339Nano, summary.FirstErrorTimestamp)
	return err == nil && !firstError.Before(deployedAt)
}

func appendDiagnosticUnavailable(packet *diagnosticEvidencePacket, value string) {
	for _, existing := range packet.Unavailable {
		if existing == value {
			return
		}
	}
	packet.Unavailable = append(packet.Unavailable, value)
}

func (a *app) gatherDiagnosticRepositoryEvidence(ctx context.Context, appName, message string) ([]repositoryLocationEvidence, []diagnosticEvidence, error) {
	query := repositorySeedQuery(message, []string{appName})
	events := []diagnosticEvidence{{
		ToolName: "search_repository",
		Args:     map[string]any{"app": appName, "query": query},
		Summary:  "inspected the deterministic safe-source repository index",
	}}
	index, _, err := a.repositoryIndex(ctx, appName)
	if err != nil {
		return nil, events, err
	}
	type candidate struct {
		path string
		line int
	}
	var candidates []candidate
	if result, ok := lookupRepositoryIndex(index, message); ok {
		for _, item := range result.Evidence {
			candidates = append(candidates, candidate{path: item.Path, line: item.Line})
		}
	} else if query != "" {
		search, searchErr := a.searchRepository(ctx, appName, ".", query)
		if searchErr != nil {
			return nil, events, searchErr
		}
		for _, path := range bestUnreadSourcePaths(search, map[string]bool{}, query, diagnosticMaxRepositorySnippets) {
			line := 0
			for _, hit := range search.Hits {
				if hit.Path == path {
					line = hit.Line
					break
				}
			}
			if line > 0 {
				candidates = append(candidates, candidate{path: path, line: line})
			}
		}
	}
	seen := map[string]bool{}
	var snippets []repositoryLocationEvidence
	for _, candidate := range candidates {
		if candidate.path == "" || candidate.line <= 0 || seen[candidate.path] || len(snippets) >= diagnosticMaxRepositorySnippets {
			continue
		}
		seen[candidate.path] = true
		file, readErr := a.readRepositoryFile(appName, candidate.path)
		if readErr != nil {
			continue
		}
		search := repoSearchResponse{Hits: []repoSearchHit{{Path: file.Path, Line: candidate.line}}}
		snippet, ok := compactRepositoryLocationEvidence(search, file)
		if !ok {
			continue
		}
		snippets = append(snippets, snippet)
		events = append(events, diagnosticEvidence{
			ToolName: "read_repository_file",
			Args:     map[string]any{"app": appName, "path": file.Path},
			Summary:  fmt.Sprintf("attached bounded repository lines %d-%d", snippet.StartLine, snippet.EndLine),
		})
	}
	if len(snippets) == 0 {
		return nil, events, fmt.Errorf("no relevant safe repository snippet found")
	}
	return snippets, events, nil
}

func assessDiagnosticEvidence(packet diagnosticEvidencePacket, message string) diagnosticEvidenceAssessment {
	assessment := diagnosticEvidenceAssessment{
		Status: "unknown", ConfidenceCeiling: "unknown",
		Working: []string{}, Failing: []string{}, RuledOut: []string{}, LessLikely: []string{},
		PossibleCauses: []string{}, KeyEvidence: []string{}, Unknowns: []string{},
	}
	snapshot := packet.Application
	allRunning := verifiedAllServicesRunning(snapshot)
	allHealthy := verifiedAllServicesHealthy(snapshot)
	if strings.EqualFold(snapshot.DeploymentState, "healthy") {
		assessment.Working = append(assessment.Working, "The deployment reports healthy.")
		assessment.LessLikely = append(assessment.LessLikely, "A complete deployment outage is less likely.")
	}
	for _, service := range snapshot.Services {
		name := service.Name
		if name == "" {
			name = "A reported service"
		}
		if strings.EqualFold(service.State, "running") {
			assessment.Working = append(assessment.Working, fmt.Sprintf("%s process is running.", name))
		} else if service.State != "" {
			assessment.Failing = append(assessment.Failing, fmt.Sprintf("%s process state is %s.", name, service.State))
		}
		if isDiagnosticUnhealthyState(service.Health) {
			assessment.Failing = append(assessment.Failing, fmt.Sprintf("%s health is %s.", name, service.Health))
		}
	}
	if allRunning {
		assessment.RuledOut = append(assessment.RuledOut, "A complete service-process outage.")
	}
	if snapshot.Database != nil {
		if strings.EqualFold(snapshot.Database.Status, "ready") {
			assessment.Working = append(assessment.Working, "The associated database reports ready.")
			assessment.LessLikely = append(assessment.LessLikely, "A general database outage is less likely based on the available database metadata.")
		} else if isDiagnosticUnhealthyState(snapshot.Database.Status) {
			assessment.Failing = append(assessment.Failing, fmt.Sprintf("The associated database status is %s.", snapshot.Database.Status))
		}
	}
	if snapshot.VersionMismatch != nil {
		if *snapshot.VersionMismatch {
			assessment.Failing = append(assessment.Failing, "The deployed and repository versions differ.")
			assessment.PossibleCauses = append(assessment.PossibleCauses, "A deployed/repository version mismatch remains relevant.")
		} else {
			assessment.RuledOut = append(assessment.RuledOut, "A deployed/repository version mismatch.")
		}
	}
	addDiagnosticLogAssessment(&assessment, packet.RuntimeLogs, "runtime")
	addDiagnosticLogAssessment(&assessment, packet.DeploymentLogs, "deployment")
	if logSummarySupportsNoCurrentFailure(packet.RuntimeLogs) {
		assessment.KeyEvidence = append(assessment.KeyEvidence, fmt.Sprintf("The %d bounded recent runtime log lines contain no detected errors, HTTP 5xx responses, or restart/stop events.", packet.RuntimeLogs.LinesExamined))
		assessment.LessLikely = append(assessment.LessLikely, "A current observable server-side error condition is not supported by the bounded runtime logs.")
	}
	if diagnosticLogContains(packet.RuntimeLogs, "database timeout", "database timed out") {
		assessment.PossibleCauses = append(assessment.PossibleCauses, "An intermittent database-path timeout remains possible.")
	}
	if diagnosticErrorsFollowDeployment(snapshot, packet.RuntimeLogs) {
		assessment.KeyEvidence = append(assessment.KeyEvidence, "The first bounded runtime error follows the reported deployment time; timing alone does not prove causation.")
		assessment.PossibleCauses = append(assessment.PossibleCauses, "A deployment-related regression remains possible but is not established by timing alone.")
	}
	for _, snippet := range packet.Repository {
		assessment.KeyEvidence = append(assessment.KeyEvidence, fmt.Sprintf("Relevant bounded repository evidence was found in %s at lines %d-%d.", snippet.Path, snippet.StartLine, snippet.EndLine))
	}
	for _, unavailable := range packet.Unavailable {
		assessment.Unknowns = append(assessment.Unknowns, unavailable+".")
	}
	if snapshot.Health == "" {
		assessment.Unknowns = append(assessment.Unknowns, "Application health is unavailable.")
	}
	if packet.RuntimeLogs == nil {
		assessment.Unknowns = append(assessment.Unknowns, "Recent runtime error state is unavailable.")
	}
	if diagnosticNeedsDeploymentEvidence(packet, message) && packet.DeploymentLogs == nil {
		assessment.Unknowns = append(assessment.Unknowns, "Relevant deployment-log evidence is unavailable.")
	}
	if diagnosticNeedsRepositoryEvidence(packet, message) && len(packet.Repository) == 0 {
		assessment.Unknowns = append(assessment.Unknowns, "Relevant repository evidence is unavailable.")
	}
	assessment.Contradictory = diagnosticEvidenceIsContradictory(packet)
	if assessment.Contradictory {
		assessment.Unknowns = append(assessment.Unknowns, "Structured health indicators conflict.")
	}

	cleanEvidence := diagnosticEvidenceConsistentlyClean(packet)
	reportedFailure := packet.UserReportedSymptom != "" || hasCurrentFailurePremise(message)
	if reportedFailure && cleanEvidence {
		assessment.PossibleCauses = append(assessment.PossibleCauses, diagnosticReportedFailurePossibilities(packet, message)...)
		assessment.Unknowns = append(assessment.Unknowns, "The reported symptom is not reproduced or explained by the current observable server-side evidence.")
	}
	failureSignals := diagnosticFailureSignalCount(packet)
	directCause := diagnosticDirectCauseEvidence(packet)
	switch {
	case assessment.Contradictory || len(packet.Unavailable) > 0 || snapshot.Health == "" || packet.RuntimeLogs == nil:
		assessment.ConfidenceCeiling = "unknown"
	case isDiagnosticVerificationQuestion(message) && cleanEvidence:
		assessment.ConfidenceCeiling = "strongly_supported"
	case reportedFailure && cleanEvidence:
		assessment.ConfidenceCeiling = "plausible"
	case directCause != "":
		assessment.ConfidenceCeiling = "confirmed"
		assessment.KeyEvidence = append(assessment.KeyEvidence, directCause)
	case failureSignals >= 2:
		assessment.ConfidenceCeiling = "strongly_supported"
	case failureSignals == 1:
		assessment.ConfidenceCeiling = "plausible"
	default:
		assessment.ConfidenceCeiling = "unknown"
	}
	switch {
	case isDiagnosticUnhealthyState(snapshot.Health) || hasFailingService(snapshot):
		assessment.Status = "unhealthy"
	case diagnosticLogHasFailure(packet.RuntimeLogs) || diagnosticLogHasFailure(packet.DeploymentLogs):
		assessment.Status = "degraded"
	case strings.EqualFold(snapshot.Health, "healthy") && allHealthy:
		assessment.Status = "healthy"
	default:
		assessment.Status = "unknown"
	}
	if failureSignals > 0 && directCause == "" {
		assessment.Unknowns = append(assessment.Unknowns, "The available evidence does not directly establish a root cause.")
	}
	boundDiagnosticAssessment(&assessment)
	return assessment
}

func diagnosticReportedFailurePossibilities(packet diagnosticEvidencePacket, message string) []string {
	m := strings.ToLower(message)
	possibilities := []string{}
	if logSummarySupportsNoCurrentFailure(packet.RuntimeLogs) {
		possibilities = append(possibilities, "An intermittent problem outside the bounded runtime-log window remains possible.")
	}
	if containsAny(m, "browser", "client", "frontend", "page", "screen", "user interface", " ui ") {
		possibilities = append(possibilities, "A client-side or frontend-specific problem not represented by the server-side health evidence remains possible.")
	}
	if containsAny(m, "request", "endpoint", "route", " api", "http", "login", "schedule") {
		possibilities = append(possibilities, "A request-specific failure not reflected in aggregate service health remains possible.")
	}
	if len(possibilities) < 2 && strings.EqualFold(packet.Application.Health, "healthy") {
		possibilities = append(possibilities, "The reported symptom may be outside the aggregate server-side health checks MiniAI can observe.")
	}
	return boundedDiagnosticStrings(possibilities, diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes)
}

func addDiagnosticLogAssessment(assessment *diagnosticEvidenceAssessment, summary *diagnosticLogSummary, label string) {
	if summary == nil {
		return
	}
	serverErrors := diagnosticHTTP5xxCount(summary)
	if summary.ErrorCount > 0 {
		assessment.Failing = append(assessment.Failing, fmt.Sprintf("Bounded %s logs contain %d reliable error lines.", label, summary.ErrorCount))
		assessment.KeyEvidence = append(assessment.KeyEvidence, fmt.Sprintf("%s errors: count=%d, first=%s, last=%s.", label, summary.ErrorCount, valueOrUnknown(summary.FirstErrorTimestamp), valueOrUnknown(summary.LastErrorTimestamp)))
	}
	if serverErrors > 0 {
		assessment.Failing = append(assessment.Failing, fmt.Sprintf("Bounded %s logs contain %d HTTP 5xx responses.", label, serverErrors))
	}
	if summary.WarningCount > 0 {
		assessment.KeyEvidence = append(assessment.KeyEvidence, fmt.Sprintf("%s warnings: count=%d.", label, summary.WarningCount))
	}
	if summary.RestartCount > 0 || summary.StopCount > 0 {
		assessment.KeyEvidence = append(assessment.KeyEvidence, fmt.Sprintf("%s lifecycle markers: restarts=%d, stops=%d.", label, summary.RestartCount, summary.StopCount))
	}
}

func diagnosticHTTP5xxCount(summary *diagnosticLogSummary) int {
	if summary == nil {
		return 0
	}
	total := 0
	for status, count := range summary.HTTPStatusCounts {
		if len(status) == 3 && status[0] == '5' {
			total += count
		}
	}
	return total
}

func diagnosticLogHasFailure(summary *diagnosticLogSummary) bool {
	return summary != nil && (summary.ErrorCount > 0 || diagnosticHTTP5xxCount(summary) > 0 || summary.StopCount > 0)
}

func diagnosticFailureSignalCount(packet diagnosticEvidencePacket) int {
	count := 0
	if isDiagnosticUnhealthyState(packet.Application.Health) || hasFailingService(packet.Application) {
		count++
	}
	if diagnosticLogHasFailure(packet.RuntimeLogs) {
		count++
	}
	if diagnosticLogHasFailure(packet.DeploymentLogs) {
		count++
	}
	if recentActivityHasFailureEvidence(packet.Application.RecentActivity) {
		count++
	}
	return count
}

func diagnosticDirectCauseEvidence(packet diagnosticEvidencePacket) string {
	for _, summary := range []*diagnosticLogSummary{packet.RuntimeLogs, packet.DeploymentLogs} {
		if summary == nil {
			continue
		}
		for _, pattern := range summary.Patterns {
			if pattern.Level != "error" {
				continue
			}
			text := strings.ToLower(pattern.Signature + " " + pattern.Representative)
			if containsAny(text, "root cause:", "caused by ", "configuration error:", "invalid configuration", "missing required configuration") {
				return "A bounded error record directly identifies: " + truncateRunes(pattern.Signature, 180)
			}
		}
	}
	return ""
}

func diagnosticEvidenceIsContradictory(packet diagnosticEvidencePacket) bool {
	snapshot := packet.Application
	actualAllRunning := verifiedAllServicesRunning(snapshot)
	if snapshot.AllServicesRunning != nil && *snapshot.AllServicesRunning != actualAllRunning {
		return true
	}
	if strings.EqualFold(snapshot.Health, "healthy") && hasFailingService(snapshot) {
		return true
	}
	if snapshot.VersionMismatch != nil && snapshot.DeployedCommit != "" && snapshot.RepositoryCommit != "" {
		actualMismatch := !commitValuesMatch(snapshot.DeployedCommit, snapshot.RepositoryCommit)
		if *snapshot.VersionMismatch != actualMismatch {
			return true
		}
	}
	return false
}

func verifiedAllServicesRunning(snapshot appDiagnosticSnapshot) bool {
	if len(snapshot.Services) == 0 {
		return false
	}
	for _, service := range snapshot.Services {
		if !strings.EqualFold(service.State, "running") {
			return false
		}
	}
	return true
}

func verifiedAllServicesHealthy(snapshot appDiagnosticSnapshot) bool {
	if len(snapshot.Services) == 0 {
		return false
	}
	for _, service := range snapshot.Services {
		if !strings.EqualFold(service.Health, "healthy") {
			return false
		}
	}
	return true
}

func hasFailingService(snapshot appDiagnosticSnapshot) bool {
	for _, service := range snapshot.Services {
		if isDiagnosticUnhealthyState(service.State) || isDiagnosticUnhealthyState(service.Health) {
			return true
		}
	}
	return false
}

func isDiagnosticUnhealthyState(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return containsAny(value, "unhealthy", "failed", "failing", "degraded", "stopped", "inactive", "exited", "crashed")
}

func boundDiagnosticAssessment(assessment *diagnosticEvidenceAssessment) {
	assessment.Working = boundedDiagnosticStrings(assessment.Working, diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes)
	assessment.Failing = boundedDiagnosticStrings(assessment.Failing, diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes)
	assessment.RuledOut = boundedDiagnosticStrings(assessment.RuledOut, diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes)
	assessment.LessLikely = boundedDiagnosticStrings(assessment.LessLikely, diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes)
	assessment.PossibleCauses = boundedDiagnosticStrings(assessment.PossibleCauses, diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes)
	assessment.KeyEvidence = boundedDiagnosticStrings(assessment.KeyEvidence, diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes)
	assessment.Unknowns = boundedDiagnosticStrings(assessment.Unknowns, diagnosticAssessmentMaxItems, diagnosticAssessmentItemMaxRunes)
}

func boundedDiagnosticStrings(values []string, limit, maxRunes int) []string {
	seen := map[string]bool{}
	out := make([]string, 0, limit)
	for _, value := range values {
		value = strings.TrimSpace(truncateDiagnosticRunes(value, maxRunes))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func truncateDiagnosticRunes(value string, limit int) string {
	runes := []rune(value)
	if limit <= 0 {
		return ""
	}
	if len(runes) <= limit {
		return value
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func valueOrUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
